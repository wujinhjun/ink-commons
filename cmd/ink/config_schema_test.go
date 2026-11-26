//go:build linux || darwin

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestExampleConfigMatchesSchemaAndLoads(t *testing.T) {
	path := filepath.Join("..", "..", "config.example.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	schema, err := jsonschema.NewCompiler().Compile(filepath.Join("..", "..", "config.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	example, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(example); err != nil {
		t.Fatal(err)
	}
	config, providers, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) < 2 || len(config.Repositories) == 0 || config.GitHubReader == nil || config.GitHubMerger == nil {
		t.Fatal("example must exercise named adapters, role credentials and policy")
	}
}
