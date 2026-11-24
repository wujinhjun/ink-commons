//go:build linux || darwin

package ink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type AutoMergeConfig struct {
	Enabled             bool   `json:"enabled"`
	Method              string `json:"method,omitempty"`
	ReuseReaderIdentity bool   `json:"reuse_reader_identity,omitempty"`
}

func (c *AutoMergeConfig) method() string {
	if c == nil || c.Method == "" {
		return "merge"
	}
	return c.Method
}
func (c *AutoMergeConfig) Validate() error {
	if c == nil {
		return nil
	}
	switch c.method() {
	case "merge", "squash", "rebase":
		return nil
	}
	return fmt.Errorf("auto_merge.method must be merge, squash or rebase")
}

type AutoMergeRun struct {
	preflight           *mergePreflight
	RequestID           string    `json:"request_id,omitempty"`
	ReuseReaderIdentity bool      `json:"reuse_reader_identity,omitempty"`
	SHA                 string    `json:"sha"`
	Reviewer            string    `json:"reviewer"`
	ReviewID            int64     `json:"review_id"`
	Method              string    `json:"method"`
	State               string    `json:"state"`
	Reason              string    `json:"reason,omitempty"`
	MergeSHA            string    `json:"merge_sha,omitempty"`
	UpdatedAt           time.Time `json:"updated_at"`
}

func (run AutoMergeRun) inFlight() bool {
	return run.State == "unknown" || run.State == "pending"
}

func hasInFlightAutoMerge(w *Work) bool {
	for _, run := range w.AutoMerges {
		if run.inFlight() {
			return true
		}
	}
	return false
}

// Closing resolves every outstanding journal entry from the observed PR result.
// A different final head does not prove that the older approved SHA was merged.
func reconcileClosedAutoMerges(w *Work, now time.Time) bool {
	if w.Pull.State != "closed" {
		return false
	}
	changed := false
	for i := range w.AutoMerges {
		run := &w.AutoMerges[i]
		if !run.inFlight() {
			continue
		}
		changed = true
		run.UpdatedAt = now
		run.recordClosedPull(w.Pull)
	}
	return changed
}

func (run *AutoMergeRun) recordClosedPull(pull Pull) {
	run.MergeSHA = ""
	switch {
	case !pull.Merged:
		run.State = "failed"
		run.Reason = "observed PR closed without merging"
	case pull.Head.SHA != run.SHA:
		run.State = "superseded"
		run.Reason = "observed PR merged with a different head " + pull.Head.SHA + "; the older SHA-bound request is not attributed as the merge"
	default:
		run.State = "merged"
		run.Reason = ""
		run.MergeSHA = pull.MergeCommitSHA
	}
}

func approvedReview(reviews []Review, sha, login string) *Review {
	var latest *Review
	for i := range reviews {
		r := &reviews[i]
		if r.SHA == sha && login != "" && strings.EqualFold(r.User.Login, login) && (r.State == "APPROVED" || r.State == "CHANGES_REQUESTED") {
			latest = r
		}
	}
	if latest != nil && latest.State == "APPROVED" {
		return latest
	}
	return nil
}

func (u upstream) checksReady(ctx context.Context, repo, sha string) (bool, error) {
	for page := 1; page <= 100; page++ {
		var runs struct {
			CheckRuns []struct {
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
			} `json:"check_runs"`
		}
		header, err := u.gitHubGet(ctx, fmt.Sprintf("/repos/%s/commits/%s/check-runs?filter=latest&per_page=100&page=%d", repo, sha, page), &runs)
		if err != nil {
			return false, err
		}
		if runs.CheckRuns == nil {
			return false, fmt.Errorf("missing check-run list")
		}
		for _, r := range runs.CheckRuns {
			if r.Status != "completed" {
				return false, nil
			}
			switch r.Conclusion {
			case "success", "neutral", "skipped":
			default:
				return false, nil
			}
		}
		if !strings.Contains(header.Get("Link"), `rel="next"`) {
			return true, nil
		}
	}
	return false, fmt.Errorf("too many check-run pages")
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
		return "", fmt.Errorf("merge outcome not confirmed")
	}
	return result.SHA, nil
}

// 合入账本与完成通知一起持久化；预留或终态记录会阻止重复 merge，
// 结果不明的外部写入不重放。
type mergePreflight struct {
	version        uint64
	policyRevision int64
}

