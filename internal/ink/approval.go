//go:build linux || darwin

package ink

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
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
	Hook           *ApprovalHook `json:"hook,omitempty"`
	ConfigRevision int64         `json:"config_revision,omitempty"`
	ApprovalEvent
	State      string    `json:"state"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	Error      string    `json:"error,omitempty"`
}

type approvalCandidate struct {
	event    ApprovalEvent
	version  uint64
	revision int64
	hook     *ApprovalHook
}

// Stage the eligible event before merge. Pending means nothing external has
// started, so it can be reconciled after a restart even if the PR has merged.
func (s *Service) approvalCandidate(ctx context.Context, repo Repository, number int) (*approvalCandidate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	work, err := s.store.loadWork(key(repo.Repo, number))
	if err != nil || work == nil || !matchesObservation(ctx, work) {
		return nil, err
	}
	effective, err := s.store.effectiveConfig(repo.Repo)
	if err != nil {
		return nil, err
	}
	candidate := pendingApproval(work, effective)
	if candidate == nil {
		return nil, s.removePendingApprovals(work)
	}
	before := len(work.Approvals)
	work.Approvals = slices.DeleteFunc(work.Approvals, func(run ApprovalRun) bool {
		return run.State == "pending" && run.ApprovalEvent != candidate.event
	})
	for _, run := range work.Approvals {
		if run.State == "pending" && run.ApprovalEvent == candidate.event {
			if len(work.Approvals) != before {
				return candidate, s.store.saveWork(work)
			}
			return candidate, nil
		}
	}
	work.Approvals = append(work.Approvals, ApprovalRun{ApprovalEvent: candidate.event, State: "pending"})
	if err := s.store.saveWork(work); err != nil {
		return nil, err
	}
	return candidate, nil
}

// A queued hook must match the approval verified by merge preflight. Once
// execution starts, the existing once-per-SHA rule covers subsequent reviews.
func (w *Work) hasApprovalReceipt(sha string, reviewID int64) bool {
	for _, run := range w.Approvals {
		if run.SHA == sha && (run.State != "pending" || run.ReviewID == reviewID) {
			return true
		}
	}
	return false
}

func pendingApproval(work *Work, effective EffectiveConfig) *approvalCandidate {
	if effective.Repository.PostApproval == nil || work.handoffMergeBlocked() {
		return nil
	}
	sha := work.Pull.Head.SHA
	var staged *ApprovalRun
	for i := range work.Approvals {
		run := &work.Approvals[i]
		if run.SHA != sha {
			continue
		}
		if run.State != "pending" {
			return nil
		}
		staged = run
	}
	if work.Phase != "approved" && !(work.Phase == "merged" && staged != nil) {
		return nil
	}
	review := approvedReview(work.Reviews, sha, work.Reviewer.GitHubLogin)
	if review == nil {
		return nil
	}
	repo, number := work.Repo, work.Pull.Number
	event := ApprovalEvent{ID: fmt.Sprintf("%s:%s", key(repo, number), sha), Event: "postApproval", Repository: repo, PullNumber: number, PullURL: work.Pull.URL, SHA: sha, ReviewID: review.ID, ReviewURL: review.URL, Reviewer: review.User.Login}
	return &approvalCandidate{event: event, version: work.Version, revision: effective.Revision, hook: effective.Repository.PostApproval}
}

// Caller holds s.mu. Remove an unstarted candidate so reapproval on the
// same SHA can schedule it again.
func (s *Service) removePendingApprovals(work *Work) error {
	before := len(work.Approvals)
	work.Approvals = slices.DeleteFunc(work.Approvals, func(run ApprovalRun) bool { return run.State == "pending" })
	if len(work.Approvals) != before {
		return s.store.saveWork(work)
	}
	return nil
}

func (s *Service) beginApproval(candidate approvalCandidate) (bool, error) {
	event := candidate.event
	s.mu.Lock()
	defer s.mu.Unlock()
	work, err := s.store.loadWork(key(event.Repository, event.PullNumber))
	if err != nil || work == nil || work.Version != candidate.version {
		return false, err
	}
	effective, err := s.store.effectiveConfig(event.Repository)
	if err != nil {
		return false, err
	}
	current := pendingApproval(work, effective)
	if current == nil || current.event != event || current.revision != candidate.revision {
		return false, nil
	}
	for i, run := range work.Approvals {
		if run.State == "pending" && run.ApprovalEvent == event {
			// Reserve before exec. An unknown or failed run may already have
			// caused an external side effect and is never replayed.
			work.Approvals[i] = ApprovalRun{ApprovalEvent: event, Hook: candidate.hook, ConfigRevision: candidate.revision, State: "unknown", StartedAt: s.now()}
			return true, s.store.saveWork(work)
		}
	}
	return false, nil
}

func (s *Service) runApproval(ctx context.Context, candidate approvalCandidate) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	started, err := s.beginApproval(candidate)
	if err != nil || !started {
		return err
	}
	event := candidate.event
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	timeout := candidate.hook.TimeoutSeconds
	if timeout == 0 {
		timeout = 60
	}
	hookCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	command := candidate.hook.Command
	cmd := exec.CommandContext(hookCtx, command[0], command[1:]...)
	cmd.Stdin = bytes.NewReader(append(payload, '\n'))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	runErr := cmd.Run()
	s.mu.Lock()
	defer s.mu.Unlock()
	work, err := s.store.loadWork(key(event.Repository, event.PullNumber))
	if err != nil {
		return err
	}
	if work == nil {
		return nil
	}
	for i := range work.Approvals {
		run := &work.Approvals[i]
		if run.ID != event.ID || run.State != "unknown" {
			continue
		}
		run.FinishedAt = s.now()
		run.State = "succeeded"
		if runErr != nil {
			run.State = "failed"
			run.Error = "hook exited unsuccessfully or could not start"
			if hookCtx.Err() != nil {
				run.Error = "hook timed out or was cancelled; external outcome may be unknown"
			}
		}
	}
	return s.store.saveWork(work)
}

type approvalJob struct {
	ctx       context.Context
	candidate approvalCandidate
	unlock    func()
}

type approvalQueue struct {
	mu     sync.Mutex
	wg     sync.WaitGroup
	ctx    context.Context
	cancel context.CancelFunc
	jobs   []approvalJob
	active int
	closed bool
}

func (s *Service) queueApproval(ctx context.Context, candidate approvalCandidate, unlock func()) bool {
	q := &s.hooks
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || ctx.Err() != nil {
		return false
	}
	if q.ctx == nil {
		q.ctx, q.cancel = context.WithCancel(context.Background())
	}
	q.jobs = append(q.jobs, approvalJob{ctx: ctx, candidate: candidate, unlock: unlock})
	if q.active < approvalWorkers {
		q.active++
		q.wg.Add(1)
		go s.approvalWorker()
	}
	return true
}

func (s *Service) stopApprovals() {
	q := &s.hooks
	q.mu.Lock()
	q.closed = true
	if q.cancel != nil {
		q.cancel()
	}
	for _, job := range q.jobs {
		job.unlock()
	}
	q.jobs = nil
	q.mu.Unlock()
	q.wg.Wait()
}

func (s *Service) approvalWorker() {
	q := &s.hooks
	defer q.wg.Done()
	for {
		q.mu.Lock()
		if len(q.jobs) == 0 {
			q.active--
			q.mu.Unlock()
			return
		}
		job := q.jobs[0]
		q.jobs[0] = approvalJob{}
		q.jobs = q.jobs[1:]
		q.mu.Unlock()

		func() {
			defer job.unlock()
			ctx, cancel := context.WithCancel(job.ctx)
			defer cancel()
			stop := context.AfterFunc(q.ctx, cancel)
			defer stop()
			if err := s.checkAndRunApproval(ctx, job.candidate); err != nil && ctx.Err() == nil {
				s.log.Warn("process approval hook", "number", job.candidate.event.PullNumber)
			}
		}()
	}
}

func (s *Service) checkAndRunApproval(ctx context.Context, candidate approvalCandidate) error {
	event := candidate.event
	path := fmt.Sprintf("/repos/%s/pulls/%d", event.Repository, event.PullNumber)
	reviews, err := listPages[Review](ctx, s.up, path+"/reviews")
	if err != nil {
		return err
	}
	review := approvedReview(mergeReviews(nil, reviews), event.SHA, event.Reviewer)
	if review == nil || review.ID != event.ReviewID {
		return s.discardApproval(candidate)
	}
	var pull Pull
	if _, err := s.up.gitHubGet(ctx, path, &pull); err != nil {
		return err
	}
	// A matching merged head is eligible because the hook was queued from
	// an approval before merge. A closed unmerged PR invalidates that sample.
	if pull.Number != event.PullNumber || pull.Head.SHA != event.SHA || pull.Draft ||
		(pull.State != "open" && (pull.State != "closed" || !pull.Merged)) {
		return s.discardApproval(candidate)
	}
	return s.runApproval(ctx, candidate)
}

func (s *Service) discardApproval(candidate approvalCandidate) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	event := candidate.event
	work, err := s.store.loadWork(key(event.Repository, event.PullNumber))
	if err != nil || work == nil || work.Version != candidate.version {
		return err
	}
	return s.removePendingApprovals(work)
}
