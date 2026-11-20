package ink

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
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
	RepositoryDefaults Repository
	Repositories       []Repository
	PollSeconds        int
	Reliability        Reliability
	GitHubToken        string
	GitHubCommand      string
	PollGate           string
}

type upstream struct {
	lastOK  *atomic.Int64
	command string
	token   string
	http    *http.Client
	github  string
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

func (u upstream) gitHubGet(ctx context.Context, path string, result any) (http.Header, error) {
	header, _, err := u.gitHubJSON(ctx, http.MethodGet, path, "inspect", "", nil, result)
	return header, err
}

const stackAPIVersion = "2026-03-10"

func (u upstream) gitHubJSON(ctx context.Context, method, path, profile, version string, input []byte, result any, acceptedErrors ...int) (http.Header, int, error) {
	var res *http.Response
	var err error
	if u.command != "" {
		res, err = u.commandResponse(ctx, method, path, profile, version, input)
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

// App CLI 每次请求获取单仓库短期凭据，不把一小时 token 固定在常驻进程中。
// 不经过 shell，不读取个人 gh 登录；失败时只返回通用错误，避免转发子进程秘密。
func (u upstream) commandResponse(ctx context.Context, method, path, profile, version string, input []byte) (*http.Response, error) {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) < 3 || parts[0] != "repos" || !ValidRepository(parts[1]+"/"+parts[2]) {
		return nil, fmt.Errorf("GitHub command requires a repository API path")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	args := []string{"--profile", profile, "-R", parts[1] + "/" + parts[2], "api", "--include", "--method", method, path}
	if version != "" {
		args = append(args, "--header", "X-GitHub-Api-Version: "+version)
	}
	if input != nil {
		args = append(args, "--input", "-")
	}
	cmd := exec.CommandContext(ctx, u.command, args...)
	if input != nil {
		cmd.Stdin = bytes.NewReader(input)
	}
	output := &boundedOutput{remaining: 32 << 20}
	cmd.Stdout = output
	// Stderr may contain broker authentication data. gh also prints the HTTP
	// response to stdout with --include, even for HTTP errors.
	runErr := cmd.Run()
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
