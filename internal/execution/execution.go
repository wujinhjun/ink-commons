package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
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
	ReuseDevelopmentThread bool   `json:"reuse_development_thread,omitempty"`
	ContextBefore          string `json:"context_before,omitempty"`
	GitHubLogin            string `json:"github_login,omitempty"`
	GitHubCommand          string `json:"github_command,omitempty"`
	Feedback               string `json:"feedback,omitempty"`
	TaskID                 string `json:"task_id,omitempty"`
	AttemptID              string `json:"attempt_id,omitempty"`
	Attempt                int    `json:"attempt,omitempty"`
	Repository             string `json:"repository"`
	Number                 int    `json:"pull_number"`
	URL                    string `json:"pull_url"`
	Role                   string `json:"role"`
	SHA                    string `json:"sha"`
	ReviewID               int64  `json:"review_id,omitempty"`
	ReviewURL              string `json:"review_url,omitempty"`
	// Source is the development conversation associated with this task.
	Source *Route `json:"source,omitempty"`
}

// ConversationReader reads only the provided source conversation. Pages are
// bounded and returned chronologically; NextBefore reads an older page.
type ConversationReader interface {
	ReadConversation(context.Context, Route, string) (ConversationPage, error)
}

type ConversationPage struct {
	TaskID       string                `json:"task_id,omitempty"`
	SHA          string                `json:"sha,omitempty"`
	ReviewBefore string                `json:"review_before,omitempty"`
	Warnings     []string              `json:"warnings,omitempty"`
	Session      string                `json:"session"`
	Before       string                `json:"before"`
	NextBefore   string                `json:"next_before,omitempty"`
	Truncated    bool                  `json:"truncated"`
	Messages     []ConversationMessage `json:"messages"`
}

type ConversationMessage struct {
	ID              string                   `json:"id"`
	Author          ConversationAuthor       `json:"author"`
	Timestamp       string                   `json:"timestamp"`
	EditedTimestamp *string                  `json:"edited_timestamp,omitempty"`
	Content         string                   `json:"content"`
	Attachments     []ConversationAttachment `json:"attachments,omitempty"`
	Embeds          []ConversationEmbed      `json:"embeds,omitempty"`
}

type ConversationEmbed struct {
	Title       string                   `json:"title,omitempty"`
	Description string                   `json:"description,omitempty"`
	URL         string                   `json:"url,omitempty"`
	Fields      []ConversationEmbedField `json:"fields,omitempty"`
}

type ConversationEmbedField struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type ConversationAuthor struct {
	ID   string `json:"id"`
	Name string `json:"username"`
	Bot  bool   `json:"bot"`
}

type ConversationAttachment struct {
	Name string `json:"filename"`
	URL  string `json:"url"`
}

// Executor is the runtime boundary. Successful delivery means the provider
// accepted a task. GitHub observations still determine workflow completion.
type Executor interface {
	Validate(Route, bool) error
	Prepare(context.Context, Route, Delivery) (string, error)
	Deliver(context.Context, Route, Delivery) error
}

// MergeStatus is a SHA-bound observation, not permission to perform a merge.
type MergeStatus struct {
	Enabled  bool   `json:"auto_merge_enabled"`
	Method   string `json:"method,omitempty"`
	State    string `json:"state"`
	Reason   string `json:"reason,omitempty"`
	MergeSHA string `json:"merge_sha,omitempty"`
}

// Notification is informational context, separate from authorization to perform work.
type Notification struct {
	EventID    string       `json:"event_id"`
	Event      string       `json:"event"`
	Repository string       `json:"repository"`
	Number     int          `json:"pull_number"`
	URL        string       `json:"pull_url"`
	SHA        string       `json:"sha"`
	Reviewer   string       `json:"reviewer"`
	ReviewID   int64        `json:"review_id"`
	ReviewURL  string       `json:"review_url"`
	Merge      *MergeStatus `json:"merge,omitempty"`
}

// Notifier is optional: runtimes must explicitly opt in rather than interpreting
// an approval notification as a developer fix task. Success is transport
// acceptance, not proof that the agent consumed the notification.
type Notifier interface {
	Notify(context.Context, Route, Notification) error
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

// PreparationValidator is optional so providers can reject missing preparation
// context at assignment time without adding side effects to route validation.
type PreparationValidator interface {
	ValidatePreparation(Route, Delivery) error
}

func ValidatePreparation(providers map[string]Executor, route Route, task Delivery) error {
	if provider, ok := providers[route.Provider].(PreparationValidator); ok {
		return provider.ValidatePreparation(route, task)
	}
	return nil
}

// Receipt is an authenticated snapshot obtained from the executor, not a
// workflow result. Terminal states promise the attempt can no longer act.
type Receipt struct {
	TaskID      string    `json:"task_id"`
	AttemptID   string    `json:"attempt_id"`
	Sequence    int64     `json:"sequence"`
	State       string    `json:"state"`
	StartedAt   time.Time `json:"started_at,omitempty"`
	HeartbeatAt time.Time `json:"heartbeat_at,omitempty"`
	FinishedAt  time.Time `json:"finished_at,omitempty"`
}

func Terminal(state string) bool {
	return state == "succeeded" || state == "failed" || state == "stopped"
}

// Monitor is optional. Status must use durable executor ownership, and Stop
// must fence delayed deliveries as well as stop all side effects before returning
// a terminal receipt. A missing record is unknown, never proof of termination.
type Monitor interface {
	Status(context.Context, Route, Delivery) (Receipt, error)
	Stop(context.Context, Route, Delivery) (Receipt, error)
}

// NotificationACKProvider opts into retries until the recipient explicitly
// acknowledges the event. Notify must carry instructions for that protocol.
type NotificationACKProvider interface {
	NotificationACKRequired() bool
}

// TaskWaker sends a retryable invitation to claim a task. Receiving a wake must
// not authorize work; the recipient must acquire the task from Ink first.
type TaskWaker interface {
	WakeTask(context.Context, Route, Delivery) error
}
