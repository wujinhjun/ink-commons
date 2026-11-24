//go:build linux || darwin

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/wujinhjun/ink-commons/internal/execution"
	"github.com/wujinhjun/ink-commons/internal/execution/httpagent"
	"github.com/wujinhjun/ink-commons/internal/ink"
)

// DaemonConfig contains only static process/provider settings. Mutable policy
// lives in SQLite; optional policy settings seed a newly initialized database.
type DaemonConfig struct {
	LogLevel            string                    `json:"log_level,omitempty"`
	LogFormat           string                    `json:"log_format,omitempty"`
	GitHubCommand       string                    `json:"github_command,omitempty"`
	GitHubCommandSHA256 string                    `json:"github_command_sha256,omitempty"`
	GitHubMergerCommand string                    `json:"github_merger_command,omitempty"`
	GitHubReadArgs      []string                  `json:"github_read_args"`
	GitHubMergeArgs     []string                  `json:"github_merge_args"`
	GitHubMergerSHA256  string                    `json:"github_merger_command_sha256,omitempty"`
	WorkspaceFullSlots  int                       `json:"workspace_full_slots"`
	PollSeconds         int                       `json:"poll_seconds"`
	Providers           map[string]ProviderConfig `json:"providers,omitempty"`
}

type ProviderConfig struct {
	URL          string   `json:"url"`
	TokenEnv     string   `json:"token_env,omitempty"`
	Managed      bool     `json:"managed,omitempty"`
	Capabilities []string `json:"capabilities"`
	RequireACK   bool     `json:"require_ack,omitempty"`
}

var providerName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
var environmentName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func loadConfig(path string) (ink.Config, map[string]execution.Executor, error) {
	var file struct {
		RepositoryDefaults ink.Repository   `json:"repository_defaults,omitempty"`
		Repositories       []ink.Repository `json:"repositories"`
		Reliability        ink.Reliability  `json:"execution"`
		DaemonConfig
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
	switch err := decoder.Decode(&extra); err {
	case io.EOF:
	case nil:
		return cfg, nil, fmt.Errorf("expected one configuration object")
	default:
		return cfg, nil, err
	}
	cfg.Reliability = file.Reliability
	if err := cfg.Reliability.Validate(); err != nil {
		return cfg, nil, err
	}
	cfg.PollSeconds = file.PollSeconds
	cfg.LogLevel, cfg.LogFormat = file.LogLevel, file.LogFormat
	cfg.WorkspaceFullSlots = file.WorkspaceFullSlots
	cfg.GitHubCommand = file.GitHubCommand
	cfg.GitHubCommandSHA256 = file.GitHubCommandSHA256
	cfg.GitHubMergerCommand = file.GitHubMergerCommand
	cfg.GitHubMergerSHA256 = file.GitHubMergerSHA256
	cfg.GitHubReadArgs = file.GitHubReadArgs
	cfg.GitHubMergeArgs = file.GitHubMergeArgs
	cfg.GitHubToken = os.Getenv("INK_GITHUB_TOKEN")
	if cfg.GitHubCommand != "" && !filepath.IsAbs(cfg.GitHubCommand) {
		return cfg, nil, fmt.Errorf("github_command must be an absolute App CLI path")
	}
	if err := cfg.ValidateGitHubCommands(); err != nil {
		return cfg, nil, err
	}
	if cfg.PollSeconds < 0 || cfg.PollSeconds > 86400 {
		return cfg, nil, fmt.Errorf("poll_seconds must be between 0 and 86400")
	}
	if cfg.PollSeconds == 0 {
		cfg.PollSeconds = 60
	}
	cfg.RepositoryDefaults = file.RepositoryDefaults
	if err := cfg.RepositoryDefaults.TaskBlocked.Validate(); err != nil {
		return cfg, nil, err
	}
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
		Timeout:       25 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	providers := make(map[string]execution.Executor, len(file.Providers))
	for _, name := range slices.Sorted(maps.Keys(file.Providers)) {
		if !providerName.MatchString(name) {
			return cfg, nil, fmt.Errorf("invalid provider name")
		}
		entry := file.Providers[name]
		var token string
		if entry.TokenEnv != "" {
			if !environmentName.MatchString(entry.TokenEnv) {
				return cfg, nil, fmt.Errorf("provider %s: invalid token_env", name)
			}
			token = os.Getenv(entry.TokenEnv)
			if strings.TrimSpace(token) == "" {
				return cfg, nil, fmt.Errorf("provider %s: token_env is unset or empty", name)
			}
		}
		agent, err := httpagent.New(client, entry.URL, token, entry.Capabilities, entry.Managed, entry.RequireACK)
		if err != nil {
			return cfg, nil, fmt.Errorf("provider %s: %w", name, err)
		}
		if err := agent.CheckManaged(context.Background()); err != nil {
			return cfg, nil, fmt.Errorf("provider %s: %w", name, err)
		}
		providers[name] = agent
	}
	if route := cfg.RepositoryDefaults.ReviewerRoute; route != nil {
		if err := execution.ValidateDestination(providers, *route, true); err != nil {
			return cfg, nil, err
		}
	}
	seen := make(map[string]bool)
	for _, repo := range file.Repositories {
		if err := repo.TaskBlocked.Validate(); err != nil {
			return cfg, nil, err
		}
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
		if route := repo.ReviewerRoute; route != nil {
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
	return execution.Route{Provider: getenv("INK_PROVIDER"), Target: getenv("INK_TARGET"), Session: getenv("INK_SESSION")}
}
