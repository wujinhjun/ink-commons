//go:build linux || darwin

package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/wujinhjun/ink-commons/internal/ink"
)

func stateCommand(args []string) error {
	if len(args) == 0 || (args[0] != "init" && args[0] != "backup" && args[0] != "export") {
		return errors.New("usage: ink state init [-data DIR] | ink state backup|export -output FILE [-data DIR] (stop daemon first)")
	}
	dir, err := directory()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("state "+args[0], flag.ContinueOnError)
	data := flags.String("data", dir, "daemon state directory")
	var output *string
	if args[0] != "init" {
		output = flags.String("output", "", "new destination file (must not exist)")
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if args[0] == "init" {
		if flags.NArg() != 0 {
			return errors.New("unexpected state init arguments")
		}
		if err := ink.InitializeState(*data); err != nil {
			return err
		}
		fmt.Fprintf(os.Stdout, "State initialized in %s.\n", *data)
		return nil
	}
	if flags.NArg() != 0 || *output == "" {
		return errors.New("specify -output FILE with no positional arguments")
	}
	if err := ink.StateSnapshot(*data, *output, args[0] == "export"); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "State %s written to %s.\n", args[0], *output)
	return nil
}
