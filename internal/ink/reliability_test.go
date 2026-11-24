//go:build linux || darwin

package ink

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wujinhjun/ink-commons/internal/execution"
	"github.com/wujinhjun/ink-commons/internal/execution/ledger"
)

type reliableExecutor struct {
	testExecutor
	ledger    *ledger.Ledger
	lostACK   bool
	stopError error
}

func (e *reliableExecutor) Deliver(_ context.Context, _ execution.Route, d execution.Delivery) error {
	e.deliveries = append(e.deliveries, recordedDelivery{task: d})
	_, err := e.ledger.Accept(d)
	if err == nil && e.lostACK {
		return errors.New("connection lost after accepted")
	}
	return err
}
func (e *reliableExecutor) Status(_ context.Context, _ execution.Route, d execution.Delivery) (execution.Receipt, error) {
	return e.ledger.Status(d)
}
func (e *reliableExecutor) Stop(ctx context.Context, _ execution.Route, d execution.Delivery) (execution.Receipt, error) {
	return e.ledger.Stop(ctx, d, func(context.Context, string) error { return e.stopError })
}
func reliabilityService(t *testing.T, c Reliability) (*Service, *reliableExecutor, Repository, string) {
	t.Helper()
	dir := t.TempDir()
	l, err := ledger.Open(filepath.Join(dir, "executor.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	e := &reliableExecutor{ledger: l}
	repo := Repository{Repo: "acme/project", ReviewerLogin: "reviewer", ReviewerRoute: &execution.Route{Provider: "test", Target: "reviewer", Session: "session"}}
	path := filepath.Join(dir, stateDatabase)
	s, err := newFixtureService(t, Config{Repositories: []Repository{repo}, Reliability: c}, path, map[string]execution.Executor{"test": e})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	w := &Work{Repo: repo.Repo, Linked: true, Reviewer: Assignee{GitHubLogin: "reviewer", Route: repo.ReviewerRoute}}
	w.Pull.Number, w.Pull.State, w.Pull.Head.SHA = 42, "open", strings.Repeat("a", 40)
	w.derive()
	if err = s.store.saveWork(w); err != nil {
		t.Fatal(err)
	}
	return s, e, repo, path
}
func loadReliability(t *testing.T, s *Service) *Work {
	t.Helper()
	st, err := loadFixture(s.store)
	if err != nil {
		t.Fatal(err)
	}
	return st.Work["acme/project#42"]
}
func reconcile(t *testing.T, s *Service) {
	t.Helper()
	if err := s.reconcileExecutions(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func dispatch(t *testing.T, s *Service, repo Repository) {
	t.Helper()
	if err := s.dispatch(context.Background(), repo, 42); err != nil {
		t.Fatal(err)
	}
}

func TestExecutionLostACKRestartAndSafeRetry(t *testing.T) {
	s, e, repo, path := reliabilityService(t, Reliability{})
	e.lostACK = true
	dispatch(t, s, repo)
	w := loadReliability(t, s)
	first := w.Task.Runs[0]
	taskID := w.Task.ID
	if w.Task.Delivery != "unknown" {
		t.Fatal("lost ACK must remain uncertain")
	}
	if _, _, err := e.ledger.Start(first.Request); err != nil {
		t.Fatal(err)
	}
	s.Close()
	var err error
	s, err = NewService(s.config, path, s.providers)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	reconcile(t, s)
	w = loadReliability(t, s)
	if w.Task.Runs[0].State != "running" || w.Task.Runs[0].Receipt.StartedAt.IsZero() {
		t.Fatal("ACK not recovered", w.Task)
	}
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, httptest.NewRequest("POST", "/v1/work/acme/project/42/retry", nil))
	if response.Code != 409 {
		t.Fatal("running attempt was retryable", response.Body)
	}
	dispatch(t, s, repo)
	if len(e.deliveries) != 1 || w.Task.ID != taskID {
		t.Fatal("restart replayed delivery or changed task")
	}
}
func TestACKTimeoutFencesDelayedDeliveryAndPersistsBackoff(t *testing.T) {
	s, e, repo, path := reliabilityService(t, Reliability{ACKSeconds: 1, BackoffSeconds: 1})
	dispatch(t, s, repo)
	st, _ := loadFixture(s.store)
	w := st.Work["acme/project#42"]
	first := w.Task.Runs[0]
	w.Task.Runs[0].CreatedAt = time.Now().Add(-time.Minute)
	saveFixture(s.store, st)
	reconcile(t, s)
	w = loadReliability(t, s)
	if w.Task.Runs[0].State != "stopped" || w.Task.RetryAt.IsZero() || w.Task.Runs[0].RecoveryReason != "execution timeout" {
		t.Fatalf("no safe recovery: %+v", w.Task)
	}
	if _, _, err := e.ledger.Start(first.Request); !errors.Is(err, ledger.ErrConflict) {
		t.Fatal("delayed request started", err)
	}
	s.Close()
	var err error
	s, err = NewService(s.config, path, s.providers)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if loadReliability(t, s).Task.RetryAt.IsZero() {
		t.Fatal("restart lost backoff")
	}
	dispatch(t, s, repo)
	if len(e.deliveries) != 1 {
		t.Fatal("backoff ignored")
	}
	st, _ = loadFixture(s.store)
	st.Work["acme/project#42"].Task.RetryAt = time.Now().Add(-time.Second)
	saveFixture(s.store, st)
	reconcile(t, s)
	dispatch(t, s, repo)
	w = loadReliability(t, s)
	if len(w.Task.Runs) != 2 || w.Task.ID != first.Request.TaskID || w.Task.Runs[1].ID == first.ID || w.Task.Runs[1].Number != 2 {
		t.Fatal("invalid retry identity", w.Task)
	}
}
func TestUnknownOldExecutionBlocksNewSHAUntilManualStop(t *testing.T) {
	s, e, repo, _ := reliabilityService(t, Reliability{})
	dispatch(t, s, repo)
	old := loadReliability(t, s).Task
	if _, _, err := e.ledger.Start(old.Runs[0].Request); err != nil {
		t.Fatal(err)
	}
	pull := loadReliability(t, s).Pull
	pull.Head.SHA = strings.Repeat("b", 40)
	if err := s.observe(repo, pull, nil); err != nil {
		t.Fatal(err)
	}
	e.stopError = ledger.ErrUnknown
	reconcile(t, s)
	dispatch(t, s, repo)
	w := loadReliability(t, s)
	if len(e.deliveries) != 1 || len(w.History) != 1 || w.History[0].Runs[0].State != "unknown" {
		t.Fatal("old execution was discarded", w)
	}
	// A local operator stops the actual runtime and fences its delayed messages.
	if _, err := e.ledger.Stop(context.Background(), old.Runs[0].Request, func(context.Context, string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	change := workChange{TaskID: old.ID, AttemptID: old.Runs[0].ID, Stopped: true, Reason: "operator joined old runtime and fenced delivery"}
	body, _ := json.Marshal(change)
	r := httptest.NewRecorder()
	s.Handler().ServeHTTP(r, httptest.NewRequest("POST", "/v1/work/acme/project/42/confirm-stopped", strings.NewReader(string(body))))
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body)
	}
	dispatch(t, s, repo)
	w = loadReliability(t, s)
	if len(e.deliveries) != 2 || w.Task.ID == old.ID || w.Task.SHA != pull.Head.SHA || w.History[0].Runs[0].ManualReason == "" {
		t.Fatal("manual resolution lost audit or identity", w)
	}
}
func TestExecutionExhaustionDoesNotAdvanceBusinessState(t *testing.T) {
	s, e, repo, _ := reliabilityService(t, Reliability{MaxAttempts: 1})
	dispatch(t, s, repo)
	d := loadReliability(t, s).Task.Runs[0].Request
	owner, _, err := e.ledger.Start(d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.ledger.Finish(d, owner, "failed"); err != nil {
		t.Fatal(err)
	}
	reconcile(t, s)
	w := loadReliability(t, s)
	if w.Phase != "review_pending" || w.Task.Error != "execution attempts exhausted; manual intervention required" || !w.Task.RetryAt.IsZero() {
		t.Fatal(w)
	}
	dispatch(t, s, repo)
	if len(e.deliveries) != 1 {
		t.Fatal("exhausted execution retried")
	}
}
func TestReceiptValidation(t *testing.T) {
	now := time.Now().UTC()
	d := execution.Delivery{TaskID: "task", AttemptID: "attempt"}
	receipt := execution.Receipt{TaskID: d.TaskID, AttemptID: d.AttemptID, Sequence: 2, State: "running", StartedAt: now, HeartbeatAt: now}
	a := Attempt{ID: d.AttemptID, Request: d, State: "running", Receipt: receipt}
	if err := validateReceipt(a, receipt, now); err != nil {
		t.Fatal("exact duplicate rejected", err)
	}
	tests := map[string]func(*execution.Receipt){
		"wrong task":            func(r *execution.Receipt) { r.TaskID = "other" },
		"old round":             func(r *execution.Receipt) { r.AttemptID = "old" },
		"stale sequence":        func(r *execution.Receipt) { r.Sequence = 1 },
		"conflicting duplicate": func(r *execution.Receipt) { r.State = "succeeded" },
		"missing ACK":           func(r *execution.Receipt) { r.Sequence = 3; r.StartedAt = time.Time{} },
		"backwards heartbeat":   func(r *execution.Receipt) { r.Sequence = 3; r.HeartbeatAt = now.Add(-time.Minute) },
		"missing end":           func(r *execution.Receipt) { r.Sequence = 3; r.State = "succeeded" },
		"future heartbeat":      func(r *execution.Receipt) { r.Sequence = 3; r.HeartbeatAt = now.Add(time.Hour) },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			bad := receipt
			mutate(&bad)
			if err := validateReceipt(a, bad, now); err == nil {
				t.Fatal("invalid receipt accepted")
			}
		})
	}
}

type delayedPreparation struct {
	testExecutor
	entered, release chan struct{}
}

func (e *delayedPreparation) Prepare(context.Context, execution.Route, execution.Delivery) (string, error) {
	close(e.entered)
	<-e.release
	return "old-session", nil
}
func TestObservationDuringPreparationCannotOverwriteNewTask(t *testing.T) {
	s, _, repo, _ := reliabilityService(t, Reliability{})
	e := &delayedPreparation{entered: make(chan struct{}), release: make(chan struct{})}
	s.providers["test"] = e
	st, _ := loadFixture(s.store)
	work := st.Work["acme/project#42"]
	work.Reviewer.Route.Session = ""
	work.ReviewerOverride = cloneAssignee(work.Reviewer)
	saveFixture(s.store, st)
	done := make(chan error, 1)
	go func() { done <- s.dispatch(context.Background(), repo, 42) }()
	<-e.entered
	pull := loadReliability(t, s).Pull
	pull.Head.SHA = strings.Repeat("b", 40)
	if err := s.observe(repo, pull, nil); err != nil {
		t.Fatal(err)
	}
	close(e.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	w := loadReliability(t, s)
	if w.Task.SHA != pull.Head.SHA || w.Task.Delivery != "pending" || len(w.History) != 1 || w.History[0].Runs[0].Route.Session != "old-session" || len(e.deliveries) != 0 {
		t.Fatal("stale preparation overwrote new task", w)
	}
}

func TestExecutionDeadlines(t *testing.T) {
	now := time.Now().UTC()
	c := Reliability{ACKSeconds: 10, HeartbeatSeconds: 20, ExecutionSeconds: 60}
	cases := []struct {
		name                  string
		age, start, heartbeat time.Duration
		expired               bool
	}{
		{"waiting for ACK", 5, 0, 0, false}, {"ACK timeout", 11, 0, 0, true},
		{"healthy", 40, 30, 2, false}, {"heartbeat timeout", 40, 30, 21, true},
		{"execution timeout despite heartbeat", 90, 70, 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := Attempt{CreatedAt: now.Add(-tc.age * time.Second)}
			if tc.start > 0 {
				a.Receipt.StartedAt = now.Add(-tc.start * time.Second)
				a.Receipt.HeartbeatAt = now.Add(-tc.heartbeat * time.Second)
			}
			if got := c.expired(a, now); got != tc.expired {
				t.Fatalf("expired=%v", got)
			}
		})
	}
}
