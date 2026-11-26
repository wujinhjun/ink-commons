//go:build linux || darwin

package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/wujinhjun/ink-commons/internal/ink"
)

func stateCommand(options *cliOptions, action string) *cobra.Command {
	command := &cobra.Command{Use: action}
	flags := command.Flags()
	var output *string
	if action != "init" {
		output = flags.String("output", "", "new destination file (must not exist)")
	}

	command.RunE = func(_ *cobra.Command, args []string) error {
		dir, err := options.directory()
		if err != nil {
			return err
		}
		if action == "init" {
			if len(args) != 0 {
				return errors.New("unexpected state init arguments")
			}
			if err := ink.InitializeState(dir); err != nil {
				return err
			}
			if options.json {
				return printJSON(map[string]string{"status": "initialized"})
			}
			fmt.Fprintf(os.Stdout, "State initialized in %s.\n", dir)
			return nil
		}
		if len(args) != 0 || *output == "" {
			return errors.New("specify --output FILE with no positional arguments")
		}
		if err := ink.StateSnapshot(dir, *output, action == "export"); err != nil {
			return err
		}
		if options.json {
			return printJSON(map[string]string{"status": "written", "action": action})
		}
		fmt.Fprintf(os.Stdout, "State %s written to %s.\n", action, *output)
		return nil
	}
	return command
}
