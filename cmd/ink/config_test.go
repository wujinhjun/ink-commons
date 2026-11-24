//go:build linux || darwin

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wujinhjun/ink-commons/internal/execution"
	"github.com/wujinhjun/ink-commons/internal/execution/ccconnect"
)

func TestCCConnectConfigAndRoute(t *testing.T) {
	t.Setenv("INK_CC_CONNECT_TOKEN", "test")
	t.Setenv("INK_DISCORD_TOKEN", "test")
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{
		"cc_connect_url":"http://127.0.0.1:9111/hook",
		"repositories":[{"repo":"acme/project","reviewer_login":"reviewer","reviewer_route":{"provider":"cc-connect","target":"reviewer","parent":"77"}}]
	}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, providers, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if actual := cfg.Repositories[0].ReviewerRoute; actual == nil || *actual != (execution.Route{Provider: ccconnect.Name, Target: "reviewer", Parent: "77"}) {
		t.Fatalf("configuration mismatch: %+v", actual)
	}
	if _, ok := providers[ccconnect.Name].(*ccconnect.Executor); !ok {
		t.Fatal("cc-connect executor was not registered")
	}

	var route execution.Route
	if err := json.Unmarshal([]byte(`{"provider":"cc-connect","target":"dev","session":"discord:19"}`), &route); err != nil {
		t.Fatal(err)
	}
	getenv := func(name string) string {
		return map[string]string{"CC_PROJECT": "dev", "CC_SESSION_KEY": "discord:19"}[name]
	}
	if route != routeFromEnv(getenv) || route != (execution.Route{Provider: ccconnect.Name, Target: "dev", Session: "discord:19"}) {
		t.Fatalf("routing mismatch: %+v", route)
	}
}

func TestRemovedConfigFieldsAreRejected(t *testing.T) {
	for _, field := range []string{"reviewer_project", "review_channel_id", "project", "session_key"} {
		t.Run(field, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			entry := fmt.Sprintf(`%q:"obsolete"`, field)
			if field == "project" || field == "session_key" {
				entry = `"reviewer_route":{"provider":"cc-connect","target":"dev",` + entry + "}"
			}
			if err := os.WriteFile(path, []byte(`{"repositories":[{"repo":"acme/project","reviewer_login":"reviewer",`+entry+`}]}`), 0600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := loadConfig(path); err == nil || !strings.Contains(err.Error(), "unknown field "+fmt.Sprintf("%q", field)) {
				t.Fatalf("configuration accepted removed field %s: %v", field, err)
			}
		})
	}
}

func TestExecutionConfigRequiresExplicitMonitoring(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "config.json")
		body := fmt.Sprintf(`{"http_provider_url":"http://127.0.0.1:9999","http_provider_execution":%v,"execution":{"ack_seconds":5,"max_attempts":1},"repositories":[{"repo":"acme/project","reviewer_login":"reviewer"}]}`, enabled)
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, providers, err := loadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		capability, ok := providers["http"].(interface{ MonitoringEnabled() bool })
		if !ok || capability.MonitoringEnabled() != enabled || cfg.Reliability.ACKSeconds != 5 || cfg.Reliability.MaxAttempts != 1 {
			t.Fatal("execution configuration not applied")
		}
	}
	for _, extra := range []string{`"http_provider_execution":true,`, `"execution":{"ack_seconds":-1},`, `"execution":{"max_attempts":101},`} {
		path := filepath.Join(t.TempDir(), "invalid.json")
		if err := os.WriteFile(path, []byte(`{`+extra+`"repositories":[{"repo":"acme/project","reviewer_login":"reviewer"}]}`), 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := loadConfig(path); err == nil {
			t.Fatal("invalid execution configuration accepted", extra)
		}
	}
}

func TestAppCommandConfiguration(t *testing.T) {
	for _, command := range []string{"/usr/local/bin/github-app-cli", "github-app-cli"} {
		path := filepath.Join(t.TempDir(), "config.json")
		body := fmt.Sprintf(`{"github_command":%q,"repositories":[{"repo":"acme/project","reviewer_login":"reviewer"}]}`, command)
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, _, err := loadConfig(path)
		if filepath.IsAbs(command) {
			if err != nil || cfg.GitHubCommand != command {
				t.Fatal(cfg, err)
			}
		} else if err == nil {
			t.Fatal("relative command accepted")
		}
	}
}
