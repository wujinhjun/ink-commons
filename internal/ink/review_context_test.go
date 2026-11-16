package ink

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"ink-commons/internal/execution"
	"ink-commons/internal/execution/ccconnect"
)

type historyExecutor struct {
	testExecutor
	calls  int
	source execution.Route
	before string
	onRead func()
}

func (e *historyExecutor) ReadConversation(_ context.Context, r execution.Route, before string) (execution.ConversationPage, error) {
	e.calls++
	e.source = r
	e.before = before
	if e.onRead != nil {
		e.onRead()
	}
	return execution.ConversationPage{Session: r.Session, Before: before, Messages: []execution.ConversationMessage{{ID: "100", Content: "original user requirement"}}}, nil
}

func reviewContextFixture(t *testing.T, enabled bool, e execution.Executor) (*Service, Repository, Pull) {
	t.Helper()
	repo := Repository{Repo: "acme/project", ReviewerLogin: "reviewer", ReviewerRoute: &execution.Route{Provider: ccconnect.Name, Target: "reviewer"}, Review: &ReviewFeatures{ReuseDevelopmentThread: true, Context: ReviewContextFeatures{ThreadHistory: enabled}}}
	s, err := NewService(Config{Repositories: []Repository{repo}}, filepath.Join(t.TempDir(), stateDatabase), map[string]execution.Executor{ccconnect.Name: e})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	p := Pull{Number: 42, State: "open", URL: "https://github.com/acme/project/pull/42"}
	p.Head.SHA = strings.Repeat("a", 40)
	w := &Work{Repo: repo.Repo, Pull: p, Linked: true, Developer: Assignee{Route: &execution.Route{Provider: ccconnect.Name, Target: "developer", Session: "discord:123"}}, Reviewer: Assignee{GitHubLogin: "reviewer", Route: repo.ReviewerRoute}}
	st, err := s.store.load()
	if err != nil {
		t.Fatal(err)
	}
	st.Work[key(repo.Repo, p.Number)] = w
	if err := s.store.save(st); err != nil {
		t.Fatal(err)
	}
	if err := s.observe(repo, p, nil); err != nil {
		t.Fatal(err)
	}
	return s, repo, p
}

func contextRequest(t *testing.T, s *Service, query string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/work/acme/project/42/context?"+query, nil))
	return w
}

func TestReviewContextGateSHAAndBoundSource(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		e := &historyExecutor{}
		s, repo, p := reviewContextFixture(t, enabled, e)
		res := contextRequest(t, s, "sha="+p.Head.SHA+"&session=discord:999")
		if !enabled {
			if res.Code != 403 || e.calls != 0 {
				t.Fatalf("disabled history was read: %d", res.Code)
			}
			continue
		}
		if res.Code != 200 || e.source.Session != "discord:123" || !strings.Contains(res.Body.String(), "original user requirement") {
			t.Fatalf("incorrect source/context: %d %s", res.Code, res.Body)
		}
		w := notificationWork(t, s, repo, p)
		want := reviewHistoryBefore(w.Task.CreatedAt)
		if e.before != want {
			t.Fatalf("cutoff %s != %s", e.before, want)
		}
		d := deliveryFor(w, w.Task, "attempt", 1)
		if d.ContextBefore != want || !d.ReuseDevelopmentThread {
			t.Fatal("feature/boundary not handed to reviewer")
		}
		beforeCalls := e.calls
		res = contextRequest(t, s, "sha="+strings.Repeat("b", 40))
		if res.Code != 409 || e.calls != beforeCalls {
			t.Fatal("old SHA could read history")
		}
		boundary, _ := strconv.ParseUint(want, 10, 64)
		for _, cursor := range []string{"bad", "0", strconv.FormatUint(boundary+1, 10)} {
			res = contextRequest(t, s, "sha="+p.Head.SHA+"&before="+cursor)
			if res.Code != 400 || e.calls != beforeCalls {
				t.Fatal("invalid future cursor reached Discord")
			}
		}
		res = contextRequest(t, s, "sha="+p.Head.SHA+"&before=101")
		if res.Code != 200 || e.before != "101" {
			t.Fatal("older-page pagination failed")
		}
	}
}

