package ink

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDeploymentGateBlocksPollingAndWrites(t *testing.T) {
	dir := t.TempDir()
	gate := filepath.Join(dir, "enabled")
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.Write([]byte(`[]`)) }))
	defer upstream.Close()
	cfg := Config{PollGate: gate, Repositories: []Repository{{Repo: "acme/project", ReviewerLogin: "reviewer"}}}
	service, err := NewService(cfg, filepath.Join(dir, "state.sqlite3"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	service.up.github = upstream.URL
	// HTTP mutations must be blocked too, so a concurrent hook cannot be lost on rollback.
	request := httptest.NewRequest(http.MethodPost, "/v1/pushes", nil)
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	if response.Code != 503 {
		t.Fatal(response.Code)
	}
	if service.pollReady() {
		t.Fatal("gate is closed")
	}
	check := httptest.NewRecorder()
	service.Handler().ServeHTTP(check, httptest.NewRequest(http.MethodGet, "/v1/self-check", nil))
	if check.Code != 200 || requests.Load() != 1 || service.up.lastOK.Load() == 0 || service.pollReady() {
		t.Fatal("self-check must use daemon upstream without opening gate", check.Code)
	}
	if err := os.WriteFile(gate, []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	if !service.pollReady() {
		t.Fatal("gate did not open")
	}
	service.Poll(context.Background())
	if requests.Load() < 2 || service.lastPoll.Load() == 0 {
		t.Fatal("poll did not resume")
	}
}

func TestServeDeploymentWaitsForGate(t *testing.T) {
	dir, err := os.MkdirTemp("", "ink-gate-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	gate := filepath.Join(dir, "gate")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, Config{PollSeconds: 1, PollGate: gate}, dir, nil) }()
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(dir, "ink.sock"))
	}}}
	deadline := time.Now().Add(3 * time.Second)
	for {
		response, err := client.Get("http://ink/healthz")
		if err == nil {
			var body map[string]string
			err = json.NewDecoder(response.Body).Decode(&body)
			response.Body.Close()
			if err != nil || body["revision"] != BuildRevision || body["gate"] != "closed" || body["last_poll"] != "" {
				t.Fatal(body, err)
			}
			break
		}
		if time.Now().After(deadline) {
			select {
			case serverErr := <-done:
				t.Fatal(serverErr)
			default:
			}
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown timed out")
	}
}

func TestConfigOnlyPermitsConfigWritesWithPollingDisabled(t *testing.T) {
	dir := t.TempDir()
	gate := filepath.Join(dir, "enabled")
	if err := os.WriteFile(gate, []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(Config{PollGate: gate, ConfigOnly: true}, filepath.Join(dir, "state.sqlite3"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	handler := service.Handler()
	const path = "/v1/config?key=reviewer_login"
	for _, test := range []struct {
		method, path, body string
		status             int
	}{
		{http.MethodPost, path, `{"value":"reviewer[bot]"}`, http.StatusOK},
		{http.MethodGet, path, "", http.StatusOK},
		{http.MethodDelete, path, "", http.StatusOK},
		{http.MethodPost, "/v1/pushes", "", http.StatusServiceUnavailable},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(test.method, test.path, strings.NewReader(test.body)))
		if response.Code != test.status {
			t.Fatalf("%s %s: got %d, want %d (%s)", test.method, test.path, response.Code, test.status, response.Body)
		}
	}
	if service.pollReady() {
		t.Fatal("a preexisting deployment gate file enabled polling in config-only mode")
	}
}
