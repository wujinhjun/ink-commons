//go:build linux || darwin

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/wujinhjun/ink-commons/internal/execution"
)

func readContext(options *cliOptions) *cobra.Command {
	command := &cobra.Command{Use: "context PR_URL", Short: "Read review conversation context"}
	flags := command.Flags()
	sha := flags.String("sha", "", "current PR head SHA supplied by the reviewer task")
	before := flags.String("before", "", "review cutoff or next_before from the previous page")

	command.RunE = func(_ *cobra.Command, args []string) error {
		if len(args) != 1 || !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(*sha) {
			return errors.New("usage: ink context --sha HEAD_SHA [--before CURSOR] PR_URL")
		}
		path, err := reviewPath(args[0])
		if err != nil {
			return err
		}
		dir, err := options.directory()
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
			return daemonProblem(res.StatusCode, problem.Error)
		}
		var page execution.ConversationPage
		if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&page); err != nil {
			return fmt.Errorf("invalid context response")
		}
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(page)
	}
	return command
}
