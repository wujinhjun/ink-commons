//go:build linux || darwin

package ink

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wujinhjun/ink-commons/internal/execution"
	"github.com/wujinhjun/ink-commons/internal/execution/adaptertest"
	"github.com/wujinhjun/ink-commons/internal/execution/httpagent"
)

func TestHTTPProviderDispatchWithoutCCConnect(t *testing.T) {
	ctx := context.Background()
	sha := strings.Repeat("a", 40)
	repo := Repository{
		Repo: "acme/project", ReviewerLogin: "reviewer",
	}
	var requests []string
	client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		var input struct {
			Version   int
			Operation string
			Route     execution.Route
			Task      execution.Delivery
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Fatal(err)
		}
		requests = append(requests, input.Operation)
		if input.Version != 1 || input.Task.SHA != sha || input.Task.Number != 3 || input.Route.Target != "reviewer" {
			t.Fatalf("unexpected request: %+v", input)
		}
		w := httptest.NewRecorder()
		switch input.Operation {
		case "prepare":
			if input.Route.Session != "" {
				t.Fatal("new route already has a session")
			}
			reply(w, 200, map[string]string{"session": "review-session/3"})
		case "deliver":
			if input.Route.Session != "review-session/3" {
				t.Fatal("delivery uses wrong session")
			}
			reply(w, 200, map[string]string{"status": "accepted"})
		default:
			t.Fatal("unknown provider operation")
		}
		return w.Result(), nil
	})}
	executor, err := httpagent.New(client, "http://127.0.0.1:5000/execute", "", []string{"prepare", "deliver"}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	service, err := newFixtureService(t, Config{Repositories: []Repository{repo}}, filepath.Join(t.TempDir(), stateDatabase), map[string]execution.Executor{"http": executor})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { service.Close() })
	var pull Pull
	pull.Number, pull.State, pull.Head.SHA, pull.Draft = 3, "open", sha, true
	if err := service.observe(repo, pull, nil); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("POST", "/v1/work/acme/project/3/link", strings.NewReader(`{"github_login":"human-dev"}`))
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatalf("link failed: %s", response.Body)
	}
	assign := func() {
		t.Helper()
		request := httptest.NewRequest("POST", "/v1/work/acme/project/3/assign", strings.NewReader(`{"role":"reviewer","github_login":"reviewer","route":{"provider":"http","target":"reviewer"}}`))
		response := httptest.NewRecorder()
		service.Handler().ServeHTTP(response, request)
		if response.Code != 200 {
			t.Fatalf("assign failed: %s", response.Body)
		}
	}
	assign()
	if err := service.dispatch(ctx, repo, 3); err != nil || len(requests) != 0 {
		t.Fatalf("draft was dispatched: %v, %v", requests, err)
	}
	pull.Draft = false
	if err := service.observe(repo, pull, nil); err != nil {
		t.Fatal(err)
	}
	if err := service.dispatch(ctx, repo, 3); err != nil {
		t.Fatal(err)
	}
	assign()
	if err := service.dispatch(ctx, repo, 3); err != nil {
		t.Fatal(err)
	}
	st, err := loadFixture(service.store)
	if err != nil {
		t.Fatal(err)
	}
	work := st.Work["acme/project#3"]
	if work.Task.Delivery != "accepted" || work.Reviewer.Route.Session != "review-session/3" || strings.Join(requests, ",") != "prepare,deliver" {
		t.Fatalf("unexpected work or duplicate delivery: %+v, %v", work, requests)
	}
}

