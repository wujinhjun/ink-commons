//go:build linux || darwin

package ink

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/wujinhjun/ink-commons/internal/ink/statedb"
)

const (
	repoPollWorkers = 4
	prPollWorkers   = 8
	approvalWorkers = 2
)

type pullKey struct {
	repo   string
	number int
}

// Only active PRs occupy the map. Scheduled hooks inherit the same reservation
// from the sync worker and release it when their work ends.
type pullLocks struct {
	mu   sync.Mutex
	busy map[pullKey]struct{}
}

func (l *pullLocks) tryLock(id pullKey) (func(), bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, busy := l.busy[id]; busy {
		return nil, false
	}
	if l.busy == nil {
		l.busy = make(map[pullKey]struct{})
	}
	l.busy[id] = struct{}{}
	return func() {
		l.mu.Lock()
		delete(l.busy, id)
		l.mu.Unlock()
	}, true
}

type pollJob struct {
	repo    Repository
	number  int
	observe bool
	recover recovery
}

type recovery uint8

const (
	recoverNotice recovery = 1 << iota
	recoverHandoff
	recoverExecution
)

func (s *Service) recoveries(ctx context.Context) map[string]map[int]recovery {
	result := make(map[string]map[int]recovery)
	q := statedb.New(s.store.db)
	for _, scan := range []struct {
		kind recovery
		list func(context.Context) ([]string, error)
	}{
		{recoverNotice, q.ListBlockedNoticePending},
		{recoverHandoff, q.ListHandoffPending},
		{recoverExecution, q.ListExecutionPending},
	} {
		ids, err := scan.list(ctx)
		if err != nil {
			s.log.Warn("scan pending PR recovery", "kind", scan.kind)
			continue
		}
		for _, id := range ids {
			repo, n, _ := strings.Cut(id, "#")
			number, err := strconv.Atoi(n)
			if err != nil || number <= 0 {
				continue
			}
			if result[repo] == nil {
				result[repo] = make(map[int]recovery)
			}
			result[repo][number] |= scan.kind
		}
	}
	return result
}

func (s *Service) Poll(ctx context.Context) {
	if !s.pollMu.TryLock() {
		return
	}
	defer s.pollMu.Unlock()
	defer func() { s.lastPoll.Store(s.now().Unix()) }()
	if err := statedb.New(s.store.db).PrunePushes(ctx, pushCutoff(s.now())); err != nil {
		s.log.Warn("prune expired push observations")
	}

	recoveries := s.recoveries(ctx)
	jobs := make(chan pollJob, prPollWorkers)
	send := func(job pollJob) {
		select {
		case jobs <- job:
		case <-ctx.Done():
		}
	}
	var workers sync.WaitGroup
	for range prPollWorkers {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for job := range jobs {
				if err := s.pollPull(ctx, job); err != nil {
					s.log.Warn("sync pull request", "number", job.number)
				}
			}
		}()
	}

	type repoJob struct {
		repo    Repository
		pending map[int]recovery
	}
	repos := make(chan repoJob)
	var discovery sync.WaitGroup
	for range repoPollWorkers {
		discovery.Add(1)
		go func() {
			defer discovery.Done()
			for job := range repos {
				if err := s.pollRepo(ctx, job.repo, job.pending, send); err != nil {
					s.log.Warn("discover pull requests")
				}
			}
		}()
	}
	for _, repo := range s.repositories() {
		select {
		case repos <- repoJob{repo, recoveries[repo.Repo]}:
			delete(recoveries, repo.Repo)
		case <-ctx.Done():
		}
	}
	close(repos)
	for repo, pending := range recoveries {
		for num, flags := range pending {
			send(pollJob{repo: Repository{Repo: repo}, number: num, recover: flags})
		}
	}
	discovery.Wait()
	close(jobs)
	workers.Wait()
}

// The open list discovers PR numbers. A worker reads the actual PR after it
// owns the key, so overlapping observations cannot apply stale list payloads.
func (s *Service) pollRepo(ctx context.Context, repo Repository, recoveryByPR map[int]recovery, send func(pollJob)) error {
	path := "/repos/" + repo.Repo + "/pulls"
	pulls, listErr := listPages[Pull](ctx, s.up, path+"?state=open")
	pending, pendingErr := statedb.New(s.store.db).ListPollPending(ctx, repo.Repo)
	numbers := make(map[int]bool, len(pulls)+len(pending))
	emit := func(number int) {
		if number > 0 && !numbers[number] {
			numbers[number] = true
			send(pollJob{repo: repo, number: number, observe: true, recover: recoveryByPR[number]})
		}
	}
	for _, pull := range pulls {
		emit(pull.Number)
	}
	for _, number := range pending {
		emit(int(number))
	}
	for number, flags := range recoveryByPR {
		if !numbers[number] {
			send(pollJob{repo: repo, number: number, recover: flags})
		}
	}
	return errors.Join(listErr, pendingErr)
}

