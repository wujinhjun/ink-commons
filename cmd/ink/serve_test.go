//go:build linux || darwin

package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/wujinhjun/ink-commons/internal/ink"
)

func TestServeRetainsActionableStartupDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name, config, state, want string
	}{
		{"missing config", "", "", "no such file or directory"},
		{"malformed JSON", `{"github_command": !}`, "", "invalid JSON syntax at byte"},
		{"bad type", `{"poll_seconds": "private-route-target"}`, "", "expected int"},
		{"unknown field", `{"private-route-target": "Bearer ghp_private-token"}`, "", "unknown JSON configuration field"},
		{"missing database", "{}", "", "ink state init"},
		{"old schema", "{}", "old", "schema version 5 (expected 6)"},
		{"malformed database row", "{}", "malformed row", "cannot read runtime configuration rows"},
		{"bad log level", `{"log_level": "ghp_private-token"}`, "ready", "invalid log_level"},
		{"bad log format", `{"log_format": "private-route-target"}`, "ready", "log_format must be text or json"},
		{"lock held", "{}", "locked", "lock daemon state (stop ink serve first)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := filepath.Join(t.TempDir(), "private-config.json")
			if tc.config != "" {
				if err := os.WriteFile(config, []byte(tc.config), 0600); err != nil {
					t.Fatal(err)
				}
			}
			dir := filepath.Join(t.TempDir(), "private-state")
			if tc.state == "" {
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
			} else if err := ink.InitializeState(dir); err != nil {
				t.Fatal(err)
			}
			switch tc.state {
			case "old", "malformed row":
				db, err := sql.Open("sqlite", filepath.Join(dir, "state.sqlite3"))
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				if tc.state == "old" {
					_, err = db.Exec("PRAGMA user_version = 5")
				} else {
					_, err = db.Exec("INSERT INTO runtime_config (scope, payload, revision) VALUES ('', '{}', ?)", "Bearer ghp_private-token private-route-target")
				}
				if err != nil {
					t.Fatal(err)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			case "locked":
				lock, err := os.OpenFile(filepath.Join(dir, "state.json.lock"), os.O_CREATE|os.O_RDWR, 0600)
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
				if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
					t.Fatal(err)
				}
			}
			err := serve([]string{"-config", config, "-data", dir})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("serve error = %v, want %q", err, tc.want)
			}
			for _, value := range []string{config, dir, "private-route-target", "ghp_private-token"} {
				if strings.Contains(err.Error(), value) {
					t.Fatalf("serve error leaked %q", value)
				}
			}
		})
	}
}
