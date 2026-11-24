//go:build linux || darwin

package ink

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/wujinhjun/ink-commons/internal/execution"
)

// NotificationRun persists transport attempts separately from agent receipt.
// Legacy providers retain their original once-only delivery behavior.
type NotificationRun struct {
	execution.Notification
	Route      *execution.Route `json:"route,omitempty"`
	State      string           `json:"state"`
	StartedAt  time.Time        `json:"started_at"`
	FinishedAt time.Time        `json:"finished_at,omitempty"`
	Error      string           `json:"error,omitempty"`
	RequireACK bool             `json:"require_ack,omitempty"`
	Attempts   int              `json:"attempts,omitempty"`
	RetryAt    time.Time        `json:"retry_at,omitempty"`
}

func (s *Service) notifyApproved(ctx context.Context, repo Repository, number int) error {
	return s.notifyEvent(ctx, repo, number, "review_approved")
}

func (s *Service) notifyMerged(ctx context.Context, repo Repository, number int) error {
	return s.notifyEvent(ctx, repo, number, "pr_merged")
}

func (s *Service) notifyEvent(ctx context.Context, repo Repository, number int, event string) error {
	if ctx.Err() != nil {
		return nil
	}
	id := key(repo.Repo, number)
	s.mu.Lock()
	w, err := s.store.loadWork(id)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	if w == nil || !matchesObservation(ctx, w) {
		s.mu.Unlock()
		return nil
	}
	if effective, ok := s.repo(repo.Repo); ok {
		repo = effective
	} else {
		s.mu.Unlock()
		return nil
	}
	if w.supersedeStaleNotifications(s.now()) {
		s.refreshWorkflow(w, s.now())
		if err := s.store.saveWork(w); err != nil {
			s.mu.Unlock()
			return err
		}
	}
	if w.Phase != "approved" && w.Phase != "merged" {
		s.mu.Unlock()
		return nil
	}
	sha := w.Pull.Head.SHA
	index := -1
	for i, n := range w.Notifications {
		if n.SHA == sha && n.Event == event {
			if !n.pendingDelivery() || s.now().Before(n.RetryAt) {
				s.mu.Unlock()
				return nil
			}
			index = i
			break
		}
	}
	var review Review
	if index < 0 {
		// Only create approval notices while approved. Completion notices are
		// already reserved with the merge journal and enter through index >= 0.
		if event != "review_approved" || w.Phase != "approved" {
			s.mu.Unlock()
			return nil
		}
		// derive selected the latest qualifying review for this SHA and reviewer.
		for _, r := range w.Reviews {
			if r.SHA == sha && strings.EqualFold(r.User.Login, w.Reviewer.GitHubLogin) && (r.State == "APPROVED" || r.State == "CHANGES_REQUESTED") {
				review = r
			}
		}
		if review.State != "APPROVED" {
			s.mu.Unlock()
			return nil
		}
	}
	n := NotificationRun{Notification: execution.Notification{
		EventID: fmt.Sprintf("%s:%s:%s", id, sha, event), Event: event,
		Repository: repo.Repo, Number: number, URL: w.Pull.URL, SHA: sha,
		Reviewer: review.User.Login, ReviewID: review.ID, ReviewURL: review.URL,
	}, State: "unknown", StartedAt: s.now()}
	if index >= 0 {
		n = w.Notifications[index]
		if w.Developer.Route == nil || n.Route == nil || *w.Developer.Route != *n.Route {
			w.Notifications[index].State = "superseded"
			w.Notifications[index].FinishedAt = s.now()
			w.Notifications[index].RetryAt = time.Time{}
			s.refreshWorkflow(w, s.now())
			err = s.store.saveWork(w)
			s.mu.Unlock()
			return err
		}
	}
	n.Merge = mergeStatus(w, repo, n.SHA)
	var notifier execution.Notifier
	if index < 0 && w.Developer.Route == nil {
		n.State = "manual"
	} else {
		if index < 0 {
			route := *w.Developer.Route
			n.Route = &route
		}
		provider := s.providers[n.Route.Provider]
		notifier, _ = provider.(execution.Notifier)
		if index < 0 {
			capability, ok := provider.(execution.NotificationACKProvider)
			n.RequireACK = ok && capability.NotificationACKRequired()
		}
		if !execution.Supports(provider, "notify", notifier != nil) {
			n.State, n.Error = "unsupported", "execution provider does not support notifications"
		} else if err := execution.ValidateDestination(s.providers, *n.Route, false); err != nil {
			n.State, n.Error = "failed", "invalid notification destination: "+err.Error()
		}
	}
	if n.State == "unknown" || n.State == "awaiting_ack" {
		n.State, n.Error, n.FinishedAt = "unknown", "", time.Time{}
		n.Attempts++
		if n.RequireACK {
			// Persist the retry lease before I/O, so concurrent polls and restart
			// cannot immediately replay a request whose response was lost.
			delay := 2 * time.Minute
			for attempt := 1; attempt < n.Attempts && delay < time.Hour; attempt++ {
				delay *= 2
			}
			if delay > time.Hour {
				delay = time.Hour
			}
			n.RetryAt = s.now().Add(delay)
		}
	} else {
		n.FinishedAt = s.now()
	}
	if index < 0 {
		w.Notifications = append(w.Notifications, n)
	} else {
		w.Notifications[index] = n
	}
	s.refreshWorkflow(w, s.now())
	err = s.store.saveWork(w)
	s.mu.Unlock()
	if err != nil || n.State != "unknown" {
		return err
	}
	err = notifier.Notify(ctx, *n.Route, n.Notification, n.RequireACK)
	n.State, n.FinishedAt = "accepted", s.now()
	if n.RequireACK {
		n.State, n.FinishedAt = "awaiting_ack", time.Time{}
	}
	if err != nil {
		n.State, n.Error = "unknown", "notification delivery failed; external outcome may be unknown: "+err.Error()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	live, loadErr := s.store.loadWork(id)
	if loadErr != nil {
		return loadErr
	}
	if live != nil {
		for i := range live.Notifications {
			if live.Notifications[i].EventID == n.EventID && live.Notifications[i].Attempts == n.Attempts && live.Notifications[i].State == "unknown" {
				live.Notifications[i] = n
			}
		}
		s.refreshWorkflow(live, s.now())
		return s.store.saveWork(live)
	}
	return nil
}

func (n NotificationRun) pendingDelivery() bool {
	return (n.State == "unknown" || n.State == "awaiting_ack") &&
		(n.RequireACK || (n.Event == "pr_merged" && n.Attempts == 0))
}

// A same-head reopen can make the original approval receipt eligible again.
func (w *Work) approvalReceiptPaused(n *NotificationRun) bool {
	return w.Pull.State == "closed" && !w.Pull.Merged && n.Event == "review_approved" && n.SHA == w.Pull.Head.SHA
}

// A changed head invalidates the old receipt lease.
func (w *Work) supersedeStaleNotifications(now time.Time) bool {
	changed := false
	for i := range w.Notifications {
		n := &w.Notifications[i]
		if n.pendingDelivery() && n.SHA != w.Pull.Head.SHA {
			n.State, n.FinishedAt, n.RetryAt = "superseded", now, time.Time{}
			changed = true
		}
	}
	return changed
}

// 与确认合入的账本更新在同一事务预留事件，不回填升级前的历史合并。
func (s *Service) reserveMergedNotification(w *Work, repo Repository, run AutoMergeRun) {
	if !w.Linked || run.State != "merged" || run.SHA != w.Pull.Head.SHA {
		return
	}
	id := fmt.Sprintf("%s:%s:pr_merged", keyForWork(w), run.SHA)
	for _, n := range w.Notifications {
		if n.EventID == id {
			return
		}
	}
	n := NotificationRun{Notification: execution.Notification{
		EventID: id, Event: "pr_merged", Repository: w.Repo, Number: w.Pull.Number, URL: w.Pull.URL,
		SHA: run.SHA, Reviewer: run.Reviewer, ReviewID: run.ReviewID, Merge: mergeStatus(w, repo, run.SHA),
	}, State: "unknown", StartedAt: s.now()}
	for _, r := range w.Reviews {
		if r.ID == run.ReviewID {
			n.ReviewURL = r.URL
			break
		}
	}
	if w.Developer.Route == nil {
		n.State = "manual"
		n.FinishedAt = s.now()
	} else {
		route := *w.Developer.Route
		n.Route = &route
		provider, ok := s.providers[route.Provider].(execution.NotificationACKProvider)
		n.RequireACK = ok && provider.NotificationACKRequired()
	}
	w.Notifications = append(w.Notifications, n)
}