func (s *Service) updateAutoMerge(repo Repository, number int, run AutoMergeRun, reserve bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, err := s.store.loadWork(key(repo.Repo, number))
	if err != nil {
		return false, err
	}
	if w == nil {
		return false, nil
	}
	if run.preflight != nil && (reserve || run.UpdatedAt.IsZero()) {
		current, err := s.store.effectiveConfig(repo.Repo)
		if err != nil {
			return false, err
		}
		if w.Version != run.preflight.version || w.Pull.Head.SHA != run.SHA || current.Revision != run.preflight.policyRevision {
			return false, nil
		}
	}
	// Preflight may not overwrite an in-flight write. A response tied to the
	// exact reservation can release it after a definitive rejection.
	if (reserve || (run.State == "waiting" && run.UpdatedAt.IsZero())) && hasInFlightAutoMerge(w) {
		return false, nil
	}
	for _, old := range w.AutoMerges {
		if old.SHA != run.SHA {
			continue
		}
		if !reserve && !run.UpdatedAt.IsZero() && !old.UpdatedAt.Equal(run.UpdatedAt) {
			return false, nil
		}
		switch old.State {
		case "merged", "failed", "superseded":
			return false, nil
		}
	}
	if reserve {
		if w.handoffMergeBlocked() || (w.Task != nil && w.Task.ConflictBase != nil) {
			return false, nil
		}
		current, ok := s.repo(repo.Repo)
		if !ok || current.AutoMerge == nil || !current.AutoMerge.Enabled || current.AutoMerge.method() != run.Method ||
			current.AutoMerge.ReuseReaderIdentity != run.ReuseReaderIdentity || w.Phase != "approved" || w.Pull.Head.SHA != run.SHA || !strings.EqualFold(w.Reviewer.GitHubLogin, run.Reviewer) {
			return false, nil
		}
		merger, err := s.up.merger(run.ReuseReaderIdentity)
		if err != nil || sameAppCommand(w.Reviewer.GitHubCommand, merger.command) {
			return false, nil
		}
	}
	if !reserve || run.UpdatedAt.IsZero() {
		run.UpdatedAt = s.now()
	}
	found := false
	for i := range w.AutoMerges {
		if w.AutoMerges[i].SHA == run.SHA {
			w.AutoMerges[i] = run
			found = true
			break
		}
	}
	if !found {
		w.AutoMerges = append(w.AutoMerges, run)
	}
	if run.State == "merged" {
		s.reserveMergedNotification(w, repo, run)
	}
	s.refreshWorkflow(w, s.now())
	return true, s.store.saveWork(w)
}

