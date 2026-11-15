package ink

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ink-commons/internal/execution"
)

func TestReviewIdentityMismatchIsVisibleAndDoesNotAdvancePhase(t *testing.T) {
	s, e, repo, _ := reliabilityService(t, Reliability{ResultSeconds: 1})
	dispatch(t, s, repo)
	w := loadReliability(t, s)
	d := w.Task.Runs[0].Request
	owner, _, err := e.ledger.Start(d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.ledger.Finish(d, owner, "succeeded"); err != nil {
		t.Fatal(err)
	}
	wrong := Review{ID: 7, SHA: w.Pull.Head.SHA, State: "CHANGES_REQUESTED", URL: "https://github.com/acme/project/pull/42#review-7", SubmittedAt: time.Now()}
	wrong.User.Login = "personal-user"
	if err = s.observe(repo, w.Pull, []Review{wrong}); err != nil {
		t.Fatal(err)
	}
	reconcile(t, s)
	w = loadReliability(t, s)
	if w.Phase != "review_pending" || w.Workflow.State != "needs_attention" || len(w.Alerts) != 1 || w.Alerts[0].Code != "review_identity_mismatch" || !strings.Contains(w.Alerts[0].Message, "personal-user") {
		t.Fatal("wrong reviewer silently ignored or trusted", w)
	}
	// Identity mismatch does not directly wake the developer to change code.
	if len(e.deliveries) != 1 {
		t.Fatal("untrusted review started development")
	}
	alertID := w.Alerts[0].ID
	if err = s.observe(repo, w.Pull, []Review{wrong}); err != nil {
		t.Fatal(err)
	}
	if got := loadReliability(t, s); len(got.Alerts) != 1 || got.Alerts[0].ID != alertID {
		t.Fatal("poll duplicated diagnostic")
	}
	correct := wrong
	correct.ID = 8
	correct.User.Login = repo.ReviewerLogin
	if err = s.observe(repo, w.Pull, []Review{wrong, correct}); err != nil {
		t.Fatal(err)
	}
	w = loadReliability(t, s)
	if w.Phase != "changes_requested" || w.Alerts[0].Active {
		t.Fatal("valid review failed to progress", w)
	}
	dispatch(t, s, repo) // No developer route in this fixture: manual developer work.
}
func TestMissingReviewSchedulesBoundedFollowupAfterConfirmedEnd(t *testing.T) {
	s, e, repo, path := reliabilityService(t, Reliability{ResultSeconds: 1, BackoffSeconds: 1, MaxAttempts: 2})
	dispatch(t, s, repo)
	w := loadReliability(t, s)
	d := w.Task.Runs[0].Request
	owner, _, err := e.ledger.Start(d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.ledger.Finish(d, owner, "succeeded"); err != nil {
		t.Fatal(err)
	}
	reconcile(t, s)
	st, _ := s.store.load()
	t1 := st.Work["acme/project#42"].Task
	t1.Runs[0].EndedAt = time.Now().Add(-time.Minute)
	if err = s.store.save(st); err != nil {
		t.Fatal(err)
	}
	reconcile(t, s)
	w = loadReliability(t, s)
	if w.Phase != "review_pending" || w.Task.RetryAt.IsZero() || w.Alerts[0].Code != "workflow_result_missing" {
		t.Fatal("agent completion was treated as workflow completion", w)
	}
	// Persisted waiting/follow-up state survives a new service, not the agent turn.
	s.Close()
	s, err = NewService(s.config, path, s.providers)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if loadReliability(t, s).Task.RetryAt.IsZero() {
		t.Fatal("restart lost follow-up")
	}
	st, _ = s.store.load()
	st.Work["acme/project#42"].Task.RetryAt = time.Now().Add(-time.Second)
	s.store.save(st)
	reconcile(t, s)
	dispatch(t, s, repo)
	w = loadReliability(t, s)
	if len(e.deliveries) != 2 || w.Task.ID != d.TaskID || w.Task.Runs[1].ID == d.AttemptID || !strings.Contains(e.deliveries[1].task.Feedback, "正式审查") {
		t.Fatal("missing outcome did not wake reviewer with feedback", w)
	}
	second := w.Task.Runs[1].Request
	owner, _, err = e.ledger.Start(second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.ledger.Finish(second, owner, "succeeded"); err != nil {
		t.Fatal(err)
	}
	reconcile(t, s)
	st, _ = s.store.load()
	st.Work["acme/project#42"].Task.Runs[1].EndedAt = time.Now().Add(-time.Minute)
	s.store.save(st)
	reconcile(t, s)
	dispatch(t, s, repo)
	if w = loadReliability(t, s); len(e.deliveries) != 2 || !strings.Contains(w.Task.Error, "attempts exhausted") {
		t.Fatal("follow-up loop not bounded", w)
	}
}
func TestQueuedHeartbeatUsesQueueDeadline(t *testing.T) {
	now := time.Now().UTC()
	c := Reliability{ACKSeconds: 1, QueueSeconds: 120, HeartbeatSeconds: 30}
	a := Attempt{CreatedAt: now.Add(-time.Minute), Receipt: execution.Receipt{State: "queued", HeartbeatAt: now}}
	if c.expired(a, now) {
		t.Fatal("healthy busy queue expired as missing start ACK")
	}
	a.Receipt.HeartbeatAt = now.Add(-time.Minute)
	if !c.expired(a, now) {
		t.Fatal("dead queue heartbeat ignored")
	}
	a.Receipt.HeartbeatAt = now
	a.CreatedAt = now.Add(-3 * time.Minute)
	if !c.expired(a, now) {
		t.Fatal("queue wait was unbounded")
	}
}

type notificationExecutor struct {
	reliableExecutor
	notices     []execution.Notice
	notifyError error
}

func (e *notificationExecutor) Notify(_ context.Context, _ execution.Route, n execution.Notice) error {
	e.notices = append(e.notices, n)
	return e.notifyError
}
func TestWorkflowAlertOutboxRetriesAndDeduplicates(t *testing.T) {
	s, e, _, _ := reliabilityService(t, Reliability{})
	n := &notificationExecutor{reliableExecutor: *e, notifyError: errors.New("offline")}
	s.providers["test"] = n
	st, _ := s.store.load()
	w := st.Work["acme/project#42"]
	w.Origin = &execution.Route{Provider: "test", Target: "developer", Session: "thread"}
	w.raiseAlert("test", "event", "diagnostic", time.Now())
	s.store.save(st)
	s.deliverAlerts(context.Background())
	w = loadReliability(t, s)
	if w.Alerts[0].NotificationAccepted || w.Alerts[0].Error == "" {
		t.Fatal("failed alert was lost")
	}
	n.notifyError = nil
	s.deliverAlerts(context.Background())
	s.deliverAlerts(context.Background())
	w = loadReliability(t, s)
	if !w.Alerts[0].NotificationAccepted || len(n.notices) != 2 || n.notices[0].ID != n.notices[1].ID {
		t.Fatal("notification outbox did not retain stable dedup ID", w.Alerts)
	}
}

type ambiguousPreparation struct {
	reliableExecutor
	prepares int
}

func (e *ambiguousPreparation) Prepare(context.Context, execution.Route, execution.Delivery) (string, error) {
	e.prepares++
	return "", errors.New("response lost after thread creation")
}
func TestAmbiguousThreadPreparationRequiresReconciliation(t *testing.T) {
	s, e, repo, _ := reliabilityService(t, Reliability{ACKSeconds: 1})
	a := &ambiguousPreparation{reliableExecutor: *e}
	s.providers["test"] = a
	st, _ := s.store.load()
	st.Work["acme/project#42"].Reviewer.Route.Session = ""
	s.store.save(st)
	dispatch(t, s, repo)
	w := loadReliability(t, s)
	if w.Task.Preparation != "unknown" || a.prepares != 1 {
		t.Fatal(w.Task)
	}
	// Even if the old non-running attempt has been stopped, a retry cannot create
	// a second conversation while the first allocation result is ambiguous.
	st, _ = s.store.load()
	task := st.Work["acme/project#42"].Task
	task.Runs[0].State = "stopped"
	task.Runs[0].EndedAt = time.Now()
	task.Delivery = "pending"
	s.store.save(st)
	dispatch(t, s, repo)
	if a.prepares != 1 {
		t.Fatal("ambiguous allocation repeated")
	}
	body := `{"task_id":"` + w.Task.ID + `","route":{"provider":"test","target":"reviewer","session":"existing-thread"},"reason":"found the created thread"}`
	out := httptest.NewRecorder()
	s.Handler().ServeHTTP(out, httptest.NewRequest("POST", "/v1/work/acme/project/42/confirm-prepared", strings.NewReader(body)))
	if out.Code != 200 {
		t.Fatal(out.Code, out.Body)
	}
	dispatch(t, s, repo)
	w = loadReliability(t, s)
	if a.prepares != 1 || w.Reviewer.Route.Session != "existing-thread" || w.Task.PreparationReason != "found the created thread" || w.Task.Runs[len(w.Task.Runs)-1].Route.Session != "existing-thread" {
		t.Fatal("reconciled session not reused", w.Task)
	}
}

func TestWrongSHAReviewDiagnosticOnlyForNewlySubmittedStaleResult(t *testing.T) {
	s, _, repo, _ := reliabilityService(t, Reliability{})
	w := loadReliability(t, s)
	review := Review{ID: 1, SHA: strings.Repeat("b", 40), State: "APPROVED", SubmittedAt: w.Task.CreatedAt.Add(-time.Minute)}
	review.User.Login = repo.ReviewerLogin
	if err := s.observe(repo, w.Pull, []Review{review}); err != nil {
		t.Fatal(err)
	}
	if len(loadReliability(t, s).Alerts) != 0 {
		t.Fatal("normal previous-SHA review raised an alert")
	}
	review.SubmittedAt = time.Now().Add(time.Second)
	if err := s.observe(repo, w.Pull, []Review{review}); err != nil {
		t.Fatal(err)
	}
	w = loadReliability(t, s)
	if len(w.Alerts) != 1 || w.Alerts[0].Code != "review_sha_mismatch" {
		t.Fatal(w.Alerts)
	}
}

// Guard the source identity contract as well as the transport lifecycle.
func TestDeliveryCarriesConfiguredGitHubIdentity(t *testing.T) {
	s, e, repo, _ := reliabilityService(t, Reliability{})
	st, _ := s.store.load()
	st.Work["acme/project#42"].Reviewer.GitHubCommand = "reviewer-app"
	s.store.save(st)
	dispatch(t, s, repo)
	d := e.deliveries[0].task
	if d.GitHubLogin != "reviewer" || d.GitHubCommand != "reviewer-app" {
		t.Fatal(d)
	}
	raw, err := json.Marshal(d)
	if err != nil || !strings.Contains(string(raw), "github_login") {
		t.Fatal(string(raw), err)
	}
}
