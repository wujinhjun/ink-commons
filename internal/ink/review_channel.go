//go:build linux || darwin

package ink

import (
	"context"
	"fmt"
	"strings"

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
	t := w.Task
	if t == nil || t.Preparation != nil || (t.Handoff != nil && t.Handoff.Wake.Preparation == "prepared") {
		return reviewChannelBinding{}
	}
	delivery := deliveryFor(w, t, "", 0)
	route := taskRoute(w, t)
	if t.Handoff != nil {
		route = t.Handoff.Assignee.Route
	}
	if route == nil {
		return reviewChannelBinding{}
	}
	provider := s.providers[route.Provider]
	_, resolves := provider.(execution.PlacementResolver)
	requiresResolution := policyNeedsPlacement(*route, delivery.RequireSameChannel, delivery.ReuseDevelopmentThread) ||
		(route.Session == "" && execution.Supports(provider, "placement", resolves))
	if !requiresResolution {
		return reviewChannelBinding{}
	}
	binding := reviewChannelBinding{
		taskID: t.ID, route: *route, reuseThread: delivery.ReuseDevelopmentThread, requireSame: delivery.RequireSameChannel,
	}
	if delivery.Source != nil {
		binding.source = *delivery.Source
	}
	return binding
}

// Resolve placement outside the state lock. The caller compares the current
// policy, route and source again before reserving any side effect.
func (s *Service) checkReviewChannel(ctx context.Context, id string) (reviewChannelCheck, error) {
	s.mu.Lock()
	w, err := s.store.loadWork(id)
	var binding reviewChannelBinding
	var delivery execution.Delivery
	if err == nil && w != nil && matchesObservation(ctx, w) && !s.delivering[id] {
		err = s.preparePendingConfig(w)
		if t := w.Task; err == nil && t != nil && t.Delivery == "pending" && !w.executionBlocked() {
			now := s.now()
			if h := t.Handoff; (h == nil && !now.Before(t.RetryAt)) ||
				(h != nil && h.State == "pending" && h.Wake.Preparation != "preparing" && !now.Before(h.Wake.RetryAt)) {
				binding = s.reviewChannelDestination(w)
				delivery = deliveryFor(w, t, "", 0)
			}
		}
	}
	s.mu.Unlock()
	check := reviewChannelCheck{binding: binding}
	if err != nil || binding == (reviewChannelBinding{}) {
		return check, err
	}
	if delivery.ContextBefore != "" {
		if err := s.validateHistorySource(delivery.Source); err != nil {
			check.err = err
			return check, nil
		}
	}
	provider := s.providers[binding.route.Provider]
	resolver, ok := provider.(execution.PlacementResolver)
	switch {
	case !execution.Supports(provider, "placement", ok):
		check.err = fmt.Errorf("execution provider does not support placement")
	default:
		if check.err = validatePlacementInput(binding); check.err == nil {
			check.placement, check.err = resolver.ResolveReviewPlacement(ctx, binding.route, delivery)
		}
		if check.err == nil {
			check.err = validatePlacement(binding, check.placement)
		}
	}
	return check, nil
}

func policyNeedsPlacement(route execution.Route, requireSame, reuse bool) bool {
	return requireSame || route.Parent != "" || (reuse && route.Session == "")
}

func validatePlacementInput(b reviewChannelBinding) error {
	if (b.requireSame || wantsReuse(b)) && (!b.source.Valid(false) || b.source.Provider != b.route.Provider) {
		return fmt.Errorf("placement policy requires a resolved development route on the same provider")
	}
	if wantsReuse(b) && b.source.Target == b.route.Target {
		return fmt.Errorf("conversation reuse requires independent execution targets")
	}
	return nil
}

func wantsReuse(b reviewChannelBinding) bool {
	return b.reuseThread && b.route.Session == "" && b.route.Parent == ""
}

func validatePlacement(b reviewChannelBinding, p *execution.ReviewPlacement) error {
	if p == nil || (strings.TrimSpace(p.Parent) == "" && strings.TrimSpace(p.Session) == "") {
		return fmt.Errorf("adapter returned incomplete placement")
	}
	if b.route.Parent != "" && b.route.Parent != p.Parent {
		return fmt.Errorf("adapter placement does not match the explicit parent")
	}
	if b.requireSame && (p.SourceParent == "" || p.Parent != p.SourceParent) {
		return fmt.Errorf("require_same_channel: review and development parents differ")
	}
	if b.route.Session != "" && p.Session != b.route.Session {
		return fmt.Errorf("adapter placement changed the explicit session")
	}
	if wantsReuse(b) && p.Session != b.source.Session {
		return fmt.Errorf("adapter placement did not reuse the development session")
	}
	if b.route.Session == "" && !wantsReuse(b) && p.Session != "" {
		return fmt.Errorf("adapter selected an existing session without a reuse policy")
	}
	return nil
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
