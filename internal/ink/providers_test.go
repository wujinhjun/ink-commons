package ink

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"ink-commons/internal/execution"
	"ink-commons/internal/execution/ccconnect"
	"ink-commons/internal/execution/httpagent"
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
	executor, err := httpagent.New(client, "http://127.0.0.1:5000/execute", "")
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(Config{Repositories: []Repository{repo}}, filepath.Join(t.TempDir(), stateDatabase), map[string]execution.Executor{httpagent.Name: executor})
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
	st, err := service.store.load()
	if err != nil {
		t.Fatal(err)
	}
	work := st.Work["acme/project#3"]
	if work.Task.Delivery != "accepted" || work.Reviewer.Route.Session != "review-session/3" || strings.Join(requests, ",") != "prepare,deliver" {
		t.Fatalf("unexpected work or duplicate delivery: %+v, %v", work, requests)
	}
}

func TestReviewPlacementIgnoresRepositoryParent(t *testing.T) {
	for _, mode := range []string{"repository default", "assignment", "human takeover", "explicit parent", "unreadable source"} {
		takeover := mode == "human takeover"
		parent := "77"
		if mode == "explicit parent" {
			parent = "66"
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
					if body.Name != "Review acme/project #3 · Development" || body.Type != 11 {
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
			executor, err := ccconnect.New(client, "http://127.0.0.1:9111/hook", "hook-token", "discord-token")
			if err != nil {
				t.Fatal(err)
			}
			repo := Repository{Repo: "acme/project", ReviewerLogin: "reviewer", ReviewerRoute: &execution.Route{Provider: ccconnect.Name, Target: "reviewer", Parent: "88"}}
			service, err := NewService(Config{Repositories: []Repository{repo}}, filepath.Join(t.TempDir(), stateDatabase), map[string]execution.Executor{ccconnect.Name: executor})
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
			request("link", `{"route":{"provider":"cc-connect","target":"developer","session":"discord:123"}}`)
			if takeover {
				request("assign", `{"role":"developer","github_login":"human-dev"}`)
			}
			if mode != "repository default" {
				parentJSON := ""
				if mode == "explicit parent" {
					parentJSON = `,"parent":"66"`
				}
				request("assign", `{"role":"reviewer","github_login":"assigned-reviewer","route":{"provider":"cc-connect","target":"reviewer"`+parentJSON+`}}`)
			}
			pull.Draft = false
			if err := service.observe(repo, pull, nil); err != nil {
				t.Fatal(err)
			}
			if err := service.dispatch(context.Background(), repo, 3); err != nil {
				t.Fatal(err)
			}
			st, err := service.store.load()
			if err != nil {
				t.Fatal(err)
			}
			work := st.Work["acme/project#3"]
			if mode == "unreadable source" {
				if work.Reviewer.Route.Session != "" || !strings.Contains(work.Task.Error, "read development conversation") || strings.Join(operations, ",") != "GET /api/v10/channels/123" {
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
	executor, err := ccconnect.New(&http.Client{}, "", "", "discord-token")
	if err != nil {
		t.Fatal(err)
	}
	repo := Repository{Repo: "acme/project", ReviewerLogin: "default-reviewer", ReviewerRoute: &execution.Route{Provider: ccconnect.Name, Target: "reviewer", Parent: "88"}}
	service, err := NewService(Config{Repositories: []Repository{repo}}, filepath.Join(t.TempDir(), stateDatabase), map[string]execution.Executor{ccconnect.Name: executor})
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
	service.Handler().ServeHTTP(response, httptest.NewRequest("POST", "/v1/work/acme/project/3/assign", strings.NewReader(`{"role":"reviewer","github_login":"reviewer","route":{"provider":"cc-connect","target":"reviewer"}}`)))
	if response.Code != 409 || !strings.Contains(response.Body.String(), "link the PR first") {
		t.Fatalf("wrong unlinked error precedence: %d %s", response.Code, response.Body)
	}
	response = httptest.NewRecorder()
	service.Handler().ServeHTTP(response, httptest.NewRequest("POST", "/v1/work/acme/project/3/link", strings.NewReader(`{"github_login":"human-dev"}`)))
	if response.Code != 200 {
		t.Fatalf("link failed: %s", response.Body)
	}
	response = httptest.NewRecorder()
	service.Handler().ServeHTTP(response, httptest.NewRequest("POST", "/v1/work/acme/project/3/assign", strings.NewReader(`{"role":"reviewer","github_login":"reviewer","route":{"provider":"cc-connect","target":"reviewer"}}`)))
	if response.Code != 400 || !strings.Contains(response.Body.String(), "Discord development conversation") {
		t.Fatalf("missing immediate validation: %d %s", response.Code, response.Body)
	}
	st, err := service.store.load()
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Work["acme/project#3"].Reviewer; got.GitHubLogin != "default-reviewer" || got.Route.Parent != "" || got.Route.Session != "" {
		t.Fatal("rejected assignment changed reviewer")
	}
}
