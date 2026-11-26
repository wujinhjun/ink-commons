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
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/wujinhjun/ink-commons/internal/execution"
	"github.com/wujinhjun/ink-commons/internal/ink"
)

func review(options *cliOptions) *cobra.Command {
	command := &cobra.Command{Use: "review PR_URL", Short: "Assign a reviewer"}
	flags := command.Flags()
	githubCommand := flags.String("github-command", "", "CLI for the configured GitHub reviewer identity")
	login := flags.String("github-login", "", "GitHub account that will submit the review")
	provider := flags.String("provider", "", "execution provider")
	target := flags.String("target", "", "execution target")
	parent := flags.String("parent", "", "parent channel override (default: development conversation channel)")
	session := flags.String("session", "", "existing execution session to reuse")

	command.RunE = func(_ *cobra.Command, args []string) error {
		if len(args) != 1 || strings.TrimSpace(*login) == "" {
			return errors.New("review requires --github-login and one GitHub PR URL")
		}
		path, err := reviewPath(args[0])
		if err != nil {
			return err
		}
		change := struct {
			GitHubCommand string           `json:"github_command,omitempty"`
			Role          string           `json:"role"`
			GitHubLogin   string           `json:"github_login"`
			Route         *execution.Route `json:"route,omitempty"`
		}{Role: "reviewer", GitHubLogin: strings.TrimSpace(*login), GitHubCommand: strings.TrimSpace(*githubCommand)}
		if *provider != "" || *target != "" || *parent != "" || *session != "" {
			if strings.TrimSpace(*provider) == "" || strings.TrimSpace(*target) == "" {
				return errors.New("--provider and --target are required for an execution route")
			}
			change.Route = &execution.Route{Provider: *provider, Target: *target, Parent: *parent, Session: *session}
		}
		payload, err := json.Marshal(change)
		if err != nil {
			return err
		}
		dir, err := options.directory()
		if err != nil {
			return err
		}
		res, err := localClient(dir, 10*time.Second).Post("http://ink"+path, "application/json", bytes.NewReader(payload))
		if err != nil {
			return errors.New("daemon unavailable")
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			var problem struct {
				Error string `json:"error"`
			}
			if json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&problem) == nil && problem.Error != "" {
				return daemonProblem(res.StatusCode, problem.Error)
			}
			return fmt.Errorf("daemon HTTP %d", res.StatusCode)
		}
		var work ink.Work
		if err := json.NewDecoder(res.Body).Decode(&work); err != nil {
			return fmt.Errorf("read reviewer assignment: %w", err)
		}
		if options.json {
			return printJSON(work)
		}
		fmt.Fprintf(os.Stdout, "Assigned %s to %s#%d (%s).\n", work.Reviewer.GitHubLogin, work.Repo, work.Pull.Number, work.Phase)
		return nil
	}
	return command
}

func reviewPath(value string) (string, error) {
	u, err := url.Parse(value)
	if err == nil && u.Scheme == "https" && strings.EqualFold(u.Host, "github.com") && u.User == nil {
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) == 4 && parts[2] == "pull" {
			repo := strings.ToLower(parts[0] + "/" + parts[1])
			number, err := strconv.Atoi(parts[3])
			if ink.ValidRepository(repo) && err == nil && number > 0 {
				return fmt.Sprintf("/v1/work/%s/%d/assign", repo, number), nil
			}
		}
	}
	return "", errors.New("expected a GitHub PR URL such as https://github.com/owner/repository/pull/42")
}
