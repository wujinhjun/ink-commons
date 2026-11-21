package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestTaskCLIUsesDurablePrivateCredentialAndIdempotentSubmission(t *testing.T) {
	dir, err := os.MkdirTemp("/private/tmp", "ink-task-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	t.Setenv("INK_DATA_DIR", dir)
	t.Setenv("INK_PROVIDER", "")
	t.Setenv("CC_PROJECT", "developer")
	t.Setenv("CC_SESSION_KEY", "discord:123")
	ln, err := net.Listen("unix", filepath.Join(dir, "ink.sock"))
	if err != nil {
		t.Fatal(err)
	}
	requests := make(chan map[string]any, 8)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		input := map[string]any{}
		if r.Method == "POST" {
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
			}
		}
		input["path"] = r.URL.Path
		requests <- input
		io.WriteString(w, `{"first":true,"task":{"handoff":{"state":"claimed"}}}`)
	})}
	go server.Serve(ln)
	defer server.Close()
	id := uuid.NewString()
	if err := taskCommand([]string{"claim", "--task", id}); err != nil {
		t.Fatal(err)
	}
	first := <-requests
	if err := taskCommand([]string{"claim", "--task", id}); err != nil {
		t.Fatal(err)
	}
	repeated := <-requests
	if first["token"] != repeated["token"] || len(first["token"].(string)) != 64 {
		t.Fatal("claim identity changed on retry")
	}
	file := filepath.Join(dir, "claims", id+".json")
	info, err := os.Stat(file)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("credential was not private", err)
	}
	args := []string{"submit", "--task", id, "--handoff", "--request-id", "fixed", "--new-sha", strings.Repeat("b", 40)}
	if err := taskCommand(args); err != nil {
		t.Fatal(err)
	}
	submitted := <-requests
	if err := taskCommand(args); err != nil {
		t.Fatal(err)
	}
	replayed := <-requests
	a, _ := json.Marshal(submitted)
	b, _ := json.Marshal(replayed)
	if string(a) != string(b) {
		t.Fatal("submission retry changed identity/evidence")
	}
	if err := taskCommand([]string{"status", "--task", id}); err != nil {
		t.Fatal(err)
	}
	if status := <-requests; status["token"] != nil {
		t.Fatal("status leaked credential")
	}
	t.Setenv("CC_PROJECT", "other")
	if err := taskCommand([]string{"claim", "--task", id}); err == nil {
		t.Fatal("credential reused from another route")
	}
}

func TestTaskCredentialRejectsSymlinksAndPublicFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claim.json")
	if err := os.WriteFile(path, []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readTaskCredential(path); err == nil {
		t.Fatal("public credential accepted")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	link := path + ".link"
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readTaskCredential(link); err == nil {
		t.Fatal("credential symlink accepted")
	}
	if err := writeTaskCredential(path, []byte("replace")); err == nil {
		t.Fatal("existing credential overwritten")
	}
}

func TestTaskModeConfigurationIsValidated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	for _, mode := range []string{"cooperative", "legacy", "invalid"} {
		body := `{"repositories":[{"repo":"acme/project","reviewer_login":"reviewer","task_mode":"` + mode + `"}]}`
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, _, err := loadConfig(path)
		if mode == "invalid" {
			if err == nil {
				t.Fatal("invalid task mode accepted")
			}
		} else if err != nil || cfg.Repositories[0].TaskMode != mode {
			t.Fatal(cfg, err)
		}
	}
}

func TestTaskClaimExplicitRouteWithoutEnvironment(t *testing.T) {
	dir, err := os.MkdirTemp("/private/tmp", "ink-task-route-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	t.Setenv("INK_DATA_DIR", dir)
	t.Setenv("INK_PROVIDER", "")
	t.Setenv("CC_PROJECT", "")
	t.Setenv("CC_SESSION_KEY", "")
	ln, err := net.Listen("unix", filepath.Join(dir, "ink.sock"))
	if err != nil {
		t.Fatal(err)
	}
	requests := make(chan map[string]any, 4)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		input := map[string]any{}
		if r.Method == http.MethodPost {
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
			}
		}
		input["path"] = r.URL.Path
		requests <- input
		io.WriteString(w, `{"first":true,"task":{"handoff":{"state":"claimed"}}}`)
	})}
	go server.Serve(ln)
	defer server.Close()

	id := uuid.NewString()
	if err := taskCommand([]string{
		"claim", "--task", id,
		"--provider", "cc-connect", "--target", "yu-bingtao-codex-acp", "--session", "discord:123",
	}); err != nil {
		t.Fatal(err)
	}
	claim := <-requests
	route, ok := claim["route"].(map[string]any)
	if !ok || route["provider"] != "cc-connect" || route["target"] != "yu-bingtao-codex-acp" || route["session"] != "discord:123" {
		t.Fatalf("explicit claim route not sent: %#v", claim["route"])
	}

	// The private credential keeps the route, so submit does not need the
	// original environment or repeated route flags.
	args := []string{"submit", "--task", id, "--handoff", "--request-id", "fixed", "--new-sha", strings.Repeat("b", 40)}
	if err := taskCommand(args); err != nil {
		t.Fatal(err)
	}
	submitted := <-requests
	route, ok = submitted["route"].(map[string]any)
	if !ok || route["target"] != "yu-bingtao-codex-acp" || route["session"] != "discord:123" {
		t.Fatalf("submit lost the explicit claim route: %#v", submitted["route"])
	}

	if err := taskCommand([]string{"claim", "--task", uuid.NewString(), "--target", "yu-bingtao-codex-acp"}); err == nil {
		t.Fatal("partial explicit route accepted")
	}
}
