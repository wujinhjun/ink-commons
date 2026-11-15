package ink

import (
	"context"
	"fmt"
	"strings"
	"time"

	"ink-commons/internal/execution"
)

// NotificationRun is a durable, once-per-PR/SHA delivery attempt. Unknown and
// failed attempts are not replayed: the external request may already have acted.
type NotificationRun struct {
	execution.Notification
	Route      *execution.Route `json:"route,omitempty"`
	State      string           `json:"state"`
	StartedAt  time.Time        `json:"started_at"`
	FinishedAt time.Time        `json:"finished_at,omitempty"`
	Error      string           `json:"error,omitempty"`
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
	for _, n := range w.Notifications {
		if n.SHA == sha && n.Event == "review_approved" {
			s.mu.Unlock()
			return nil
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
	var notifier execution.Notifier
	if w.Developer.Route == nil {
		n.State = "manual"
	} else {
		route := *w.Developer.Route
		n.Route = &route
		provider := s.providers[route.Provider]
		notifier, _ = provider.(execution.Notifier)
		if notifier == nil {
			n.State, n.Error = "unsupported", "execution provider does not support notifications"
		} else if err := execution.ValidateDestination(s.providers, route, false); err != nil {
			n.State, n.Error = "failed", "invalid notification destination: "+err.Error()
		}
	}
	if n.State != "unknown" {
		n.FinishedAt = time.Now()
	}
	// Reserve before contacting the runtime, including across concurrent polls
	// and daemon restarts. Notifications do not create/replace execution tasks.
	w.Notifications = append(w.Notifications, n)
	if n.State != "unknown" {
		w.refreshWorkflow(time.Now(), s.config.Reliability.defaults())
	}
	err = s.store.save(st)
	s.mu.Unlock()
	if err != nil || n.State != "unknown" {
		return err
	}
	err = notifier.Notify(ctx, *n.Route, n.Notification)
	n.State, n.FinishedAt = "accepted", time.Now()
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
			if live.Notifications[i].EventID == n.EventID {
				live.Notifications[i] = n
			}
		}
		live.refreshWorkflow(time.Now(), s.config.Reliability.defaults())
	}
	return s.store.save(st)
}
