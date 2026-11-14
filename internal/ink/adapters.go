package ink

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

var repoName = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var shaPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)

type Repository struct {
	Repo            string `json:"repo"`
	HumanDeveloper  string `json:"human_developer,omitempty"`
	ReviewerLogin   string `json:"reviewer_login"`
	ReviewerProject string `json:"reviewer_project,omitempty"`
	ReviewChannel   string `json:"review_channel_id,omitempty"`
}

type Config struct {
	Repositories   []Repository `json:"repositories"`
	PollSeconds    int          `json:"poll_seconds"`
	GitHubToken    string       `json:"-"`
	DiscordToken   string       `json:"-"`
	CCConnectURL   string       `json:"cc_connect_url"`
	CCConnectToken string       `json:"-"`
}

func LoadConfig(path string) (Config, error) {
	var c Config
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer f.Close()
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&c); err != nil {
		return c, err
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		return c, fmt.Errorf("expected one configuration object")
	}
	c.GitHubToken = os.Getenv("INK_GITHUB_TOKEN")
	c.DiscordToken = os.Getenv("INK_DISCORD_TOKEN")
	c.CCConnectToken = os.Getenv("INK_CC_CONNECT_TOKEN")
	if c.PollSeconds < 0 || c.PollSeconds > 86400 {
		return c, fmt.Errorf("poll_seconds must be between 0 and 86400")
	}
	if c.PollSeconds == 0 {
		c.PollSeconds = 60
	}
	if len(c.Repositories) == 0 {
		return c, fmt.Errorf("configure at least one repository")
	}
	if c.CCConnectURL != "" {
		u, err := url.Parse(c.CCConnectURL)
		if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Scheme != "http" || (u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" && u.Hostname() != "::1") || c.CCConnectToken == "" {
			return c, fmt.Errorf("cc_connect_url must be a loopback HTTP endpoint and INK_CC_CONNECT_TOKEN must be set")
		}
	}
	seen := map[string]bool{}
	for i := range c.Repositories {
		repo := &c.Repositories[i]
		repo.Repo = strings.ToLower(repo.Repo)
		if !repoName.MatchString(repo.Repo) || seen[repo.Repo] || repo.ReviewerLogin == "" {
			return c, fmt.Errorf("each repository needs a unique owner/repo and reviewer_login")
		}
		seen[repo.Repo] = true
		if repo.ReviewerProject != "" && (!snowflake(repo.ReviewChannel) || c.DiscordToken == "" || c.CCConnectURL == "") {
			return c, fmt.Errorf("bot review requires review_channel_id, INK_DISCORD_TOKEN and cc-connect")
		}
	}
	return c, nil
}

type upstream struct {
	config  Config
	http    *http.Client
	github  string
	discord string
}

func newUpstream(config Config) upstream {
	return upstream{
		config:  config,
		github:  "https://api.github.com",
		discord: "https://discord.com/api/v10",
		http: &http.Client{
			Timeout:       10 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

func (u upstream) request(ctx context.Context, method, endpoint, auth string, payload, result any) (http.Header, error) {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("invalid upstream request")
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if strings.HasPrefix(endpoint, u.github+"/") {
		req.Header.Set("Accept", "application/vnd.github+json")
	}
	res, err := u.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstream transport failure")
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("upstream HTTP %d", res.StatusCode)
	}
	if result != nil {
		if err := json.NewDecoder(io.LimitReader(res.Body, 32<<20)).Decode(result); err != nil {
			return nil, fmt.Errorf("invalid upstream response")
		}
	}
	return res.Header, nil
}

func (u upstream) gitHubGet(ctx context.Context, path string, result any) (http.Header, error) {
	auth := ""
	if u.config.GitHubToken != "" {
		auth = "Bearer " + u.config.GitHubToken
	}
	return u.request(ctx, http.MethodGet, u.github+path, auth, nil, result)
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

func (u upstream) createThread(ctx context.Context, repo Repository, work Work) (string, error) {
	name := fmt.Sprintf("Review %s #%d", repo.Repo, work.Pull.Number)
	if len(name) > 100 {
		name = name[:100]
	}
	body := map[string]any{"name": name, "type": 11}
	var thread struct {
		ID string `json:"id"`
	}
	_, err := u.request(ctx, http.MethodPost, u.discord+"/channels/"+repo.ReviewChannel+"/threads", "Bot "+u.config.DiscordToken, body, &thread)
	if err != nil {
		return "", err
	}
	if !snowflake(thread.ID) {
		return "", fmt.Errorf("Discord returned an invalid thread ID")
	}
	return thread.ID, nil
}

func (u upstream) notify(ctx context.Context, route Route, work Work) error {
	if u.config.CCConnectURL == "" {
		return fmt.Errorf("cc-connect is not configured")
	}
	task := work.Task
	prompt := fmt.Sprintf("Ink Commons %s task for PR %s\nCommit: %s\n", task.Role, work.Pull.URL, task.SHA)
	if task.Role == "reviewer" {
		prompt += "Review this exact commit and submit the official review in GitHub against that commit. Use Approve or Request changes."
	} else {
		prompt += "Read the GitHub review at " + task.ReviewURL + " and address its findings. Check that the PR head is still the indicated commit before working."
	}
	body := map[string]string{"project": route.Project, "session_key": route.SessionKey, "prompt": prompt, "event": "ink-commons:" + task.Role}
	var ack struct {
		Status string `json:"status"`
	}
	_, err := u.request(ctx, http.MethodPost, u.config.CCConnectURL, "Bearer "+u.config.CCConnectToken, body, &ack)
	if err != nil {
		return err
	}
	if ack.Status != "accepted" {
		return fmt.Errorf("cc-connect did not accept the task")
	}
	return nil
}
