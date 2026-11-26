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
	if err := run(append([]string{"hook"}, flags...)); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "custom-hooks", "pre-push")
	if status, err := hookStatus(path); status != "installed" || err != nil {
		t.Fatalf("hook status %q, %v", status, err)
	}
	if err := run(append([]string{"hook"}, flags...)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("custom"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := run(append([]string{"hook"}, flags...)); err == nil {
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
	if err := run([]string{"hook", "install", "--checkout", dir}); err != nil {
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
		err = run([]string{"hook", "status", "--checkout", dir, "--repo", repo})
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

func TestInstalledHookInvokesInkFromPathAndEnv(t *testing.T) {
	dir := t.TempDir()
	if output, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %s %v", output, err)
	}
	if err := run([]string{"hook", "install", "--checkout", dir}); err != nil {
		t.Fatal(err)
	}
	hookPath := filepath.Join(dir, ".git", "hooks", "pre-push")

	fakeBinDir := t.TempDir()
	logFile := filepath.Join(t.TempDir(), "invocations.log")
	fakeInk := filepath.Join(fakeBinDir, "ink")
	script := "#!/bin/sh\nprintf '%s|%s\\n' \"$*\" \"$(cat)\" >> " + logFile + "\n"
	if err := os.WriteFile(fakeInk, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}

	isolatedHome := t.TempDir()
	runHook := func(env []string, stdin string) {
		t.Helper()
		cmd := exec.Command(hookPath, "origin", "https://github.com/acme/project.git")
		cmd.Env = env
		cmd.Stdin = strings.NewReader(stdin)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("hook execution failed: %s %v", output, err)
		}
	}

	// Release-style install: ink available on PATH with an isolated HOME.
	runHook([]string{
		"HOME=" + isolatedHome,
		"PATH=" + fakeBinDir + ":/usr/bin:/bin",
	}, "refs/heads/main aaaa refs/heads/main bbbb")

	// Source-build install: ink not on PATH, resolved via INK_BIN.
	sourceBin := filepath.Join(t.TempDir(), "custom-ink")
	if err := os.WriteFile(sourceBin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	runHook([]string{
		"HOME=" + isolatedHome,
		"PATH=/usr/bin:/bin",
		"INK_BIN=" + sourceBin,
	}, "refs/heads/feat cccc refs/heads/feat dddd")

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 hook invocations, got %d: %q", len(lines), string(data))
	}
	if lines[0] != "pre-push https://github.com/acme/project.git|refs/heads/main aaaa refs/heads/main bbbb" {
		t.Fatalf("unexpected PATH invocation: %q", lines[0])
	}
	if lines[1] != "pre-push https://github.com/acme/project.git|refs/heads/feat cccc refs/heads/feat dddd" {
		t.Fatalf("unexpected INK_BIN invocation: %q", lines[1])
	}

	// Legacy hook installations remain recognized as installed.
	if err := os.WriteFile(hookPath, []byte(legacyPrePushHook), 0700); err != nil {
		t.Fatal(err)
	}
	if status, err := hookStatus(hookPath); status != "installed" || err != nil {
		t.Fatalf("legacy hook status = %q, %v; want installed", status, err)
	}
}