func TestReviewContextRejectsRetiredTaskAndConcurrentReassignment(t *testing.T) {
	e := &historyExecutor{}
	s, repo, p := reviewContextFixture(t, true, e)
	e.onRead = func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		st, err := s.store.load()
		if err != nil {
			t.Fatal(err)
		}
		w := st.Work[key(repo.Repo, p.Number)]
		w.Reviewer = Assignee{GitHubLogin: "human"}
		w.replaceTask(nil)
		w.derive()
		if err := s.store.save(st); err != nil {
			t.Fatal(err)
		}
	}
	res := contextRequest(t, s, "sha="+p.Head.SHA)
	if res.Code != 409 || strings.Contains(res.Body.String(), "original user requirement") {
		t.Fatal("returned history after task changed")
	}
	e.onRead = nil
	p.State = "closed"
	if err := s.observe(repo, p, nil); err != nil {
		t.Fatal(err)
	}
	calls := e.calls
	res = contextRequest(t, s, "sha="+p.Head.SHA)
	if res.Code != 409 || e.calls != calls {
		t.Fatal("closed PR accessed history")
	}
}

func TestReviewFeaturesRefreshForNewSHAAndPersistBoundary(t *testing.T) {
	e := &historyExecutor{}
	s, repo, p := reviewContextFixture(t, false, e)
	w := notificationWork(t, s, repo, p)
	cutoff := reviewHistoryBefore(w.Task.CreatedAt)
	if cutoff == "" || cutoff != reviewHistoryBefore(w.Task.CreatedAt.In(time.FixedZone("other", -5*3600))) {
		t.Fatal("unstable cutoff")
	}
	st, _ := s.store.load()
	w = st.Work[key(repo.Repo, p.Number)]
	w.Task.Runs = []Attempt{{ID: "old", State: "succeeded"}}
	if err := s.store.save(st); err != nil {
		t.Fatal(err)
	}
	repo.Review = &ReviewFeatures{Context: ReviewContextFeatures{ThreadHistory: true}}
	p.Head.SHA = strings.Repeat("b", 40)
	if err := s.observe(repo, p, nil); err != nil {
		t.Fatal(err)
	}
	w = notificationWork(t, s, repo, p)
	if !w.Review.Context.ThreadHistory || deliveryFor(w, w.Task, "new", 1).ContextBefore == "" {
		t.Fatal("new task retained stale feature settings")
	}
	st, _ = s.store.load()
	if err := s.store.save(st); err != nil {
		t.Fatal(err)
	}
	w = notificationWork(t, s, repo, p)
	if deliveryFor(w, w.Task, "new", 1).ContextBefore != reviewHistoryBefore(w.Task.CreatedAt) {
		t.Fatal("boundary changed after storage round trip")
	}
}

func TestDispatchReviewReusesThreadAndProvidesContextCommand(t *testing.T) {
	var destination map[string]string
	client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		res := httptest.NewRecorder()
		switch r.URL.Path {
		case "/api/v10/channels/123":
			reply(res, 200, map[string]any{"id": "123", "type": 11, "parent_id": "77"})
		case "/api/v10/channels/77":
			reply(res, 200, map[string]any{"id": "77", "type": 0})
		case "/hook":
			if err := json.NewDecoder(r.Body).Decode(&destination); err != nil {
				t.Fatal(err)
			}
			reply(res, 200, map[string]string{"status": "accepted"})
		default:
			t.Fatalf("unexpected allocation/read %s %s", r.Method, r.URL.Path)
		}
		return res.Result(), nil
	})}
	e, err := ccconnect.New(client, "http://127.0.0.1:9111/hook", "test", "discord-test")
	if err != nil {
		t.Fatal(err)
	}
	s, repo, p := reviewContextFixture(t, true, e)
	if err := s.dispatch(context.Background(), repo, p.Number); err != nil {
		t.Fatal(err)
	}
	w := notificationWork(t, s, repo, p)
	if destination["project"] != "reviewer" || destination["session_key"] != "discord:123" || !strings.Contains(destination["prompt"], "ink context --sha "+p.Head.SHA) || w.Reviewer.Route.Session != "discord:123" || w.Task.Delivery != "accepted" {
		t.Fatalf("dispatch: %+v, %+v", destination, w.Task)
	}
}

