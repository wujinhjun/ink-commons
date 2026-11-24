//go:build linux || darwin

package ink

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wujinhjun/ink-commons/internal/execution"
	"github.com/wujinhjun/ink-commons/internal/execution/ccconnect"
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
	setRepositoryConfigForTest(t, s, repo)
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
	s, repo, p := reviewContextFixture(t, true, e)
	e.onRead = func() {
		s.mu.Lock()
		repo.Review = &ReviewFeatures{}
		setRepositoryConfigForTest(t, s, repo)
		s.mu.Unlock()
	}
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
		if a.Code == "review_preparation_invalid" && a.Active {
			t.Fatal("resolved source alert remains active")
		}
	}
}

func TestReviewPreparationFailuresDoNotReserveAttempts(t *testing.T) {
	for _, scenario := range []string{"reuse only missing source", "reuse only same project", "reuse only invalid source", "history with existing session missing source", "history with explicit parent missing source"} {
		t.Run(scenario, func(t *testing.T) {
			networkCalls := 0
			client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
				networkCalls++
				w := httptest.NewRecorder()
				switch r.URL.Path {
				case "/api/v10/channels/123":
					reply(w, 200, map[string]any{"id": "123", "type": 11, "parent_id": "77"})
				case "/api/v10/channels/77":
					reply(w, 200, map[string]any{"id": "77", "type": 0})
				case "/hook":
					reply(w, 200, map[string]string{"status": "accepted"})
				default:
					t.Fatalf("unexpected preparation I/O: %s", r.URL.Path)
				}
				return w.Result(), nil
			})}
			e, err := ccconnect.New(client, "http://127.0.0.1:9111/hook", "hook", "discord")
			if err != nil {
				t.Fatal(err)
			}
			history := strings.HasPrefix(scenario, "history")
			s, repo, p := reviewContextFixture(t, history, e)
			st, _ := s.store.load()
			w := st.Work[key(repo.Repo, p.Number)]
			switch scenario {
			case "reuse only same project":
				w.Developer.Route.Target = "reviewer"
			case "reuse only invalid source":
				w.Developer.Route.Session = "invalid"
			default:
				w.Developer = Assignee{GitHubLogin: "human"}
				w.Origin = nil
			}
			if strings.Contains(scenario, "existing session") {
				w.Reviewer.Route.Session = "discord:999"
			}
			if strings.Contains(scenario, "explicit parent") {
				w.Reviewer.Route.Parent = "88"
			}
			if err := s.store.save(st); err != nil {
				t.Fatal(err)
			}
			if err := s.dispatch(context.Background(), repo, p.Number); err != nil {
				t.Fatal(err)
			}
			w = notificationWork(t, s, repo, p)
			if networkCalls != 0 || len(w.Task.Runs) != 0 || w.Task.Attempts != 0 || w.Task.Delivery != "pending" || w.Workflow.State != "needs_attention" {
				t.Fatalf("local preparation failure started an attempt: %+v", w.Task)
			}
			st, _ = s.store.load()
			w = st.Work[key(repo.Repo, p.Number)]
			w.Developer.Route = &execution.Route{Provider: ccconnect.Name, Target: "developer", Session: "discord:123"}
			w.Reviewer.Route = &execution.Route{Provider: ccconnect.Name, Target: "reviewer"}
			if err := s.store.save(st); err != nil {
				t.Fatal(err)
			}
			if err := s.dispatch(context.Background(), repo, p.Number); err != nil {
				t.Fatal(err)
			}
			w = notificationWork(t, s, repo, p)
			if networkCalls != 3 || w.Task.Attempts != 1 || w.Task.Delivery != "accepted" {
				t.Fatal("corrected preparation did not start its first attempt")
			}
		})
	}
}

func TestReuseOnlyExplicitPlacementAllowsMissingDevelopmentSource(t *testing.T) {
	for _, placement := range []string{"session", "parent"} {
		t.Run(placement, func(t *testing.T) {
			calls := 0
			e, err := ccconnect.New(&http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				w := httptest.NewRecorder()
				switch r.URL.Path {
				case "/api/v10/channels/88/threads":
					reply(w, 200, map[string]string{"id": "999"})
				case "/hook":
					reply(w, 200, map[string]string{"status": "accepted"})
				default:
					t.Fatalf("unexpected source lookup for explicit placement: %s", r.URL)
				}
				return w.Result(), nil
			})}, "http://127.0.0.1:9111/hook", "hook", "discord")
			if err != nil {
				t.Fatal(err)
			}
			s, repo, p := reviewContextFixture(t, false, e)
			st, _ := s.store.load()
			w := st.Work[key(repo.Repo, p.Number)]
			w.Developer = Assignee{GitHubLogin: "human"}
			w.Origin = nil
			if placement == "session" {
				w.Reviewer.Route.Session = "discord:999"
			} else {
				w.Reviewer.Route.Parent = "88"
			}
			if err := s.store.save(st); err != nil {
				t.Fatal(err)
			}
			if err := s.dispatch(context.Background(), repo, p.Number); err != nil {
				t.Fatal(err)
			}
			w = notificationWork(t, s, repo, p)
			wantCalls := 1
			if placement == "parent" {
				wantCalls = 2
			}
			if calls != wantCalls || w.Task.Attempts != 1 || w.Task.Delivery != "accepted" {
				t.Fatalf("explicit placement was incorrectly blocked: %+v", w.Task)
			}
		})
	}
}

type historyWakeExecutor struct {
	historyExecutor
	wakes int
}

func (e *historyWakeExecutor) WakeTask(_ context.Context, _ execution.Route, _ execution.Delivery) error {
	e.wakes++
	return nil
}

