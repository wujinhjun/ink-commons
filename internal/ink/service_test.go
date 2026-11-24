//go:build linux || darwin

package ink

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wujinhjun/ink-commons/internal/execution"
)

type testTransport func(*http.Request) (*http.Response, error)

func (f testTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type recordedDelivery struct {
	route execution.Route
	task  execution.Delivery
}

type testExecutor struct {
	prepared      int
	preparedTasks []execution.Delivery
	deliveries    []recordedDelivery
}

func (*testExecutor) Validate(execution.Route, bool) error { return nil }

func (e *testExecutor) Prepare(_ context.Context, _ execution.Route, task execution.Delivery) (string, error) {
	e.prepared++
	e.preparedTasks = append(e.preparedTasks, task)
	return "review-session", nil
}

// The workflow fixture represents a runtime that has joined its tools before
// publishing a final result. Reliability failure scenarios use separate fixtures.
func (e *testExecutor) Status(_ context.Context, _ execution.Route, d execution.Delivery) (execution.Receipt, error) {
	now := time.Now().UTC()
	return execution.Receipt{TaskID: d.TaskID, AttemptID: d.AttemptID, Sequence: 1, State: "succeeded", StartedAt: now, HeartbeatAt: now, FinishedAt: now}, nil
}
func (e *testExecutor) Stop(ctx context.Context, r execution.Route, d execution.Delivery) (execution.Receipt, error) {
	return e.Status(ctx, r, d)
}

func (e *testExecutor) Deliver(_ context.Context, route execution.Route, task execution.Delivery) error {
	e.deliveries = append(e.deliveries, recordedDelivery{route: route, task: task})
	return nil
}

func TestReviewLoopAndHumanTakeover(t *testing.T) {
	sha1 := strings.Repeat("a", 40)
	sha2 := strings.Repeat("b", 40)
	sha3 := strings.Repeat("c", 40)
	var pull Pull
	payload := fmt.Sprintf(`{"number":42,"html_url":"https://github.com/acme/project/pull/42","state":"open","draft":true,"head":{"ref":"feat/export","sha":%q,"repo":{"full_name":"acme/project"}}}`, sha1)
	if err := json.Unmarshal([]byte(payload), &pull); err != nil {
		t.Fatal(err)
	}
	var reviews []Review
	executor := &testExecutor{}
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/project/pulls":
			if pull.State == "closed" {
				reply(w, 200, []Pull{})
				return
			}
			reply(w, 200, []Pull{pull})
		case "/repos/acme/project/pulls/42":
			reply(w, 200, pull)
		case "/repos/acme/project/pulls/42/reviews":
			reply(w, 200, reviews)
		default:
			t.Errorf("unexpected upstream endpoint %s", r.URL.Path)
			w.WriteHeader(404)
		}
	})

	repo := Repository{Repo: "acme/project", ReviewerLogin: "reviewer-bot", ReviewerRoute: &execution.Route{Provider: "test", Target: "reviewer"}}
	cfg := Config{Repositories: []Repository{repo}}
	path := filepath.Join(t.TempDir(), "state", stateDatabase)
	if err := InitializeState(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	makeService := func() *Service {
		service, err := NewService(cfg, path, map[string]execution.Executor{"test": executor})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { service.Close() })
		service.up.github = "https://stub.invalid"
		transport := testTransport(func(r *http.Request) (*http.Response, error) {
			w := httptest.NewRecorder()
			mock.ServeHTTP(w, r)
			return w.Result(), nil
		})
		service.up.http.Transport = transport
		return service
	}
	s := makeService()
	request := func(method, path string, body []byte, status int) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest(method, path, bytes.NewReader(body)))
		if w.Code != status {
			t.Fatalf("%s %s: got %d, want %d, %s", method, path, w.Code, status, w.Body)
		}
		return w
	}
	work := func(phase string) Work {
		t.Helper()
		var w Work
		if err := json.Unmarshal(request("GET", "/v1/work/acme/project/42", nil, 200).Body.Bytes(), &w); err != nil {
			t.Fatal(err)
		}
		if w.Phase != phase {
			t.Fatalf("phase %s, want %s", w.Phase, phase)
		}
		return w
	}
	poll := func() {
		t.Helper()
		if err := s.reconcileExecutions(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := s.pollRepo(context.Background(), repo); err != nil {
			t.Fatal(err)
		}
	}
	addReview := func(id int64, sha, login, status string) {
		r := Review{ID: id, SHA: sha, State: status, URL: pull.URL + "#pullrequestreview-" + fmt.Sprint(id)}
		r.User.Login = login
		reviews = append(reviews, r)
	}

	poll()
	work("unclaimed")
	pushes, err := ReadPush("git@github.com:Acme/Project.git", execution.Route{Provider: "test", Target: "developer", Session: "dev-session"}, strings.NewReader("refs/heads/local "+sha1+" refs/heads/feat/export "+strings.Repeat("0", 40)+"\n"))
	if err != nil || len(pushes) != 1 {
		t.Fatalf("parsed pushes %v: %v", pushes, err)
	}
	data, err := json.Marshal(pushes)
	if err != nil {
		t.Fatal(err)
	}
	request("POST", "/v1/pushes", data, 202)
	poll()
	if w := work("draft"); w.Origin == nil || w.Origin.Session != "dev-session" || executor.prepared != 0 {
		t.Fatalf("draft routing: %+v", w)
	}

	pull.Draft = false
	poll()
	if w := work("review_pending"); w.Task.Delivery != "accepted" || w.Reviewer.Route.Session != "review-session" || len(executor.deliveries) != 1 || executor.deliveries[0].route.Session != "review-session" || executor.deliveries[0].task.SHA != sha1 {
		t.Fatalf("review dispatch: %+v, %v", w, executor.deliveries)
	}

	if source := executor.preparedTasks[0].Source; source == nil || source.Session != "dev-session" {
		t.Fatalf("missing preparation source: %+v", source)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s = makeService()
	poll()
	if executor.prepared != 1 || len(executor.deliveries) != 1 {
		t.Fatal("restart repeated review delivery")
	}
	addReview(1, sha1, "someone-else", "APPROVED")
	addReview(2, sha2, "reviewer-bot", "APPROVED")
	poll()
	work("review_pending")
	addReview(3, sha1, "reviewer-bot", "CHANGES_REQUESTED")
	poll()
	if w := work("changes_requested"); w.Task.Delivery != "accepted" || len(executor.deliveries) != 2 || executor.deliveries[1].route.Session != "dev-session" || executor.deliveries[1].task.ReviewID != 3 {
		t.Fatalf("developer dispatch: %+v, %v", w, executor.deliveries)
	}

	request("POST", "/v1/work/acme/project/42/assign", []byte(`{"role":"developer","github_login":"human-dev"}`), 200)
	w := work("changes_requested")
	if w.Task.Delivery != "manual" || w.Developer.Route != nil || w.Origin == nil || w.Origin.Session != "dev-session" {
		t.Fatalf("developer takeover: %+v", w)
	}
	pull.Head.SHA = sha3
	poll()
	if w := work("review_pending"); w.Task.SHA != sha3 || executor.prepared != 1 || len(executor.deliveries) != 3 || executor.deliveries[2].route.Session != "review-session" || executor.deliveries[2].task.SHA != sha3 {
		t.Fatalf("re-review: %+v, %v", w, executor.deliveries)
	}

	request("POST", "/v1/work/acme/project/42/assign", []byte(`{"role":"reviewer","github_login":"human-reviewer"}`), 200)
	if w := work("review_pending"); w.Task.Delivery != "manual" {
		t.Fatalf("reviewer takeover: %+v", w)
	}
	addReview(4, sha3, "reviewer-bot", "APPROVED")
	poll()
	work("review_pending")
	addReview(5, sha3, "human-reviewer", "APPROVED")
	poll()
	work("approved")
	pull.State, pull.Merged = "closed", true
	poll()
	work("merged")
	if executor.prepared != 1 || len(executor.deliveries) != 3 {
		t.Fatal("manual assignments triggered bot deliveries")
	}
}
