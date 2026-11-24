//go:build linux || darwin

package ink

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

var repoName = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func ValidRepository(name string) bool {
	owner, repo, _ := strings.Cut(name, "/")
	return repoName.MatchString(name) && owner != "." && owner != ".." && repo != "." && repo != ".."
}

var shaPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)

func (c Config) ValidateGitHubCommands() error {
	for _, credential := range []*GitHubCredential{c.GitHubReader, c.GitHubMerger} {
		if credential != nil {
			if err := credential.validate(); err != nil {
				return err
			}
		}
	}
	if (c.GitHubReader != nil && (c.GitHubCommand != "" || c.GitHubToken != "")) ||
		(c.GitHubMerger != nil && c.GitHubMergerCommand != "") {
		return diagnosticError("configure only one GitHub credential source per role")
	}
	if (c.GitHubCommand != "" && !safeCredentialCommand(c.GitHubCommand)) ||
		(c.GitHubMergerCommand != "" && !safeCredentialCommand(c.GitHubMergerCommand)) {
		return diagnosticError("GitHub command requires an absolute App wrapper")
	}
	if c.GitHubReader != nil && c.GitHubMerger != nil && c.GitHubReader.sameIdentity(*c.GitHubMerger) {
		return diagnosticError("GitHub reader and merger credentials must be separate")
	}
	if (c.GitHubReader != nil && c.GitHubReader.Helper != nil && sameAppCommand(c.GitHubReader.Helper.Command, c.GitHubMergerCommand)) ||
		(c.GitHubMerger != nil && c.GitHubMerger.Helper != nil && sameAppCommand(c.GitHubMerger.Helper.Command, c.GitHubCommand)) {
		return diagnosticError("GitHub reader and merger commands must be separate")
	}
	if (c.GitHubReadArgs != nil && c.GitHubCommand == "") ||
		(c.GitHubMergeArgs != nil && c.GitHubCommand == "" && c.GitHubMergerCommand == "") {
		return fmt.Errorf("GitHub CLI arguments require the corresponding GitHub command")
	}
	for _, args := range [][]string{c.GitHubReadArgs, c.GitHubMergeArgs} {
		for _, arg := range args {
			if arg == "" || strings.ContainsRune(arg, 0) {
				return fmt.Errorf("GitHub CLI arguments must be nonempty and contain no NUL")
			}
		}
	}
	if c.GitHubMergerCommand != "" {
		if c.GitHubMergerSHA256 == "" {
			return fmt.Errorf("merger requires a dedicated absolute command and checksum")
		}
		if sameAppCommand(c.GitHubCommand, c.GitHubMergerCommand) {
			return fmt.Errorf("merger and reader must use separate App commands")
		}
	}
	if err := checkCommandDigest(c.GitHubCommand, c.GitHubCommandSHA256); err != nil {
		return fmt.Errorf("reader command: %w", err)
	}
	if err := checkCommandDigest(c.GitHubMergerCommand, c.GitHubMergerSHA256); err != nil {
		return fmt.Errorf("merger command: %w", err)
	}
	return nil
}

func sameAppCommand(first, second string) bool {
	if first == "" || second == "" {
		return false
	}
	if filepath.Clean(first) == filepath.Clean(second) {
		return true
	}
	a, errA := os.Stat(first)
	b, errB := os.Stat(second)
	return errA == nil && errB == nil && os.SameFile(a, b)
}

func checkCommandDigest(command, digest string) error {
	if digest == "" {
		return nil // Older installations can adopt a pin at their next deployment.
	}
	raw, err := hex.DecodeString(digest)
	if err != nil || len(raw) != sha256.Size || !filepath.IsAbs(command) {
		return fmt.Errorf("invalid App command checksum or path")
	}
	file, err := os.Open(command)
	if err != nil {
		return fmt.Errorf("App command is unavailable")
	}
	defer file.Close()
	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil || !bytes.Equal(h.Sum(nil), raw) {
		return fmt.Errorf("App command checksum mismatch")
	}
	return nil
}

type upstream struct {
	observations                 *observationCache
	observeETags                 bool
	credential, mergerCredential *GitHubCredential
	expectedIdentity             string
	tokens                       *tokenCache
	throttle                     *githubThrottle
	secretEnv                    []string
	lastOK                       *atomic.Int64
	command                      string
	commandDigest                string
	readArgs                     []string
	mergeArgs                    []string
	mergerCommand                string
	mergerDigest                 string
	token                        string
	http                         *http.Client
	github                       string
}

