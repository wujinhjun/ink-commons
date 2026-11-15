package ink

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log"
	"strings"
	"time"

	"ink-commons/internal/execution"
)

func (w *Work) raiseAlert(code, key, message string, now time.Time) {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s#%d\x00%s\x00%s\x00%s", w.Repo, w.Pull.Number, w.Pull.Head.SHA, code, key)))
	id := fmt.Sprintf("%x", sum[:])
	for i := range w.Alerts {
		if w.Alerts[i].ID == id {
			w.Alerts[i].Active = true
			w.Alerts[i].Message = message
			return
		}
	}
	w.Alerts = append(w.Alerts, Alert{ID: id, Code: code, Message: message, SHA: w.Pull.Head.SHA, Active: true, CreatedAt: now})
	log.Printf("workflow %s: %s: %s", keyForWork(w), code, message)
}
func keyForWork(w *Work) string { return key(w.Repo, w.Pull.Number) }
func retryDelay(c Reliability, attempts int) time.Duration {
	delay := time.Duration(c.BackoffSeconds) * time.Second
	for n := 1; n < attempts && delay < time.Hour; n++ {
		delay *= 2
	}
	if delay > time.Hour {
		delay = time.Hour
	}
	return delay
}
func (w *Work) refreshWorkflow(now time.Time, c Reliability) {
	for i := range w.Alerts {
		w.Alerts[i].Active = false
	}
	w.Workflow = Workflow{State: w.Phase}
	t := w.Task
	if t != nil && (t.Preparation == "reserved" || t.Preparation == "unknown") {
		w.raiseAlert("preparation_unknown", t.ID, "审查 thread 创建结果不明；请核查已有 thread 并用 confirm-prepared 关联，确认前不会再次创建。", now)
	}
	for _, task := range w.executionTasks() {
		for _, a := range task.Runs {
			if !execution.Terminal(a.State) && (a.Legacy || a.Error != "" || c.expired(a, now)) {
				w.raiseAlert("execution_unresolved", a.ID, "旧执行尚未确认结束，后续任务已保留并等待处理。轮次 "+a.ID+"："+a.Error, now)
			}
		}
	}
	if t != nil {
		w.Workflow.State = "waiting_agent"
		if t.Delivery == "manual" {
			w.Workflow.State = "waiting_human"
		}
		if w.Phase == "review_pending" {
			var unexpected, stale *Review
			for i := range w.Reviews {
				r := &w.Reviews[i]
				if r.State != "APPROVED" && r.State != "CHANGES_REQUESTED" {
					continue
				}
				if r.SHA == w.Pull.Head.SHA && !strings.EqualFold(r.User.Login, w.Reviewer.GitHubLogin) {
					unexpected = r
				}
				if r.SHA != w.Pull.Head.SHA && strings.EqualFold(r.User.Login, w.Reviewer.GitHubLogin) && !t.CreatedAt.IsZero() && !r.SubmittedAt.Before(t.CreatedAt) {
					stale = r
				}
			}
			if unexpected != nil {
				w.raiseAlert("review_identity_mismatch", fmt.Sprint(unexpected.ID), fmt.Sprintf("审查未推进状态：期望作者 %s，实际作者 %s。请使用配置的审查身份对当前 SHA 提交正式 review：%s", w.Reviewer.GitHubLogin, unexpected.User.Login, unexpected.URL), now)
			}
			if stale != nil {
				w.raiseAlert("review_sha_mismatch", fmt.Sprint(stale.ID), fmt.Sprintf("审查提交到了旧 SHA %s，当前 SHA 为 %s：%s", stale.SHA, w.Pull.Head.SHA, stale.URL), now)
			}
		}
		if len(t.Runs) > 0 {
			last := &t.Runs[len(t.Runs)-1]
			switch last.State {
			case "queued":
				w.Workflow.State = "queued"
			case "running", "accepted", "unknown":
				w.Workflow.State = "executing"
			case "succeeded":
				w.Workflow.State = "waiting_review_result"
				message := "Agent 已结束，但尚未收到指定 reviewer 对当前 SHA 的正式审查结论。"
				if t.Role == "developer" {
					w.Workflow.State = "waiting_new_sha"
					message = "开发 Agent 已结束，但 PR head 尚未更新；请报告修复提交或阻塞。"
				}
				if now.Sub(last.EndedAt) >= time.Duration(c.ResultSeconds)*time.Second {
					w.raiseAlert("workflow_result_missing", last.ID, message, now)
					if t.Role == "reviewer" && t.Delivery != "pending" && t.RetryAt.IsZero() {
						if t.Attempts < c.MaxAttempts {
							t.RetryAt = now.Add(retryDelay(c, t.Attempts))
							last.RetryAt = t.RetryAt
							t.Error = "expected GitHub review missing; follow-up scheduled"
						} else {
							t.Error = "workflow attempts exhausted; manual intervention required"
						}
					}
				}
			}
		}
		if strings.Contains(t.Error, "attempts exhausted") {
			w.raiseAlert("attempts_exhausted", t.ID, t.Error, now)
		}
	}
	if w.executionBlocked() && (t == nil || len(t.Runs) == 0) {
		w.Workflow.State = "waiting_previous_execution"
	}
	for _, a := range w.Alerts {
		if a.Active {
			w.Workflow.State = "needs_attention"
			w.Workflow.Reason = a.Message
			break
		}
	}
}

func (s *Service) deliverAlerts(ctx context.Context) {
	s.mu.Lock()
	st, err := s.store.load()
	s.mu.Unlock()
	if err != nil {
		return
	}
	for id, w := range st.Work {
		route := reviewSource(w)
		if route == nil {
			continue
		}
		provider := s.providers[route.Provider]
		notifier, ok := provider.(execution.Notifier)
		if capability, exists := provider.(interface{ MonitoringEnabled() bool }); exists && !capability.MonitoringEnabled() {
			ok = false
		}
		for _, a := range w.Alerts {
			if !a.Active || a.NotificationAccepted {
				continue
			}
			message := fmt.Sprintf("同砚 · %s #%d\n%s\n%s", w.Repo, w.Pull.Number, a.Message, w.Pull.URL)
			sendErr := fmt.Errorf("execution provider has no durable notification support")
			if ok {
				sendErr = notifier.Notify(ctx, *route, execution.Notice{ID: a.ID, Message: message})
			}
			s.mu.Lock()
			current, loadErr := s.store.load()
			if loadErr == nil && current.Work[id] != nil {
				for i := range current.Work[id].Alerts {
					dest := &current.Work[id].Alerts[i]
					if dest.ID != a.ID {
						continue
					}
					if sendErr == nil {
						dest.NotificationAccepted = true
						dest.Error = ""
					} else {
						dest.Error = sendErr.Error()
					}
				}
				_ = s.store.save(current)
			}
			s.mu.Unlock()
		}
	}
}
