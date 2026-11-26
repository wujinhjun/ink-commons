//go:build linux || darwin

package ink

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/wujinhjun/ink-commons/internal/execution"
)

type Service struct {
	now                func() time.Time
	log                *slog.Logger
	pollMu             sync.Mutex
	pullLocks          pullLocks
	hooks              approvalQueue
	store              *stateStore
	workspaceMu        sync.Mutex
	workspaceRoot      string
	fullWorkspaceSlots int
	config             Config
	up                 upstream
	mu                 sync.Mutex
	lastPoll           atomic.Int64
	delivering         map[string]bool
	providers          map[string]execution.Executor
}

func NewService(config Config, path string, providers map[string]execution.Executor) (*Service, error) {
	logger, err := newLogger(os.Stderr, config.LogLevel, config.LogFormat)
	if err != nil {
		return nil, err
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.WorkspaceFullSlots < 0 || config.WorkspaceFullSlots > 32 {
		return nil, fmt.Errorf("workspace_full_slots must be between 0 and 32")
	}
	if config.WorkspaceFullSlots == 0 {
		config.WorkspaceFullSlots = 2
	}
	for _, repo := range append([]Repository{config.RepositoryDefaults}, config.Repositories...) {
		if err := repo.ValidateReviewFeatures(); err != nil {
			return nil, err
		}
	}
	if err := config.Reliability.Validate(); err != nil {
		return nil, err
	}
	for _, repo := range append([]Repository{config.RepositoryDefaults}, config.Repositories...) {
		if err := repo.AutoMerge.Validate(); err != nil {
			return nil, err
		}
	}
	up, err := configuredUpstream(config)
	if err != nil {
		return nil, err
	}
	store, err := openStore(path)
	if err != nil {
		return nil, err
	}
	if err := store.initializeConfig(config); err != nil {
		store.db.Close()
		return nil, err
	}
	stateDir := filepath.Dir(path)
	if resolved, err := filepath.EvalSymlinks(stateDir); err == nil {
		stateDir = resolved
	}
	if absolute, err := filepath.Abs(stateDir); err == nil {
		stateDir = absolute
	}
	return &Service{now: config.Now, log: logger, store: store, config: config, up: up, providers: providers, delivering: make(map[string]bool),
		workspaceRoot: filepath.Join(stateDir, "workspaces"), fullWorkspaceSlots: config.WorkspaceFullSlots}, nil
}

func (s *Service) Close() error {
	s.stopApprovals()
	s.pollMu.Lock()
	defer s.pollMu.Unlock()
	return s.store.db.Close()
}

// Caller holds s.mu. All repository policy is resolved from central SQLite.
func (s *Service) repo(name string) (Repository, bool) {
	cfg, err := s.store.effectiveConfig(name)
	return cfg.Repository, err == nil
}

func (s *Service) repositories() []Repository {
	s.mu.Lock()
	defer s.mu.Unlock()
	repos, err := s.runtimeRepositories()
	if err != nil {
		s.warnError("load repository configuration", err)
	}
	return repos
}

func (s *Service) recordPushes(pushes []Push, now time.Time) error {
	records, err := s.store.configRecords()
	if err != nil {
		return err
	}
	registered := recordMap(records)
	accepted := make([]Push, 0, len(pushes))
	for _, p := range pushes {
		if _, ok := registered[p.Repo]; ok && p.Repo != "" {
			p.At = now
			accepted = append(accepted, p)
		}
	}
	return s.store.savePushes(accepted, now)
}

func key(repo string, number int) string { return fmt.Sprintf("%s#%d", repo, number) }

func (s *Service) observe(repo Repository, pull Pull, reviews []Review) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	work, err := s.store.loadWork(key(repo.Repo, pull.Number))
	if err != nil {
		return err
	}
	return s.saveObservation(repo, pull, reviews, work)
}

// Caller holds s.mu.
func (s *Service) saveObservation(repo Repository, pull Pull, reviews []Review, work *Work) error {
	if work == nil {
		work = &Work{Repo: repo.Repo}
	}
	work.Version++
	work.Pull = pull
	// Re-read under the state lock; the poll list may predate a config update.
	latest, err := s.store.effectiveConfig(repo.Repo)
	if err != nil {
		return err
	}
	repo = latest.Repository
	s.refreshPendingConfig(work, repo)
	if pull.State == "open" && !work.Linked {
		origin, err := s.store.matchOrigin(pull, s.now())
		if err != nil {
			return err
		}
		if origin != nil {
			work.Origin, work.Linked, work.Developer.Route = origin, true, origin
		}
	}
	if pull.State == "open" && !pull.Draft {
		work.Reviews = mergeReviews(work.Reviews, reviews)
	}
	work.derive(s.now())
	if work.Task != nil && work.Task.Delivery == "manual" && !taskStarted(work.Task) {
		if err := s.freezeTaskConfig(work); err != nil {
			return err
		}
	}
	s.refreshWorkflow(work, s.now())
	return s.store.saveWork(work)
}

func mergeReviews(previous, current []Review) []Review {
	seen := make(map[int64]bool, len(current))
	for _, review := range current {
		seen[review.ID] = true
	}
	for _, review := range previous {
		if !seen[review.ID] {
			review.State = "REMOVED"
			current = append(current, review)
		}
	}
	sort.SliceStable(current, func(i, j int) bool {
		if !current[i].SubmittedAt.Equal(current[j].SubmittedAt) {
			return current[i].SubmittedAt.Before(current[j].SubmittedAt)
		}
		return current[i].ID < current[j].ID
	})
	return current
}

