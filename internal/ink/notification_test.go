package ink

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ink-commons/internal/execution"
)

type notificationExecutor struct {
	testExecutor
	notices    []execution.Notification
	routes     []execution.Route
	onNotify   func()
	err        error
	requireACK bool
}

func (e *notificationExecutor) NotificationACKRequired() bool { return e.requireACK }

func (e *notificationExecutor) Notify(_ context.Context, r execution.Route, n execution.Notification) error {
	if e.onNotify != nil {
		e.onNotify()
	}
	e.notices = append(e.notices, n)
	e.routes = append(e.routes, r)
	return e.err
}

func notificationFixture(t *testing.T, executor execution.Executor) (*Service, Repository, Pull, Review, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), stateDatabase)
	repo := Repository{Repo: "acme/project", ReviewerLogin: "reviewer"}
	s, err := NewService(Config{Repositories: []Repository{repo}}, path, map[string]execution.Executor{"test": executor})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	p := Pull{Number: 42, URL: "https://github.com/acme/project/pull/42", State: "open"}
	p.Head.SHA = strings.Repeat("a", 40)
	r := Review{ID: 9, SHA: p.Head.SHA, State: "APPROVED", URL: p.URL + "#pullrequestreview-9", SubmittedAt: time.Now()}
	r.User.Login = "Reviewer"
	st, err := s.store.load()
	if err != nil {
		t.Fatal(err)
	}
	st.Work[key(repo.Repo, p.Number)] = &Work{Repo: repo.Repo, Pull: p, Linked: true,
		Developer: Assignee{Route: &execution.Route{Provider: "test", Target: "developer", Session: "dev-session"}},
		Reviewer:  Assignee{GitHubLogin: repo.ReviewerLogin}}
	if err := s.store.save(st); err != nil {
		t.Fatal(err)
	}
	return s, repo, p, r, path
}

func notificationWork(t *testing.T, s *Service, repo Repository, p Pull) *Work {
	t.Helper()
	st, err := s.store.load()
	if err != nil {
		t.Fatal(err)
	}
	return st.Work[key(repo.Repo, p.Number)]
}

// Exercises the real GitHub poll path, not just a direct notification helper.
func TestPollNotifiesDeveloperOfApprovalOnceAcrossRestart(t *testing.T) {
	e := &notificationExecutor{}
	s, repo, p, r, path := notificationFixture(t, e)
	configureUpstream := func(service *Service) {
		service.up.http.Transport = testTransport(func(req *http.Request) (*http.Response, error) {
			w := httptest.NewRecorder()
			if strings.HasSuffix(req.URL.Path, "/reviews") {
				reply(w, 200, []Review{r})
			} else {
				reply(w, 200, []Pull{p})
			}
			return w.Result(), nil
		})
	}
	configureUpstream(s)
	// During transport, the event must already be durably reserved.
	e.onNotify = func() {
		w := notificationWork(t, s, repo, p)
		if len(w.Notifications) != 1 || w.Notifications[0].State != "unknown" {
			t.Fatalf("not reserved: %+v", w.Notifications)
		}
	}
	s.Poll(context.Background())
	e.onNotify = nil
	s.Poll(context.Background())
	w := notificationWork(t, s, repo, p)
	if w.Phase != "approved" || w.Task != nil || len(w.Notifications) != 1 || w.Notifications[0].State != "accepted" {
		t.Fatalf("approval should remain complete without a fix task: %+v", w)
	}
	if len(e.notices) != 1 || e.routes[0].Session != "dev-session" || e.routes[0].Target != "developer" {
		t.Fatalf("wrong destination/count: %+v", e)
	}
	n := e.notices[0]
	if n.SHA != p.Head.SHA || n.Reviewer != r.User.Login || n.ReviewID != r.ID || n.ReviewURL != r.URL || n.URL != p.URL || n.Event != "review_approved" || n.EventID == "" {
		t.Fatalf("missing approval context: %+v", n)
	}
	s.Close()
	s2, err := NewService(Config{Repositories: []Repository{repo}}, path, map[string]execution.Executor{"test": e})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	configureUpstream(s2)
	s2.Poll(context.Background())
	if len(e.notices) != 1 {
		t.Fatal("restart replayed approval")
	}
	// A second approval of the same SHA is still one event.
	r.ID++
	r.SubmittedAt = r.SubmittedAt.Add(time.Second)
	s2.Poll(context.Background())
	if len(e.notices) != 1 {
		t.Fatal("repeated approval replayed notification")
	}
	p.Head.SHA = strings.Repeat("b", 40)
	s2.Poll(context.Background())
	if len(e.notices) != 1 {
		t.Fatal("old approval notified for new head")
	}
	r.SHA = p.Head.SHA
	s2.Poll(context.Background())
	if len(e.notices) != 2 || e.notices[0].EventID == e.notices[1].EventID {
		t.Fatal("new approved SHA did not get its own event")
	}
}

