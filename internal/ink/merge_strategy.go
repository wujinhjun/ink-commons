//go:build linux || darwin

package ink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
)

const directStrategyName = "direct"

type mergeTarget struct {
	Repository string
	Number     int
	SHA        string
	Method     string
	Stack      *PullStack
	Identity   string
}

type mergeOutcome uint8

const (
	mergeUncertain mergeOutcome = iota
	mergeCompleted
	mergePending
	mergeRejected
)

type mergeResult struct {
	outcome             mergeOutcome
	retryable           bool
	reason              string
	requestID, mergeSHA string
	verifyClosed        bool
	err                 error
}

// MergeStrategy submits or reconciles an already authorized, SHA-bound merge.
type MergeStrategy interface {
	Submit(ctx context.Context, merger Merger, target mergeTarget) mergeResult
	Reconcile(ctx context.Context, merger Merger, target mergeTarget, requestID string) mergeResult
}

// Merger performs the GitHub operations using the identity in the frozen target.
type Merger interface {
	Merge(ctx context.Context, target mergeTarget) (string, error)
	SubmitStack(ctx context.Context, target mergeTarget) (stackMergeResult, int, error)
	StackStatus(ctx context.Context, target mergeTarget, requestID uuid.UUID) (stackMergeResult, error)
}

func strategyFor(name string) MergeStrategy {
	switch name {
	case "", directStrategyName:
		return directMergeStrategy{}
	default:
		return nil
	}
}

// A refusal proves that GitHub could not accept the merge. It covers
// requests stopped locally and definitive authentication or rate-limit responses.
type mergeRequestRefused struct{ error }

func (e mergeRequestRefused) Unwrap() error { return e.error }

func notSubmitted(err error) error { return mergeRequestRefused{err} }

func canRetryMerge(err error) bool {
	var refused mergeRequestRefused
	return errors.As(err, &refused) || errors.Is(err, errAppCommandNotStarted)
}

type directMergeStrategy struct{}

func (directMergeStrategy) Submit(ctx context.Context, merger Merger, target mergeTarget) mergeResult {
	if target.Stack == nil {
		sha, err := merger.Merge(ctx, target)
		switch {
		case canRetryMerge(err):
			return mergeResult{outcome: mergeRejected, retryable: true, reason: err.Error(), err: err}
		case err != nil:
			return mergeResult{outcome: mergeUncertain, reason: "merge request failed or response was lost: " + err.Error() + "; verify GitHub before taking action", err: err}
		default:
			return mergeResult{outcome: mergeCompleted, mergeSHA: sha}
		}
	}

	result, status, err := merger.SubmitStack(ctx, target)
	switch {
	case canRetryMerge(err):
		return mergeResult{outcome: mergeRejected, retryable: true, reason: err.Error(), err: err}
	case status == http.StatusBadRequest:
		return mergeResult{outcome: mergeRejected, retryable: true, reason: "GitHub rejected the stack merge before accepting it (HTTP 400); waiting for preflight", err: err}
	case err != nil:
		return mergeResult{outcome: mergeUncertain, reason: "stack merge request could not be confirmed: " + err.Error(), err: err}
	case status == http.StatusAccepted || status == http.StatusConflict:
		details := result.Details
		id, idErr := uuid.Parse(details.ID)
		exact := details.ExpectedHead == target.SHA && details.Method == target.Method &&
			details.Action == stackMergeAction && !details.BypassRules
		accepted := status == http.StatusAccepted && !details.BypassRules &&
			(details.ExpectedHead == "" || details.ExpectedHead == target.SHA) &&
			(details.Method == "" || details.Method == target.Method) &&
			(details.Action == "" || details.Action == stackMergeAction)
		if idErr == nil && (accepted || exact) {
			return stackOutcome(stackMergeResult{Status: "pending"}, id.String())
		}
		return mergeResult{outcome: mergeUncertain, reason: "GitHub did not confirm a matching stack merge request"}
	case status == http.StatusOK && result.Status == "merged":
		return mergeResult{outcome: mergeUncertain, verifyClosed: true, reason: "waiting to verify the head of the already merged PR"}
	default:
		return stackOutcome(result, "")
	}
}

func (directMergeStrategy) Reconcile(ctx context.Context, merger Merger, target mergeTarget, requestID string) mergeResult {
	id, err := uuid.Parse(requestID)
	if err != nil {
		return mergeResult{outcome: mergeUncertain, reason: "invalid stored stack merge request ID", requestID: requestID}
	}
	result, err := merger.StackStatus(ctx, target, id)
	if err != nil {
		return mergeResult{outcome: mergeUncertain, requestID: requestID, reason: "cannot read the GitHub stack merge result: " + err.Error(), err: err}
	}
	return stackOutcome(result, requestID)
}

func stackOutcome(result stackMergeResult, requestID string) mergeResult {
	switch result.Status {
	case "pending":
		if requestID != "" {
			return mergeResult{outcome: mergePending, requestID: requestID, reason: "waiting for GitHub to complete the stack merge"}
		}
	case "merged":
		if validSHA(result.Details.SHA) {
			return mergeResult{outcome: mergeCompleted, requestID: requestID, mergeSHA: result.Details.SHA}
		}
	case "enqueued":
		return mergeResult{outcome: mergePending, reason: "waiting for GitHub's merge queue to merge the PR"}
	case "failed":
		return mergeResult{outcome: mergeRejected, requestID: requestID, reason: "GitHub reported that the stack merge failed"}
	}
	return mergeResult{outcome: mergeUncertain, requestID: requestID, reason: "GitHub did not confirm the stack merge outcome"}
}

