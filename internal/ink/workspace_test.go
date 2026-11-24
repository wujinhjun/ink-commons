//go:build linux || darwin

package ink

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wujinhjun/ink-commons/internal/workspace"
)

func workspaceTestGit(t *testing.T, repo string, args ...string) {
	t.Helper()
	if _, err := workspace.Git(context.Background(), repo, args...); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceFullLiteLifecycle(t *testing.T) {
	ctx := context.Background()
	source, _ := workspaceTestSource(t)
	db := filepath.Join(t.TempDir(), "state", stateDatabase)
	if err := InitializeState(filepath.Dir(db)); err != nil {
		t.Fatal(err)
	}
	open := func() *Service {
		t.Helper()
		s, err := NewService(Config{WorkspaceFullSlots: 1}, db, nil)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	s := open()
	defer func() { s.Close() }()
	acquire := func(id, kind, branch string) WorkspaceAllocation {
		t.Helper()
		a, err := s.acquireWorkspace(ctx, workspaceRequest{Source: source, RequestID: id, Preference: kind, Branch: branch})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	finish := func(a WorkspaceAllocation, action string) WorkspaceAllocation {
		t.Helper()
		result, err := s.finishWorkspace(ctx, a.ID, action)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	full := acquire("first", "auto", "feature/full")
	lite := acquire("second", "auto", "feature/lite")
	if full.Kind != "full" || lite.Kind != "lite" || full.Path == lite.Path || full.State != "active" || lite.State != "active" {
		t.Fatalf("wrong allocation: full=%+v lite=%+v", full, lite)
	}
	if _, err := s.acquireWorkspace(ctx, workspaceRequest{Source: source, RequestID: "third", Preference: "full"}); err == nil {
		t.Fatal("required Full allocated when all slots were used")
	}
	s.Close()
	s = open()
	if retry := acquire("first", "auto", "feature/full"); retry.ID != full.ID || retry.Path != full.Path {
		t.Fatalf("retry changed allocation: %+v", retry)
	}
	cache := filepath.Join(full.Path, "cache")
	if err := os.Mkdir(cache, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "keep"), []byte("cached"), 0600); err != nil {
		t.Fatal(err)
	}
	dirty := filepath.Join(full.Path, "untracked")
	if err := os.WriteFile(dirty, nil, 0600); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s = open()
	if _, err := s.finishWorkspace(ctx, full.ID, "release"); err == nil {
		t.Fatal("dirty Full worktree released")
	}
	if retry := acquire("first", "auto", "feature/full"); retry.State != "active" || retry.Path != full.Path {
		t.Fatalf("dirty workspace lost its active reservation: %+v", retry)
	}
	if _, err := s.acquireWorkspace(ctx, workspaceRequest{Source: source, RequestID: "while-dirty", Preference: "full"}); err == nil {
		t.Fatal("dirty Full slot was made available to another request")
	}
	if err := os.Remove(dirty); err != nil {
		t.Fatal(err)
	}
	workspaceTestGit(t, full.Path, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "unpushed")
	if _, err := s.finishWorkspace(ctx, full.ID, "release"); err == nil {
		t.Fatal("unpublished Full worktree released")
	}
	workspaceTestGit(t, full.Path, "push", "origin", "feature/full")
	if released := finish(full, "release"); released.State != "released" || released.Head == full.Head {
		t.Fatalf("Full release failed to record published HEAD: %+v", released)
	}
	reused := acquire("third", "full", "feature/next")
	if reused.Path != full.Path || reused.ID == full.ID {
		t.Fatalf("Full slot not reused: %+v", reused)
	}
	if data, err := os.ReadFile(filepath.Join(reused.Path, "cache", "keep")); err != nil || string(data) != "cached" {
		t.Fatalf("Full cache was lost: %q %v", data, err)
	}
	if retry := acquire("first", "auto", "feature/full"); retry.State != "released" {
		t.Fatalf("old request reclaimed a reused Full slot: %+v", retry)
	}
	finish(lite, "release")
	if err := os.Mkdir(filepath.Join(lite.Path, "cache"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lite.Path, "cache", "user-file"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.finishWorkspace(ctx, lite.ID, "remove"); err == nil {
		t.Fatal("Lite with ignored files was removed")
	}
	if err := os.Remove(filepath.Join(lite.Path, "cache", "user-file")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(lite.Path, "cache")); err != nil {
		t.Fatal(err)
	}
	if removed := finish(lite, "remove"); removed.State != "removed" {
		t.Fatalf("Lite removal not recorded: %+v", removed)
	}
	if _, err := os.Stat(lite.Path); !os.IsNotExist(err) {
		t.Fatalf("removed Lite path still exists: %v", err)
	}
	if _, err := workspace.Git(ctx, source, "show-ref", "--verify", "refs/heads/feature/lite"); err != nil {
		t.Fatal(fmt.Errorf("worktree removal should preserve the branch: %w", err))
	}
}

func workspaceTestHead(t *testing.T, repo string) string {
	t.Helper()
	out, err := workspace.Git(context.Background(), repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func workspaceTestSource(t *testing.T) (string, string) {
	t.Helper()
	source, origin := t.TempDir(), t.TempDir()
	workspaceTestGit(t, source, "init", "-b", "main")
	workspaceTestGit(t, origin, "init", "--bare", "-b", "main")
	if err := os.WriteFile(filepath.Join(source, ".gitignore"), []byte("cache/\n"), 0600); err != nil {
		t.Fatal(err)
	}
	workspaceTestGit(t, source, "add", ".gitignore")
	workspaceTestGit(t, source, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "base")
	workspaceTestGit(t, source, "remote", "add", "origin", origin)
	workspaceTestGit(t, source, "push", "origin", "main")
	return source, workspaceTestHead(t, source)
}

func TestWorkspaceCheckoutFailureReleasesUntouchedFull(t *testing.T) {
	for _, slots := range []int{1, 2} {
		t.Run(fmt.Sprintf("%d slots", slots), func(t *testing.T) {
			ctx := context.Background()
			source, base := workspaceTestSource(t)
			s, err := newFixtureService(t, Config{WorkspaceFullSlots: slots}, filepath.Join(t.TempDir(), stateDatabase), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()

			first, err := s.acquireWorkspace(ctx, workspaceRequest{Source: source, RequestID: "first", Ref: base})
			if err != nil {
				t.Fatal(err)
			}
			cache := filepath.Join(first.Path, "cache", "build")
			if err := os.Mkdir(filepath.Dir(cache), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(cache, []byte("cache"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := s.finishWorkspace(ctx, first.ID, "release"); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(source, "cache"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(source, "cache", "build"), []byte("tracked"), 0600); err != nil {
				t.Fatal(err)
			}
			workspaceTestGit(t, source, "add", "-f", "cache/build")
			workspaceTestGit(t, source, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "track former cache")
			ref := workspaceTestHead(t, source)

			forced := workspaceRequest{Source: source, RequestID: "forced", Ref: ref, Preference: "full"}
			if _, err := s.acquireWorkspace(ctx, forced); err == nil {
				t.Fatal("forced Full silently accepted the checkout conflict")
			}
			var count int
			if err := s.store.db.QueryRow("SELECT count(*) FROM workspace_allocations WHERE request_id = ?", forced.RequestID).Scan(&count); err != nil || count != 0 {
				t.Fatalf("failed Full request left a reservation: count=%d err=%v", count, err)
			}

			auto := workspaceRequest{Source: source, RequestID: "auto", Ref: ref}
			next, err := s.acquireWorkspace(ctx, auto)
			want := "lite"
			if slots > 1 {
				want = "full"
			}
			if err != nil || next.Kind != want || next.State != "active" || next.Head != ref || next.Path == first.Path {
				t.Fatalf("fallback did not use an alternate workspace: %+v, err=%v", next, err)
			}
			again, err := s.acquireWorkspace(ctx, auto)
			if err != nil || again.ID != next.ID {
				t.Fatalf("retry changed fallback allocation: %+v, err=%v", again, err)
			}
			reused, err := s.acquireWorkspace(ctx, workspaceRequest{Source: source, RequestID: "reuse", Ref: base, Preference: "full"})
			if err != nil || reused.Path != first.Path {
				t.Fatalf("original slot was not available: %+v, err=%v", reused, err)
			}
			if data, err := os.ReadFile(cache); err != nil || string(data) != "cache" {
				t.Fatalf("cache changed: %q, err=%v", data, err)
			}
		})
	}
}

func TestWorkspaceCheckoutFailureRetainsChangedFull(t *testing.T) {
	ctx := context.Background()
	source, base := workspaceTestSource(t)
	s, err := newFixtureService(t, Config{WorkspaceFullSlots: 1}, filepath.Join(t.TempDir(), stateDatabase), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	first, err := s.acquireWorkspace(ctx, workspaceRequest{Source: source, RequestID: "first", Ref: base})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.finishWorkspace(ctx, first.ID, "release"); err != nil {
		t.Fatal(err)
	}
	workspaceTestGit(t, source, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "next")
	next := workspaceTestHead(t, source)
	common, err := workspace.CommonDir(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	req := workspaceRequest{Source: source, RequestID: "retry", Ref: next, Branch: "feature", Preference: "auto"}
	reserved, err := s.reserveWorkspace(ctx, req, common, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	workspaceTestGit(t, source, "branch", "feature", base)
	got, err := s.acquireWorkspace(ctx, req)
	if err == nil || !strings.Contains(err.Error(), "ink workspace release --id "+reserved.ID) ||
		got.ID != reserved.ID || got.State != "preparing" || workspaceTestHead(t, got.Path) != next {
		t.Fatalf("changed Full should retain recovery state: %+v, err=%v", got, err)
	}
	workspaceTestGit(t, source, "branch", "-f", "feature", next)
	retry, err := s.acquireWorkspace(ctx, req)
	if err != nil || retry.ID != reserved.ID || retry.State != "active" {
		t.Fatalf("original request could not resume: %+v, err=%v", retry, err)
	}
}

func TestWorkspacePreparingRetryRequiresCurrentClaim(t *testing.T) {
	ctx := context.Background()
	f := newHandoffFixture(t)
	source, head := workspaceTestSource(t)
	workspaceTestGit(t, source, "remote", "set-url", "origin", "https://github.com/"+f.repo.Repo+".git")
	st, err := loadFixture(f.s.store)
	if err != nil {
		t.Fatal(err)
	}
	w := st.Work[key(f.repo.Repo, f.pull.Number)]
	w.Pull.Head.SHA = head
	w.Task.SHA = head
	if err := saveFixture(f.s.store, st); err != nil {
		t.Fatal(err)
	}
	task := f.work(t).Task
	f.claim(t, task, claimA, 200)
	req := workspaceRequest{Source: source, RequestID: "task-retry", Ref: head, Preference: "lite",
		TaskID: task.ID, Owner: task.Handoff.Assignee.Route, ClaimToken: claimA}
	common, err := workspace.CommonDir(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	reserved, err := f.s.reserveWorkspace(ctx, req, common, "", nil)
	if err != nil {
		t.Fatal(err)
	}

	assertNoCheckout := func(request workspaceRequest) {
		t.Helper()
		if _, err := f.s.acquireWorkspace(ctx, request); err == nil {
			t.Fatal("invalid task claim activated reservation")
		}
		if _, err := os.Stat(reserved.Path); !os.IsNotExist(err) {
			t.Fatalf("invalid claim wrote a worktree: %v", err)
		}
		var state string
		if err := f.s.store.db.QueryRow("SELECT state FROM workspace_allocations WHERE id = ?", reserved.ID).Scan(&state); err != nil || state != "preparing" {
			t.Fatalf("invalid claim changed reservation: %q, %v", state, err)
		}
	}
	badToken := req
	badToken.ClaimToken = strings.Repeat("z", 64)
	assertNoCheckout(badToken)

	st, err = loadFixture(f.s.store)
	if err != nil {
		t.Fatal(err)
	}
	w = st.Work[key(f.repo.Repo, f.pull.Number)]
	w.Task.SHA = strings.Repeat("b", 40)
	if err := saveFixture(f.s.store, st); err != nil {
		t.Fatal(err)
	}
	assertNoCheckout(req)
	w.Task.SHA = head
	if err := saveFixture(f.s.store, st); err != nil {
		t.Fatal(err)
	}
	f.submit(t, task, claimA, HandoffSubmission{RequestID: "blocked", Handoff: true, Outcome: "blocked", Reason: "fixture handoff"}, 200)
	assertNoCheckout(req)
}
