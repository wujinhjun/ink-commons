//go:build linux || darwin

package ink

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestInstallationTokensAreRepositoryAndRoleScoped(t *testing.T) {
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(t.TempDir(), "app.pem")
	writeKey := func(key *rsa.PrivateKey) {
		t.Helper()
		if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeKey(private)
	now := time.Unix(1800000000, 0)
	up := newUpstream("")
	src := up.source(GitHubCredential{App: &GitHubAppCredential{AppID: 13, InstallationID: 37, PrivateKeyFile: keyFile}}, "")
	src.now = func() time.Time { return now }
	var calls atomic.Int32
	src.client.Transport = testTransport(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost || req.URL.Path != "/app/installations/37/access_tokens" {
			t.Errorf("unexpected App token endpoint")
		}
		jwt := strings.Split(strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "), ".")
		if len(jwt) != 3 {
			t.Error("invalid JWT structure")
			return nil, fmt.Errorf("invalid JWT")
		}
		signature, err := base64.RawURLEncoding.DecodeString(jwt[2])
		hash := sha256.Sum256([]byte(jwt[0] + "." + jwt[1]))
		if err != nil || rsa.VerifyPKCS1v15(&private.PublicKey, crypto.SHA256, hash[:], signature) != nil {
			t.Error("JWT signature did not validate")
		}
		claims, _ := base64.RawURLEncoding.DecodeString(jwt[1])
		var claim struct {
			Iss string `json:"iss"`
			Iat int64  `json:"iat"`
			Exp int64  `json:"exp"`
		}
		if json.Unmarshal(claims, &claim) != nil || claim.Iss != "13" || claim.Iat >= now.Unix() || claim.Exp > now.Add(10*time.Minute).Unix() {
			t.Error("incorrect JWT claims")
		}
		var body struct {
			Repositories []string          `json:"repositories"`
			Permissions  map[string]string `json:"permissions"`
		}
		if json.NewDecoder(req.Body).Decode(&body) != nil || len(body.Repositories) != 1 {
			t.Error("missing repository scope")
			return nil, fmt.Errorf("invalid scope")
		}
		access := body.Permissions["contents"]
		want := map[string]string{"contents": access, "pull_requests": access}
		if access == "read" {
			want["checks"] = "read"
		}
		if (access != "read" && access != "write") || !reflect.DeepEqual(want, body.Permissions) {
			t.Errorf("unexpected App permissions %v", body.Permissions)
		}
		w := httptest.NewRecorder()
		reply(w, http.StatusCreated, map[string]any{
			"token":        fmt.Sprintf("installation-%s-%d", access, calls.Add(1)),
			"expires_at":   now.Add(time.Hour),
			"repositories": []map[string]string{{"full_name": "acme/" + body.Repositories[0]}},
		})
		return w.Result(), nil
	})
	ref, _ := src.reference()
	reader := tokenScope{"github.com", "acme/project", ref, githubReader}
	read := func(scope tokenScope) string {
		t.Helper()
		token, _, err := src.Token(context.Background(), scope)
		if err != nil {
			t.Error(err)
		}
		return token
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if token := read(reader); !strings.HasPrefix(token, "installation-read-") {
				t.Error("expected a read token")
			}
		}()
	}
	wg.Wait()
	first := read(reader)
	merger := reader
	merger.role = githubMerger
	if calls.Load() != 1 || !strings.HasPrefix(read(merger), "installation-write-") {
		t.Fatal("reader and merger shared a token")
	}
	other := reader
	other.repository = "acme/other"
	if read(other) == first || read(reader) != first {
		t.Fatal("repository scopes shared a token")
	}
	other.host = "another.example"
	if token := read(other); token == first || calls.Load() != 4 {
		t.Fatal("hosts shared a token")
	}
	now = now.Add(time.Hour - 20*time.Second)
	if read(reader) == first || calls.Load() != 5 {
		t.Fatal("did not refresh a near-expiry token")
	}
	private, err = rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	writeKey(private)
	if token := read(reader); token == first || calls.Load() != 6 {
		t.Fatal("signing key rotation did not invalidate token cache")
	}
	scope := tokenScope{"github.com", "different/project", ref, githubReader}
	if _, _, err := src.Token(context.Background(), scope); err == nil {
		t.Fatal("accepted an installation token for a different repository owner")
	}
}

