//go:build ccconnect_integration

package ink

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"ink-commons/internal/execution"
	"ink-commons/internal/execution/ccconnect"
)

// This uses the companion repository's REAL durable inbox, engine and session
// locks, mocking only the agent process, messaging platform and GitHub boundary.
func TestCCConnectFeedbackLoop(t *testing.T) {
	binary := os.Getenv("INK_CC_CONNECT_FIXTURE")
	if binary == "" {
		t.Skip("build tests/ink-managed-execution in the companion cc-connect checkout; set INK_CC_CONNECT_FIXTURE")
	}
	var mu sync.Mutex
	var pull Pull
	pull.Number, pull.State, pull.URL = 42, "open", "https://github.com/acme/project/pull/42"
	pull.Head.SHA, pull.Head.Ref = strings.Repeat("a", 40), "feature"
	reviews := []Review{}
	reviewCalls, developerCalls := 0, 0
	prompts := []string{}
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/repos/acme/project/pulls":
			reply(w, 200, []Pull{pull})
		case "/repos/acme/project/pulls/42/reviews":
			reply(w, 200, reviews)
		case "/fixture/finish":
			var result struct{ Role, SHA, Prompt string }
			if err := json.NewDecoder(r.Body).Decode(&result); err != nil {
				http.Error(w, "bad callback", 400)
				return
			}
			prompts = append(prompts, result.Prompt)
			if result.Role == "developer" {
				developerCalls++
				pull.Head.SHA = strings.Repeat("b", 40)
			} else {
				reviewCalls++
				review := Review{ID: int64(reviewCalls), SHA: result.SHA, SubmittedAt: time.Now().UTC(), URL: fmt.Sprintf("%s#review-%d", pull.URL, reviewCalls), State: "CHANGES_REQUESTED"}
				review.User.Login = "reviewer-app[bot]"
				if reviewCalls == 1 {
					review.User.Login = "wrong-personal-login"
				}
				if result.SHA == strings.Repeat("b", 40) {
					review.State = "APPROVED"
				}
				reviews = append(reviews, review)
			}
			reply(w, 200, map[string]bool{"ok": true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer github.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	dir := t.TempDir()
	log, err := os.Create(filepath.Join(dir, "fixture.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	cmd := exec.Command(binary, "--port", strconv.Itoa(port), "--state", filepath.Join(dir, "private", "inbox.json"), "--upstream", github.URL, "--busy", "7s")
	cmd.Stdout, cmd.Stderr = log, log
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}()
	endpoint := fmt.Sprintf("http://127.0.0.1:%d/hook", port)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		res, e := http.Get(endpoint + "/execution")
		if e == nil {
			res.Body.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	provider, err := ccconnect.New(&http.Client{Timeout: 2 * time.Second}, endpoint, "fixture-secret", "")
	if err != nil {
		t.Fatal(err)
	}
	provider.EnableMonitoring(true)
	reviewer := execution.Route{Provider: ccconnect.Name, Target: "reviewer", Session: "discord:202"}
	developer := execution.Route{Provider: ccconnect.Name, Target: "developer", Session: "discord:101"}
	repo := Repository{Repo: "acme/project", ReviewerLogin: "reviewer-app[bot]", ReviewerCommand: "reviewer-cli", ReviewerRoute: &reviewer}
	cfg := Config{Repositories: []Repository{repo}, Reliability: Reliability{ACKSeconds: 1, HeartbeatSeconds: 5, QueueSeconds: 20, ExecutionSeconds: 10, ResultSeconds: 1, BackoffSeconds: 1, MaxAttempts: 3}}
	path := filepath.Join(dir, stateDatabase)
	providers := map[string]execution.Executor{ccconnect.Name: provider}
	s, err := NewService(cfg, path, providers)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	mu.Lock()
	initial := pull
	mu.Unlock()
	work := &Work{Repo: repo.Repo, Pull: initial, Linked: true, Origin: &developer, Developer: Assignee{Route: &developer}, Reviewer: Assignee{GitHubLogin: repo.ReviewerLogin, GitHubCommand: repo.ReviewerCommand, Route: &reviewer}}
	work.derive()
	if err = s.store.save(state{Work: map[string]*Work{key(repo.Repo, 42): work}}); err != nil {
		t.Fatal(err)
	}
	s.up.github, s.up.http = github.URL, github.Client()
	mismatchSeen, queuedSeen, restarted := false, false, false
	deadline = time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		s.Poll(context.Background())
		w := loadReliability(t, s)
		for _, alert := range w.Alerts {
			if alert.Code == "review_identity_mismatch" {
				mismatchSeen = true
			}
		}
		for _, task := range w.executionTasks() {
			for _, a := range task.Runs {
				if a.State == "queued" && task.Role == "developer" {
					queuedSeen = true
				}
			}
		}
		if mismatchSeen && !restarted && w.Task != nil && len(w.Task.Runs) > 0 && w.Task.Runs[0].State == "succeeded" {
			s.Close()
			s, err = NewService(cfg, path, providers)
			if err != nil {
				t.Fatal(err)
			}
			s.up.github, s.up.http = github.URL, github.Client()
			restarted = true
		}
		if w.Phase == "approved" && !w.executionBlocked() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	w := loadReliability(t, s)
	mu.Lock()
	rc, dc := reviewCalls, developerCalls
	allPrompts := strings.Join(prompts, "\n")
	mu.Unlock()
	if w.Phase != "approved" || w.executionBlocked() || !mismatchSeen || !queuedSeen || !restarted || rc != 3 || dc != 1 {
		raw, _ := os.ReadFile(filepath.Join(dir, "fixture.log"))
		t.Fatalf("feedback loop incomplete: phase=%s workflow=%+v blocked=%v mismatch=%v queued=%v restarted=%v reviewCalls=%d developerCalls=%d\n%s", w.Phase, w.Workflow, w.executionBlocked(), mismatchSeen, queuedSeen, restarted, rc, dc, raw)
	}
	if !strings.Contains(allPrompts, "reviewer-app[bot]") || !strings.Contains(allPrompts, "reviewer-cli") || !strings.Contains(allPrompts, "wrong-personal-login") || !strings.Contains(allPrompts, "commit and push") {
		t.Fatal("identity or workflow feedback did not cross the real HTTP boundary", allPrompts)
	}
}
