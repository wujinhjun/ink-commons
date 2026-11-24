//go:build linux || darwin

package ink

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestObservationETagReusesEachPageAndItsLink(t *testing.T) {
	t.Setenv("INK_TEST_READER_PAT", "first-reader-pat")
	up, err := configuredUpstream(Config{GitHubReader: &GitHubCredential{TokenEnv: "INK_TEST_READER_PAT"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	path := "/repos/acme/project/pulls?state=open"
	var conditional []string
	up.http.Transport = testTransport(func(req *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		page := req.URL.Query().Get("page")
		tag := fmt.Sprintf(`"page-%s"`, page)
		if validator := req.Header.Get("If-None-Match"); validator != "" {
			conditional = append(conditional, validator)
			if validator != tag || req.Header.Get("Authorization") != "Bearer first-reader-pat" {
				t.Fatal("an ETag crossed credential or page boundaries")
			}
			w.WriteHeader(http.StatusNotModified)
			return w.Result(), nil
		}
		w.Header().Set("ETag", tag)
		switch page {
		case "1":
			w.Header().Add("Link", `<https://api.github.com/prev>; rel="prev"`)
			w.Header().Add("Link", `<https://api.github.com/next>; rel="next"`)
			reply(w, 200, []int{1})
		case "2":
			reply(w, 200, []int{2})
		default:
			t.Fatalf("unexpected page %q", page)
		}
		return w.Result(), nil
	})

	for range 2 {
		pages, err := listPages[int](ctx, up.observer(), path)
		if err != nil || !reflect.DeepEqual(pages, []int{1, 2}) {
			t.Fatalf("page body or Link was lost: %v (%v)", pages, err)
		}
	}
	if !reflect.DeepEqual(conditional, []string{`"page-1"`, `"page-2"`}) {
		t.Fatalf("unexpected conditional requests: %v", conditional)
	}
	conditional = nil
	t.Setenv("INK_TEST_READER_PAT", "rotated-reader-pat")
	if pages, err := listPages[int](ctx, up.observer(), path); err != nil || len(pages) != 2 || len(conditional) != 0 {
		t.Fatalf("rotated credential reused cached response: %v (%v)", pages, err)
	}
}

func TestObservationETagCannotHideApprovalRevocationFromPreflight(t *testing.T) {
	f := newAutoMergeFixture(t)
	ctx := context.Background()
	t.Setenv("INK_TEST_READER_PAT", "fixture-reader-pat")
	f.s.up.credential = &GitHubCredential{TokenEnv: "INK_TEST_READER_PAT"}
	base := f.s.up.http.Transport
	observing := true
	var conditional int
	f.s.up.http.Transport = testTransport(func(req *http.Request) (*http.Response, error) {
		path := req.URL.Path
		if req.Method != http.MethodGet || !(strings.HasSuffix(path, "/reviews") || strings.HasSuffix(path, "/pulls/42")) {
			return base.RoundTrip(req)
		}
		w := httptest.NewRecorder()
		if observing {
			etag := `"fresh"`
			if req.Header.Get("If-None-Match") == etag {
				conditional++
				w.WriteHeader(http.StatusNotModified)
				return w.Result(), nil
			}
			w.Header().Set("ETag", etag)
		} else if req.Header.Get("If-None-Match") != "" || req.Header.Get("Cache-Control") != "no-cache" {
			t.Fatal("merge preflight sent a conditional request")
		}
		switch {
		case strings.HasSuffix(path, "/pulls/42"):
			reply(w, 200, f.pull)
		case observing && req.URL.Query().Get("page") == "1":
			w.Header().Set("Link", `<https://api.github.com/next>; rel="next"`)
			reply(w, 200, []Review{})
		case observing:
			reply(w, 200, f.reviews)
		default:
			reply(w, 200, []Review{})
		}
		return w.Result(), nil
	})
	for range 2 {
		if sample, err := f.s.observePull(ctx, f.repo, 42); err != nil || sample == nil {
			t.Fatalf("observation did not reconcile: %v (%v)", sample, err)
		}
	}
	if work := notificationWork(t, f.s, f.repo, f.pull); conditional != 5 || work.Phase != "approved" {
		t.Fatalf("cached reviews did not reconcile: phase=%s, conditionals=%d", work.Phase, conditional)
	}
	observing = false
	if err := f.s.autoMerge(ctx, f.repo, 42); err != nil {
		t.Fatal(err)
	}
	run := notificationWork(t, f.s, f.repo, f.pull).AutoMerges[0]
	if run.State != "waiting" || !strings.Contains(run.Reason, "approval is no longer valid") || f.writes != 0 {
		t.Fatalf("stale observation affected merge preflight: %+v", run)
	}
}

func TestObservationCacheCapacity(t *testing.T) {
	cache := newObservationCache()
	body := make([]byte, 512<<10)
	for index := range observationCacheEntries + 1 {
		key := observationCacheKey{url: fmt.Sprint(index)}
		cache.store(cachedObservation{key: key, tag: `"etag"`, body: body, started: time.Now()})
	}
	if len(cache.entries) != 31 || cache.bytes > observationCacheBytes {
		t.Fatalf("observation cache exceeded byte limit: entries=%d, bytes=%d", len(cache.entries), cache.bytes)
	}
	for index := range observationCacheEntries + 1 {
		key := observationCacheKey{url: fmt.Sprint(index)}
		cache.store(cachedObservation{key: key, tag: `"etag"`, body: []byte("[]"), started: time.Now()})
	}
	if len(cache.entries) != observationCacheEntries {
		t.Fatalf("observation cache exceeded entry limit: entries=%d", len(cache.entries))
	}
}
