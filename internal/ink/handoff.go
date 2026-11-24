//go:build linux || darwin

package ink

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/wujinhjun/ink-commons/internal/execution"
)

// TaskHandoff is a cooperative business handoff, never an executor termination
// receipt. It is opt-in for newly created tasks; legacy attempts remain intact.
type TaskHandoff struct {
	BlockedNotice *BlockedNotice     `json:"blocked_notice,omitempty"`
	State         string             `json:"state"`
	Assignee      Assignee           `json:"assignee"`
	ClaimHash     string             `json:"claim_hash,omitempty"`
	ClaimedAt     time.Time          `json:"claimed_at,omitempty"`
	SubmittedAt   time.Time          `json:"submitted_at,omitempty"`
	FinishedAt    time.Time          `json:"finished_at,omitempty"`
	Submission    *HandoffSubmission `json:"submission,omitempty"`
	Verification  string             `json:"verification,omitempty"`
	Wake          TaskWake           `json:"wake"`
}

type TaskWake struct {
	Route               *execution.Route                  `json:"route,omitempty"`
	Request             *execution.Delivery               `json:"request,omitempty"`
	Preparation         string                            `json:"preparation,omitempty"`
	PreparationAttempts int                               `json:"preparation_attempts,omitempty"`
	Allocation          *execution.ConversationAllocation `json:"allocation,omitempty"`
	AllocationError     string                            `json:"allocation_error,omitempty"`
	RecoveryAttempts    int                               `json:"recovery_attempts,omitempty"`
	RecoveryStopped     bool                              `json:"recovery_stopped,omitempty"`
	Attempts            int                               `json:"attempts"`
	RetryAt             time.Time                         `json:"retry_at,omitempty"`
	AcceptedAt          *time.Time                        `json:"accepted_at,omitempty"`
	Error               string                            `json:"error,omitempty"`
}

