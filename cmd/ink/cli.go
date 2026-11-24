//go:build linux || darwin

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/wujinhjun/ink-commons/internal/ink"
)

type cliOptions struct {
	data string
	json bool
}

func (options *cliOptions) directory() (string, error) {
	if options.data != "" {
		return filepath.Abs(options.data)
	}
	return directory()
}

func main() {
	options, command := cli()
	command.SetArgs(legacyFlags(os.Args[1:]))
	if err := command.Execute(); err != nil {
		message := cliDiagnostic(err)
		if options.json {
			_ = json.NewEncoder(os.Stderr).Encode(map[string]string{"error": message})
		} else {
			fmt.Fprintln(os.Stderr, "ink:", message)
		}
		os.Exit(1)
	}
}

func run(args []string) error {
	_, command := cli()
	command.SetArgs(legacyFlags(args))
	return command.Execute()
}

func cli() (*cliOptions, *cobra.Command) {
	options := &cliOptions{}
	root := &cobra.Command{
		Use: "ink", Short: "Coordinate GitHub pull requests",
		SilenceUsage: true, SilenceErrors: true,
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
	}
	root.PersistentFlags().StringVar(&options.data, "data", "", "private daemon state directory (overrides INK_DATA_DIR)")
	root.PersistentFlags().BoolVar(&options.json, "json", false, "emit machine-readable JSON")
	root.SetFlagErrorFunc(func(cmd *cobra.Command, _ error) error {
		return fmt.Errorf("invalid flags; see %s --help", cmd.CommandPath())
	})
	root.AddCommand(serve(options), initialize(options), register(options), review(options), readContext(options),
		versionCommand(options), prePushCommand(options), doctorCommand(options),
		deploymentCommand(options, "upgrade"), deploymentCommand(options, "rollback"))
	skills := initialize(options)
	skills.Use = "skills"
	root.AddCommand(skills)
	root.AddCommand(
		commandGroup("config", "Inspect or change central configuration",
			[]string{"get", "set", "unset", "list"}, options, configurationCommand,
			&cobra.Command{Use: "export", Short: "Export central policy", Args: cobra.NoArgs,
				RunE: func(_ *cobra.Command, args []string) error { return options.exportConfiguration(args) }}),
		commandGroup("task", "Manage cooperative task handoffs", []string{"claim", "status", "submit", "retry"}, options, taskCommand),
		commandGroup("hook", "Manage the Git pre-push hook", []string{"install", "status"}, options, hookCommand),
		commandGroup("workspace", "Manage Git workspaces", []string{"list", "acquire", "release", "remove"}, options, workspaceCommand),
		commandGroup("state", "Initialize or snapshot the state database", []string{"init", "backup", "export"}, options, stateCommand),
		commandGroup("notification", "Inspect or acknowledge notifications", []string{"ack", "status"}, options, notificationCommand),
		workCommands(options), repoCommands(options),
	)
	return options, root
}

func commandGroup(name, description string, actions []string, options *cliOptions, build func(*cliOptions, string) *cobra.Command, extra ...*cobra.Command) *cobra.Command {
	group := &cobra.Command{Use: name, Short: description,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return fmt.Errorf("expected a subcommand; see %s --help", cmd.CommandPath())
		}}
	for _, action := range actions {
		group.AddCommand(build(options, action))
	}
	group.AddCommand(extra...)
	return group
}

func versionCommand(options *cliOptions) *cobra.Command {
	return &cobra.Command{Use: "version", Short: "Show the build revision", Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if options.json {
				return json.NewEncoder(os.Stdout).Encode(map[string]string{"revision": ink.BuildRevision})
			}
			fmt.Println(ink.BuildRevision)
			return nil
		}}
}

func prePushCommand(options *cliOptions) *cobra.Command {
	return &cobra.Command{Use: "pre-push REMOTE_URL", Hidden: true, Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			// Recording provenance must never stop a normal Git push.
			status := "ok"
			if err := options.push(args[0]); err != nil {
				status = "association_unavailable"
				fmt.Fprintln(os.Stderr, "ink: push association could not be recorded:", cliDiagnostic(err))
			}
			if options.json {
				return json.NewEncoder(os.Stdout).Encode(map[string]string{"status": status})
			}
			return nil
		}}
}

func deploymentCommand(options *cliOptions, name string) *cobra.Command {
	command := &cobra.Command{Use: name, Short: "Manage an existing macOS system deployment", Args: cobra.NoArgs}
	command.Flags().String("tag", "", "published build tag")
	command.Flags().Bool("dry-run", false, "preview deployment changes")
	if name == "upgrade" {
		command.Flags().String("channel", "", "stable or preview")
		command.Flags().Bool("apply", false, "activate the prepared release")
	}
	command.RunE = func(_ *cobra.Command, _ []string) error {
		args := []string{name}
		command.LocalNonPersistentFlags().Visit(func(flag *pflag.Flag) {
			if flag.Name == "help" {
				return
			}
			if flag.Value.Type() == "bool" {
				if flag.Value.String() == "true" {
					args = append(args, "--"+flag.Name)
				}
				return
			}
			args = append(args, "--"+flag.Name+"="+flag.Value.String())
		})
		if err := upgradeServices(args, options.json); err != nil {
			return err
		}
		if options.json {
			return json.NewEncoder(os.Stdout).Encode(map[string]string{"status": "completed", "action": name})
		}
		return nil
	}
	return command
}

// Existing deployment scripts use the Go flag package's single-dash spellings.
func legacyFlags(args []string) []string {
	result := append([]string{}, args...)
	for i, arg := range result {
		if arg == "--" {
			break
		}
		name, _, _ := strings.Cut(arg, "=")
		switch name {
		case "-data", "-config", "-config-only", "-poll-gate", "-output", "-repo", "-pr":
			result[i] = "-" + arg
		}
	}
	return result
}

func cliDiagnostic(err error) string {
	message := ink.StartupDiagnostic(err)
	if strings.HasPrefix(message, "unknown command ") {
		return "unknown command; see ink --help"
	}
	var path *os.PathError
	if errors.As(err, &path) {
		message = strings.ReplaceAll(message, path.Path, "[path]")
	}
	if sensitiveDiagnostic.MatchString(message) {
		return "details withheld; check the local configuration and command arguments"
	}
	return message
}

// Keep daemon validation and conflict messages. Older daemon releases may
// return subprocess diagnostics with paths, authenticated URLs or credentials.
var sensitiveDiagnostic = regexp.MustCompile(`(?i)(https?://|(^|[[:space:]="'(:])/[^[:space:]]|[\r\n]|bearer[[:space:]]|(?:gh[pousr]_|github_pat_|sk-)[a-z0-9_-]+|(?:token|password|authorization|secret)[=:])`)

func daemonProblem(status int, message string) error {
	if sensitiveDiagnostic.MatchString(message) || len(message) > 512 {
		message = "details withheld; inspect the local configuration or status command"
	}
	if message == "" {
		return fmt.Errorf("daemon HTTP %d", status)
	}
	return fmt.Errorf("daemon HTTP %d: %s", status, message)
}