func TestReviewPlacementHonorsExplicitParent(t *testing.T) {
	for _, mode := range []string{"repository default", "assignment", "human takeover", "explicit parent", "unreadable source"} {
		takeover := mode == "human takeover"
		parent := "77"
		switch mode {
		case "explicit parent":
			parent = "66"
		case "repository default":
			parent = "88"
		}
		t.Run(mode, func(t *testing.T) {
			var operations []string
			client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
				operations = append(operations, r.Method+" "+r.URL.Path)
				w := httptest.NewRecorder()
				switch r.Method + " " + r.URL.Path {
				case "GET /api/v10/channels/123":
					if mode == "unreadable source" {
						reply(w, 404, map[string]string{"error": "deleted source"})
						break
					}
					reply(w, 200, map[string]any{"id": "123", "type": 11, "parent_id": "77", "name": "Development"})
				case "GET /api/v10/channels/77":
					reply(w, 200, map[string]any{"id": "77", "type": 0})
				case "POST /api/v10/channels/" + parent + "/threads":
					var body struct {
						Name string
						Type int
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Fatal(err)
					}
					if !strings.HasPrefix(body.Name, "Review acme/project #3 · ") || body.Type != 11 {
						t.Fatalf("bad fallback: %+v", body)
					}
					reply(w, 201, map[string]string{"id": "99"})
				case "POST /hook":
					reply(w, 200, map[string]string{"status": "accepted"})
				default:
					t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				return w.Result(), nil
			})}
			executor, err := adaptertest.Discord(t, "chat", client, "http://127.0.0.1:9111/hook", "hook-token", "discord-token")
			if err != nil {
				t.Fatal(err)
			}
			repo := Repository{Repo: "acme/project", ReviewerLogin: "reviewer", ReviewerRoute: &execution.Route{Provider: "chat", Target: "reviewer", Parent: "88"}}
			service, err := newFixtureService(t, Config{Repositories: []Repository{repo}}, filepath.Join(t.TempDir(), stateDatabase), map[string]execution.Executor{"chat": executor})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { service.Close() })
			var pull Pull
			pull.Number, pull.State, pull.Draft, pull.Head.SHA = 3, "open", true, strings.Repeat("a", 40)
			if err := service.observe(repo, pull, nil); err != nil {
				t.Fatal(err)
			}
			request := func(action, body string) {
				t.Helper()
				response := httptest.NewRecorder()
				service.Handler().ServeHTTP(response, httptest.NewRequest("POST", "/v1/work/acme/project/3/"+action, strings.NewReader(body)))
				if response.Code != 200 {
					t.Fatalf("%s: %d %s", action, response.Code, response.Body)
				}
			}
			request("link", `{"route":{"provider":"chat","target":"developer","session":"discord:123"}}`)
			if takeover {
				request("assign", `{"role":"developer","github_login":"human-dev"}`)
			}
			if mode != "repository default" {
				parentJSON := ""
				if mode == "explicit parent" {
					parentJSON = `,"parent":"66"`
				}
				request("assign", `{"role":"reviewer","github_login":"assigned-reviewer","route":{"provider":"chat","target":"reviewer"`+parentJSON+`}}`)
			}
			pull.Draft = false
			if err := service.observe(repo, pull, nil); err != nil {
				t.Fatal(err)
			}
			if err := service.dispatch(context.Background(), repo, 3); err != nil {
				t.Fatal(err)
			}
			st, err := loadFixture(service.store)
			if err != nil {
				t.Fatal(err)
			}
			work := st.Work["acme/project#3"]
			if mode == "unreadable source" {
				if work.Reviewer.Route.Session != "" || !strings.Contains(work.Task.Error, "adapter placement: upstream HTTP 404") || strings.Join(operations, ",") != "GET /api/v10/channels/123" {
					t.Fatalf("unreadable source fell back to a different channel: %+v, %v", work.Task, operations)
				}
				return
			}
			if work.Task.Delivery != "accepted" || work.Reviewer.Route.Session != "discord:99" {
				t.Fatalf("dispatch failed: %+v", work.Task)
			}
			if strings.Join(operations, ",") != "GET /api/v10/channels/123,GET /api/v10/channels/77,POST /api/v10/channels/"+parent+"/threads,POST /hook" {
				t.Fatalf("wrong review placement: %v", operations)
			}
			if repo.ReviewerRoute.Parent != "88" {
				t.Fatal("mutated repository config")
			}
			if takeover && work.Developer.Route != nil {
				t.Fatal("test did not exercise human takeover")
			}
		})
	}
}

func TestReviewerAssignmentRejectsMissingSourceBeforeMutation(t *testing.T) {
	executor, err := adaptertest.Discord(t, "chat", &http.Client{}, "", "", "discord-token")
	if err != nil {
		t.Fatal(err)
	}
	repo := Repository{Repo: "acme/project", ReviewerLogin: "default-reviewer", ReviewerRoute: &execution.Route{Provider: "chat", Target: "reviewer", Parent: "88"}, Review: &ReviewFeatures{RequireSameChannel: true}}
	service, err := newFixtureService(t, Config{Repositories: []Repository{repo}}, filepath.Join(t.TempDir(), stateDatabase), map[string]execution.Executor{"chat": executor})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { service.Close() })
	var pull Pull
	pull.Number, pull.State, pull.Head.SHA = 3, "open", strings.Repeat("a", 40)
	if err := service.observe(repo, pull, nil); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, httptest.NewRequest("POST", "/v1/work/acme/project/3/assign", strings.NewReader(`{"role":"reviewer","github_login":"reviewer","route":{"provider":"chat","target":"reviewer"}}`)))
	if response.Code != 409 || !strings.Contains(response.Body.String(), "link the PR first") {
		t.Fatalf("wrong unlinked error precedence: %d %s", response.Code, response.Body)
	}
	response = httptest.NewRecorder()
	service.Handler().ServeHTTP(response, httptest.NewRequest("POST", "/v1/work/acme/project/3/link", strings.NewReader(`{"github_login":"human-dev"}`)))
	if response.Code != 200 {
		t.Fatalf("link failed: %s", response.Body)
	}
	response = httptest.NewRecorder()
	service.Handler().ServeHTTP(response, httptest.NewRequest("POST", "/v1/work/acme/project/3/assign", strings.NewReader(`{"role":"reviewer","github_login":"reviewer","route":{"provider":"chat","target":"reviewer"}}`)))
	if response.Code != 400 || !strings.Contains(response.Body.String(), "resolved development route") {
		t.Fatalf("missing immediate validation: %d %s", response.Code, response.Body)
	}
	st, err := loadFixture(service.store)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Work["acme/project#3"].Reviewer; got.GitHubLogin != "default-reviewer" || got.Route.Parent != "88" || got.Route.Session != "" {
		t.Fatal("rejected assignment changed reviewer")
	}
}

