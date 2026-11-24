//go:build linux || darwin

package ink

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type autoMergeFixture struct {
	s                      *Service
	repo                   Repository
	pull                   Pull
	reviews                []Review
	checkState, conclusion string
	checkDenied            bool
	secondPageFailure      bool
	changedOnRevalidation  bool
	reads, writes          int
	lostResponse           bool
	onMerge                func()
	path                   string
}

func newAutoMergeFixture(t *testing.T) *autoMergeFixture {
	t.Helper()
	f := &autoMergeFixture{path: filepath.Join(t.TempDir(), stateDatabase), checkState: "completed", conclusion: "success"}
	f.repo = Repository{Repo: "acme/project", ReviewerLogin: "reviewer", AutoMerge: &AutoMergeConfig{Enabled: true}}
	yes := true
	f.pull = Pull{Number: 42, URL: "https://github.com/acme/project/pull/42", State: "open", Mergeable: &yes, MergeableState: "clean", MergeCommitSHA: strings.Repeat("b", 40)}
	f.pull.Head.SHA = strings.Repeat("a", 40)
	f.pull.Base.SHA = strings.Repeat("c", 40)
	r := Review{ID: 1, SHA: f.pull.Head.SHA, State: "APPROVED", SubmittedAt: time.Now()}
	r.User.Login = "reviewer"
	f.reviews = []Review{r}
	s, err := NewService(Config{Repositories: []Repository{f.repo}}, f.path, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.s = s
	t.Cleanup(func() { f.s.Close() })
	f.installTransport(t)
	st, _ := s.store.load()
	st.Work[key(f.repo.Repo, 42)] = &Work{Repo: f.repo.Repo, Pull: f.pull, Linked: true, Reviewer: Assignee{GitHubLogin: "reviewer"}}
	if err := s.store.save(st); err != nil {
		t.Fatal(err)
	}
	if err := s.observe(f.repo, f.pull, f.reviews); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *autoMergeFixture) installTransport(t *testing.T) {
	f.s.up.http.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		switch {
		case strings.HasSuffix(r.URL.Path, "/merge"):
			f.writes++
			if r.Method != "PUT" {
				t.Fatal("merge must use PUT")
			}
			var input map[string]string
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Fatal(err)
			}
			if input["sha"] != strings.Repeat("a", 40) || input["merge_method"] != f.repo.AutoMerge.method() {
				t.Fatalf("unguarded merge: %+v", input)
			}
			st, err := f.s.store.load()
			if err != nil {
				t.Fatal(err)
			}
			if st.Work[key(f.repo.Repo, 42)].AutoMerges[0].State != "unknown" {
				t.Fatal("merge not reserved before network")
			}
			if f.onMerge != nil {
				f.onMerge()
			}
			if f.lostResponse {
				return nil, errors.New("lost merge response")
			}
			reply(w, 200, map[string]any{"merged": true, "sha": strings.Repeat("d", 40)})
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			if f.checkDenied {
				w.WriteHeader(403)
				break
			}
			conclusion := f.conclusion
			if f.secondPageFailure {
				if r.URL.Query().Get("page") == "1" {
					w.Header().Set("Link", `<https://api.github.com/next>; rel="next"`)
				} else {
					conclusion = "failure"
				}
			}
			reply(w, 200, map[string]any{"check_runs": []map[string]string{{"status": f.checkState, "conclusion": conclusion}}})
		case strings.HasSuffix(r.URL.Path, "/reviews"):
			reply(w, 200, f.reviews)
		case strings.HasSuffix(r.URL.Path, "/pulls/42"):
			f.reads++
			p := f.pull
			if f.changedOnRevalidation && f.reads%2 == 0 {
				p.Head.SHA = strings.Repeat("e", 40)
			}
			reply(w, 200, p)
		case strings.HasSuffix(r.URL.Path, "/pulls"):
			reply(w, 200, []Pull{f.pull})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL)
		}
		return w.Result(), nil
	})
}

