package httpagent

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/wujinhjun/ink-commons/internal/execution"
)

type Request struct {
	Version      int                               `json:"version"`
	Operation    string                            `json:"operation"`
	Route        execution.Route                   `json:"route"`
	Task         *execution.Delivery               `json:"task,omitempty"`
	Allocation   *execution.ConversationAllocation `json:"allocation,omitempty"`
	Mode         string                            `json:"mode,omitempty"`
	Notification *execution.Notification           `json:"notification,omitempty"`
	RequireACK   *bool                             `json:"require_ack,omitempty"`
	Reason       string                            `json:"reason,omitempty"`
	Before       string                            `json:"before,omitempty"`
}

type Response struct {
	Session      string                      `json:"session,omitempty"`
	Status       string                      `json:"status,omitempty"`
	Placement    *execution.ReviewPlacement  `json:"placement,omitempty"`
	Conversation *execution.ConversationPage `json:"conversation,omitempty"`
	Receipt      *execution.Receipt          `json:"receipt,omitempty"`
}

type Executor struct {
	managed, requireACK bool
	capabilities        map[string]bool
	client              *http.Client
	endpoint, token     string
}

func New(client *http.Client, endpoint, token string, capabilities []string, managed, requireACK bool) (*Executor, error) {
	u, err := execution.Endpoint(endpoint)
	if err != nil || (!execution.IsLoopback(u) && (u.Scheme != "https" || token == "")) {
		return nil, fmt.Errorf("adapter URL must be a loopback endpoint or HTTPS with a bearer token")
	}
	caps := make(map[string]bool)
	for _, capability := range capabilities {
		switch capability {
		case "prepare", "deliver", "wake", "notify", "blocked_notice", "placement", "conversation_context", "status", "stop":
		default:
			return nil, fmt.Errorf("unknown adapter capability")
		}
		if caps[capability] {
			return nil, fmt.Errorf("duplicate adapter capability")
		}
		caps[capability] = true
	}
	if len(caps) == 0 {
		return nil, fmt.Errorf("adapter capabilities must be declared explicitly")
	}
	if managed {
		if !caps["deliver"] || !caps["status"] || !caps["stop"] || token == "" {
			return nil, fmt.Errorf("managed adapters require deliver, status, stop and a bearer token")
		}
	} else if caps["status"] || caps["stop"] {
		return nil, fmt.Errorf("status and stop require managed execution")
	}
	if requireACK && !caps["notify"] {
		return nil, fmt.Errorf("require_ack needs the notify capability")
	}
	safeClient := *client
	safeClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Executor{client: &safeClient, endpoint: endpoint, token: token, capabilities: caps, managed: managed, requireACK: requireACK}, nil
}

// CheckManaged requires an authenticated, read-only declaration from the
// adapter. The operator still owns verification of its stop implementation.
func (e *Executor) CheckManaged(ctx context.Context) error {
	if !e.managed {
		return nil
	}
	var declaration struct {
		Version                   int      `json:"version"`
		Capabilities              []string `json:"capabilities"`
		Managed                   bool     `json:"managed"`
		StopFencesDelayedDelivery bool     `json:"stop_fences_delayed_delivery"`
	}
	if err := execution.GetAdapterJSON(ctx, e.client, e.endpoint, "Bearer "+e.token, &declaration); err != nil {
		return fmt.Errorf("managed adapter admission: %w", err)
	}
	if declaration.Version != 1 || !declaration.Managed || !declaration.StopFencesDelayedDelivery {
		return fmt.Errorf("adapter does not declare the managed execution and delayed-delivery guarantees")
	}
	for _, operation := range []string{"deliver", "status", "stop"} {
		if !slices.Contains(declaration.Capabilities, operation) {
			return fmt.Errorf("managed adapter does not declare %s", operation)
		}
	}
	return nil
}

func (e *Executor) Supports(operation string) bool { return e.capabilities[operation] }
func (e *Executor) MonitoringEnabled() bool        { return e.managed }
func (e *Executor) NotificationACKRequired() bool  { return e.requireACK }

func (e *Executor) Validate(route execution.Route, allocate bool) error {
	if allocate && route.Session == "" && !e.Supports("prepare") && !e.Supports("placement") {
		return fmt.Errorf("adapter needs prepare or placement for an unresolved route")
	}
	return nil
}

