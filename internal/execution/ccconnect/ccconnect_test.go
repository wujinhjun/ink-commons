package ccconnect

import (
	"context"
	"encoding/json"
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
