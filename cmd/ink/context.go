//go:build linux || darwin

package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/wujinhjun/ink-commons/internal/execution"
)

func readContext(args []string) error {
	flags := flag.NewFlagSet("context", flag.ContinueOnError)
	sha := flags.String("sha", "", "current PR head SHA supplied by the reviewer task")
	before := flags.String("before", "", "review cutoff or next_before from the previous page")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 1 || !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(*sha) {
		return errors.New("usage: ink context --sha HEAD_SHA [--before MESSAGE_ID] PR_URL")
	}
	path, err := reviewPath(flags.Arg(0))
	if err != nil {
		return err
	}
	dir, err := directory()
	if err != nil {
		return err
	}
	query := url.Values{"sha": {*sha}}
	if *before != "" {
		query.Set("before", *before)
	}
	res, err := localClient(dir, 30*time.Second).Get("http://ink" + strings.TrimSuffix(path, "/assign") + "/context?" + query.Encode())
	if err != nil {
		return errors.New("daemon unavailable while reading context")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		var problem struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&problem)
		return fmt.Errorf("context HTTP %d: %s", res.StatusCode, problem.Error)
	}
	var page execution.ConversationPage
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&page); err != nil {
		return fmt.Errorf("invalid context response")
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(page)
}
