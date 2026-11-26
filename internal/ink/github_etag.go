//go:build linux || darwin

package ink

import (
	"container/list"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	observationCacheEntries = 256
	observationCacheBytes   = 16 << 20
	observationPageBytes    = 1 << 20
)

type observationCacheKey struct {
	scope tokenScope
	url   string
	api   string
}

type cachedObservation struct {
	key     observationCacheKey
	tag     string
	link    []string
	body    []byte
	started time.Time
}

func (entry cachedObservation) size() int {
	size := len(entry.body) + len(entry.tag) + len(entry.key.url)
	for _, link := range entry.link {
		size += len(link)
	}
	return size
}

type observationCache struct {
	mu      sync.Mutex
	entries map[observationCacheKey]*list.Element
	recency list.List
	bytes   int
}

func newObservationCache() *observationCache {
	return &observationCache{entries: make(map[observationCacheKey]*list.Element)}
}

func (c *observationCache) get(key observationCacheKey) (cachedObservation, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	item := c.entries[key]
	if item == nil {
		return cachedObservation{}, false
	}
	c.recency.MoveToFront(item)
	return item.Value.(cachedObservation), true
}

func (c *observationCache) store(entry cachedObservation) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if old := c.entries[entry.key]; old != nil {
		if old.Value.(cachedObservation).started.After(entry.started) {
			return
		}
		c.remove(old)
	}
	size := entry.size()
	if entry.tag == "" || size > observationPageBytes {
		return
	}
	for len(c.entries) >= observationCacheEntries || c.bytes+size > observationCacheBytes {
		c.remove(c.recency.Back())
	}
	c.entries[entry.key] = c.recency.PushFront(entry)
	c.bytes += size
}

func (c *observationCache) remove(item *list.Element) {
	entry := item.Value.(cachedObservation)
	c.bytes -= entry.size()
	delete(c.entries, entry.key)
	c.recency.Remove(item)
}

func observationETag(header http.Header) string {
	for _, control := range header.Values("Cache-Control") {
		for _, directive := range strings.Split(control, ",") {
			if strings.EqualFold(strings.TrimSpace(directive), "no-store") {
				return ""
			}
		}
	}
	return header.Get("ETag")
}

func (u upstream) observer() upstream {
	u.observeETags = true
	return u
}

// read handles complete cacheable responses and 304 revalidations. The caller
// still owns generic response decoding and upstream health accounting.
func (c *observationCache) read(res *http.Response, key observationCacheKey, cached cachedObservation, conditional bool, started time.Time, result any) (http.Header, bool, error) {
	empty := cachedObservation{key: key, started: started}
	switch res.StatusCode {
	case http.StatusNotModified:
		if conditional {
			header := res.Header.Clone()
			if _, supplied := header["Link"]; !supplied && len(cached.link) > 0 {
				header["Link"] = slices.Clone(cached.link)
			}
			if header.Get("ETag") == "" {
				header.Set("ETag", cached.tag)
			}
			cached.tag, cached.link, cached.started = observationETag(header), slices.Clone(header.Values("Link")), started
			if err := json.Unmarshal(cached.body, result); err != nil {
				c.store(empty)
				return nil, false, diagnosticError("invalid cached upstream response (HTTP 304)")
			}
			c.store(cached)
			return header, true, nil
		}
	case http.StatusOK:
		tag := observationETag(res.Header)
		if tag != "" {
			const maxResponse = 32 << 20
			body, err := io.ReadAll(io.LimitReader(res.Body, maxResponse+1))
			if err != nil || len(body) > maxResponse || json.Unmarshal(body, result) != nil {
				c.store(empty)
				return nil, false, diagnosticError("invalid upstream response (HTTP 200)")
			}
			c.store(cachedObservation{key: key, tag: tag, link: slices.Clone(res.Header.Values("Link")), body: body, started: started})
			return res.Header, true, nil
		}
	}
	c.store(empty)
	return nil, false, nil
}
