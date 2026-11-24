//go:build linux || darwin

package ink

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func awaitPollSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for poll")
	}
}

func TestPollBoundsAndIsolatesSlowRepositoryAndPR(t *testing.T) {
	repos := []Repository{{Repo: "test/a", ReviewerLogin: "reviewer"}, {Repo: "test/b", ReviewerLogin: "reviewer"}, {Repo: "test/c", ReviewerLogin: "reviewer"}}
	s, err := newFixtureService(t, Config{Repositories: repos}, filepath.Join(t.TempDir(), stateDatabase), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	slowList, slowPR := make(chan struct{}), make(chan struct{})
	fast := make(chan struct{})
	var entered, active, peak atomic.Int32
	var once sync.Once
	s.up.http.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		parts := strings.Split(r.URL.Path, "/")
		repo := parts[3]
		if len(parts) == 5 {
			if repo == "a" {
				close(slowList)
				<-r.Context().Done()
				return nil, r.Context().Err()
			}
			count := 2
			if repo == "c" {
				select {
				case <-fast:
				case <-r.Context().Done():
					return nil, r.Context().Err()
				}
				count = 24
			}
			pulls := make([]Pull, count)
			for i := range pulls {
				pulls[i].Number = i + 1
			}
			reply(w, 200, pulls)
			return w.Result(), nil
		}
		n := int(active.Add(1))
		defer active.Add(-1)
		for old := peak.Load(); int32(n) > old && !peak.CompareAndSwap(old, int32(n)); old = peak.Load() {
		}
		if repo == "b" && parts[5] == "1" {
			close(slowPR)
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		if repo == "b" {
			once.Do(func() { close(fast) })
			p := Pull{Number: 2, State: "closed"}
			reply(w, 200, p)
			return w.Result(), nil
		}
		entered.Add(1)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Poll(ctx)
	}()
	awaitPollSignal(t, slowList)
	awaitPollSignal(t, fast)
	awaitPollSignal(t, slowPR)
	deadline := time.Now().Add(5 * time.Second)
	for peak.Load() < prPollWorkers && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if peak.Load() != prPollWorkers || entered.Load() == 0 {
		t.Errorf("peak PR reads %d, other-repo reads %d", peak.Load(), entered.Load())
	}
	cancel()
	awaitPollSignal(t, done)
}

func TestPollSlowHookRunsAfterMergeAndLeavesOtherPRsFree(t *testing.T) {
	f := newAutoMergeFixture(t)
	dir := t.TempDir()
	script, marker := filepath.Join(dir, "hook"), filepath.Join(dir, "ran")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n: > \"$1\"\nexec sleep 60\n"), 0700); err != nil {
		t.Fatal(err)
	}
	hook := ApprovalHook{Command: []string{script, marker}, TimeoutSeconds: 2}
	if _, err := f.s.changeConfig(f.repo.Repo, "postApproval", jsonValue(hook), false); err != nil {
		t.Fatal(err)
	}
	original := f.s.up.http.Transport
	var sibling atomic.Bool
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	f.s.up.http.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		switch {
		case strings.HasSuffix(r.URL.Path, "/pulls"):
			pulls := []Pull{f.pull}
			if sibling.Load() {
				pulls = append(pulls, Pull{Number: 43})
			}
			reply(w, 200, pulls)
		case strings.HasSuffix(r.URL.Path, "/pulls/43"):
			reply(w, 200, Pull{Number: 43, State: "closed"})
		default:
			return original.RoundTrip(r)
		}
		return w.Result(), nil
	})
	f.s.Poll(ctx)
	if f.writes != 1 {
		t.Fatalf("merge waited for hook: writes=%d", f.writes)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("approval hook did not start")
		}
		time.Sleep(time.Millisecond)
	}
	first := notificationWork(t, f.s, f.repo, f.pull)
	if len(first.Approvals) != 1 || first.Approvals[0].State != "unknown" {
		t.Fatalf("hook reservation: %+v", first.Approvals)
	}
	sibling.Store(true)
	f.s.Poll(ctx)
	next := notificationWork(t, f.s, f.repo, f.pull)
	other, err := f.s.store.loadWork(key(f.repo.Repo, 43))
	if err != nil || other == nil || next.Version != first.Version {
		t.Fatalf("other PR=%v err=%v version=%d want=%d", other != nil, err, next.Version, first.Version)
	}
	f.s.hooks.wg.Wait()
	if run := notificationWork(t, f.s, f.repo, f.pull).Approvals[0]; run.State != "failed" || !strings.Contains(run.Error, "timed out") {
		t.Fatalf("hook timeout not recorded: %+v", run)
	}
}

func TestObservationDiscardsHeadChangeDuringReviewRead(t *testing.T) {
	f := newAutoMergeFixture(t)
	f.changedOnRevalidation = true
	before := notificationWork(t, f.s, f.repo, f.pull)
	f.s.Poll(context.Background())
	after := notificationWork(t, f.s, f.repo, f.pull)
	if f.writes != 0 || before.Version != after.Version || before.storedRevision != after.storedRevision {
		t.Fatal("mixed GitHub observation reached work state or merge")
	}
}

