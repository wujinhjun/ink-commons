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

func TestInspectRejectsReusedWorktreePath(t *testing.T) {
	for _, test := range []struct {
		name             string
		independentClone bool
	}{
		{name: "another clone", independentClone: true},
		{name: "directory inside the same clone"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := testRepo(t)
			linked := filepath.Join(root, "linked")
			testGit(t, root, "worktree", "add", "--detach", linked, "HEAD")
			testGit(t, root, "worktree", "lock", linked)
			if err := os.Rename(linked, filepath.Join(t.TempDir(), "moved")); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(linked, 0700); err != nil {
				t.Fatal(err)
			}
			if test.independentClone {
				testGit(t, linked, "init", "-b", "replacement")
			}
			if err := os.WriteFile(filepath.Join(linked, "untracked"), nil, 0600); err != nil {
				t.Fatal(err)
			}

			repo, err := Inspect(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			if len(repo.Worktree) != 2 {
				t.Fatalf("expected one linked registration: %+v", repo.Worktree)
			}
			main, entry := repo.Worktree[0], repo.Worktree[1]
			if !entry.Locked || entry.Prunable || entry.Head != main.Head || entry.Error == "" || entry.Dirty || entry.Ignored {
				t.Fatalf("expected registered HEAD and unknown file status: %+v", entry)
			}
		})
	}
}
