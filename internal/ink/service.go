//go:build linux || darwin

package ink

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/wujinhjun/ink-commons/internal/execution"
	"github.com/wujinhjun/ink-commons/internal/execution/ccconnect"
	"github.com/wujinhjun/ink-commons/internal/ink/statedb"
)

type Service struct {
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
	store, err := openStore(path)
	if err != nil {
		return nil, err
	}
	if err := store.initializeConfig(config); err != nil {
		store.db.Close()
		return nil, err
	}
	up := newUpstream(config.GitHubToken)
	up.command = config.GitHubCommand
	up.commandDigest = config.GitHubCommandSHA256
	up.readArgs, up.mergeArgs = config.GitHubReadArgs, config.GitHubMergeArgs
	up.mergerCommand, up.mergerDigest = config.GitHubMergerCommand, config.GitHubMergerSHA256
	stateDir := filepath.Dir(path)
	if resolved, err := filepath.EvalSymlinks(stateDir); err == nil {
		stateDir = resolved
	}
	if absolute, err := filepath.Abs(stateDir); err == nil {
		stateDir = absolute
	}
	return &Service{store: store, config: config, up: up, providers: providers, delivering: make(map[string]bool),
		workspaceRoot: filepath.Join(stateDir, "workspaces"), fullWorkspaceSlots: config.WorkspaceFullSlots}, nil
}

func (s *Service) Close() error { return s.store.db.Close() }

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
		log.Printf("load repository configuration: %v", err)
	}
	return repos
}

func (s *Service) recordPushes(pushes []Push, now time.Time) error {
	accepted := make([]Push, 0, len(pushes))
	for _, p := range pushes {
		if _, ok := s.repo(p.Repo); ok {
			p.At = now
			accepted = append(accepted, p)
		}
	}
	return s.store.savePushes(accepted, now)
}

func key(repo string, number int) string { return fmt.Sprintf("%s#%d", repo, number) }

func (s *Service) Poll(ctx context.Context) {
	defer func() { s.lastPoll.Store(time.Now().Unix()) }()
	if err := statedb.New(s.store.db).PrunePushes(ctx, pushCutoff(time.Now())); err != nil {
		log.Printf("prune expired push observations: %v", err)
	}
	if err := s.reconcileBlockedNotices(ctx); err != nil {
		log.Printf("blocked task notifications: %v", err)
	}
	if err := s.reconcileHandoffs(ctx); err != nil {
		log.Printf("handoff verification: %v", err)
	}
	if err := s.reconcileExecutions(ctx); err != nil {
		log.Printf("execution recovery: %v", err)
	}
	for _, repo := range s.repositories() {
		if ctx.Err() != nil {
			return
		}
		if err := s.pollRepo(ctx, repo); err != nil {
			log.Printf("sync %s: %v", repo.Repo, err)
		}
	}
}

