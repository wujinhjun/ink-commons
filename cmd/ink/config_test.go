package main

import (
	"encoding/json"
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