func TestDirectHTTPAllocationUsesFindAfterRestart(t *testing.T) {
	now := time.Now().UTC()
	route := &execution.Route{Provider: "review-http", Target: "reviewer"}
	repo := Repository{Repo: "acme/project", ReviewerLogin: "reviewer", ReviewerRoute: route}
	cfg := Config{Repositories: []Repository{repo}, Now: func() time.Time { return now }}
	db := filepath.Join(t.TempDir(), "state", stateDatabase)
	if err := InitializeState(filepath.Dir(db)); err != nil {
		t.Fatal(err)
	}
	var s *Service
	var operations []string
	var reserved *execution.ConversationAllocation
	var preparedTask execution.Delivery
	var pull Pull
	pull.Number, pull.State, pull.Head.SHA = 3, "open", strings.Repeat("a", 40)
	client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		var req httpagent.Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		operations = append(operations, req.Operation+req.Mode)
		response := httptest.NewRecorder()
		w := notificationWork(t, s, repo, pull)
		switch req.Operation + req.Mode {
		case "placement":
			reply(response, 200, httpagent.Response{Placement: &execution.ReviewPlacement{Parent: "room-a"}})
		case "preparecreate":
			reserved = req.Allocation
			preparedTask = *req.Task
			if reserved == nil || reserved.ID == "" || req.Route.Parent != "room-a" ||
				w.Task.Preparation.Allocation.ID != reserved.ID || len(w.Task.Runs) != 0 {
				t.Fatal("create was sent without a durable reservation")
			}
			return nil, context.DeadlineExceeded
		case "preparefind":
			if req.Allocation == nil || req.Allocation.ID != reserved.ID ||
				!req.Allocation.ReservedAt.Equal(*reserved.ReservedAt) ||
				req.Route.Parent != reserved.Parent || req.Task.TaskID != preparedTask.TaskID {
				t.Fatal("find changed the reservation")
			}
			reply(response, 200, httpagent.Response{Session: "session-recovered"})
		case "deliver":
			if w.Reviewer.Route.Session != "session-recovered" || req.Route != *w.Reviewer.Route ||
				req.Task.TaskID != preparedTask.TaskID || req.Task.Attempt != 1 || req.Task.AttemptID == "" {
				t.Fatal("delivery used an unpersisted destination or wrong attempt")
			}
			reply(response, 200, httpagent.Response{Status: "accepted"})
		default:
			t.Fatalf("unexpected operation %+v", req)
		}
		return response.Result(), nil
	})}
	executor, err := httpagent.New(client, "http://127.0.0.1:5000/execute", "", []string{"placement", "prepare", "deliver"}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	open := func() *Service {
		t.Helper()
		service, err := NewService(cfg, db, map[string]execution.Executor{route.Provider: executor})
		if err != nil {
			t.Fatal(err)
		}
		return service
	}
	s = open()
	if err := s.observe(repo, pull, nil); err != nil {
		t.Fatal(err)
	}
	out := httptest.NewRecorder()
	s.Handler().ServeHTTP(out, httptest.NewRequest("POST", "/v1/work/acme/project/3/link", strings.NewReader(`{"github_login":"developer"}`)))
	if out.Code != 200 {
		t.Fatalf("link failed: %s", out.Body)
	}
	if err := s.dispatch(context.Background(), repo, pull.Number); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	s = open()
	defer s.Close()
	if err := s.dispatch(context.Background(), repo, pull.Number); err != nil {
		t.Fatal(err)
	}
	w := notificationWork(t, s, repo, pull)
	if strings.Join(operations, ",") != "placement,preparecreate,preparefind,deliver" || w.Task.Delivery != "accepted" {
		t.Fatalf("unexpected recovery: %v, %+v", operations, w.Task)
	}
	out = httptest.NewRecorder()
	s.Handler().ServeHTTP(out, httptest.NewRequest("POST", "/v1/work/acme/project/3/assign", strings.NewReader(`{"role":"reviewer","github_login":"reviewer","route":{"provider":"review-http","target":"reviewer"}}`)))
	if out.Code != 200 || notificationWork(t, s, repo, pull).Task.ID != preparedTask.TaskID {
		t.Fatalf("repeated assignment reset the prepared task: %d %s", out.Code, out.Body)
	}
	pull.Head.SHA = strings.Repeat("b", 40)
	if err := s.observe(repo, pull, nil); err != nil {
		t.Fatal(err)
	}
	if w := notificationWork(t, s, repo, pull); w.Reviewer.Route.Session != "session-recovered" {
		t.Fatal("new SHA discarded the session on the same configured route")
	}
}

