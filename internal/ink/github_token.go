//go:build linux || darwin

package ink

import (
	"bytes"
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
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// GitHubCredential configures exactly one explicit source for a daemon role.
type GitHubCredential struct {
	TokenEnv string                  `json:"token_env,omitempty"`
	App      *GitHubAppCredential    `json:"app,omitempty"`
	Helper   *GitHubHelperCredential `json:"helper,omitempty"`
}

type GitHubAppCredential struct {
	AppID          int64  `json:"app_id"`
	InstallationID int64  `json:"installation_id"`
	PrivateKeyFile string `json:"private_key_file"`
}

type GitHubHelperCredential struct {
	Command  string   `json:"command"`
	Args     []string `json:"args,omitempty"`
	Identity string   `json:"identity"`
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var helperIdentity = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

func (c GitHubCredential) validate() error {
	switch {
	case c.TokenEnv != "" && c.App == nil && c.Helper == nil && envName.MatchString(c.TokenEnv):
		return nil
	case c.TokenEnv == "" && c.App != nil && c.Helper == nil:
		if c.App.AppID > 0 && c.App.InstallationID > 0 && filepath.IsAbs(c.App.PrivateKeyFile) {
			return nil
		}
	case c.TokenEnv == "" && c.App == nil && c.Helper != nil:
		if safeCredentialCommand(c.Helper.Command) && helperIdentity.MatchString(c.Helper.Identity) {
			for _, arg := range c.Helper.Args {
				if strings.ContainsRune(arg, 0) {
					return diagnosticError("invalid GitHub credential helper arguments")
				}
			}
			return nil
		}
	}
	return diagnosticError("configure one valid GitHub credential source per role")
}

type githubRole string

const (
	githubReader githubRole = "reader"
	githubMerger githubRole = "merger"
)

type tokenScope struct {
	host, repository, identity string
	role                       githubRole
}

type TokenProvider interface {
	Token(ctx context.Context, scope tokenScope) (token string, expiry time.Time, err error)
}

func fingerprint(values ...string) string {
	data, _ := json.Marshal(values)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func validBearer(token string) bool {
	return token != "" && len(token) <= 16<<10 &&
		strings.IndexFunc(token, func(r rune) bool { return r <= ' ' || r >= 127 }) == -1
}

type cachedToken struct {
	Token  string    `json:"token"`
	Expiry time.Time `json:"expires_at"`
}

type tokenSource struct {
	config    GitHubCredential
	literal   string
	getenv    func(string) string
	client    *http.Client
	api       string
	now       func() time.Time
	cache     *tokenCache
	throttle  *githubThrottle
	secretEnv []string
}

func (p tokenSource) pat() string {
	if p.config.TokenEnv != "" {
		return p.getenv(p.config.TokenEnv)
	}
	return p.literal
}

func (p tokenSource) reference() (string, error) {
	switch {
	case p.config.App != nil:
		app := p.config.App
		return fingerprint("app", strconv.FormatInt(app.AppID, 10), strconv.FormatInt(app.InstallationID, 10)), nil
	case p.config.Helper != nil:
		h := p.config.Helper
		ref, err := credentialCommandReference(h.Command, h.Args)
		return fingerprint("helper", h.Identity, ref), err
	default:
		token := p.pat()
		if !validBearer(token) {
			return "", diagnosticError("GitHub PAT is unavailable or invalid")
		}
		return fingerprint("pat", token), nil
	}
}

// Cache slots share no tokens across roles. Queries for one slot share a
// refresh while repositories with different credentials can refresh concurrently.
type tokenSlot struct {
	host, repository string
	role             githubRole
}

type tokenCacheEntry struct {
	identity string
	cachedToken
}

type tokenCache struct {
	mu      sync.Mutex
	entries map[tokenSlot]tokenCacheEntry
	running map[tokenSlot]chan struct{}
}

func (c *tokenCache) invalidate(host, repo string, role githubRole, rejected string) {
	slot := tokenSlot{host, strings.ToLower(repo), role}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries[slot].Token == rejected {
		delete(c.entries, slot)
	}
}

func (c *tokenCache) get(ctx context.Context, scope tokenScope, now func() time.Time, refresh func() (cachedToken, error)) (cachedToken, error) {
	slot := tokenSlot{scope.host, scope.repository, scope.role}
	for {
		c.mu.Lock()
		entry, ok := c.entries[slot]
		if ok && entry.identity == scope.identity && (entry.Expiry.IsZero() || now().Add(30*time.Second).Before(entry.Expiry)) {
			c.mu.Unlock()
			return entry.cachedToken, nil
		}
		if done := c.running[slot]; done != nil {
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				return cachedToken{}, ctx.Err()
			case <-done:
				continue
			}
		}
		done := make(chan struct{})
		c.running[slot] = done
		delete(c.entries, slot)
		c.mu.Unlock()
		token, err := refresh()
		c.mu.Lock()
		if err == nil {
			if len(c.entries) >= 256 {
				for key := range c.entries {
					delete(c.entries, key)
					break
				}
			}
			c.entries[slot] = tokenCacheEntry{scope.identity, token}
		}
		delete(c.running, slot)
		close(done)
		c.mu.Unlock()
		return token, err
	}
}

func (p tokenSource) Token(ctx context.Context, scope tokenScope) (string, time.Time, error) {
	var refresh func() (cachedToken, error)
	switch {
	case p.config.App != nil:
		key, err := readAppKey(p.config.App.PrivateKeyFile)
		if err != nil {
			return "", time.Time{}, err
		}
		// Rotate the cache entry with the signing key while keeping the App's
		// reconciliation identity stable across signing-key rotations.
		scope.identity = fingerprint(scope.identity, fmt.Sprintf("%x", sha256.Sum256(key)))
		refresh = func() (cachedToken, error) { return p.appToken(ctx, scope, key) }
	case p.config.Helper != nil:
		refresh = func() (cachedToken, error) { return p.helperToken(ctx, scope) }
	default:
		token := p.pat()
		if !validBearer(token) || fingerprint("pat", token) != scope.identity {
			return "", time.Time{}, diagnosticError("GitHub PAT is unavailable or changed")
		}
		refresh = func() (cachedToken, error) { return cachedToken{Token: token}, nil }
	}
	entry, err := p.cache.get(ctx, scope, p.now, refresh)
	return entry.Token, entry.Expiry, err
}

func readAppKey(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64<<10 || info.Mode().Perm()&0077 != 0 {
		return nil, diagnosticError("GitHub App private key must be a private regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, diagnosticError("GitHub App private key is unavailable")
	}
	defer f.Close()
	key, err := io.ReadAll(io.LimitReader(f, 64<<10))
	if err != nil {
		return nil, diagnosticError("GitHub App private key cannot be read")
	}
	return key, nil
}

func appJWT(id int64, key []byte, now time.Time) (string, error) {
	block, _ := pem.Decode(key)
	if block == nil {
		return "", diagnosticError("invalid GitHub App private key")
	}
	private, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		parsed, _ := x509.ParsePKCS8PrivateKey(block.Bytes)
		private, _ = parsed.(*rsa.PrivateKey)
	}
	if private == nil || private.N.BitLen() < 2048 {
		return "", diagnosticError("invalid GitHub App RSA private key")
	}
	claims, _ := json.Marshal(map[string]any{
		"iss": strconv.FormatInt(id, 10),
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(8 * time.Minute).Unix(),
	})
	encode := base64.RawURLEncoding.EncodeToString
	unsigned := encode([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + encode(claims)
	hash := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(rand.Reader, private, crypto.SHA256, hash[:])
	if err != nil {
		return "", diagnosticError("GitHub App JWT signing failed")
	}
	return unsigned + "." + encode(sig), nil
}

func (p tokenSource) appToken(ctx context.Context, scope tokenScope, key []byte) (cachedToken, error) {
	jwt, err := appJWT(p.config.App.AppID, key, p.now())
	if err != nil {
		return cachedToken{}, err
	}
	permissions := map[string]string{"contents": "read", "pull_requests": "read", "checks": "read"}
	if scope.role == githubMerger {
		permissions = map[string]string{"contents": "write", "pull_requests": "write"}
	}
	_, repo, _ := strings.Cut(scope.repository, "/")
	body, _ := json.Marshal(map[string]any{"repositories": []string{repo}, "permissions": permissions})
	endpoint := fmt.Sprintf("%s/app/installations/%d/access_tokens", p.api, p.config.App.InstallationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return cachedToken{}, diagnosticError("invalid GitHub App token request")
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", stackAPIVersion)
	req.Header.Set("Content-Type", "application/json")
	started := p.now()
	if err := p.throttle.waiting(started, scope.role); err != nil {
		return cachedToken{}, err
	}
	res, err := p.client.Do(req)
	if err != nil {
		return cachedToken{}, transportDiagnostic(err)
	}
	defer res.Body.Close()
	if err := p.throttle.response(res, started, p.now(), scope.role); err != nil {
		return cachedToken{}, err
	}
	if res.StatusCode != http.StatusCreated {
		return cachedToken{}, diagnosticError(fmt.Sprintf("GitHub App token exchange HTTP %d", res.StatusCode))
	}
	var result struct {
		cachedToken
		Repositories []struct {
			FullName string `json:"full_name"`
		} `json:"repositories"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&result); err != nil ||
		!validBearer(result.Token) || !p.now().Add(30*time.Second).Before(result.Expiry) {
		return cachedToken{}, diagnosticError("invalid GitHub App token response (HTTP 201)")
	}
	// Repository names are resolved by GitHub within the installation. Check
	// the owner as well to prevent a same-name repo from being used as scope.
	if len(result.Repositories) != 1 || !strings.EqualFold(result.Repositories[0].FullName, scope.repository) {
		return cachedToken{}, diagnosticError("GitHub App token response has unexpected repository scope (HTTP 201)")
	}
	return result.cachedToken, nil
}

func (p tokenSource) helperToken(ctx context.Context, scope tokenScope) (cachedToken, error) {
	helper := p.config.Helper
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, helper.Command, append(slices.Clone(helper.Args), "get")...)
	api, _ := url.Parse(p.api)
	cmd.Stdin = strings.NewReader("protocol=" + api.Scheme + "\nhost=" + scope.host + "\npath=" + scope.repository + "\n\n")
	output := &boundedOutput{remaining: 32 << 10}
	cmd.Stdout = output
	cleanup, err := isolateCredentialCommand(cmd, p.secretEnv)
	if err != nil {
		return cachedToken{}, err
	}
	defer cleanup()
	cmd.Env = append(cmd.Env, "INK_GITHUB_ROLE="+string(scope.role))
	if err := cmd.Run(); err != nil {
		return cachedToken{}, diagnosticError("GitHub credential helper failed (" + redactedError(ctxOrProcessError(ctx, err)) + ")")
	}
	raw := strings.TrimSpace(output.String())
	var result cachedToken
	if strings.HasPrefix(raw, "{") {
		if json.Unmarshal([]byte(raw), &result) != nil {
			return cachedToken{}, diagnosticError("invalid GitHub credential helper response")
		}
	} else if strings.Contains(raw, "\n") || strings.HasPrefix(raw, "password=") {
		for _, line := range strings.Split(raw, "\n") {
			name, value, _ := strings.Cut(strings.TrimSuffix(line, "\r"), "=")
			switch name {
			case "password":
				result.Token = value
			case "password_expiry_utc":
				seconds, err := strconv.ParseInt(value, 10, 64)
				if err != nil || seconds <= 0 {
					return cachedToken{}, diagnosticError("invalid GitHub credential helper expiry")
				}
				result.Expiry = time.Unix(seconds, 0)
			}
		}
	} else {
		result.Token = raw
	}
	if !validBearer(result.Token) || (!result.Expiry.IsZero() && !p.now().Add(30*time.Second).Before(result.Expiry)) {
		return cachedToken{}, diagnosticError("GitHub credential helper returned no usable token")
	}
	if result.Expiry.IsZero() {
		result.Expiry = p.now().Add(time.Minute)
	}
	return result, nil
}

func ctxOrProcessError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}
