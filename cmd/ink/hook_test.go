package main

import (
	"os"
	"os/exec"
	"path/filepath"
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
