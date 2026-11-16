package ink

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"ink-commons/internal/execution"
)

// Reliability values are seconds; zero selects a default, including MaxAttempts.
type Reliability struct {
	ResultSeconds    int `json:"result_seconds,omitempty"`
	ACKSeconds       int `json:"ack_seconds,omitempty"`
	HeartbeatSeconds int `json:"heartbeat_seconds,omitempty"`
	ExecutionSeconds int `json:"execution_seconds,omitempty"`
	BackoffSeconds   int `json:"backoff_seconds,omitempty"`
	MaxAttempts      int `json:"max_attempts,omitempty"`
}

func (c Reliability) Validate() error {
	for _, v := range []int{c.ResultSeconds, c.ACKSeconds, c.HeartbeatSeconds, c.ExecutionSeconds, c.BackoffSeconds} {
		if v < 0 || v > 604800 {
			return fmt.Errorf("execution timeouts/backoff must be between 0 and 604800 seconds")
		}
	}
	if c.MaxAttempts < 0 || c.MaxAttempts > 100 {
		return fmt.Errorf("execution max_attempts must be between 0 and 100")
	}
	return nil
}
func (c Reliability) defaults() Reliability {
	if c.ResultSeconds == 0 {
		c.ResultSeconds = 120
	}
	if c.ACKSeconds == 0 {
		c.ACKSeconds = 120
	}
	if c.HeartbeatSeconds == 0 {
		c.HeartbeatSeconds = 180
	}
	if c.ExecutionSeconds == 0 {
		c.ExecutionSeconds = 3600
	}
	if c.BackoffSeconds == 0 {
		c.BackoffSeconds = 30
	}
	if c.MaxAttempts == 0 {
		c.MaxAttempts = 3
	}
	return c
}

func taskRoute(w *Work, t *Task) *execution.Route {
	if t.Role == "reviewer" {
		return w.Reviewer.Route
	}
	return w.Developer.Route
}

// Upgrade old deliveries conservatively: they might already be running and did
// not receive an attempt identity. Only an operator can confirm them stopped.
func migrateExecution(w *Work) bool {
	t := w.Task
	if t == nil || t.Handoff != nil {
		return false
	}
	changed := false
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now()
		changed = true
	}
	if t.ID == "" {
		t.ID = uuid.NewString()
		changed = true
	}
	// The old retry API retained Attempts when resetting Delivery to pending.
	// Any evidence of a previous attempt without a run must remain unresolved,
	// even if an earlier migration has already assigned a task ID.
	if len(t.Runs) == 0 && (t.Attempts > 0 || t.Delivery == "accepted" || t.Delivery == "unknown") {
		if t.Attempts < 1 {
			t.Attempts = 1
		}
		route := taskRoute(w, t)
		a := Attempt{ID: uuid.NewString(), Number: t.Attempts, State: "unknown", CreatedAt: time.Now(), Error: "legacy delivery: manual stop confirmation required", Legacy: true}
		if route != nil {
			a.Route = *route
		}
		a.Request = deliveryFor(w, t, a.ID, a.Number)
		t.Runs = append(t.Runs, a)
		t.Delivery, t.Error = "unknown", a.Error
		changed = true
	}
	return changed
}

func deliveryFor(w *Work, t *Task, attemptID string, number int) execution.Delivery {
	d := execution.Delivery{TaskID: t.ID, AttemptID: attemptID, Attempt: number, Repository: w.Repo, Number: w.Pull.Number, URL: w.Pull.URL, Role: t.Role, SHA: t.SHA, ReviewID: t.ReviewID, ReviewURL: t.ReviewURL}
	assignee := w.Developer
	if t.Role == "reviewer" {
		assignee = w.Reviewer
	}
	// A cooperative owner keeps the identity it claimed, even after the
	// work is reassigned and this task moves into history.
	if t.Handoff != nil {
		assignee = t.Handoff.Assignee
	}
	d.GitHubLogin, d.GitHubCommand = assignee.GitHubLogin, assignee.GitHubCommand
	for _, a := range w.Alerts {
		if a.Active && a.SHA == t.SHA {
			d.Feedback += a.Message + "\n"
		}
	}
	if t.Role == "reviewer" {
		d.Source = reviewSource(w)
		if w.Review != nil {
			d.ReuseDevelopmentThread = w.Review.ReuseDevelopmentThread
			if w.Review.Context.ThreadHistory {
				d.ContextBefore = reviewHistoryBefore(t.CreatedAt)
			}
		}
	}
	return d
}

