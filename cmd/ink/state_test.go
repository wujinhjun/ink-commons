//go:build linux || darwin

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/wujinhjun/ink-commons/internal/ink"
)

func TestStateCommands(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := run([]string{"state", "init", "-data", dir}); err != nil {
		t.Fatal(err)
	}
	service, err := ink.NewService(ink.Config{}, filepath.Join(dir, "state.sqlite3"), nil)
	if err != nil {
		t.Fatal(err)
	}
	service.Close()
	exported := filepath.Join(dir, "rollback.json")
	backup := filepath.Join(dir, "backup.sqlite3")
	for _, args := range [][]string{
		{"state", "backup", "-data", dir, "-output", backup},
		{"state", "export", "-data", dir, "-output", exported},
	} {
		if err := run(args); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(exported)
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Work map[string]ink.Work `json:"work"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	if state.Work == nil || len(state.Work) != 0 {
		t.Fatalf("a new database must export an empty work index: %+v", state.Work)
	}
	restored, err := ink.NewService(ink.Config{}, backup, nil)
	if err != nil {
		t.Fatal(err)
	}
	restored.Close()
	if err := run([]string{"state", "export", "-data", dir, "-output", exported}); err == nil {
		t.Fatal("command overwrote export")
	}
}
