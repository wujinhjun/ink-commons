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
	taskMode := flags.String("task-mode", "", "legacy or cooperative task handoff")
	remote := flags.String("remote", "origin", "Git remote to detect when OWNER/REPO is omitted")
	login := flags.String("github-login", "", "reviewer login (default: central global configuration)")
	command := flags.String("github-command", "", "reviewer GitHub CLI/App command")
	provider := flags.String("provider", "cc-connect", "review execution provider")
	target := flags.String("target", "", "review execution target")
	parent := flags.String("parent", "", "legacy parent (ignored for new cc-connect review threads; use ink review --parent per PR)")
	session := flags.String("session", "", "existing reviewer session")
	developer := flags.String("developer", "", "human developer login")
	autoMerge := flags.Bool("auto-merge-after-approval", false, "automatically merge the approved head after checks pass")
	mergeMethod := flags.String("merge-method", "merge", "merge, squash or rebase (for automatic merge)")
	hook := flags.String("post-approval", "", "absolute path of postApproval executable; receives event JSON on stdin")
	reuseThread := flags.Bool("reuse-development-thread", false, "reuse the development thread with an independent reviewer project")
	sameChannel := flags.Bool("require-same-channel", false, "require Discord reviews to stay in the development channel")
	threadHistory := flags.Bool("thread-history", false, "allow reviewer access to original development thread messages")
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
	repo := ink.Repository{TaskMode: *taskMode, Repo: name, ReviewerLogin: *login, ReviewerCommand: *command, HumanDeveloper: *developer}
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "reuse-development-thread" || f.Name == "thread-history" || f.Name == "require-same-channel" {
			repo.Review = &ink.ReviewFeatures{RequireSameChannel: *sameChannel, ReuseDevelopmentThread: *reuseThread, Context: ink.ReviewContextFeatures{ThreadHistory: *threadHistory}}
		}
		if f.Name == "auto-merge-after-approval" || f.Name == "merge-method" {
			repo.AutoMerge = &ink.AutoMergeConfig{Enabled: *autoMerge, Method: *mergeMethod}
		}
	})
	if *taskMode != "" && *taskMode != "legacy" && *taskMode != "cooperative" {
		return errors.New("--task-mode must be legacy or cooperative")
	}
	if err := repo.AutoMerge.Validate(); err != nil {
		return err
	}
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
	var explicit map[string]json.RawMessage
	encoded, _ := json.Marshal(repo)
	_ = json.Unmarshal(encoded, &explicit)
	if *login == "" {
		delete(explicit, "reviewer_login")
	}
	payload, err := json.Marshal(explicit)
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
