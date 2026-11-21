package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const prePushHook = `#!/bin/sh
if [ "$#" -ge 2 ]; then
  "$HOME/.local/lib/ink-commons/ink" pre-push "$2"
fi
exit 0
`

func hookCommand(args []string) error {
	if len(args) == 0 || (args[0] != "install" && args[0] != "status") {
		return errors.New("usage: ink hook install|status [--checkout DIR]")
	}
	flags := flag.NewFlagSet("hook "+args[0], flag.ContinueOnError)
	checkout := flags.String("checkout", ".", "Git checkout to inspect or install into")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected hook arguments")
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
	if args[0] == "status" || status == "installed" {
		fmt.Println(status)
		return nil
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
	fmt.Println("installed")
	return nil
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
