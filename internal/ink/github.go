package ink

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"ink-commons/internal/execution"
)

var repoName = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func ValidRepository(name string) bool {
	return repoName.MatchString(name)
}

var shaPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)

type Repository struct {
	ReviewerCommand string           `json:"reviewer_command,omitempty"`
	Repo            string           `json:"repo"`
	HumanDeveloper  string           `json:"human_developer,omitempty"`
	ReviewerRoute   *execution.Route `json:"reviewer_route,omitempty"`
	ReviewerLogin   string           `json:"reviewer_login"`
}

type Config struct {
	Repositories  []Repository
	PollSeconds   int
	Reliability   Reliability
	GitHubToken   string
	GitHubCommand string
	PollGate      string
}

type upstream struct {
	command string
	token   string
	http    *http.Client
	github  string
}

func newUpstream(token string) upstream {
	return upstream{
		token:  token,
		github: "https://api.github.com",
		http: &http.Client{
			Timeout:       10 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

func (u upstream) gitHubGet(ctx context.Context, path string, result any) (http.Header, error) {
	if u.command != "" {
		return u.commandGet(ctx, path, result)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.github+path, nil)
	if err != nil {
		return nil, fmt.Errorf("invalid upstream request")
	}
	if u.token != "" {
		req.Header.Set("Authorization", "Bearer "+u.token)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	res, err := u.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstream transport failure")
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("upstream HTTP %d", res.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 32<<20)).Decode(result); err != nil {
		return nil, fmt.Errorf("invalid upstream response")
	}
	return res.Header, nil
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
func (u upstream) commandGet(ctx context.Context, path string, result any) (http.Header, error) {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) < 3 || parts[0] != "repos" || !ValidRepository(parts[1]+"/"+parts[2]) {
		return nil, fmt.Errorf("GitHub command requires a repository API path")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, u.command, "--profile", "inspect", "-R", parts[1]+"/"+parts[2], "api", "--include", "--method", "GET", path)
	output := &boundedOutput{remaining: 32 << 20}
	cmd.Stdout = output
	// Stderr 不继承：broker/CLI 的诊断可能包含本机认证上下文。
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("GitHub App command failed")
	}
	response, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(output.Bytes())), nil)
	if err != nil {
		return nil, fmt.Errorf("invalid GitHub command response")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream HTTP %d", response.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 32<<20)).Decode(result); err != nil {
		return nil, fmt.Errorf("invalid upstream response")
	}
	return response.Header, nil
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
