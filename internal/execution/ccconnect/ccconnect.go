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
var _ execution.PreparationValidator = (*Executor)(nil)

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
		if !allocate || (route.Parent != "" && !snowflake(route.Parent)) || e.discordToken == "" {
			return fmt.Errorf("new cc-connect session requires INK_DISCORD_TOKEN and a valid parent or development conversation")
		}
		return nil
	}
	_, err := discordChannel(route.Session)
	return err
}

func (e *Executor) Prepare(ctx context.Context, route execution.Route, task execution.Delivery) (string, error) {
	parent := route.Parent
	name := fmt.Sprintf("Review %s #%d", task.Repository, task.Number)
	if source := task.Source; source != nil && source.Provider == Name {
		sourceParent, sourceName, err := e.developmentConversation(ctx, source.Session)
		if err != nil && parent == "" {
			return "", err
		}
		if err == nil {
			if parent == "" {
				parent = sourceParent
			}
			// Keep the PR identifier even when a long Unicode title must be truncated.
			prefix := fmt.Sprintf("Review %s #%d · ", task.Repository, task.Number)
			if len([]rune(prefix)) > 90 {
				prefix = fmt.Sprintf("Review #%d · ", task.Number)
			}
			title := []rune(sourceName)
			name = prefix + string(title[:min(len(title), 100-len([]rune(prefix)))])
		}
	}
	if !snowflake(parent) {
		return "", fmt.Errorf("new review session needs a Discord development conversation or an explicit parent")
	}
	if len([]rune(name)) > 100 {
		name = string([]rune(name)[:100])
	}
	body := map[string]any{"name": name, "type": 11}
	var thread struct {
		ID string `json:"id"`
	}
	err := execution.PostJSON(ctx, e.client, "https://discord.com/api/v10/channels/"+parent+"/threads", "Bot "+e.discordToken, body, &thread)
	if err != nil {
		return "", err
	}
	if !snowflake(thread.ID) {
		return "", fmt.Errorf("Discord returned an invalid thread ID")
	}
	return "discord:" + thread.ID, nil
}

// ValidatePreparation performs the local checks that can be reported immediately
// by assignment, without a Discord request or allocating a session.
func (e *Executor) ValidatePreparation(route execution.Route, task execution.Delivery) error {
	if route.Session != "" || route.Parent != "" {
		return nil
	}
	if task.Source == nil || task.Source.Provider != Name {
		return fmt.Errorf("new review session needs a Discord development conversation or an explicit parent")
	}
	if _, err := discordChannel(task.Source.Session); err != nil {
		return fmt.Errorf("invalid development conversation: %w", err)
	}
	return nil
}

func (e *Executor) developmentConversation(ctx context.Context, session string) (string, string, error) {
	channelID, err := discordChannel(session)
	if err != nil {
		return "", "", fmt.Errorf("invalid development conversation: %w", err)
	}
	var channel struct {
		ID     string `json:"id"`
		Type   int    `json:"type"`
		Parent string `json:"parent_id"`
		Name   string `json:"name"`
	}
	if err := execution.GetJSON(ctx, e.client, "https://discord.com/api/v10/channels/"+channelID, "Bot "+e.discordToken, &channel); err != nil {
		return "", "", fmt.Errorf("read development conversation: %w", err)
	}
	if channel.ID != channelID || strings.TrimSpace(channel.Name) == "" {
		return "", "", fmt.Errorf("Discord returned an invalid development conversation")
	}
	parent := channel.ID
	switch channel.Type {
	case 11:
		parent = channel.Parent
	case 0: // A development session in a normal text channel.
	case 10:
		return "", "", fmt.Errorf("automatic review placement does not support announcement threads; specify an explicit text-channel parent")
	case 12:
		return "", "", fmt.Errorf("automatic review placement does not support private threads; specify an explicit public review parent or an existing review session")
	default:
		return "", "", fmt.Errorf("development conversation must be a public Discord thread or text channel")
	}
	if !snowflake(parent) {
		return "", "", fmt.Errorf("development conversation has no valid parent channel")
	}
	if channel.Type == 11 {
		var parentChannel struct {
			ID   string `json:"id"`
			Type int    `json:"type"`
		}
		if err := execution.GetJSON(ctx, e.client, "https://discord.com/api/v10/channels/"+parent, "Bot "+e.discordToken, &parentChannel); err != nil {
			return "", "", fmt.Errorf("read development parent channel: %w", err)
		}
		if parentChannel.ID != parent || parentChannel.Type != 0 {
			return "", "", fmt.Errorf("automatic review placement requires a text-channel parent; forum, media and announcement parents are unsupported")
		}
	}
	return parent, channel.Name, nil
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

// Discord sessions can optionally carry a user ID after the channel ID.
func discordChannel(session string) (string, error) {
	platform, rest, ok := strings.Cut(session, ":")
	channel, user, hasUser := strings.Cut(rest, ":")
	if !ok || platform != "discord" || !snowflake(channel) || (hasUser && !snowflake(user)) {
		return "", fmt.Errorf("invalid cc-connect Discord session")
	}
	return channel, nil
}