func (e *Executor) Prepare(ctx context.Context, route execution.Route, task execution.Delivery) (string, error) {
	return e.prepare(ctx, route, task, nil, "")
}

func (e *Executor) PrepareAllocation(ctx context.Context, route execution.Route, task execution.Delivery, allocation execution.ConversationAllocation, mode string) (string, error) {
	return e.prepare(ctx, route, task, &allocation, mode)
}

func (e *Executor) prepare(ctx context.Context, route execution.Route, task execution.Delivery, allocation *execution.ConversationAllocation, mode string) (string, error) {
	res, err := e.call(ctx, Request{Operation: "prepare", Route: route, Task: &task, Allocation: allocation, Mode: mode})
	if err == nil && strings.TrimSpace(res.Session) == "" {
		err = fmt.Errorf("adapter did not return a session")
	}
	return res.Session, err
}

func (e *Executor) ResolveReviewPlacement(ctx context.Context, route execution.Route, task execution.Delivery) (*execution.ReviewPlacement, error) {
	res, err := e.call(ctx, Request{Operation: "placement", Route: route, Task: &task})
	if err == nil && res.Placement == nil {
		err = fmt.Errorf("adapter did not return placement")
	}
	return res.Placement, err
}

func (e *Executor) ReadConversation(ctx context.Context, route execution.Route, task execution.Delivery, before string) (execution.ConversationPage, error) {
	res, err := e.call(ctx, Request{Operation: "conversation_context", Route: route, Task: &task, Before: before})
	if err != nil {
		return execution.ConversationPage{}, err
	}
	if res.Conversation == nil {
		return execution.ConversationPage{}, fmt.Errorf("adapter did not return conversation history")
	}
	return *res.Conversation, nil
}

func (e *Executor) Deliver(ctx context.Context, route execution.Route, task execution.Delivery) error {
	return e.accept(ctx, Request{Operation: "deliver", Route: route, Task: &task})
}

func (e *Executor) WakeTask(ctx context.Context, route execution.Route, task execution.Delivery) error {
	return e.accept(ctx, Request{Operation: "wake", Route: route, Task: &task})
}

func (e *Executor) Notify(ctx context.Context, route execution.Route, notification execution.Notification, requireACK bool) error {
	return e.accept(ctx, Request{Operation: "notify", Route: route, Notification: &notification, RequireACK: &requireACK})
}

func (e *Executor) NotifyTaskBlocked(ctx context.Context, route execution.Route, task execution.Delivery, reason string) error {
	return e.accept(ctx, Request{Operation: "blocked_notice", Route: route, Task: &task, Reason: reason})
}

func (e *Executor) Status(ctx context.Context, route execution.Route, task execution.Delivery) (execution.Receipt, error) {
	return e.monitor(ctx, "status", route, task)
}

func (e *Executor) Stop(ctx context.Context, route execution.Route, task execution.Delivery) (execution.Receipt, error) {
	return e.monitor(ctx, "stop", route, task)
}

func (e *Executor) monitor(ctx context.Context, operation string, route execution.Route, task execution.Delivery) (execution.Receipt, error) {
	res, err := e.call(ctx, Request{Operation: operation, Route: route, Task: &task})
	if err != nil {
		return execution.Receipt{}, err
	}
	if res.Receipt == nil {
		return execution.Receipt{}, fmt.Errorf("adapter did not return an execution receipt")
	}
	return *res.Receipt, nil
}

func (e *Executor) accept(ctx context.Context, req Request) error {
	res, err := e.call(ctx, req)
	if err == nil && res.Status != "accepted" {
		err = fmt.Errorf("adapter did not return acceptance")
	}
	return err
}

func (e *Executor) call(ctx context.Context, req Request) (Response, error) {
	var response Response
	if !e.Supports(req.Operation) {
		return response, fmt.Errorf("adapter does not support %s", req.Operation)
	}
	req.Version = 1
	var auth string
	if e.token != "" {
		auth = "Bearer " + e.token
	}
	err := execution.PostJSONBounded(ctx, e.client, e.endpoint, auth, req, &response)
	if err != nil {
		return Response{}, fmt.Errorf("adapter %s: %w", req.Operation, err)
	}
	return response, nil
}
