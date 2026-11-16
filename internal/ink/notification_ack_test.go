package ink

import (
	"context"
	"encoding/json"
	"errors"
	"ink-commons/internal/execution/ccconnect"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ink-commons/internal/execution"
)

func receiptRequest(s *Service, event string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"event_id": event})
	res := httptest.NewRecorder()
	s.Handler().ServeHTTP(res, httptest.NewRequest("POST", "/v1/notifications/ack", strings.NewReader(string(body))))
	return res
}

func receiptFirst(t *testing.T, s *Service, event string, want bool) {
	t.Helper()
	res := receiptRequest(s, event)
	var receipt struct {
		First bool `json:"first"`
	}
	if res.Code != 200 || json.Unmarshal(res.Body.Bytes(), &receipt) != nil || receipt.First != want {
		t.Fatalf("receipt: %d %s, want first=%v", res.Code, res.Body, want)
	}
}

func expireNotification(t *testing.T, s *Service, repo Repository, p Pull) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.store.load()
	if err != nil {
		t.Fatal(err)
	}
	st.Work[key(repo.Repo, p.Number)].Notifications[0].RetryAt = time.Now().Add(-time.Second)
	if err := s.store.save(st); err != nil {
		t.Fatal(err)
	}
}

// Regression: cc-connect accepts HTTP before discovering that the session is
// busy. Transport success must not suppress durable retry after a restart.
func TestApprovalNotificationAcceptedButDroppedRetriesUntilAgentReceipt(t *testing.T) {
	e := &notificationExecutor{requireACK: true}
	s, repo, p, r, path := notificationFixture(t, e)
	if err := s.observe(repo, p, []Review{r}); err != nil {
		t.Fatal(err)
	}
	if err := s.notifyApproved(context.Background(), repo, p.Number); err != nil {
		t.Fatal(err)
	}
	n := notificationWork(t, s, repo, p).Notifications[0]
	if n.State != "awaiting_ack" || n.Attempts != 1 || n.RetryAt.Before(time.Now()) || !n.FinishedAt.IsZero() {
		t.Fatalf("HTTP accepted confused with agent receipt: %+v", n)
	}
	if err := s.notifyApproved(context.Background(), repo, p.Number); err != nil {
		t.Fatal(err)
	}
	if len(e.notices) != 1 {
		t.Fatal("ignored retry backoff")
	}
	s.Close()
	s2, err := NewService(Config{}, path, map[string]execution.Executor{"test": e})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	expireNotification(t, s2, repo, p)
	if err := s2.notifyApproved(context.Background(), repo, p.Number); err != nil {
		t.Fatal(err)
	}
	n2 := notificationWork(t, s2, repo, p).Notifications[0]
	if len(e.notices) != 2 || e.notices[0].EventID != e.notices[1].EventID || n2.Attempts != 2 || time.Until(n2.RetryAt) < 3*time.Minute {
		t.Fatalf("restart did not retry same event with backoff: %+v", n2)
	}
	receiptFirst(t, s2, n.EventID, true)
	receiptFirst(t, s2, n.EventID, false)
	expireNotification(t, s2, repo, p)
	if err := s2.notifyApproved(context.Background(), repo, p.Number); err != nil {
		t.Fatal(err)
	}
	if len(e.notices) != 2 || notificationWork(t, s2, repo, p).Notifications[0].State != "acknowledged" {
		t.Fatal("acknowledged notification replayed")
	}
	s2.Close()
	s3, err := NewService(Config{}, path, map[string]execution.Executor{"test": e})
	if err != nil {
		t.Fatal(err)
	}
	defer s3.Close()
	receiptFirst(t, s3, n.EventID, false)
	if err := s3.notifyApproved(context.Background(), repo, p.Number); err != nil {
		t.Fatal(err)
	}
	if len(e.notices) != 2 {
		t.Fatal("restart lost durable receipt")
	}
}

func TestApprovalNotificationReceiptWinsOverLateTransportFailure(t *testing.T) {
	e := &notificationExecutor{requireACK: true, err: errors.New("response lost")}
	s, repo, p, r, _ := notificationFixture(t, e)
	if err := s.observe(repo, p, []Review{r}); err != nil {
		t.Fatal(err)
	}
	e.onNotify = func() {
		n := notificationWork(t, s, repo, p).Notifications[0]
		receiptFirst(t, s, n.EventID, true)
	}
	if err := s.notifyApproved(context.Background(), repo, p.Number); err != nil {
		t.Fatal(err)
	}
	n := notificationWork(t, s, repo, p).Notifications[0]
	if n.State != "acknowledged" || n.Error != "" || !n.RetryAt.IsZero() {
		t.Fatalf("late completion overwrote receipt: %+v", n)
	}
}

