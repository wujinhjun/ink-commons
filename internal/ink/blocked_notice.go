package ink

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/wujinhjun/ink-commons/internal/execution"
	"github.com/wujinhjun/ink-commons/internal/execution/ccconnect"
)

// A nil hook disables delivery. An empty object uses the development conversation.
type TaskBlockedHook struct {
	DiscordChannelID string `json:"discord_channel_id,omitempty"`
}

func (h *TaskBlockedHook) Validate() error {
	if h != nil && strings.Trim(h.DiscordChannelID, "0123456789") != "" {
		return fmt.Errorf("task_blocked.discord_channel_id must be a Discord channel ID")
	}
	return nil
}

type BlockedNotice struct {
	Session  string    `json:"session,omitempty"`
	Attempts int       `json:"attempts"`
	RetryAt  time.Time `json:"retry_at,omitempty"`
	SentAt   time.Time `json:"sent_at,omitempty"`
	Error    string    `json:"error,omitempty"`
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
	switch {
	case repo.TaskBlocked.DiscordChannelID != "":
		n.Session = "discord:" + repo.TaskBlocked.DiscordChannelID
	case source != nil && source.Provider == ccconnect.Name && source.Session != "":
		n.Session = source.Session
	default:
		n.Error = "no linked Discord development conversation; configure task_blocked.discord_channel_id"
	}
	task.Handoff.BlockedNotice = n
}

func (s *Service) reconcileBlockedNotices(ctx context.Context) error {
	s.mu.Lock()
	st, err := s.store.load()
	s.mu.Unlock()
	if err != nil {
		return err
	}
	for _, w := range st.Work {
		for _, t := range w.executionTasks() {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if blockedNoticeDue(t, time.Now()) {
				if err := s.deliverBlockedNotice(ctx, t.ID); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func blockedNoticeDue(t *Task, now time.Time) bool {
	if t == nil || t.Handoff == nil || t.Handoff.State != "blocked" || t.Handoff.Submission == nil {
		return false
	}
	n := t.Handoff.BlockedNotice
	return n != nil && n.Session != "" && n.SentAt.IsZero() && !now.Before(n.RetryAt)
}

func (s *Service) deliverBlockedNotice(ctx context.Context, taskID string) error {
	s.mu.Lock()
	st, err := s.store.load()
	if err != nil {
		s.mu.Unlock()
		return err
	}
	w, t := findTask(st, taskID)
	now := time.Now()
	if !blockedNoticeDue(t, now) {
		s.mu.Unlock()
		return nil
	}
	n := t.Handoff.BlockedNotice
	n.Attempts++
	// Persist the retry lease before I/O. Concurrent polls and daemon
	// restarts must not immediately repeat an uncertain send.
	n.RetryAt = now.Add(min(2*time.Minute<<min(n.Attempts-1, 5), time.Hour))
	attempt, session := n.Attempts, n.Session
	delivery := deliveryFor(w, t, "", 0)
	reason := t.Handoff.Submission.Reason
	err = s.store.save(st)
	s.mu.Unlock()
	if err != nil {
		return err
	}

	notifier, ok := s.providers[ccconnect.Name].(execution.TaskBlockedNotifier)
	sendErr := fmt.Errorf("cc-connect provider does not support direct blocked notifications")
	if ok {
		sendErr = notifier.NotifyTaskBlocked(ctx, session, delivery, reason)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	st, err = s.store.load()
	if err != nil {
		return err
	}
	_, t = findTask(st, taskID)
	if t == nil || t.Handoff == nil || t.Handoff.BlockedNotice == nil || t.Handoff.BlockedNotice.Attempts != attempt {
		return nil
	}
	n = t.Handoff.BlockedNotice
	n.Error = ""
	if sendErr != nil {
		n.Error = sendErr.Error()
	} else {
		n.SentAt, n.RetryAt = time.Now(), time.Time{}
	}
	return s.store.save(st)
}
