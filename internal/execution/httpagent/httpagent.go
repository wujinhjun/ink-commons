package httpagent

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"ink-commons/internal/execution"
)

const Name = "http"

var _ execution.Executor = (*Executor)(nil)

func New(client *http.Client, endpoint, token string) (*Executor, error) {
	u, err := execution.Endpoint(endpoint)
	if err != nil || (!execution.IsLoopback(u) && (u.Scheme != "https" || token == "")) {
		return nil, fmt.Errorf("http_provider_url requires HTTPS and INK_HTTP_PROVIDER_TOKEN for remote endpoints")
	}
	return &Executor{client: client, endpoint: endpoint, token: token}, nil
}

type Executor struct {
	client   *http.Client
	endpoint string
	token    string
}

func (*Executor) Validate(execution.Route, bool) error { return nil }

func (e *Executor) Prepare(ctx context.Context, route execution.Route, task execution.Delivery) (string, error) {
	response, err := e.call(ctx, "prepare", route, task)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(response.Session) == "" {
		return "", fmt.Errorf("HTTP provider did not return a session")
	}
	return response.Session, nil
}

func (e *Executor) Deliver(ctx context.Context, route execution.Route, task execution.Delivery) error {
	response, err := e.call(ctx, "deliver", route, task)
	if err != nil {
		return err
	}
	if response.Status != "accepted" {
		return fmt.Errorf("HTTP provider did not accept the task")
	}
	return nil
}

type providerResponse struct {
	Session string `json:"session"`
	Status  string `json:"status"`
}

func (e *Executor) call(ctx context.Context, operation string, route execution.Route, task execution.Delivery) (providerResponse, error) {
	body := struct {
		Version   int                `json:"version"`
		Operation string             `json:"operation"`
		Route     execution.Route    `json:"route"`
		Task      execution.Delivery `json:"task"`
	}{Version: 1, Operation: operation, Route: route, Task: task}
	var response providerResponse
	var auth string
	if e.token != "" {
		auth = "Bearer " + e.token
	}
	err := execution.PostJSON(ctx, e.client, e.endpoint, auth, body, &response)
	return response, err
}
