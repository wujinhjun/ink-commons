package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"ink-commons/internal/ink"
)

func TestStateCommands(t *testing.T) {
	dir := t.TempDir()
	legacy := []byte(`{"work":{"acme/project#7":{"repo":"acme/project","pull_request":{"number":7,"state":"open"},"linked":true,"developer":{"github_login":"dev"},"reviewer":{"github_login":"reviewer"},"phase":"review_pending","task":{"role":"reviewer","sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","delivery":"unknown","attempts":2}}}}`)
	if err := os.WriteFile(filepath.Join(dir, "state.json"), legacy, 0600); err != nil {
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
	if task := state.Work["acme/project#7"].Task; task == nil || task.Delivery != "unknown" || task.Attempts != 2 {
		t.Fatalf("export lost delivery state: %+v", task)
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
