package ink

import (
	"context"
	"fmt"
	"time"

	"github.com/wujinhjun/ink-commons/internal/execution"
)

type reviewChannelBinding struct {
	taskID      string
	route       execution.Route
	source      execution.Route
	reuseThread bool
	requireSame bool
}

type reviewChannelCheck struct {
	binding   reviewChannelBinding
	placement *execution.ReviewPlacement
	err       error
}

func (s *Service) reviewChannelDestination(w *Work) reviewChannelBinding {
	if w.Task == nil || w.Task.Role != "reviewer" {
		return reviewChannelBinding{}
	}
	delivery := deliveryFor(w, w.Task, "", 0)
	route := taskRoute(w, w.Task)
	if w.Task.Handoff != nil {
		route = w.Task.Handoff.Assignee.Route
	}
	if route == nil {
		return reviewChannelBinding{}
	}
	_, recoverable := s.providers[route.Provider].(execution.ConversationAllocator)
	resolveAllocation := recoverable && w.Task.Handoff != nil && route.Session == ""
	if !resolveAllocation && (!delivery.RequireSameChannel || delivery.Source == nil) {
		return reviewChannelBinding{}
	}
	binding := reviewChannelBinding{
		taskID: w.Task.ID, route: *route, reuseThread: delivery.ReuseDevelopmentThread, requireSame: delivery.RequireSameChannel,
	}
	if delivery.Source != nil {
		binding.source = *delivery.Source
	}
	return binding
}

// Resolve Discord channels without holding the state lock. The caller compares
// the route and source again under the lock before starting an execution.
func (s *Service) checkReviewChannel(ctx context.Context, id string) (reviewChannelCheck, error) {
	s.mu.Lock()
	w, err := s.store.loadWork(id)
	var binding reviewChannelBinding
	if err == nil && w != nil && !s.delivering[id] {
		err = s.preparePendingConfig(w)
		if t := w.Task; err == nil && t != nil && t.Delivery == "pending" && !w.executionBlocked() {
			now := time.Now()
			if h := t.Handoff; (h == nil && !now.Before(t.RetryAt)) ||
				(h != nil && h.State == "pending" && h.Wake.Preparation != "preparing" && !now.Before(h.Wake.RetryAt)) {
				binding = s.reviewChannelDestination(w)
			}
		}
	}
	s.mu.Unlock()
	check := reviewChannelCheck{binding: binding}
	if err != nil || binding == (reviewChannelBinding{}) {
		return check, err
	}
	provider, ok := s.providers[binding.route.Provider].(interface {
		ResolveReviewPlacement(context.Context, execution.Route, execution.Delivery) (*execution.ReviewPlacement, error)
	})
	if !ok {
		check.err = fmt.Errorf("provider does not support require_same_channel")
	} else {
		delivery := execution.Delivery{RequireSameChannel: binding.requireSame, ReuseDevelopmentThread: binding.reuseThread}
		if binding.source != (execution.Route{}) {
			delivery.Source = &binding.source
		}
		check.placement, check.err = provider.ResolveReviewPlacement(ctx, binding.route, delivery)
	}
	return check, nil
}

func (c reviewChannelCheck) result(s *Service, w *Work) (bool, error) {
	binding := s.reviewChannelDestination(w)
	if binding == (reviewChannelBinding{}) {
		return true, nil
	}
	if binding != c.binding {
		return false, nil
	}
	return true, c.err
}
