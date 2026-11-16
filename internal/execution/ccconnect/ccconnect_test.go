package ccconnect

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ink-commons/internal/execution"
)

type testTransport func(*http.Request) (*http.Response, error)

func TestApprovalNotificationEntersDeveloperContext(t *testing.T) {
	if !(&Executor{}).NotificationACKRequired() {
		t.Fatal("cc-connect must require an agent receipt")
	}
	for _, response := range []string{`{"status":"accepted"}`, `{"status":"busy"}`} {
		t.Run(response, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Path != "/hook" || r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer hook-test" {
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				var input map[string]string
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
					t.Fatal(err)
				}
				if input["project"] != "developer" || input["session_key"] != "discord:123" || input["event"] != "ink-commons:review_approved" {
					t.Fatalf("not sent to developer context: %+v", input)
				}
				for _, expected := range []string{"https://github.com/acme/project/pull/42", "Commit: abc", "Reviewer: reviewer-bot", "#pullrequestreview-9", "Event: approval-event", "acknowledge the result", "not a fix task", "merge", "head has changed"} {
					if !strings.Contains(input["prompt"], expected) {
						t.Fatalf("missing %q: %s", expected, input["prompt"])
					}
				}
				for _, required := range []string{"ink notification ack --event 'approval-event'", "first=true", "first=false", "If the command fails"} {
					if !strings.Contains(input["prompt"], required) {
						t.Fatalf("missing receipt instruction %q", required)
					}
				}
				if strings.Contains(input["prompt"], "address its findings") {
					t.Fatal("approval interpreted as fix task")
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(response))}, nil
			})}
			e, err := New(client, "http://127.0.0.1:9111/hook", "hook-test", "")
			if err != nil {
				t.Fatal(err)
			}
			n := execution.Notification{EventID: "approval-event", Event: "review_approved", Repository: "acme/project", Number: 42, URL: "https://github.com/acme/project/pull/42", SHA: "abc", Reviewer: "reviewer-bot", ReviewID: 9, ReviewURL: "https://github.com/acme/project/pull/42#pullrequestreview-9"}
			err = e.Notify(context.Background(), execution.Route{Provider: Name, Target: "developer", Session: "discord:123"}, n)
			if (err == nil) != strings.Contains(response, "accepted") || calls != 1 {
				t.Fatalf("unexpected acceptance: calls=%d err=%v", calls, err)
			}
		})
	}
}

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

func TestLegacyWebhookCarriesReviewIdentityWithoutNewOperations(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/hook" {
			t.Errorf("unexpected new executor operation: %s", r.URL.Path)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if body["operation"] != "" || body["project"] != "reviewer" || body["session_key"] != "discord:123" {
			t.Error(body)
		}
		for _, text := range []string{"reviewer-app[bot]", "reviewer-cli", "Previous workflow feedback", "identity mismatch"} {
			if !strings.Contains(body["prompt"], text) {
				t.Errorf("prompt missing %q", text)
			}
		}
		_, _ = w.Write([]byte(`{"status":"accepted"}`))
	}))
	defer server.Close()
	executor, err := New(server.Client(), server.URL+"/hook", "secret", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := any(executor).(execution.Monitor); ok {
		t.Fatal("legacy provider unexpectedly requires new lifecycle operations")
	}
	err = executor.Deliver(context.Background(), execution.Route{Provider: Name, Target: "reviewer", Session: "discord:123"}, execution.Delivery{Role: "reviewer", GitHubLogin: "reviewer-app[bot]", GitHubCommand: "reviewer-cli", Feedback: "identity mismatch"})
	if err != nil || calls != 1 {
		t.Fatal(calls, err)
	}
}

func TestCooperativeWakeRequiresClaimBeforeAnyWork(t *testing.T) {
	var prompt string
	client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["event"] != "ink-commons:task-wake" || body["project"] != "developer" || r.Header.Get("Authorization") != "Bearer hook-test" {
			t.Fatal(body)
		}
		prompt = body["prompt"]
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"status":"accepted"}`))}, nil
	})}
	e, err := New(client, "http://127.0.0.1:9111/hook", "hook-test", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.WakeTask(context.Background(), execution.Route{Provider: Name, Target: "developer", Session: "discord:123"}, execution.Delivery{TaskID: "task-1", URL: "https://github.com/acme/project/pull/42"}); err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"not an execution authorization", "ink task claim --task task-1", "first=true", "first=false", "--handoff", "--request-id", "--new-sha", "--review-id", "After handoff, perform no more work", "never repeat code work"} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("missing %q", required)
		}
	}
	if strings.Contains(prompt, "hook-test") {
		t.Fatal("wake disclosed runtime credential")
	}
}

func TestMergeCompletionNotificationCarriesOutcomeAndReadOnlyStatusCommand(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		var input map[string]string
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Fatal(err)
		}
		if input["event"] != "ink-commons:pr_merged" || input["project"] != "developer" || input["session_key"] != "discord:123" {
			t.Fatal(input)
		}
		for _, required := range []string{`"state":"merged"`, `"auto_merge_enabled":true`, `"merge_sha":"def"`, "ink notification ack --event 'merge-event'", "ink notification status --event 'merge-event'", "Do not perform a merge yourself", "do not poll", "first=false"} {
			if !strings.Contains(input["prompt"], required) {
				t.Fatalf("completion missing %q", required)
			}
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"status":"accepted"}`))}, nil
	})}
	e, err := New(client, "http://127.0.0.1:9111/hook", "secret", "")
	if err != nil {
		t.Fatal(err)
	}
	n := execution.Notification{EventID: "merge-event", Event: "pr_merged", SHA: "abc", Merge: &execution.MergeStatus{Enabled: true, State: "merged", MergeSHA: "def"}}
	if err := e.Notify(context.Background(), execution.Route{Provider: Name, Target: "developer", Session: "discord:123"}, n); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("completion sent more than once")
	}
	n.Event = "unsupported"
	if err := e.Notify(context.Background(), execution.Route{Provider: Name, Target: "developer", Session: "discord:123"}, n); err == nil || calls != 1 {
		t.Fatal("unsupported event was delivered")
	}
}
