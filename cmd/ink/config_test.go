//go:build linux || darwin

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wujinhjun/ink-commons/internal/execution"
	"github.com/wujinhjun/ink-commons/internal/execution/httpagent"
)

func TestNamedAdapterConfigAndRoute(t *testing.T) {
	t.Setenv("CHAT_ADAPTER_TOKEN", "test")
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{
		"providers":{"chat":{"url":"http://127.0.0.1:8788/v1/execute","token_env":"CHAT_ADAPTER_TOKEN","capabilities":["placement","prepare","deliver"]},"another":{"url":"http://127.0.0.1:9999","capabilities":["notify"]}},
		"repositories":[{"repo":"acme/project","reviewer_login":"reviewer","reviewer_route":{"provider":"chat","target":"reviewer","parent":"77"}}]
	}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, providers, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if actual := cfg.Repositories[0].ReviewerRoute; actual == nil || *actual != (execution.Route{Provider: "chat", Target: "reviewer", Parent: "77"}) {
		t.Fatalf("configuration mismatch: %+v", actual)
	}
	if _, ok := providers["chat"].(*httpagent.Executor); !ok {
		t.Fatal("named adapter was not registered")
	}

	if len(providers) != 2 || !execution.Supports(providers["another"], "notify", false) {
		t.Fatal("second named adapter is missing")
	}
	var route execution.Route
	if err := json.Unmarshal([]byte(`{"provider":"chat","target":"dev","session":"discord:19"}`), &route); err != nil {
		t.Fatal(err)
	}
	getenv := func(name string) string {
		return map[string]string{"INK_PROVIDER": "chat", "INK_TARGET": "dev", "INK_SESSION": "discord:19"}[name]
	}
	if route != routeFromEnv(getenv) || route != (execution.Route{Provider: "chat", Target: "dev", Session: "discord:19"}) {
		t.Fatalf("routing mismatch: %+v", route)
	}
}

func TestRemovedConfigFieldsAreRejected(t *testing.T) {
	for _, field := range []string{"reviewer_project", "review_channel_id", "project", "session_key", "post" + "Approval"} {
		t.Run(field, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			entry := fmt.Sprintf(`%q:"obsolete"`, field)
			if field == "project" || field == "session_key" {
				entry = `"reviewer_route":{"provider":"chat","target":"dev",` + entry + "}"
			}
			if err := os.WriteFile(path, []byte(`{"repositories":[{"repo":"acme/project","reviewer_login":"reviewer",`+entry+`}]}`), 0600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := loadConfig(path); err == nil || !strings.Contains(err.Error(), "unknown") {
				t.Fatalf("configuration accepted removed field %s: %v", field, err)
			}
		})
	}
}

func TestExecutionConfigRequiresExplicitMonitoring(t *testing.T) {
	t.Setenv("ADAPTER_TEST_TOKEN", "managed-test")
	for _, declared := range []bool{false, true} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer managed-test" {
				t.Error("expected an authenticated read-only admission request")
			}
			fmt.Fprintf(w, `{"version":1,"capabilities":["deliver","status","stop"],"managed":%v,"stop_fences_delayed_delivery":%v}`, declared, declared)
		}))
		path := filepath.Join(t.TempDir(), "config.json")
		body := fmt.Sprintf(`{"providers":{"worker":{"url":%q,"managed":true,"token_env":"ADAPTER_TEST_TOKEN","capabilities":["deliver","status","stop"]}},"execution":{"ack_seconds":5,"max_attempts":1}}`, server.URL)
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, providers, err := loadConfig(path)
		server.Close()
		if !declared {
			if err == nil {
				t.Fatal("adapter without managed guarantees was admitted")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if !providers["worker"].(*httpagent.Executor).MonitoringEnabled() || cfg.Reliability.ACKSeconds != 5 || cfg.Reliability.MaxAttempts != 1 {
			t.Fatal("execution configuration not applied")
		}
	}
	for _, extra := range []string{
		`"cc_connect_url":"http://localhost:8788",`, `"http_provider_url":"http://localhost:8788",`, `"http_provider_execution":true,`,
		`"providers":{"worker":{"url":"http://127.0.0.1:9999","managed":true,"capabilities":["deliver","status"]}},`,
		`"execution":{"ack_seconds":-1},`, `"execution":{"max_attempts":101},`,
	} {
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
