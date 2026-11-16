package ink

import (
	"context"
	"fmt"
	"strings"
	"time"

	"ink-commons/internal/execution"
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
	if ctx.Err() != nil {
		return nil
	}
	id := key(repo.Repo, number)
	s.mu.Lock()
	st, err := s.store.load()
	if err != nil {
		s.mu.Unlock()
		return err
	}
	w := st.Work[id]
	if w == nil || w.Phase != "approved" {
		s.mu.Unlock()
		return nil
	}
	sha := w.Pull.Head.SHA
	index := -1
	for i, n := range w.Notifications {
		if n.SHA == sha && n.Event == "review_approved" {
			if !n.RequireACK || (n.State != "unknown" && n.State != "awaiting_ack") || time.Now().Before(n.RetryAt) {
				s.mu.Unlock()
				return nil
			}
			index = i
			break
		}
	}
	// derive selected the latest qualifying review for this SHA and reviewer.
	var review Review
	for _, r := range w.Reviews {
		if r.SHA == sha && strings.EqualFold(r.User.Login, w.Reviewer.GitHubLogin) && (r.State == "APPROVED" || r.State == "CHANGES_REQUESTED") {
			review = r
		}
	}
	if review.State != "APPROVED" {
		s.mu.Unlock()
		return nil
	}
	n := NotificationRun{Notification: execution.Notification{
		EventID: fmt.Sprintf("%s:%s:review_approved", id, sha), Event: "review_approved",
		Repository: repo.Repo, Number: number, URL: w.Pull.URL, SHA: sha,
		Reviewer: review.User.Login, ReviewID: review.ID, ReviewURL: review.URL,
	}, State: "unknown", StartedAt: time.Now()}
	if index >= 0 {
		n = w.Notifications[index]
		if w.Developer.Route == nil || n.Route == nil || *w.Developer.Route != *n.Route {
			w.Notifications[index].State = "superseded"
			w.Notifications[index].FinishedAt = time.Now()
			w.Notifications[index].RetryAt = time.Time{}
			err = s.store.save(st)
			s.mu.Unlock()
			return err
		}
	}
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
		if notifier == nil {
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
			n.RetryAt = time.Now().Add(delay)
		}
	} else {
		n.FinishedAt = time.Now()
	}
	if index < 0 {
		w.Notifications = append(w.Notifications, n)
	} else {
		w.Notifications[index] = n
	}
	w.refreshWorkflow(time.Now(), s.config.Reliability.defaults())
	err = s.store.save(st)
	s.mu.Unlock()
	if err != nil || n.State != "unknown" {
		return err
	}
	err = notifier.Notify(ctx, *n.Route, n.Notification)
	n.State, n.FinishedAt = "accepted", time.Now()
	if n.RequireACK {
		n.State, n.FinishedAt = "awaiting_ack", time.Time{}
	}
	if err != nil {
		n.State, n.Error = "unknown", "notification delivery failed; external outcome may be unknown: "+err.Error()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, loadErr := s.store.load()
	if loadErr != nil {
		return loadErr
	}
	if live := st.Work[id]; live != nil {
		for i := range live.Notifications {
			if live.Notifications[i].EventID == n.EventID && live.Notifications[i].Attempts == n.Attempts && live.Notifications[i].State == "unknown" {
				live.Notifications[i] = n
			}
		}
		live.refreshWorkflow(time.Now(), s.config.Reliability.defaults())
	}
	return s.store.save(st)
}
