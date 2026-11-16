package ink

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMergeUsesCodeProfileAndGuardedJSONStdin(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "app-cli")
	argsLog := filepath.Join(dir, "args")
	bodyLog := filepath.Join(dir, "body")
	t.Setenv("MERGE_TEST_ARGS", argsLog)
	t.Setenv("MERGE_TEST_BODY", bodyLog)
	body := `#!/bin/sh
printf '%s\n' "$*" > "$MERGE_TEST_ARGS"
cat > "$MERGE_TEST_BODY"
printf 'HTTP/2.0 200 OK\r\n\r\n{"merged":true,"sha":"dddddddddddddddddddddddddddddddddddddddd"}'
`
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	u := newUpstream("unused-token")
	u.command = script
	sha, err := u.merge(context.Background(), "acme/project", 42, strings.Repeat("a", 40), "squash")
	if err != nil || sha != strings.Repeat("d", 40) {
		t.Fatalf("%s %v", sha, err)
	}
	args, _ := os.ReadFile(argsLog)
	if strings.TrimSpace(string(args)) != "--profile code -R acme/project api --include --method PUT /repos/acme/project/pulls/42/merge --input -" {
		t.Fatal(string(args))
	}
	input, _ := os.ReadFile(bodyLog)
	var payload map[string]string
	if err := json.Unmarshal(input, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["sha"] != strings.Repeat("a", 40) || payload["merge_method"] != "squash" || strings.Contains(string(args), "unused-token") {
		t.Fatal("merge was not SHA-bound or leaked credentials")
	}
}

func TestGitHubCommandScopesRequestsAndKeepsPagination(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "app-cli")
	log := filepath.Join(dir, "calls")
	t.Setenv("TEST_CALLS", log)
	body := `#!/bin/sh
printf '%s\n' "$*" >> "$TEST_CALLS"
case "$*" in
 *page=1) printf 'HTTP/2.0 200 OK\r\nLink: <https://api.github.com/next>; rel="next"\r\n\r\n[1]' ;;
 *page=2) printf 'HTTP/2.0 200 OK\r\n\r\n[2]' ;;
 *) exit 1 ;;
esac
`
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	up := newUpstream("ignored-personal-token")
	up.command = script
	result, err := listPages[int](context.Background(), up, "/repos/acme/project/pulls")
	if err != nil || len(result) != 2 || result[0] != 1 || result[1] != 2 {
		t.Fatalf("%v %v", result, err)
	}
	calls, _ := os.ReadFile(log)
	for _, line := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
		if !strings.HasPrefix(line, "--profile inspect -R acme/project api --include --method GET /repos/acme/project/pulls?") {
			t.Fatal(line)
		}
	}
	// 每次调用 CLI，凭据由 broker 更新；不缓存进 daemon，也没有 HTTP token 回退。
	if strings.Count(string(calls), "--profile inspect") != 2 {
		t.Fatal(string(calls))
	}
}

func TestGitHubCommandFailureIsSanitizedAndDoesNotFallback(t *testing.T) {
	script := filepath.Join(t.TempDir(), "app-cli")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho secret >&2\necho secret\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	up := newUpstream("personal-token")
	up.command = script
	var result any
	_, err := up.gitHubGet(context.Background(), "/repos/acme/project/pulls", &result)
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatal(err)
	}
	_, err = up.gitHubGet(context.Background(), "/user", &result)
	if err == nil {
		t.Fatal("unscoped path accepted")
	}
}

func TestGitHubAppIntegration(t *testing.T) {
	command, repo := os.Getenv("INK_TEST_APP_COMMAND"), os.Getenv("INK_TEST_APP_REPO")
	if command == "" || repo == "" {
		t.Skip("explicit read-only App integration test")
	}
	if !ValidRepository(repo) {
		t.Fatal("invalid integration repository")
	}
	up := newUpstream("")
	up.command = command
	var result struct {
		FullName string `json:"full_name"`
	}
	if _, err := up.gitHubGet(context.Background(), "/repos/"+repo, &result); err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(result.FullName, repo) {
		t.Fatal("unexpected repository")
	}
}
