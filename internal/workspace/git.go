package workspace

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Worktree struct {
	Path     string `json:"path"`
	Head     string `json:"head,omitempty"`
	Branch   string `json:"branch,omitempty"`
	Main     bool   `json:"main"`
	Bare     bool   `json:"bare,omitempty"`
	Locked   bool   `json:"locked,omitempty"`
	Prunable bool   `json:"prunable,omitempty"`
	Dirty    bool   `json:"dirty"`
	Ignored  bool   `json:"ignored,omitempty"`
	Error    string `json:"error,omitempty"`
}

type Repository struct {
	Path     string     `json:"path"`
	Common   string     `json:"git_common_dir"`
	Worktree []Worktree `json:"worktrees"`
}

func Git(ctx context.Context, path string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"--no-optional-locks", "-c", "core.hooksPath=/dev/null", "-C", path}, args...)...)
	// The selected clone must win over Git's inherited repository overrides.
	for _, variable := range os.Environ() {
		if !strings.HasPrefix(variable, "GIT_") {
			cmd.Env = append(cmd.Env, variable)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s failed: %w", args[0], err)
	}
	return out, nil
}

func CommonDir(ctx context.Context, path string) (string, error) {
	out, err := Git(ctx, path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(strings.TrimSuffix(string(out), "\n"))
}

func Inspect(ctx context.Context, path string) (Repository, error) {
	common, err := CommonDir(ctx, path)
	if err != nil {
		return Repository{}, err
	}
	repo := Repository{Path: path, Common: common}
	// The URL is deliberately omitted. Clone configuration may contain credentials.
	out, err := Git(ctx, path, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return Repository{}, err
	}
	var entry Worktree
	for _, field := range strings.Split(string(out), "\x00") {
		name, value, _ := strings.Cut(field, " ")
		switch name {
		case "worktree":
			entry = Worktree{Path: value, Main: len(repo.Worktree) == 0}
		case "HEAD":
			entry.Head = value
		case "branch":
			entry.Branch = strings.TrimPrefix(value, "refs/heads/")
		case "bare":
			entry.Bare = true
		case "locked":
			entry.Locked = true
		case "prunable":
			entry.Prunable = true
		case "":
			if entry.Path == "" {
				continue
			}
			if !entry.Bare && !entry.Prunable {
				err := checkWorktreeIdentity(ctx, entry.Path, common)
				if err == nil {
					entry.Dirty, entry.Ignored, err = Status(ctx, entry.Path)
				}
				if err != nil {
					entry.Error = err.Error()
				}
			}
			repo.Worktree = append(repo.Worktree, entry)
			entry = Worktree{}
		}
	}
	return repo, nil
}

// A stale registration may point inside an existing clone or at a different
// repository. Git discovery at that path must match both the root and clone.
func checkWorktreeIdentity(ctx context.Context, path, expectedCommon string) error {
	root, err := Git(ctx, path, "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	if filepath.Clean(strings.TrimSuffix(string(root), "\n")) != filepath.Clean(path) {
		return fmt.Errorf("Git worktree root differs from its registered path")
	}
	common, err := CommonDir(ctx, path)
	if err != nil {
		return err
	}
	if common != expectedCommon {
		return fmt.Errorf("Git common directory differs from the inspected clone")
	}
	return nil
}

func Status(ctx context.Context, path string) (dirty, ignored bool, err error) {
	out, err := Git(ctx, path, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignored=matching", "--ignore-submodules=none")
	if err != nil {
		return false, false, err
	}
	fields := strings.Split(string(out), "\x00")
	for i := 0; i < len(fields); i++ {
		entry := fields[i]
		switch {
		case entry == "":
		case strings.HasPrefix(entry, "!! "):
			ignored = true
		default:
			dirty = true
			if len(entry) >= 2 && (strings.ContainsAny(entry[:2], "RC")) {
				i++ // Porcelain -z emits the old pathname as a second field.
			}
		}
	}
	return dirty, ignored, nil
}

// CheckIdle protects a clean index during a paused Git operation.
func CheckIdle(ctx context.Context, path string) error {
	out, err := Git(ctx, path, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return err
	}
	dir := strings.TrimSuffix(string(out), "\n")
	for _, name := range []string{"MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "REBASE_HEAD", "BISECT_START", "rebase-merge", "rebase-apply", "sequencer", "index.lock"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
			return fmt.Errorf("worktree has a Git operation in progress (%s)", name)
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
