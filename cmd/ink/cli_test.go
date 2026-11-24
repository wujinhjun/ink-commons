//go:build linux || darwin

package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestCLIParseErrorsHonorJSONFlag(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		json bool
	}{
		{"unknown command", []string{"bogus", "--json"}, true},
		{"unknown flag", []string{"repo", "list", "--nonesuch", "--json"}, true},
		{"explicit true", []string{"repo", "list", "--nonesuch", "--json=true"}, true},
		{"explicit false", []string{"repo", "list", "--json", "--nonesuch", "--json=false"}, false},
		{"literal after delimiter", []string{"repo", "list", "--", "--json"}, false},
		{"flag argument", []string{"repo", "update", "--values", "--json", "--nonesuch"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			if status := executeCLI(tc.args, &stderr); status != 1 {
				t.Fatalf("exit status %d, stderr %s", status, &stderr)
			}
			if !tc.json {
				if !strings.HasPrefix(stderr.String(), "ink: ") {
					t.Fatalf("expected text diagnostics: %s", &stderr)
				}
				return
			}
			var problem map[string]string
			if err := json.Unmarshal(stderr.Bytes(), &problem); err != nil || problem["error"] == "" || len(problem) != 1 {
				t.Fatalf("expected a JSON error object: %s", &stderr)
			}
		})
	}
}
