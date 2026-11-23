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
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"ink-commons/internal/execution"
)

var repoName = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func ValidRepository(name string) bool {
	return repoName.MatchString(name)
}

var shaPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)

type Repository struct {
	TaskBlocked     *TaskBlockedHook `json:"task_blocked,omitempty"`
	TaskMode        string           `json:"task_mode,omitempty"`
	Review          *ReviewFeatures  `json:"review,omitempty"`
	PostApproval    *ApprovalHook    `json:"postApproval,omitempty"`
	ReviewerCommand string           `json:"reviewer_command,omitempty"`
	Repo            string           `json:"repo"`
	HumanDeveloper  string           `json:"human_developer,omitempty"`
	AutoMerge       *AutoMergeConfig `json:"auto_merge,omitempty"`
	ReviewerRoute   *execution.Route `json:"reviewer_route,omitempty"`
	ReviewerLogin   string           `json:"reviewer_login"`
}

type Config struct {
	WorkspaceFullSlots  int
	RepositoryDefaults  Repository
	Repositories        []Repository
	PollSeconds         int
	Reliability         Reliability
	GitHubToken         string
	GitHubCommand       string
	GitHubCommandSHA256 string
	GitHubReadArgs      []string
	GitHubMergeArgs     []string
	GitHubMergerCommand string
	GitHubMergerSHA256  string
	PollGate            string
	ConfigOnly          bool
}

func (c Config) ValidateGitHubCommands() error {
	if (c.GitHubReadArgs != nil || c.GitHubMergeArgs != nil) && c.GitHubCommand == "" {
		return fmt.Errorf("GitHub CLI arguments require github_command")
	}
	for _, args := range [][]string{c.GitHubReadArgs, c.GitHubMergeArgs} {
		for _, arg := range args {
			if arg == "" || strings.ContainsRune(arg, 0) {
				return fmt.Errorf("GitHub CLI arguments must be nonempty and contain no NUL")
			}
		}
	}
	if c.GitHubMergerCommand != "" {
		if c.GitHubCommand == "" || !filepath.IsAbs(c.GitHubMergerCommand) || c.GitHubMergerSHA256 == "" {
			return fmt.Errorf("merger requires a dedicated absolute command, checksum and separate reader")
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
	lastOK        *atomic.Int64
	command       string
	commandDigest string
	readArgs      []string
	mergeArgs     []string
	mergerCommand string
	mergerDigest  string
	token         string
	http          *http.Client
	github        string
}

func newUpstream(token string) upstream {
	return upstream{
		lastOK: new(atomic.Int64),
		token:  token,
		github: "https://api.github.com",
		http: &http.Client{
			Timeout:       10 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// Only explicitly opted-in repositories may use the reader App for merge calls.
func (u upstream) merger(reuseReader bool) (upstream, error) {
	if reuseReader {
		if u.command == "" {
			return upstream{}, fmt.Errorf("auto_merge.reuse_reader_identity requires a GitHub App command")
		}
		u.token = ""
		return u, nil
	}
	if u.mergerCommand != "" {
		u.command, u.commandDigest = u.mergerCommand, u.mergerDigest
		u.token = ""
		return u, nil
	}
	if u.command != "" || u.token != "" {
		return upstream{}, fmt.Errorf("automatic merge requires a dedicated GitHub merger App")
	}
	return u, nil
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
	var res *http.Response
	var err error
	if u.command != "" {
		res, err = u.commandResponse(ctx, repo, method, path, profile, version, input)
	} else {
		var req *http.Request
		req, err = http.NewRequestWithContext(ctx, method, u.github+path, bytes.NewReader(input))
		if err != nil {
			return nil, 0, fmt.Errorf("invalid upstream request")
		}
		if u.token != "" {
			req.Header.Set("Authorization", "Bearer "+u.token)
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		if version != "" {
			req.Header.Set("X-GitHub-Api-Version", version)
		}
		if input != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		res, err = u.http.Do(req)
		if err != nil {
			err = fmt.Errorf("upstream transport failure")
		}
	}
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	status := res.StatusCode
	if (status < 200 || status >= 300) && !slices.Contains(acceptedErrors, status) {
		return nil, status, fmt.Errorf("upstream HTTP %d", status)
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 32<<20)).Decode(result); err != nil {
		return nil, status, fmt.Errorf("invalid upstream response (HTTP %d)", status)
	}
	u.lastOK.Store(time.Now().Unix())
	return res.Header, status, nil
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
		if !strings.Contains(header.Get("Link"), `rel="next"`) {
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
		return nil, fmt.Errorf("%w: %v", errAppCommandNotStarted, err)
	}
	cmd := exec.CommandContext(ctx, u.command, args...)
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
		return nil, fmt.Errorf("GitHub App command timed out or cancelled")
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
			return nil, fmt.Errorf("GitHub App command exited (status %d)", exit.ExitCode())
		}
		return nil, fmt.Errorf("GitHub App command could not run")
	}
	if parseErr != nil {
		return nil, fmt.Errorf("invalid GitHub command response")
	}
	return res, nil
}

type boundedOutput struct {
	bytes.Buffer
	remaining int
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	if len(p) > b.remaining {
		return 0, fmt.Errorf("GitHub output exceeds limit")
	}
	b.remaining -= len(p)
	return b.Buffer.Write(p)
}
