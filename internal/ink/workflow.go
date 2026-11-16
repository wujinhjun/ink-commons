package ink

import (
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
func (w *Work) refreshWorkflow(now time.Time, c Reliability) {
	for i := range w.Alerts {
		w.Alerts[i].Active = false
	}
	w.Workflow = Workflow{State: w.Phase}
	for _, run := range w.AutoMerges {
		if run.State == "unknown" {
			w.raiseAlert("auto_merge_unknown", run.SHA, "自动合入结果未确认（SHA "+run.SHA+"）："+run.Reason, now)
		}
		if w.Phase == "approved" && run.SHA == w.Pull.Head.SHA && run.State == "waiting" {
			w.Workflow = Workflow{State: "waiting_auto_merge", Reason: run.Reason}
		}
	}
	for _, n := range w.Notifications {
		if (w.Phase == "approved" || w.Phase == "merged") && n.SHA == w.Pull.Head.SHA && (n.State == "awaiting_ack" || n.State == "unknown" || n.State == "failed" || n.State == "unsupported") {
			w.raiseAlert("approval_notification_unresolved", n.EventID, "审查已通过，但开发会话通知尚未确认接收（"+n.State+"）。"+n.Error, now)
		}
	}
	t := w.Task
	for _, task := range w.executionTasks() {
		for _, a := range task.Runs {
			if !execution.Terminal(a.State) && (a.Legacy || a.Error != "" || c.expired(a, now)) {
				w.raiseAlert("execution_unresolved", a.ID, "旧执行尚未确认结束，后续任务已保留并等待处理。轮次 "+a.ID+"："+a.Error, now)
			}
		}
	}
	if t != nil {
		if len(t.Runs) == 0 && strings.HasPrefix(t.Error, "review preparation:") {
			w.raiseAlert("review_preparation_invalid", t.ID, "审查上下文或会话准备配置无效，任务未启动："+t.Error, now)
		}
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
			case "running":
				w.Workflow.State = "executing"
			case "accepted":
				w.Workflow.State = "waiting_start"
			case "unknown":
				w.Workflow.State = "execution_unknown"
			case "succeeded":
				w.Workflow.State = "waiting_review_result"
				message := "Agent 已结束，但尚未收到指定 reviewer 对当前 SHA 的正式审查结论。"
				if t.Role == "developer" {
					w.Workflow.State = "waiting_new_sha"
					message = "开发 Agent 已结束，但 PR head 尚未更新；请报告修复提交或阻塞。"
				}
				if now.Sub(last.EndedAt) >= time.Duration(c.ResultSeconds)*time.Second {
					w.raiseAlert("workflow_result_missing", last.ID, message, now)

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
	w.refreshHandoffWorkflow(now, c)
	for _, a := range w.Alerts {
		if a.Active {
			w.Workflow.State = "needs_attention"
			w.Workflow.Reason = a.Message
			break
		}
	}
}
