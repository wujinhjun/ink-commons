//go:build linux || darwin

package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/spf13/cobra"
	"github.com/wujinhjun/ink-commons/internal/ink"
)

type doctorCheck struct {
	Name         string `json:"name"`
	Status       string `json:"status"`
	Message      string `json:"message,omitempty"`
	Revision     string `json:"revision,omitempty"`
	Gate         string `json:"gate,omitempty"`
	LastPoll     string `json:"last_poll,omitempty"`
	LastGitHubOK string `json:"last_github_ok,omitempty"`
}

func doctorCommand(options *cliOptions) *cobra.Command {
	command := &cobra.Command{Use: "doctor", Short: "Check local state, configuration and daemon health", Args: cobra.NoArgs}
	path := command.Flags().String("config", "config.json", "static configuration file to diagnose")
	command.RunE = func(cmd *cobra.Command, _ []string) error {
		summary := struct {
			OK     bool          `json:"ok"`
			Checks []doctorCheck `json:"checks"`
		}{OK: true}
		record := func(check doctorCheck, err error) {
			if err != nil {
				check.Status, check.Message = "failed", cliDiagnostic(err)
				summary.OK = false
			}
			summary.Checks = append(summary.Checks, check)
		}
		dir, err := options.directory()
		if err == nil {
			err = ink.CheckState(dir)
		}
		record(doctorCheck{Name: "state", Status: "ok"}, err)
		if _, err := os.Stat(*path); errors.Is(err, os.ErrNotExist) && !cmd.Flags().Changed("config") {
			record(doctorCheck{Name: "config", Status: "skipped", Message: "pass --config to check the daemon's static configuration"}, nil)
		} else {
			_, _, err := readConfig(*path, false)
			record(doctorCheck{Name: "config", Status: "ok"}, err)
		}
		var health struct {
			Status       string `json:"status"`
			Revision     string `json:"revision"`
			Gate         string `json:"gate"`
			LastPoll     string `json:"last_poll"`
			LastGitHubOK string `json:"last_github_ok"`
		}
		err = options.request(cmd.Context(), http.MethodGet, "/healthz", nil, &health)
		if err == nil && (health.Status != "ok" || (health.Gate != "open" && health.Gate != "closed")) {
			err = errors.New("unexpected health response")
		}
		record(doctorCheck{Name: "healthz", Status: "ok", Revision: health.Revision, Gate: health.Gate, LastPoll: health.LastPoll, LastGitHubOK: health.LastGitHubOK}, err)
		var self struct {
			Status string `json:"status"`
		}
		err = options.request(cmd.Context(), http.MethodGet, "/v1/self-check", nil, &self)
		if err == nil && self.Status != "ok" {
			err = errors.New("unexpected self-check response")
		}
		record(doctorCheck{Name: "self_check", Status: "ok"}, err)
		if options.json {
			if err := printJSON(summary); err != nil {
				return err
			}
		} else {
			for _, check := range summary.Checks {
				fmt.Printf("%s\t%s", check.Name, check.Status)
				if check.Gate != "" {
					fmt.Printf("\tgate=%s", check.Gate)
				}
				if check.Message != "" {
					fmt.Printf("\t%s", check.Message)
				}
				fmt.Println()
			}
		}
		if !summary.OK {
			return errors.New("one or more doctor checks failed")
		}
		return nil
	}
	return command
}