type HandoffSubmission struct {
	RequestID string `json:"request_id"`
	Handoff   bool   `json:"handoff"`
	NewSHA    string `json:"new_sha,omitempty"`
	ReviewID  int64  `json:"review_id,omitempty"`
	Outcome   string `json:"outcome,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

type handoffInput struct {
	Token      string             `json:"token"`
	Route      *execution.Route   `json:"route,omitempty"`
	Login      string             `json:"github_login,omitempty"`
	Submission *HandoffSubmission `json:"submission,omitempty"`
}

func (w *Work) findTask(id string) *Task {
	if w != nil {
		for _, t := range w.executionTasks() {
			if t.ID == id {
				return t
			}
		}
	}
	return nil
}

func claimHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (w *Work) handoffMergeBlocked() bool {
	cooperative := w.TaskMode == "cooperative"
	approval := approvedReview(w.Reviews, w.Pull.Head.SHA, w.Reviewer.GitHubLogin)
	for _, t := range w.executionTasks() {
		h := t.Handoff
		if h == nil {
			continue
		}
		// Switching modes must not release existing cooperative ownership.
		if h.State == "claimed" || h.State == "submitted" || h.State == "blocked" {
			return true
		}
		// A completed old round does not turn all future legacy approvals into
		// cooperative approvals. Retain the handoff policy only for this review.
		if approval != nil && t.Role == "reviewer" && t.SHA == w.Pull.Head.SHA && h.Submission != nil && h.Submission.ReviewID == approval.ID {
			cooperative = true
		}
	}
	if !cooperative {
		return false
	}
	if w.executionBlocked() || approval == nil {
		return true
	}
	for _, t := range w.executionTasks() {
		h := t.Handoff
		if h != nil && h.State == "completed" && t.Role == "reviewer" && t.SHA == w.Pull.Head.SHA && h.Submission != nil && h.Submission.ReviewID == approval.ID {
			return false
		}
	}
	return true
}

func reviewChannelUnverified(w *Work, t *Task) bool {
	route := t.Handoff.Assignee.Route
	if route == nil || t.Handoff.Wake.Preparation == "prepared" {
		return false
	}
	d := deliveryFor(w, t, "", 0)
	return policyNeedsPlacement(*route, d.RequireSameChannel, d.ReuseDevelopmentThread)
}

func handoffResponse(w *Work, t *Task, first bool) map[string]any {
	next := ""
	if w.Task != nil && w.Task.ID != t.ID {
		next = w.Task.ID
	}
	return map[string]any{"next_task_id": next, "repo": w.Repo, "pull_url": w.Pull.URL, "phase": w.Phase, "task": t, "delivery": deliveryFor(w, t, "", 0), "first": first, "eligible": w.Task == t && t.Handoff.State == "pending" && !w.executionBlocked() && !reviewChannelUnverified(w, t)}
}

// The private Unix socket is the trust boundary (same as assign/confirm-stopped).
// Route/login assertions bind a claim to the intended participant; they are not
// independent authentication of mutually untrusted processes running as one UID.
func (s *Service) handoffTask(out http.ResponseWriter, r *http.Request) {
	var input handoffInput
	if r.Method == http.MethodPost {
		if err := decode(out, r, &input); err != nil {
			fail(out, err)
			return
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	w, t, err := s.store.loadTask(r.PathValue("task"))
	if err != nil {
		fail(out, err)
		return
	}
	if t == nil || t.Handoff == nil {
		fail(out, apiError{404, "cooperative task not found"})
		return
	}
	if r.Method == http.MethodGet {
		reply(out, 200, handoffResponse(w, t, false))
		return
	}
	if _, err := s.store.effectiveConfig(w.Repo); err != nil {
		fail(out, err)
		return
	}
	if len(input.Token) < 32 || len(input.Token) > 256 {
		fail(out, apiError{400, "a private random claim token of 32-256 bytes is required"})
		return
	}
	if r.PathValue("action") == "claim" && w.Task == t {
		if err := s.preparePendingConfig(w); err != nil {
			fail(out, err)
			return
		}
	}
	if t.Handoff == nil {
		fail(out, apiError{409, "task mode changed before dispatch"})
		return
	}
	h := t.Handoff
	hash := claimHash(input.Token)
	first := false
	switch r.PathValue("action") {
	case "claim":
		owner := h.Assignee
		if owner.Route != nil {
			if input.Route == nil || input.Route.Provider != owner.Route.Provider || input.Route.Target != owner.Route.Target || input.Route.Session != owner.Route.Session || owner.Route.Session == "" {
				fail(out, apiError{403, "claim route does not match task assignee"})
				return
			}
		} else if owner.GitHubLogin == "" || !strings.EqualFold(owner.GitHubLogin, input.Login) {
			fail(out, apiError{403, "claim requires the assigned human login"})
			return
		}
		if h.ClaimHash != "" {
			if h.ClaimHash != hash {
				fail(out, apiError{409, "task already claimed; do not execute"})
				return
			}
			reply(out, 200, handoffResponse(w, t, false))
			return
		}
		if w.Task != t || h.State != "pending" || w.Pull.State != "open" || w.Pull.Draft || !w.Linked || w.executionBlocked() {
			fail(out, apiError{409, "task is not eligible; wait for the previous handoff or resolve the previous execution"})
			return
		}
		if reviewChannelUnverified(w, t) {
			fail(out, apiError{409, "review channel must be verified before claiming"})
			return
		}
		if owner.Route != nil {
			if err := s.validateReviewPreparation(w); err != nil {
				fail(out, apiError{409, "review preparation: " + err.Error()})
				return
			}
		}
		if err := s.freezeTaskConfig(w); err != nil {
			fail(out, err)
			return
		}
		h.State, h.ClaimHash, h.ClaimedAt = "claimed", hash, s.now()
		first = true
	case "retry":
		if h.ClaimHash != hash || h.ClaimHash == "" {
			fail(out, apiError{403, "claim credential does not match"})
			return
		}
		if h.State != "blocked" {
			fail(out, apiError{409, "only an explicitly blocked and handed-off task may be retried"})
			return
		}
		h.State = "superseded"
		if w.Task == t {
			w.replaceTask(nil)
			w.derive(s.now())
		}
		first = true
	case "submit":
		if h.ClaimHash != hash || h.ClaimHash == "" {
			fail(out, apiError{403, "claim credential does not match"})
			return
		}
		result := input.Submission
		if result == nil || strings.TrimSpace(result.RequestID) == "" || len(result.RequestID) > 200 || !result.Handoff {
			fail(out, apiError{400, "submission requires request_id and handoff=true after all task work has stopped"})
			return
		}
		if h.Submission != nil {
			if *h.Submission != *result {
				fail(out, apiError{409, "a different submission is already recorded"})
				return
			}
			reply(out, 200, handoffResponse(w, t, false))
			return
		}
		if h.State != "claimed" {
			fail(out, apiError{409, "task must be claimed before submission"})
			return
		}
		if result.Outcome == "blocked" {
			if strings.TrimSpace(result.Reason) == "" || result.NewSHA != "" || result.ReviewID != 0 {
				fail(out, apiError{400, "blocked handoff requires a reason and no result evidence"})
				return
			}
			h.State, h.FinishedAt = "blocked", s.now()
		} else if t.Role == "developer" {
			if !validSHA(result.NewSHA) || result.NewSHA == t.SHA || result.ReviewID != 0 || result.Outcome != "" {
				fail(out, apiError{400, "developer handoff requires a different valid new_sha"})
				return
			}
			h.State = "submitted"
		} else {
			if result.ReviewID <= 0 || result.NewSHA != "" || (result.Outcome != "APPROVED" && result.Outcome != "CHANGES_REQUESTED") {
				fail(out, apiError{400, "reviewer handoff requires review_id and APPROVED or CHANGES_REQUESTED outcome"})
				return
			}
			h.State = "submitted"
		}
		h.Submission = result
		h.SubmittedAt = s.now()
		h.Verification = "waiting for GitHub verification"
		if h.State == "blocked" {
			h.Verification = result.Reason
			s.reserveBlockedNotice(w, t)
		}
		first = true
	default:
		fail(out, apiError{404, "expected claim or submit"})
		return
	}
	s.refreshWorkflow(w, s.now())
	if err := s.store.saveWork(w); err != nil {
		fail(out, err)
		return
	}
	reply(out, 200, handoffResponse(w, t, first))
}

// Verify outside the state lock. On return, version and claim comparisons fence
// observations/assignments that raced with the network request. Completion,
// successor creation and its pending wake record share one SQLite transaction.
func (s *Service) reconcileHandoffWork(ctx context.Context, id string) error {
	w, err := s.store.loadWork(id)
	if err != nil {
		return err
	}
	if w == nil {
		return nil
	}
	for _, t := range w.executionTasks() {
		if h := t.Handoff; h != nil && h.State == "submitted" {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err := s.verifyHandoff(ctx, w, t); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) verifyHandoff(ctx context.Context, snapshot *Work, task *Task) error {
	result := *task.Handoff.Submission
	path := fmt.Sprintf("/repos/%s/pulls/%d", snapshot.Repo, snapshot.Pull.Number)
	var pull Pull
	_, readErr := s.up.gitHubGet(ctx, path, &pull)
	reason := "waiting for GitHub PR observation"
	state := "submitted"
	var reviews []Review
	if readErr == nil {
		if pull.Number != snapshot.Pull.Number || !validSHA(pull.Head.SHA) || (pull.State != "open" && pull.State != "closed") {
			readErr = fmt.Errorf("invalid PR observation")
		}
	}
	if readErr == nil {
		switch {
		case pull.State == "closed" || pull.Draft:
			state, reason = "superseded", "PR closed or moved to Draft; handoff recorded without claiming completion"
		case task.Role == "developer":
			switch pull.Head.SHA {
			case result.NewSHA:
				state, reason = "completed", ""
			case task.SHA:
				reason = "waiting for GitHub to observe submitted new_sha"
			default:
				state, reason = "superseded", "submitted SHA was superseded by another head"
			}
		default:
			reviews, readErr = listPages[Review](ctx, s.up, path+"/reviews")
			if readErr == nil {
				reason = "waiting for the specified GitHub review and identity"
				for _, review := range reviews {
					if review.ID == result.ReviewID && review.SHA == task.SHA && strings.EqualFold(review.User.Login, task.Handoff.Assignee.GitHubLogin) && review.State == result.Outcome {
						state, reason = "completed", ""
						break
					}
				}
				if pull.Head.SHA != task.SHA {
					state, reason = "superseded", "reviewed head has changed; handoff recorded for the old task"
				}
			}
		}
	}
	if readErr != nil {
		reason = "GitHub verification unavailable; retrying on the next poll"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	w, t, err := s.store.loadTask(task.ID)
	if err != nil {
		return err
	}
	if t == nil || t.Handoff == nil || w.Version != snapshot.Version || t.Handoff.State != "submitted" || t.Handoff.ClaimHash != task.Handoff.ClaimHash || t.Handoff.Submission == nil || *t.Handoff.Submission != result {
		return nil
	}
	h := t.Handoff
	h.Verification = reason
	if readErr == nil {
		// A changed assignee or retired task can release its claim, but cannot
		// complete work on behalf of a newer task. Evidence belongs to this task.
		h.State = state
		if state != "submitted" {
			h.FinishedAt = s.now()
		}
		w.Pull = pull
		if task.Role == "reviewer" {
			w.Reviews = mergeReviews(w.Reviews, reviews)
		}
		w.Version++
		w.derive(s.now())
	}
	s.refreshWorkflow(w, s.now())
	return s.store.saveWork(w)
}

func (w *Work) refreshHandoffWorkflow(now time.Time, c Reliability) {
	for _, t := range w.executionTasks() {
		h := t.Handoff
		if h == nil {
			continue
		}
		switch h.State {
		case "claimed":
			w.Workflow = Workflow{State: "waiting_handoff", Reason: "Agent owns task " + t.ID + "; waiting for explicit handoff"}
			if now.Sub(h.ClaimedAt) >= time.Duration(c.ExecutionSeconds)*time.Second {
				w.raiseAlert("handoff_overdue", t.ID, "Task handoff is overdue; ownership retained, no automatic code retry", now)
			}
		case "submitted":
			w.Workflow = Workflow{State: "verifying_handoff", Reason: h.Verification}
		case "blocked":
			w.raiseAlert("handoff_blocked", t.ID, h.Verification, now)
		case "pending":
			if w.Task == t {
				state := "waiting_claim"
				if h.Assignee.Route == nil {
					state = "waiting_human"
				}
				if w.executionBlocked() {
					state = "waiting_previous_handoff"
				}
				w.Workflow = Workflow{State: state}
				if state == "waiting_claim" {
					switch {
					case h.Wake.AcceptedAt != nil:
						w.Workflow.Reason = "Wake accepted by the execution provider. Waiting for the " + t.Role + " to claim."
					case h.Assignee.Route.Session != "" && !reviewChannelUnverified(w, t):
						w.Workflow.Reason = "Conversation assigned to " + h.Assignee.Route.Session + ". Waiting for wake acceptance."
					case h.Wake.Preparation == "preparing":
						w.Workflow.Reason = "Waiting for conversation allocation to be confirmed"
					default:
						w.Workflow.Reason = "Waiting for conversation allocation"
					}
				}
				if h.Wake.Error != "" {
					w.raiseAlert("task_wake_unresolved", t.ID, h.Wake.Error, now)
				}
				if h.Wake.Preparation == "preparing" {
					message := "Conversation allocation is unresolved. Verify and bind an existing session."
					if t.Role == "reviewer" {
						message = "Conversation allocation is unresolved. Verify the thread, then bind it with ink review --session."
					}
					if h.Wake.RecoveryStopped {
						message = "Conversation recovery was stopped. Assign a verified session."
					} else if h.Wake.canRecover() {
						message = "Conversation allocation is unresolved. The adapter will look up the original allocation."
					}
					w.raiseAlert("task_preparation_unknown", t.ID, message, now)
				}
			}
		}
	}
	if w.Phase == "approved" && (w.Task == nil || w.Task.ConflictBase == nil) && w.handoffMergeBlocked() {
		w.Workflow = Workflow{State: "waiting_handoff", Reason: "Approval observed; waiting for a verified reviewer handoff"}
	}
}
