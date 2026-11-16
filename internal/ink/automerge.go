package ink

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type AutoMergeConfig struct {
	Enabled bool   `json:"enabled"`
	Method  string `json:"method,omitempty"`
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
	SHA       string    `json:"sha"`
	Reviewer  string    `json:"reviewer"`
	ReviewID  int64     `json:"review_id"`
	Method    string    `json:"method"`
	State     string    `json:"state"`
	Reason    string    `json:"reason,omitempty"`
	MergeSHA  string    `json:"merge_sha,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

func hasUnknownAutoMerge(w *Work) bool {
	for _, run := range w.AutoMerges {
		if run.State == "unknown" {
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
		if run.State != "unknown" {
			continue
		}
		changed = true
		run.UpdatedAt = now
		run.State = "failed"
		run.Reason = "observed PR closed without merging"
		run.MergeSHA = ""
		if w.Pull.Merged {
			if w.Pull.Head.SHA == run.SHA {
				run.State = "merged"
				run.Reason = ""
				run.MergeSHA = w.Pull.MergeCommitSHA
			} else {
				run.State = "superseded"
				run.Reason = "observed PR merged with a different head " + w.Pull.Head.SHA + "; the older SHA-bound request is not attributed as the merge"
			}
		}
	}
	return changed
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
	if u.command != "" {
		if _, err := u.commandRequest(ctx, "PUT", path, "code", data, &result); err != nil {
			return "", err
		}
	} else {
		req, err := http.NewRequestWithContext(ctx, "PUT", u.github+path, bytes.NewReader(data))
		if err != nil {
			return "", fmt.Errorf("invalid merge request")
		}
		if u.token != "" {
			req.Header.Set("Authorization", "Bearer "+u.token)
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("Content-Type", "application/json")
		res, err := u.http.Do(req)
		if err != nil {
			return "", fmt.Errorf("merge transport failure")
		}
		defer res.Body.Close()
		if res.StatusCode != 200 {
			return "", fmt.Errorf("merge HTTP %d", res.StatusCode)
		}
		if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&result); err != nil {
			return "", fmt.Errorf("invalid merge response")
		}
	}
	if !result.Merged || !validSHA(result.SHA) {
		return "", fmt.Errorf("merge outcome not confirmed")
	}
	return result.SHA, nil
}

// updateAutoMerge changes only the merge journal. A reserved/terminal attempt
// fences concurrent polls and survives restart; ambiguous writes are not retried.
func (s *Service) updateAutoMerge(repo Repository, number int, run AutoMergeRun, reserve bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.store.load()
	if err != nil {
		return false, err
	}
	w := st.Work[key(repo.Repo, number)]
	if w == nil {
		return false, nil
	}
	if (reserve || run.State == "waiting") && hasUnknownAutoMerge(w) {
		return false, nil
	}
	for _, old := range w.AutoMerges {
		if old.SHA == run.SHA && ((run.State == "waiting" && (old.State == "unknown" || old.State == "merged" || old.State == "failed")) || (old.State == "merged" && run.State != "merged")) {
			return false, nil
		}
	}
	if reserve {
		current, ok := s.repo(repo.Repo)
		if !ok || current.AutoMerge == nil || !current.AutoMerge.Enabled || current.AutoMerge.method() != run.Method || w.Phase != "approved" || w.Pull.Head.SHA != run.SHA || !strings.EqualFold(w.Reviewer.GitHubLogin, run.Reviewer) {
			return false, nil
		}
		for _, r := range w.AutoMerges {
			if r.SHA == run.SHA && (r.State == "unknown" || r.State == "merged" || r.State == "failed") {
				return false, nil
			}
		}
	}
	run.UpdatedAt = time.Now()
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
	w.refreshWorkflow(time.Now(), s.config.Reliability.defaults())
	return true, s.store.save(st)
}

func (s *Service) autoMerge(ctx context.Context, repo Repository, number int) error {
	if ctx.Err() != nil {
		return nil
	}
	s.mu.Lock()
	st, err := s.store.load()
	if err != nil {
		s.mu.Unlock()
		return err
	}
	w := st.Work[key(repo.Repo, number)]
	if w == nil {
		s.mu.Unlock()
		return nil
	}
	// Reconcile ambiguous responses using GitHub observations even if the switch
	// has subsequently been disabled. Never infer failure from a lost response.
	if reconcileClosedAutoMerges(w, time.Now()) {
		w.refreshWorkflow(time.Now(), s.config.Reliability.defaults())
		err := s.store.save(st)
		s.mu.Unlock()
		return err
	}
	// Keep historical unknowns visible and fence all subsequent heads until an
	// operator reconciles the outcome or GitHub reports the PR's closed result.
	if hasUnknownAutoMerge(w) {
		s.mu.Unlock()
		return nil
	}
	if repo.AutoMerge == nil || !repo.AutoMerge.Enabled || w.Phase != "approved" {
		s.mu.Unlock()
		return nil
	}
	for _, run := range w.AutoMerges {
		if run.SHA == w.Pull.Head.SHA && (run.State == "unknown" || run.State == "merged" || run.State == "failed") {
			s.mu.Unlock()
			return nil
		}
	}
	run := AutoMergeRun{SHA: w.Pull.Head.SHA, Reviewer: w.Reviewer.GitHubLogin, Method: repo.AutoMerge.method(), State: "waiting"}
	s.mu.Unlock()
	path := fmt.Sprintf("/repos/%s/pulls/%d", repo.Repo, number)
	wait := func(reason string) error {
		run.State = "waiting"
		run.Reason = reason
		_, err := s.updateAutoMerge(repo, number, run, false)
		return err
	}
	var pull Pull
	if _, err := s.up.gitHubGet(ctx, path, &pull); err != nil {
		return wait("cannot refresh PR before merge")
	}
	if pull.State != "open" || pull.Draft || pull.Head.SHA != run.SHA {
		return wait("PR state or head changed; waiting for observation")
	}
	if pull.Mergeable == nil || !*pull.Mergeable || pull.MergeableState != "clean" {
		return wait("GitHub has not reported a clean, mergeable PR")
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
	if _, err := s.up.gitHubGet(ctx, path, &fresh); err != nil {
		return wait("cannot revalidate PR before merge")
	}
	if fresh.State != "open" || fresh.Draft || fresh.Head.SHA != run.SHA || fresh.Base.SHA != pull.Base.SHA || fresh.MergeCommitSHA != pull.MergeCommitSHA || fresh.Mergeable == nil || !*fresh.Mergeable || fresh.MergeableState != "clean" {
		return wait("PR changed during preflight")
	}
	run.State = "unknown"
	run.Reason = "merge request reserved; outcome not yet confirmed"
	reserved, err := s.updateAutoMerge(repo, number, run, true)
	if err != nil || !reserved {
		return err
	}
	mergeSHA, err := s.up.merge(ctx, repo.Repo, number, run.SHA, run.Method)
	if err != nil {
		run.Reason = "merge request failed or response was lost: " + err.Error() + "; verify GitHub before taking action"
	} else {
		run.State = "merged"
		run.Reason = ""
		run.MergeSHA = mergeSHA
	}
	_, saveErr := s.updateAutoMerge(repo, number, run, false)
	return saveErr
}
