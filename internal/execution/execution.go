package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Route addresses an executor and an optional reusable session. Parent is an
// optional provider-specific destination for allocating a new session.
type Route struct {
	Provider string `json:"provider"`
	Target   string `json:"target"`
	Session  string `json:"session,omitempty"`
	Parent   string `json:"parent,omitempty"`
}

func (r Route) Valid(allocate bool) bool {
	return strings.TrimSpace(r.Provider) != "" && strings.TrimSpace(r.Target) != "" && (allocate || strings.TrimSpace(r.Session) != "")
}

// UnmarshalJSON accepts the original cc-connect route in persisted state and API requests.
func (r *Route) UnmarshalJSON(data []byte) error {
	type route Route
	var value struct {
		route
		Project    string `json:"project"`
		SessionKey string `json:"session_key"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	if value.Project != "" || value.SessionKey != "" {
		if value.route != (route{}) {
			return fmt.Errorf("cannot mix legacy and provider route fields")
		}
		*r = Route{Provider: "cc-connect", Target: value.Project, Session: value.SessionKey}
		return nil
	}
	*r = Route(value.route)
	return nil
}

// Delivery is the work description shared by execution providers.
type Delivery struct {
	Repository string `json:"repository"`
	Number     int    `json:"pull_number"`
	URL        string `json:"pull_url"`
	Role       string `json:"role"`
	SHA        string `json:"sha"`
	ReviewID   int64  `json:"review_id,omitempty"`
	ReviewURL  string `json:"review_url,omitempty"`
	// Source is the development conversation associated with this task.
	Source *Route `json:"source,omitempty"`
}

// Executor is the runtime boundary. Successful delivery means the provider
// accepted a task. GitHub observations still determine workflow completion.
type Executor interface {
	Validate(Route, bool) error
	Prepare(context.Context, Route, Delivery) (string, error)
	Deliver(context.Context, Route, Delivery) error
}

func ValidateDestination(providers map[string]Executor, route Route, allocate bool) error {
	if !route.Valid(allocate) {
		return fmt.Errorf("incomplete execution destination")
	}
	provider, ok := providers[route.Provider]
	if !ok {
		return fmt.Errorf("unknown execution provider")
	}
	return provider.Validate(route, allocate)
}
