package ink

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ink-commons/internal/execution"
	"ink-commons/internal/execution/ccconnect"
)

func TestTaskWakeRetryDelay(t *testing.T) {
	for _, tc := range []struct {
		attempts int
		minutes  int
	}{
		{1, 1}, {2, 1}, {3, 1}, {4, 2}, {5, 3}, {6, 4},
		{61, 59}, {62, 60}, {63, 60}, {1000000, 60},
	} {
		if got := taskWakeRetryDelay(tc.attempts); got != time.Duration(tc.minutes)*time.Minute {
			t.Errorf("after wake %d: got %s, want %d minutes", tc.attempts, got, tc.minutes)
		}
	}
}

func TestDiscordAllocationRecoversAfterRestart(t *testing.T) {
	for _, outcome := range []string{"active", "archived", "missing", "ambiguous"} {
		t.Run(outcome, func(t *testing.T) {
			var creates, wakes, archiveReads, sourceReads int
			var title string
			var s *Service
			repo := Repository{
				Repo: "acme/project", TaskMode: "cooperative", ReviewerLogin: "reviewer",
				ReviewerRoute: &execution.Route{Provider: ccconnect.Name, Target: "reviewer"},
			}
			p := Pull{Number: 42, State: "open", URL: "https://github.com/acme/project/pull/42"}
			p.Head.SHA = strings.Repeat("a", 40)
			client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
				out := httptest.NewRecorder()
				thread := func(id, parent, owner string) map[string]any {
					return map[string]any{
						"id": id, "type": 11, "name": title, "parent_id": parent, "owner_id": owner,
						"thread_metadata": map[string]string{"archive_timestamp": "2026-10-21T12:00:00Z"},
					}
				}
				switch r.Method + " " + r.URL.Path {
				case "POST /api/v10/channels/77/threads":
					creates++
					var body struct{ Name string }
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Fatal(err)
					}
					title = body.Name
					wake := notificationWork(t, s, repo, p).Task.Handoff.Wake
					if wake.Preparation != "preparing" || wake.Allocation == nil || wake.Allocation.Name != title || wake.Allocation.Parent != "77" {
						t.Fatalf("Discord request was not reserved: %+v", wake)
					}
					return nil, io.EOF
				case "GET /api/v10/channels/123":
					sourceReads++
					if sourceReads == 1 {
						return nil, io.EOF
					}
					reply(out, 200, map[string]any{"id": "123", "type": 11, "parent_id": "77", "name": "Development"})
				case "GET /api/v10/channels/77":
					reply(out, 200, map[string]any{"id": "77", "guild_id": "55", "type": 0})
				case "GET /api/v10/users/@me":
					reply(out, 200, map[string]string{"id": "12"})
				case "GET /api/v10/guilds/55/threads/active":
					threads := []map[string]any{thread("111", "88", "12"), thread("222", "77", "15")}
					if outcome == "active" || outcome == "ambiguous" {
						threads = append(threads, thread("999", "77", "12"))
					}
					reply(out, 200, map[string]any{"threads": threads})
				case "GET /api/v10/channels/77/threads/archived/public":
					archiveReads++
					threads := []map[string]any{}
					more := outcome == "archived" && r.URL.Query().Get("before") == ""
					switch {
					case more:
						threads = append(threads, thread("333", "77", "15"))
					case outcome == "archived":
						if r.URL.Query().Get("before") != "2026-10-21T12:00:00Z" {
							t.Fatalf("incorrect cursor %s", r.URL)
						}
						threads = append(threads, thread("999", "77", "12"))
					case outcome == "ambiguous":
						threads = append(threads, thread("888", "77", "12"))
					}
					reply(out, 200, map[string]any{"threads": threads, "has_more": more})
				case "POST /hook":
					wakes++
					wake := notificationWork(t, s, repo, p).Task.Handoff
					if wake.Wake.Preparation != "prepared" || wake.Assignee.Route.Session != "discord:999" {
						t.Fatalf("wake sent before recovered thread was saved: %+v", wake)
					}
					reply(out, 200, map[string]string{"status": "accepted"})
				default:
					t.Fatalf("unexpected request %s %s", r.Method, r.URL)
				}
				return out.Result(), nil
			})}
			e, err := ccconnect.New(client, "http://127.0.0.1:9111/hook", "hook", "discord")
			if err != nil {
				t.Fatal(err)
			}
			db := filepath.Join(t.TempDir(), stateDatabase)
			open := func() *Service {
				service, err := NewService(Config{Repositories: []Repository{repo}}, db, map[string]execution.Executor{ccconnect.Name: e})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { service.Close() })
				return service
			}
			s = open()
			st, err := s.store.load()
			if err != nil {
				t.Fatal(err)
			}
			id := key(repo.Repo, p.Number)
			st.Work[id] = &Work{
				Repo: repo.Repo, Pull: p, Linked: true,
				Developer: Assignee{Route: &execution.Route{Provider: ccconnect.Name, Target: "developer", Session: "discord:123"}},
				Reviewer:  Assignee{GitHubLogin: "reviewer", Route: repo.ReviewerRoute},
			}
			if err := s.store.save(st); err != nil {
				t.Fatal(err)
			}
			if err := s.observe(repo, p, nil); err != nil {
				t.Fatal(err)
			}
			dispatch := func() {
				t.Helper()
				if err := s.dispatch(context.Background(), repo, p.Number); err != nil {
					t.Fatal(err)
				}
			}
			retry := func() {
				t.Helper()
				st, err := s.store.load()
				if err != nil {
					t.Fatal(err)
				}
				st.Work[id].Task.Handoff.Wake.RetryAt = time.Time{}
				if err := s.store.save(st); err != nil {
					t.Fatal(err)
				}
				dispatch()
			}
			dispatch()
			w := notificationWork(t, s, repo, p)
			wake := w.Task.Handoff.Wake
			if creates != 0 || wakes != 0 || wake.Allocation != nil || wake.Preparation != "" ||
				wake.PreparationAttempts != 1 || !strings.Contains(wake.Error, "read development conversation 123: upstream transport failure: EOF") {
				t.Fatalf("development lookup should be retryable without reserving a write: %+v", wake)
			}
			dispatch()
			if sourceReads != 1 {
				t.Fatal("development lookup ignored its retry delay")
			}
			retry()
			w = notificationWork(t, s, repo, p)
			if creates != 1 || wakes != 0 || !strings.Contains(w.Task.Handoff.Wake.Error, "create Discord thread in channel 77: upstream transport failure: EOF") {
				t.Fatalf("expected ambiguous creation with original error and no wake: %+v", w.Task.Handoff.Wake)
			}
			s.Close()
			s = open()
			retry()
			w = notificationWork(t, s, repo, p)
			h := w.Task.Handoff
			if creates != 1 || h.Wake.RecoveryAttempts != 1 || !strings.Contains(h.Wake.AllocationError, "EOF") {
				t.Fatalf("wrong recovery bookkeeping: %+v, creates=%d", h.Wake, creates)
			}
			if outcome == "missing" || outcome == "ambiguous" {
				for i := 0; i < maxAllocationRecoveryAttempts; i++ {
					retry()
				}
				h = notificationWork(t, s, repo, p).Task.Handoff
				if creates != 1 || wakes != 0 || h.Wake.RecoveryAttempts != maxAllocationRecoveryAttempts || h.Wake.canRecover() ||
					archiveReads != maxAllocationRecoveryAttempts || !strings.Contains(h.Wake.Error, "EOF") {
					t.Fatalf("unresolved allocation should stay fenced: %+v, creates=%d, wakes=%d", h.Wake, creates, wakes)
				}
				return
			}
			if wakes != 1 || h.Wake.Error != "" || h.Wake.AcceptedAt == nil || h.State != "pending" ||
				!strings.Contains(w.Workflow.Reason, "Wake accepted") {
				t.Fatalf("expected an accepted wake and a pending claim: %+v, workflow=%+v", h, w.Workflow)
			}
			(&handoffFixture{s: s}).claim(t, w.Task, strings.Repeat("a", 32), 200)
			w = notificationWork(t, s, repo, p)
			if w.Task.Handoff.State != "claimed" || w.Task.Handoff.ClaimedAt.IsZero() {
				t.Fatalf("expected a durable reviewer claim: %+v", w.Task.Handoff)
			}
		})
	}
}

func TestDiscordPreparationDistinguishesRejectedAndAmbiguousCreates(t *testing.T) {
	for _, tc := range []struct {
		status               execution.HTTPStatusError
		wantRetryCreate      bool
		wantReadOnlyRecovery bool
	}{
		{status: http.StatusTooManyRequests, wantRetryCreate: true},
		{status: http.StatusForbidden},
		{status: http.StatusBadGateway, wantReadOnlyRecovery: true},
	} {
		t.Run(tc.status.Error(), func(t *testing.T) {
			wake := TaskWake{Preparation: "preparing", Allocation: &execution.ConversationAllocation{Parent: "77", Name: "Review"}}
			wake.recordPreparationError(fmt.Errorf("create Discord thread: %w", tc.status), false)
			if (wake.Preparation == "" && wake.Allocation == nil) != tc.wantRetryCreate ||
				wake.canRecover() != tc.wantReadOnlyRecovery || !strings.Contains(wake.AllocationError, tc.status.Error()) {
				t.Fatalf("incorrect allocation handling: %+v", wake)
			}
		})
	}
}
