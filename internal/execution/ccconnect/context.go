package ccconnect

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"ink-commons/internal/execution"
)

var _ execution.ConversationReader = (*Executor)(nil)

// Only public threads under ordinary text channels are eligible. In particular,
// a channel-level session must not expose unrelated channel conversations.
func (e *Executor) publicDevelopmentThread(ctx context.Context, source execution.Route) (string, error) {
	if source.Provider != Name || e.discordToken == "" {
		return "", fmt.Errorf("development thread access requires cc-connect and INK_DISCORD_TOKEN")
	}
	id, err := discordChannel(source.Session)
	if err != nil {
		return "", err
	}
	var thread struct {
		ID     string `json:"id"`
		Type   int    `json:"type"`
		Parent string `json:"parent_id"`
	}
	if err := execution.GetJSON(ctx, e.client, "https://discord.com/api/v10/channels/"+id, "Bot "+e.discordToken, &thread); err != nil {
		return "", fmt.Errorf("read development thread %s: %w", id, err)
	}
	if thread.ID != id || thread.Type != 11 || !snowflake(thread.Parent) {
		return "", fmt.Errorf("development source must be a public Discord thread")
	}
	var parent struct {
		ID   string `json:"id"`
		Type int    `json:"type"`
	}
	if err := execution.GetJSON(ctx, e.client, "https://discord.com/api/v10/channels/"+thread.Parent, "Bot "+e.discordToken, &parent); err != nil {
		return "", fmt.Errorf("read development parent channel %s: %w", thread.Parent, err)
	}
	if parent.ID != thread.Parent || parent.Type != 0 {
		return "", fmt.Errorf("development thread requires a normal text-channel parent")
	}
	return id, nil
}

func (e *Executor) ReadConversation(ctx context.Context, source execution.Route, before string) (execution.ConversationPage, error) {
	page := execution.ConversationPage{Before: before, Messages: []execution.ConversationMessage{}}
	cursor, err := strconv.ParseUint(before, 10, 64)
	if err != nil || cursor == 0 {
		return page, fmt.Errorf("invalid history cursor")
	}
	id, err := e.publicDevelopmentThread(ctx, source)
	if err != nil {
		return page, err
	}
	page.Session = "discord:" + id
	var messages []execution.ConversationMessage
	const pageLimit = 50
	if err := execution.GetJSON(ctx, e.client, "https://discord.com/api/v10/channels/"+id+"/messages?limit=50&before="+before, "Bot "+e.discordToken, &messages); err != nil {
		return page, err
	}
	if len(messages) > pageLimit {
		return page, fmt.Errorf("unexpected Discord history page size")
	}
	if len(messages) == 0 {
		page.Warnings = append(page.Warnings, "No messages returned: this can mean no older messages or missing READ_MESSAGE_HISTORY permission. Do not infer the developer conversation is empty.")
	}
	// Validate cursor progress before selecting a bounded page. Discord returns
	// newest first; expose chronological order while retaining older pagination.
	previous := cursor
	for _, m := range messages {
		value, err := strconv.ParseUint(m.ID, 10, 64)
		if err != nil || value == 0 || value >= previous {
			return page, fmt.Errorf("Discord history did not advance the requested cursor")
		}
		previous = value
	}
	for _, m := range messages {
		if m.Content == "" {
			page.Warnings = append(page.Warnings, "Some messages expose no text. They may be attachment/embed-only or system messages, or content withheld by Discord; check MESSAGE_CONTENT configuration before claiming the history is complete.")
			break
		}
	}
	const maxPageBytes = 64 * 1024
	bytes := 0
	for _, m := range messages {
		encoded, err := json.Marshal(m)
		if err != nil {
			return page, fmt.Errorf("invalid conversation message")
		}
		if len(encoded) > maxPageBytes {
			return page, fmt.Errorf("conversation message exceeds page size; inspect the original Discord message")
		}
		if bytes+len(encoded) > maxPageBytes {
			break
		}
		page.Messages = append(page.Messages, m)
		bytes += len(encoded)
	}
	if len(page.Messages) > 0 && (len(messages) == pageLimit || len(page.Messages) < len(messages)) {
		page.NextBefore = page.Messages[len(page.Messages)-1].ID
		page.Truncated = true
	}
	for i, j := 0, len(page.Messages)-1; i < j; i, j = i+1, j-1 {
		page.Messages[i], page.Messages[j] = page.Messages[j], page.Messages[i]
	}
	return page, nil
}
