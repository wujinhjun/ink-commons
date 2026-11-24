//go:build linux || darwin

package ink

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wujinhjun/ink-commons/internal/execution"
)

func readNoticeStatus(t *testing.T, s *Service, id string, code int) *execution.MergeStatus {
	t.Helper()
	res := httptest.NewRecorder()
	s.Handler().ServeHTTP(res, httptest.NewRequest("GET", "/v1/notifications/status?event_id="+url.QueryEscape(id), nil))
	if res.Code != code {
		t.Fatalf("status HTTP %d: %s", res.Code, res.Body)
	}
	if code != 200 {
		return nil
	}
	var result struct {
		EventID string                 `json:"event_id"`
		Merge   *execution.MergeStatus `json:"merge"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &result); err != nil || result.EventID != id || result.Merge == nil {
		t.Fatalf("invalid status: %s", res.Body)
	}
	return result.Merge
}

func TestNotificationStatusUsesCurrentReadOnlyObservation(t *testing.T) {
	e := &notificationExecutor{requireACK: true}
	s, repo, p, r, _ := notificationFixture(t, e)
	repo.AutoMerge = &AutoMergeConfig{Enabled: true, Method: "squash"}
	setRepositoryConfigForTest(t, s, repo)
	if err := s.observe(repo, p, []Review{r}); err != nil {
		t.Fatal(err)
	}
	if err := s.notifyApproved(context.Background(), repo, p.Number); err != nil {
		t.Fatal(err)
	}
	event := e.notices[0].EventID
	if e.notices[0].Merge == nil || e.notices[0].Merge.State != "waiting" || !e.notices[0].Merge.Enabled {
		t.Fatal("approval missing automatic merge policy")
	}
	s.up.http.Transport = testTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("read-only status called upstream")
		return nil, nil
	})
	for _, tc := range []struct {
		name, state, phase, runState string
		enabled, merged, changed     bool
	}{
		{"waiting", "waiting", "approved", "", true, false, false},
		{"checks pending", "waiting", "approved", "waiting", true, false, false},
		{"merge succeeded before next poll", "merged", "approved", "merged", true, false, false},
		{"lost response", "unknown", "approved", "unknown", true, false, false},
		{"blocked", "blocked", "approved", "failed", true, false, false},
		{"disabled", "disabled", "approved", "", false, false, false},
		{"disabled after success", "merged", "approved", "merged", false, false, false},
		{"closed", "closed", "closed", "", true, false, false},
		{"approval revoked", "not_approved", "changes_requested", "", true, false, false},
		{"new head", "superseded", "review_pending", "", true, false, true},
		{"observed merge", "merged", "merged", "", false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, _ := loadFixture(s.store)
			w := st.Work[key(repo.Repo, p.Number)]
			w.Pull = p
			w.Phase = tc.phase
			w.Pull.Merged = tc.merged
			w.Pull.MergeCommitSHA = strings.Repeat("d", 40)
			if tc.phase == "closed" {
				w.Pull.State = "closed"
			}
			if tc.changed {
				w.Pull.Head.SHA = strings.Repeat("b", 40)
			}
			w.AutoMerges = nil
			if tc.runState != "" {
				w.AutoMerges = []AutoMergeRun{{SHA: p.Head.SHA, State: tc.runState, Method: "squash", Reason: "check result", MergeSHA: strings.Repeat("d", 40)}}
			}
			repo.AutoMerge = &AutoMergeConfig{Enabled: tc.enabled, Method: "squash"}
			setRepositoryConfigForTest(t, s, repo)
			if err := saveFixture(s.store, st); err != nil {
				t.Fatal(err)
			}
			before, _ := json.Marshal(st)
			status := readNoticeStatus(t, s, event, 200)
			if status.State != tc.state || status.Enabled != tc.enabled {
				t.Fatalf("status %+v", status)
			}
			if tc.state == "merged" && status.MergeSHA != strings.Repeat("d", 40) {
				t.Fatal("merge SHA missing")
			}
			next, _ := loadFixture(s.store)
			after, _ := json.Marshal(next)
			if string(before) != string(after) {
				t.Fatal("status mutated persisted state or acknowledged event")
			}
		})
	}
	readNoticeStatus(t, s, "missing", 404)
	readNoticeStatus(t, s, "", 400)
	if _, err := s.store.db.Exec("DELETE FROM runtime_config WHERE scope = ?", repo.Repo); err != nil {
		t.Fatal(err)
	}
	readNoticeStatus(t, s, event, 409)
}

func TestMergedNoticeIsReservedWithResultAndRecoveredAfterRestart(t *testing.T) {
	for _, ack := range []bool{false, true} {
		name := "once"
		if ack {
			name = "receipt"
		}
		t.Run(name, func(t *testing.T) {
			e := &notificationExecutor{requireACK: ack}
			s, repo, p, r, path := notificationFixture(t, e)
			repo.AutoMerge = &AutoMergeConfig{Enabled: true}
			setRepositoryConfigForTest(t, s, repo)
			if err := s.observe(repo, p, []Review{r}); err != nil {
				t.Fatal(err)
			}
			run := AutoMergeRun{SHA: p.Head.SHA, State: "merged", Reviewer: r.User.Login, ReviewID: r.ID, Method: "merge", MergeSHA: strings.Repeat("d", 40)}
			if _, err := s.updateAutoMerge(repo, p.Number, run, false); err != nil {
				t.Fatal(err)
			}
			w := notificationWork(t, s, repo, p)
			if len(w.Notifications) != 1 || w.Notifications[0].Event != "pr_merged" || w.Notifications[0].Attempts != 0 {
				t.Fatal("merge result did not atomically reserve notification")
			}
			event := w.Notifications[0].EventID
			// 模拟在发送前退出，GitHub 之后已从 open 列表移除该 PR。
			st, _ := loadFixture(s.store)
			w = st.Work[key(repo.Repo, p.Number)]
			w.Pull.State = "closed"
			w.Pull.Merged = true
			w.Phase = "merged"
			if err := saveFixture(s.store, st); err != nil {
				t.Fatal(err)
			}
			if !pollIncludes(t, s, repo.Repo, p.Number) {
				t.Fatal("unsent completion excluded from closed PR recovery")
			}
			s.Close()
			s2, err := NewService(Config{Repositories: []Repository{repo}}, path, map[string]execution.Executor{"test": e})
			if err != nil {
				t.Fatal(err)
			}
			defer s2.Close()
			if err := s2.notifyMerged(context.Background(), repo, p.Number); err != nil {
				t.Fatal(err)
			}
			if len(e.notices) != 1 || e.notices[0].EventID != event || e.notices[0].Merge.State != "merged" {
				t.Fatal("completion lost across restart")
			}
			if err := s2.notifyMerged(context.Background(), repo, p.Number); err != nil {
				t.Fatal(err)
			}
			if len(e.notices) != 1 {
				t.Fatal("completion repeated before deadline")
			}
			if ack {
				st, _ := loadFixture(s2.store)
				st.Work[key(repo.Repo, p.Number)].Notifications[0].RetryAt = time.Time{}
				saveFixture(s2.store, st)
				if err := s2.notifyMerged(context.Background(), repo, p.Number); err != nil {
					t.Fatal(err)
				}
				if len(e.notices) != 2 || e.notices[1].EventID != event {
					t.Fatal("completion not retried")
				}
				receiptFirst(t, s2, event, true)
				receiptFirst(t, s2, event, false)
			}
			if pollIncludes(t, s2, repo.Repo, p.Number) {
				t.Fatal("terminal notice kept polling closed PR")
			}
		})
	}
}

func TestMergedReceiptSupersedesUnconsumedApproval(t *testing.T) {
	e := &notificationExecutor{requireACK: true}
	s, repo, p, r, _ := notificationFixture(t, e)
	if err := s.observe(repo, p, []Review{r}); err != nil {
		t.Fatal(err)
	}
	if err := s.notifyApproved(context.Background(), repo, p.Number); err != nil {
		t.Fatal(err)
	}
	if _, err := s.updateAutoMerge(repo, p.Number, AutoMergeRun{SHA: p.Head.SHA, State: "merged", Reviewer: r.User.Login, ReviewID: r.ID, MergeSHA: strings.Repeat("d", 40)}, false); err != nil {
		t.Fatal(err)
	}
	w := notificationWork(t, s, repo, p)
	receiptFirst(t, s, w.Notifications[1].EventID, true)
	receiptFirst(t, s, w.Notifications[0].EventID, false)
	if notificationWork(t, s, repo, p).Notifications[0].State != "superseded" {
		t.Fatal("approval reminder survived completion receipt")
	}
}

func TestApprovalStatusCanAdvanceWhileNoticeIsInFlight(t *testing.T) {
	e := &notificationExecutor{requireACK: true}
	s, repo, p, r, _ := notificationFixture(t, e)
	repo.AutoMerge = &AutoMergeConfig{Enabled: true}
	setRepositoryConfigForTest(t, s, repo)
	if err := s.observe(repo, p, []Review{r}); err != nil {
		t.Fatal(err)
	}
	e.onNotify = func() {
		if _, err := s.updateAutoMerge(repo, p.Number, AutoMergeRun{SHA: p.Head.SHA, State: "merged", Reviewer: r.User.Login, ReviewID: r.ID, Method: "merge", MergeSHA: strings.Repeat("d", 40)}, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.notifyApproved(context.Background(), repo, p.Number); err != nil {
		t.Fatal(err)
	}
	if e.notices[0].Merge.State != "waiting" {
		t.Fatal("test did not capture pre-merge delivery snapshot")
	}
	event := e.notices[0].EventID
	receiptFirst(t, s, event, true)
	if status := readNoticeStatus(t, s, event, 200); status.State != "merged" {
		t.Fatalf("reply would use stale snapshot: %+v", status)
	}
}

func TestAmbiguousMergeNotifiesOnlyAfterSameHeadIsConfirmed(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "same head", true: "different head"}[changed], func(t *testing.T) {
			f := newAutoMergeFixture(t)
			e := &notificationExecutor{requireACK: true}
			f.s.providers = map[string]execution.Executor{"test": e}
			st, _ := loadFixture(f.s.store)
			work := st.Work[key(f.repo.Repo, 42)]
			work.Developer.Route = &execution.Route{Provider: "test", Target: "developer", Session: "dev-session"}
			work.DeveloperOverride = cloneAssignee(work.Developer)
			saveFixture(f.s.store, st)
			f.lostResponse = true
			if err := f.s.autoMerge(context.Background(), f.repo, 42); err != nil {
				t.Fatal(err)
			}
			if len(notificationWork(t, f.s, f.repo, f.pull).Notifications) != 0 {
				t.Fatal("unknown merge generated success notice")
			}
			f.pull.State = "closed"
			f.pull.Merged = true
			f.pull.MergeCommitSHA = strings.Repeat("d", 40)
			if changed {
				f.pull.Head.SHA = strings.Repeat("e", 40)
			}
			if err := f.s.observe(f.repo, f.pull, nil); err != nil {
				t.Fatal(err)
			}
			if err := f.s.autoMerge(context.Background(), f.repo, 42); err != nil {
				t.Fatal(err)
			}
			w := notificationWork(t, f.s, f.repo, f.pull)
			if changed {
				if len(w.Notifications) != 0 {
					t.Fatal("old SHA reported as merged into different head")
				}
				return
			}
			if len(w.Notifications) != 1 || w.Notifications[0].Event != "pr_merged" || !pollIncludes(t, f.s, f.repo.Repo, f.pull.Number) {
				t.Fatal("confirmed result did not reserve durable notice")
			}
			if err := f.s.notifyMerged(context.Background(), f.repo, 42); err != nil {
				t.Fatal(err)
			}
			if len(e.notices) != 1 || f.writes != 1 {
				t.Fatal("reconciliation repeated merge or lost notice")
			}
		})
	}
}

func TestHistoricalLegacyApprovalWithoutAttemptCountIsNotReplayed(t *testing.T) {
	e := &notificationExecutor{}
	s, repo, p, r, _ := notificationFixture(t, e)
	if err := s.observe(repo, p, []Review{r}); err != nil {
		t.Fatal(err)
	}
	if err := s.notifyApproved(context.Background(), repo, p.Number); err != nil {
		t.Fatal(err)
	}
	st, _ := loadFixture(s.store)
	n := &st.Work[key(repo.Repo, p.Number)].Notifications[0]
	n.Attempts = 0
	n.State = "unknown"
	saveFixture(s.store, st)
	if err := s.notifyApproved(context.Background(), repo, p.Number); err != nil {
		t.Fatal(err)
	}
	if len(e.notices) != 1 {
		t.Fatal("upgrade replayed historical once-only approval")
	}
}

func TestPollRecoversMergeAfterClosedObservationBeforeReconcile(t *testing.T) {
	f := newAutoMergeFixture(t)
	e := &notificationExecutor{requireACK: true}
	f.s.providers = map[string]execution.Executor{"test": e}
	st, _ := loadFixture(f.s.store)
	work := st.Work[key(f.repo.Repo, 42)]
	work.Developer.Route = &execution.Route{Provider: "test", Target: "developer", Session: "dev-session"}
	work.DeveloperOverride = cloneAssignee(work.Developer)
	saveFixture(f.s.store, st)
	f.lostResponse = true
	if err := f.s.autoMerge(context.Background(), f.repo, 42); err != nil {
		t.Fatal(err)
	}
	f.pull.State = "closed"
	f.pull.Merged = true
	f.pull.MergeCommitSHA = strings.Repeat("d", 40)
	if err := f.s.observe(f.repo, f.pull, nil); err != nil {
		t.Fatal(err)
	}
	// 在 observe 保存 closed 之后、对账及完成事件预留之前退出。
	f.s.Close()
	var err error
	f.s, err = NewService(Config{Repositories: []Repository{f.repo}}, f.path, map[string]execution.Executor{"test": e})
	if err != nil {
		t.Fatal(err)
	}
	f.installTransport(t)
	original := f.s.up.http.Transport
	f.s.up.http.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/pulls") {
			out := httptest.NewRecorder()
			reply(out, 200, []Pull{})
			return out.Result(), nil
		}
		return original.RoundTrip(r)
	})
	f.s.Poll(context.Background())
	if len(e.notices) != 1 || e.notices[0].Event != "pr_merged" || f.writes != 1 {
		t.Fatal("closed observation lost merge reconciliation/notice or repeated write")
	}
}

func TestCompletedMergeDoesNotBackfillUnreservedNotices(t *testing.T) {
	f := newAutoMergeFixture(t)
	e := &notificationExecutor{requireACK: true}
	f.s.providers = map[string]execution.Executor{"test": e}
	st, _ := loadFixture(f.s.store)
	w := st.Work[key(f.repo.Repo, 42)]
	w.Developer.Route = &execution.Route{Provider: "test", Target: "developer", Session: "dev-session"}
	w.AutoMerges = []AutoMergeRun{{SHA: f.pull.Head.SHA, State: "merged", MergeSHA: strings.Repeat("d", 40)}}
	w.Pull.State = "closed"
	w.Pull.Merged = true
	w.Phase = "merged"
	if err := saveFixture(f.s.store, st); err != nil {
		t.Fatal(err)
	}
	if err := f.s.autoMerge(context.Background(), f.repo, 42); err != nil {
		t.Fatal(err)
	}
	if err := f.s.notifyMerged(context.Background(), f.repo, 42); err != nil {
		t.Fatal(err)
	}
	w = notificationWork(t, f.s, f.repo, f.pull)
	if len(w.Notifications) != 0 || len(e.notices) != 0 || pollIncludes(t, f.s, f.repo.Repo, f.pull.Number) {
		t.Fatal("upgrade backfilled already completed merges")
	}
}

func TestPollReconcilesOnlyPendingClosedPullsAfterRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := InitializeState(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, stateDatabase)
	repo := Repository{Repo: "acme/project", ReviewerLogin: "reviewer"}
	e := &notificationExecutor{requireACK: true}
	cfg := Config{Repositories: []Repository{repo}}
	providers := map[string]execution.Executor{"test": e}
	s, err := NewService(cfg, path, providers)
	if err != nil {
		t.Fatal(err)
	}
	route := &execution.Route{Provider: "test", Target: "developer", Session: "dev-session"}
	owner := Assignee{Route: route}
	sha := strings.Repeat("a", 40)
	pulls := make(map[string]Pull)
	for _, n := range []int{10, 11, 12, 99} {
		pull := Pull{Number: n, State: "closed", Merged: n == 10}
		pull.Head.SHA = sha
		pulls[fmt.Sprintf("/repos/acme/project/pulls/%d", n)] = pull
		work := &Work{Repo: repo.Repo, Pull: pull, Linked: true, Developer: owner, DeveloperOverride: &owner}
		work.derive()
		switch n {
		case 10, 11:
			event := "review_approved"
			if n == 10 {
				event = "pr_merged"
			}
			work.Notifications = []NotificationRun{{
				Notification: execution.Notification{EventID: fmt.Sprintf("event-%d", n), Event: event, SHA: sha},
				Route:        route, RequireACK: true, State: "awaiting_ack", Attempts: 1,
			}}
		case 12:
			work.AutoMerges = []AutoMergeRun{{State: "pending", SHA: sha, RequestID: testStackRequestID}}
		}
		if err := s.store.saveWork(work); err != nil {
			t.Fatal(err)
		}
	}
	// A malformed archived payload must not interfere with polling other PRs.
	if _, err := s.store.db.Exec("UPDATE work_items SET payload = 'broken' WHERE id = 'acme/project#99'"); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s, err = NewService(cfg, path, providers)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var readNumbers []int
	s.up.github = "https://stub.invalid"
	s.up.http.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
		out := httptest.NewRecorder()
		if r.URL.Path == "/repos/acme/project/pulls" && r.URL.Query().Get("state") == "open" {
			reply(out, 200, []Pull{})
		} else if pull, ok := pulls[r.URL.Path]; ok && r.Method == http.MethodGet {
			readNumbers = append(readNumbers, pull.Number)
			reply(out, 200, pull)
		} else {
			t.Fatalf("unexpected GitHub request: %s %s", r.Method, r.URL)
		}
		return out.Result(), nil
	})
	if err := s.pollRepo(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(readNumbers, []int{10, 12}) {
		t.Fatalf("unexpected closed PR lookups: %v", readNumbers)
	}
	if len(e.notices) != 1 || e.notices[0].EventID != "event-10" {
		t.Fatalf("wrong pending receipt sent: %+v", e.notices)
	}
	receiptFirst(t, s, "event-10", true)
	receiptFirst(t, s, "event-10", false)
	closed, err := s.store.loadNotification("event-11")
	if err != nil || closed == nil || closed.Notifications[0].State != "awaiting_ack" {
		t.Fatalf("closed unmerged receipt was not preserved: %+v, %v", closed, err)
	}
	merged, err := s.store.loadWork(key(repo.Repo, 12))
	if err != nil || merged == nil || merged.AutoMerges[0].State != "failed" {
		t.Fatalf("pending stack request not reconciled from closed PR: %+v, %v", merged, err)
	}
	readNumbers = nil
	if err := s.pollRepo(context.Background(), repo); err != nil || len(readNumbers) != 0 {
		t.Fatalf("settled PRs kept polling: %v, %v", readNumbers, err)
	}
}
