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
	"strconv"
	"strings"
	"time"

	"ink-commons/internal/execution"
	"ink-commons/internal/ink"
)

func review(args []string) error {
	flags := flag.NewFlagSet("review", flag.ContinueOnError)
	login := flags.String("github-login", "", "GitHub account that will submit the review")
	provider := flags.String("provider", "cc-connect", "execution provider")
	target := flags.String("target", "", "execution target, e.g. a cc-connect project")
	parent := flags.String("parent", "", "parent for a new session, e.g. a Discord channel ID")
	session := flags.String("session", "", "existing execution session to reuse")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Usage: ink review --github-login LOGIN [--target PROJECT --parent CHANNEL_ID] PR_URL")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 1 || strings.TrimSpace(*login) == "" {
		return errors.New("review requires --github-login and one GitHub PR URL after the flags")
	}
	path, err := reviewPath(flags.Arg(0))
	if err != nil {
		return err
	}
	change := struct {
		Role        string           `json:"role"`
		GitHubLogin string           `json:"github_login"`
		Route       *execution.Route `json:"route,omitempty"`
	}{Role: "reviewer", GitHubLogin: strings.TrimSpace(*login)}
	if *target != "" || *parent != "" || *session != "" {
		if strings.TrimSpace(*target) == "" {
			return errors.New("--target is required for an execution route")
		}
		change.Route = &execution.Route{Provider: *provider, Target: *target, Parent: *parent, Session: *session}
	}
	payload, err := json.Marshal(change)
	if err != nil {
		return err
	}
	dir, err := directory()
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
			return fmt.Errorf("daemon HTTP %d: %s", res.StatusCode, problem.Error)
		}
		return fmt.Errorf("daemon HTTP %d", res.StatusCode)
	}
	var work ink.Work
	if err := json.NewDecoder(res.Body).Decode(&work); err != nil {
		return fmt.Errorf("read reviewer assignment: %w", err)
	}
	fmt.Fprintf(os.Stdout, "Assigned %s to %s#%d (%s).\n", work.Reviewer.GitHubLogin, work.Repo, work.Pull.Number, work.Phase)
	return nil
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
