//go:build linux || darwin

package ink

import (
	"net/http"

	"github.com/wujinhjun/ink-commons/internal/execution"
)

// 只返回当前持久化观测；不调用 GitHub、不触发合入或修改通知回执。
func mergeStatus(w *Work, repo Repository, sha string) *execution.MergeStatus {
	result := &execution.MergeStatus{Enabled: repo.AutoMerge != nil && repo.AutoMerge.Enabled, State: "disabled"}
	if result.Enabled {
		result.Method = repo.AutoMerge.method()
		result.State = "waiting"
	}
	if w.Pull.Head.SHA != sha {
		result.State = "superseded"
		result.Reason = "PR head changed; event applies only to its SHA"
		return result
	}
	if w.Pull.Merged {
		result.State = "merged"
		result.MergeSHA = w.Pull.MergeCommitSHA
		return result
	}
	for _, run := range w.AutoMerges {
		if run.SHA != sha {
			continue
		}
		result.Method = run.Method
		result.Reason = run.Reason
		switch run.State {
		case "merged":
			result.State = "merged"
			result.MergeSHA = run.MergeSHA
			return result
		case "pending":
			result.State = "waiting"
			return result
		case "unknown":
			result.State = "unknown"
			return result
		case "failed":
			result.State = "blocked"
			return result
		case "waiting":
			if result.Enabled {
				result.State = "waiting"
			}
		}
	}
	if w.Pull.State == "closed" {
		result.State = "closed"
		result.Reason = "PR closed without merge"
		return result
	}
	if !result.Enabled {
		return result
	}
	if w.Phase != "approved" {
		result.State = "not_approved"
		result.Reason = "current PR observation is not approved"
		return result
	}
	if hasInFlightAutoMerge(w) {
		result.State = "unknown"
		result.Reason = "an earlier merge outcome is unresolved"
		return result
	}
	if w.Task != nil && w.Task.ConflictBase != nil && w.Task.Handoff != nil {
		result.Reason = "waiting for merge conflict task " + w.Task.ID
		if w.Task.Handoff.State == "blocked" {
			result.State = "blocked"
			result.Reason = w.Task.Handoff.Verification
		}
		return result
	}
	if w.handoffMergeBlocked() {
		result.State = "waiting"
		result.Reason = "waiting for verified reviewer handoff"
	}
	return result
}

func (s *Service) notificationStatus(out http.ResponseWriter, r *http.Request) {
	event := r.URL.Query().Get("event_id")
	if event == "" {
		fail(out, apiError{400, "event_id is required"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.store.load()
	if err != nil {
		fail(out, err)
		return
	}
	for _, w := range st.Work {
		for _, n := range w.Notifications {
			if n.EventID != event {
				continue
			}
			repo, ok := s.repo(w.Repo)
			if !ok {
				fail(out, apiError{409, "repository configuration unavailable"})
				return
			}
			reply(out, 200, map[string]any{"event_id": event, "event": n.Event, "sha": n.SHA, "current_head_sha": w.Pull.Head.SHA, "phase": w.Phase, "notification_state": n.State, "merge": mergeStatus(w, repo, n.SHA)})
			return
		}
	}
	fail(out, apiError{404, "notification event not found"})
}
