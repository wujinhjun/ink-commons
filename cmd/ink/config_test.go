package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"ink-commons/internal/execution"
	"ink-commons/internal/execution/ccconnect"
)

func TestLegacyCCConnectConfigAndRoute(t *testing.T) {
	t.Setenv("INK_CC_CONNECT_TOKEN", "test")
	t.Setenv("INK_DISCORD_TOKEN", "test")
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{
		"cc_connect_url":"http://127.0.0.1:9111/hook",
		"repositories":[{"repo":"acme/project","reviewer_login":"reviewer","reviewer_project":"reviewer","review_channel_id":"77"}]
	}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, providers, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if actual := cfg.Repositories[0].ReviewerRoute; actual == nil || *actual != (execution.Route{Provider: ccconnect.Name, Target: "reviewer", Parent: "77"}) {
		t.Fatalf("legacy configuration mismatch: %+v", actual)
	}
	if _, ok := providers[ccconnect.Name].(*ccconnect.Executor); !ok {
		t.Fatal("cc-connect executor was not registered")
	}

	var route execution.Route
	if err := json.Unmarshal([]byte(`{"project":"dev","session_key":"discord:19"}`), &route); err != nil {
		t.Fatal(err)
	}
	getenv := func(name string) string {
		return map[string]string{"CC_PROJECT": "dev", "CC_SESSION_KEY": "discord:19"}[name]
	}
	if route != routeFromEnv(getenv) || route != (execution.Route{Provider: ccconnect.Name, Target: "dev", Session: "discord:19"}) {
		t.Fatalf("legacy routing mismatch: %+v", route)
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
	for _, command := range []string{"/example/bin/github-reader", "github-reader"} {
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
