//go:build linux || darwin

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/wujinhjun/ink-commons/internal/ink"
)

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

func serve(options *cliOptions) *cobra.Command {
	command := &cobra.Command{Use: "serve", Short: "Run the daemon"}
	flags := command.Flags()
	configPath := flags.String("config", "config.json", "configuration file")
	gate := flags.String("poll-gate", "", "delay polling until this deployment gate file exists")
	configOnly := flags.Bool("config-only", false, "allow local configuration and work item setup without polling or dispatch")

	command.RunE = func(_ *cobra.Command, args []string) error {
		dir, err := options.directory()
		if err != nil {
			return err
		}

		if len(args) != 0 {
			return errors.New("unexpected serve arguments")
		}
		config, providers, err := loadConfig(*configPath)
		if err != nil {
			return fmt.Errorf("cannot load daemon configuration: %s", ink.StartupDiagnostic(err))
		}
		config.PollGate = *gate
		config.ConfigOnly = *configOnly
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := ink.Serve(ctx, config, dir, providers); err != nil {
			return fmt.Errorf("daemon could not start or continue serving: %s", ink.StartupDiagnostic(err))
		}
		if options.json {
			return printJSON(map[string]string{"status": "stopped", "revision": ink.BuildRevision})
		}
		return nil
	}
	return command
}

func (options *cliOptions) push(remote string) error {
	route := routeFromEnv(os.Getenv)
	pushes, err := ink.ReadPush(remote, route, os.Stdin)
	if err != nil || len(pushes) == 0 {
		return err
	}
	dir, err := options.directory()
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
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(dir, "ink.sock"))
		}},
	}
}