func TestApprovalNotificationLostResponseRetriesAndConcurrentReceiptDeduplicates(t *testing.T) {
	e := &notificationExecutor{requireACK: true, err: errors.New("connection reset")}
	s, repo, p, r, _ := notificationFixture(t, e)
	if err := s.observe(repo, p, []Review{r}); err != nil {
		t.Fatal(err)
	}
	if err := s.notifyApproved(context.Background(), repo, p.Number); err != nil {
		t.Fatal(err)
	}
	if notificationWork(t, s, repo, p).Notifications[0].State != "unknown" {
		t.Fatal("missing unknown state")
	}
	expireNotification(t, s, repo, p)
	e.err = nil
	if err := s.notifyApproved(context.Background(), repo, p.Number); err != nil {
		t.Fatal(err)
	}
	event := e.notices[0].EventID
	var wg sync.WaitGroup
	results := make(chan bool, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := receiptRequest(s, event)
			var receipt struct {
				First bool `json:"first"`
			}
			if res.Code != 200 || json.Unmarshal(res.Body.Bytes(), &receipt) != nil {
				t.Errorf("receipt failed: %s", res.Body)
				return
			}
			results <- receipt.First
		}()
	}
	wg.Wait()
	close(results)
	first := 0
	for result := range results {
		if result {
			first++
		}
	}
	if first != 1 || len(e.notices) != 2 {
		t.Fatalf("claims=%d attempts=%d", first, len(e.notices))
	}
}

func TestApprovalNotificationStaleReceiptDoesNotAnnounceApproval(t *testing.T) {
	for _, scenario := range []string{"head changed", "closed", "approval revoked", "route changed"} {
		t.Run(scenario, func(t *testing.T) {
			e := &notificationExecutor{requireACK: true}
			s, repo, p, r, _ := notificationFixture(t, e)
			if err := s.observe(repo, p, []Review{r}); err != nil {
				t.Fatal(err)
			}
			if err := s.notifyApproved(context.Background(), repo, p.Number); err != nil {
				t.Fatal(err)
			}
			event := e.notices[0].EventID
			st, err := s.store.load()
			if err != nil {
				t.Fatal(err)
			}
			w := st.Work[key(repo.Repo, p.Number)]
			switch scenario {
			case "head changed":
				w.Pull.Head.SHA = strings.Repeat("b", 40)
			case "closed":
				w.Pull.State = "closed"
			case "approval revoked":
				w.Reviews[0].State = "CHANGES_REQUESTED"
			case "route changed":
				w.Developer.Route.Session = "new-session"
			}
			w.derive()
			if err := s.store.save(st); err != nil {
				t.Fatal(err)
			}
			receiptFirst(t, s, event, false)
			n := notificationWork(t, s, repo, p).Notifications[0]
			if n.State != "superseded" {
				t.Fatalf("stale receipt: %+v", n)
			}
			expireNotification(t, s, repo, p)
			if err := s.notifyApproved(context.Background(), repo, p.Number); err != nil {
				t.Fatal(err)
			}
			if len(e.notices) != 1 {
				t.Fatal("superseded notification retried")
			}
		})
	}
}

func TestApprovalNotificationReceiptRejectsInvalidEvents(t *testing.T) {
	s, _, _, _, _ := notificationFixture(t, &notificationExecutor{})
	for _, tc := range []struct {
		event  string
		status int
	}{{"", 400}, {"missing", 404}} {
		if res := receiptRequest(s, tc.event); res.Code != tc.status {
			t.Fatalf("%q: %d %s", tc.event, res.Code, res.Body)
		}
	}
}