func (s *Service) beginDelivery(ctx context.Context, id string, channelCheck reviewChannelCheck) (*Work, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	work, err := s.store.loadWork(id)
	if err != nil {
		return nil, err
	}
	if !matchesObservation(ctx, work) {
		return nil, nil
	}
	if work != nil {
		if err := s.preparePendingConfig(work); err != nil {
			return nil, err
		}
	}
	if work == nil || work.Task == nil || work.Task.Handoff != nil || work.Task.Delivery != "pending" || s.delivering[id] || work.executionBlocked() || s.now().Before(work.Task.RetryAt) {
		return nil, nil
	}
	// Missing provenance is a local configuration problem, not an unknown
	// external execution. Reject it before reserving an attempt or session.
	if err := s.validateReviewPreparation(work); err != nil {
		work.Task.PreparationInvalid = true
		work.Task.Error = "review preparation: " + err.Error()
		s.refreshWorkflow(work, s.now())
		return nil, s.store.saveWork(work)
	}

	if current, err := channelCheck.result(s, work); !current || err != nil {
		if err != nil {
			work.Task.PreparationInvalid = true
			work.Task.Error = "review preparation: " + err.Error()
			s.refreshWorkflow(work, s.now())
			return nil, s.store.saveWork(work)
		}
		return nil, nil
	}

	route := taskRoute(work, work.Task)
	if route == nil || !route.Valid(false) {
		return nil, fmt.Errorf("pending task has no resolved execution route")
	}
	provider := s.providers[route.Provider]
	if !execution.Supports(provider, "deliver", provider != nil) {
		return nil, fmt.Errorf("execution provider does not support deliver")
	}
	if err := s.freezeTaskConfig(work); err != nil {
		return nil, err
	}

	work.Task.PreparationInvalid = false
	// Persist the attempt before making external requests. If the daemon restarts
	// during delivery, the operator can inspect the unknown outcome before retrying.
	work.Task.Delivery, work.Task.Error = "unknown", "delivery interrupted or in progress"
	work.Task.Attempts++
	if work.Task.ID == "" {
		work.Task.ID = uuid.NewString()
	}
	attemptID := uuid.NewString()
	work.Task.RetryAt = time.Time{}
	work.Task.Runs = append(work.Task.Runs, Attempt{ID: attemptID, Number: work.Task.Attempts, Route: *route, Request: deliveryFor(work, work.Task, attemptID, work.Task.Attempts), State: "unknown", CreatedAt: s.now()})
	if err := s.store.saveWork(work); err != nil {
		return nil, err
	}
	s.delivering[id] = true
	return work, nil
}

func (s *Service) saveDelivery(id string, work *Work) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Reload so hooks and operations on other PRs can save while delivery runs.
	live, err := s.store.loadWork(id)
	if err != nil {
		return err
	}
	if live == nil {
		return nil
	}
	for _, t := range live.executionTasks() {
		if t.ID != work.Task.ID {
			continue
		}
		t.Runs = work.Task.Runs
		if live.Task == t {
			t.Delivery, t.Error = work.Task.Delivery, work.Task.Error
			if route := taskRoute(work, work.Task); route != nil {
				copyRoute := *route
				if t.Role == "reviewer" {
					live.Reviewer.Route = &copyRoute
				} else {
					live.Developer.Route = &copyRoute
				}
			}
		}
	}
	s.refreshWorkflow(live, s.now())
	return s.store.saveWork(live)
}

func (s *Service) dispatch(ctx context.Context, repo Repository, number int) error {
	if ctx.Err() != nil {
		return nil
	}
	id := key(repo.Repo, number)
	channelCheck, err := s.checkReviewChannel(ctx, id)
	if err != nil {
		return err
	}
	if _, guarded := ctx.Value(observationKey{}).(prObservation); guarded {
		live, err := s.store.loadWork(id)
		if err != nil {
			return err
		}
		if !matchesObservation(ctx, live) || live.Task == nil || live.Task.Delivery != "pending" {
			return nil
		}
		if current, err := s.observedHeadCurrent(ctx); err != nil || !current {
			return err
		}
	}
	if handled, err := s.dispatchHandoff(ctx, id, channelCheck); handled || err != nil {
		return err
	}
	ready, err := s.prepareDirect(ctx, id, channelCheck)
	if err != nil || !ready {
		return err
	}
	work, err := s.beginDelivery(ctx, id, channelCheck)
	if err != nil || work == nil {
		return err
	}
	defer func() {
		s.mu.Lock()
		delete(s.delivering, id)
		s.mu.Unlock()
	}()
	task := work.Task
	route := taskRoute(work, task)
	executor := s.providers[route.Provider]
	attempt := &task.Runs[len(task.Runs)-1]
	delivery := attempt.Request
	// Do not deliver a task superseded while its session was being allocated.
	s.mu.Lock()
	live, loadErr := s.store.loadWork(id)
	current := loadErr == nil && live != nil && live.Task != nil && live.Task.ID == task.ID && matchesObservation(ctx, live)
	s.mu.Unlock()
	if loadErr != nil {
		return loadErr
	}
	if !current {
		return nil
	}
	if err := executor.Deliver(ctx, *route, delivery); err != nil {
		task.Error = "deliver task: " + err.Error()
		attempt.Error = task.Error
	} else {
		task.Delivery, task.Error = "accepted", ""
		attempt.State = "accepted"
	}
	return s.saveDelivery(id, work)
}

func reviewSource(work *Work) *execution.Route {
	if work.Developer.Route != nil {
		return work.Developer.Route
	}
	return work.Origin
}