func cooperativeContextFixture(t *testing.T, enabled bool, claim bool) (*Service, Repository, Pull, *historyWakeExecutor) {
	t.Helper()
	e := &historyWakeExecutor{}
	s, repo, p := reviewContextFixture(t, enabled, e)
	configAPI(t, s, "POST", repo.Repo, "task_mode", `"cooperative"`, 200)
	if err := s.dispatch(context.Background(), repo, p.Number); err != nil {
		t.Fatal(err)
	}
	work := notificationWork(t, s, repo, p)
	if e.wakes != 1 || work.Task.ConfigSnapshot == nil || work.Task.Handoff == nil || len(work.Task.Runs) != 0 {
		t.Fatal("fixture did not freeze a cooperative wake")
	}
	if claim {
		input := map[string]any{"token": claimA, "route": work.Task.Handoff.Assignee.Route}
		body, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		out := httptest.NewRecorder()
		s.Handler().ServeHTTP(out, httptest.NewRequest("POST", "/v1/tasks/"+work.Task.ID+"/claim", strings.NewReader(string(body))))
		if out.Code != 200 {
			t.Fatal(out.Code, out.Body)
		}
	}
	return s, repo, p, e
}

func reassignContextDeveloper(t *testing.T, s *Service) {
	t.Helper()
	out := httptest.NewRecorder()
	s.Handler().ServeHTTP(out, httptest.NewRequest("POST", "/v1/work/acme/project/42/assign", strings.NewReader(`{"role":"developer","github_login":"replacement","route":{"provider":"cc-connect","target":"developer","session":"discord:999"}}`)))
	if out.Code != 200 {
		t.Fatal(out.Code, out.Body)
	}
}

func TestCooperativeContextCannotEnableHistoryAfterDispatch(t *testing.T) {
	for _, claim := range []bool{false, true} {
		for _, initial := range []string{"null", `{"context":{"thread_history":false}}`} {
			t.Run(fmt.Sprintf("claimed=%v/initial=%s", claim, initial), func(t *testing.T) {
				e := &historyWakeExecutor{}
				s, repo, p := reviewContextFixture(t, false, e)
				configAPI(t, s, "POST", repo.Repo, "review", initial, 200)
				configAPI(t, s, "POST", repo.Repo, "task_mode", `"cooperative"`, 200)
				if err := s.dispatch(context.Background(), repo, p.Number); err != nil {
					t.Fatal(err)
				}
				work := notificationWork(t, s, repo, p)
				if e.wakes != 1 || work.Task.ConfigSnapshot == nil {
					t.Fatal("wake was not frozen")
				}
				if claim {
					body, _ := json.Marshal(map[string]any{"token": claimA, "route": work.Task.Handoff.Assignee.Route})
					out := httptest.NewRecorder()
					s.Handler().ServeHTTP(out, httptest.NewRequest("POST", "/v1/tasks/"+work.Task.ID+"/claim", strings.NewReader(string(body))))
					if out.Code != 200 {
						t.Fatal(out.Code, out.Body)
					}
				}
				configAPI(t, s, "POST", repo.Repo, "review", `{"context":{"thread_history":true}}`, 200)
				res := contextRequest(t, s, "sha="+p.Head.SHA)
				if res.Code != 403 || e.calls != 0 {
					t.Fatalf("live enable broadened frozen task permission: %d %s calls=%d", res.Code, res.Body, e.calls)
				}
			})
		}
	}
}

func TestCooperativeContextKeepsSnapshotSourceAfterDeveloperReassignment(t *testing.T) {
	for _, claim := range []bool{false, true} {
		for _, duringRead := range []bool{false, true} {
			t.Run(fmt.Sprintf("claimed=%v/duringRead=%v", claim, duringRead), func(t *testing.T) {
				s, repo, p, e := cooperativeContextFixture(t, true, claim)
				original := notificationWork(t, s, repo, p).Task
				if duringRead {
					e.onRead = func() { reassignContextDeveloper(t, s) }
				} else {
					reassignContextDeveloper(t, s)
				}
				res := contextRequest(t, s, "sha="+p.Head.SHA)
				if res.Code != 200 || e.calls != 1 || e.source.Session != "discord:123" {
					t.Fatalf("frozen context changed source: %d %s source=%+v", res.Code, res.Body, e.source)
				}
				work := notificationWork(t, s, repo, p)
				if work.Task.ID != original.ID || work.Developer.Route.Session != "discord:999" || deliveryFor(work, work.Task, "", 0).Source.Session != "discord:123" {
					t.Fatal("fixture did not preserve the reviewer task and frozen delivery")
				}
			})
		}
	}
}

func TestCooperativeContextRevalidatesFrozenBindingAfterRead(t *testing.T) {
	for _, change := range []string{"snapshot-policy", "snapshot-source", "live-disable"} {
		t.Run(change, func(t *testing.T) {
			s, repo, p, e := cooperativeContextFixture(t, true, true)
			e.onRead = func() {
				if change == "live-disable" {
					configAPI(t, s, "POST", repo.Repo, "review", `null`, 200)
					return
				}
				s.mu.Lock()
				defer s.mu.Unlock()
				st, err := s.store.load()
				if err != nil {
					t.Fatal(err)
				}
				snapshot := st.Work[key(repo.Repo, p.Number)].Task.ConfigSnapshot
				if change == "snapshot-policy" {
					snapshot.Repository.Review.Context.ThreadHistory = false
				} else {
					snapshot.Source.Session = "discord:999"
				}
				if err := s.store.save(st); err != nil {
					t.Fatal(err)
				}
			}
			res := contextRequest(t, s, "sha="+p.Head.SHA)
			if res.Code != 409 || strings.Contains(res.Body.String(), "original user requirement") {
				t.Fatalf("returned context after binding changed: %d %s", res.Code, res.Body)
			}
		})
	}
}