func validateReceipt(a Attempt, r execution.Receipt, now time.Time) error {
	if r.TaskID != a.Request.TaskID || r.AttemptID != a.ID || r.Sequence <= 0 {
		return fmt.Errorf("receipt identity or sequence mismatch")
	}
	if r.Sequence < a.Receipt.Sequence {
		return fmt.Errorf("stale receipt")
	}
	if r.Sequence == a.Receipt.Sequence {
		if r != a.Receipt {
			return fmt.Errorf("conflicting duplicate receipt")
		}
		return nil
	}
	if execution.Terminal(a.State) {
		return fmt.Errorf("receipt after terminal state")
	}
	switch r.State {
	case "accepted":
		if !r.StartedAt.IsZero() || !r.HeartbeatAt.IsZero() || !r.FinishedAt.IsZero() {
			return fmt.Errorf("accepted is not a start ACK")
		}
		if !a.Receipt.StartedAt.IsZero() {
			return fmt.Errorf("execution cannot return to accepted")
		}
	case "running", "succeeded", "failed":
		if r.State == "running" && !r.FinishedAt.IsZero() {
			return fmt.Errorf("running execution has an end time")
		}
		if r.StartedAt.IsZero() || r.HeartbeatAt.Before(r.StartedAt) {
			return fmt.Errorf("missing start ACK or heartbeat")
		}
	case "stopped":
	default:
		return fmt.Errorf("invalid execution state")
	}
	if !a.Receipt.StartedAt.IsZero() && !r.StartedAt.Equal(a.Receipt.StartedAt) {
		return fmt.Errorf("start ACK changed")
	}
	if r.HeartbeatAt.Before(a.Receipt.HeartbeatAt) {
		return fmt.Errorf("heartbeat moved backwards")
	}
	if execution.Terminal(r.State) && (r.FinishedAt.IsZero() || r.FinishedAt.Before(r.StartedAt) || r.FinishedAt.Before(r.HeartbeatAt)) {
		return fmt.Errorf("missing execution end")
	}
	for _, stamp := range []time.Time{r.StartedAt, r.HeartbeatAt, r.FinishedAt} {
		if stamp.After(now.Add(30 * time.Second)) {
			return fmt.Errorf("receipt timestamp is in the future")
		}
	}
	return nil
}

func (c Reliability) expired(a Attempt, now time.Time) bool {
	if a.Receipt.StartedAt.IsZero() {
		return now.Sub(a.CreatedAt) >= time.Duration(c.ACKSeconds)*time.Second
	}
	return now.Sub(a.Receipt.StartedAt) >= time.Duration(c.ExecutionSeconds)*time.Second || now.Sub(a.Receipt.HeartbeatAt) >= time.Duration(c.HeartbeatSeconds)*time.Second
}

