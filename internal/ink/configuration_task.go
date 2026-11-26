//go:build linux || darwin

package ink

import (
	"encoding/json"

	"github.com/wujinhjun/ink-commons/internal/execution"
)

// Task snapshots are persisted before prepare/deliver/wake/claim. Retries keep
// the same policy even if a later config generation changes defaults.
type TaskConfigSnapshot struct {
	EffectiveConfig
	Source    *execution.Route `json:"source,omitempty"`
	Reviewer  Assignee         `json:"reviewer"`
	Developer Assignee         `json:"developer"`
	SHA       string           `json:"sha"`
}

func cloneAssignee(a Assignee) *Assignee {
	result := a
	if a.Route != nil {
		route := *a.Route
		result.Route = &route
	}
	return &result
}

func taskStarted(t *Task) bool {
	return t.Preparation != nil || t.ConfigSnapshot != nil || len(t.Runs) > 0 || t.Attempts > 0 ||
		(t.Handoff != nil && (t.Handoff.Wake.Attempts > 0 || t.Handoff.State != "pending" || t.Handoff.Wake.Preparation != ""))
}

func (t *Task) reliability(fallback Reliability) Reliability {
	if t != nil && t.ConfigSnapshot != nil {
		return t.ConfigSnapshot.Execution
	}
	return fallback.defaults()
}

func compatibleRoute(a, b *execution.Route) bool {
	return a != nil && b != nil && a.Provider == b.Provider && a.Target == b.Target && a.Parent == b.Parent
}

// Caller holds Service.mu. Preserve the reviewer for an already exposed SHA:
// changing defaults must never reinterpret an existing review as an approval.
func (s *Service) refreshPendingConfig(w *Work, repo Repository) {
	if repo.AutoMerge == nil || !repo.AutoMerge.Enabled || repo.TaskMode != "cooperative" {
		w.MergeConflict = nil
		if t := w.Task; t != nil && t.ConflictBase != nil && t.Handoff.State == "pending" {
			w.replaceTask(nil)
		}
	}
	if w.Task != nil && taskStarted(w.Task) && w.Task.SHA == w.Pull.Head.SHA {
		return
	}
	reviewer := Assignee{GitHubLogin: repo.ReviewerLogin, GitHubCommand: repo.ReviewerCommand}
	if repo.ReviewerRoute != nil {
		route := *repo.ReviewerRoute
		reviewer.Route = &route
	}
	if w.ReviewerOverride != nil {
		reviewer = *cloneAssignee(*w.ReviewerOverride)
	}
	if w.RoundConfig == nil || w.RoundConfig.SHA != w.Pull.Head.SHA {
		// A session is runtime state. Reuse only when the configured destination and
		// reviewer identity still match; otherwise prepare a new conversation.
		prior := w.Reviewer.Route
		if w.RoundConfig != nil && w.RoundConfig.Reviewer.Route != nil {
			// Compare the selected policy before placement filled in a parent.
			prior = w.RoundConfig.Reviewer.Route
		}
		if w.Reviewer.Route != nil && reviewer.GitHubLogin == w.Reviewer.GitHubLogin &&
			compatibleRoute(reviewer.Route, prior) && reviewer.Route.Session == "" {
			reviewer.Route.Session = w.Reviewer.Route.Session
		}
		w.Reviewer = reviewer
	}
	developer := Assignee{GitHubLogin: repo.HumanDeveloper}
	if w.Origin != nil {
		route := *w.Origin
		developer.Route = &route
	}
	if w.DeveloperOverride != nil {
		developer = *cloneAssignee(*w.DeveloperOverride)
	}
	w.Developer = developer
	w.TaskMode = repo.TaskMode
	w.Review = repo.Review
	if t := w.Task; t != nil && !taskStarted(t) {
		owner := w.Developer
		if t.Role == "reviewer" {
			owner = w.Reviewer
		}
		t.Delivery = "manual"
		if owner.Route != nil {
			t.Delivery = "pending"
		}
		if normalTaskMode(w.TaskMode) == "cooperative" {
			if t.Handoff == nil {
				t.Handoff = &TaskHandoff{State: "pending"}
			}
			t.Handoff.Assignee = *cloneAssignee(owner)
		} else {
			t.Handoff = nil
		}
	}
}

func applyPRValues(effective *EffectiveConfig, w *Work) {
	// Sources describe policy origins, not allocated runtime session state.
	if w.ReviewerOverride != nil {
		for key, value := range map[string]any{"reviewer_login": w.ReviewerOverride.GitHubLogin, "reviewer_command": w.ReviewerOverride.GitHubCommand, "reviewer_route": w.ReviewerOverride.Route} {
			effective.Values[key] = jsonValue(value)
			effective.Sources[key] = "pr"
		}
	}
	if w.DeveloperOverride != nil {
		effective.Values["human_developer"] = jsonValue(w.DeveloperOverride.GitHubLogin)
		effective.Sources["human_developer"] = "pr"
	}
}

func (s *Service) freezeTaskConfig(w *Work) error {
	t := w.Task
	if t == nil || t.ConfigSnapshot != nil {
		return nil
	}
	effective, err := s.store.effectiveConfig(w.Repo)
	if err != nil {
		return err
	}
	s.refreshPendingConfig(w, effective.Repository)
	applyPRValues(&effective, w)
	if w.RoundConfig != nil && w.RoundConfig.SHA == t.SHA && w.ReviewerOverride == nil {
		for _, key := range []string{"reviewer_login", "reviewer_command", "reviewer_route"} {
			effective.Values[key] = append(json.RawMessage(nil), w.RoundConfig.Values[key]...)
			effective.Sources[key] = "round_snapshot"
		}
	}
	// Round reviewer remains fixed even if the developer's next task uses a new
	// execution policy. The effective snapshot records the actual selected policy.
	effective.Repository.ReviewerLogin = w.Reviewer.GitHubLogin
	effective.Repository.ReviewerCommand = w.Reviewer.GitHubCommand
	effective.Repository.ReviewerRoute = cloneAssignee(w.Reviewer).Route
	effective.Repository.HumanDeveloper = w.Developer.GitHubLogin
	effective.Repository.Review = w.Review
	effective.Repository.TaskMode = w.TaskMode
	t.ConfigSnapshot = &TaskConfigSnapshot{EffectiveConfig: effective, Reviewer: *cloneAssignee(w.Reviewer), Developer: *cloneAssignee(w.Developer), SHA: t.SHA, Source: cloneRoute(reviewSource(w))}
	if t.Role == "reviewer" {
		w.RoundConfig = copyTaskSnapshot(t.ConfigSnapshot)
	}
	if t.Handoff != nil {
		owner := w.Developer
		if t.Role == "reviewer" {
			owner = w.Reviewer
		}
		t.Handoff.Assignee = *cloneAssignee(owner)
	}
	return nil
}

// Keep the round policy independent of the mutable task snapshot.
func copyTaskSnapshot(snapshot *TaskConfigSnapshot) *TaskConfigSnapshot {
	if snapshot == nil {
		return nil
	}
	var result TaskConfigSnapshot
	_ = json.Unmarshal(jsonValue(snapshot), &result)
	return &result
}

func (s *Service) preparePendingConfig(w *Work) error {
	if w.Task == nil || (taskStarted(w.Task) && w.Task.ConflictBase == nil) {
		return nil
	}
	effective, err := s.store.effectiveConfig(w.Repo)
	if err != nil {
		return err
	}
	s.refreshPendingConfig(w, effective.Repository)
	return nil
}

func cloneRoute(route *execution.Route) *execution.Route {
	if route == nil {
		return nil
	}
	result := *route
	return &result
}
