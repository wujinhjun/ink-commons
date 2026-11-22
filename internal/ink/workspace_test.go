package ink

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"ink-commons/internal/workspace"
)

func workspaceTestGit(t *testing.T, repo string, args ...string) {
	t.Helper()
	if _, err := workspace.Git(context.Background(), repo, args...); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceFullLiteLifecycle(t *testing.T) {
	ctx := context.Background()
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
	db := filepath.Join(t.TempDir(), stateDatabase)
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
	if _, err := s.finishWorkspace(ctx, full.ID, "release"); err == nil {
		t.Fatal("dirty Full worktree released")
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
