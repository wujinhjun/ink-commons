package ccconnect

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/wujinhjun/ink-commons/internal/execution"
)

func contextExecutor(t *testing.T, kind int, messages []execution.ConversationMessage, inspect func(*http.Request)) *Executor {
	t.Helper()
	client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		if inspect != nil {
			inspect(r)
		}
		if r.Header.Get("Authorization") != "Bot discord-test" {
			t.Fatal("wrong history identity")
		}
		var body any
		switch r.URL.Path {
		case "/api/v10/channels/123":
			body = map[string]any{"id": "123", "type": kind, "parent_id": "77"}
		case "/api/v10/channels/77":
			body = map[string]any{"id": "77", "type": 0}
		case "/api/v10/channels/123/messages":
			if r.URL.Query().Get("limit") != "50" || r.URL.Query().Get("before") == "" {
				t.Fatal("history must be bounded")
			}
			body = messages
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL)
			return nil, nil
		}
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(b)))}, nil
	})}
	e, err := New(client, "", "", "discord-test")
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestReuseDevelopmentThreadKeepsIndependentProject(t *testing.T) {
	source := &execution.Route{Provider: Name, Target: "developer", Session: "discord:123:456"}
	e := contextExecutor(t, 11, nil, func(r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatal("reusing a thread must not create one")
		}
	})
	route := execution.Route{Provider: Name, Target: "reviewer"}
	task := execution.Delivery{ReuseDevelopmentThread: true, Source: source}
	session, err := e.Prepare(context.Background(), route, task)
	if err != nil || session != "discord:123" || source.Target != "developer" || route.Target != "reviewer" {
		t.Fatalf("reuse: %s %v", session, err)
	}
	route.Target = "developer"
	if _, err := e.Prepare(context.Background(), route, task); err == nil {
		t.Fatal("reused developer's own agent context")
	}
	task.Source = nil
	route.Target = "reviewer"
	if _, err := e.Prepare(context.Background(), route, task); err == nil {
		t.Fatal("reused without source")
	}
}

func TestHistoryRejectsPrivateAndChannelSources(t *testing.T) {
	for _, kind := range []int{0, 10, 12, 15} {
		e := contextExecutor(t, kind, nil, func(r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/messages") {
				t.Fatal("read unsupported source")
			}
		})
		if _, err := e.ReadConversation(context.Background(), execution.Route{Provider: Name, Session: "discord:123"}, "1000"); err == nil {
			t.Fatalf("accepted channel type %d", kind)
		}
	}
}

func TestConversationHistoryReturnsOriginalChronologicalMessages(t *testing.T) {
	messages := []execution.ConversationMessage{
		{ID: "900", Author: execution.ConversationAuthor{ID: "2", Name: "developer", Bot: true}, Content: "已按你的要求修改", Timestamp: "2026-11-16T01:00:01Z", Embeds: []execution.ConversationEmbed{{Description: "保留了所有接口"}}},
		{ID: "800", Author: execution.ConversationAuthor{ID: "1", Name: "user"}, Content: "必须保留所有接口", Attachments: []execution.ConversationAttachment{{Name: "requirement.png", URL: "https://cdn.discordapp.com/example.png"}}},
	}
	e := contextExecutor(t, 11, messages, nil)
	page, err := e.ReadConversation(context.Background(), execution.Route{Provider: Name, Session: "discord:123"}, "1000")
	if err != nil {
		t.Fatal(err)
	}
	if page.Messages[0].ID != "800" || page.Messages[0].Content != "必须保留所有接口" || page.Messages[0].Attachments[0].Name != "requirement.png" || !page.Messages[1].Author.Bot || page.NextBefore != "" || page.Truncated {
		t.Fatalf("lost original history: %+v", page)
	}
	if page.Messages[1].Embeds[0].Description != "保留了所有接口" {
		t.Fatal("lost bot embed text")
	}
}

func TestConversationHistoryPaginationAndByteLimit(t *testing.T) {
	for _, large := range []bool{false, true} {
		var messages []execution.ConversationMessage
		for i := 0; i < 50; i++ {
			text := "message"
			if large {
				text = strings.Repeat("字", 3000)
			}
			messages = append(messages, execution.ConversationMessage{ID: fmt.Sprint(999 - i), Content: text})
		}
		e := contextExecutor(t, 11, messages, nil)
		page, err := e.ReadConversation(context.Background(), execution.Route{Provider: Name, Session: "discord:123"}, "1000")
		if err != nil {
			t.Fatal(err)
		}
		if !page.Truncated || page.NextBefore != page.Messages[0].ID || len(page.Messages) == 0 || (large && len(page.Messages) >= 50) {
			t.Fatalf("bad pagination: %+v", page)
		}
		b, _ := json.Marshal(page)
		if len(b) > 66*1024 {
			t.Fatalf("page not bounded: %d", len(b))
		}
		if large && !strings.HasPrefix(page.Messages[0].Content, "字") {
			t.Fatal("Unicode content corrupted")
		}
	}
}

