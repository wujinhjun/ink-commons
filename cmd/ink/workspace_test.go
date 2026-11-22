package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ink-commons/internal/workspace"
)

func TestWorkspaceListFallsBackWhenDaemonDisablesAPI(t *testing.T) {
	dir, err := os.MkdirTemp("", "ink-ws-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	t.Setenv("INK_DATA_DIR", dir)
	ln, err := net.Listen("unix", filepath.Join(dir, "ink.sock"))
	if err != nil {
		t.Fatal(err)
	}
	calls := make(chan string, 2)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls <- r.Method
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"workspace operations require an unprivileged daemon"}`))
	})}
	go server.Serve(ln)
	defer server.Close()

	source := t.TempDir()
	if _, err := workspace.Git(context.Background(), source, "init", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	if err := workspaceCommand([]string{"list", "--repo", source, "--json"}); err != nil {
		t.Fatalf("local inventory should remain available: %v", err)
	}
	if err := workspaceCommand([]string{"acquire", "--repo", source, "--request-id", "disabled"}); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("mutation must retain daemon refusal: %v", err)
	}
	if len(calls) != 2 || <-calls != http.MethodGet || <-calls != http.MethodPost {
		t.Fatal("workspace commands did not reach the daemon")
	}
}
