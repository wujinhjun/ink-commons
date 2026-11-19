package ink

import (
	"context"
	"fmt"
	"testing"
	"time"

	"ink-commons/internal/execution"
)

type blockedNoticeExecutor struct {
	testExecutor
	calls   int
	session string
	task    execution.Delivery
	reason  string
	err     error
}

func (e *blockedNoticeExecutor) NotifyTaskBlocked(_ context.Context, session string, task execution.Delivery, reason string) error {
	e.calls++
	e.session, e.task, e.reason = session, task, reason
	return e.err
}

func TestBlockedNoticeRetriesAfterRestart(t *testing.T) {
	f := newHandoffFixture(t)
	if _, err := f.s.changeConfig(f.repo.Repo, "task_blocked", jsonValue(TaskBlockedHook{DiscordChannelID: "123"}), false); err != nil {
		t.Fatal(err)
	}
	task := f.work(t).Task
	f.claim(t, task, claimA, 200)
	result := HandoffSubmission{RequestID: "blocked", Handoff: true, Outcome: "blocked", Reason: "GitHub unavailable"}
	f.submit(t, task, claimA, result, 200)
	f.submit(t, task, claimA, result, 200)

	executor := &blockedNoticeExecutor{err: fmt.Errorf("Discord unavailable")}
	f.s.providers["cc-connect"] = executor
	run := func() {
		t.Helper()
		if err := f.s.reconcileBlockedNotices(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	run()
	run()
	n := f.work(t).Task.Handoff.BlockedNotice
	if n == nil || n.Attempts != 1 || n.Error != "Discord unavailable" || n.RetryAt.IsZero() || executor.calls != 1 {
		t.Fatalf("expected one leased failed attempt, got %+v and %d calls", n, executor.calls)
	}

	f.restart(t)
	f.s.providers["cc-connect"] = executor
	run()
	if executor.calls != 1 {
		t.Fatal("restart ignored the persisted retry lease")
	}
	st, err := f.s.store.load()
	if err != nil {
		t.Fatal(err)
	}
	st.Work[key(f.repo.Repo, 42)].Task.Handoff.BlockedNotice.RetryAt = time.Time{}
	if err := f.s.store.save(st); err != nil {
		t.Fatal(err)
	}
	executor.err = nil
	run()
	run()
	n = f.work(t).Task.Handoff.BlockedNotice
	if executor.calls != 2 || n.SentAt.IsZero() || n.Error != "" ||
		executor.session != "discord:123" || executor.task.TaskID != task.ID || executor.reason != result.Reason {
		t.Fatalf("unexpected delivery: %+v, attempts: %d", n, executor.calls)
	}
}

func TestBlockedNoticeOptInAndDefaultDestination(t *testing.T) {
	f := newHandoffFixture(t)
	task := f.work(t).Task
	f.claim(t, task, claimA, 200)
	result := HandoffSubmission{RequestID: "blocked", Handoff: true, Outcome: "blocked", Reason: "GitHub unavailable"}
	f.submit(t, task, claimA, result, 200)
	if f.work(t).Task.Handoff.BlockedNotice != nil {
		t.Fatal("blocked notifications should be opt-in")
	}
	if _, err := f.s.changeConfig(f.repo.Repo, "task_blocked", jsonValue(TaskBlockedHook{}), false); err != nil {
		t.Fatal(err)
	}
	f.submit(t, task, claimA, result, 200)
	if f.work(t).Task.Handoff.BlockedNotice != nil {
		t.Fatal("repeated handoff backfilled a historical notification")
	}
	w := f.work(t)
	w.Developer.Route = &execution.Route{Provider: "cc-connect", Target: "developer", Session: "discord:456"}
	f.s.reserveBlockedNotice(w, w.Task.Handoff)
	if n := w.Task.Handoff.BlockedNotice; n == nil || n.Session != "discord:456" {
		t.Fatalf("unexpected default destination: %+v", n)
	}
}