func TestApprovalNotificationRequiresCurrentAssignedReview(t *testing.T) {
	for _, scenario := range []string{"wrong author", "old sha", "changes requested", "draft", "closed", "unlinked", "newer request changes"} {
		t.Run(scenario, func(t *testing.T) {
			e := &notificationExecutor{}
			s, repo, p, r, _ := notificationFixture(t, e)
			reviews := []Review{r}
			switch scenario {
			case "wrong author":
				reviews[0].User.Login = "someone-else"
			case "old sha":
				reviews[0].SHA = strings.Repeat("b", 40)
			case "changes requested":
				reviews[0].State = "CHANGES_REQUESTED"
			case "draft":
				p.Draft = true
			case "closed":
				p.State = "closed"
			case "unlinked":
				st, _ := s.store.load()
				st.Work[key(repo.Repo, p.Number)].Linked = false
				if err := s.store.save(st); err != nil {
					t.Fatal(err)
				}
			case "newer request changes":
				r.ID++
				r.State = "CHANGES_REQUESTED"
				r.SubmittedAt = r.SubmittedAt.Add(time.Second)
				reviews = append(reviews, r)
			}
			if err := s.observe(repo, p, reviews); err != nil {
				t.Fatal(err)
			}
			if err := s.notifyApproved(context.Background(), repo, p.Number); err != nil {
				t.Fatal(err)
			}
			if len(e.notices) != 0 || len(notificationWork(t, s, repo, p).Notifications) != 0 {
				t.Fatal("non-qualifying review notified")
			}
		})
	}
}

func TestApprovalNotificationUnknownIsNotReplayed(t *testing.T) {
	e := &notificationExecutor{err: errors.New("response lost")}
	s, repo, p, r, path := notificationFixture(t, e)
	if err := s.observe(repo, p, []Review{r}); err != nil {
		t.Fatal(err)
	}
	if err := s.notifyApproved(context.Background(), repo, p.Number); err != nil {
		t.Fatal(err)
	}
	w := notificationWork(t, s, repo, p)
	if w.Notifications[0].State != "unknown" || w.Workflow.State != "needs_attention" || w.Notifications[0].Error == "" {
		t.Fatalf("lost outcome not visible: %+v", w)
	}
	s.Close()
	s2, err := NewService(Config{}, path, map[string]execution.Executor{"test": e})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if err := s2.notifyApproved(context.Background(), repo, p.Number); err != nil {
		t.Fatal(err)
	}
	if len(e.notices) != 1 {
		t.Fatal("unknown transport outcome was replayed")
	}
}

func TestApprovalNotificationUnsupportedAndHuman(t *testing.T) {
	for _, human := range []bool{false, true} {
		s, repo, p, r, _ := notificationFixture(t, &testExecutor{})
		if human {
			st, _ := s.store.load()
			st.Work[key(repo.Repo, p.Number)].Developer.Route = nil
			st.Work[key(repo.Repo, p.Number)].Origin = &execution.Route{Provider: "test", Target: "old-bot", Session: "old-session"}
			if err := s.store.save(st); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.observe(repo, p, []Review{r}); err != nil {
			t.Fatal(err)
		}
		if err := s.notifyApproved(context.Background(), repo, p.Number); err != nil {
			t.Fatal(err)
		}
		w := notificationWork(t, s, repo, p)
		want := "unsupported"
		if human {
			want = "manual"
		}
		if w.Phase != "approved" || w.Task != nil || len(w.Notifications) != 1 || w.Notifications[0].State != want {
			t.Fatalf("unexpected fallback: %+v", w)
		}
	}
}

func TestNotificationPreservesConcurrentWorkChangesAndExecutionHistory(t *testing.T) {
	e := &notificationExecutor{}
	s, repo, p, r, _ := notificationFixture(t, e)
	if err := s.observe(repo, p, []Review{r}); err != nil {
		t.Fatal(err)
	}
	st, err := s.store.load()
	if err != nil {
		t.Fatal(err)
	}
	w := st.Work[key(repo.Repo, p.Number)]
	w.History = []*Task{{ID: "old-review", Role: "reviewer", SHA: p.Head.SHA, Runs: []Attempt{{ID: "old-attempt", State: "unknown"}}}}
	if err := s.store.save(st); err != nil {
		t.Fatal(err)
	}
	e.onNotify = func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		st, err := s.store.load()
		if err != nil {
			t.Fatal(err)
		}
		live := st.Work[key(repo.Repo, p.Number)]
		live.Developer = Assignee{GitHubLogin: "human-takeover"}
		live.Pull.Head.SHA = strings.Repeat("c", 40)
		live.derive()
		if err := s.store.save(st); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.notifyApproved(context.Background(), repo, p.Number); err != nil {
		t.Fatal(err)
	}
	w = notificationWork(t, s, repo, p)
	if len(e.notices) != 1 || w.Developer.GitHubLogin != "human-takeover" || w.Pull.Head.SHA != strings.Repeat("c", 40) || w.Phase != "review_pending" || len(w.History) != 1 || w.History[0].Runs[0].State != "unknown" || w.Notifications[0].Route.Session != "dev-session" {
		t.Fatalf("notification overwrote work or execution state: %+v", w)
	}
}

func TestConcurrentApprovalNotificationsReserveOnce(t *testing.T) {
	e := &notificationExecutor{}
	s, repo, p, r, _ := notificationFixture(t, e)
	if err := s.observe(repo, p, []Review{r}); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	e.onNotify = func() { close(entered); <-release }
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := s.notifyApproved(context.Background(), repo, p.Number); err != nil {
			t.Error(err)
		}
	}()
	<-entered
	if err := s.notifyApproved(context.Background(), repo, p.Number); err != nil {
		t.Fatal(err)
	}
	close(release)
	wg.Wait()
	if len(e.notices) != 1 {
		t.Fatal("concurrent duplicate notification")
	}
}
