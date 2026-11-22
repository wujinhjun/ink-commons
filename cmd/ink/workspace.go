package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"ink-commons/internal/workspace"
)

func workspaceCommand(args []string) error {
	if len(args) == 0 || args[0] != "list" {
		return errors.New("usage: ink workspace list [--repo DIRECTORY] [--json]")
	}
	flags := flag.NewFlagSet("workspace list", flag.ContinueOnError)
	repo := flags.String("repo", ".", "local clone or worktree")
	asJSON := flags.Bool("json", false, "emit JSON")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected workspace arguments")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	inventory, err := workspace.Inspect(ctx, *repo)
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(inventory)
	}
	for _, entry := range inventory.Worktree {
		fmt.Printf("%s\t%s\t%s", entry.Path, entry.Branch, entry.Head)
		switch {
		case entry.Prunable:
			fmt.Print("\tprunable")
		case entry.Error != "":
			fmt.Printf("\tunknown (%s)", entry.Error)
		case entry.Dirty:
			fmt.Print("\tdirty")
		}
		if entry.Locked {
			fmt.Print("\tlocked")
		}
		fmt.Println()
	}
	return nil
}