// Run the real polling and cc-connect adapter together. The unchanged runtime
// accepts and discards the first request, then the recipient claims the retry.
func TestPollCCConnectApprovalBusyDropRecoveredWithoutRuntimeChanges(t *testing.T) {
	var service *Service
	var event string
	var calls atomic.Int32
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Prompt string `json:"prompt"`
		}
		if r.Header.Get("Authorization") != "Bearer hook-test" || json.NewDecoder(r.Body).Decode(&request) != nil {
			t.Error("invalid hook request")
		}
		if !strings.Contains(request.Prompt, "ink notification ack --event") {
			t.Error("missing receipt command")
		}
		if calls.Add(1) == 2 {
			result := receiptRequest(service, event)
			if result.Code != 200 || !strings.Contains(result.Body.String(), `"first":true`) {
				t.Errorf("agent could not claim retry: %s", result.Body)
			}
		}
		reply(w, 200, map[string]string{"status": "accepted"})
	}))
	defer hook.Close()
	executor, err := ccconnect.New(hook.Client(), hook.URL, "hook-test", "")
	if err != nil {
		t.Fatal(err)
	}
	s, repo, p, r, _ := notificationFixture(t, executor)
	service = s
	st, err := s.store.load()
	if err != nil {
		t.Fatal(err)
	}
	st.Work[key(repo.Repo, p.Number)].Developer.Route.Session = "discord:123"
	if err := s.store.save(st); err != nil {
		t.Fatal(err)
	}
	s.up.http.Transport = testTransport(func(req *http.Request) (*http.Response, error) {
		res := httptest.NewRecorder()
		if strings.HasSuffix(req.URL.Path, "/reviews") {
			reply(res, 200, []Review{r})
		} else {
			reply(res, 200, []Pull{p})
		}
		return res.Result(), nil
	})
	s.Poll(context.Background())
	n := notificationWork(t, s, repo, p).Notifications[0]
	event = n.EventID
	if !n.RequireACK || n.State != "awaiting_ack" {
		t.Fatalf("dropped runtime request marked received: %+v", n)
	}
	expireNotification(t, s, repo, p)
	s.Poll(context.Background())
	s.Poll(context.Background())
	n = notificationWork(t, s, repo, p).Notifications[0]
	if n.State != "acknowledged" || n.Attempts != 2 || calls.Load() != 2 {
		t.Fatalf("busy drop not recovered: %+v calls=%d", n, calls.Load())
	}
}

func TestApprovalNotificationRetriesAndReceiptsSurviveAutoMerge(t *testing.T) {
	f := newAutoMergeFixture(t)
	e := &notificationExecutor{requireACK: true}
	configure := func() {
		f.s.providers = map[string]execution.Executor{"test": e}
		f.installTransport(t)
		original := f.s.up.http.Transport
		f.s.up.http.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
			// GitHub's open-PR listing no longer contains a merged PR. Recovery must
			// explicitly refresh an already closed work item on later poll cycles.
			if strings.HasSuffix(r.URL.Path, "/pulls") && f.pull.State == "closed" {
				res := httptest.NewRecorder()
				reply(res, 200, []Pull{})
				return res.Result(), nil
			}
			return original.RoundTrip(r)
		})
	}
	configure()
	st, err := f.s.store.load()
	if err != nil {
		t.Fatal(err)
	}
	st.Work[key(f.repo.Repo, 42)].Developer.Route = &execution.Route{Provider: "test", Target: "developer", Session: "dev-session"}
	if err := f.s.store.save(st); err != nil {
		t.Fatal(err)
	}
	f.onMerge = func() { f.pull.State, f.pull.Merged = "closed", true }
	f.s.Poll(context.Background())
	if f.writes != 1 || len(e.notices) != 1 {
		t.Fatal("first poll did not deliver and merge")
	}
	f.s.Poll(context.Background()) // Observe the merge before the retry is due.
	if w := notificationWork(t, f.s, f.repo, f.pull); w.Phase != "merged" || w.Notifications[0].State != "awaiting_ack" {
		t.Fatalf("pending merged notification: %+v", w)
	}
	f.s.Close()
	f.s, err = NewService(Config{Repositories: []Repository{f.repo}}, f.path, map[string]execution.Executor{"test": e})
	if err != nil {
		t.Fatal(err)
	}
	configure()
	expireNotification(t, f.s, f.repo, f.pull)
	f.s.Poll(context.Background())
	w := notificationWork(t, f.s, f.repo, f.pull)
	if len(e.notices) != 2 || w.Notifications[0].Attempts != 2 || f.writes != 1 || e.notices[0].EventID != e.notices[1].EventID {
		t.Fatalf("lost retry after merge/restart: %+v, calls=%d", w.Notifications, len(e.notices))
	}
	receiptFirst(t, f.s, e.notices[0].EventID, true)
	receiptFirst(t, f.s, e.notices[0].EventID, false)
	reads := f.reads
	f.s.Poll(context.Background())
	if len(e.notices) != 2 || f.reads != reads {
		t.Fatal("received merged notification kept polling/replaying")
	}
}
