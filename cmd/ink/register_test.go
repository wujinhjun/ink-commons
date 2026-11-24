//go:build linux || darwin

package main

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/wujinhjun/ink-commons/internal/ink"
)

func TestRegisterDetectsGitRemoteAndUsesLocalDaemon(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "ink-reg-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	t.Setenv("INK_DATA_DIR", dir)
	ln, err := net.Listen("unix", filepath.Join(dir, "ink.sock"))
	if err != nil {
		t.Fatal(err)
	}
	received := make(chan ink.Repository, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/repositories" || r.Method != "POST" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var repo ink.Repository
		if err := json.NewDecoder(r.Body).Decode(&repo); err != nil {
			t.Error(err)
		}
		received <- repo
		w.WriteHeader(201)
	})}
	go server.Serve(ln)
	defer server.Close()
	checkout := t.TempDir()
	for _, args := range [][]string{{"init", checkout}, {"-C", checkout, "remote", "add", "origin", "git@github.com:Acme/Project.git"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatal(string(out), err)
		}
	}
	cwd, _ := os.Getwd()
	if err := os.Chdir(checkout); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(cwd)
	if err := register([]string{"--github-login", "reviewer", "--provider", "chat", "--target", "review-project"}); err != nil {
		t.Fatal(err)
	}
	repo := <-received
	if repo.Repo != "acme/project" || repo.ReviewerLogin != "reviewer" || repo.ReviewerRoute.Target != "review-project" {
		t.Fatalf("bad registration: %+v", repo)
	}
}

func TestRepositoryDefaultsAndApprovalConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	for _, test := range []struct {
		body  string
		valid bool
	}{
		{`{"repositories":[],"repository_defaults":{"reviewer_login":"reviewer","postApproval":{"command":["/usr/local/bin/after-approval","--notify"]}}}`, true},
		{`{"repository_defaults":{"postApproval":{"command":["relative"]}}}`, false},
		{`{"repository_defaults":{"postApproval":{"command":["/bin/true"],"timeout_seconds":-1}}}`, false},
		{`{"repository_defaults":{"repo":"acme/project"}}`, false},
		{`{"repository_defaults":{"auto_merge":{"enabled":true,"method":"unsafe"}}}`, false},
	} {
		if err := os.WriteFile(path, []byte(test.body), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, _, err := loadConfig(path)
		if (err == nil) != test.valid {
			t.Fatalf("config %s: %v", test.body, err)
		}
		if test.valid && (cfg.RepositoryDefaults.ReviewerLogin != "reviewer" || cfg.RepositoryDefaults.PostApproval == nil) {
			t.Fatal("lost defaults")
		}
	}
}
