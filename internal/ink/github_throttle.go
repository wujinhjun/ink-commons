//go:build linux || darwin

package ink

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

type cooldown struct {
	until  time.Time
	status int
}

type githubThrottle struct {
	mu            sync.Mutex
	primary       map[githubRole]cooldown
	secondary     cooldown
	secondaryRuns int
}

func rateLimitDiagnostic(status int, secondary bool) error {
	if secondary {
		return diagnosticError(fmt.Sprintf("upstream HTTP %d (secondary rate limit)", status))
	}
	return diagnosticError(fmt.Sprintf("upstream HTTP %d (rate limit)", status))
}

func (t *githubThrottle) waiting(now time.Time, role githubRole) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if now.Before(t.secondary.until) {
		return rateLimitDiagnostic(t.secondary.status, true)
	}
	if state := t.primary[role]; now.Before(state.until) {
		return rateLimitDiagnostic(state.status, false)
	}
	return nil
}

// Releases the poll worker and lets the next poll respect GitHub's deadline.
// Requests already on the wire cannot clear a newer cooldown.
func (t *githubThrottle) response(res *http.Response, started, now time.Time, role githubRole) error {
	status := res.StatusCode
	if status != http.StatusForbidden && status != http.StatusTooManyRequests {
		if status >= 200 && status < 400 {
			t.mu.Lock()
			if !started.Before(t.secondary.until) {
				t.secondaryRuns = 0
			}
			t.mu.Unlock()
		}
		return nil
	}
	primary := res.Header.Get("X-RateLimit-Remaining") == "0"
	retry := res.Header.Get("Retry-After")
	secondary := retry != "" || status == http.StatusTooManyRequests && !primary
	if !secondary && status == http.StatusForbidden {
		// A 403 can represent ordinary permission denial. GitHub signals
		// headerless secondary limits in the error message.
		body, _ := io.ReadAll(io.LimitReader(res.Body, 8<<10))
		secondary = bytes.Contains(bytes.ToLower(body), []byte("secondary rate limit"))
	}
	if !primary && !secondary {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if primary {
		reset, err := strconv.ParseInt(res.Header.Get("X-RateLimit-Reset"), 10, 64)
		until := time.Unix(reset, 0)
		if err != nil || !until.After(now) {
			until = now.Add(time.Minute)
		}
		if until.After(t.primary[role].until) {
			t.primary[role] = cooldown{until, status}
		}
	}
	if secondary {
		if !started.Before(t.secondary.until) && t.secondaryRuns < 6 {
			t.secondaryRuns++
		}
		until := now.Add(time.Minute << max(0, t.secondaryRuns-1))
		if seconds, err := strconv.ParseInt(retry, 10, 64); err == nil && seconds >= 0 && seconds <= int64((1<<63-1)/time.Second) {
			until = now.Add(time.Duration(seconds) * time.Second)
		} else if date, err := http.ParseTime(retry); err == nil && date.After(now) {
			until = date
		}
		if until.After(t.secondary.until) {
			t.secondary = cooldown{until, status}
		}
	}
	return rateLimitDiagnostic(status, secondary)
}
