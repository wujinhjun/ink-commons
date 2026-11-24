// Package adaptertest supplies integration fixtures for Ink's adapter boundary.
package adaptertest

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wujinhjun/ink-commons/internal/execution/ccconnect"
	"github.com/wujinhjun/ink-commons/internal/execution/httpagent"
)

func Discord(t testing.TB, provider string, upstream *http.Client, hook, hookToken, discordToken string) (*httpagent.Executor, error) {
	t.Helper()
	adapter, err := ccconnect.New(upstream, hook, hookToken, discordToken)
	if err != nil {
		return nil, err
	}
	server := httptest.NewServer(adapter.Handler(provider, "fixture-token"))
	t.Cleanup(server.Close)
	return httpagent.New(server.Client(), server.URL, "fixture-token",
		[]string{"prepare", "deliver", "wake", "notify", "blocked_notice", "placement", "conversation_context"}, false, true)
}
