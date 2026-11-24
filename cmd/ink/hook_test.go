//go:build linux || darwin

package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallHookUsesConfiguredPathAndPreservesExistingHook(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"config", "core.hooksPath", "custom-hooks"}} {
		if output, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git: %s %v", output, err)
		}
	}
	flags := []string{"install", "--checkout", dir}
	if err := hookCommand(flags); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "custom-hooks", "pre-push")
	if status, err := hookStatus(path); status != "installed" || err != nil {
		t.Fatalf("hook status %q, %v", status, err)
	}
	if err := hookCommand(flags); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("custom"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := hookCommand(flags); err == nil {
		t.Fatal("unmanaged hook was accepted")
	}
	if content, err := os.ReadFile(path); err != nil || string(content) != "custom" {
		t.Fatalf("existing hook changed: %q, %v", content, err)
	}
}

func TestHookStatusMatchesActualGitHubPushURLs(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		if output, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git: %s %v", output, err)
		}
	}
	git("init", "-q")
	git("remote", "add", "origin", "ssh://git@github.com/Acme/Upstream.git")
	if err := hookCommand([]string{"install", "--checkout", dir}); err != nil {
		t.Fatal(err)
	}
	status := func(repo, want string) {
		t.Helper()
		reader, writer, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer reader.Close()
		old := os.Stdout
		os.Stdout = writer
		err = hookCommand([]string{"status", "--checkout", dir, "--repo", repo})
		os.Stdout = old
		writer.Close()
		if err != nil {
			t.Fatal(err)
		}
		output, err := io.ReadAll(reader)
		if err != nil || strings.TrimSpace(string(output)) != want {
			t.Fatalf("status %s = %q, %v; want %q", repo, output, err, want)
		}
	}

	status("acme/upstream", "installed")
	git("config", "--add", "remote.origin.pushurl", "git@github.com:fork-owner/fork.git")
	status("acme/upstream", "unknown")
	status("fork-owner/fork", "installed")
	git("config", "--add", "remote.origin.pushurl", "https://github.com/acme/upstream.git")
	status("acme/upstream", "installed")
}
