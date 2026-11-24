//go:build linux || darwin

package ink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/wujinhjun/ink-commons/internal/execution"
	"modernc.org/sqlite"
)

func newLogger(out io.Writer, level, format string) (*slog.Logger, error) {
	opts := &slog.HandlerOptions{}
	if level != "" {
		var parsed slog.Level
		if err := parsed.UnmarshalText([]byte(level)); err != nil {
			return nil, fmt.Errorf("invalid log_level")
		}
		opts.Level = parsed
	}
	switch format {
	case "", "text":
		return slog.New(slog.NewTextHandler(out, opts)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(out, opts)), nil
	default:
		return nil, fmt.Errorf("log_format must be text or json")
	}
}

// diagnosticError contains only a constant message or classified metadata.
type diagnosticError string

func (e diagnosticError) Error() string { return string(e) }

// errorDiagnostic projects errors onto safe metadata. In particular, SQLite
// messages, URLs, command output, and arbitrary provider errors can contain
// credentials, private routes, configuration, or local paths.
func errorDiagnostic(err error) (string, bool) {
	switch e := err.(type) {
	case diagnosticError:
		return string(e), true
	case execution.AdapterTransportError:
		return e.Error(), true
	case execution.HTTPStatusError:
		return fmt.Sprintf("provider HTTP %d", int(e)), true
	case *sqlite.Error:
		return fmt.Sprintf("SQLite code %d (%s)", e.Code(), sqlite.ErrorCodeString[e.Code()&0xff]), true
	case *json.SyntaxError:
		return fmt.Sprintf("invalid JSON syntax at byte %d", e.Offset), true
	case *json.UnmarshalTypeError:
		return fmt.Sprintf("wrong JSON value type at byte %d (expected %s)", e.Offset, e.Type), true
	case *os.PathError:
		return "filesystem operation: " + redactedError(e.Err), true
	case *os.SyscallError:
		return "system call: " + redactedError(e.Err), true
	case *net.OpError:
		return "network operation: " + redactedError(e.Err), true
	case *url.Error:
		if e.Timeout() {
			return "network request timed out", true
		}
		return redactedError(e.Err), true
	case *exec.ExitError:
		return fmt.Sprintf("process exited (status %d)", e.ExitCode()), true
	case syscall.Errno:
		return e.Error(), true
	}
	switch err {
	case context.DeadlineExceeded:
		return "operation timed out", true
	case context.Canceled:
		return "operation cancelled", true
	case io.EOF, io.ErrUnexpectedEOF:
		return "unexpected end of input", true
	case errAppCommandNotStarted:
		return errAppCommandNotStarted.Error(), true
	}
	return "", false
}

func redactedError(err error) string {
	if diagnostic, ok := errorDiagnostic(err); ok {
		return diagnostic
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var diagnostics []string
		for _, cause := range joined.Unwrap() {
			diagnostics = append(diagnostics, redactedError(cause))
		}
		return strings.Join(diagnostics, "; ")
	}
	if cause := errors.Unwrap(err); cause != nil {
		return redactedError(cause)
	}
	return "details withheld"
}

// StartupDiagnostic preserves app-owned validation and stage messages. The
// startup path supplies only local validation errors and typed I/O causes.
func StartupDiagnostic(err error) string {
	if diagnostic, ok := errorDiagnostic(err); ok {
		return diagnostic
	}
	if cause := errors.Unwrap(err); cause != nil {
		return strings.Replace(err.Error(), cause.Error(), StartupDiagnostic(cause), 1)
	}
	if strings.HasPrefix(err.Error(), "json: unknown field ") {
		return "unknown JSON configuration field"
	}
	return err.Error()
}

func (s *Service) warnError(message string, err error, attrs ...any) {
	s.log.Warn(message, append(attrs, "err", redactedError(err))...)
}

// Workflow details are available via the local work API. Log only the stable
// alert code and PR number, as messages can contain provider-supplied data.
func (s *Service) refreshWorkflow(w *Work, now time.Time) {
	before := len(w.Alerts)
	w.refreshWorkflow(now, s.config.Reliability.defaults())
	for _, alert := range w.Alerts[before:] {
		s.log.Warn("workflow alert", "repo", w.Repo, "number", w.Pull.Number, "code", alert.Code)
	}
}

// net/http can include request details and panic text in its log message.
type httpErrorHandler struct {
	slog.Handler
}

func (h httpErrorHandler) Handle(ctx context.Context, r slog.Record) error {
	return h.Handler.Handle(ctx, slog.NewRecord(r.Time, r.Level, "local HTTP server error", 0))
}
