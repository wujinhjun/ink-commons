//go:build linux || darwin

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"time"

	"github.com/wujinhjun/ink-commons/internal/ink"
)

func exportConfiguration(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: ink config export")
	}
	dir, err := directory()
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

func configurationCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: ink config get|set|unset|list [KEY] [JSON_VALUE] --global|--repo OWNER/REPO [--effective] [--json]")
	}
	action := args[0]
	if action == "export" {
		return exportConfiguration(args[1:])
	}
	if action != "get" && action != "set" && action != "unset" && action != "list" {
		return errors.New("expected config get, set, unset, or list")
	}
	flags := flag.NewFlagSet("config "+action, flag.ContinueOnError)
	repo := flags.String("repo", "", "central repository scope (owner/repo)")
	global := flags.Bool("global", false, "central global scope")
	effective := flags.Bool("effective", false, "show inherited values and sources")
	pr := flags.Int("pr", 0, "PR number for effective assignment sources (requires --repo)")
	asJSON := flags.Bool("json", false, "print the API JSON response")
	// Accept flags before or after positional arguments, matching documented usage.
	var positional, options []string
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if arg == "--repo" || arg == "-repo" || arg == "--pr" || arg == "-pr" {
			if i+1 >= len(args) {
				return errors.New("--repo needs owner/repo")
			}
			options = append(options, arg, args[i+1])
			i++
			continue
		}
		if len(arg) > 0 && arg[0] == '-' && !json.Valid([]byte(arg)) {
			options = append(options, arg)
		} else {
			positional = append(positional, arg)
		}
	}
	if err := flags.Parse(options); err != nil {
		return err
	}
	if (*repo == "") == !*global {
		return errors.New("choose exactly one of --global or --repo OWNER/REPO")
	}
	if *repo != "" && !ink.ValidRepository(*repo) {
		return errors.New("expected owner/repo")
	}
	expected := map[string]int{"list": 0, "get": 1, "unset": 1, "set": 2}[action]
	if len(positional) != expected {
		return fmt.Errorf("config %s expects %d positional arguments", action, expected)
	}
	if *effective && action != "get" && action != "list" {
		return errors.New("--effective applies to get/list")
	}
	query := url.Values{"repo": {*repo}}
	if len(positional) > 0 {
		query.Set("key", positional[0])
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
		value := json.RawMessage(positional[1])
		if !json.Valid(value) { // Bare strings are convenient; booleans/null/numbers remain typed JSON.
			value, _ = json.Marshal(positional[1])
		}
		body, _ = json.Marshal(map[string]json.RawMessage{"value": value})
		method = http.MethodPost
	}
	if action == "unset" {
		method = http.MethodDelete
	}
	dir, err := directory()
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
		return fmt.Errorf("daemon HTTP %d: %s", response.StatusCode, problem.Error)
	}
	if *asJSON {
		_, err = os.Stdout.Write(append(bytes.TrimSpace(data), '\n'))
		return err
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
