package ccconnect

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"ink-commons/internal/execution"
)

type testTransport func(*http.Request) (*http.Response, error)

func (f testTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestDiscordSessionAndWebhook(t *testing.T) {
	ctx := context.Background()
	var paths []string
	client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.Path)
		if r.Method != http.MethodPost {
			t.Fatalf("unexpected method %s", r.Method)
		}
		var ack string
		switch r.URL.Path {
		case "/api/v10/channels/77/threads":
			var input struct {
				Name string
				Type int
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Fatal(err)
			}
			if input.Name != "Review acme/project #42" || input.Type != 11 || r.Header.Get("Authorization") != "Bot discord-test" {
				t.Fatalf("unexpected thread request: %+v", input)
			}
			ack = `{"id":"99"}`
		case "/hook":
			var input map[string]string
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Fatal(err)
			}
			if input["project"] != "reviewer" || input["session_key"] != "discord:99" || !strings.Contains(input["prompt"], "Commit: abc") || r.Header.Get("Authorization") != "Bearer hook-test" {
				t.Fatalf("unexpected webhook request: %+v", input)
			}
			ack = `{"status":"accepted"}`
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(ack))}, nil
	})}
	executor, err := New(client, "http://127.0.0.1:9111/hook", "hook-test", "discord-test")
	if err != nil {
		t.Fatal(err)
	}
	route := execution.Route{Provider: Name, Target: "reviewer", Parent: "77"}
	task := execution.Delivery{Repository: "acme/project", Number: 42, Role: "reviewer", SHA: "abc"}
	if err := executor.Validate(route, true); err != nil {
		t.Fatal(err)
	}
	route.Session, err = executor.Prepare(ctx, route, task)
	if err != nil {
		t.Fatal(err)
	}
	if err := executor.Validate(route, false); err != nil {
		t.Fatal(err)
	}
	if err := executor.Deliver(ctx, route, task); err != nil {
		t.Fatal(err)
	}
	if strings.Join(paths, ",") != "/api/v10/channels/77/threads,/hook" {
		t.Fatalf("unexpected requests: %v", paths)
	}
}