func TestWorkflowClockAndRedactedStructuredLogs(t *testing.T) {
	repo := Repository{Repo: "test/private", ReviewerLogin: "reviewer"}
	clock := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	s, err := newFixtureService(t, Config{Repositories: []Repository{repo}, Now: func() time.Time { return clock }}, filepath.Join(t.TempDir(), stateDatabase), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var out bytes.Buffer
	s.log, err = newLogger(&out, "warn", "json")
	if err != nil {
		t.Fatal(err)
	}
	const secret = "Bearer token /home/private/config private-channel"
	s.log.Info("disabled")
	s.up.http.Transport = testTransport(func(*http.Request) (*http.Response, error) { return nil, errors.New(secret) })
	s.Poll(context.Background())
	if s.lastPoll.Load() != clock.Unix() {
		t.Fatal("poll did not use injected clock")
	}
	w := &Work{Repo: repo.Repo, Linked: true, Pull: Pull{Number: 1, State: "open"}}
	w.derive(s.now())
	if !w.Task.CreatedAt.Equal(clock) {
		t.Fatal("task did not use injected clock")
	}
	w.AutoMerges = []AutoMergeRun{{State: "unknown", Reason: secret}}
	s.refreshWorkflow(w, s.now())
	if len(w.Alerts) != 1 || !w.Alerts[0].CreatedAt.Equal(clock) {
		t.Fatal("workflow alert did not use injected clock")
	}
	if !strings.Contains(w.Alerts[0].Message, secret) {
		t.Fatal("fixture did not exercise sensitive alert content")
	}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil || event["level"] != "WARN" {
			t.Fatalf("invalid structured warning: %s", line)
		}
	}
	if !strings.Contains(out.String(), "auto_merge_unknown") {
		t.Fatal("workflow alert was not logged")
	}
	for _, forbidden := range []string{"disabled", "Bearer", "/home/private", "private-channel", repo.Repo} {
		if strings.Contains(out.String(), forbidden) {
			t.Errorf("log leaked %q: %s", forbidden, out.String())
		}
	}
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			var dest bytes.Buffer
			log, err := newLogger(&dest, "debug", format)
			if err != nil {
				t.Fatal(err)
			}
			log.Debug("diagnostic", "number", 42)
			if text := dest.String(); !strings.Contains(text, "DEBUG") || !strings.Contains(text, "diagnostic") {
				t.Fatalf("debug message was not emitted: %s", text)
			}
		})
	}
}

func TestServiceRestartDoesNotWriteWorkOrRuntimeConfig(t *testing.T) {
	repo := Repository{Repo: "test/restart", ReviewerLogin: "reviewer"}
	cfg := Config{Repositories: []Repository{repo}}
	path := filepath.Join(t.TempDir(), stateDatabase)
	s, err := newFixtureService(t, cfg, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	work := &Work{Repo: repo.Repo, Pull: Pull{Number: 1, State: "closed"}}
	if err := s.store.saveWork(work); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"work_items", "runtime_config"} {
		for _, op := range []string{"INSERT", "UPDATE", "DELETE"} {
			_, err := s.store.db.Exec("CREATE TRIGGER stop_" + table + "_" + op +
				" BEFORE " + op + " ON " + table + " BEGIN SELECT RAISE(ABORT, 'startup write'); END")
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewService(cfg, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := reopened.store.loadWork(key(repo.Repo, 1))
	if err != nil || got == nil || got.storedRevision != work.storedRevision {
		t.Fatalf("restart changed existing work: %+v, %v", got, err)
	}
}

func TestPendingApprovalSurvivesRestartAndMerge(t *testing.T) {
	f := newAutoMergeFixture(t)
	script := filepath.Join(t.TempDir(), "hook")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncat >/dev/null\n"), 0700); err != nil {
		t.Fatal(err)
	}
	hook := ApprovalHook{Command: []string{script}}
	if _, err := f.s.changeConfig(f.repo.Repo, "postApproval", jsonValue(hook), false); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if item, err := f.s.approvalCandidate(ctx, f.repo, 42); err != nil || item == nil {
		t.Fatalf("stage approval: %v, %v", item, err)
	}
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewService(Config{Repositories: []Repository{f.repo}}, f.path, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.s = reopened
	defer reopened.Close()
	f.pull.State, f.pull.Merged = "closed", true
	f.installTransport(t)
	original := reopened.up.http.Transport
	reopened.up.http.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/pulls") {
			w := httptest.NewRecorder()
			reply(w, 200, []Pull{})
			return w.Result(), nil
		}
		return original.RoundTrip(r)
	})
	reopened.Poll(ctx)
	reopened.hooks.wg.Wait()
	work := notificationWork(t, reopened, f.repo, f.pull)
	if len(work.Approvals) != 1 || work.Approvals[0].State != "succeeded" || f.writes != 0 {
		t.Fatalf("pending approval not reconciled on the merged PR: %+v", work.Approvals)
	}
}

func TestObservationDoesNotOverwriteConcurrentAssignment(t *testing.T) {
	f := newAutoMergeFixture(t)
	before := notificationWork(t, f.s, f.repo, f.pull)
	original := f.s.up.http.Transport
	var changed sync.Once
	f.s.up.http.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/reviews") {
			changed.Do(func() {
				req := httptest.NewRequest(http.MethodPost, "/v1/work/acme/project/42/assign", strings.NewReader(`{"role":"reviewer","github_login":"replacement"}`))
				out := httptest.NewRecorder()
				f.s.Handler().ServeHTTP(out, req)
				if out.Code != http.StatusOK {
					t.Errorf("concurrent assignment: HTTP %d", out.Code)
				}
			})
		}
		return original.RoundTrip(r)
	})
	f.s.Poll(context.Background())
	after := notificationWork(t, f.s, f.repo, f.pull)
	if f.writes != 0 || after.Reviewer.GitHubLogin != "replacement" || after.Version != before.Version+1 || after.storedRevision != before.storedRevision+1 {
		t.Fatal("observation raced the concurrent assignment")
	}
}