func (s *Service) pollPull(ctx context.Context, job pollJob) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	repo, number := job.repo, job.number
	unlock, ok := s.pullLocks.tryLock(pullKey{repo.Repo, number})
	if !ok {
		return nil
	}
	queued := false
	defer func() {
		if !queued {
			unlock()
		}
	}()
	id := key(repo.Repo, number)
	var recoveryErr error
	if job.recover&recoverNotice != 0 {
		recoveryErr = errors.Join(recoveryErr, s.reconcileBlockedWork(ctx, id))
	}
	if job.recover&recoverHandoff != 0 {
		recoveryErr = errors.Join(recoveryErr, s.reconcileHandoffWork(ctx, id))
	}
	if job.recover&recoverExecution != 0 {
		recoveryErr = errors.Join(recoveryErr, s.reconcileWork(ctx, id))
	}
	if !job.observe {
		return recoveryErr
	}
	observed, err := s.observePull(ctx, repo, number)
	if err != nil || observed == nil {
		return errors.Join(recoveryErr, err)
	}
	ctx = context.WithValue(ctx, observationKey{}, *observed)
	// Keep the hook tied to this observed approval. Run it after the merge
	// decision, on its own workers, even if another effect reported an error.
	hook, hookErr := s.approvalCandidate(ctx, repo, number)
	for _, effect := range []func(context.Context, Repository, int) error{
		s.notifyApproved, s.autoMerge, s.notifyMerged, s.dispatch,
	} {
		err = errors.Join(err, effect(ctx, repo, number))
	}
	if hook != nil && hook.version == observed.version && hook.event.SHA == observed.head {
		queued = s.queueApproval(ctx, *hook, unlock)
	}
	return errors.Join(recoveryErr, hookErr, err)
}

type observationKey struct{}

type prObservation struct {
	repo    string
	number  int
	head    string
	version uint64
}

func matchesObservation(ctx context.Context, w *Work) bool {
	observed, guarded := ctx.Value(observationKey{}).(prObservation)
	return !guarded || (w != nil && w.Repo == observed.repo && w.Pull.Number == observed.number &&
		w.Pull.Head.SHA == observed.head && w.Version == observed.version)
}

func (s *Service) observePull(ctx context.Context, repo Repository, number int) (*prObservation, error) {
	id := key(repo.Repo, number)
	before, err := s.store.loadWork(id)
	if err != nil {
		return nil, err
	}
	path := fmt.Sprintf("/repos/%s/pulls/%d", repo.Repo, number)
	var pull Pull
	if _, err := s.up.gitHubGet(ctx, path, &pull); err != nil {
		return nil, err
	}
	if pull.Number != number || (pull.State != "closed" && (pull.State != "open" || !validSHA(pull.Head.SHA))) {
		return nil, nil
	}
	var reviews []Review
	if pull.State == "open" && !pull.Draft {
		reviews, err = listPages[Review](ctx, s.up, path+"/reviews")
		if err != nil {
			return nil, err
		}
		var current Pull
		if _, err := s.up.gitHubGet(ctx, path, &current); err != nil {
			return nil, err
		}
		if current.Number != number || current.Head.SHA != pull.Head.SHA || current.State != pull.State || current.Draft != pull.Draft {
			return nil, nil
		}
		pull = current
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	live, err := s.store.loadWork(id)
	if err != nil {
		return nil, err
	}
	// API writes can proceed while GitHub is being read. Skip this sample if
	// they changed the PR generation, and use the latest policy under s.mu.
	if (before == nil) != (live == nil) || (before != nil && before.Version != live.Version) {
		return nil, nil
	}
	if live == nil {
		live = &Work{Repo: repo.Repo}
	}
	if err := s.saveObservation(repo, pull, reviews, live); err != nil {
		return nil, err
	}
	return &prObservation{repo: repo.Repo, number: number, head: pull.Head.SHA, version: live.Version}, nil
}

// Revalidate after preparation I/O before sending a new task to an executor.
func (s *Service) observedHeadCurrent(ctx context.Context) (bool, error) {
	observed, guarded := ctx.Value(observationKey{}).(prObservation)
	if !guarded {
		return true, nil
	}
	var pull Pull
	path := fmt.Sprintf("/repos/%s/pulls/%d", observed.repo, observed.number)
	if _, err := s.up.gitHubGet(ctx, path, &pull); err != nil {
		return false, err
	}
	return pull.Number == observed.number && pull.State == "open" && !pull.Draft && pull.Head.SHA == observed.head, nil
}
