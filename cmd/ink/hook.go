//go:build linux || darwin

package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/wujinhjun/ink-commons/internal/ink"
)

const prePushHook = `#!/bin/sh
if [ "$#" -ge 2 ]; then
  "$HOME/.local/lib/ink-commons/ink" pre-push "$2"
fi
exit 0
`

func hookCommand(options *cliOptions, action string) *cobra.Command {
	command := &cobra.Command{Use: action}
	flags := command.Flags()
	checkout := flags.String("checkout", ".", "Git checkout to inspect or install into")
	repo := flags.String("repo", "", "only report status if origin pushes to OWNER/REPO")

	command.RunE = func(_ *cobra.Command, args []string) error {
		if len(args) != 0 {
			return errors.New("unexpected hook arguments")
		}
		if *repo != "" {
			name := strings.ToLower(strings.TrimSpace(*repo))
			if action != "status" || !ink.ValidRepository(name) {
				return errors.New("--repo requires hook status and a valid owner/repo")
			}
			if !checkoutPushesTo(*checkout, name) {
				return options.hookResult("unknown")
			}
		}
		output, err := exec.Command("git", "-C", *checkout, "rev-parse", "--path-format=absolute", "--git-path", "hooks/pre-push").Output()
		if err != nil {
			return errors.New("cannot resolve the checkout's pre-push hook path")
		}
		path := strings.TrimRight(string(output), "\r\n")
		if !filepath.IsAbs(path) {
			return errors.New("Git returned an invalid hook path")
		}
		status, err := hookStatus(path)
		if err != nil {
			return err
		}
		if action == "status" || status == "installed" {
			return options.hookResult(status)
		}
		if status == "unmanaged" {
			return errors.New("pre-push hook already exists; chain the Ink entry or use explicit link")
		}
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return err
		}
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0700)
		if err != nil {
			return err
		}
		if _, err := file.WriteString(prePushHook); err != nil {
			file.Close()
			os.Remove(path)
			return err
		}
		if err := file.Close(); err != nil {
			os.Remove(path)
			return err
		}
		return options.hookResult("installed")
	}
	return command
}

func (options *cliOptions) hookResult(status string) error {
	if options.json {
		return printJSON(map[string]string{"status": status})
	}
	fmt.Println(status)
	return nil
}

func checkoutPushesTo(checkout, repo string) bool {
	urls, err := exec.Command("git", "-C", checkout, "remote", "get-url", "--push", "--all", "origin").Output()
	if err != nil {
		return false
	}
	for _, remote := range strings.Split(string(urls), "\n") {
		if ink.GitHubRepository(remote) == repo {
			return true
		}
	}
	return false
}

func hookStatus(path string) (string, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "missing", nil
	}
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode()&0100 == 0 {
		return "unmanaged", nil
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if bytes.Equal(content, []byte(prePushHook)) {
		return "installed", nil
	}
	return "unmanaged", nil
}
