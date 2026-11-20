package ink

import (
	"context"
	"strings"
	"testing"
	"time"
)

func approvedConflictFixture(t *testing.T) *handoffFixture {
	t.Helper()
	f := newHandoffFixture(t)
	f.repo.AutoMerge = &AutoMergeConfig{Enabled: true}
	setRepositoryConfigForTest(t, f.s, f.repo)
	f.pull.Base = PullBase{Ref: "main", SHA: strings.Repeat("c", 40)}
	mergeable := false
	f.pull.Mergeable, f.pull.MergeableState = &mergeable, "dirty"
	reviewTask := f.work(t).Task
	f.claim(t, reviewTask, claimA, 200)
	f.reviews = []Review{reviewFor(7, f.pull.Head.SHA, "reviewer", "APPROVED")}
	f.observe(t)
	f.submit(t, reviewTask, claimA, HandoffSubmission{RequestID: "approved", Handoff: true, ReviewID: 7, Outcome: "APPROVED"}, 200)
	f.verify(t)
	return f
}

func preflightConflict(t *testing.T, f *handoffFixture) {
	t.Helper()
	if err := f.s.autoMerge(context.Background(), f.repo, f.pull.Number); err != nil {
		t.Fatal(err)
	}
}

func confirmConflict(t *testing.T, f *handoffFixture) *Task {
	t.Helper()
	preflightConflict(t, f)
	preflightConflict(t, f)
	st, err := f.s.store.load()
	if err != nil {
		t.Fatal(err)
	}
	w := st.Work[key(f.repo.Repo, f.pull.Number)]
	if w.Task != nil || w.MergeConflict == nil {
		t.Fatal("conflict must wait for confirmation")
	}
	w.MergeConflict.Since = time.Now().Add(-mergeConflictConfirmation)
	if err := f.s.store.save(st); err != nil {
		t.Fatal(err)
	}
	f.restart(t)
	preflightConflict(t, f)
	task := f.work(t).Task
	if task == nil || task.Role != "developer" || task.Handoff == nil || task.ConflictBase == nil || *task.ConflictBase != f.pull.Base {
		t.Fatalf("wrong conflict task: %+v", task)
	}
	return task
}

func TestMergeConflictTaskReusesDeveloperHandoff(t *testing.T) {
	f := approvedConflictFixture(t)
	task := confirmConflict(t, f)
	f.githubError = true
	preflightConflict(t, f)
	if w := f.work(t); w.Task == nil || w.Task.ID != task.ID {
		t.Fatal("failed PR read replaced the active conflict task")
	}
	f.githubError = false
	f.observe(t)
	f.restart(t)
	preflightConflict(t, f)
	if w := f.work(t); w.Task == nil || w.Task.ID != task.ID || w.Phase != "approved" {
		t.Fatal("poll or restart replaced the active conflict task")
	}
	if err := f.s.dispatch(context.Background(), f.repo, f.pull.Number); err != nil {
		t.Fatal(err)
	}
	if len(f.executor.wakes) != 1 || f.executor.wakes[0].route.Session != "dev-session" {
		t.Fatalf("conflict task was not routed to the developer: %+v", f.executor.wakes)
	}
	claim := f.claim(t, task, claimB, 200)
	delivery := claim["delivery"].(map[string]any)
	if delivery["conflict_base_ref"] != f.pull.Base.Ref || delivery["conflict_base_sha"] != f.pull.Base.SHA {
		t.Fatalf("claim missing conflict base: %+v", delivery)
	}
	f.pull.Head.SHA = strings.Repeat("b", 40)
	f.observe(t)
	reviewTask := f.work(t).Task
	if reviewTask == nil || reviewTask.Role != "reviewer" || !f.work(t).executionBlocked() {
		t.Fatal("new head should wait for the conflict task handoff")
	}
	f.submit(t, task, claimB, HandoffSubmission{RequestID: "resolved", Handoff: true, NewSHA: f.pull.Head.SHA}, 200)
	f.verify(t)
	if w := f.work(t); w.Task.ID != reviewTask.ID || w.executionBlocked() {
		t.Fatal("verified handoff should release the new head for review")
	}
}

func TestMergeConflictTaskClearsOnPositiveMergeabilityOrChangedBase(t *testing.T) {
	for _, scenario := range []string{"pending and clean", "blocked and clean", "blocked and new base"} {
		t.Run(scenario, func(t *testing.T) {
			f := approvedConflictFixture(t)
			task := confirmConflict(t, f)
			if strings.HasPrefix(scenario, "blocked") {
				f.claim(t, task, claimB, 200)
				f.submit(t, task, claimB, HandoffSubmission{RequestID: "no-conflict", Handoff: true, Outcome: "blocked", Reason: "local merge succeeds"}, 200)
				preflightConflict(t, f)
				if w := f.work(t); w.Task.ID != task.ID || mergeStatus(w, f.repo, task.SHA).State != "blocked" {
					t.Fatal("persistent conflict replaced the blocked task")
				}
			}
			if scenario == "blocked and new base" {
				f.pull.Base.SHA = strings.Repeat("d", 40)
				f.observe(t)
				preflightConflict(t, f)
			} else {
				mergeable := true
				f.pull.Mergeable, f.pull.MergeableState = &mergeable, "unstable"
				preflightConflict(t, f)
			}
			if w := f.work(t); w.Task != nil || w.executionBlocked() {
				t.Fatal("obsolete conflict task still blocks the PR")
			}
			if scenario == "pending and clean" {
				f.claim(t, task, claimB, 409)
			}
		})
	}
}

func TestMergeConflictConfirmationRestartsAfterFailedPRRead(t *testing.T) {
	f := approvedConflictFixture(t)
	preflightConflict(t, f)
	st, err := f.s.store.load()
	if err != nil {
		t.Fatal(err)
	}
	w := st.Work[key(f.repo.Repo, f.pull.Number)]
	if w.MergeConflict == nil {
		t.Fatal("first dirty response should begin confirmation")
	}
	w.MergeConflict.Since = time.Now().Add(-mergeConflictConfirmation)
	if err := f.s.store.save(st); err != nil {
		t.Fatal(err)
	}

	f.githubError = true
	preflightConflict(t, f)
	if f.work(t).MergeConflict != nil {
		t.Fatal("failed PR read retained the confirmation")
	}
	f.githubError = false
	preflightConflict(t, f)
	if w := f.work(t); w.Task != nil || w.MergeConflict == nil {
		t.Fatal("first dirty response after recovery should restart confirmation")
	}
}
