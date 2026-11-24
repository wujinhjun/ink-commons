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
	"sync"
	"testing"
	"time"

	"github.com/wujinhjun/ink-commons/internal/execution"
)

type wakeExecutor struct {
	testExecutor
	wakes        []recordedDelivery
	prepareError bool
}

func (e *wakeExecutor) WakeTask(_ context.Context, r execution.Route, d execution.Delivery) error {
	e.wakes = append(e.wakes, recordedDelivery{r, d})
	return nil
}
func (e *wakeExecutor) Prepare(ctx context.Context, r execution.Route, d execution.Delivery) (string, error) {
	if e.prepareError {
		e.prepared++
		return "", fmt.Errorf("lost allocation response")
	}
	return e.testExecutor.Prepare(ctx, r, d)
}

type handoffFixture struct {
	s                         *Service
	repo                      Repository
	pull                      Pull
	reviews                   []Review
	executor                  *wakeExecutor
	path                      string
	githubError               bool
	conflictBaseSHA           string
	conflictRefMergeable      bool
	conflictDetailBaseSHA     string
	conflictLookupUnavailable bool
	beforeRead                func()
}

func newHandoffFixture(t *testing.T) *handoffFixture {
	t.Helper()
	f := &handoffFixture{executor: &wakeExecutor{}, path: filepath.Join(t.TempDir(), stateDatabase)}
	f.repo = Repository{Repo: "acme/project", TaskMode: "cooperative", ReviewerLogin: "reviewer", ReviewerRoute: &execution.Route{Provider: "test", Target: "reviewer", Session: "review-session"}}
	f.pull = Pull{Number: 42, URL: "https://github.com/acme/project/pull/42", State: "open"}
	f.pull.Head.SHA = strings.Repeat("a", 40)
	s, err := NewService(Config{Repositories: []Repository{f.repo}}, f.path, map[string]execution.Executor{"test": f.executor})
	if err != nil {
		t.Fatal(err)
	}
	f.s = s
	t.Cleanup(func() { f.s.Close() })
	f.upstream()
	st, _ := s.store.load()
	w := &Work{Repo: f.repo.Repo, TaskMode: "cooperative", Linked: true, Pull: f.pull, Reviewer: Assignee{GitHubLogin: "reviewer", Route: f.repo.ReviewerRoute}, Developer: Assignee{GitHubLogin: "developer", Route: &execution.Route{Provider: "test", Target: "developer", Session: "dev-session"}}}
	w.derive()
	st.Work[key(f.repo.Repo, 42)] = w
	if err := s.store.save(st); err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *handoffFixture) upstream() {
	f.s.up.github = "https://stub.invalid"
	f.s.up.http = &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		if f.beforeRead != nil {
			fn := f.beforeRead
			f.beforeRead = nil
			fn()
		}
		out := httptest.NewRecorder()
		if f.githubError {
			out.WriteHeader(503)
		} else if r.URL.Path == "/graphql" {
			if f.conflictLookupUnavailable {
				out.WriteHeader(503)
				return out.Result(), nil
			}
			base := f.conflictBaseSHA
			if base == "" {
				base = f.pull.Base.SHA
			}
			mergeable, state := "CONFLICTING", "DIRTY"
			if f.conflictRefMergeable {
				mergeable, state = "MERGEABLE", "UNSTABLE"
			}
			reply(out, 200, map[string]any{"data": map[string]any{"repository": map[string]any{
				"pullRequest": map[string]any{
					"state": "OPEN", "headRefOid": f.pull.Head.SHA, "baseRefName": f.pull.Base.Ref,
					"mergeable": mergeable, "mergeStateStatus": state,
					"baseRef": map[string]any{"target": map[string]string{"oid": base}},
				},
			}}})
		} else if strings.HasSuffix(r.URL.Path, "/reviews") {
			reply(out, 200, f.reviews)
		} else {
			pull := f.pull
			if r.URL.Path == fmt.Sprintf("/repos/%s/pulls/%d", f.repo.Repo, pull.Number) && f.conflictDetailBaseSHA != "" {
				pull.Base.SHA = f.conflictDetailBaseSHA
			}
			reply(out, 200, pull)
		}
		return out.Result(), nil
	})}
}
func (f *handoffFixture) work(t *testing.T) *Work {
	t.Helper()
	st, err := f.s.store.load()
	if err != nil {
		t.Fatal(err)
	}
	return st.Work[key(f.repo.Repo, 42)]
}
func (f *handoffFixture) call(t *testing.T, action, id string, input any, status int) map[string]any {
	t.Helper()
	data, _ := json.Marshal(input)
	r := httptest.NewRequest("POST", "/v1/tasks/"+id+"/"+action, bytes.NewReader(data))
	out := httptest.NewRecorder()
	f.s.Handler().ServeHTTP(out, r)
	if out.Code != status {
		t.Fatalf("%s %s got %d want %d: %s", action, id, out.Code, status, out.Body)
	}
	var result map[string]any
	_ = json.Unmarshal(out.Body.Bytes(), &result)
	return result
}
func (f *handoffFixture) claim(t *testing.T, task *Task, token string, status int) map[string]any {
	return f.call(t, "claim", task.ID, handoffInput{Token: token, Route: task.Handoff.Assignee.Route}, status)
}
func (f *handoffFixture) submit(t *testing.T, task *Task, token string, result HandoffSubmission, status int) map[string]any {
	return f.call(t, "submit", task.ID, handoffInput{Token: token, Submission: &result}, status)
}
func (f *handoffFixture) observe(t *testing.T) {
	t.Helper()
	if err := f.s.observe(f.repo, f.pull, f.reviews); err != nil {
		t.Fatal(err)
	}
}
func (f *handoffFixture) verify(t *testing.T) {
	t.Helper()
	if err := f.s.reconcileHandoffs(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func (f *handoffFixture) restart(t *testing.T) {
	t.Helper()
	cfg := f.s.config
	f.s.Close()
	s, err := NewService(cfg, f.path, map[string]execution.Executor{"test": f.executor})
	if err != nil {
		t.Fatal(err)
	}
	f.s = s
	f.upstream()
}
func reviewFor(id int64, sha, login, state string) Review {
	r := Review{ID: id, SHA: sha, State: state, SubmittedAt: time.Now()}
	r.User.Login = login
	return r
}

var claimA = strings.Repeat("a", 64)
var claimB = strings.Repeat("b", 64)

func TestCooperativeHandoffFullLoopWithGitHubObservingFirst(t *testing.T) {
	f := newHandoffFixture(t)
	reviewTask := f.work(t).Task
	if got := f.claim(t, reviewTask, claimA, 200); got["first"] != true {
		t.Fatal(got)
	}
	f.restart(t)
	if got := f.claim(t, reviewTask, claimA, 200); got["first"] != false {
		t.Fatalf("replaying the persisted claim acquired a second turn: %v", got)
	}
	f.claim(t, reviewTask, claimB, 409)
	// GitHub's result creates a successor, but it must remain unclaimable.
	f.reviews = []Review{reviewFor(7, f.pull.Head.SHA, "reviewer", "CHANGES_REQUESTED")}
	f.observe(t)
	dev := f.work(t).Task
	if dev.Role != "developer" || !f.work(t).executionBlocked() {
		t.Fatal("review result released a claim")
	}
	f.claim(t, dev, claimB, 409)
	result := HandoffSubmission{RequestID: "review-A", Handoff: true, ReviewID: 7, Outcome: "CHANGES_REQUESTED"}
	f.submit(t, reviewTask, claimA, result, 200)
	f.verify(t)
	if f.work(t).executionBlocked() {
		t.Fatal("verified review did not hand off")
	}
	if got := f.claim(t, dev, claimB, 200); got["first"] != true {
		t.Fatal(got)
	}
	// Push observation likewise cannot bypass the developer's handoff.
	f.pull.Head.SHA = strings.Repeat("b", 40)
	f.observe(t)
	next := f.work(t).Task
	if next.Role != "reviewer" {
		t.Fatal(next)
	}
	f.claim(t, next, claimA, 409)
	submitted := HandoffSubmission{RequestID: "developer-B", Handoff: true, NewSHA: f.pull.Head.SHA}
	f.submit(t, dev, claimB, submitted, 200)
	f.restart(t)
	f.verify(t)
	if f.work(t).Task.ID != next.ID {
		t.Fatal("restart duplicated the successor")
	}
	if got := f.submit(t, dev, claimB, submitted, 200); got["first"] != false {
		t.Fatal("lost response replay was not idempotent")
	}
	f.claim(t, next, claimA, 200)
	f.reviews = append(f.reviews, reviewFor(8, f.pull.Head.SHA, "reviewer", "APPROVED"))
	f.observe(t)
	if f.work(t).Task != nil || !f.work(t).handoffMergeBlocked() {
		t.Fatal("approval bypassed reviewer handoff")
	}
	f.submit(t, next, claimA, HandoffSubmission{RequestID: "review-B", Handoff: true, ReviewID: 8, Outcome: "APPROVED"}, 200)
	f.verify(t)
	w := f.work(t)
	if w.Phase != "approved" || w.handoffMergeBlocked() || w.executionBlocked() {
		t.Fatalf("approval handoff failed: %+v", w)
	}
	if len(w.History) != 3 {
		t.Fatalf("unexpected task journal: %d", len(w.History))
	}
}

func TestCooperativeClaimIsAtomicAndNeverExpiresIntoDuplicateWork(t *testing.T) {
	f := newHandoffFixture(t)
	task := f.work(t).Task
	const n = 12
	var wg sync.WaitGroup
	codes := make(chan int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			b, _ := json.Marshal(handoffInput{Token: strings.Repeat(fmt.Sprintf("%02d", i), 32), Route: task.Handoff.Assignee.Route})
			out := httptest.NewRecorder()
			f.s.Handler().ServeHTTP(out, httptest.NewRequest("POST", "/v1/tasks/"+task.ID+"/claim", bytes.NewReader(b)))
			codes <- out.Code
		}(i)
	}
	wg.Wait()
	close(codes)
	won := 0
	for code := range codes {
		if code == 200 {
			won++
		} else if code != 409 {
			t.Fatalf("unexpected status %d", code)
		}
	}
	if won != 1 {
		t.Fatalf("owners: %d", won)
	}
	st, _ := f.s.store.load()
	w := st.Work[key(f.repo.Repo, 42)]
	w.Task.Handoff.ClaimedAt = time.Now().Add(-24 * time.Hour)
	if err := f.s.store.save(st); err != nil {
		t.Fatal(err)
	}
	f.restart(t)
	if err := f.s.reconcileExecutions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !f.work(t).executionBlocked() || f.work(t).Task.Handoff.State != "claimed" || len(f.work(t).Task.Runs) != 0 {
		t.Fatal("claim expired or was migrated to an executor attempt")
	}
}

func TestCooperativeSubmissionWaitsForEvidenceAndRejectsConflicts(t *testing.T) {
	f := newHandoffFixture(t)
	task := f.work(t).Task
	f.claim(t, task, claimA, 200)
	bad := task.Handoff.Assignee
	bad.Route = &execution.Route{Provider: "test", Target: "other", Session: "review-session"}
	f.call(t, "claim", task.ID, handoffInput{Token: claimA, Route: bad.Route}, 403)
	result := HandoffSubmission{RequestID: "r1", Handoff: true, ReviewID: 9, Outcome: "CHANGES_REQUESTED"}
	f.submit(t, task, claimB, result, 403)
	f.submit(t, task, claimA, result, 200)
	f.githubError = true
	f.verify(t)
	f.githubError = false
	if f.work(t).Task.Handoff.State != "submitted" {
		t.Fatal("GitHub error released claim")
	}
	f.reviews = []Review{reviewFor(9, f.pull.Head.SHA, "wrong-reviewer", "CHANGES_REQUESTED")}
	f.verify(t)
	if !f.work(t).executionBlocked() {
		t.Fatal("wrong identity passed verification")
	}
	conflict := result
	conflict.ReviewID = 10
	f.submit(t, task, claimA, conflict, 409)
	f.reviews = []Review{reviewFor(9, f.pull.Head.SHA, "reviewer", "CHANGES_REQUESTED")}
	f.verify(t)
	if f.work(t).Task.Role != "developer" || f.work(t).executionBlocked() {
		t.Fatal("valid evidence did not progress")
	}
	f.submit(t, task, claimA, result, 200)
}

func TestCooperativeVerificationFencesObservationRacesAndStaleResults(t *testing.T) {
	f := newHandoffFixture(t)
	task := f.work(t).Task
	f.claim(t, task, claimA, 200)
	result := HandoffSubmission{RequestID: "r1", Handoff: true, ReviewID: 9, Outcome: "CHANGES_REQUESTED"}
	f.submit(t, task, claimA, result, 200)
	f.reviews = []Review{reviewFor(9, f.pull.Head.SHA, "reviewer", "CHANGES_REQUESTED")}
	f.beforeRead = func() { f.observe(t) }
	f.verify(t)
	w := f.work(t)
	_, old := findTask(state{Work: map[string]*Work{"w": w}}, task.ID)
	if old.Handoff.State != "submitted" {
		t.Fatal("stale network observation committed across version change")
	}
	f.pull.Head.SHA = strings.Repeat("c", 40)
	f.observe(t)
	newTask := f.work(t).Task
	f.verify(t)
	w = f.work(t)
	if w.Task.ID != newTask.ID || w.executionBlocked() {
		t.Fatal("old handoff affected a new task")
	}
	_, old = findTask(state{Work: map[string]*Work{"w": w}}, task.ID)
	if old.Handoff.State != "superseded" {
		t.Fatal(old.Handoff)
	}
	f.submit(t, task, claimA, result, 200)
	if f.work(t).Task.ID != newTask.ID {
		t.Fatal("late duplicate replaced current task")
	}
}

func TestCooperativeWakesRetryWithoutExecutingOrAllocatingAgain(t *testing.T) {
	f := newHandoffFixture(t)
	st, _ := f.s.store.load()
	w := st.Work[key(f.repo.Repo, 42)]
	w.Reviewer.Route.Session = ""
	w.Task.Handoff.Assignee.Route.Session = ""
	if err := f.s.store.save(st); err != nil {
		t.Fatal(err)
	}
	if err := f.s.dispatch(context.Background(), f.repo, 42); err != nil {
		t.Fatal(err)
	}
	w = f.work(t)
	if f.executor.prepared != 1 || len(f.executor.wakes) != 1 || len(f.executor.deliveries) != 0 || len(w.Task.Runs) != 0 {
		t.Fatal("wake became a code delivery")
	}
	retryAt := w.Task.Handoff.Wake.RetryAt
	if delay := time.Until(retryAt); delay <= 0 || delay > time.Minute {
		t.Fatalf("first wake retry delay: %s", delay)
	}
	f.restart(t)
	w = f.work(t)
	if w.Task.Handoff.Wake.Attempts != 1 || !w.Task.Handoff.Wake.RetryAt.Equal(retryAt) {
		t.Fatal("restart reset wake retry schedule")
	}
	if err := f.s.dispatch(context.Background(), f.repo, 42); err != nil {
		t.Fatal(err)
	}
	if len(f.executor.wakes) != 1 {
		t.Fatal("wake retried before its persisted deadline")
	}
	st, _ = f.s.store.load()
	st.Work[key(f.repo.Repo, 42)].Task.Handoff.Wake.RetryAt = time.Time{}
	if err := f.s.store.save(st); err != nil {
		t.Fatal(err)
	}
	if err := f.s.dispatch(context.Background(), f.repo, 42); err != nil {
		t.Fatal(err)
	}
	if f.executor.prepared != 1 || len(f.executor.wakes) != 2 {
		t.Fatal("wake recovery allocated twice or did not retry")
	}
	task := f.work(t).Task
	f.claim(t, task, claimA, 200)
	if err := f.s.dispatch(context.Background(), f.repo, 42); err != nil {
		t.Fatal(err)
	}
	if len(f.executor.wakes) != 2 {
		t.Fatal("claimed task was woken again")
	}
}

func TestCooperativeAmbiguousPreparationIsNotRepeated(t *testing.T) {
	f := newHandoffFixture(t)
	f.executor.prepareError = true
	st, _ := f.s.store.load()
	w := st.Work[key(f.repo.Repo, 42)]
	w.Reviewer.Route.Session = ""
	w.Task.Handoff.Assignee.Route.Session = ""
	_ = f.s.store.save(st)
	_ = f.s.dispatch(context.Background(), f.repo, 42)
	f.restart(t)
	st, _ = f.s.store.load()
	st.Work[key(f.repo.Repo, 42)].Task.Handoff.Wake.RetryAt = time.Time{}
	_ = f.s.store.save(st)
	_ = f.s.dispatch(context.Background(), f.repo, 42)
	if f.executor.prepared != 1 || len(f.executor.wakes) != 0 {
		t.Fatal("ambiguous allocation was repeated")
	}
}

func TestCooperativeBlockedHandoffRequiresExplicitRetry(t *testing.T) {
	f := newHandoffFixture(t)
	task := f.work(t).Task
	f.claim(t, task, claimA, 200)
	f.submit(t, task, claimA, HandoffSubmission{RequestID: "blocked", Handoff: true, Outcome: "blocked", Reason: "missing acceptance criteria"}, 200)
	if !f.work(t).executionBlocked() {
		t.Fatal("blocked task silently released scheduling")
	}
	f.call(t, "retry", task.ID, handoffInput{Token: claimA}, 200)
	w := f.work(t)
	if w.Task.ID == task.ID || w.Task.Handoff.State != "pending" || w.executionBlocked() {
		t.Fatal("explicit retry did not create a fresh task")
	}
	f.claim(t, w.Task, claimB, 200)
}

func assignMigratedReviewer(t *testing.T, f *handoffFixture) {
	t.Helper()
	body := strings.NewReader(`{"role":"reviewer","github_login":"reviewer","route":{"provider":"test","target":"new-project","session":"new-session"}}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/work/acme/project/42/assign", body)
	response := httptest.NewRecorder()
	f.s.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("assign during migration: %d %s", response.Code, response.Body)
	}
}

func TestConfigOnlyReplacesFrozenPendingRouteBeforePollingResumes(t *testing.T) {
	f := newHandoffFixture(t)
	if err := f.s.dispatch(context.Background(), f.repo, 42); err != nil {
		t.Fatal(err)
	}
	old := f.work(t).Task
	if old.ConfigSnapshot == nil || len(f.executor.wakes) != 1 {
		t.Fatal("expected a frozen pending task with one wake")
	}
	f.s.config.ConfigOnly = true
	f.restart(t)
	assignMigratedReviewer(t, f)
	work := f.work(t)
	if work.Task.ID == old.ID || work.ReviewerOverride.Route.Target != "new-project" ||
		len(work.History) != 1 || work.History[0].Handoff.State != "superseded" ||
		work.History[0].ConfigSnapshot.Reviewer.Route.Target != old.ConfigSnapshot.Reviewer.Route.Target {
		t.Fatal("reassignment did not replace the task and retain its old snapshot")
	}
	f.claim(t, work.Task, claimA, http.StatusServiceUnavailable)
	if f.s.pollReady() || len(f.executor.wakes) != 1 {
		t.Fatal("migration triggered polling or a wake")
	}
	f.s.config.ConfigOnly = false
	f.restart(t)
	f.claim(t, old, claimA, http.StatusConflict)
	if err := f.s.dispatch(context.Background(), f.repo, 42); err != nil {
		t.Fatal(err)
	}
	pending := f.work(t).Task
	if len(f.executor.wakes) != 2 || pending.ConfigSnapshot.Reviewer.Route.Target != "new-project" ||
		f.executor.wakes[1].route.Target != "new-project" || f.executor.wakes[1].route.Session != "new-session" {
		t.Fatal("resumed task did not dispatch with the replacement route")
	}
}

func TestConfigOnlyReassignmentPreservesClaimUntilAuthorizedHandoffAndRetry(t *testing.T) {
	f := newHandoffFixture(t)
	old := f.work(t).Task
	f.claim(t, old, claimA, http.StatusOK)
	f.s.config.ConfigOnly = true
	assignMigratedReviewer(t, f)
	work := f.work(t)
	if !work.executionBlocked() || work.History[0].Handoff.State != "claimed" {
		t.Fatal("reassignment released the existing owner")
	}
	f.call(t, "retry", old.ID, handoffInput{Token: claimA}, http.StatusConflict)
	result := HandoffSubmission{RequestID: "migration-stopped", Handoff: true, Outcome: "blocked", Reason: "old runner stopped"}
	f.submit(t, old, claimB, result, http.StatusForbidden)
	f.submit(t, old, claimA, result, http.StatusOK)
	f.call(t, "retry", old.ID, handoffInput{Token: claimB}, http.StatusForbidden)
	f.call(t, "retry", old.ID, handoffInput{Token: claimA}, http.StatusOK)
	work = f.work(t)
	if work.executionBlocked() || work.Task.ID == old.ID ||
		work.Task.Handoff.Assignee.Route.Target != "new-project" || len(f.executor.wakes) != 0 {
		t.Fatal("expected an undispatched replacement after the original owner's handoff")
	}
}

func TestCooperativeNeverClearsLegacyUnknown(t *testing.T) {
	f := newHandoffFixture(t)
	st, _ := f.s.store.load()
	w := st.Work[key(f.repo.Repo, 42)]
	w.History = append(w.History, &Task{ID: "legacy", Runs: []Attempt{{ID: "old", State: "unknown"}}})
	_ = f.s.store.save(st)
	f.claim(t, f.work(t).Task, claimA, 409)
	f.restart(t)
	w = f.work(t)
	if w.History[0].Runs[0].State != "unknown" || !w.executionBlocked() {
		t.Fatal("legacy unknown was cleared")
	}
}

func TestCooperativeCloseAndDraftPreserveOwnershipUntilHandoff(t *testing.T) {
	for _, closed := range []bool{false, true} {
		t.Run(fmt.Sprint(closed), func(t *testing.T) {
			f := newHandoffFixture(t)
			task := f.work(t).Task
			f.claim(t, task, claimA, 200)
			if closed {
				f.pull.State = "closed"
			} else {
				f.pull.Draft = true
			}
			f.observe(t)
			if !f.work(t).executionBlocked() {
				t.Fatal("PR transition released owner")
			}
			f.submit(t, task, claimA, HandoffSubmission{RequestID: "retired", Handoff: true, ReviewID: 7, Outcome: "APPROVED"}, 200)
			f.verify(t)
			if f.work(t).executionBlocked() || f.work(t).Task != nil {
				t.Fatal("retired handoff restarted work")
			}
		})
	}
}

func TestCooperativeClaimReplayAndSubmissionWithoutHandoff(t *testing.T) {
	f := newHandoffFixture(t)
	task := f.work(t).Task
	f.claim(t, task, claimA, 200)
	if r := f.claim(t, task, claimA, 200); r["first"] != false {
		t.Fatal("repeated wake authorized execution again")
	}
	f.submit(t, task, claimA, HandoffSubmission{RequestID: "oops", ReviewID: 7, Outcome: "APPROVED"}, 400)
	if f.work(t).Task.Handoff.State != "claimed" {
		t.Fatal("invalid submission mutated owner")
	}
}

func TestCooperativeDeveloperWaitsForNewSHAAndNoCodeRetry(t *testing.T) {
	f := newHandoffFixture(t)
	// Human-originated Request changes creates a cooperative developer task.
	f.reviews = []Review{reviewFor(7, f.pull.Head.SHA, "reviewer", "CHANGES_REQUESTED")}
	f.observe(t)
	task := f.work(t).Task
	f.claim(t, task, claimB, 200)
	result := HandoffSubmission{RequestID: "pushed", Handoff: true, NewSHA: strings.Repeat("b", 40)}
	f.submit(t, task, claimB, result, 200)
	f.verify(t)
	if w := f.work(t); w.Task.ID != task.ID || w.Task.Handoff.State != "submitted" {
		t.Fatal("old head triggered a new code attempt")
	}
	f.restart(t)
	f.verify(t)
	if len(f.executor.deliveries) != 0 {
		t.Fatal("verification failure executed code")
	}
	f.pull.Head.SHA = result.NewSHA
	f.verify(t)
	if w := f.work(t); w.Task.Role != "reviewer" || w.executionBlocked() {
		t.Fatal("new head verification failed")
	}
}

func TestCooperativeAutoMergeDoesNotRunBeforeVerifiedHandoff(t *testing.T) {
	f := newHandoffFixture(t)
	task := f.work(t).Task
	f.claim(t, task, claimA, 200)
	f.repo.AutoMerge = &AutoMergeConfig{Enabled: true}
	setRepositoryConfigForTest(t, f.s, f.repo)
	f.reviews = []Review{reviewFor(7, f.pull.Head.SHA, "reviewer", "APPROVED")}
	f.observe(t)
	called := false
	f.beforeRead = func() { called = true }
	if err := f.s.autoMerge(context.Background(), f.repo, 42); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("merge preflight started before reviewer handoff")
	}
	f.beforeRead = nil
}

func TestCooperativeSchemaUpgradeProtectsClaimsFromOlderBinaries(t *testing.T) {
	f := newHandoffFixture(t)
	task := f.work(t).Task
	f.claim(t, task, claimA, 200)
	if _, err := f.s.store.db.Exec("DROP TABLE workspace_allocations; DROP TABLE runtime_config; PRAGMA user_version=2"); err != nil {
		t.Fatal(err)
	}
	f.restart(t)
	var version int
	if err := f.s.store.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != schemaVersion {
		t.Fatal(version, err)
	}
	if f.work(t).Task.Handoff.ClaimHash != claimHash(claimA) {
		t.Fatal("migration discarded claim")
	}
}

func TestResolvedCooperativeHistoryDoesNotGateLaterLegacyApproval(t *testing.T) {
	for _, sameSHA := range []bool{false, true} {
		t.Run(fmt.Sprintf("sameSHA=%v", sameSHA), func(t *testing.T) {
			f := newHandoffFixture(t)
			task := f.work(t).Task
			f.claim(t, task, claimA, 200)
			f.reviews = []Review{reviewFor(7, f.pull.Head.SHA, "reviewer", "APPROVED")}
			f.observe(t)
			f.submit(t, task, claimA, HandoffSubmission{RequestID: "approve-A", Handoff: true, ReviewID: 7, Outcome: "APPROVED"}, 200)
			f.verify(t)
			f.repo.TaskMode = "legacy"
			setRepositoryConfigForTest(t, f.s, f.repo)
			if !sameSHA {
				f.pull.Head.SHA = strings.Repeat("b", 40)
			}
			f.observe(t)
			f.reviews = append(f.reviews, reviewFor(8, f.pull.Head.SHA, "reviewer", "APPROVED"))
			f.observe(t)
			w := f.work(t)
			if w.executionBlocked() {
				t.Fatal("fixture contains unresolved ownership")
			}
			if w.handoffMergeBlocked() {
				t.Fatal("completed cooperative history permanently gates a later legacy approval")
			}
		})
	}
}

func TestSwitchToLegacyRetainsUnresolvedCooperativeOwnership(t *testing.T) {
	f := newHandoffFixture(t)
	task := f.work(t).Task
	f.claim(t, task, claimA, 200)
	f.repo.TaskMode = "legacy"
	setRepositoryConfigForTest(t, f.s, f.repo)
	f.pull.Head.SHA = strings.Repeat("b", 40)
	f.reviews = []Review{reviewFor(8, f.pull.Head.SHA, "reviewer", "APPROVED")}
	f.observe(t)
	if !f.work(t).handoffMergeBlocked() {
		t.Fatal("legacy switch bypassed an unresolved cooperative owner")
	}
}

func TestReassignmentPreservesCooperativeDeliveryIdentity(t *testing.T) {
	for _, role := range []string{"reviewer", "developer"} {
		t.Run(role, func(t *testing.T) {
			f := newHandoffFixture(t)
			if role == "developer" {
				f.reviews = []Review{reviewFor(7, f.pull.Head.SHA, "reviewer", "CHANGES_REQUESTED")}
				f.observe(t)
			}
			task := f.work(t).Task
			st, _ := f.s.store.load()
			w := st.Work[key(f.repo.Repo, 42)]
			w.Task.Handoff.Assignee.GitHubCommand = "/original-command"
			if role == "reviewer" {
				w.Reviewer.GitHubCommand = "/original-command"
			} else {
				w.Developer.GitHubCommand = "/original-command"
			}
			if err := f.s.store.save(st); err != nil {
				t.Fatal(err)
			}
			task = f.work(t).Task
			f.claim(t, task, claimA, 200)
			st, _ = f.s.store.load()
			w = st.Work[key(f.repo.Repo, 42)]
			if err := applyChange(w, "assign", workChange{Role: role, GitHubLogin: "replacement", GitHubCommand: "/replacement-command", Route: &execution.Route{Provider: "test", Target: "replacement", Session: "replacement-session"}}); err != nil {
				t.Fatal(err)
			}
			if err := f.s.store.save(st); err != nil {
				t.Fatal(err)
			}
			f.restart(t)
			replay := f.claim(t, task, claimA, 200)
			d := replay["delivery"].(map[string]any)
			if d["github_login"] != task.Handoff.Assignee.GitHubLogin || d["github_command"] != "/original-command" {
				t.Fatalf("replayed claim changed identity: %v", d)
			}
			out := httptest.NewRecorder()
			f.s.Handler().ServeHTTP(out, httptest.NewRequest("GET", "/v1/tasks/"+task.ID, nil))
			if out.Code != 200 {
				t.Fatal(out.Code)
			}
			var status struct{ Delivery execution.Delivery }
			if err := json.Unmarshal(out.Body.Bytes(), &status); err != nil {
				t.Fatal(err)
			}
			if status.Delivery.GitHubLogin != task.Handoff.Assignee.GitHubLogin || status.Delivery.GitHubCommand != "/original-command" {
				t.Fatal("status changed original identity", status)
			}
			if role == "reviewer" {
				replacementTask := f.work(t).Task.ID
				f.reviews = []Review{reviewFor(9, task.SHA, task.Handoff.Assignee.GitHubLogin, "APPROVED")}
				f.submit(t, task, claimA, HandoffSubmission{RequestID: "original-review", Handoff: true, ReviewID: 9, Outcome: "APPROVED"}, 200)
				f.verify(t)
				w := f.work(t)
				_, old := findTask(state{Work: map[string]*Work{"w": w}}, task.ID)
				if old.Handoff.State != "completed" || w.Task.ID != replacementTask || w.executionBlocked() {
					t.Fatal("original owner could not finish without changing the replacement task")
				}
			}
		})
	}
}