func TestExplicitCredentialHelperScopeAndNoAmbientFallback(t *testing.T) {
	t.Setenv("GH_TOKEN", "ambient-secret")
	t.Setenv("TEST_READER_PAT", "reader-secret")
	dir := t.TempDir()
	marker, command := filepath.Join(dir, "calls"), filepath.Join(dir, "helper")
	t.Setenv("INK_TEST_HELPER_CALLS", marker)
	script := `#!/bin/sh
set -eu
test "$1" = 'get'
test -z "${GH_TOKEN+x}" && test -z "${TEST_READER_PAT+x}"
test -n "$GH_CONFIG_DIR" && test ! -f "$GH_CONFIG_DIR/hosts.yml"
test "$INK_GITHUB_ROLE" = merger
test "$(cat)" = "$(printf 'protocol=https\nhost=github.com\npath=acme/project')"
printf x >> "$INK_TEST_HELPER_CALLS"
printf 'username=x-access-token\npassword=merger-installation-token\n\n'
`
	if err := os.WriteFile(command, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	up, err := configuredUpstream(Config{
		GitHubReader: &GitHubCredential{TokenEnv: "TEST_READER_PAT"},
		GitHubMerger: &GitHubCredential{Helper: &GitHubHelperCredential{Command: command, Identity: "fixture-merger"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	up.http.Transport = testTransport(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("Authorization") != "Bearer merger-installation-token" {
			t.Fatal("merge used the wrong credential")
		}
		w := httptest.NewRecorder()
		reply(w, 200, map[string]any{"merged": true, "sha": strings.Repeat("c", 40)})
		return w.Result(), nil
	})
	merge, err := up.resolveMerger("acme/project", false, "")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := merge.merge(context.Background(), "acme/project", 42, strings.Repeat("a", 40), "merge"); err != nil {
			t.Fatal(err)
		}
	}
	if calls, err := os.ReadFile(marker); err != nil || string(calls) != "x" {
		t.Fatal("helper token was not cached for its scope")
	}
	if _, err := up.mergeRole(true); err == nil {
		t.Fatal("accepted explicit identity reuse for a PAT")
	}
	if err := os.WriteFile(command, []byte("#!/bin/sh\necho helper-secret >&2\nexit 42\n"), 0700); err != nil {
		t.Fatal(err)
	}
	merge, err = up.resolveMerger("acme/project", false, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := merge.merge(context.Background(), "acme/project", 42, strings.Repeat("a", 40), "merge"); err == nil ||
		strings.Contains(err.Error(), "helper-secret") || !strings.Contains(err.Error(), "status 42") {
		t.Fatalf("expected sanitized helper error, got %v", err)
	}
}

func TestPrimaryAndSecondaryRateLimitCooldowns(t *testing.T) {
	throttle := githubThrottle{primary: make(map[githubRole]cooldown)}
	now := time.Now().Truncate(time.Second)
	respond := func(status int, remaining, retry, body string) error {
		w := httptest.NewRecorder()
		w.Header().Set("X-RateLimit-Remaining", remaining)
		w.Header().Set("Retry-After", retry)
		w.Header().Set("X-RateLimit-Reset", fmt.Sprint(now.Add(30*time.Second).Unix()))
		w.WriteHeader(status)
		_, _ = w.WriteString(body)
		res := w.Result()
		defer res.Body.Close()
		return throttle.response(res, now, now, githubReader)
	}
	if err := respond(403, "0", "", "private"); err == nil || throttle.waiting(now, githubReader) == nil ||
		throttle.waiting(now, githubMerger) != nil || throttle.waiting(now.Add(30*time.Second), githubReader) != nil {
		t.Fatal("primary cooldown should be limited to the affected role")
	}
	if err := respond(429, "1", "45", "private"); err == nil || throttle.waiting(now.Add(44*time.Second), githubMerger) == nil ||
		throttle.waiting(now.Add(45*time.Second), githubReader) != nil {
		t.Fatal("secondary cooldown did not honor Retry-After across roles")
	}
	now = now.Add(time.Minute)
	if err := respond(403, "1", "", "You have exceeded a secondary rate limit."); err == nil ||
		throttle.waiting(now.Add(119*time.Second), githubMerger) == nil || throttle.waiting(now.Add(2*time.Minute), githubReader) != nil {
		t.Fatal("headerless secondary limit did not apply exponential backoff")
	}
	now = now.Add(3 * time.Minute)
	if err := respond(403, "1", "", "permission denied"); err != nil || throttle.waiting(now, githubReader) != nil {
		t.Fatal("ordinary permission denial was classified as rate limiting")
	}
}
