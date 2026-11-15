package ink

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	if err := os.WriteFile(gate, []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	if !service.pollReady() {
		t.Fatal("gate did not open")
	}
	service.Poll(context.Background())
	if requests.Load() == 0 {
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
			if err != nil || body["revision"] != BuildRevision {
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