func TestConversationHistoryRejectsNonProgressingCursor(t *testing.T) {
	for _, ids := range [][]string{{"1000"}, {"900", "900"}, {"800", "900"}, {"invalid"}} {
		var messages []execution.ConversationMessage
		for _, id := range ids {
			messages = append(messages, execution.ConversationMessage{ID: id})
		}
		e := contextExecutor(t, 11, messages, nil)
		if _, err := e.ReadConversation(context.Background(), execution.Route{Provider: Name, Session: "discord:123"}, "1000"); err == nil {
			t.Fatal("invalid history cursor accepted")
		}
	}
}

func TestReviewPromptInstructsGatedHistoryRead(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(body["prompt"], "ink context") != enabled {
				t.Fatal("history read instruction does not match gate")
			}
			if enabled && (!strings.Contains(body["prompt"], "--sha abc --before 1000") || !strings.Contains(body["prompt"], "next_before")) {
				t.Fatal("missing review boundary/pagination")
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"status":"accepted"}`))}, nil
		})}
		e, _ := New(client, "http://127.0.0.1:9111/hook", "test", "")
		d := execution.Delivery{Role: "reviewer", SHA: "abc", URL: "https://github.com/acme/project/pull/42", Source: &execution.Route{Provider: Name, Session: "discord:123"}}
		if enabled {
			d.ContextBefore = "1000"
		}
		if err := e.Deliver(context.Background(), execution.Route{Provider: Name, Target: "reviewer", Session: "discord:123"}, d); err != nil {
			t.Fatal(err)
		}
	}
}

func TestExplicitPlacementOverridesThreadReuse(t *testing.T) {
	posts := 0
	e, _ := New(&http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		body := `{"id":"123","type":11,"parent_id":"77","name":"development"}`
		if r.URL.Path == "/api/v10/channels/77" {
			body = `{"id":"77","type":0}`
		}
		if r.Method == "POST" {
			posts++
			if r.URL.Path != "/api/v10/channels/88/threads" {
				t.Fatal("ignored explicit parent")
			}
			body = `{"id":"999"}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}, "", "", "discord-test")
	d := execution.Delivery{ReuseDevelopmentThread: true, Repository: "acme/project", Number: 42, Source: &execution.Route{Provider: Name, Target: "developer", Session: "discord:123"}}
	session, err := e.Prepare(context.Background(), execution.Route{Provider: Name, Target: "reviewer", Parent: "88"}, d)
	if err != nil || session != "discord:999" || posts != 1 {
		t.Fatalf("override: %s %v", session, err)
	}
	session, err = e.Prepare(context.Background(), execution.Route{Provider: Name, Target: "reviewer", Session: "discord:777"}, d)
	if err != nil || session != "discord:777" || posts != 1 {
		t.Fatal("existing session moved")
	}
}

func TestHistoryPermissionFailureIsReportedWithoutContent(t *testing.T) {
	e, _ := New(&http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader(`{"secret":"do-not-echo"}`))}, nil
	})}, "", "", "discord-test")
	page, err := e.ReadConversation(context.Background(), execution.Route{Provider: Name, Session: "discord:123"}, "1000")
	if err == nil || strings.Contains(err.Error(), "do-not-echo") || len(page.Messages) != 0 {
		t.Fatal("history error was hidden or leaked")
	}
	e = contextExecutor(t, 11, nil, nil)
	page, err = e.ReadConversation(context.Background(), execution.Route{Provider: Name, Session: "discord:123"}, "1000")
	if err != nil || len(page.Warnings) == 0 {
		t.Fatal("empty history silently claimed completeness")
	}
}

func TestEmptyTextWithAttachmentsOrEmbedsStillWarns(t *testing.T) {
	for _, m := range []execution.ConversationMessage{
		{ID: "900", Attachments: []execution.ConversationAttachment{{Name: "requirements.png", URL: "https://cdn.discordapp.com/requirements.png"}}},
		{ID: "900", Embeds: []execution.ConversationEmbed{{Description: "visible card"}}},
	} {
		e := contextExecutor(t, 11, []execution.ConversationMessage{m}, nil)
		page, err := e.ReadConversation(context.Background(), execution.Route{Provider: Name, Session: "discord:123"}, "1000")
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Warnings) == 0 || !strings.Contains(page.Warnings[0], "MESSAGE_CONTENT") {
			t.Fatal("empty text with other visible payload silently claimed completeness")
		}
		if len(page.Messages) != 1 || (len(page.Messages[0].Attachments) == 0 && len(page.Messages[0].Embeds) == 0) {
			t.Fatal("warning discarded available content")
		}
	}
}