func TestAutoMergePollDedupAndRestart(t *testing.T) {
	f := newAutoMergeFixture(t)
	f.s.Poll(context.Background())
	w := notificationWork(t, f.s, f.repo, f.pull)
	if f.writes != 1 || len(w.AutoMerges) != 1 || w.AutoMerges[0].State != "merged" || w.AutoMerges[0].ReviewID != 1 || w.Phase != "approved" {
		t.Fatalf("merge result: %+v, writes %d", w, f.writes)
	}
	f.s.Close()
	s, err := NewService(Config{Repositories: []Repository{f.repo}}, f.path, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.s = s
	f.installTransport(t)
	f.s.Poll(context.Background())
	if f.writes != 1 {
		t.Fatal("restart repeated merge")
	}
}

func TestAutoMergeWaitsForChecksAndMergeability(t *testing.T) {
	for _, scenario := range []string{"queued", "failure", "unknown conclusion", "checks denied", "later page failure", "conflict", "unknown mergeability", "unstable", "head changed", "approval revoked"} {
		t.Run(scenario, func(t *testing.T) {
			f := newAutoMergeFixture(t)
			switch scenario {
			case "queued":
				f.checkState = "in_progress"
			case "failure":
				f.conclusion = "failure"
			case "unknown conclusion":
				f.conclusion = ""
			case "checks denied":
				f.checkDenied = true
			case "later page failure":
				f.secondPageFailure = true
			case "conflict":
				no := false
				f.pull.Mergeable = &no
			case "unknown mergeability":
				f.pull.Mergeable = nil
			case "unstable":
				f.pull.MergeableState = "unstable"
			case "head changed":
				f.changedOnRevalidation = true
			case "approval revoked":
				f.reviews[0].State = "DISMISSED"
			}
			if err := f.s.autoMerge(context.Background(), f.repo, 42); err != nil {
				t.Fatal(err)
			}
			w := notificationWork(t, f.s, f.repo, f.pull)
			if f.writes != 0 || len(w.AutoMerges) != 1 || w.AutoMerges[0].State != "waiting" {
				t.Fatalf("merged without preconditions: %+v", w.AutoMerges)
			}
		})
	}
}

func TestAutoMergeDisabledAndNonQualifyingReviews(t *testing.T) {
	for _, scenario := range []string{"disabled", "draft", "closed", "wrong author", "old sha", "changes requested", "unlinked"} {
		t.Run(scenario, func(t *testing.T) {
			f := newAutoMergeFixture(t)
			switch scenario {
			case "disabled":
				f.repo.AutoMerge = &AutoMergeConfig{}
			case "draft":
				f.pull.Draft = true
			case "closed":
				f.pull.State = "closed"
			case "wrong author":
				f.reviews[0].User.Login = "other"
			case "old sha":
				f.reviews[0].SHA = strings.Repeat("f", 40)
			case "changes requested":
				f.reviews[0].State = "CHANGES_REQUESTED"
			case "unlinked":
				st, _ := f.s.store.load()
				st.Work[key(f.repo.Repo, 42)].Linked = false
				if err := f.s.store.save(st); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.s.observe(f.repo, f.pull, f.reviews); err != nil {
				t.Fatal(err)
			}
			if err := f.s.autoMerge(context.Background(), f.repo, 42); err != nil {
				t.Fatal(err)
			}
			if f.reads != 0 || f.writes != 0 {
				t.Fatal("automatic merge ran while disabled or unapproved")
			}
		})
	}
}

func TestAutoMergeUnknownReconcilesWithoutReplay(t *testing.T) {
	f := newAutoMergeFixture(t)
	f.lostResponse = true
	if err := f.s.autoMerge(context.Background(), f.repo, 42); err != nil {
		t.Fatal(err)
	}
	w := notificationWork(t, f.s, f.repo, f.pull)
	if w.AutoMerges[0].State != "unknown" {
		t.Fatal("lost response claimed success")
	}
	if err := f.s.autoMerge(context.Background(), f.repo, 42); err != nil {
		t.Fatal(err)
	}
	if f.writes != 1 {
		t.Fatal("ambiguous write repeated")
	}
	f.pull.State = "closed"
	f.pull.Merged = true
	f.pull.MergeCommitSHA = strings.Repeat("d", 40)
	if err := f.s.observe(f.repo, f.pull, nil); err != nil {
		t.Fatal(err)
	}
	f.repo.AutoMerge = &AutoMergeConfig{}
	if err := f.s.autoMerge(context.Background(), f.repo, 42); err != nil {
		t.Fatal(err)
	}
	w = notificationWork(t, f.s, f.repo, f.pull)
	if w.AutoMerges[0].State != "merged" || w.AutoMerges[0].MergeSHA != f.pull.MergeCommitSHA || w.Phase != "merged" || f.writes != 1 {
		t.Fatal("merge observation did not reconcile unknown outcome")
	}
}

func TestAutoMergeConcurrentReservationAndWaitingCannotOverwrite(t *testing.T) {
	f := newAutoMergeFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	f.onMerge = func() { close(entered); <-release }
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := f.s.autoMerge(context.Background(), f.repo, 42); err != nil {
			t.Error(err)
		}
	}()
	<-entered
	if err := f.s.autoMerge(context.Background(), f.repo, 42); err != nil {
		t.Fatal(err)
	}
	_, err := f.s.updateAutoMerge(f.repo, 42, AutoMergeRun{SHA: f.pull.Head.SHA, State: "waiting"}, false)
	if err != nil {
		t.Fatal(err)
	}
	w := notificationWork(t, f.s, f.repo, f.pull)
	if w.AutoMerges[0].State != "unknown" {
		t.Fatal("waiting preflight removed reservation")
	}
	close(release)
	wg.Wait()
	if f.writes != 1 {
		t.Fatal("concurrent merge duplicated")
	}
}

func TestAutoMergeRegistrationRequiresRepositoryOptIn(t *testing.T) {
	path := filepath.Join(t.TempDir(), stateDatabase)
	cfg := Config{RepositoryDefaults: Repository{ReviewerLogin: "reviewer", AutoMerge: &AutoMergeConfig{Enabled: true, Method: "squash"}}}
	s, err := NewService(cfg, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.up.mergerCommand = filepath.Join(t.TempDir(), "merger")
	if err := os.WriteFile(s.up.mergerCommand, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	s.up.http.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		reply(w, 200, []Pull{})
		return w.Result(), nil
	})
	for _, body := range []string{`{"repo":"acme/inherit"}`, `{"repo":"acme/disabled","auto_merge":{"enabled":false}}`, `{"repo":"acme/enabled","auto_merge":{"enabled":true}}`} {
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
		if (r.AutoMerge != nil && r.AutoMerge.Enabled) != (r.Repo == "acme/enabled") {
			t.Fatal("lost explicit auto merge policy")
		}
	}
}

func TestAutoMergeWaitEndsWhenChecksPass(t *testing.T) {
	f := newAutoMergeFixture(t)
	f.checkState = "in_progress"
	if err := f.s.autoMerge(context.Background(), f.repo, 42); err != nil {
		t.Fatal(err)
	}
	if f.writes != 0 {
		t.Fatal("merged while checks running")
	}
	f.checkState = "completed"
	if err := f.s.autoMerge(context.Background(), f.repo, 42); err != nil {
		t.Fatal(err)
	}
	if f.writes != 1 {
		t.Fatal("did not resume after checks passed")
	}
}

func TestHistoricalUnknownStaysVisibleAndBlocksNewApprovedHead(t *testing.T) {
	f := newAutoMergeFixture(t)
	f.lostResponse = true
	if err := f.s.autoMerge(context.Background(), f.repo, 42); err != nil {
		t.Fatal(err)
	}
	f.pull.Head.SHA = strings.Repeat("e", 40)
	f.reviews[0].ID = 2
	f.reviews[0].SHA = f.pull.Head.SHA
	if err := f.s.observe(f.repo, f.pull, f.reviews); err != nil {
		t.Fatal(err)
	}
	w := notificationWork(t, f.s, f.repo, f.pull)
	active := false
	for _, a := range w.Alerts {
		if a.Code == "auto_merge_unknown" && a.Active {
			active = true
		}
	}
	if !active || w.Workflow.State != "needs_attention" {
		t.Fatal("old unknown disappeared after head change")
	}
	reads := f.reads
	if err := f.s.autoMerge(context.Background(), f.repo, 42); err != nil {
		t.Fatal(err)
	}
	if f.writes != 1 || f.reads != reads {
		t.Fatal("new approved head proceeded while an old merge remained unknown")
	}
	// Also fence a preflight that started before the older request was reserved.
	ok, err := f.s.updateAutoMerge(f.repo, 42, AutoMergeRun{SHA: f.pull.Head.SHA, Reviewer: "reviewer", Method: "merge", State: "unknown"}, true)
	if err != nil || ok {
		t.Fatal("new SHA acquired a reservation despite historical unknown")
	}
}

func TestHistoricalUnknownsAllReconcileWhenPRCloses(t *testing.T) {
	for _, merged := range []bool{false, true} {
		f := newAutoMergeFixture(t)
		oldSHA := strings.Repeat("e", 40)
		st, err := f.s.store.load()
		if err != nil {
			t.Fatal(err)
		}
		st.Work[key(f.repo.Repo, 42)].AutoMerges = []AutoMergeRun{
			{SHA: oldSHA, Reviewer: "reviewer", State: "unknown", Reason: "response lost"},
			{SHA: f.pull.Head.SHA, Reviewer: "reviewer", State: "unknown", Reason: "response lost"},
		}
		if err := f.s.store.save(st); err != nil {
			t.Fatal(err)
		}
		f.pull.State = "closed"
		f.pull.Merged = merged
		f.pull.MergeCommitSHA = strings.Repeat("d", 40)
		if err := f.s.observe(f.repo, f.pull, nil); err != nil {
			t.Fatal(err)
		}
		f.repo.AutoMerge = &AutoMergeConfig{}
		if err := f.s.autoMerge(context.Background(), f.repo, 42); err != nil {
			t.Fatal(err)
		}
		w := notificationWork(t, f.s, f.repo, f.pull)
		for _, r := range w.AutoMerges {
			if !merged {
				if r.State != "failed" {
					t.Fatalf("closed PR left unresolved record: %+v", r)
				}
				continue
			}
			if r.SHA == oldSHA {
				if r.State != "superseded" || r.Reason == "" || r.MergeSHA != "" {
					t.Fatalf("different head falsely attributed as merged or left unknown: %+v", r)
				}
			} else if r.State != "merged" || r.MergeSHA != f.pull.MergeCommitSHA {
				t.Fatalf("matching head not reconciled: %+v", r)
			}
		}
		for _, a := range w.Alerts {
			if a.Code == "auto_merge_unknown" && a.Active {
				t.Fatal("closed PR retained unresolved merge diagnostic")
			}
		}
		if f.writes != 0 {
			t.Fatal("read-only reconciliation submitted a merge")
		}
	}
}

func TestAutoMergeRetriesWhenPinnedMergerIsRestored(t *testing.T) {
	for _, tc := range []struct {
		name, response, wantState, wantRequestID string
		stack                                    bool
	}{
		{name: "ordinary", response: "HTTP/1.1 200 OK", wantState: "merged"},
		{name: "stack", response: "HTTP/1.1 202 Accepted", wantState: "pending", wantRequestID: testStackRequestID, stack: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAutoMergeFixture(t)
			if tc.stack {
				f.setStack(1, "main")
			}
			dir := t.TempDir()
			merger := filepath.Join(dir, "merger")
			marker := filepath.Join(dir, "calls")
			t.Setenv("INK_TEST_MERGER_CALLS", marker)
			payload := fmt.Sprintf(`{"merged":true,"sha":%q}`, strings.Repeat("d", 40))
			if tc.stack {
				payload = fmt.Sprintf(`{"status":"pending","details":{"uuid":%q}}`, testStackRequestID)
			}
			valid := []byte(fmt.Sprintf(`#!/bin/sh
printf x >> "$INK_TEST_MERGER_CALLS"
cat >/dev/null
printf '%%s\r\n\r\n%%s' '%s' '%s'
`, tc.response, payload))
			if err := os.WriteFile(merger, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
				t.Fatal(err)
			}
			f.s.up.mergerCommand, f.s.up.mergerDigest = merger, fmt.Sprintf("%x", sha256.Sum256(valid))
			ctx := context.Background()
			if err := f.s.autoMerge(ctx, f.repo, f.pull.Number); err != nil {
				t.Fatal(err)
			}
			work := notificationWork(t, f.s, f.repo, f.pull)
			if len(work.AutoMerges) != 1 || work.AutoMerges[0].State != "waiting" ||
				!strings.Contains(work.AutoMerges[0].Reason, "checksum mismatch") || hasInFlightAutoMerge(work) {
				t.Fatalf("merge should be eligible for retry: %+v", work.AutoMerges)
			}
			if err := os.WriteFile(merger, valid, 0700); err != nil {
				t.Fatal(err)
			}
			if err := f.s.autoMerge(ctx, f.repo, f.pull.Number); err != nil {
				t.Fatal(err)
			}
			work = notificationWork(t, f.s, f.repo, f.pull)
			run := work.AutoMerges[0]
			if run.State != tc.wantState || run.RequestID != tc.wantRequestID {
				t.Fatalf("merge did not resume after restoring the pinned command: %+v", run)
			}
			if calls, err := os.ReadFile(marker); err != nil || string(calls) != "x" {
				t.Fatalf("expected one write attempt, marker %q, error %v", calls, err)
			}
		})
	}
}

func TestAutoMergeReservationRejectsChangedIdentityMode(t *testing.T) {
	f := newAutoMergeFixture(t)
	current := f.repo
	current.AutoMerge = &AutoMergeConfig{Enabled: true, ReuseReaderIdentity: true}
	setRepositoryConfigForTest(t, f.s, current)
	run := AutoMergeRun{SHA: f.pull.Head.SHA, Reviewer: f.repo.ReviewerLogin, Method: "merge", State: "unknown"}
	reserved, err := f.s.updateAutoMerge(f.repo, 42, run, true)
	if err != nil || reserved {
		t.Fatalf("reserved merge under a stale identity policy: %v (%v)", reserved, err)
	}
}