func TestInvalidPreparationLeavesPendingTaskOpenToProviderCorrection(t *testing.T) {
	for _, mode := range []string{"legacy", "cooperative"} {
		t.Run(mode, func(t *testing.T) {
			operation := "deliver"
			if mode == "cooperative" {
				operation = "wake"
			}
			var requests []string
			client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
				var req httpagent.Request
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Fatal(err)
				}
				requests = append(requests, req.Route.Provider+":"+req.Operation)
				response := httptest.NewRecorder()
				switch req.Operation {
				case "placement":
					reply(response, 200, httpagent.Response{Placement: &execution.ReviewPlacement{Parent: "room"}})
				case "prepare":
					reply(response, 200, httpagent.Response{Session: "new-session"})
				case operation:
					if req.Route.Session != "new-session" {
						t.Fatalf("unexpected dispatch route: %+v", req.Route)
					}
					reply(response, 200, httpagent.Response{Status: "accepted"})
				default:
					t.Fatalf("unexpected adapter operation: %s", req.Operation)
				}
				return response.Result(), nil
			})}
			newProvider := func(capabilities ...string) execution.Executor {
				t.Helper()
				adapter, err := httpagent.New(client, "http://127.0.0.1:5000/execute", "", capabilities, false, false)
				if err != nil {
					t.Fatal(err)
				}
				return adapter
			}
			repo := Repository{Repo: "acme/project", TaskMode: mode, ReviewerLogin: "reviewer",
				ReviewerRoute: &execution.Route{Provider: "unready", Target: "reviewer"}}
			s, _ := centralConfigService(t, Config{Repositories: []Repository{repo}}, map[string]execution.Executor{
				"unready": newProvider("placement", operation),
				"working": newProvider("placement", "prepare", operation),
			})
			var pull Pull
			pull.Number, pull.State, pull.Head.SHA = 3, "open", strings.Repeat("a", 40)
			if err := s.observe(repo, pull, nil); err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			s.Handler().ServeHTTP(response, httptest.NewRequest("POST", "/v1/work/acme/project/3/link",
				strings.NewReader(`{"github_login":"developer"}`)))
			if response.Code != http.StatusOK {
				t.Fatalf("link failed: %s", response.Body)
			}
			dispatch := func() *Work {
				t.Helper()
				if err := s.dispatch(context.Background(), repo, pull.Number); err != nil {
					t.Fatal(err)
				}
				return notificationWork(t, s, repo, pull)
			}
			failed := dispatch()
			task := failed.Task
			diagnostic := task.Error
			if task.Handoff != nil {
				diagnostic = task.Handoff.Wake.Error
				if task.Handoff.Wake.Request != nil || task.Handoff.Wake.Attempts != 0 {
					t.Fatal("failed validation persisted a wake request")
				}
			}
			if failed.RoundConfig != nil || task.ConfigSnapshot != nil || task.Preparation != nil ||
				task.Delivery != "pending" || !strings.Contains(diagnostic, "does not support prepare") {
				t.Fatalf("failed validation froze a pending task: %+v", task)
			}
			configAPI(t, s, "POST", repo.Repo, "reviewer_route", `{"provider":"working","target":"reviewer"}`, http.StatusOK)
			corrected := dispatch()
			if corrected.Task.ID != task.ID || corrected.Task.ConfigSnapshot == nil ||
				corrected.Reviewer.Route.Provider != "working" || corrected.Reviewer.Route.Session != "new-session" {
				t.Fatalf("correction did not dispatch the same task: %+v", corrected.Task)
			}
			if got := strings.Join(requests, ","); got != "unready:placement,working:placement,working:prepare,working:"+operation {
				t.Fatalf("unexpected adapter requests: %s", got)
			}
		})
	}
}
