package websearch

import (
	"net/url"
	"strings"
	"sync"
	"time"
)

// HostCache remembers, for an hour, the hosts whose pages could not be read (the service answered
// blocked or login, or the page timed out twice), so later searches do not wait out the same
// deadline for them again (a Korean wiki that always times out, a shop that always blocks).
// It lives in memory; a restart forgets it.
type HostCache struct {
	mu  sync.Mutex
	m   map[string]hostFail
	Now func() time.Time
	TTL time.Duration
}

type hostFail struct {
	err     string
	strikes int
	until   time.Time
}

// NewHostCache returns an empty cache with the one-hour memory.
func NewHostCache() *HostCache {
	return &HostCache{m: map[string]hostFail{}, Now: time.Now, TTL: time.Hour}
}

func hostKey(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
}

// Skip reports why a page of this address is not worth asking for now: "blocked" or "timeout".
func (h *HostCache) Skip(rawURL string) (why string, skip bool) {
	if h == nil {
		return "", false
	}
	key := hostKey(rawURL)
	h.mu.Lock()
	defer h.mu.Unlock()
	f, ok := h.m[key]
	if !ok || key == "" {
		return "", false
	}
	if !h.Now().Before(f.until) {
		delete(h.m, key)
		return "", false
	}
	if f.err == "timeout" && f.strikes < 2 {
		return "", false // one timeout may be a slow minute, not the host
	}
	return f.err, true
}

// Note records how a page of this address failed (Fetched.Err). blocked and login are remembered at
// once (as "blocked"); a timeout after the second time within the hour.
func (h *HostCache) Note(rawURL, fetchErr string) {
	if h == nil {
		return
	}
	var kind string
	switch fetchErr {
	case "blocked", "login":
		kind = "blocked"
	case "timeout":
		kind = "timeout"
	default:
		return
	}
	key := hostKey(rawURL)
	if key == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.Now()
	f := h.m[key]
	if now.After(f.until) {
		f = hostFail{}
	}
	switch {
	case kind == "blocked":
		f.err, f.strikes = "blocked", 2
	case f.err != "blocked":
		f.err = "timeout"
		f.strikes++
	}
	f.until = now.Add(h.TTL)
	h.m[key] = f
}
