package ink

import (
	"encoding/json"

	"ink-commons/internal/execution"
	"ink-commons/internal/execution/ccconnect"
)

// Task snapshots are persisted before prepare/deliver/wake/claim. Retries keep
// the same policy even if a later config generation changes defaults.
type TaskConfigSnapshot struct {
	EffectiveConfig
	Source    *execution.Route `json:"source,omitempty"`
	Reviewer  Assignee         `json:"reviewer"`
	Developer Assignee         `json:"developer"`
	SHA       string           `json:"sha"`
	Legacy    bool             `json:"legacy,omitempty"`
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
	return t.ConfigSnapshot != nil || len(t.Runs) > 0 || t.Attempts > 0 ||
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
	if w.InheritsConfig == nil {
		managed := false
		w.InheritsConfig = &managed
		w.ReviewerOverride = cloneAssignee(w.Reviewer)
		w.DeveloperOverride = cloneAssignee(w.Developer)
	}
	if w.Task != nil && taskStarted(w.Task) && w.Task.SHA == w.Pull.Head.SHA {
		return
	}
	if *w.InheritsConfig {
		reviewer := Assignee{GitHubLogin: repo.ReviewerLogin, GitHubCommand: repo.ReviewerCommand}
		if repo.ReviewerRoute != nil {
			route := *repo.ReviewerRoute
			if route.Provider == ccconnect.Name && route.Session == "" {
				route.Parent = ""
			}
			reviewer.Route = &route
		}
		if w.ReviewerOverride != nil {
			reviewer = *cloneAssignee(*w.ReviewerOverride)
		}
		if w.RoundConfig == nil || w.RoundConfig.SHA != w.Pull.Head.SHA {
			// A session is runtime state. Reuse only when the configured destination and
			// reviewer identity still match; otherwise prepare a new conversation.
			if reviewer.GitHubLogin == w.Reviewer.GitHubLogin && compatibleRoute(reviewer.Route, w.Reviewer.Route) && reviewer.Route.Session == "" {
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
	} else {
		// Historical work did not record whether an assignment was explicit. Keep
		// its saved identity and session rather than guessing an inheritance link.
		w.ReviewerOverride = cloneAssignee(w.Reviewer)
		w.DeveloperOverride = cloneAssignee(w.Developer)
	}
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

// Deep copy used when migrating snapshots; no pointer can be shared across
// mutable work state, runtime route allocation and historical policy.
func copyTaskSnapshot(snapshot *TaskConfigSnapshot) *TaskConfigSnapshot {
	if snapshot == nil {
		return nil
	}
	var result TaskConfigSnapshot
	_ = json.Unmarshal(jsonValue(snapshot), &result)
	return &result
}

func (s *Service) preparePendingConfig(w *Work) error {
	if w.Task == nil || taskStarted(w.Task) {
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