type githubMergeOperations struct{ upstream }

func (m githubMergeOperations) Merge(ctx context.Context, target mergeTarget) (string, error) {
	if err := m.validateTarget(target); err != nil {
		return "", err
	}
	return m.merge(ctx, target.Repository, target.Number, target.SHA, target.Method)
}

func (u upstream) merge(ctx context.Context, repo string, number int, sha, method string) (string, error) {
	if !ValidRepository(repo) || number <= 0 || !validSHA(sha) {
		return "", fmt.Errorf("invalid merge target")
	}
	path := fmt.Sprintf("/repos/%s/pulls/%d/merge", repo, number)
	data, _ := json.Marshal(map[string]string{"sha": sha, "merge_method": method})
	var result struct {
		Merged bool   `json:"merged"`
		SHA    string `json:"sha"`
	}
	if _, _, err := u.gitHubJSON(ctx, http.MethodPut, path, "code", "", data, &result); err != nil {
		return "", err
	}
	if !result.Merged || !validSHA(result.SHA) {
		return "", diagnosticError("merge outcome not confirmed")
	}
	return result.SHA, nil
}

func (m githubMergeOperations) validateTarget(target mergeTarget) error {
	if target.Identity == "" || m.expectedIdentity != target.Identity {
		return notSubmitted(diagnosticError("original GitHub merger identity is unavailable"))
	}
	if !ValidRepository(target.Repository) || target.Number <= 0 || !validSHA(target.SHA) {
		return notSubmitted(diagnosticError("invalid merge target"))
	}
	return nil
}

func (m githubMergeOperations) SubmitStack(ctx context.Context, target mergeTarget) (stackMergeResult, int, error) {
	var result stackMergeResult
	if err := m.validateTarget(target); err != nil {
		return result, 0, err
	}
	body, _ := json.Marshal(map[string]string{"sha": target.SHA, "merge_method": target.Method, "merge_action": stackMergeAction})
	_, status, err := m.gitHubJSON(ctx, http.MethodPut, stackMergePath(Repository{Repo: target.Repository}, target.Number), "code", stackAPIVersion, body, &result, http.StatusConflict)
	return result, status, err
}

func (m githubMergeOperations) StackStatus(ctx context.Context, target mergeTarget, requestID uuid.UUID) (stackMergeResult, error) {
	var result stackMergeResult
	if err := m.validateTarget(target); err != nil {
		return result, err
	}
	path := stackMergePath(Repository{Repo: target.Repository}, target.Number) + "/" + requestID.String()
	_, _, err := m.gitHubJSON(ctx, http.MethodGet, path, "code", stackAPIVersion, nil, &result)
	return result, err
}

func (run AutoMergeRun) mergeTarget(repo Repository, number int) mergeTarget {
	return mergeTarget{Repository: repo.Repo, Number: number, SHA: run.SHA, Method: run.Method, Identity: run.MergerIdentity}
}

func (s *Service) applyMergeResult(ctx context.Context, target mergeTarget, run *AutoMergeRun, result mergeResult) {
	switch result.outcome {
	case mergeCompleted:
		run.State = "merged"
	case mergePending:
		run.State = "pending"
	case mergeRejected:
		run.State = "failed"
		if result.retryable {
			run.State = "waiting"
		}
	default:
		run.State = "unknown"
	}
	run.Reason = result.reason
	run.RequestID, run.MergeSHA = result.requestID, result.mergeSHA
	if result.verifyClosed {
		// An already-merged response does not identify which head landed.
		var pull Pull
		path := fmt.Sprintf("/repos/%s/pulls/%d", target.Repository, target.Number)
		if _, _, err := s.up.gitHubJSON(ctx, http.MethodGet, path, "inspect", stackAPIVersion, nil, &pull); err == nil &&
			pull.Number == target.Number && pull.State == "closed" && pull.Merged && validSHA(pull.Head.SHA) && validSHA(pull.MergeCommitSHA) {
			run.recordClosedPull(pull)
		}
	}
}

func (s *Service) reconcileMerge(ctx context.Context, repo Repository, number int, run AutoMergeRun) error {
	next := run
	strategy := strategyFor(run.Strategy)
	merger, err := s.up.mergeRole(run.ReuseReaderIdentity)
	switch {
	case strategy == nil:
		next.State, next.Reason = "unknown", "stored GitHub merge strategy is unavailable"
	case run.MergerIdentity == "" || err != nil:
		next.State, next.Reason = "unknown", "original GitHub merger identity is unavailable"
	default:
		merger.expectedIdentity = run.MergerIdentity
		target := run.mergeTarget(repo, number)
		result := strategy.Reconcile(ctx, githubMergeOperations{merger}, target, run.RequestID)
		s.applyMergeResult(ctx, target, &next, result)
	}
	if next == run || ctx.Err() != nil {
		return nil
	}
	_, err = s.updateAutoMerge(repo, number, next, false)
	return err
}
