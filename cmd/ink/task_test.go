//go:build linux || darwin

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
	"github.com/wujinhjun/ink-commons/internal/execution"
)

func TestTaskCLIUsesDurablePrivateCredentialAndIdempotentSubmission(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "ink-task-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	t.Setenv("INK_DATA_DIR", dir)
	t.Setenv("INK_PROVIDER", "")
	t.Setenv("INK_PROVIDER", "chat")
	t.Setenv("INK_TARGET", "developer")
	t.Setenv("INK_SESSION", "discord:123")
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
	if err := run([]string{"task", "claim", "--task", id}); err != nil {
		t.Fatal(err)
	}
	first := <-requests
	if err := run([]string{"task", "claim", "--task", id}); err != nil {
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
	if err := run(append([]string{"task"}, args...)); err != nil {
		t.Fatal(err)
	}
	submitted := <-requests
	if err := run(append([]string{"task"}, args...)); err != nil {
		t.Fatal(err)
	}
	replayed := <-requests
	a, _ := json.Marshal(submitted)
	b, _ := json.Marshal(replayed)
	if string(a) != string(b) {
		t.Fatal("submission retry changed identity/evidence")
	}
	if err := run([]string{"task", "status", "--task", id}); err != nil {
		t.Fatal(err)
	}
	if status := <-requests; status["token"] != nil {
		t.Fatal("status leaked credential")
	}
	t.Setenv("INK_TARGET", "other")
	if err := run([]string{"task", "claim", "--task", id}); err == nil {
		t.Fatal("credential reused from another route")
	}
}

func TestTaskCredentialRejectsSymlinksAndPublicFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claim.json")
	if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
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
	dir, err := os.MkdirTemp("/tmp", "ink-task-route-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	t.Setenv("INK_DATA_DIR", dir)
	t.Setenv("INK_PROVIDER", "")
	t.Setenv("INK_TARGET", "")
	t.Setenv("INK_SESSION", "")
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
	if err := run([]string{"task",
		"claim", "--task", id,
		"--provider", "chat", "--target", "yu-bingtao-codex-acp", "--session", "discord:123",
	}); err != nil {
		t.Fatal(err)
	}
	claim := <-requests
	route, ok := claim["route"].(map[string]any)
	if !ok || route["provider"] != "chat" || route["target"] != "yu-bingtao-codex-acp" || route["session"] != "discord:123" {
		t.Fatalf("explicit claim route not sent: %#v", claim["route"])
	}

	// The private credential keeps the route, so submit does not need the
	// original environment or repeated route flags.
	args := []string{"submit", "--task", id, "--handoff", "--request-id", "fixed", "--new-sha", strings.Repeat("b", 40)}
	if err := run(append([]string{"task"}, args...)); err != nil {
		t.Fatal(err)
	}
	submitted := <-requests
	route, ok = submitted["route"].(map[string]any)
	if !ok || route["target"] != "yu-bingtao-codex-acp" || route["session"] != "discord:123" {
		t.Fatalf("submit lost the explicit claim route: %#v", submitted["route"])
	}

	if err := run([]string{"task", "claim", "--task", uuid.NewString(), "--target", "yu-bingtao-codex-acp"}); err == nil {
		t.Fatal("partial explicit route accepted")
	}
}

func TestTaskClaimRecoversFromRejectedEnvironmentRoute(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "ink-task-reject-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	t.Setenv("INK_DATA_DIR", dir)
	t.Setenv("INK_PROVIDER", "chat")
	t.Setenv("INK_TARGET", "wrong-project")
	t.Setenv("INK_SESSION", "discord:999")
	ln, err := net.Listen("unix", filepath.Join(dir, "ink.sock"))
	if err != nil {
		t.Fatal(err)
	}
	requests := make(chan map[string]any, 2)
	attempts := 0
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		input := map[string]any{}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		requests <- input
		attempts++
		if attempts == 1 {
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"error":"claim route does not match task assignee"}`)
			return
		}
		io.WriteString(w, `{"first":true,"task":{"handoff":{"state":"claimed"}}}`)
	})}
	go server.Serve(ln)
	defer server.Close()

	id := uuid.NewString()
	if err := run([]string{"task", "claim", "--task", id}); err == nil {
		t.Fatal("rejected environment claim was accepted")
	}
	first := <-requests
	firstRoute, _ := first["route"].(map[string]any)
	if firstRoute["target"] != "wrong-project" {
		t.Fatalf("unexpected first route: %#v", firstRoute)
	}
	credential, err := readTaskCredential(filepath.Join(dir, "claims", id+".json"))
	if err != nil || !credential.Rejected {
		t.Fatalf("rejected claim was not recorded: %#v %v", credential, err)
	}

	if err := run([]string{"task",
		"claim", "--task", id,
		"--provider", "chat", "--target", "correct-project", "--session", "discord:123",
	}); err != nil {
		t.Fatal(err)
	}
	second := <-requests
	secondRoute, _ := second["route"].(map[string]any)
	if second["token"] != first["token"] || secondRoute["target"] != "correct-project" || secondRoute["session"] != "discord:123" {
		t.Fatalf("corrected claim did not reuse the credential: first=%#v second=%#v", first, second)
	}
	credential, err = readTaskCredential(filepath.Join(dir, "claims", id+".json"))
	if err != nil || credential.Rejected || credential.Route == nil || credential.Route.Target != "correct-project" {
		t.Fatalf("corrected credential not persisted: %#v %v", credential, err)
	}
}

func TestExplicitTaskRoutePreservesExactValues(t *testing.T) {
	route, err := explicitTaskRoute(" chat ", " target ", " discord:123 ")
	if err != nil {
		t.Fatal(err)
	}
	if route.Provider != " chat " || route.Target != " target " || route.Session != " discord:123 " {
		t.Fatalf("explicit route was normalized: %#v", route)
	}
}

func TestTaskClaimReplacesLegacyCredentialRoute(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "ink-task-legacy-route-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	t.Setenv("INK_DATA_DIR", dir)
	t.Setenv("INK_PROVIDER", "")
	t.Setenv("INK_TARGET", "")
	t.Setenv("INK_SESSION", "")
	claims := filepath.Join(dir, "claims")
	if err := os.Mkdir(claims, 0700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", filepath.Join(dir, "ink.sock"))
	if err != nil {
		t.Fatal(err)
	}
	requests := make(chan map[string]any, 2)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		input := map[string]any{}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		requests <- input
		io.WriteString(w, `{"first":true,"task":{"handoff":{"state":"claimed"}}}`)
	})}
	go server.Serve(ln)
	defer server.Close()

	id := uuid.NewString()
	token := strings.Repeat("a", 64)
	credential := taskCredential{
		TaskID: id,
		Token:  token,
		Route:  &execution.Route{Provider: "chat", Target: "old-target", Session: "discord:999"},
	}
	data, _ := json.Marshal(credential)
	path := filepath.Join(claims, id+".json")
	if err := writeTaskCredential(path, append(data, '\n')); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"task",
		"claim", "--task", id, "--replace-route",
		"--provider", "chat", "--target", "new-target", "--session", "discord:123",
	}); err != nil {
		t.Fatal(err)
	}
	request := <-requests
	route, _ := request["route"].(map[string]any)
	if request["token"] != token || route["target"] != "new-target" || route["session"] != "discord:123" {
		t.Fatalf("legacy credential was not replaced: %#v", request)
	}
	credential, err = readTaskCredential(path)
	if err != nil || credential.Rejected || credential.AttemptID == "" || credential.Route == nil || credential.Route.Target != "new-target" {
		t.Fatalf("replacement not persisted: %#v %v", credential, err)
	}

	if err := run([]string{"task", "claim", "--task", id, "--replace-route"}); err == nil || !strings.Contains(err.Error(), "--replace-route requires") {
		t.Fatalf("missing explicit route was not rejected: %v", err)
	}
}

func TestTaskRejectionIsBoundToTheAttempt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claim.json")
	id := uuid.NewString()
	token := strings.Repeat("b", 64)
	credential := taskCredential{
		TaskID:    id,
		Token:     token,
		Route:     &execution.Route{Provider: "chat", Target: "target", Session: "discord:123"},
		AttemptID: "current",
	}
	data, _ := json.Marshal(credential)
	if err := writeTaskCredential(path, append(data, '\n')); err != nil {
		t.Fatal(err)
	}
	if err := markTaskCredentialRejected(path, id, token, "older"); err != nil {
		t.Fatal(err)
	}
	credential, err := readTaskCredential(path)
	if err != nil || credential.Rejected {
		t.Fatalf("stale rejection changed the credential: %#v %v", credential, err)
	}
	if err := markTaskCredentialRejected(path, id, token, "current"); err != nil {
		t.Fatal(err)
	}
	credential, err = readTaskCredential(path)
	if err != nil || !credential.Rejected {
		t.Fatalf("current rejection was not recorded: %#v %v", credential, err)
	}
}

func TestTaskLockRejectsSymlinksAndPublicFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claim.json")
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("target"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path+".lock"); err != nil {
		t.Fatal(err)
	}
	if _, err := lockTaskCredential(path); err == nil {
		t.Fatal("symlink lock accepted")
	}
	info, err := os.Stat(target)
	if err != nil || info.Mode().Perm() != 0644 {
		t.Fatalf("symlink target mode changed: %#v %v", info, err)
	}
	if err := os.Remove(path + ".lock"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".lock", []byte("public"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path+".lock", 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := lockTaskCredential(path); err == nil {
		t.Fatal("public lock accepted")
	}
	info, err = os.Stat(path + ".lock")
	if err != nil || info.Mode().Perm() != 0644 {
		t.Fatalf("existing lock mode changed: %#v %v", info, err)
	}
	if err := os.Remove(path + ".lock"); err != nil {
		t.Fatal(err)
	}
	lock, err := lockTaskCredential(path)
	if err != nil {
		t.Fatal(err)
	}
	unlockTaskCredential(lock)
	info, err = os.Stat(path + ".lock")
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("new lock is not private: %#v %v", info, err)
	}
}
