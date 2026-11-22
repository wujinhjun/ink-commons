package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func testGit(t *testing.T, path string, args ...string) {
	t.Helper()
	if _, err := Git(context.Background(), path, args...); err != nil {
		t.Fatal(err)
	}
}

func testRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	testGit(t, root, "init", "-b", "main")
	testGit(t, root, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "base")
	return root
}

func TestInspectLinkedWorktree(t *testing.T) {
	root := testRepo(t)
	linked := filepath.Join(t.TempDir(), "linked worktree")
	testGit(t, root, "worktree", "add", "--detach", linked, "HEAD")
	if err := os.WriteFile(filepath.Join(linked, "untracked"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	testGit(t, root, "worktree", "lock", "--reason", "active", linked)
	repo, err := Inspect(context.Background(), linked)
	if err != nil {
		t.Fatal(err)
	}
	if len(repo.Worktree) != 2 || !repo.Worktree[0].Main || repo.Worktree[0].Branch != "main" ||
		repo.Worktree[1].Main || !repo.Worktree[1].Locked || !repo.Worktree[1].Dirty || repo.Worktree[1].Branch != "" {
		t.Fatalf("unexpected inventory: %+v", repo)
	}
}
