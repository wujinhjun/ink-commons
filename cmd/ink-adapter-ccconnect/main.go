//go:build linux || darwin

package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/wujinhjun/ink-commons/internal/execution/ccconnect"
)

func run() error {
	addr := flag.String("listen", "127.0.0.1:8788", "loopback listen address")
	provider := flag.String("provider", "", "persistent Ink provider name")
	flag.Parse()
	host, _, err := net.SplitHostPort(*addr)
	if err != nil || !net.ParseIP(host).IsLoopback() {
		return fmt.Errorf("listen address must be a loopback IP and port")
	}
	endpoint := os.Getenv("INK_CC_CONNECT_URL")
	token := os.Getenv("INK_ADAPTER_TOKEN")
	if strings.TrimSpace(*provider) == "" || endpoint == "" || token == "" {
		return fmt.Errorf("configure --provider, INK_CC_CONNECT_URL and INK_ADAPTER_TOKEN")
	}
	client := &http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	adapter, err := ccconnect.New(client, endpoint, os.Getenv("INK_CC_CONNECT_TOKEN"), os.Getenv("INK_DISCORD_TOKEN"))
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("/v1/execute", adapter.Handler(*provider, token))
	server := http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second}
	slog.Info("cc-connect adapter listening")
	if err := server.ListenAndServe(); err != nil {
		return fmt.Errorf("adapter HTTP listener failed")
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		slog.Error("adapter startup failed", "error", err)
		os.Exit(1)
	}
}
