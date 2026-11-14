package ink

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"ink-commons/internal/execution"
)

type Service struct {
	store      string
	config     Config
	up         upstream
	mu         sync.Mutex
	delivering map[string]bool
	providers  map[string]execution.Executor
}

func NewService(config Config, path string, providers map[string]execution.Executor) (*Service, error) {
	if _, err := openState(path); err != nil {
		return nil, err
	}
	return &Service{store: path, config: config, up: newUpstream(config.GitHubToken), providers: providers, delivering: make(map[string]bool)}, nil
}

func (s *Service) repo(name string) (Repository, bool) {
	for _, r := range s.config.Repositories {
		if r.Repo == name {
			return r, true
		}
	}
	return Repository{}, false
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
	for _, repo := range s.config.Repositories {
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
	snapshot, err := openState(s.store)
	s.mu.Unlock()
	if err != nil {
		return err
	}

	open := make(map[int]bool, len(pulls))
	for _, pull := range pulls {
		open[pull.Number] = true
	}
	for _, work := range snapshot.Work {
		if work.Repo != repo.Repo || work.Pull.State != "open" || open[work.Pull.Number] {
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
		if err := s.dispatch(ctx, repo, pull.Number); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) observe(repo Repository, pull Pull, reviews []Review) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := openState(s.store)
	if err != nil {
		return err
	}
	prunePushes(&st, time.Now())
	id := key(repo.Repo, pull.Number)
	work := st.Work[id]
	if work == nil {
		work = &Work{Repo: repo.Repo, Reviewer: Assignee{GitHubLogin: repo.ReviewerLogin}, Developer: Assignee{GitHubLogin: repo.HumanDeveloper}}
		if repo.ReviewerRoute != nil {
			route := *repo.ReviewerRoute
			work.Reviewer.Route = &route
		}
		st.Work[id] = work
	}
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
	return saveState(s.store, st)
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
	st, err := openState(s.store)
	if err != nil {
		return nil, err
	}
	work := st.Work[id]
	if work == nil || work.Task == nil || work.Task.Delivery != "pending" || s.delivering[id] {
		return nil, nil
	}

	// Persist the attempt before making external requests. If the daemon restarts
	// during delivery, the operator can inspect the unknown outcome before retrying.
	work.Task.Delivery, work.Task.Error = "unknown", "delivery interrupted or in progress"
	work.Task.Attempts++
	if err := saveState(s.store, st); err != nil {
		return nil, err
	}
	s.delivering[id] = true
	return work, nil
}

func (s *Service) saveDelivery(id string, work *Work) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Reload so hooks and operations on other PRs can save while delivery runs.
	st, err := openState(s.store)
	if err != nil {
		return err
	}
	st.Work[id] = work
	return saveState(s.store, st)
}

func (s *Service) dispatch(ctx context.Context, repo Repository, number int) error {
	if ctx.Err() != nil {
		return nil
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
	delivery := execution.Delivery{Repository: work.Repo, Number: work.Pull.Number, URL: work.Pull.URL, Role: task.Role, SHA: task.SHA, ReviewID: task.ReviewID, ReviewURL: task.ReviewURL}
	if task.Role == "reviewer" {
		delivery.Source = work.Developer.Route
		if delivery.Source == nil {
			delivery.Source = work.Origin
		}
	}
	if route.Session == "" {
		if fallback := repo.ReviewerRoute; route.Parent == "" && fallback != nil && fallback.Provider == route.Provider && fallback.Target == route.Target {
			route.Parent = fallback.Parent
		}
		if err := execution.ValidateDestination(s.providers, *route, true); err != nil {
			task.Error = "invalid unallocated destination: " + err.Error()
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
		if err := s.saveDelivery(id, work); err != nil {
			return err
		}
	}
	if err := executor.Deliver(ctx, *route, delivery); err != nil {
		task.Error = "deliver task: " + err.Error()
	} else {
		task.Delivery, task.Error = "accepted", ""
	}
	return s.saveDelivery(id, work)
}
