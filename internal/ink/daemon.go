//go:build linux || darwin

package ink

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/wujinhjun/ink-commons/internal/execution"
)

func Serve(ctx context.Context, cfg Config, dir string, providers map[string]execution.Executor) error {
	if err := checkStateDir(dir); err != nil {
		return err
	}
	statePath := filepath.Join(dir, stateDatabase)
	lock, err := lockState(dir)
	if err != nil {
		return err
	}
	defer lock.Close()
	service, err := NewService(cfg, statePath, providers)
	if err != nil {
		return err
	}
	defer service.Close()

	socket := filepath.Join(dir, "ink.sock")
	if info, err := os.Lstat(socket); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return errors.New("ink.sock exists and is not a socket")
		}
		if err := os.Remove(socket); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	defer ln.Close()
	if err := os.Chmod(socket, 0600); err != nil {
		return err
	}

	server := &http.Server{
		Handler:           service.Handler(),
		ErrorLog:          slog.NewLogLogger(httpErrorHandler{service.log.Handler()}, slog.LevelError),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	pollCtx, cancel := context.WithCancel(ctx)
	var done = make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Duration(cfg.PollSeconds) * time.Second)
		defer ticker.Stop()
		for {
			// 部署验证期间 API 可用，但尚不触发外部派发。gate 只由可信本地操作者控制。
			if service.pollReady() {
				service.Poll(pollCtx)
			}
			select {
			case <-pollCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	defer func() { cancel(); <-done }()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-pollCtx.Done()
		shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
		}
	}()
	service.log.Info("Ink Commons listening on Unix socket")
	err = server.Serve(ln)
	cancel()
	<-stopped
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
