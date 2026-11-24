//go:build linux || darwin

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wujinhjun/ink-commons/skills"
)

func TestWorkflowInstallCheckAndUpgrade(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "data", "skills")
	dest := filepath.Join(home, ".agents", "skills")
	if err := installWorkflow(root, dest, true); err == nil {
		t.Fatal("missing install passed check")
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("check wrote resources")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("check wrote discovery directory")
	}
	if err := installWorkflow(root, dest, false); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dest, skills.Name)
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatal(err)
	}
	if target != workflowTarget(root) {
		t.Fatalf("unexpected resource target: %s", target)
	}
	if err := installWorkflow(root, dest, true); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Lstat(link)
	if err := installWorkflow(root, dest, false); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Lstat(link)
	if !os.SameFile(before, after) {
		t.Fatal("idempotent init replaced current link")
	}
	// 旧版本及丢失资源的托管链接都可升级，检查不能把它们当作当前版本。
	old := filepath.Join(root, strings.Repeat("a", 64), skills.Name)
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(old, link); err != nil {
		t.Fatal(err)
	}
	if err := installWorkflow(root, dest, true); err == nil {
		t.Fatal("outdated link passed check")
	}
	if err := installWorkflow(root, dest, false); err != nil {
		t.Fatal(err)
	}
	if err := installWorkflow(root, dest, true); err != nil {
		t.Fatal(err)
	}
	// 当前版本资源丢失时仍能通过初始化修复。
	if err := os.RemoveAll(workflowTarget(root)); err != nil {
		t.Fatal(err)
	}
	if err := installWorkflow(root, dest, true); err == nil {
		t.Fatal("broken link passed check")
	}
	if err := installWorkflow(root, dest, false); err != nil {
		t.Fatal(err)
	}
	if err := installWorkflow(root, dest, true); err != nil {
		t.Fatal(err)
	}
}

func TestWorkflowInstallPreservesConflicts(t *testing.T) {
	for _, kind := range []string{"directory", "file", "link", "modified-resource", "resource-symlink", "version-symlink"} {
		t.Run(kind, func(t *testing.T) {
			home := t.TempDir()
			root := filepath.Join(home, "resources")
			dest := filepath.Join(home, "agent")
			if err := os.MkdirAll(dest, 0755); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(dest, skills.Name)
			outside := filepath.Join(home, "user-skill")
			if err := os.MkdirAll(outside, 0755); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(outside, "SKILL.md")
			if err := os.WriteFile(marker, []byte("user instructions"), 0644); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "directory":
				if err := os.Mkdir(link, 0755); err != nil {
					t.Fatal(err)
				}
			case "file":
				if err := os.WriteFile(link, []byte("user content"), 0644); err != nil {
					t.Fatal(err)
				}
			case "link":
				if err := os.Symlink(outside, link); err != nil {
					t.Fatal(err)
				}
			case "modified-resource", "resource-symlink":
				if err := installWorkflow(root, dest, false); err != nil {
					t.Fatal(err)
				}
				file := filepath.Join(workflowTarget(root), "SKILL.md")
				if err := os.Remove(file); err != nil {
					t.Fatal(err)
				}
				if kind == "modified-resource" {
					if err := os.WriteFile(file, []byte("user edits"), 0644); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Symlink(marker, file); err != nil {
					t.Fatal(err)
				}
			case "version-symlink":
				if err := os.MkdirAll(root, 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Dir(workflowTarget(root))); err != nil {
					t.Fatal(err)
				}
			}
			for _, check := range []bool{true, false} {
				if err := installWorkflow(root, dest, check); err == nil {
					t.Fatal("conflict accepted")
				}
			}
			got, _ := os.ReadFile(marker)
			if string(got) != "user instructions" {
				t.Fatal("user content modified")
			}
			if kind == "file" {
				got, _ = os.ReadFile(link)
				if string(got) != "user content" {
					t.Fatal("user file modified")
				}
			}
			if kind == "modified-resource" {
				got, _ = os.ReadFile(filepath.Join(workflowTarget(root), "SKILL.md"))
				if string(got) != "user edits" {
					t.Fatal("edited resource overwritten")
				}
			}
		})
	}
}

func TestInitCLIUsesHomeAndCustomDiscoveryDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	if err := run([]string{"init", "--check"}); err == nil {
		t.Fatal("missing install passed")
	}
	if err := run([]string{"init"}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"init", "--check"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Readlink(filepath.Join(home, ".agents", "skills", skills.Name)); err != nil {
		t.Fatal(err)
	}
	custom := filepath.Join(home, "custom-agent", "skills")
	if err := run([]string{"init", "--skills-dir", custom}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"init", "--check", "--skills-dir", custom}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"init", "unexpected"}); err == nil {
		t.Fatal("positional argument accepted")
	}
	t.Setenv("XDG_DATA_HOME", "relative")
	if err := run([]string{"init"}); err == nil {
		t.Fatal("relative XDG data path accepted")
	}
}
