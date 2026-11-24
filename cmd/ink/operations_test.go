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

	"github.com/wujinhjun/ink-commons/internal/ink"
)

func TestOperationsUseGlobalFlagsAndExpectedAPI(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "ink-ops-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	t.Setenv("INK_DATA_DIR", filepath.Join(dir, "unused"))
	expected := make(chan struct{ method, path, key string }, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := <-expected
		if r.Method != want.method || r.URL.Path != want.path {
			t.Errorf("request %s %s, want %s %s", r.Method, r.URL.Path, want.method, want.path)
		}
		if want.key != "" {
			var body map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body[want.key] == nil {
				t.Errorf("missing %s in request: %v", want.key, err)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/work", "GET /v1/repositories":
			io.WriteString(w, "[]")
		case "GET /healthz":
			io.WriteString(w, `{"status":"ok","revision":"test","gate":"open"}`)
		case "GET /v1/self-check":
			io.WriteString(w, `{"status":"ok"}`)
		default:
			io.WriteString(w, `{"repo":"acme/project"}`)
		}
	})}
	if err := ink.InitializeState(dir); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "ink.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(socket, 0600); err != nil {
		t.Fatal(err)
	}
	go server.Serve(listener)
	defer server.Close()
	const ref = "acme/project#42"
	for _, tc := range []struct {
		args              []string
		method, path, key string
	}{
		{[]string{"--data", dir, "work", "--json", "list"}, "GET", "/v1/work", ""},
		{[]string{"work", "--data", dir, "show", ref, "--json"}, "GET", "/v1/work/acme/project/42", ""},
		{[]string{"work", "link", ref, "--github-login", "developer", "--data", dir, "--json"}, "POST", "/v1/work/acme/project/42/link", "github_login"},
		{[]string{"work", "assign", ref, "--role", "reviewer", "--data=" + dir, "--github-login", "reviewer", "--json"}, "POST", "/v1/work/acme/project/42/assign", "role"},
		{[]string{"work", "retry", ref, "--json", "--data", dir}, "POST", "/v1/work/acme/project/42/retry", ""},
		{[]string{"work", "confirm-stopped", "--task", "task", ref, "--reason", "verified exit", "--attempt", "attempt", "--stopped", "--data", dir, "--json"}, "POST", "/v1/work/acme/project/42/confirm-stopped", "attempt_id"},
		{[]string{"repo", "list", "--data", dir, "--json"}, "GET", "/v1/repositories", ""},
		{[]string{"repo", "add", "acme/project", "--github-login", "reviewer", "--data", dir, "--json"}, "POST", "/v1/repositories", "reviewer_login"},
		{[]string{"repo", "update", "acme/project", "--data", dir, "--json", "--values", `{"post_approval":null}`}, "PATCH", "/v1/repositories/acme/project", "post_approval"},
		{[]string{"repo", "remove", "acme/project", "--json", "--data", dir}, "DELETE", "/v1/repositories/acme/project", ""},
	} {
		expected <- struct{ method, path, key string }{tc.method, tc.path, tc.key}
		result := captureOperationsOutput(t, tc.args)
		if !json.Valid(result) {
			t.Fatalf("%v returned invalid JSON: %s", tc.args, result)
		}
	}
	// Both endpoints are queried even though configuration is optional.
	go func() {
		expected <- struct{ method, path, key string }{"GET", "/healthz", ""}
		expected <- struct{ method, path, key string }{"GET", "/v1/self-check", ""}
	}()
	result := captureOperationsOutput(t, []string{"doctor", "--data", dir, "--json"})
	var doctor struct {
		OK     bool
		Checks []doctorCheck
	}
	if err := json.Unmarshal(result, &doctor); err != nil || !doctor.OK || len(doctor.Checks) != 4 {
		t.Fatalf("unexpected doctor report: %s", result)
	}
}

func captureOperationsOutput(t *testing.T, args []string) []byte {
	t.Helper()
	out, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	original := os.Stdout
	os.Stdout = out
	err = run(args)
	os.Stdout = original
	if err != nil {
		t.Fatalf("%v: %v", args, err)
	}
	result, err := os.ReadFile(out.Name())
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestCLIRedactsPrivateDaemonErrorDetails(t *testing.T) {
	for _, message := range []string{
		"subprocess stderr: /Users/local/private/key.pem",
		"invalid response from https://user:credential@example.invalid",
		"Authorization: Bearer private-test-credential",
	} {
		err := daemonProblem(http.StatusConflict, message)
		if !strings.Contains(err.Error(), "409") || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "credential") {
			t.Fatalf("unsafe error: %v", err)
		}
	}
	if got := daemonProblem(409, "repository has an in-flight merge"); !strings.Contains(got.Error(), "in-flight merge") {
		t.Fatal(got)
	}
}
