//go:build linux || darwin

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"time"

	"github.com/spf13/cobra"
	"github.com/wujinhjun/ink-commons/internal/ink"
)

func (options *cliOptions) exportConfiguration(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: ink config export")
	}
	dir, err := options.directory()
	if err != nil {
		return err
	}
	response, err := localClient(dir, 10*time.Second).Get("http://ink/v1/config/export")
	if err != nil {
		return errors.New("daemon unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("configuration export failed: HTTP %d", response.StatusCode)
	}
	var content bytes.Buffer
	raw, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return err
	}
	if err := json.Indent(&content, raw, "", "  "); err != nil {
		return errors.New("invalid configuration export")
	}
	_, err = content.WriteTo(os.Stdout)
	return err
}

func configurationCommand(options *cliOptions, action string) *cobra.Command {
	command := &cobra.Command{Use: action}
	flags := command.Flags()
	repo := flags.String("repo", "", "central repository scope (owner/repo)")
	global := flags.Bool("global", false, "central global scope")
	effective := flags.Bool("effective", false, "show inherited values and sources")
	pr := flags.Int("pr", 0, "PR number for effective assignment sources (requires --repo)")

	command.RunE = func(_ *cobra.Command, args []string) error {
		if (*repo == "") == !*global {
			return errors.New("choose exactly one of --global or --repo OWNER/REPO")
		}
		if *repo != "" && !ink.ValidRepository(*repo) {
			return errors.New("expected owner/repo")
		}
		expected := map[string]int{"list": 0, "get": 1, "unset": 1, "set": 2}[action]
		if len(args) != expected {
			return fmt.Errorf("config %s expects %d args arguments", action, expected)
		}
		if *effective && action != "get" && action != "list" {
			return errors.New("--effective applies to get/list")
		}
		query := url.Values{"repo": {*repo}}
		if len(args) > 0 {
			query.Set("key", args[0])
		}
		if *pr != 0 {
			if *pr < 1 || *repo == "" || !*effective {
				return errors.New("--pr requires --repo and --effective")
			}
			query.Set("pr", fmt.Sprint(*pr))
		}
		if *effective {
			query.Set("effective", "true")
		}
		method := http.MethodGet
		var body []byte
		if action == "set" {
			value := json.RawMessage(args[1])
			if !json.Valid(value) { // Bare strings are convenient; booleans/null/numbers remain typed JSON.
				value, _ = json.Marshal(args[1])
			}
			body, _ = json.Marshal(map[string]json.RawMessage{"value": value})
			method = http.MethodPost
		}
		if action == "unset" {
			method = http.MethodDelete
		}
		dir, err := options.directory()
		if err != nil {
			return err
		}
		request, err := http.NewRequest(method, "http://ink/v1/config?"+query.Encode(), bytes.NewReader(body))
		if err != nil {
			return err
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := localClient(dir, 10*time.Second).Do(request)
		if err != nil {
			return errors.New("daemon unavailable")
		}
		defer response.Body.Close()
		data, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
		if err != nil {
			return err
		}
		if response.StatusCode != 200 {
			var problem struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal(data, &problem)
			return daemonProblem(response.StatusCode, problem.Error)
		}
		if options.json {
			if !json.Valid(data) {
				return errors.New("invalid JSON configuration response")
			}
			return printJSON(json.RawMessage(data))
		}
		if action == "set" || action == "unset" {
			fmt.Fprintf(os.Stdout, "Updated %s configuration.\n", map[bool]string{true: "global", false: *repo}[*global])
			return nil
		}
		if action == "get" {
			var result struct {
				Value   json.RawMessage `json:"value"`
				Source  string          `json:"source"`
				Present *bool           `json:"present"`
			}
			if err := json.Unmarshal(data, &result); err != nil {
				return err
			}
			if result.Present != nil && !*result.Present {
				fmt.Fprintln(os.Stdout, "<inherited>")
				return nil
			}
			if result.Source != "" {
				fmt.Fprintf(os.Stdout, "%s\t%s\n", result.Value, result.Source)
			} else {
				fmt.Fprintln(os.Stdout, string(result.Value))
			}
			return nil
		}
		values := ink.ConfigValues{}
		sources := map[string]string{}
		if *effective {
			var result ink.EffectiveConfig
			if err := json.Unmarshal(data, &result); err != nil {
				return err
			}
			values, sources = result.Values, result.Sources
		} else {
			var result ink.ConfigRecord
			if err := json.Unmarshal(data, &result); err != nil {
				return err
			}
			values = result.Values
		}
		keys := make([]string, 0, len(values))
		for key := range values {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			fmt.Fprintf(os.Stdout, "%s\t%s", key, values[key])
			if source := sources[key]; source != "" {
				fmt.Fprintf(os.Stdout, "\t%s", source)
			}
			fmt.Fprintln(os.Stdout)
		}
		return nil
	}
	return command
}
