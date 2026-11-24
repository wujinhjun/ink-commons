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

func sameStack(a, b *PullStack) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

type stackMergeResult struct {
	Status  string `json:"status"`
	Details struct {
		ID           string `json:"uuid"`
		ExpectedHead string `json:"expected_head_sha"`
		Action       string `json:"merge_action"`
		Method       string `json:"merge_method"`
		BypassRules  bool   `json:"bypass_rules"`
		SHA          string `json:"sha"`
	} `json:"details"`
}

const stackMergeAction = "direct_merge"

func stackMergePath(repo Repository, number int) string {
	return fmt.Sprintf("/repos/%s/pulls/%d/merge-async", repo.Repo, number)
}

func (s *Service) startStackMerge(ctx context.Context, repo Repository, number int, run AutoMergeRun, merger upstream) error {
	body, _ := json.Marshal(map[string]string{
		"sha": run.SHA, "merge_method": run.Method, "merge_action": stackMergeAction,
	})
	var result stackMergeResult
	_, status, err := merger.gitHubJSON(ctx, http.MethodPut, stackMergePath(repo, number), "code", stackAPIVersion, body, &result, http.StatusConflict)
	switch {
	case errors.Is(err, errAppCommandNotStarted):
		run.State = "waiting"
		run.Reason = err.Error()
	case status == http.StatusBadRequest:
		run.State = "waiting"
		run.Reason = "GitHub rejected the stack merge before accepting it (HTTP 400); waiting for preflight"
	case err != nil:
		run.Reason = "stack merge request could not be confirmed: " + err.Error()
	case status == http.StatusAccepted || status == http.StatusConflict:
		run.Reason = "GitHub did not confirm a matching stack merge request"
		id, idErr := uuid.Parse(result.Details.ID)
		exact := result.Details.ExpectedHead == run.SHA && result.Details.Method == run.Method &&
			result.Details.Action == stackMergeAction && !result.Details.BypassRules
		accepted := status == http.StatusAccepted && !result.Details.BypassRules &&
			(result.Details.ExpectedHead == "" || result.Details.ExpectedHead == run.SHA) &&
			(result.Details.Method == "" || result.Details.Method == run.Method) &&
			(result.Details.Action == "" || result.Details.Action == stackMergeAction)
		if idErr == nil && (accepted || exact) {
			run.RequestID = id.String()
			run.State = "pending"
			run.Reason = "waiting for GitHub to complete the stack merge"
		}
	case status == http.StatusOK && result.Status == "merged":
		// A PUT that finds an already merged PR does not identify which PR head landed.
		path := fmt.Sprintf("/repos/%s/pulls/%d", repo.Repo, number)
		var pull Pull
		if _, _, readErr := s.up.gitHubJSON(ctx, http.MethodGet, path, "inspect", stackAPIVersion, nil, &pull); readErr != nil ||
			pull.Number != number || pull.State != "closed" || !pull.Merged || !validSHA(pull.Head.SHA) || !validSHA(pull.MergeCommitSHA) {
			run.Reason = "waiting to verify the head of the already merged PR"
		} else {
			run.recordClosedPull(pull)
		}
	default:
		run.recordStackMergeResult(result)
	}
	_, err = s.updateAutoMerge(repo, number, run, false)
	return err
}

func (s *Service) pollStackMerge(ctx context.Context, repo Repository, number int, run AutoMergeRun) error {
	id, err := uuid.Parse(run.RequestID)
	if err != nil {
		return fmt.Errorf("invalid stored stack merge request ID")
	}
	var result stackMergeResult
	// Poll with the same identity mode that submitted the original request,
	// even if the repository configuration changed while it was pending.
	merger, err := s.up.merger(run.ReuseReaderIdentity)
	if err == nil {
		_, _, err = merger.gitHubJSON(ctx, http.MethodGet, stackMergePath(repo, number)+"/"+id.String(), "code", stackAPIVersion, nil, &result)
	}
	next := run
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		next.State = "unknown"
		next.Reason = "cannot read the GitHub stack merge result: " + err.Error()
	} else {
		next.recordStackMergeResult(result)
	}
	if next == run {
		return nil
	}
	_, err = s.updateAutoMerge(repo, number, next, false)
	return err
}

func (run *AutoMergeRun) recordStackMergeResult(result stackMergeResult) {
	run.State = "unknown"
	run.Reason = "GitHub did not confirm the stack merge outcome"
	switch result.Status {
	case "pending":
		if run.RequestID != "" {
			run.State = "pending"
			run.Reason = "waiting for GitHub to complete the stack merge"
		}
	case "merged":
		if validSHA(result.Details.SHA) {
			run.State = "merged"
			run.Reason = ""
			run.MergeSHA = result.Details.SHA
		}
	case "enqueued":
		run.State = "pending"
		run.Reason = "waiting for GitHub's merge queue to merge the PR"
		run.RequestID = ""
	case "failed":
		run.State = "failed"
		run.Reason = "GitHub reported that the stack merge failed"
	}
}
