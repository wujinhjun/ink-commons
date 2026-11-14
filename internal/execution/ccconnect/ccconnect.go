package ccconnect

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"ink-commons/internal/execution"
)

const Name = "cc-connect"

type Executor struct {
	client       *http.Client
	endpoint     string
	token        string
	discordToken string
}

var _ execution.Executor = (*Executor)(nil)

func New(client *http.Client, endpoint, token, discordToken string) (*Executor, error) {
	if endpoint != "" {
		u, err := execution.Endpoint(endpoint)
		if err != nil || !execution.IsLoopback(u) || u.Scheme != "http" || token == "" {
			return nil, fmt.Errorf("cc_connect_url requires a loopback HTTP endpoint and INK_CC_CONNECT_TOKEN")
		}
	}
	return &Executor{client: client, endpoint: endpoint, token: token, discordToken: discordToken}, nil
}

func RouteFromEnv(getenv func(string) string) execution.Route {
	return execution.Route{Provider: Name, Target: getenv("CC_PROJECT"), Session: getenv("CC_SESSION_KEY")}
}

func snowflake(value string) bool {
	return value != "" && strings.Trim(value, "0123456789") == ""
}

func (e *Executor) Validate(route execution.Route, allocate bool) error {
	if route.Session == "" {
		if !allocate || !snowflake(route.Parent) || e.discordToken == "" {
			return fmt.Errorf("new cc-connect session requires a Discord parent and INK_DISCORD_TOKEN")
		}
		return nil
	}
	platform, remainder, ok := strings.Cut(route.Session, ":")
	channel, user, hasUser := strings.Cut(remainder, ":")
	if !ok || platform != "discord" || !snowflake(channel) || (hasUser && !snowflake(user)) {
		return fmt.Errorf("invalid cc-connect Discord session")
	}
	return nil
}

func (e *Executor) Prepare(ctx context.Context, route execution.Route, task execution.Delivery) (string, error) {
	name := fmt.Sprintf("Review %s #%d", task.Repository, task.Number)
	if len(name) > 100 {
		name = name[:100]
	}
	body := map[string]any{"name": name, "type": 11}
	var thread struct {
		ID string `json:"id"`
	}
	err := execution.PostJSON(ctx, e.client, "https://discord.com/api/v10/channels/"+route.Parent+"/threads", "Bot "+e.discordToken, body, &thread)
	if err != nil {
		return "", err
	}
	if !snowflake(thread.ID) {
		return "", fmt.Errorf("Discord returned an invalid thread ID")
	}
	return "discord:" + thread.ID, nil
}

func (e *Executor) Deliver(ctx context.Context, route execution.Route, task execution.Delivery) error {
	if e.endpoint == "" {
		return fmt.Errorf("cc-connect endpoint is not configured")
	}
	prompt := fmt.Sprintf("Ink Commons %s task for PR %s\nCommit: %s\n", task.Role, task.URL, task.SHA)
	if task.Role == "reviewer" {
		prompt += "Review this exact commit and submit the official review in GitHub against that commit. Use Approve or Request changes."
	} else {
		prompt += "Read the GitHub review at " + task.ReviewURL + " and address its findings. Check that the PR head is still the indicated commit before working."
	}
	body := map[string]string{"project": route.Target, "session_key": route.Session, "prompt": prompt, "event": "ink-commons:" + task.Role}
	var ack struct {
		Status string `json:"status"`
	}
	err := execution.PostJSON(ctx, e.client, e.endpoint, "Bearer "+e.token, body, &ack)
	if err != nil {
		return err
	}
	if ack.Status != "accepted" {
		return fmt.Errorf("cc-connect did not accept the task")
	}
	return nil
}
