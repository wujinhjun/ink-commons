package main

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestConfigCLIUsesCentralSocketAndPreservesTypedValues(t *testing.T) {
	dir, err := os.MkdirTemp("/private/tmp", "ink-config-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	t.Setenv("INK_DATA_DIR", dir)
	ln, err := net.Listen("unix", filepath.Join(dir, "ink.sock"))
	if err != nil {
		t.Fatal(err)
	}
	type request struct{ method, scope, key, pr, effective, value string }
	requests := make(chan request, 10)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Value json.RawMessage `json:"value"`
		}
		if r.Method == "POST" {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
		}
		query := r.URL.Query()
		requests <- request{r.Method, query.Get("repo"), query.Get("key"), query.Get("pr"), query.Get("effective"), string(body.Value)}
		w.Header().Set("Content-Type", "application/json")
		if query.Get("effective") == "true" {
			_, _ = w.Write([]byte(`{"value":"reviewer","source":"pr","revision":3}`))
		} else {
			_, _ = w.Write([]byte(`{"scope":"","values":{},"revision":3}`))
		}
	})}
	go server.Serve(ln)
	defer server.Close()
	cases := []struct {
		args []string
		want request
	}{
		{[]string{"config", "set", "reviewer_login", "reviewer[bot]", "--global"}, request{"POST", "", "reviewer_login", "", "", `"reviewer[bot]"`}},
		{[]string{"config", "set", "postApproval", "null", "--repo", "acme/project"}, request{"POST", "acme/project", "postApproval", "", "", "null"}},
		{[]string{"config", "--global", "set"}, request{}}, // rejected before contacting daemon
		{[]string{"config", "set", "auto_merge", `{"enabled":false}`, "--repo=acme/project"}, request{"POST", "acme/project", "auto_merge", "", "", `{"enabled":false}`}},
		{[]string{"config", "get", "reviewer_login", "--repo", "acme/project", "--pr", "42", "--effective", "--json"}, request{"GET", "acme/project", "reviewer_login", "42", "true", ""}},
		{[]string{"config", "unset", "reviewer_route", "--repo", "acme/project"}, request{"DELETE", "acme/project", "reviewer_route", "", "", ""}},
	}
	for _, tc := range cases {
		err := run(tc.args)
		if tc.want.method == "" {
			if err == nil {
				t.Fatal("accepted invalid config command")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		got := <-requests
		if got != tc.want {
			t.Fatalf("request %+v want %+v", got, tc.want)
		}
	}
	for _, args := range [][]string{{"list"}, {"list", "--repo", "acme/project", "--global"}, {"get", "reviewer_login", "--global", "--pr", "42", "--effective"}, {"set", "reviewer_login", "a", "--global", "--effective"}, {"list", "--repo", "invalid"}} {
		if err := configurationCommand(args); err == nil {
			t.Errorf("accepted invalid scope/flags: %v", args)
		}
	}
}
