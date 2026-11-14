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
