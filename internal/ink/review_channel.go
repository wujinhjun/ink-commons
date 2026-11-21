package ink

import (
	"context"
	"fmt"
	"time"

	"ink-commons/internal/execution"
)

type reviewChannelBinding struct {
	taskID      string
	route       execution.Route
	source      execution.Route
	reuseThread bool
}

type reviewChannelCheck struct {
	binding   reviewChannelBinding
	placement *execution.ReviewPlacement
	err       error
}

func reviewChannelDestination(w *Work) reviewChannelBinding {
	if w.Task == nil || w.Task.Role != "reviewer" {
		return reviewChannelBinding{}
	}
	delivery := deliveryFor(w, w.Task, "", 0)
	route := taskRoute(w, w.Task)
	if w.Task.Handoff != nil {
		route = w.Task.Handoff.Assignee.Route
	}
	if !delivery.RequireSameChannel || route == nil || delivery.Source == nil {
		return reviewChannelBinding{}
	}
	return reviewChannelBinding{w.Task.ID, *route, *delivery.Source, delivery.ReuseDevelopmentThread}
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
				binding = reviewChannelDestination(w)
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
		check.placement, check.err = provider.ResolveReviewPlacement(ctx, binding.route, execution.Delivery{
			RequireSameChannel: true, ReuseDevelopmentThread: binding.reuseThread, Source: &binding.source,
		})
	}
	return check, nil
}

func (c reviewChannelCheck) result(w *Work) (bool, error) {
	binding := reviewChannelDestination(w)
	if binding == (reviewChannelBinding{}) {
		return true, nil
	}
	if binding != c.binding {
		return false, nil
	}
	return true, c.err
}
