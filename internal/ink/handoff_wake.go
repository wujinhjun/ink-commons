//go:build linux || darwin

package ink

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/google/uuid"
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
	w, err := s.store.loadWork(id)
	if err != nil {
		s.mu.Unlock()
		return false, err
	}
	if !matchesObservation(ctx, w) {
		s.mu.Unlock()
		return true, nil
	}
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
	if h.State != "pending" || w.executionBlocked() || s.delivering[id] || s.now().Before(h.Wake.RetryAt) ||
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
	if !execution.Supports(executor, "wake", ok) {
		h.Wake.Error = "provider does not support cooperative task wakes"
		s.refreshWorkflow(w, s.now())
		err = s.store.saveWork(w)
		s.mu.Unlock()
		return true, err
	}
	if err := s.validateReviewPreparation(w); err != nil {
		h.Wake.Error = "review preparation: " + err.Error()
		s.refreshWorkflow(w, s.now())
		saveErr := s.store.saveWork(w)
		s.mu.Unlock()
		return true, saveErr
	}
	if recovering && !supportsAllocation(executor) {
		h.Wake.Error = "provider cannot recover the reserved conversation"
		err := s.store.saveWork(w)
		s.mu.Unlock()
		return true, err
	}
	if !recovering {
		if current, err := channelCheck.result(s, w); !current || err != nil {
			if err != nil {
				message := "review preparation: " + err.Error()
				if h.Wake.Error != message {
					s.warnError("review placement check failed", err, "repo", w.Repo, "number", w.Pull.Number)
				}
				h.Wake.Error = message
				if retryablePlacementRead(err) {
					h.Wake.PreparationAttempts++
					h.Wake.RetryAt = s.now().Add(taskWakeRetryDelay(h.Wake.PreparationAttempts))
				}
				s.refreshWorkflow(w, s.now())
				err = s.store.saveWork(w)
			}
			s.mu.Unlock()
			return true, err
		}
	}
	if err := execution.ValidateDestination(s.providers, *route, route.Session == ""); err != nil {
		s.mu.Unlock()
		return true, err
	}
	prepared := *route
	wake := h.Wake
	if !recovering {
		if err := reservePreparation(executor, &wake, &prepared, deliveryFor(w, t, "", 0), channelCheck.placement, s.now()); err != nil {
			h.Wake.Error = err.Error()
			s.refreshWorkflow(w, s.now())
			err = s.store.saveWork(w)
			s.mu.Unlock()
			return true, err
		}
	}
	if err := s.freezeTaskConfig(w); err != nil {
		s.mu.Unlock()
		return true, err
	}
	if wake.Request == nil {
		d := deliveryFor(w, t, "", 0)
		wake.Request = &d
	}
	h.Wake = wake
	d := *wake.Request
	// Until preparation is saved, claims against a newly resolved route fail.
	needsPreparation := route.Session == ""
	if !needsPreparation {
		h.Assignee.Route = &prepared
		if t.Role == "reviewer" {
			w.Reviewer.Route = &prepared
		}
	}
	if recovering {
		h.Wake.RecoveryAttempts++
		h.Wake.RetryAt = s.now().Add(taskWakeRetryDelay(h.Wake.RecoveryAttempts))
	} else {
		// Reserve the wake before sending so interrupted dispatches retain their delay.
		h.Wake.Attempts++
		h.Wake.RetryAt = s.now().Add(taskWakeRetryDelay(h.Wake.Attempts))
	}
	if err = s.store.saveWork(w); err != nil {
		s.mu.Unlock()
		return true, err
	}
	s.delivering[id] = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.delivering, id); s.mu.Unlock() }()
	if needsPreparation {
		prepareErr := prepareConversation(ctx, executor, &prepared, d, h.Wake.Allocation, recovering)
		s.mu.Lock()
		lw, loadErr := s.store.loadWork(id)
		if loadErr != nil {
			s.mu.Unlock()
			return true, loadErr
		}
		lt := lw.findTask(t.ID)
		if lt == nil || lt.Handoff == nil {
			s.mu.Unlock()
			return true, nil
		}
		lh := lt.Handoff
		if prepareErr != nil {
			lh.Wake.recordPreparationError(prepareErr, recovering)
			s.warnError("task wake preparation failed", prepareErr, "repo", lw.Repo, "number", lw.Pull.Number)
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
		s.refreshWorkflow(lw, s.now())
		err = s.store.saveWork(lw)
		s.mu.Unlock()
		if err != nil || prepareErr != nil {
			return true, err
		}
		if current, err := s.observedHeadCurrent(ctx); err != nil || !current {
			return true, err
		}
	}
	// Recheck after allocation. A delayed wake is harmless because claim performs
	// the same eligibility check under the state lock.
	s.mu.Lock()
	current, err := s.store.loadWork(id)
	latest := current.findTask(t.ID)
	eligible := err == nil && matchesObservation(ctx, current) && latest != nil && current.Task == latest && latest.Handoff != nil && latest.Handoff.State == "pending" && !current.executionBlocked()
	s.mu.Unlock()
	if err != nil || !eligible {
		return true, err
	}
	sendErr := waker.WakeTask(ctx, prepared, d)
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err = s.store.loadWork(id)
	if err != nil {
		return true, err
	}
	latest = current.findTask(t.ID)
	if latest != nil && latest.Handoff != nil {
		latest.Handoff.Wake.Error = ""
		if sendErr != nil {
			latest.Handoff.Wake.Error = fmt.Sprintf("wake not confirmed; will retry: %v", sendErr)
		} else {
			now := s.now()
			latest.Handoff.Wake.AcceptedAt = &now
		}
		s.refreshWorkflow(current, s.now())
		return true, s.store.saveWork(current)
	}
	return true, nil
}

