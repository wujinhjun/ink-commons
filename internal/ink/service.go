package ink

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"ink-commons/internal/execution"
	"ink-commons/internal/execution/ccconnect"
)

type Service struct {
	store      *stateStore
	config     Config
	up         upstream
	mu         sync.Mutex
	lastPoll   atomic.Int64
	delivering map[string]bool
	providers  map[string]execution.Executor
}

func NewService(config Config, path string, providers map[string]execution.Executor) (*Service, error) {
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
	st, err := store.load()
	if err != nil {
		store.db.Close()
		return nil, err
	}
	changed := false
	for _, w := range st.Work {
		if migrateExecution(w) {
			changed = true
		}
		w.refreshWorkflow(time.Now(), config.Reliability.defaults())
		changed = true
	}
	if changed {
		if err := store.save(st); err != nil {
			store.db.Close()
			return nil, err
		}
	}
	up := newUpstream(config.GitHubToken)
	up.command = config.GitHubCommand
	return &Service{store: store, config: config, up: up, providers: providers, delivering: make(map[string]bool)}, nil
}

func (s *Service) Close() error { return s.store.db.Close() }

// Caller holds s.mu. Static configuration takes precedence over runtime registration.
func (s *Service) repo(name string) (Repository, bool) {
	for _, r := range s.config.Repositories {
		if r.Repo == name {
			return r, true
		}
	}
	st, err := s.store.load()
	if err == nil {
		for _, r := range st.Repositories {
			if r.Repo == name {
				return r, true
			}
		}
	}
	return Repository{}, false
}

func (s *Service) repositories() []Repository {
	s.mu.Lock()
	defer s.mu.Unlock()
	repos := append([]Repository{}, s.config.Repositories...)
	st, err := s.store.load()
	if err != nil {
		log.Printf("load repository registry: %v", err)
		return repos
	}
	seen := map[string]bool{}
	for _, r := range repos {
		seen[r.Repo] = true
	}
	for _, r := range st.Repositories {
		if !seen[r.Repo] {
			repos = append(repos, r)
		}
	}
	return repos
}

func (s *Service) recordPushes(st *state, pushes []Push, now time.Time) {
	for _, p := range pushes {
		if _, ok := s.repo(p.Repo); !ok {
			continue
		}
		p.At = now
		replaced := false
		for i, old := range st.Pushes {
			if p.Repo == old.Repo && p.Ref == old.Ref && p.SHA == old.SHA && p.Route == old.Route {
				st.Pushes[i] = p
				replaced = true
				break
			}
		}
		if !replaced {
			st.Pushes = append(st.Pushes, p)
		}
	}
}

func matchOrigin(pushes []Push, pull Pull) *execution.Route {
	if pull.Head.Repo == nil || !validSHA(pull.Head.SHA) {
		return nil
	}
	var match *execution.Route
	for _, p := range pushes {
		if !strings.EqualFold(p.Repo, pull.Head.Repo.FullName) || p.Ref != pull.Head.Ref || p.SHA != pull.Head.SHA {
			continue
		}
		if match != nil && *match != p.Route {
			return nil
		}
		route := p.Route
		match = &route
	}
	return match
}

func key(repo string, number int) string { return fmt.Sprintf("%s#%d", repo, number) }