func (s *Service) pollRepo(ctx context.Context, repo Repository) error {
	path := "/repos/" + repo.Repo + "/pulls"
	pulls, err := listPages[Pull](ctx, s.up, path+"?state=open")
	if err != nil {
		return err
	}
	s.mu.Lock()
	pending, err := statedb.New(s.store.db).ListPollPending(ctx, repo.Repo)
	s.mu.Unlock()
	if err != nil {
		return err
	}

	open := make(map[int]bool, len(pulls))
	for _, pull := range pulls {
		open[pull.Number] = true
	}
	for _, pendingNumber := range pending {
		number := int(pendingNumber)
		if open[number] {
			continue
		}
		var pull Pull
		_, err := s.up.gitHubGet(ctx, fmt.Sprintf("%s/%d", path, number), &pull)
		if err != nil {
			log.Printf("refresh %s: %v", key(repo.Repo, number), err)
			continue
		}
		pulls = append(pulls, pull)
	}
	for _, pull := range pulls {
		if pull.Number <= 0 || (pull.State != "closed" && (pull.State != "open" || !validSHA(pull.Head.SHA))) {
			continue
		}
		var reviews []Review
		if pull.State == "open" && !pull.Draft {
			reviews, err = listPages[Review](ctx, s.up, fmt.Sprintf("%s/%d/reviews", path, pull.Number))
			if err != nil {
				log.Printf("reviews %s: %v", key(repo.Repo, pull.Number), err)
				continue
			}
		}
		if err := s.observe(repo, pull, reviews); err != nil {
			return err
		}
		if err := s.notifyApproved(ctx, repo, pull.Number); err != nil {
			return err
		}
		if err := s.postApproval(ctx, repo, pull.Number); err != nil {
			return err
		}
		if err := s.autoMerge(ctx, repo, pull.Number); err != nil {
			return err
		}
		if err := s.notifyMerged(ctx, repo, pull.Number); err != nil {
			return err
		}
		if err := s.dispatch(ctx, repo, pull.Number); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) observe(repo Repository, pull Pull, reviews []Review) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	work, err := s.store.loadWork(key(repo.Repo, pull.Number))
	if err != nil {
		return err
	}
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
		origin, err := s.store.matchOrigin(pull, time.Now())
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
	work.derive()
	if work.Task != nil && work.Task.Delivery == "manual" && !taskStarted(work.Task) {
		if err := s.freezeTaskConfig(work); err != nil {
			return err
		}
	}
	work.refreshWorkflow(time.Now(), s.config.Reliability.defaults())
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

func (s *Service) beginDelivery(id string, channelCheck reviewChannelCheck) (*Work, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	work, err := s.store.loadWork(id)
	if err != nil {
		return nil, err
	}
	if work != nil {
		if err := s.preparePendingConfig(work); err != nil {
			return nil, err
		}
	}
	if work == nil || work.Task == nil || work.Task.Handoff != nil || work.Task.Delivery != "pending" || s.delivering[id] || work.executionBlocked() || time.Now().Before(work.Task.RetryAt) {
		return nil, nil
	}
	// Missing provenance is a local configuration problem, not an unknown
	// external execution. Reject it before reserving an attempt or session.
	if err := s.validateReviewPreparation(work); err != nil {
		work.Task.Error = "review preparation: " + err.Error()
		work.refreshWorkflow(time.Now(), s.config.Reliability.defaults())
		return nil, s.store.saveWork(work)
	}

	if current, err := channelCheck.result(s, work); !current || err != nil {
		if err != nil {
			work.Task.Error = "review preparation: " + err.Error()
			work.refreshWorkflow(time.Now(), s.config.Reliability.defaults())
			return nil, s.store.saveWork(work)
		}
		return nil, nil
	}

	if err := s.freezeTaskConfig(work); err != nil {
		return nil, err
	}

	// Persist the attempt before making external requests. If the daemon restarts
	// during delivery, the operator can inspect the unknown outcome before retrying.
	work.Task.Delivery, work.Task.Error = "unknown", "delivery interrupted or in progress"
	work.Task.Attempts++
	if work.Task.ID == "" {
		work.Task.ID = uuid.NewString()
	}
	route := taskRoute(work, work.Task)
	if route == nil {
		return nil, fmt.Errorf("pending task has no execution route")
	}
	attemptID := uuid.NewString()
	work.Task.RetryAt = time.Time{}
	work.Task.Runs = append(work.Task.Runs, Attempt{ID: attemptID, Number: work.Task.Attempts, Route: *route, Request: deliveryFor(work, work.Task, attemptID, work.Task.Attempts), State: "unknown", CreatedAt: time.Now()})
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
	live.refreshWorkflow(time.Now(), s.config.Reliability.defaults())
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
	if handled, err := s.dispatchHandoff(ctx, id, channelCheck); handled || err != nil {
		return err
	}
	work, err := s.beginDelivery(id, channelCheck)
	if err != nil || work == nil {
		return err
	}
	defer func() {
		s.mu.Lock()
		delete(s.delivering, id)
		s.mu.Unlock()
	}()
	task := work.Task
	if task.ConfigSnapshot != nil {
		repo = task.ConfigSnapshot.Repository
	}
	route := work.Developer.Route
	if task.Role == "reviewer" {
		route = work.Reviewer.Route
	}
	if route == nil {
		return fmt.Errorf("pending task has no assignee route")
	}
	executor, ok := s.providers[route.Provider]
	if !ok {
		task.Error = "execution provider is not configured"
		return s.saveDelivery(id, work)
	}
	attempt := &task.Runs[len(task.Runs)-1]
	delivery := attempt.Request
	if route.Session == "" {
		*route = allocationRoute(repo, *route)
		attempt.Route = *route
		if err := s.saveDelivery(id, work); err != nil {
			return err
		}
		if err := execution.ValidateDestination(s.providers, *route, true); err != nil {
			task.Error = "invalid unallocated destination: " + err.Error()
			return s.saveDelivery(id, work)
		}
		if err := execution.ValidatePreparation(s.providers, *route, delivery); err != nil {
			task.Error = "invalid preparation context: " + err.Error()
			return s.saveDelivery(id, work)
		}
		preparation := delivery
		if delivery.RequireSameChannel {
			preparation.ReviewPlacement = channelCheck.placement
		}
		session, err := executor.Prepare(ctx, *route, preparation)
		if err != nil {
			task.Error = "prepare destination: " + err.Error()
			return s.saveDelivery(id, work)
		}
		prepared := *route
		prepared.Session = session
		if err := execution.ValidateDestination(s.providers, prepared, false); err != nil {
			task.Error = "provider returned an invalid destination"
			return s.saveDelivery(id, work)
		}
		*route = prepared
		attempt.Route = prepared
		if err := s.saveDelivery(id, work); err != nil {
			return err
		}
	}
	// Do not deliver a task superseded while its session was being allocated.
	s.mu.Lock()
	live, loadErr := s.store.loadWork(id)
	current := loadErr == nil && live != nil && live.Task != nil && live.Task.ID == task.ID
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

func allocationRoute(repo Repository, route execution.Route) execution.Route {
	// A cc-connect parent is only an explicit per-work override. In particular,
	// omitting it on assignment must not restore a legacy repository parent.
	if route.Provider == ccconnect.Name {
		return route
	}
	if fallback := repo.ReviewerRoute; route.Parent == "" && fallback != nil && fallback.Provider == route.Provider && fallback.Target == route.Target {
		route.Parent = fallback.Parent
	}
	return route
}
