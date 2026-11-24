//go:build linux || darwin

package ink

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wujinhjun/ink-commons/internal/execution"
	"github.com/wujinhjun/ink-commons/internal/execution/ccconnect"
)

func TestRequireSameReviewChannel(t *testing.T) {
	cases := []struct {
		name, parent, session, wantSession, wantError string
		reuse, allocate, disabled, restoreAccess      bool
	}{
		{name: "default placement", wantSession: "discord:999", allocate: true},
		{name: "reuse developer thread", reuse: true, wantSession: "discord:123"},
		{name: "retry default placement after access restored", wantSession: "discord:999", allocate: true, restoreAccess: true, wantError: "read development conversation"},
		{name: "retry thread reuse after access restored", reuse: true, wantSession: "discord:123", restoreAccess: true, wantError: "reuse development thread"},
		{name: "same parent", parent: "77", wantSession: "discord:999", allocate: true},
		{name: "same channel session", session: "discord:456:22", wantSession: "discord:456:22"},
		{name: "cross-channel parent", parent: "88", wantError: "expected development channel 77, got review channel 88"},
		{name: "cross-channel session", session: "discord:789", wantError: "expected development channel 77, got review channel 88"},
		{name: "destination is unavailable", session: "discord:555", wantError: "read review conversation"},
		{name: "disabled permits cross-channel session", session: "discord:789", wantSession: "discord:789", disabled: true},
	}
	for _, mode := range []string{"legacy", "cooperative"} {
		for _, tc := range cases {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				var allocations, deliveries, sourceReads int
				sourceReadable := !tc.restoreAccess
				client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
					out := httptest.NewRecorder()
					switch r.Method + " " + r.URL.Path {
					case "GET /api/v10/channels/123":
						sourceReads++
						if sourceReadable {
							reply(out, 200, map[string]any{"id": "123", "type": 11, "parent_id": "77", "name": "Development"})
						} else {
							out.WriteHeader(http.StatusForbidden)
						}
					case "GET /api/v10/channels/77":
						reply(out, 200, map[string]any{"id": "77", "type": 0})
					case "GET /api/v10/channels/456":
						reply(out, 200, map[string]any{"id": "456", "type": 11, "parent_id": "77"})
					case "GET /api/v10/channels/789":
						reply(out, 200, map[string]any{"id": "789", "type": 11, "parent_id": "88"})
					case "GET /api/v10/channels/555":
						out.WriteHeader(http.StatusForbidden)
					case "POST /api/v10/channels/77/threads":
						allocations++
						reply(out, 200, map[string]string{"id": "999"})
					case "POST /hook":
						deliveries++
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
				route := &execution.Route{Provider: ccconnect.Name, Target: "reviewer", Parent: tc.parent, Session: tc.session}
				repo := Repository{
					Repo: "acme/project", TaskMode: mode, ReviewerLogin: "reviewer",
					ReviewerRoute: route,
					Review:        &ReviewFeatures{RequireSameChannel: !tc.disabled, ReuseDevelopmentThread: tc.reuse},
				}
				s, err := NewService(Config{Repositories: []Repository{repo}}, filepath.Join(t.TempDir(), stateDatabase), map[string]execution.Executor{ccconnect.Name: e})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { s.Close() })
				p := Pull{Number: 42, State: "open", URL: "https://github.com/acme/project/pull/42"}
				p.Head.SHA = strings.Repeat("a", 40)
				st, err := s.store.load()
				if err != nil {
					t.Fatal(err)
				}
				st.Work[key(repo.Repo, p.Number)] = &Work{
					Repo: repo.Repo, Pull: p, Linked: true,
					Developer: Assignee{Route: &execution.Route{Provider: ccconnect.Name, Target: "developer", Session: "discord:123"}},
					Reviewer:  Assignee{GitHubLogin: "reviewer", Route: route},
				}
				if err := s.store.save(st); err != nil {
					t.Fatal(err)
				}
				if err := s.observe(repo, p, nil); err != nil {
					t.Fatal(err)
				}
				w := notificationWork(t, s, repo, p)
				f := &handoffFixture{s: s}
				token := strings.Repeat("a", 32)
				if mode == "cooperative" && !tc.disabled && tc.session != "" {
					f.claim(t, w.Task, token, 409)
				}
				if err := s.dispatch(context.Background(), repo, p.Number); err != nil {
					t.Fatal(err)
				}
				w = notificationWork(t, s, repo, p)
				if tc.wantError != "" {
					diagnostic := w.Task.Error
					if mode == "cooperative" {
						diagnostic = w.Task.Handoff.Wake.Error
						if w.Task.Handoff.Wake.Attempts != 0 || w.Task.Handoff.Wake.Preparation != "" {
							t.Fatal("invalid route reserved a wake or allocation")
						}
						if tc.session != "" {
							f.claim(t, w.Task, token, 409)
						}
					}
					if !strings.Contains(diagnostic, tc.wantError) || deliveries != 0 || allocations != 0 ||
						w.Task.Attempts != 0 || w.Task.Delivery != "pending" {
						t.Fatalf("invalid route was dispatched or lost its diagnostic: %+v", w.Task)
					}
					if !tc.restoreAccess {
						return
					}
					if sourceReads != 1 {
						t.Fatalf("first dispatch attempted %d source reads", sourceReads)
					}
					sourceReadable = true
					if err := s.dispatch(context.Background(), repo, p.Number); err != nil {
						t.Fatal(err)
					}
					w = notificationWork(t, s, repo, p)
					if sourceReads != 2 {
						t.Fatalf("retry resolved the source more than once: %d total reads", sourceReads)
					}
					if mode == "legacy" && (w.Task.Attempts != 1 || w.Task.Delivery != "accepted" || w.Task.Error != "") {
						t.Fatalf("retry did not start the first legacy attempt: %+v", w.Task)
					}
					if mode == "cooperative" && (w.Task.Handoff.Wake.Attempts != 1 || w.Task.Handoff.Wake.Error != "") {
						t.Fatalf("retry did not start the first cooperative wake: %+v", w.Task.Handoff.Wake)
					}
				}
				if deliveries != 1 || allocations > 1 || (allocations == 1) != tc.allocate || w.Reviewer.Route.Session != tc.wantSession {
					t.Fatalf("delivery=%d allocation=%d reviewer=%+v", deliveries, allocations, w.Reviewer)
				}
				if mode == "cooperative" {
					f.claim(t, w.Task, token, 200)
				}
			})
		}
	}
}
