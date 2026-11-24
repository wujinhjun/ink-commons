//go:build linux || darwin

package ink

import (
	"context"
	"fmt"
	"time"

	"github.com/wujinhjun/ink-commons/internal/execution"
)

// A nil hook disables delivery. An empty object uses the development conversation.
type TaskBlockedHook struct {
	Route *execution.Route `json:"route,omitempty"`
}

func (h *TaskBlockedHook) Validate() error {
	if h != nil && h.Route != nil && !h.Route.Valid(false) {
		return fmt.Errorf("task_blocked.route must have provider, target and session")
	}
	return nil
}

type BlockedNotice struct {
	Route    *execution.Route `json:"route,omitempty"`
	Session  string           `json:"session,omitempty"`
	Attempts int              `json:"attempts"`
	RetryAt  time.Time        `json:"retry_at,omitempty"`
	SentAt   time.Time        `json:"sent_at,omitempty"`
	Error    string           `json:"error,omitempty"`
}

// The notice shares the transaction with the blocked handoff. Historical
// blocked tasks are not backfilled when an operator enables the hook.
func (s *Service) reserveBlockedNotice(w *Work, task *Task) {
	repo, ok := s.repo(w.Repo)
	if !ok || repo.TaskBlocked == nil {
		return
	}
	source := reviewSource(w)
	if task.ConfigSnapshot != nil {
		source = task.ConfigSnapshot.Source
	}
	n := &BlockedNotice{}
	n.Route = cloneRoute(repo.TaskBlocked.Route)
	if n.Route == nil {
		n.Route = cloneRoute(source)
	}
	if n.Route == nil || !n.Route.Valid(false) {
		n.Route = nil
		n.Error = "no resolved destination; configure task_blocked.route"
	}
	task.Handoff.BlockedNotice = n
}

func (s *Service) reconcileBlockedWork(ctx context.Context, id string) error {
	w, err := s.store.loadWork(id)
	if err != nil {
		return err
	}
	if w == nil {
		return nil
	}
	for _, t := range w.executionTasks() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if blockedNoticeDue(t, s.now()) {
			if err := s.deliverBlockedNotice(ctx, t.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

func blockedNoticeDue(t *Task, now time.Time) bool {
	return hasBlockedNotice(t) && !now.Before(t.Handoff.BlockedNotice.RetryAt)
}

func hasBlockedNotice(t *Task) bool {
	if t == nil || t.Handoff == nil || t.Handoff.State != "blocked" || t.Handoff.Submission == nil {
		return false
	}
	n := t.Handoff.BlockedNotice
	return n != nil && (n.Route != nil || n.Session != "") && n.SentAt.IsZero()
}

func (s *Service) deliverBlockedNotice(ctx context.Context, taskID string) error {
	s.mu.Lock()
	w, t, err := s.store.loadTask(taskID)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	now := s.now()
	if !blockedNoticeDue(t, now) {
		s.mu.Unlock()
		return nil
	}
	n := t.Handoff.BlockedNotice
	n.Attempts++
	// Persist the retry lease before I/O. Concurrent polls and daemon
	// restarts must not immediately repeat an uncertain send.
	n.RetryAt = now.Add(min(2*time.Minute<<min(n.Attempts-1, 5), time.Hour))
	if n.Route == nil {
		// Upgrade an older implicit destination only when the frozen source
		// identifies it. A bare session cannot select a named provider.
		if source := t.ConfigSnapshot; source != nil && source.Source != nil && source.Source.Session == n.Session {
			n.Route = cloneRoute(source.Source)
		} else {
			n.Error, n.Session = "older blocked notice has no frozen provider route", ""
			err := s.store.saveWork(w)
			s.mu.Unlock()
			return err
		}
	}
	attempt, route := n.Attempts, *n.Route
	delivery := deliveryFor(w, t, "", 0)
	reason := t.Handoff.Submission.Reason
	err = s.store.saveWork(w)
	s.mu.Unlock()
	if err != nil {
		return err
	}

	provider := s.providers[route.Provider]
	notifier, ok := provider.(execution.TaskBlockedNotifier)
	sendErr := fmt.Errorf("execution provider does not support blocked_notice")
	if execution.Supports(provider, "blocked_notice", ok) {
		sendErr = notifier.NotifyTaskBlocked(ctx, route, delivery, reason)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	w, t, err = s.store.loadTask(taskID)
	if err != nil {
		return err
	}
	if t == nil || t.Handoff == nil || t.Handoff.BlockedNotice == nil || t.Handoff.BlockedNotice.Attempts != attempt {
		return nil
	}
	n = t.Handoff.BlockedNotice
	n.Error = ""
	if sendErr != nil {
		n.Error = sendErr.Error()
	} else {
		n.SentAt, n.RetryAt = s.now(), time.Time{}
	}
	return s.store.saveWork(w)
}
