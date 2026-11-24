package ink

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/wujinhjun/ink-commons/internal/execution"
)

const maxAllocationRecoveryAttempts = 6

func (w TaskWake) canRecover() bool {
	return w.Allocation != nil && !w.RecoveryStopped && w.RecoveryAttempts < maxAllocationRecoveryAttempts
}

func retryablePlacementRead(err error) bool {
	var status execution.HTTPStatusError
	if errors.As(err, &status) {
		return status == http.StatusTooManyRequests || status >= 500
	}
	var networkErr net.Error
	return errors.As(err, &networkErr) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// Each pending task is its own durable outbox entry. Wake retries never create
// executor attempts: only claim grants permission to do work.
func (s *Service) dispatchHandoff(ctx context.Context, id string, channelCheck reviewChannelCheck) (bool, error) {
	s.mu.Lock()
	st, err := s.store.load()
	if err != nil {
		s.mu.Unlock()
		return false, err
	}
	w := st.Work[id]
	if w != nil {
		if err := s.preparePendingConfig(w); err != nil {
			s.mu.Unlock()
			return false, err
		}
	}
	if w == nil || w.Task == nil || w.Task.Handoff == nil {
		s.mu.Unlock()
		return false, nil
	}
	t := w.Task
	h := t.Handoff
	recovering := h.Wake.Preparation == "preparing"
	if h.State != "pending" || w.executionBlocked() || s.delivering[id] || time.Now().Before(h.Wake.RetryAt) ||
		(recovering && !h.Wake.canRecover()) {
		s.mu.Unlock()
		return true, nil
	}
	route := h.Assignee.Route
	if route == nil {
		s.mu.Unlock()
		return true, nil
	}
	executor := s.providers[route.Provider]
	waker, ok := executor.(execution.TaskWaker)
	if !ok {
		h.Wake.Error = "provider does not support cooperative task wakes"
		w.refreshWorkflow(time.Now(), s.config.Reliability.defaults())
		err = s.store.save(st)
		s.mu.Unlock()
		return true, err
	}
	if err := s.validateReviewPreparation(w); err != nil {
		h.Wake.Error = "review preparation: " + err.Error()
		w.refreshWorkflow(time.Now(), s.config.Reliability.defaults())
		saveErr := s.store.save(st)
		s.mu.Unlock()
		return true, saveErr
	}
	allocator, allocates := executor.(execution.ConversationAllocator)
	if recovering && !allocates {
		s.mu.Unlock()
		return true, nil
	}
	if !recovering {
		if current, err := channelCheck.result(s, w); !current || err != nil {
			if err != nil {
				message := "review preparation: " + err.Error()
				if h.Wake.Error != message {
					log.Printf("workflow %s task %s: %s", id, t.ID, message)
				}
				h.Wake.Error = message
				if retryablePlacementRead(err) {
					h.Wake.PreparationAttempts++
					h.Wake.RetryAt = time.Now().Add(taskWakeRetryDelay(h.Wake.PreparationAttempts))
				}
				w.refreshWorkflow(time.Now(), s.config.Reliability.defaults())
				err = s.store.save(st)
			}
			s.mu.Unlock()
			return true, err
		}
	}
	if err := execution.ValidateDestination(s.providers, *route, route.Session == ""); err != nil {
		s.mu.Unlock()
		return true, err
	}
	if err := s.freezeTaskConfig(w); err != nil {
		s.mu.Unlock()
		return true, err
	}
	d := deliveryFor(w, t, "", 0)
	prepared := *route
	if prepared.Session == "" {
		if err := execution.ValidatePreparation(s.providers, prepared, d); err != nil {
			t.ConfigSnapshot = nil
			if t.Role == "reviewer" && w.RoundConfig != nil && w.RoundConfig.SHA == t.SHA {
				w.RoundConfig = nil
			}
			h.Wake.Error = err.Error()
			err = s.store.save(st)
			s.mu.Unlock()
			return true, err
		}
		// Persist the exact destination before sending a creation request.
		if allocates && !recovering && channelCheck.placement != nil && channelCheck.placement.Session == "" {
			allocation := allocator.PlanConversation(*channelCheck.placement, d)
			reservedAt := time.Now().UTC()
			allocation.ReservedAt = &reservedAt
			h.Wake.Allocation = &allocation
		}
		h.Wake.Preparation = "preparing"
	} else {
		h.Wake.Preparation = "prepared"
	}
	if recovering {
		h.Wake.RecoveryAttempts++
		h.Wake.RetryAt = time.Now().Add(taskWakeRetryDelay(h.Wake.RecoveryAttempts))
	} else {
		// Reserve the wake before sending so interrupted dispatches retain their delay.
		h.Wake.Attempts++
		h.Wake.RetryAt = time.Now().Add(taskWakeRetryDelay(h.Wake.Attempts))
	}
	if err = s.store.save(st); err != nil {
		s.mu.Unlock()
		return true, err
	}
	s.delivering[id] = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.delivering, id); s.mu.Unlock() }()
	if prepared.Session == "" {
		preparation := d
		preparation.ReviewPlacement = channelCheck.placement
		var prepareErr error
		switch {
		case recovering:
			prepared.Session, prepareErr = allocator.FindConversation(ctx, *h.Wake.Allocation)
		case h.Wake.Allocation != nil:
			prepared.Session, prepareErr = allocator.CreateConversation(ctx, *h.Wake.Allocation)
		default:
			prepared.Session, prepareErr = executor.Prepare(ctx, prepared, preparation)
		}
		if prepareErr == nil {
			prepareErr = execution.ValidateDestination(s.providers, prepared, false)
		}
		s.mu.Lock()
		live, loadErr := s.store.load()
		if loadErr != nil {
			s.mu.Unlock()
			return true, loadErr
		}
		lw, lt := findTask(live, t.ID)
		if lt == nil || lt.Handoff == nil {
			s.mu.Unlock()
			return true, nil
		}
		lh := lt.Handoff
		if prepareErr != nil {
			lh.Wake.recordPreparationError(prepareErr, recovering)
			log.Printf("workflow %s task %s: %s", id, t.ID, lh.Wake.Error)
		} else {
			lh.Wake.Preparation = "prepared"
			lh.Assignee.Route = &prepared
			lh.Wake.Error = ""
			if lw.Task == lt {
				if lt.Role == "reviewer" {
					lw.Reviewer.Route = &prepared
				} else {
					lw.Developer.Route = &prepared
				}
			}
		}
		lw.refreshWorkflow(time.Now(), s.config.Reliability.defaults())
		err = s.store.save(live)
		s.mu.Unlock()
		if err != nil || prepareErr != nil {
			return true, err
		}
	}
	// Recheck after allocation. A delayed wake is harmless because claim performs
	// the same eligibility check under the state lock.
	s.mu.Lock()
	live, err := s.store.load()
	current, latest := findTask(live, t.ID)
	eligible := err == nil && latest != nil && current.Task == latest && latest.Handoff != nil && latest.Handoff.State == "pending" && !current.executionBlocked()
	s.mu.Unlock()
	if err != nil || !eligible {
		return true, err
	}
	sendErr := waker.WakeTask(ctx, prepared, d)
	s.mu.Lock()
	defer s.mu.Unlock()
	live, err = s.store.load()
	if err != nil {
		return true, err
	}
	current, latest = findTask(live, t.ID)
	if latest != nil && latest.Handoff != nil {
		latest.Handoff.Wake.Error = ""
		if sendErr != nil {
			latest.Handoff.Wake.Error = fmt.Sprintf("wake not confirmed; will retry: %v", sendErr)
		} else {
			now := time.Now()
			latest.Handoff.Wake.AcceptedAt = &now
		}
		current.refreshWorkflow(time.Now(), s.config.Reliability.defaults())
	}
	return true, s.store.save(live)
}

func (w *TaskWake) recordPreparationError(err error, recovering bool) {
	if recovering {
		w.Error = fmt.Sprintf("reconcile Discord thread: %v", err)
		if w.AllocationError != "" {
			w.Error = w.AllocationError + "; " + w.Error
		}
		return
	}
	w.Error = "conversation preparation: " + err.Error()
	w.AllocationError = w.Error
	if w.Allocation == nil {
		return
	}
	var status execution.HTTPStatusError
	if !errors.As(err, &status) {
		return
	}
	switch status {
	case http.StatusTooManyRequests:
		// Discord definitively rejected the request. It is safe to create later.
		w.Allocation = nil
		w.Preparation = ""
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusMethodNotAllowed:
		w.RecoveryStopped = true
	}
}

// 未领取的任务先保持三次短间隔，再线性退避，避免忙会话持续刷屏。
// 次数由任务持久化，重启不重置；与执行失败重试的 backoff_seconds 独立。
func taskWakeRetryDelay(attempts int) time.Duration {
	if attempts <= 3 {
		return time.Minute
	}
	if attempts >= 62 {
		return time.Hour
	}
	return time.Duration(attempts-2) * time.Minute
}