func (s *Service) autoMerge(ctx context.Context, repo Repository, number int) error {
	if ctx.Err() != nil {
		return nil
	}
	s.mu.Lock()
	w, err := s.store.loadWork(key(repo.Repo, number))
	if err != nil {
		s.mu.Unlock()
		return err
	}
	if w == nil || !matchesObservation(ctx, w) {
		s.mu.Unlock()
		return nil
	}
	effective, err := s.store.effectiveConfig(repo.Repo)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	repo = effective.Repository
	// Reconcile ambiguous responses using GitHub observations even if the switch
	// has subsequently been disabled. Never infer failure from a lost response.
	mergedBefore := make(map[string]bool)
	for _, run := range w.AutoMerges {
		mergedBefore[run.SHA] = run.State == "merged"
	}
	if reconcileClosedAutoMerges(w, s.now()) {
		for _, run := range w.AutoMerges {
			if run.State == "merged" && !mergedBefore[run.SHA] {
				s.reserveMergedNotification(w, repo, run)
			}
		}
		s.refreshWorkflow(w, s.now())
		err := s.store.saveWork(w)
		s.mu.Unlock()
		return err
	}
	for _, run := range w.AutoMerges {
		if run.inFlight() && run.RequestID != "" {
			s.mu.Unlock()
			return s.pollStackMerge(ctx, repo, number, run)
		}
	}
	// An unresolved write fences subsequent heads until GitHub reports an
	// outcome or an operator reconciles it.
	if hasInFlightAutoMerge(w) {
		s.mu.Unlock()
		return nil
	}
	if repo.AutoMerge == nil || !repo.AutoMerge.Enabled || w.Phase != "approved" || (w.handoffMergeBlocked() && !w.blockedConflictTask()) {
		s.mu.Unlock()
		return nil
	}
	for _, run := range w.AutoMerges {
		if run.SHA == w.Pull.Head.SHA && (run.State == "merged" || run.State == "failed" || run.State == "superseded") {
			s.mu.Unlock()
			return nil
		}
	}
	run := AutoMergeRun{preflight: &mergePreflight{version: w.Version, policyRevision: effective.Revision}, SHA: w.Pull.Head.SHA, Reviewer: w.Reviewer.GitHubLogin, Method: repo.AutoMerge.method(), State: "waiting", ReuseReaderIdentity: repo.AutoMerge.ReuseReaderIdentity}
	version, observedPull, reviewerCommand := w.Version, w.Pull, w.Reviewer.GitHubCommand
	confirmingConflict := w.MergeConflict != nil
	trackingConflict := confirmingConflict || (w.Task != nil && w.Task.ConflictBase != nil)
	observedConflictBase := w.conflictBase()
	s.mu.Unlock()
	path := fmt.Sprintf("/repos/%s/pulls/%d", repo.Repo, number)
	wait := func(reason string) error {
		run.State = "waiting"
		run.Reason = reason
		_, err := s.updateAutoMerge(repo, number, run, false)
		return err
	}
	var pull Pull
	if _, _, err := s.up.gitHubJSON(ctx, http.MethodGet, path, "inspect", stackAPIVersion, nil, &pull); err != nil {
		if confirmingConflict {
			observedPull.Mergeable = nil
			if err := s.observeMergeConflict(repo.Repo, number, version, observedPull, PullBase{}, false); err != nil {
				return err
			}
		}
		return wait("cannot refresh PR before merge")
	}
	if pull.State != "open" || pull.Draft || pull.Head.SHA != run.SHA {
		return wait("PR state or head changed; waiting for observation")
	}
	if pull.Stack != nil && (pull.Stack.ID <= 0 || pull.Stack.Position <= 0 || pull.Stack.Base.Ref == "" || pull.Base.Ref != pull.Stack.Base.Ref) {
		return wait("waiting for this PR to become the bottom of its GitHub stack")
	}
	dirty := pull.Mergeable != nil && !*pull.Mergeable && pull.MergeableState == "dirty"
	mergeable := pull.Mergeable != nil && *pull.Mergeable
	var base PullBase
	if (dirty && repo.TaskMode == "cooperative") || (trackingConflict && mergeable && pull.Base != observedConflictBase) {
		var err error
		base, err = s.up.verifyConflictBase(ctx, repo.Repo, pull, dirty)
		if err != nil {
			if dirty && confirmingConflict {
				pull.Mergeable = nil
				if resetErr := s.observeMergeConflict(repo.Repo, number, version, pull, PullBase{}, false); resetErr != nil {
					return resetErr
				}
			}
			return wait("cannot confirm mergeability against the current GitHub base ref")
		}
	}
	if trackingConflict || dirty {
		if err := s.observeMergeConflict(repo.Repo, number, version, pull, base, dirty); err != nil {
			return err
		}
	}
	if dirty {
		return wait("GitHub reports a merge conflict with " + pull.Base.Ref)
	}
	if pull.Mergeable == nil || !*pull.Mergeable || pull.MergeableState != "clean" {
		return wait("GitHub has not reported a clean, mergeable PR")
	}
	mergeUpstream, err := s.up.merger(run.ReuseReaderIdentity)
	if err != nil {
		return wait(err.Error())
	}
	if sameAppCommand(reviewerCommand, mergeUpstream.command) {
		return wait("reviewer and merger must use separate GitHub Apps")
	}
	refs := []string{run.SHA}
	if validSHA(pull.MergeCommitSHA) && pull.MergeCommitSHA != run.SHA {
		refs = append(refs, pull.MergeCommitSHA)
	}
	for _, sha := range refs {
		ready, err := s.up.checksReady(ctx, repo.Repo, sha)
		if err != nil {
			return wait("cannot read checks; automatic merge is blocked: " + err.Error())
		}
		if !ready {
			return wait("waiting for checks to pass")
		}
	}
	reviews, err := listPages[Review](ctx, s.up, path+"/reviews")
	if err != nil {
		return wait("cannot refresh reviews before merge")
	}
	approval := approvedReview(mergeReviews(nil, reviews), run.SHA, run.Reviewer)
	if approval == nil {
		return wait("current-SHA approval is no longer valid")
	}
	run.ReviewID = approval.ID
	var fresh Pull
	if _, _, err := s.up.gitHubJSON(ctx, http.MethodGet, path, "inspect", stackAPIVersion, nil, &fresh); err != nil {
		return wait("cannot revalidate PR before merge")
	}
	if fresh.State != "open" || fresh.Draft || fresh.Head.SHA != run.SHA || fresh.Base != pull.Base || !sameStack(fresh.Stack, pull.Stack) || fresh.MergeCommitSHA != pull.MergeCommitSHA || fresh.Mergeable == nil || !*fresh.Mergeable || fresh.MergeableState != "clean" {
		return wait("PR changed during preflight")
	}
	run.State = "unknown"
	run.Reason = "merge request reserved; outcome not yet confirmed"
	run.UpdatedAt = s.now()
	reserved, err := s.updateAutoMerge(repo, number, run, true)
	if err != nil || !reserved {
		return err
	}
	if fresh.Stack != nil {
		return s.startStackMerge(ctx, repo, number, run, mergeUpstream)
	}
	mergeSHA, err := mergeUpstream.merge(ctx, repo.Repo, number, run.SHA, run.Method)
	switch {
	case errors.Is(err, errAppCommandNotStarted):
		run.State = "waiting"
		run.Reason = err.Error()
	case err != nil:
		run.Reason = "merge request failed or response was lost: " + err.Error() + "; verify GitHub before taking action"
	default:
		run.State = "merged"
		run.Reason = ""
		run.MergeSHA = mergeSHA
	}
	_, saveErr := s.updateAutoMerge(repo, number, run, false)
	return saveErr
}