func (w *TaskWake) recordPreparationError(err error, recovering bool) {
	if recovering {
		w.Error = fmt.Sprintf("find reserved conversation: %v", err)
		if w.AllocationError != "" {
			w.Error = w.AllocationError + "; " + w.Error
		}
		return
	}
	w.Error = "conversation preparation: " + err.Error()
	w.AllocationError = w.Error
}

func supportsAllocation(provider execution.Executor) bool {
	_, allocates := provider.(execution.ConversationAllocator)
	return allocates && execution.Supports(provider, "placement", true) && execution.Supports(provider, "prepare", true)
}

func reservePreparation(provider execution.Executor, state *TaskWake, route *execution.Route, task execution.Delivery, placement *execution.ReviewPlacement, now time.Time) error {
	if placement != nil {
		route.Parent, route.Session = placement.Parent, placement.Session
	}
	if route.Session != "" {
		state.Preparation = "prepared"
		return nil
	}
	if !execution.Supports(provider, "prepare", provider != nil) {
		return fmt.Errorf("execution provider does not support prepare")
	}
	if v, ok := provider.(execution.PreparationValidator); ok {
		if err := v.ValidatePreparation(*route, task); err != nil {
			return err
		}
	}
	if placement != nil {
		if !supportsAllocation(provider) {
			return fmt.Errorf("execution provider does not support allocation recovery")
		}
		stamp := now.UTC()
		state.Allocation = &execution.ConversationAllocation{ID: uuid.NewString(), Parent: route.Parent, ReservedAt: &stamp}
	}
	state.Preparation = "preparing"
	return nil
}

func prepareConversation(ctx context.Context, provider execution.Executor, route *execution.Route, task execution.Delivery, allocation *execution.ConversationAllocation, recovering bool) error {
	if route.Session == "" {
		var err error
		if allocation == nil {
			route.Session, err = provider.Prepare(ctx, *route, task)
		} else {
			mode := "create"
			if recovering {
				mode = "find"
			}
			route.Parent = allocation.Parent
			route.Session, err = provider.(execution.ConversationAllocator).PrepareAllocation(ctx, *route, task, *allocation, mode)
		}
		if err != nil {
			return err
		}
	}
	if !route.Valid(false) {
		return fmt.Errorf("provider returned an invalid destination")
	}
	return provider.Validate(*route, false)
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
