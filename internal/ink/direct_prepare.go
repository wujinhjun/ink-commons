//go:build linux || darwin

package ink

import (
	"context"
	"fmt"

	"github.com/wujinhjun/ink-commons/internal/execution"
)

// Prepare a direct route before reserving an execution attempt. A restart can
// recover the allocation without asking the runtime about a nonexistent run.
func (s *Service) prepareDirect(ctx context.Context, id string, check reviewChannelCheck) (bool, error) {
	s.mu.Lock()
	work, err := s.store.loadWork(id)
	if err != nil || !matchesObservation(ctx, work) || work == nil || work.Task == nil {
		s.mu.Unlock()
		return false, err
	}
	if err := s.preparePendingConfig(work); err != nil {
		s.mu.Unlock()
		return false, err
	}
	task := work.Task
	if task.Handoff != nil || task.Delivery != "pending" || s.delivering[id] || work.executionBlocked() || s.now().Before(task.RetryAt) {
		s.mu.Unlock()
		return false, nil
	}
	route := taskRoute(work, task)
	if route == nil {
		s.mu.Unlock()
		return false, fmt.Errorf("pending task has no execution route")
	}
	provider := s.providers[route.Provider]
	current, validation := check.result(s, work)
	if !current {
		s.mu.Unlock()
		return false, nil
	}
	if validation == nil {
		validation = s.validateReviewPreparation(work)
	}
	if validation == nil && !execution.Supports(provider, "deliver", provider != nil) {
		validation = fmt.Errorf("execution provider does not support deliver")
	}
	if validation == nil {
		validation = execution.ValidateDestination(s.providers, *route, route.Session == "")
	}
	if validation != nil {
		task.PreparationInvalid, task.Error = true, "review preparation: "+validation.Error()
		s.refreshWorkflow(work, s.now())
		err := s.store.saveWork(work)
		s.mu.Unlock()
		return false, err
	}
	if route.Session != "" && check.placement == nil {
		s.mu.Unlock()
		return true, nil
	}

	state := task.Preparation
	recovering := state != nil
	prepared := *route
	if recovering {
		if state.canRecover() && !supportsAllocation(provider) {
			task.PreparationInvalid = true
			task.Error = "provider cannot recover the reserved conversation"
			s.refreshWorkflow(work, s.now())
			err := s.store.saveWork(work)
			s.mu.Unlock()
			return false, err
		}
		if !state.canRecover() || s.now().Before(state.RetryAt) {
			s.mu.Unlock()
			return false, nil
		}
		if state.Route != nil {
			prepared = *state.Route
		}
		state.RecoveryAttempts++
		state.RetryAt = s.now().Add(taskWakeRetryDelay(state.RecoveryAttempts))
	} else {
		if err := s.freezeTaskConfig(work); err != nil {
			s.mu.Unlock()
			return false, err
		}
		d := deliveryFor(work, task, "", 0)
		state = &TaskWake{Request: &d}
		if err := reservePreparation(provider, state, &prepared, d, check.placement, s.now()); err != nil {
			task.PreparationInvalid, task.Error = true, "conversation preparation: "+err.Error()
			s.refreshWorkflow(work, s.now())
			err := s.store.saveWork(work)
			s.mu.Unlock()
			return false, err
		}
		state.RetryAt = s.now().Add(taskWakeRetryDelay(1))
		state.Route = cloneRoute(&prepared)
		task.Preparation = state
		if state.Preparation == "preparing" {
			task.Error = "waiting for conversation allocation to be confirmed"
		}
	}
	if err := s.store.saveWork(work); err != nil {
		s.mu.Unlock()
		return false, err
	}
	s.delivering[id] = true
	s.mu.Unlock()

	prepareErr := prepareConversation(ctx, provider, &prepared, *state.Request, state.Allocation, recovering)
	s.mu.Lock()
	delete(s.delivering, id)
	live, err := s.store.loadWork(id)
	latest := live.findTask(task.ID)
	if err != nil || latest == nil {
		s.mu.Unlock()
		return false, err
	}
	if prepareErr != nil {
		latest.Preparation.recordPreparationError(prepareErr, recovering)
		latest.Error = latest.Preparation.Error
		latest.PreparationInvalid = true
	} else {
		latest.Preparation.Preparation, latest.Preparation.Error = "prepared", ""
		latest.Error, latest.PreparationInvalid = "", false
		latest.Preparation.Route = &prepared
		if latest == live.Task {
			*taskRoute(live, latest) = prepared
		}
	}
	s.refreshWorkflow(live, s.now())
	err = s.store.saveWork(live)
	s.mu.Unlock()
	if err != nil || prepareErr != nil || latest != live.Task {
		return false, err
	}
	return s.observedHeadCurrent(ctx)
}
