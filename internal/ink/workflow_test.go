package ink

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
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
func TestMissingReviewRemainsWaitingAcrossRestartWithoutRedispatch(t *testing.T) {
	s, e, repo, path := reliabilityService(t, Reliability{ResultSeconds: 1})
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
	st, err := s.store.load()
	if err != nil {
		t.Fatal(err)
	}
	st.Work["acme/project#42"].Task.Runs[0].EndedAt = time.Now().Add(-time.Minute)
	if err = s.store.save(st); err != nil {
		t.Fatal(err)
	}
	reconcile(t, s)
	w = loadReliability(t, s)
	if w.Phase != "review_pending" || !w.Task.RetryAt.IsZero() || len(w.Alerts) != 1 || w.Alerts[0].Code != "workflow_result_missing" {
		t.Fatal("missing result was lost or scheduled new work", w)
	}
	alertID := w.Alerts[0].ID
	taskID := w.Task.ID
	s.Close()
	s, err = NewService(s.config, path, s.providers)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := 0; i < 3; i++ {
		reconcile(t, s)
		dispatch(t, s, repo)
	}
	w = loadReliability(t, s)
	if len(e.deliveries) != 1 || w.Task.ID != taskID || !w.Task.RetryAt.IsZero() || len(w.Alerts) != 1 || w.Alerts[0].ID != alertID || !w.Alerts[0].Active {
		t.Fatal("restart replayed successful work or lost waiting diagnosis", w)
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
