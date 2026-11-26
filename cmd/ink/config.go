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
	return readConfig(path, true)
}

func readConfig(path string, admitManaged bool) (ink.Config, map[string]execution.Executor, error) {
	var file *struct {
		RepositoryDefaults json.RawMessage   `json:"repository_defaults,omitempty"`
		Repositories       []json.RawMessage `json:"repositories"`
		Reliability        ink.Reliability   `json:"execution"`
		DaemonConfig
		GitHubReader *ink.GitHubCredential `json:"github_reader,omitempty"`
		GitHubMerger *ink.GitHubCredential `json:"github_merger,omitempty"`
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
	if file == nil {
		return cfg, nil, fmt.Errorf("expected one configuration object")
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
	cfg.PollSeconds = file.PollSeconds
	cfg.LogLevel, cfg.LogFormat = file.LogLevel, file.LogFormat
	cfg.WorkspaceFullSlots = file.WorkspaceFullSlots
	cfg.GitHubCommand = file.GitHubCommand
	cfg.GitHubCommandSHA256 = file.GitHubCommandSHA256
	cfg.GitHubMergerCommand = file.GitHubMergerCommand
	cfg.GitHubMergerSHA256 = file.GitHubMergerSHA256
	cfg.GitHubReadArgs = file.GitHubReadArgs
	cfg.GitHubMergeArgs = file.GitHubMergeArgs
	cfg.GitHubReader, cfg.GitHubMerger = file.GitHubReader, file.GitHubMerger
	if len(file.RepositoryDefaults) != 0 {
		cfg.RepositoryDefaults, err = ink.ParseStaticRepository(file.RepositoryDefaults, true)
		if err != nil {
			return cfg, nil, err
		}
	}
	for _, raw := range file.Repositories {
		repo, err := ink.ParseStaticRepository(raw, false)
		if err != nil {
			return cfg, nil, err
		}
		cfg.Repositories = append(cfg.Repositories, repo)
	}
	client := &http.Client{
		Timeout:       25 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	providers := make(map[string]execution.Executor, len(file.Providers))
	adapters := make([]*httpagent.Executor, 0, len(file.Providers))
	for _, name := range slices.Sorted(maps.Keys(file.Providers)) {
		if !providerName.MatchString(name) {
			return cfg, nil, fmt.Errorf("invalid provider name")
		}
		entry := file.Providers[name]
		var token string
		if entry.TokenEnv != "" {
			if !environmentName.MatchString(entry.TokenEnv) {
				return cfg, nil, fmt.Errorf("invalid provider token_env")
			}
			token = os.Getenv(entry.TokenEnv)
			if strings.TrimSpace(token) == "" {
				return cfg, nil, fmt.Errorf("provider token_env is unset or empty")
			}
		}
		agent, err := httpagent.New(client, entry.URL, token, entry.Capabilities, entry.Managed, entry.RequireACK)
		if err != nil {
			return cfg, nil, fmt.Errorf("provider configuration: %w", err)
		}

		providers[name] = agent
		adapters = append(adapters, agent)
	}
	if err := cfg.ValidateStaticPolicy(providers); err != nil {
		return cfg, nil, err
	}
	if admitManaged {
		for _, agent := range adapters {
			if err := agent.CheckManaged(context.Background()); err != nil {
				return cfg, nil, fmt.Errorf("managed provider admission: %w", err)
			}
		}
	}
	if cfg.PollSeconds == 0 {
		cfg.PollSeconds = 60
	}
	return cfg, providers, nil
}

func routeFromEnv(getenv func(string) string) execution.Route {
	return execution.Route{Provider: getenv("INK_PROVIDER"), Target: getenv("INK_TARGET"), Session: getenv("INK_SESSION")}
}
