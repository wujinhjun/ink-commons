package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAutoMergeConfigDefaultsAndValidation(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	for _, method := range []string{"merge", "squash", "rebase"} {
		body := `{"repository_defaults":{"auto_merge":{"enabled":true,"method":"` + method + `"}}}`
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, _, err := loadConfig(p)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.RepositoryDefaults.AutoMerge == nil || !cfg.RepositoryDefaults.AutoMerge.Enabled || cfg.RepositoryDefaults.AutoMerge.Method != method {
			t.Fatal("lost auto merge config")
		}
	}
	if err := register([]string{"--auto-merge-after-approval", "--merge-method", "invalid", "acme/project"}); err == nil {
		t.Fatal("invalid method accepted")
	}
}