func TestReviewThreadFollowsDevelopmentConversation(t *testing.T) {
	for _, tc := range []struct {
		name, session, response, override, parent string
	}{
		{"public thread", "discord:123", `{"id":"123","type":11,"parent_id":"77","name":"实现同砚 SQLite"}`, "", "77"},
		{"public thread with user", "discord:123:456", `{"id":"123","type":11,"parent_id":"77","name":"实现同砚 SQLite"}`, "", "77"},
		{"text channel", "discord:123", `{"id":"123","type":0,"name":"实现同砚 SQLite"}`, "", "123"},
		{"explicit parent", "discord:123", `{"id":"123","type":11,"parent_id":"77","name":"实现同砚 SQLite"}`, "88", "88"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var paths []string
			client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
				paths = append(paths, r.Method+" "+r.URL.Path)
				if r.Header.Get("Authorization") != "Bot discord-test" {
					t.Fatal("wrong Discord identity")
				}
				if r.Method == http.MethodGet && r.URL.Path == "/api/v10/channels/77" {
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"id":"77","type":0}`))}, nil
				}
				response := tc.response
				if r.Method == http.MethodPost {
					if r.URL.Path != "/api/v10/channels/"+tc.parent+"/threads" {
						t.Fatalf("wrong parent: %s", r.URL.Path)
					}
					var body struct {
						Name string
						Type int
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Fatal(err)
					}
					if body.Name != "Review acme/project #42 · 实现同砚 SQLite" || body.Type != 11 {
						t.Fatalf("thread not linked by name: %+v", body)
					}
					response = `{"id":"99"}`
				} else if r.Method != http.MethodGet || r.URL.Path != "/api/v10/channels/123" {
					t.Fatalf("unexpected lookup %s", r.URL.Path)
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(response))}, nil
			})}
			executor, _ := New(client, "", "", "discord-test")
			route := execution.Route{Provider: Name, Target: "reviewer", Parent: tc.override}
			if err := executor.Validate(route, true); err != nil {
				t.Fatal(err)
			}
			session, err := executor.Prepare(context.Background(), route, execution.Delivery{Repository: "acme/project", Number: 42, Role: "reviewer", Source: &execution.Route{Provider: Name, Target: "developer", Session: tc.session}})
			if err != nil || session != "discord:99" {
				t.Fatalf("prepare: %s, %v", session, err)
			}
			want := 3
			if tc.name == "text channel" {
				want = 2
			}
			if len(paths) != want {
				t.Fatalf("expected source/parent lookup then one allocation: %v", paths)
			}
		})
	}
}

func TestInvalidDevelopmentConversationDoesNotAllocateElsewhere(t *testing.T) {
	for _, tc := range []struct {
		name, session, response string
		status                  int
	}{
		{"inaccessible", "discord:123", `{}`, 403},
		{"missing parent", "discord:123", `{"id":"123","type":11,"name":"dev"}`, 200},
		{"wrong channel", "discord:123", `{"id":"456","type":11,"parent_id":"77","name":"dev"}`, 200},
		{"unsupported channel", "discord:123", `{"id":"123","type":4,"name":"category"}`, 200},
		{"announcement thread", "discord:123", `{"id":"123","type":10,"parent_id":"77","name":"dev"}`, 200},
		{"private thread", "discord:123:456", `{"id":"123","type":12,"parent_id":"77","name":"private title"}`, 200},
		{"invalid session", "discord:123:456:789", `{}`, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodGet {
					t.Fatal("created a thread without a resolved source")
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.response))}, nil
			})}
			executor, _ := New(client, "", "", "discord-test")
			_, err := executor.Prepare(context.Background(), execution.Route{Provider: Name, Target: "reviewer"}, execution.Delivery{Repository: "acme/project", Number: 42, Role: "reviewer", Source: &execution.Route{Provider: Name, Target: "dev", Session: tc.session}})
			if err == nil {
				t.Fatal("unresolved source accepted")
			}
			if tc.name == "announcement thread" && !strings.Contains(err.Error(), "announcement threads") {
				t.Fatalf("missing announcement explanation: %v", err)
			}
			if tc.name == "private thread" && !strings.Contains(err.Error(), "private threads") {
				t.Fatalf("missing visibility explanation: %v", err)
			}
		})
	}
	executor, _ := New(&http.Client{Transport: testTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("allocated without source or parent")
		return nil, nil
	})}, "", "", "discord-test")
	if _, err := executor.Prepare(context.Background(), execution.Route{Provider: Name, Target: "reviewer"}, execution.Delivery{}); err == nil {
		t.Fatal("missing source accepted")
	}
}

func TestReviewThreadNamePreservesPRAndUnicode(t *testing.T) {
	var name string
	client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v10/channels/77" {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"id":"77","type":0}`))}, nil
		}
		response := `{"id":"123","type":11,"parent_id":"77","name":"` + strings.Repeat("同砚", 100) + `"}`
		if r.Method == http.MethodPost {
			var body struct{ Name string }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			name = body.Name
			response = `{"id":"99"}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(response))}, nil
	})}
	executor, _ := New(client, "", "", "discord-test")
	_, err := executor.Prepare(context.Background(), execution.Route{Provider: Name, Target: "reviewer"}, execution.Delivery{Repository: "acme/project", Number: 42, Source: &execution.Route{Provider: Name, Session: "discord:123"}})
	if err != nil {
		t.Fatal(err)
	}
	if len([]rune(name)) != 100 || !strings.HasPrefix(name, "Review acme/project #42 · 同砚") {
		t.Fatalf("invalid truncated name %q", name)
	}
}

func TestExplicitParentSurvivesUnavailableSource(t *testing.T) {
	for _, tc := range []struct {
		name, session, response string
		status                  int
	}{
		{"forbidden", "discord:123", `{}`, 403},
		{"deleted", "discord:123", `{}`, 404},
		{"invalid session", "discord:123:invalid", `{}`, 200},
		{"announcement source", "discord:123", `{"id":"123","type":10,"parent_id":"77","name":"announcement"}`, 200},
		{"private source", "discord:123", `{"id":"123","type":12,"parent_id":"77","name":"private title"}`, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			created := false
			client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodGet {
					return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.response))}, nil
				}
				if r.Method != http.MethodPost || r.URL.Path != "/api/v10/channels/88/threads" {
					t.Fatalf("wrong destination: %s %s", r.Method, r.URL.Path)
				}
				var body struct {
					Name string
					Type int
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if body.Name != "Review acme/project #42" || body.Type != 11 {
					t.Fatalf("unexpected fallback or leaked source title: %+v", body)
				}
				created = true
				return &http.Response{StatusCode: 201, Body: io.NopCloser(strings.NewReader(`{"id":"99"}`))}, nil
			})}
			executor, _ := New(client, "", "", "discord-test")
			session, err := executor.Prepare(context.Background(), execution.Route{Provider: Name, Target: "reviewer", Parent: "88"}, execution.Delivery{Repository: "acme/project", Number: 42, Source: &execution.Route{Provider: Name, Session: tc.session}})
			if err != nil || session != "discord:99" || !created {
				t.Fatalf("explicit parent failed: %s, %v", session, err)
			}
		})
	}
}

func TestAutomaticReviewRejectsNonTextParents(t *testing.T) {
	for _, parentType := range []int{5, 15, 16} {
		t.Run(fmt.Sprint(parentType), func(t *testing.T) {
			client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodGet {
					t.Fatal("allocated a review under a non-text parent")
				}
				response := `{"id":"123","type":11,"parent_id":"77","name":"dev"}`
				if r.URL.Path == "/api/v10/channels/77" {
					response = fmt.Sprintf(`{"id":"77","type":%d}`, parentType)
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(response))}, nil
			})}
			executor, _ := New(client, "", "", "discord-token")
			_, err := executor.Prepare(context.Background(), execution.Route{Provider: Name, Target: "reviewer"}, execution.Delivery{Source: &execution.Route{Provider: Name, Session: "discord:123"}})
			if err == nil || !strings.Contains(err.Error(), "text-channel parent") {
				t.Fatalf("missing parent-type rejection: %v", err)
			}
		})
	}
}
