package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ink-commons/internal/execution"
	"ink-commons/internal/execution/ccconnect"
	"ink-commons/internal/execution/httpagent"
	"ink-commons/internal/ink"
)

func loadConfig(path string) (ink.Config, map[string]execution.Executor, error) {
	var file struct {
		RepositoryDefaults ink.Repository `json:"repository_defaults,omitempty"`
		Repositories       []struct {
			ink.Repository
			ReviewerProject string `json:"reviewer_project,omitempty"`
			ReviewChannel   string `json:"review_channel_id,omitempty"`
		} `json:"repositories"`
		Reliability   ink.Reliability `json:"execution"`
		GitHubCommand string          `json:"github_command,omitempty"`
		PollSeconds   int             `json:"poll_seconds"`
		CCConnectURL  string          `json:"cc_connect_url"`
		HTTPURL       string          `json:"http_provider_url,omitempty"`
		HTTPExecution bool            `json:"http_provider_execution,omitempty"`
	}
	var cfg ink.Config
	f, err := os.Open(path)
	if err != nil {
		return cfg, nil, err
	}
	defer f.Close()
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&file); err != nil {
		return cfg, nil, err
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		return cfg, nil, fmt.Errorf("expected one configuration object")
	}
	cfg.Reliability = file.Reliability
	if err := cfg.Reliability.Validate(); err != nil {
		return cfg, nil, err
	}
	cfg.PollSeconds = file.PollSeconds
	cfg.GitHubCommand = file.GitHubCommand
	cfg.GitHubToken = os.Getenv("INK_GITHUB_TOKEN")
	if cfg.GitHubCommand != "" && !filepath.IsAbs(cfg.GitHubCommand) {
		return cfg, nil, fmt.Errorf("github_command must be an absolute App CLI path")
	}
	if cfg.PollSeconds < 0 || cfg.PollSeconds > 86400 {
		return cfg, nil, fmt.Errorf("poll_seconds must be between 0 and 86400")
	}
	if cfg.PollSeconds == 0 {
		cfg.PollSeconds = 60
	}
	cfg.RepositoryDefaults = file.RepositoryDefaults
	if err := cfg.RepositoryDefaults.AutoMerge.Validate(); err != nil {
		return cfg, nil, err
	}
	if cfg.RepositoryDefaults.Repo != "" {
		return cfg, nil, fmt.Errorf("repository_defaults must not set repo")
	}
	if err := cfg.RepositoryDefaults.PostApproval.Validate(); err != nil {
		return cfg, nil, err
	}
	if err := cfg.RepositoryDefaults.ValidateReviewFeatures(); err != nil {
		return cfg, nil, err
	}

	client := &http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	// Keep accepting historical cc-connect origins even on a daemon with no
	// delivery endpoint. Selecting one as the default reviewer requires its endpoint.
	cc, err := ccconnect.New(client, file.CCConnectURL, os.Getenv("INK_CC_CONNECT_TOKEN"), os.Getenv("INK_DISCORD_TOKEN"))
	if err != nil {
		return cfg, nil, err
	}
	providers := map[string]execution.Executor{ccconnect.Name: cc}
	if file.HTTPURL != "" {
		newAgent := httpagent.New
		if file.HTTPExecution {
			newAgent = httpagent.NewManaged
		}
		agent, err := newAgent(client, file.HTTPURL, os.Getenv("INK_HTTP_PROVIDER_TOKEN"))
		if err != nil {
			return cfg, nil, err
		}
		providers[httpagent.Name] = agent
	}

	if file.HTTPExecution && file.HTTPURL == "" {
		return cfg, nil, fmt.Errorf("http_provider_execution requires http_provider_url")
	}
	if route := cfg.RepositoryDefaults.ReviewerRoute; route != nil {
		if route.Provider == ccconnect.Name && file.CCConnectURL == "" {
			return cfg, nil, fmt.Errorf("configure cc_connect_url for repository_defaults")
		}
		if err := execution.ValidateDestination(providers, *route, true); err != nil {
			return cfg, nil, err
		}
	}
	seen := make(map[string]bool)
	for _, entry := range file.Repositories {
		repo := entry.Repository
		if err := repo.AutoMerge.Validate(); err != nil {
			return cfg, nil, err
		}
		repo.Repo = strings.ToLower(repo.Repo)
		if err := repo.PostApproval.Validate(); err != nil {
			return cfg, nil, err
		}
		if !ink.ValidRepository(repo.Repo) || seen[repo.Repo] || repo.ReviewerLogin == "" {
			return cfg, nil, fmt.Errorf("each repository needs a unique owner/repo and reviewer_login")
		}
		seen[repo.Repo] = true
		if entry.ReviewerProject != "" || entry.ReviewChannel != "" {
			if repo.ReviewerRoute != nil {
				return cfg, nil, fmt.Errorf("%s: choose reviewer_route or the legacy reviewer fields", repo.Repo)
			}
			repo.ReviewerRoute = &execution.Route{Provider: ccconnect.Name, Target: entry.ReviewerProject, Parent: entry.ReviewChannel}
		}
		if route := repo.ReviewerRoute; route != nil {
			if route.Provider == ccconnect.Name && file.CCConnectURL == "" {
				return cfg, nil, fmt.Errorf("%s: configure cc_connect_url for the reviewer", repo.Repo)
			}
			if err := execution.ValidateDestination(providers, *route, true); err != nil {
				return cfg, nil, fmt.Errorf("%s reviewer: %w", repo.Repo, err)
			}
		}
		if err := repo.ValidateReviewFeatures(); err != nil {
			return cfg, nil, err
		}
		cfg.Repositories = append(cfg.Repositories, repo)
	}
	return cfg, providers, nil
}

func routeFromEnv(getenv func(string) string) execution.Route {
	if provider := getenv("INK_PROVIDER"); provider != "" {
		return execution.Route{Provider: provider, Target: getenv("INK_TARGET"), Session: getenv("INK_SESSION")}
	}
	return ccconnect.RouteFromEnv(getenv)
}
