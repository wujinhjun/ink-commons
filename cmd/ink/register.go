package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"ink-commons/internal/execution"
	"ink-commons/internal/ink"
)

func register(args []string) error {
	flags := flag.NewFlagSet("register", flag.ContinueOnError)
	remote := flags.String("remote", "origin", "Git remote to detect when OWNER/REPO is omitted")
	login := flags.String("github-login", "", "reviewer login (default: daemon repository_defaults)")
	command := flags.String("github-command", "", "reviewer GitHub CLI/App command")
	provider := flags.String("provider", "cc-connect", "review execution provider")
	target := flags.String("target", "", "review execution target")
	parent := flags.String("parent", "", "review parent channel")
	session := flags.String("session", "", "existing reviewer session")
	developer := flags.String("developer", "", "human developer login")
	hook := flags.String("post-approval", "", "absolute path of postApproval executable; receives event JSON on stdin")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() > 1 {
		return errors.New("usage: ink register [flags] [OWNER/REPO]")
	}
	name := flags.Arg(0)
	if name == "" {
		output, err := exec.Command("git", "remote", "get-url", "--", *remote).Output()
		if err != nil {
			return errors.New("cannot read Git remote; run in a checkout or specify OWNER/REPO")
		}
		name = ink.GitHubRepository(string(output))
	}
	name = strings.ToLower(name)
	if !ink.ValidRepository(name) {
		return errors.New("expected a GitHub remote or OWNER/REPO")
	}
	repo := ink.Repository{Repo: name, ReviewerLogin: *login, ReviewerCommand: *command, HumanDeveloper: *developer}
	if *target != "" || *parent != "" || *session != "" {
		if *target == "" {
			return errors.New("--target is required for an execution route")
		}
		repo.ReviewerRoute = &execution.Route{Provider: *provider, Target: *target, Parent: *parent, Session: *session}
	}
	if *hook != "" {
		repo.PostApproval = &ink.ApprovalHook{Command: []string{*hook}}
		if err := repo.PostApproval.Validate(); err != nil {
			return err
		}
	}
	payload, err := json.Marshal(repo)
	if err != nil {
		return err
	}
	dir, err := directory()
	if err != nil {
		return err
	}
	res, err := localClient(dir, 25*time.Second).Post("http://ink/v1/repositories", "application/json", bytes.NewReader(payload))
	if err != nil {
		return errors.New("daemon unavailable")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusCreated {
		var problem struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&problem)
		return fmt.Errorf("daemon HTTP %d: %s", res.StatusCode, problem.Error)
	}
	fmt.Fprintf(os.Stdout, "Registered %s; polling starts on the next daemon cycle.\n", name)
	return nil
}
