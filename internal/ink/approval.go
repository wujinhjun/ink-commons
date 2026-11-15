package ink

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// ApprovalHook is trusted local configuration, never a command taken from PR contents.
// Arguments are passed directly to exec, without shell interpolation.
type ApprovalHook struct {
	Command        []string `json:"command"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
}

func (h *ApprovalHook) Validate() error {
	if h == nil {
		return nil
	}
	if len(h.Command) == 0 || !filepath.IsAbs(h.Command[0]) {
		return fmt.Errorf("postApproval.command requires an absolute executable path")
	}
	if h.TimeoutSeconds < 0 || h.TimeoutSeconds > 3600 {
		return fmt.Errorf("postApproval.timeout_seconds must be between 0 and 3600")
	}
	return nil
}

type ApprovalEvent struct {
	ID         string `json:"event_id"`
	Event      string `json:"event"`
	Repository string `json:"repository"`
	PullNumber int    `json:"pull_number"`
	PullURL    string `json:"pull_url"`
	SHA        string `json:"sha"`
	ReviewID   int64  `json:"review_id"`
	ReviewURL  string `json:"review_url"`
	Reviewer   string `json:"reviewer"`
}

type ApprovalRun struct {
	ApprovalEvent
	State      string    `json:"state"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	Error      string    `json:"error,omitempty"`
}

func (s *Service) postApproval(ctx context.Context, repo Repository, number int) error {
	if repo.PostApproval == nil || ctx.Err() != nil {
		return nil
	}
	s.mu.Lock()
	st, err := s.store.load()
	if err != nil {
		s.mu.Unlock()
		return err
	}
	work := st.Work[key(repo.Repo, number)]
	if work == nil || work.Phase != "approved" {
		s.mu.Unlock()
		return nil
	}
	sha := work.Pull.Head.SHA
	for _, run := range work.Approvals {
		if run.SHA == sha {
			s.mu.Unlock()
			return nil
		}
	}
	var review Review
	for _, r := range work.Reviews {
		if r.SHA == sha && strings.EqualFold(r.User.Login, work.Reviewer.GitHubLogin) && (r.State == "APPROVED" || r.State == "CHANGES_REQUESTED") {
			review = r
		}
	}
	if review.State != "APPROVED" {
		s.mu.Unlock()
		return nil
	}
	event := ApprovalEvent{ID: fmt.Sprintf("%s:%s", key(repo.Repo, number), sha), Event: "postApproval", Repository: repo.Repo, PullNumber: number, PullURL: work.Pull.URL, SHA: sha, ReviewID: review.ID, ReviewURL: review.URL, Reviewer: review.User.Login}
	// Reserve durably BEFORE exec. Interrupted or failed hooks are not automatically
	// replayed: a hook may already have caused an external side effect.
	work.Approvals = append(work.Approvals, ApprovalRun{ApprovalEvent: event, State: "unknown", StartedAt: time.Now()})
	err = s.store.save(st)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	timeout := repo.PostApproval.TimeoutSeconds
	if timeout == 0 {
		timeout = 60
	}
	hookCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	command := repo.PostApproval.Command
	cmd := exec.CommandContext(hookCtx, command[0], command[1:]...)
	cmd.Stdin = bytes.NewReader(append(payload, '\n'))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	runErr := cmd.Run()
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err = s.store.load()
	if err != nil {
		return err
	}
	work = st.Work[key(repo.Repo, number)]
	if work == nil {
		return nil
	}
	for i := range work.Approvals {
		run := &work.Approvals[i]
		if run.ID != event.ID {
			continue
		}
		run.FinishedAt = time.Now()
		run.State = "succeeded"
		if runErr != nil {
			run.State = "failed"
			run.Error = "hook exited unsuccessfully or could not start"
			if hookCtx.Err() != nil {
				run.Error = "hook timed out or was cancelled; external outcome may be unknown"
			}
		}
	}
	return s.store.save(st)
}
