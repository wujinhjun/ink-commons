//go:build linux || darwin

package ink

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"
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

// Workflow details are available via the local work API. Log only the stable
// alert code and PR number, as messages can contain provider-supplied data.
func (s *Service) refreshWorkflow(w *Work, now time.Time) {
	before := len(w.Alerts)
	w.refreshWorkflow(now, s.config.Reliability.defaults())
	for _, alert := range w.Alerts[before:] {
		s.log.Warn("workflow alert", "number", w.Pull.Number, "code", alert.Code)
	}
}

// net/http can include request details and panic text in its log message.
type httpErrorHandler struct {
	slog.Handler
}

func (h httpErrorHandler) Handle(ctx context.Context, r slog.Record) error {
	return h.Handler.Handle(ctx, slog.NewRecord(r.Time, r.Level, "local HTTP server error", 0))
}
