package ink

import "time"

const mergeConflictConfirmation = 30 * time.Second

type MergeConflict struct {
	Base     PullBase  `json:"base"`
	HeadSHA  string    `json:"head_sha"`
	ReviewID int64     `json:"review_id"`
	Since    time.Time `json:"since"`
}

func (w *Work) matchesConflictTask(t *Task, review *Review) bool {
	return w.Linked && w.Pull.State == "open" && !w.Pull.Draft &&
		review != nil && t.ReviewID == review.ID &&
		t.SHA == w.Pull.Head.SHA && *t.ConflictBase == w.Pull.Base
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
		review == nil || review.ID != c.ReviewID || w.Pull.Head.SHA != c.HeadSHA || w.Pull.Base != c.Base) {
		w.MergeConflict = nil
	}
}

func (w *Work) blockedConflictTask() bool {
	t := w.Task
	return t != nil && t.ConflictBase != nil && t.Handoff != nil && t.Handoff.State == "blocked"
}

// Only the individual PR endpoint supplies mergeability. The list observation
// supplies the version and commit pair that fence this preflight result.
func (s *Service) observeMergeConflict(repoName string, number int, version uint64, pull Pull, dirty bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.store.load()
	if err != nil {
		return err
	}
	w := st.Work[key(repoName, number)]
	repo, ok := s.repo(repoName)
	if w == nil || w.Version != version || w.Phase != "approved" || !ok || (repo.AutoMerge == nil || !repo.AutoMerge.Enabled) ||
		w.Pull.Head.SHA != pull.Head.SHA || w.Pull.Base != pull.Base || hasInFlightAutoMerge(w) {
		return nil
	}

	now := time.Now()
	if !dirty {
		changed := w.MergeConflict != nil
		w.MergeConflict = nil
		// Unknown mergeability resets confirmation but does not cancel a
		// claimable task. A positive result can retire only unowned work.
		t := w.Task
		if pull.Mergeable != nil && *pull.Mergeable && t != nil && t.ConflictBase != nil && t.Handoff != nil &&
			(t.Handoff.State == "pending" || t.Handoff.State == "blocked") {
			t.Handoff.State = "superseded"
			w.replaceTask(nil)
			changed = true
		}
		if !changed {
			return nil
		}
	} else {
		if w.Task != nil || w.TaskMode != "cooperative" || repo.TaskMode != "cooperative" ||
			pull.Base.Ref == "" || !validSHA(pull.Base.SHA) || w.handoffMergeBlocked() {
			return nil
		}
		review := approvedReview(w.Reviews, pull.Head.SHA, w.Reviewer.GitHubLogin)
		if review == nil {
			return nil
		}
		c := w.MergeConflict
		if c == nil || c.HeadSHA != pull.Head.SHA || c.Base != pull.Base || c.ReviewID != review.ID {
			w.MergeConflict = &MergeConflict{Base: pull.Base, HeadSHA: pull.Head.SHA, ReviewID: review.ID, Since: now}
		} else {
			if now.Sub(c.Since) < mergeConflictConfirmation {
				return nil
			}
			w.setTask(Task{Role: "developer", SHA: pull.Head.SHA, ReviewID: review.ID, ReviewURL: review.URL, ConflictBase: &pull.Base}, w.Developer)
			if err := s.freezeTaskConfig(w); err != nil {
				return err
			}
		}
	}
	w.refreshWorkflow(now, s.config.Reliability.defaults())
	return s.store.save(st)
}
