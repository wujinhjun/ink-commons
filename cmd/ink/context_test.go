package main

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wujinhjun/ink-commons/internal/ink"
)

func TestContextCLIUsesPrivateDaemonAndBoundSHA(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "ink-ctx-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	t.Setenv("INK_DATA_DIR", dir)
	ln, err := net.Listen("unix", filepath.Join(dir, "ink.sock"))
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("a", 40)
	calls := 0
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "GET" || r.URL.Path != "/v1/work/acme/project/42/context" || r.URL.Query().Get("sha") != sha || r.URL.Query().Get("before") != "1000" {
			t.Errorf("wrong request: %s", r.URL)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"session":"discord:123","before":"1000","messages":[],"truncated":false}`))
	})}
	go server.Serve(ln)
	defer server.Close()
	if err := readContext([]string{"--sha", sha, "--before", "1000", "https://github.com/acme/project/pull/42"}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("CLI did not read daemon")
	}
	if err := readContext([]string{"https://github.com/acme/project/pull/42"}); err == nil {
		t.Fatal("context requires task SHA")
	}
}

func TestReviewFeatureConfiguration(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	for _, tc := range []struct {
		body  string
		valid bool
	}{
		{`{"repository_defaults":{"review":{"reuse_development_thread":true,"context":{"thread_history":true}}}}`, true},
		{`{"repository_defaults":{"review":{"context":{"thread_history":true}},"reviewer_route":{"provider":"http","target":"reviewer"}}}`, false},
		{`{"repository_defaults":{"review":{"context":{"unknown":true}}}}`, false},
	} {
		if err := os.WriteFile(p, []byte(tc.body), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, _, err := loadConfig(p)
		if (err == nil) != tc.valid {
			t.Fatalf("config: %s: %v", tc.body, err)
		}
		if tc.valid && (cfg.RepositoryDefaults.Review == nil || !cfg.RepositoryDefaults.Review.ReuseDevelopmentThread || !cfg.RepositoryDefaults.Review.Context.ThreadHistory) {
			t.Fatal("lost feature configuration")
		}
	}
}

func TestRegisterExplicitFeatureFlags(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "ink-feat-")
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
		var v ink.Repository
		if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
			t.Error(err)
		}
		received <- v
		w.WriteHeader(201)
	})}
	go server.Serve(ln)
	defer server.Close()
	if err := register([]string{"--require-same-channel", "--reuse-development-thread", "--thread-history", "--auto-merge-after-approval", "--merge-method", "squash", "--reuse-reader-identity", "acme/project"}); err != nil {
		t.Fatal(err)
	}
	v := <-received
	if v.Review == nil || !v.Review.RequireSameChannel || !v.Review.ReuseDevelopmentThread || !v.Review.Context.ThreadHistory {
		t.Fatal("registration omitted features")
	}
	if v.AutoMerge == nil || !v.AutoMerge.Enabled || v.AutoMerge.Method != "squash" || !v.AutoMerge.ReuseReaderIdentity {
		t.Fatal("registration omitted auto merge policy")
	}
}
