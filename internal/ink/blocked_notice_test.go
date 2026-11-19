package ink

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	w.Task.ConfigSnapshot = nil // Historical task without a frozen source.
	f.s.reserveBlockedNotice(w, w.Task)
	if n := w.Task.Handoff.BlockedNotice; n == nil || n.Session != "discord:456" {
		t.Fatalf("unexpected default destination: %+v", n)
	}
}

func TestBlockedNoticeKeepsClaimedTaskDestinationAfterDeveloperReassignment(t *testing.T) {
	for _, tc := range []struct {
		name     string
		override string
		want     string
	}{
		{"original development conversation", "", "discord:123"},
		{"explicit channel override", "555", "discord:555"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHandoffFixture(t)
			f.s.providers["cc-connect"] = &blockedNoticeExecutor{}
			if _, err := f.s.changeConfig(f.repo.Repo, "task_blocked", jsonValue(TaskBlockedHook{DiscordChannelID: tc.override}), false); err != nil {
				t.Fatal(err)
			}
			assign := func(session string) {
				t.Helper()
				route := &execution.Route{Provider: "cc-connect", Target: "developer", Session: session}
				body := jsonValue(workChange{Role: "developer", GitHubLogin: "developer", Route: route})
				req := httptest.NewRequest(http.MethodPost, "/v1/work/acme/project/42/assign", bytes.NewReader(body))
				out := httptest.NewRecorder()
				f.s.Handler().ServeHTTP(out, req)
				if out.Code != http.StatusOK {
					t.Fatalf("assign got %d: %s", out.Code, out.Body)
				}
			}

			assign("discord:123")
			task := f.work(t).Task
			f.claim(t, task, claimA, 200)
			assign("discord:999")
			w := f.work(t)
			if w.Developer.Route.Session != "discord:999" || w.Task.ID != task.ID ||
				w.Task.ConfigSnapshot == nil || w.Task.ConfigSnapshot.Source == nil ||
				w.Task.ConfigSnapshot.Source.Session != "discord:123" {
				t.Fatal("expected a reassigned developer and an unchanged reviewer task source")
			}
			f.submit(t, task, claimA, HandoffSubmission{
				RequestID: "blocked", Handoff: true, Outcome: "blocked", Reason: "GitHub unavailable",
			}, 200)
			n := f.work(t).Task.Handoff.BlockedNotice
			if n == nil || n.Session != tc.want {
				t.Fatalf("expected notice to %s, got %+v", tc.want, n)
			}
		})
	}
}
