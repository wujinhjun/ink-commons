//go:build linux || darwin

package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

func upgradeServices(args []string, quiet bool) error {
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	binary, err = filepath.EvalSymlinks(binary)
	if err != nil {
		return err
	}
	release := filepath.Dir(binary)
	var settings struct {
		Python string `json:"python"`
	}
	content, err := os.ReadFile(filepath.Join(release, "deployment.json"))
	if err != nil {
		return errors.New("ink upgrade requires an existing system deployment")
	}
	if err := json.Unmarshal(content, &settings); err != nil || !filepath.IsAbs(settings.Python) {
		return errors.New("installed deployment has no valid Python path")
	}
	command := exec.Command(settings.Python, append([]string{filepath.Join(release, "manage.py")}, args...)...)
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if quiet {
		command.Stdout, command.Stderr = io.Discard, io.Discard
	}
	return command.Run()
}
