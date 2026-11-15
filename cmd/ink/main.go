package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"ink-commons/internal/ink"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "ink:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: ink serve [-config FILE] [-data DIR] | ink pre-push REMOTE_URL | ink register [flags] [OWNER/REPO] | ink review [flags] PR_URL | ink state backup|export [flags]")
	}
	switch args[0] {
	case "serve":
		return serve(args[1:])
	case "state":
		return stateCommand(args[1:])
	case "register":
		return register(args[1:])
	case "review":
		return review(args[1:])
	case "pre-push":
		if len(args) != 2 {
			return errors.New("usage: ink pre-push REMOTE_URL")
		}
		// Recording provenance must never stop a normal Git push.
		if err := push(args[1]); err != nil {
			fmt.Fprintln(os.Stderr, "ink: push association could not be recorded:", err)
		}
		return nil
	default:
		return errors.New("expected serve, register, pre-push, review, or state")
	}
}

func directory() (string, error) {
	if path := os.Getenv("INK_DATA_DIR"); path != "" {
		return filepath.Abs(path)
	}
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "ink-commons"), nil
}

func serve(args []string) error {
	dir, err := directory()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	configPath := flags.String("config", "config.json", "configuration file")
	data := flags.String("data", dir, "private state directory")
	gate := flags.String("poll-gate", "", "delay polling until this deployment gate file exists")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected serve arguments")
	}
	config, providers, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	config.PollGate = *gate
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return ink.Serve(ctx, config, *data, providers)
}

func push(remote string) error {
	route := routeFromEnv(os.Getenv)
	pushes, err := ink.ReadPush(remote, route, os.Stdin)
	if err != nil || len(pushes) == 0 {
		return err
	}
	dir, err := directory()
	if err != nil {
		return err
	}
	data, err := json.Marshal(pushes)
	if err != nil {
		return err
	}
	client := localClient(dir, 2*time.Second)
	res, err := client.Post("http://ink/v1/pushes", "application/json", bytes.NewReader(data))
	if err != nil {
		return errors.New("daemon unavailable")
	}
	defer res.Body.Close()
	if res.StatusCode != 202 {
		return fmt.Errorf("daemon HTTP %d", res.StatusCode)
	}
	return nil
}

func localClient(dir string, timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(dir, "ink.sock"))
		}},
	}
}