// Reconcile independently of GitHub availability, including retired task versions.
func (s *Service) reconcileExecutions(ctx context.Context) error {
	s.mu.Lock()
	st, err := s.store.load()
	s.mu.Unlock()
	if err != nil {
		return err
	}
	for id := range st.Work {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := s.reconcileWork(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) reconcileWork(ctx context.Context, id string) error {
	s.mu.Lock()
	if s.delivering[id] {
		s.mu.Unlock()
		return nil
	}
	st, err := s.store.load()
	if err != nil {
		s.mu.Unlock()
		return err
	}
	w := st.Work[id]
	if w == nil {
		s.mu.Unlock()
		return nil
	}
	s.delivering[id] = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.delivering, id); s.mu.Unlock() }()
	c := s.config.Reliability.defaults()
	for _, t := range w.executionTasks() {
		for i := range t.Runs {
			a := &t.Runs[i]
			if execution.Terminal(a.State) {
				continue
			}
			now := time.Now()
			expired := c.expired(*a, now)
			retired := w.Task == nil || w.Task.ID != t.ID
			provider := s.providers[a.Route.Provider]
			monitor, ok := provider.(execution.Monitor)
			if capability, exists := provider.(interface{ MonitoringEnabled() bool }); exists && !capability.MonitoringEnabled() {
				ok = false
			}
			if !ok || a.Legacy {
				if retired || expired {
					a.State, a.Error = "unknown", "executor cannot confirm termination; manual stop confirmation required"
				}
				continue
			}
			r, queryErr := monitor.Status(ctx, a.Route, a.Request)
			if queryErr == nil {
				queryErr = validateReceipt(*a, r, time.Now())
			}
			if queryErr == nil {
				a.Receipt, a.State, a.Error = r, r.State, ""
				if execution.Terminal(a.State) {
					a.EndedAt = r.FinishedAt
				}
			} else {
				a.Error = "query execution: " + queryErr.Error()
			}
			expired = c.expired(*a, time.Now())
			if !execution.Terminal(a.State) && (retired || expired) {
				if a.StopRequestedAt.IsZero() {
					a.StopRequestedAt = time.Now()
					a.RecoveryReason = "execution timeout"
					if retired {
						a.RecoveryReason = "task superseded"
					}
				}
				a.State = "unknown"
				a.Error = "execution requires confirmed stop before replacement"
				if err := s.saveExecution(id, w); err != nil {
					return err
				}
				// A stop must persist a tombstone even when delivery/prepare has not yet
				// arrived, so a delayed request cannot resurrect the old attempt.
				r, stopErr := monitor.Stop(ctx, a.Route, a.Request)
				if stopErr == nil {
					stopErr = validateReceipt(*a, r, time.Now())
				}
				if stopErr == nil && execution.Terminal(r.State) {
					a.Receipt, a.State, a.EndedAt, a.Error = r, r.State, r.FinishedAt, ""
				} else if stopErr != nil {
					a.Error = "stop execution: " + stopErr.Error()
				}
			}
		}
		if w.Task != nil && w.Task.ID == t.ID && len(t.Runs) > 0 {
			a := &t.Runs[len(t.Runs)-1]
			if execution.Terminal(a.State) && a.State != "succeeded" && t.Delivery != "pending" && t.RetryAt.IsZero() {
				if t.Attempts < c.MaxAttempts {
					delay := time.Duration(c.BackoffSeconds) * time.Second
					for n := 1; n < t.Attempts && delay < time.Hour; n++ {
						delay *= 2
					}
					if delay > time.Hour {
						delay = time.Hour
					}
					t.RetryAt = a.EndedAt.Add(delay)
					a.RetryAt = t.RetryAt
				} else {
					t.Error = "execution attempts exhausted; manual intervention required"
				}
			}
			if !t.RetryAt.IsZero() && !time.Now().Before(t.RetryAt) && !w.executionBlocked() {
				t.Delivery, t.Error = "pending", ""
			}
		}
	}
	return s.saveExecution(id, w)
}

// Merge only execution fields. GitHub observations can advance business state
// while an executor request is in flight.
func (s *Service) saveExecution(id string, w *Work) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.store.load()
	if err != nil {
		return err
	}
	live := current.Work[id]
	if live == nil {
		return nil
	}
	for _, t := range w.executionTasks() {
		for _, dest := range live.executionTasks() {
			if dest.ID == t.ID {
				dest.Runs = t.Runs
				if live.Task == dest && w.Task == t {
					dest.Delivery, dest.Error, dest.RetryAt = t.Delivery, t.Error, t.RetryAt
				}
			}
		}
	}
	live.refreshWorkflow(time.Now(), s.config.Reliability.defaults())
	return s.store.save(current)
}
