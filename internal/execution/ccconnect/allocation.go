package ccconnect

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/url"
	"time"

	"ink-commons/internal/execution"
)

const discordAPI = "https://discord.com/api/v10"

func (e *Executor) PlanConversation(placement execution.ReviewPlacement, task execution.Delivery) execution.ConversationAllocation {
	id := sha256.Sum256([]byte(task.TaskID))
	return threadAllocation(placement, task, fmt.Sprintf(" · %x", id[:8]))
}

func threadAllocation(placement execution.ReviewPlacement, task execution.Delivery, suffix string) execution.ConversationAllocation {
	name := fmt.Sprintf("Review %s #%d", task.Repository, task.Number)
	if placement.DevelopmentName != "" {
		if len([]rune(name))+len([]rune(suffix)) > 87 {
			name = fmt.Sprintf("Review #%d", task.Number)
		}
		name += " · " + placement.DevelopmentName
	} else if suffix != "" && len([]rune(name))+len([]rune(suffix)) > 100 {
		name = fmt.Sprintf("Review #%d", task.Number)
	}
	runes := []rune(name)
	name = string(runes[:min(len(runes), 100-len([]rune(suffix)))]) + suffix
	return execution.ConversationAllocation{Parent: placement.Parent, Name: name}
}

func (e *Executor) CreateConversation(ctx context.Context, allocation execution.ConversationAllocation) (string, error) {
	if !snowflake(allocation.Parent) {
		return "", fmt.Errorf("new review session needs a Discord development conversation or an explicit parent")
	}
	body := map[string]any{"name": allocation.Name, "type": 11}
	var thread struct {
		ID string `json:"id"`
	}
	err := execution.PostJSON(ctx, e.client, discordAPI+"/channels/"+allocation.Parent+"/threads", "Bot "+e.discordToken, body, &thread)
	if err != nil {
		return "", fmt.Errorf("create Discord thread in channel %s: %w", allocation.Parent, err)
	}
	if !snowflake(thread.ID) {
		return "", fmt.Errorf("create Discord thread in channel %s: Discord returned an invalid thread ID", allocation.Parent)
	}
	return "discord:" + thread.ID, nil
}

type threadPage struct {
	Threads []struct {
		discordConversation
		Owner    string `json:"owner_id"`
		Metadata struct {
			ArchivedAt time.Time `json:"archive_timestamp"`
		} `json:"thread_metadata"`
	} `json:"threads"`
	HasMore bool `json:"has_more"`
}

func (e *Executor) FindConversation(ctx context.Context, allocation execution.ConversationAllocation) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if !snowflake(allocation.Parent) || allocation.Name == "" {
		return "", fmt.Errorf("invalid Discord thread allocation")
	}
	parent, err := e.readDiscordChannel(ctx, allocation.Parent)
	if err != nil {
		return "", fmt.Errorf("read Discord allocation channel %s: %w", allocation.Parent, err)
	}
	if !snowflake(parent.Guild) {
		return "", fmt.Errorf("Discord allocation channel %s has no guild ID", allocation.Parent)
	}
	auth := "Bot " + e.discordToken
	var bot struct {
		ID string `json:"id"`
	}
	if err := execution.GetJSON(ctx, e.client, discordAPI+"/users/@me", auth, &bot); err != nil {
		return "", fmt.Errorf("read Discord bot identity: %w", err)
	}
	if !snowflake(bot.ID) {
		return "", fmt.Errorf("Discord returned an invalid bot ID")
	}
	var match string
	check := func(page threadPage) error {
		for _, thread := range page.Threads {
			if thread.Type != 11 || thread.Parent != allocation.Parent || thread.Owner != bot.ID || thread.Name != allocation.Name || !snowflake(thread.ID) {
				continue
			}
			if match != "" && match != thread.ID {
				return fmt.Errorf("multiple Discord threads match allocation %q", allocation.Name)
			}
			match = thread.ID
		}
		return nil
	}
	var active threadPage
	if err := execution.GetJSON(ctx, e.client, discordAPI+"/guilds/"+parent.Guild+"/threads/active", auth, &active); err != nil {
		return "", fmt.Errorf("list active Discord threads for channel %s: %w", allocation.Parent, err)
	}
	if err := check(active); err != nil {
		return "", err
	}
	archivedURL := discordAPI + "/channels/" + allocation.Parent + "/threads/archived/public?limit=100"
	var before time.Time
	for {
		endpoint := archivedURL
		if !before.IsZero() {
			endpoint += "&before=" + url.QueryEscape(before.Format(time.RFC3339Nano))
		}
		var archived threadPage
		if err := execution.GetJSON(ctx, e.client, endpoint, auth, &archived); err != nil {
			return "", fmt.Errorf("list archived Discord threads for channel %s: %w", allocation.Parent, err)
		}
		if err := check(archived); err != nil {
			return "", err
		}
		if !archived.HasMore {
			break
		}
		if len(archived.Threads) == 0 {
			return "", fmt.Errorf("Discord archive pagination returned an empty page")
		}
		next := archived.Threads[len(archived.Threads)-1].Metadata.ArchivedAt
		if next.IsZero() || (!before.IsZero() && !next.Before(before)) {
			return "", fmt.Errorf("Discord archive pagination did not advance")
		}
		before = next
	}
	if match == "" {
		return "", fmt.Errorf("Discord thread %q was not found in channel %s", allocation.Name, allocation.Parent)
	}
	return "discord:" + match, nil
}