func TestReviewContextAttemptCannotFollowNewDeveloperSource(t *testing.T) {
	e := &historyExecutor{}
	s, repo, p := reviewContextFixture(t, true, e)
	st, err := s.store.load()
	if err != nil {
		t.Fatal(err)
	}
	w := st.Work[key(repo.Repo, p.Number)]
	request := deliveryFor(w, w.Task, "attempt", 1)
	w.Task.Runs = []Attempt{{ID: "attempt", State: "accepted", Request: request}}
	w.Developer.Route = &execution.Route{Provider: ccconnect.Name, Target: "developer", Session: "discord:456"}
	if err := s.store.save(st); err != nil {
		t.Fatal(err)
	}
	res := contextRequest(t, s, "sha="+p.Head.SHA)
	if res.Code != 409 || e.calls != 0 {
		t.Fatal("old attempt followed newly assigned developer conversation")
	}
}

func TestReviewContextDisabledDuringReadReturnsNoMessages(t *testing.T) {
	e := &historyExecutor{}
	s, _, p := reviewContextFixture(t, true, e)
	e.onRead = func() { s.mu.Lock(); s.config.Repositories[0].Review = &ReviewFeatures{}; s.mu.Unlock() }
	res := contextRequest(t, s, "sha="+p.Head.SHA)
	if res.Code != 409 || strings.Contains(res.Body.String(), "original user requirement") {
		t.Fatal("context returned after gate disabled")
	}
}

func TestRegistrationReviewDefaultCanBeExplicitlyDisabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), stateDatabase)
	cfg := Config{RepositoryDefaults: Repository{ReviewerLogin: "reviewer", Review: &ReviewFeatures{ReuseDevelopmentThread: true, Context: ReviewContextFeatures{ThreadHistory: true}}}}
	s, err := NewService(cfg, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.up.http.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		reply(w, 200, []Pull{})
		return w.Result(), nil
	})
	for _, body := range []string{`{"repo":"acme/inherit"}`, `{"repo":"acme/disabled","review":{}}`} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/v1/repositories", strings.NewReader(body)))
		if w.Code != 201 {
			t.Fatalf("registration: %d %s", w.Code, w.Body)
		}
	}
	s.Close()
	s2, err := NewService(cfg, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	for _, r := range s2.repositories() {
		if r.Review == nil {
			t.Fatal("review settings were not persisted")
		}
		enabled := r.Repo == "acme/inherit"
		if r.Review.ReuseDevelopmentThread != enabled || r.Review.Context.ThreadHistory != enabled {
			t.Fatalf("default override lost: %+v", r)
		}
	}
}

func TestAutomaticReviewWithHistoryMissingSourceDoesNotStartAttempt(t *testing.T) {
	e := &historyExecutor{}
	s, repo, p := reviewContextFixture(t, true, e)
	st, err := s.store.load()
	if err != nil {
		t.Fatal(err)
	}
	w := st.Work[key(repo.Repo, p.Number)]
	w.Developer = Assignee{GitHubLogin: "human"}
	w.Origin = nil
	if err := s.store.save(st); err != nil {
		t.Fatal(err)
	}
	if err := s.dispatch(context.Background(), repo, p.Number); err != nil {
		t.Fatal(err)
	}
	w = notificationWork(t, s, repo, p)
	if len(w.Task.Runs) != 0 || w.Task.Attempts != 0 || w.Task.Delivery != "pending" || w.Task.Error == "" || w.Workflow.State != "needs_attention" || e.prepared != 0 || len(e.deliveries) != 0 {
		t.Fatalf("missing history source started an execution: %+v", w.Task)
	}
	// Once the operator supplies the proper source, the first real attempt can
	// proceed without a fake unknown run or manual stop confirmation.
	st, err = s.store.load()
	if err != nil {
		t.Fatal(err)
	}
	st.Work[key(repo.Repo, p.Number)].Developer.Route = &execution.Route{Provider: ccconnect.Name, Target: "developer", Session: "discord:123"}
	if err := s.store.save(st); err != nil {
		t.Fatal(err)
	}
	if err := s.dispatch(context.Background(), repo, p.Number); err != nil {
		t.Fatal(err)
	}
	w = notificationWork(t, s, repo, p)
	if w.Task.Attempts != 1 || w.Task.Delivery != "accepted" || w.Task.Error != "" || len(e.deliveries) != 1 {
		t.Fatal("correcting source did not resume the first attempt")
	}
	for _, a := range w.Alerts {
		if a.Code == "review_context_source_invalid" && a.Active {
			t.Fatal("resolved source alert remains active")
		}
	}
}
