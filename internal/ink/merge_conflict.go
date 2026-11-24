//go:build linux || darwin

package ink

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const mergeConflictConfirmation = 30 * time.Second

type MergeConflict struct {
	Base         PullBase  `json:"base"`
	ReportedBase *PullBase `json:"reported_base,omitempty"`
	HeadSHA      string    `json:"head_sha"`
	ReviewID     int64     `json:"review_id"`
	Since        time.Time `json:"since"`
}

// GitHub's PR snapshot can retain the original base oid. Use the current ref
// target and mergeability from one query, scoped to the already observed PR.
func (u upstream) verifyConflictBase(ctx context.Context, repo string, pull Pull, dirty bool) (PullBase, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || !ValidRepository(repo) || pull.Number <= 0 {
		return PullBase{}, fmt.Errorf("invalid conflict lookup target")
	}
	const query = `query($owner: String!, $name: String!, $number: Int!) {
		repository(owner: $owner, name: $name) {
			pullRequest(number: $number) {
				state isDraft headRefOid baseRefName mergeable mergeStateStatus
				baseRef { target { oid } }
			}
		}
	}`
	body, _ := json.Marshal(map[string]any{"query": query,
		"variables": map[string]any{"owner": owner, "name": name, "number": pull.Number}})
	var response struct {
		Errors []json.RawMessage
		Data   struct {
			Repository struct {
				PullRequest struct {
					State, HeadRefOid, BaseRefName, Mergeable, MergeStateStatus string
					IsDraft                                                     bool
					BaseRef                                                     struct{ Target struct{ OID string } }
				}
			}
		}
	}
	if _, _, err := u.gitHubScopedJSON(ctx, repo, http.MethodPost, "/graphql", "inspect", "", body, &response); err != nil {
		return PullBase{}, err
	}
	result := response.Data.Repository.PullRequest
	base := PullBase{Ref: result.BaseRefName, SHA: result.BaseRef.Target.OID}
	if len(response.Errors) != 0 || result.State != "OPEN" || result.IsDraft ||
		result.HeadRefOid != pull.Head.SHA || base.Ref != pull.Base.Ref || !validSHA(base.SHA) {
		return PullBase{}, fmt.Errorf("GitHub has not confirmed the current PR refs")
	}
	if dirty {
		if result.Mergeable != "CONFLICTING" || result.MergeStateStatus != "DIRTY" {
			return PullBase{}, fmt.Errorf("GitHub has not confirmed a conflict for the current PR refs")
		}
	} else if base != pull.Base || result.Mergeable != "MERGEABLE" || result.MergeStateStatus == "DIRTY" {
		return PullBase{}, fmt.Errorf("GitHub has not confirmed mergeability for the reported base")
	}
	return base, nil
}

func (w *Work) conflictBase() PullBase {
	if observed := w.MergeConflict; observed != nil && observed.HeadSHA == w.Pull.Head.SHA &&
		observed.ReportedBase != nil && *observed.ReportedBase == w.Pull.Base {
		return observed.Base
	}
	return w.Pull.Base
}

func (w *Work) matchesConflictTask(t *Task, review *Review) bool {
	return w.Linked && w.Pull.State == "open" && !w.Pull.Draft &&
		review != nil && t.ReviewID == review.ID &&
		t.SHA == w.Pull.Head.SHA && *t.ConflictBase == w.conflictBase()
}

func (w *Work) retireStaleConflictTasks() {
	review := approvedReview(w.Reviews, w.Pull.Head.SHA, w.Reviewer.GitHubLogin)
	// A blocked submission has relinquished ownership. A claimed or submitted
	// task still owns its work and must go through the normal handoff.
	for _, t := range w.executionTasks() {
		if t.ConflictBase != nil && t.Handoff != nil && t.Handoff.State == "blocked" && !w.matchesConflictTask(t, review) {
			t.Handoff.State = "superseded"
		}
	}
	if c := w.MergeConflict; c != nil && (!w.Linked || w.Pull.State != "open" || w.Pull.Draft ||
		review == nil || review.ID != c.ReviewID || w.Pull.Head.SHA != c.HeadSHA || w.conflictBase() != c.Base) {
		w.MergeConflict = nil
	}
}

func (w *Work) blockedConflictTask() bool {
	t := w.Task
	return t != nil && t.ConflictBase != nil && t.Handoff != nil && t.Handoff.State == "blocked"
}

// The stored list observation fences concurrent changes to the PR version,
// head, and target branch. base is the verified live ref when a GraphQL
// lookup was needed. Its SHA may differ from a stale REST snapshot.
func (s *Service) observeMergeConflict(repoName string, number int, version uint64, pull Pull, base PullBase, dirty bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.store.load()
	if err != nil {
		return err
	}
	w := st.Work[key(repoName, number)]
	repo, ok := s.repo(repoName)
	if w == nil || w.Version != version || w.Phase != "approved" || !ok || (repo.AutoMerge == nil || !repo.AutoMerge.Enabled) ||
		w.Pull.Head.SHA != pull.Head.SHA || w.Pull.Base.Ref != pull.Base.Ref || hasInFlightAutoMerge(w) {
		return nil
	}

	now := time.Now()
	if !dirty {
		mergeable := pull.Mergeable != nil && *pull.Mergeable
		t := w.Task
		// Keep the verified base mapping for an issued task when a check is
		// inconclusive. A clean result for a stale SHA cannot retire that task.
		if mergeable && pull.Base != w.conflictBase() && (base.Ref == "" || base != pull.Base) {
			return nil
		}
		changed := w.MergeConflict != nil
		if t != nil && t.ConflictBase != nil {
			if !mergeable || t.Handoff == nil || (t.Handoff.State != "pending" && t.Handoff.State != "blocked") {
				return nil
			}
			t.Handoff.State = "superseded"
			w.replaceTask(nil)
			changed = true
		}
		w.MergeConflict = nil
		if !changed {
			return nil
		}
	} else {
		if w.TaskMode != "cooperative" || repo.TaskMode != "cooperative" ||
			base.Ref == "" || base.Ref != pull.Base.Ref || !validSHA(base.SHA) {
			return nil
		}
		review := approvedReview(w.Reviews, w.Pull.Head.SHA, w.Reviewer.GitHubLogin)
		if review == nil {
			return nil
		}
		if t := w.Task; t != nil && t.ConflictBase != nil && *t.ConflictBase != base && t.Handoff != nil &&
			(t.Handoff.State == "pending" || t.Handoff.State == "blocked") {
			t.Handoff.State = "superseded"
			w.replaceTask(nil)
		}
		if w.Task != nil || w.handoffMergeBlocked() {
			return nil
		}
		c := w.MergeConflict
		if c == nil || c.HeadSHA != pull.Head.SHA || c.Base != base || c.ReviewID != review.ID {
			w.MergeConflict = &MergeConflict{Base: base, HeadSHA: pull.Head.SHA, ReviewID: review.ID, Since: now}
			if reported := w.Pull.Base; base != reported {
				w.MergeConflict.ReportedBase = &reported
			}
		} else {
			if now.Sub(c.Since) < mergeConflictConfirmation {
				return nil
			}
			w.setTask(Task{Role: "developer", SHA: pull.Head.SHA, ReviewID: review.ID, ReviewURL: review.URL, ConflictBase: &base}, w.Developer)
			if err := s.freezeTaskConfig(w); err != nil {
				return err
			}
		}
	}
	w.refreshWorkflow(now, s.config.Reliability.defaults())
	return s.store.save(st)
}