func newUpstream(token string) upstream {
	return upstream{
		observations: newObservationCache(),
		lastOK:       new(atomic.Int64),
		tokens:       &tokenCache{entries: make(map[tokenSlot]tokenCacheEntry), running: make(map[tokenSlot]chan struct{})},
		throttle:     &githubThrottle{primary: make(map[githubRole]cooldown)},
		token:        token,
		github:       "https://api.github.com",
		http: &http.Client{
			Timeout:       10 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

func (u upstream) gitHubGet(ctx context.Context, path string, result any) (http.Header, error) {
	header, _, err := u.gitHubJSON(ctx, http.MethodGet, path, "inspect", "", nil, result)
	return header, err
}

const stackAPIVersion = "2026-03-10"

func (u upstream) gitHubJSON(ctx context.Context, method, path, profile, version string, input []byte, result any, acceptedErrors ...int) (http.Header, int, error) {
	return u.gitHubScopedJSON(ctx, "", method, path, profile, version, input, result, acceptedErrors...)
}

func (u upstream) gitHubScopedJSON(ctx context.Context, repo, method, path, profile, version string, input []byte, result any, acceptedErrors ...int) (http.Header, int, error) {
	repo, err := repositoryScope(repo, method, path)
	if err != nil {
		return nil, 0, err
	}
	role := githubReader
	if profile == "code" {
		role = githubMerger
	}
	started := time.Now()
	if err := u.throttle.waiting(started, role); err != nil {
		return nil, 0, notSubmitted(err)
	}
	var res *http.Response
	var token string
	var cacheKey observationCacheKey
	var cached cachedObservation
	var conditional bool
	if u.command != "" {
		if u.expectedIdentity != "" {
			if err := u.checkMergeIdentity(repo); err != nil {
				return nil, 0, notSubmitted(err)
			}
		}
		res, err = u.commandResponse(ctx, repo, method, path, profile, version, input)
	} else {
		var tokenErr error
		token, tokenErr = u.requestToken(ctx, repo, role)
		if tokenErr != nil {
			return nil, 0, notSubmitted(tokenErr)
		}
		req, requestErr := http.NewRequestWithContext(ctx, method, u.github+path, bytes.NewReader(input))
		if requestErr != nil {
			return nil, 0, notSubmitted(diagnosticError("invalid upstream request"))
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if method == http.MethodGet {
			if u.observeETags && role == githubReader && token != "" {
				cacheKey = observationCacheKey{
					scope: tokenScope{u.apiHost(), strings.ToLower(repo), fingerprint(token), role},
					url:   req.URL.String(),
					api:   version,
				}
				cached, conditional = u.observations.get(cacheKey)
				if conditional {
					req.Header.Set("If-None-Match", cached.tag)
				}
			} else {
				req.Header.Set("Cache-Control", "no-cache")
			}
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		if version != "" {
			req.Header.Set("X-GitHub-Api-Version", version)
		}
		if input != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if err := u.throttle.waiting(time.Now(), role); err != nil {
			return nil, 0, notSubmitted(err)
		}
		res, err = u.http.Do(req)
		if err != nil {
			err = transportDiagnostic(err)
		}
	}
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	status := res.StatusCode
	if status == http.StatusUnauthorized && token != "" {
		u.tokens.invalidate(u.apiHost(), repo, role, token)
	}
	mergeWrite := role == githubMerger && method == http.MethodPut
	if err := u.throttle.response(res, started, time.Now(), role); err != nil {
		if mergeWrite {
			err = mergeRequestRefused{err}
		}
		return nil, status, err
	}
	if mergeWrite && status == http.StatusUnauthorized {
		return nil, status, mergeRequestRefused{diagnosticError("upstream HTTP 401")}
	}
	if cacheKey.url != "" {
		header, handled, err := u.observations.read(res, cacheKey, cached, conditional, started, result)
		if err != nil {
			return nil, status, err
		}
		if handled {
			u.lastOK.Store(time.Now().Unix())
			return header, http.StatusOK, nil
		}
	}
	if (status < 200 || status >= 300) && !slices.Contains(acceptedErrors, status) {
		return nil, status, diagnosticError(fmt.Sprintf("upstream HTTP %d", status))
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 32<<20)).Decode(result); err != nil {
		return nil, status, diagnosticError(fmt.Sprintf("invalid upstream response (HTTP %d)", status))
	}
	u.lastOK.Store(time.Now().Unix())
	return res.Header, status, nil
}

func transportDiagnostic(err error) error {
	var timeout net.Error
	switch {
	case errors.Is(err, context.Canceled):
		return diagnosticError("upstream request cancelled")
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &timeout) && timeout.Timeout():
		return diagnosticError("upstream request timed out")
	default:
		return diagnosticError("upstream transport failure")
	}
}

func listPages[T any](ctx context.Context, u upstream, path string) ([]T, error) {
	var all []T
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	for page := 1; ; page++ {
		var items []T
		query := fmt.Sprintf("%s%sper_page=100&page=%d", path, separator, page)
		header, err := u.gitHubGet(ctx, query, &items)
		if err != nil {
			return nil, err
		}
		all = append(all, items...)
		if !strings.Contains(strings.Join(header.Values("Link"), ","), `rel="next"`) {
			return all, nil
		}
	}
}

// Only errors known to occur before the child process starts may release a
// merge reservation for retry.
var errAppCommandNotStarted = errors.New("GitHub App command did not start")

// App CLI 每次请求获取单仓库短期凭据，不把一小时 token 固定在常驻进程中。
// 不经过 shell，不读取个人 gh 登录；失败时只返回通用错误，避免转发子进程秘密。
func (u upstream) commandResponse(ctx context.Context, repo, method, path, profile, version string, input []byte) (*http.Response, error) {
	if path == "/graphql" && method == http.MethodPost && profile == "inspect" {
		if !ValidRepository(repo) {
			return nil, fmt.Errorf("GitHub GraphQL requires a repository scope")
		}
		path = "graphql"
	} else {
		parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
		if len(parts) < 3 || parts[0] != "repos" || !ValidRepository(parts[1]+"/"+parts[2]) {
			return nil, fmt.Errorf("GitHub command requires a repository API path")
		}
		repo = parts[1] + "/" + parts[2]
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if !safeCredentialCommand(u.command) {
		return nil, notSubmitted(diagnosticError("GitHub command requires an explicit App wrapper"))
	}
	prefix := u.readArgs
	if profile == "code" {
		prefix = u.mergeArgs
	}
	args := make([]string, 0, len(prefix)+9)
	if prefix == nil {
		args = append(args, "--profile", profile, "-R", repo)
	}
	for _, arg := range prefix {
		args = append(args, strings.ReplaceAll(arg, "{repo}", repo))
	}
	args = append(args, "api", "--include", "--method", method, path)
	if version != "" {
		args = append(args, "--header", "X-GitHub-Api-Version: "+version)
	}
	if input != nil {
		args = append(args, "--input", "-")
	}
	if err := checkCommandDigest(u.command, u.commandDigest); err != nil {
		return nil, notSubmitted(diagnosticError(err.Error()))
	}
	cmd := exec.CommandContext(ctx, u.command, args...)
	cleanup, err := isolateCredentialCommand(cmd, u.secretEnv)
	if err != nil {
		return nil, notSubmitted(err)
	}
	defer cleanup()
	role := githubReader
	if profile == "code" {
		role = githubMerger
	}
	cmd.Env = append(cmd.Env, "INK_GITHUB_ROLE="+string(role))
	if input != nil {
		cmd.Stdin = bytes.NewReader(input)
	}
	output := &boundedOutput{remaining: 32 << 20}
	cmd.Stdout = output
	// Stderr may contain broker authentication data. gh also prints the HTTP
	// response to stdout with --include, even for HTTP errors.
	if err := cmd.Start(); err != nil {
		return nil, errAppCommandNotStarted
	}
	runErr := cmd.Wait()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	res, parseErr := http.ReadResponse(bufio.NewReader(bytes.NewReader(output.Bytes())), nil)
	if runErr != nil {
		if parseErr == nil {
			if res.StatusCode >= 400 {
				return res, nil
			}
			res.Body.Close()
		}
		var exit *exec.ExitError
		if errors.As(runErr, &exit) {
			return nil, diagnosticError(fmt.Sprintf("GitHub App command exited (status %d)", exit.ExitCode()))
		}
		return nil, diagnosticError("GitHub App command could not run")
	}
	if parseErr != nil {
		return nil, diagnosticError("invalid GitHub command response")
	}
	return res, nil
}

type boundedOutput struct {
	bytes.Buffer
	remaining int
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	if len(p) > b.remaining {
		return 0, diagnosticError("GitHub output exceeds limit")
	}
	b.remaining -= len(p)
	return b.Buffer.Write(p)
}
