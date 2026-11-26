//go:build linux || darwin

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/wujinhjun/ink-commons/internal/execution"
	"github.com/wujinhjun/ink-commons/internal/ink"
)

func workCommands(options *cliOptions) *cobra.Command {
	return commandGroup("work", "Inspect and manage pull-request work",
		[]string{"list", "show", "link", "assign", "retry", "confirm-stopped"}, options, workCommand)
}

func workCommand(options *cliOptions, action string) *cobra.Command {
	command := &cobra.Command{Use: action + " PR_URL|OWNER/REPO#NUMBER", Args: cobra.ExactArgs(1)}
	flags := command.Flags()
	var input struct {
		ink.Assignee
		Role      string `json:"role,omitempty"`
		TaskID    string `json:"task_id,omitempty"`
		AttemptID string `json:"attempt_id,omitempty"`
		Stopped   bool   `json:"stopped,omitempty"`
		Reason    string `json:"reason,omitempty"`
	}
	var route execution.Route
	switch action {
	case "list":
		command.Use, command.Args = "list", cobra.NoArgs
	case "link", "assign":
		flags.StringVar(&input.GitHubLogin, "github-login", "", "assignee GitHub login")
		flags.StringVar(&input.GitHubCommand, "github-command", "", "assignee GitHub CLI or App command")
		flags.StringVar(&route.Provider, "provider", "", "execution provider")
		flags.StringVar(&route.Target, "target", "", "execution target")
		flags.StringVar(&route.Parent, "parent", "", "parent for new reviewer sessions")
		flags.StringVar(&route.Session, "session", "", "existing execution session")
		if action == "assign" {
			flags.StringVar(&input.Role, "role", "", "reviewer or developer")
			_ = command.MarkFlagRequired("role")
		}
	case "confirm-stopped":
		flags.StringVar(&input.TaskID, "task", "", "task ID")
		flags.StringVar(&input.AttemptID, "attempt", "", "execution attempt ID")
		flags.StringVar(&input.Reason, "reason", "", "evidence that the attempt stopped")
		flags.BoolVar(&input.Stopped, "stopped", false, "confirm the attempt has stopped")
		for _, flag := range []string{"task", "attempt", "reason", "stopped"} {
			_ = command.MarkFlagRequired(flag)
		}
	}
	command.RunE = func(cmd *cobra.Command, args []string) error {
		if action == "list" {
			var works []ink.Work
			if err := options.request(cmd.Context(), http.MethodGet, "/v1/work", nil, &works); err != nil {
				return err
			}
			if options.json {
				return printJSON(works)
			}
			for _, work := range works {
				printWork(work)
			}
			return nil
		}
		path, err := workPath(args[0])
		if err != nil {
			return err
		}
		method := http.MethodGet
		var payload any
		if action != "show" {
			method, path = http.MethodPost, path+"/"+action
			if action != "retry" {
				if route != (execution.Route{}) {
					if strings.TrimSpace(route.Provider) == "" || strings.TrimSpace(route.Target) == "" {
						return errors.New("--provider and --target are required for an execution route")
					}
					input.Route = &route
				}
				payload = input
			}
		}
		var work ink.Work
		if err := options.request(cmd.Context(), method, path, payload, &work); err != nil {
			return err
		}
		if options.json {
			return printJSON(work)
		}
		printWork(work)
		return nil
	}
	return command
}

func workPath(value string) (string, error) {
	if repo, raw, ok := strings.Cut(value, "#"); ok {
		number, err := strconv.Atoi(raw)
		if err == nil && number > 0 && ink.ValidRepository(repo) {
			return fmt.Sprintf("/v1/work/%s/%d", strings.ToLower(repo), number), nil
		}
	}
	path, err := reviewPath(value)
	if err != nil {
		return "", errors.New("expected PR_URL or OWNER/REPO#NUMBER")
	}
	return strings.TrimSuffix(path, "/assign"), nil
}

func printWork(work ink.Work) {
	fmt.Printf("%s#%d\t%s\t%s\n", work.Repo, work.Pull.Number, work.Phase, work.Workflow.State)
}

func repoCommands(options *cliOptions) *cobra.Command {
	add := register(options)
	add.Use, add.Short = "add [OWNER/REPO]", "Register a repository"
	return commandGroup("repo", "Manage registered repositories", []string{"list", "update", "remove"}, options, repoCommand, add)
}

func repoCommand(options *cliOptions, action string) *cobra.Command {
	command := &cobra.Command{Use: action + " OWNER/REPO", Args: cobra.ExactArgs(1)}
	var values string
	if action == "list" {
		command.Use, command.Args = "list", cobra.NoArgs
	}
	if action == "update" {
		command.Flags().StringVar(&values, "values", "", "JSON object of runtime policy overrides")
		_ = command.MarkFlagRequired("values")
	}
	command.RunE = func(cmd *cobra.Command, args []string) error {
		const base = "/v1/repositories"
		if action == "list" {
			var repos []ink.Repository
			if err := options.request(cmd.Context(), http.MethodGet, base, nil, &repos); err != nil {
				return err
			}
			if options.json {
				return printJSON(repos)
			}
			for _, repo := range repos {
				fmt.Printf("%s\t%s\n", repo.Repo, repo.ReviewerLogin)
			}
			return nil
		}
		name := strings.ToLower(args[0])
		if !ink.ValidRepository(name) {
			return errors.New("expected owner/repo")
		}
		path := base + "/" + name
		if action == "remove" {
			var receipt struct {
				Repo   string `json:"repo"`
				Status string `json:"status"`
			}
			if err := options.request(cmd.Context(), http.MethodDelete, path, nil, &receipt); err != nil {
				return err
			}
			if options.json {
				return printJSON(receipt)
			}
			fmt.Printf("Removed %s.\n", name)
			return nil
		}
		var patch ink.ConfigValues
		if err := json.Unmarshal([]byte(values), &patch); err != nil || len(patch) == 0 {
			return errors.New("--values requires a nonempty JSON object")
		}
		var repo ink.Repository
		if err := options.request(cmd.Context(), http.MethodPatch, path, patch, &repo); err != nil {
			return err
		}
		if options.json {
			return printJSON(repo)
		}
		fmt.Printf("Updated %s.\n", name)
		return nil
	}
	return command
}

func (options *cliOptions) request(ctx context.Context, method, path string, payload, result any) error {
	dir, err := options.directory()
	if err != nil {
		return err
	}
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://ink"+path, body)
	if err != nil {
		return errors.New("invalid daemon request")
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := localClient(dir, 25*time.Second).Do(request)
	if err != nil {
		return errors.New("daemon unavailable; check the state directory and whether ink serve is running")
	}
	defer response.Body.Close()
	const maxResponse = 16 << 20
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponse+1))
	if err != nil {
		return errors.New("cannot read daemon response")
	}
	if len(data) > maxResponse {
		return errors.New("daemon response exceeds size limit")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var problem struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &problem)
		return daemonProblem(response.StatusCode, problem.Error)
	}
	if err := json.Unmarshal(data, result); err != nil {
		return errors.New("invalid JSON daemon response")
	}
	return nil
}

func printJSON(value any) error {
	return json.NewEncoder(os.Stdout).Encode(value)
}
