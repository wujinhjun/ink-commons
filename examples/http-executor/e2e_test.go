//go:build linux || darwin

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wujinhjun/ink-commons/internal/execution"
	"github.com/wujinhjun/ink-commons/internal/execution/httpagent"
	"github.com/wujinhjun/ink-commons/internal/ink"
)

func TestInkPreparesAndDeliversToDemo(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "ink-demo-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if err := ink.InitializeState(dir); err != nil {
		t.Fatal(err)
	}

	// Fake the read-only GitHub CLI protocol so the test needs neither network
	// access nor credentials. Ink still observes, derives and persists real work.
	sha := strings.Repeat("a", 40)
	pull := fmt.Sprintf(`{"number":42,"html_url":"https://github.com/acme/demo/pull/42","state":"open","head":{"sha":%q}}`, sha)
	script := fmt.Sprintf(`#!/bin/sh
printf 'HTTP/1.1 200 OK\r\n\r\n'
case "$9" in
  /repos/acme/demo/pulls/42/reviews*) printf '[]' ;;
  /repos/acme/demo/pulls/42) printf '%%s' '%s' ;;
  /repos/acme/demo/pulls\?*) printf '%%s' '[%s]' ;;
  *) exit 1 ;;
esac
`, pull, pull)
	github := filepath.Join(dir, "github")
	if err := os.WriteFile(github, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	adapter := httptest.NewServer(http.HandlerFunc(execute))
	defer adapter.Close()
	provider, err := httpagent.New(adapter.Client(), adapter.URL, "", []string{"prepare", "deliver"}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	repo := ink.Repository{Repo: "acme/demo", ReviewerLogin: "reviewer",
		ReviewerRoute: &execution.Route{Provider: "local-demo", Target: "reviewer"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	done := make(chan error, 1)
	go func() {
		done <- ink.Serve(ctx, ink.Config{PollSeconds: 1, GitHubCommand: github, Repositories: []ink.Repository{repo}},
			dir, map[string]execution.Executor{"local-demo": provider})
	}()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	socket := filepath.Join(dir, "ink.sock")
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	local := &http.Client{Transport: transport, Timeout: time.Second}

	const workURL = "http://ink/v1/work/acme/demo/42"
	awaitWork := func(ready func(ink.Work) bool) ink.Work {
		t.Helper()
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			var work ink.Work
			res, err := local.Get(workURL)
			if err == nil {
				err = json.NewDecoder(res.Body).Decode(&work)
				res.Body.Close()
				if err == nil && res.StatusCode == http.StatusOK && ready(work) {
					return work
				}
			}
			select {
			case <-ctx.Done():
				t.Fatalf("work did not reach expected state: %+v", work)
			case <-ticker.C:
			}
		}
	}
	awaitWork(func(w ink.Work) bool { return w.Pull.Head.SHA == sha })
	res, err := local.Post(workURL+"/link", "application/json", strings.NewReader(`{"github_login":"developer"}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("link failed with HTTP %d", res.StatusCode)
	}
	work := awaitWork(func(w ink.Work) bool { return w.Task != nil && w.Task.Delivery == "accepted" })
	if work.Phase != "review_pending" || !strings.HasPrefix(work.Reviewer.Route.Session, "demo-") ||
		len(work.Task.Runs) != 1 || work.Task.Runs[0].Request.SHA != sha ||
		work.Task.Runs[0].Route != *work.Reviewer.Route {
		t.Fatalf("unexpected persisted work: %+v", work)
	}
}
