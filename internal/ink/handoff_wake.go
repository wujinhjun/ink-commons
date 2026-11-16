package ink

import (
	"context"
	"fmt"
	"time"

	"ink-commons/internal/execution"
)

// Each pending task is its own durable outbox entry. Wake retries never create
// executor attempts: only claim grants permission to do work.
func (s *Service) dispatchHandoff(ctx context.Context, repo Repository, number int) (bool, error) {
	id := key(repo.Repo, number)
	s.mu.Lock()
	st, err := s.store.load()
	if err != nil {
		s.mu.Unlock()
		return false, err
	}
	w := st.Work[id]
	if w == nil || w.Task == nil || w.Task.Handoff == nil {
		s.mu.Unlock()
		return false, nil
	}
	t := w.Task
	h := t.Handoff
	if h.State != "pending" || w.executionBlocked() || s.delivering[id] || time.Now().Before(h.Wake.RetryAt) || h.Wake.Preparation == "preparing" {
		s.mu.Unlock()
		return true, nil
	}
	route := h.Assignee.Route
	if route == nil {
		s.mu.Unlock()
		return true, nil
	}
	executor := s.providers[route.Provider]
	waker, ok := executor.(execution.TaskWaker)
	if !ok {
		h.Wake.Error = "provider does not support cooperative task wakes"
		w.refreshWorkflow(time.Now(), s.config.Reliability.defaults())
		err = s.store.save(st)
		s.mu.Unlock()
		return true, err
	}
	if err := s.validateReviewPreparation(w); err != nil {
		h.Wake.Error = "review preparation: " + err.Error()
		w.refreshWorkflow(time.Now(), s.config.Reliability.defaults())
		saveErr := s.store.save(st)
		s.mu.Unlock()
		return true, saveErr
	}
	if err := execution.ValidateDestination(s.providers, *route, route.Session == ""); err != nil {
		s.mu.Unlock()
		return true, err
	}
	d := deliveryFor(w, t, "", 0)
	prepared := *route
	if prepared.Session == "" {
		if err := execution.ValidatePreparation(s.providers, prepared, d); err != nil {
			h.Wake.Error = err.Error()
			err = s.store.save(st)
			s.mu.Unlock()
			return true, err
		}
		// Never repeat ambiguous external conversation creation after restart.
		h.Wake.Preparation = "preparing"
	}
	// Bound wake retries, including transport failures and process interruptions.
	h.Wake.Attempts++
	delay := time.Duration(s.config.Reliability.defaults().BackoffSeconds) * time.Second
	for n := 1; n < h.Wake.Attempts && delay < time.Hour; n++ {
		delay *= 2
	}
	if delay > time.Hour {
		delay = time.Hour
	}
	h.Wake.RetryAt = time.Now().Add(delay)
	if err = s.store.save(st); err != nil {
		s.mu.Unlock()
		return true, err
	}
	s.delivering[id] = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.delivering, id); s.mu.Unlock() }()
	if prepared.Session == "" {
		session, prepareErr := executor.Prepare(ctx, prepared, d)
		prepared.Session = session
		if prepareErr == nil {
			prepareErr = execution.ValidateDestination(s.providers, prepared, false)
		}
		s.mu.Lock()
		live, loadErr := s.store.load()
		if loadErr != nil {
			s.mu.Unlock()
			return true, loadErr
		}
		lw, lt := findTask(live, t.ID)
		if lt == nil || lt.Handoff == nil {
			s.mu.Unlock()
			return true, nil
		}
		lh := lt.Handoff
		if prepareErr != nil {
			lh.Wake.Error = "conversation allocation unresolved; inspect before retrying"
		} else {
			lh.Wake.Preparation = "prepared"
			lh.Assignee.Route = &prepared
			lh.Wake.Error = ""
			if lw.Task == lt {
				if lt.Role == "reviewer" {
					lw.Reviewer.Route = &prepared
				} else {
					lw.Developer.Route = &prepared
				}
			}
		}
		lw.refreshWorkflow(time.Now(), s.config.Reliability.defaults())
		err = s.store.save(live)
		s.mu.Unlock()
		if err != nil || prepareErr != nil {
			return true, err
		}
	}
	// Recheck after allocation. A delayed wake is harmless because claim performs
	// the same eligibility check under the state lock.
	s.mu.Lock()
	live, err := s.store.load()
	current, latest := findTask(live, t.ID)
	eligible := err == nil && latest != nil && current.Task == latest && latest.Handoff != nil && latest.Handoff.State == "pending" && !current.executionBlocked()
	s.mu.Unlock()
	if err != nil || !eligible {
		return true, err
	}
	sendErr := waker.WakeTask(ctx, prepared, d)
	s.mu.Lock()
	defer s.mu.Unlock()
	live, err = s.store.load()
	if err != nil {
		return true, err
	}
	current, latest = findTask(live, t.ID)
	if latest != nil && latest.Handoff != nil {
		latest.Handoff.Wake.Error = ""
		if sendErr != nil {
			latest.Handoff.Wake.Error = fmt.Sprintf("wake not confirmed; will retry: %v", sendErr)
		}
		current.refreshWorkflow(time.Now(), s.config.Reliability.defaults())
	}
	return true, s.store.save(live)
}
