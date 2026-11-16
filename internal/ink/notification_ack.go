package ink

import (
	"net/http"
	"strings"
	"time"
)

// Receipt claims are atomic and durable. A duplicate delivery can enter the
// agent conversation, but only the first claim asks the agent to acknowledge it.
func (s *Service) ackNotification(w http.ResponseWriter, r *http.Request) {
	var input struct {
		EventID string `json:"event_id"`
	}
	if err := decode(w, r, &input); err != nil {
		fail(w, err)
		return
	}
	if strings.TrimSpace(input.EventID) == "" {
		fail(w, apiError{400, "event_id is required"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.store.load()
	if err != nil {
		fail(w, err)
		return
	}
	for _, work := range st.Work {
		for i := range work.Notifications {
			n := &work.Notifications[i]
			if n.EventID != input.EventID {
				continue
			}
			if !n.RequireACK {
				fail(w, apiError{409, "notification does not use agent receipts"})
				return
			}
			first := false
			switch n.State {
			case "acknowledged", "superseded":
			case "unknown", "awaiting_ack":
				n.State = "superseded"
				if (work.Phase == "approved" || work.Phase == "merged") && work.Pull.Head.SHA == n.SHA && work.Developer.Route != nil && n.Route != nil && *work.Developer.Route == *n.Route {
					n.State, first = "acknowledged", true
				}
				// 完成通知已被消费后，不再补发同 SHA 的旧批准提示。
				if first && n.Event == "pr_merged" {
					for j := range work.Notifications {
						old := &work.Notifications[j]
						if old.Event == "review_approved" && old.SHA == n.SHA && (old.State == "unknown" || old.State == "awaiting_ack") {
							old.State = "superseded"
							old.FinishedAt = time.Now()
							old.RetryAt = time.Time{}
						}
					}
				}
				n.FinishedAt, n.RetryAt, n.Error = time.Now(), time.Time{}, ""
				work.refreshWorkflow(time.Now(), s.config.Reliability.defaults())
				if err := s.store.save(st); err != nil {
					fail(w, err)
					return
				}
			default:
				fail(w, apiError{409, "notification is not awaiting receipt"})
				return
			}
			reply(w, 200, map[string]any{"status": n.State, "first": first})
			return
		}
	}
	fail(w, apiError{404, "notification event not found"})
}