func (s *Service) Poll(ctx context.Context) {
	defer func() { s.lastPoll.Store(time.Now().Unix()) }()
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
	snapshot, err := s.store.load()
	s.mu.Unlock()
	if err != nil {
		return err
	}

	open := make(map[int]bool, len(pulls))
	for _, pull := range pulls {
		open[pull.Number] = true
	}
	for _, work := range snapshot.Work {
		if work.Repo != repo.Repo || (work.Pull.State != "open" && !work.pendingMergedNotification() && !hasUnknownAutoMerge(work)) || open[work.Pull.Number] {
			continue
		}
		var pull Pull
		_, err := s.up.gitHubGet(ctx, fmt.Sprintf("%s/%d", path, work.Pull.Number), &pull)
		if err != nil {
			log.Printf("refresh %s: %v", key(repo.Repo, work.Pull.Number), err)
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
	st, err := s.store.load()
	if err != nil {
		return err
	}
	prunePushes(&st, time.Now())
	id := key(repo.Repo, pull.Number)
	work := st.Work[id]
	if work == nil {
		work = &Work{Repo: repo.Repo, Reviewer: Assignee{GitHubLogin: repo.ReviewerLogin, GitHubCommand: repo.ReviewerCommand}, Developer: Assignee{GitHubLogin: repo.HumanDeveloper}}
		if repo.ReviewerRoute != nil {
			route := *repo.ReviewerRoute
			// Discord placement belongs to this PR's development conversation.
			// Legacy repository parents must not silently move new reviews elsewhere.
			if route.Provider == ccconnect.Name && route.Session == "" {
				route.Parent = ""
			}
			work.Reviewer.Route = &route
		}
		st.Work[id] = work
	}
	work.TaskMode = repo.TaskMode
	work.Version++
	work.Pull = pull
	if pull.State == "open" && !work.Linked {
		if origin := matchOrigin(st.Pushes, pull); origin != nil {
			work.Origin, work.Linked, work.Developer.Route = origin, true, origin
		}
	}
	if pull.State == "open" && !pull.Draft {
		work.Reviews = mergeReviews(work.Reviews, reviews)
	}
	work.derive()
	// Refresh features only before a new task has made an external attempt.
	// Existing reviewer sessions retain their saved placement.
	if work.Task == nil || len(work.Task.Runs) == 0 {
		work.Review = nil
		if repo.Review != nil {
			features := *repo.Review
			work.Review = &features
		}
	}
	work.refreshWorkflow(time.Now(), s.config.Reliability.defaults())
	return s.store.save(st)
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

func prunePushes(st *state, now time.Time) {
	cutoff := now.Add(-7 * 24 * time.Hour)
	kept := st.Pushes[:0]
	for _, push := range st.Pushes {
		if push.At.After(cutoff) {
			kept = append(kept, push)
		}
	}
	st.Pushes = kept
}

func (s *Service) beginDelivery(id string) (*Work, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.store.load()
	if err != nil {
		return nil, err
	}
	work := st.Work[id]
	if work == nil || work.Task == nil || work.Task.Handoff != nil || work.Task.Delivery != "pending" || s.delivering[id] || work.executionBlocked() || time.Now().Before(work.Task.RetryAt) {
		return nil, nil
	}
	// Missing provenance is a local configuration problem, not an unknown
	// external execution. Reject it before reserving an attempt or session.
	if err := s.validateReviewPreparation(work); err != nil {
		work.Task.Error = "review preparation: " + err.Error()
		work.refreshWorkflow(time.Now(), s.config.Reliability.defaults())
		return nil, s.store.save(st)
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
	if err := s.store.save(st); err != nil {
		return nil, err
	}
	s.delivering[id] = true
	return work, nil
}

func (s *Service) saveDelivery(id string, work *Work) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Reload so hooks and operations on other PRs can save while delivery runs.
	st, err := s.store.load()
	if err != nil {
		return err
	}
	live := st.Work[id]
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
	return s.store.save(st)
}

func (s *Service) dispatch(ctx context.Context, repo Repository, number int) error {
	if ctx.Err() != nil {
		return nil
	}
	if handled, err := s.dispatchHandoff(ctx, repo, number); handled || err != nil {
		return err
	}
	id := key(repo.Repo, number)
	work, err := s.beginDelivery(id)
	if err != nil || work == nil {
		return err
	}
	defer func() {
		s.mu.Lock()
		delete(s.delivering, id)
		s.mu.Unlock()
	}()
	task := work.Task
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
		session, err := executor.Prepare(ctx, *route, delivery)
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
	latest, loadErr := s.store.load()
	live := latest.Work[id]
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
