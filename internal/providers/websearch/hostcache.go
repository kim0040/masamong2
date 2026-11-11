package websearch

import (
	"net/url"
	"strings"
	"sync"
	"time"
)

// HostCache remembers, for an hour, the hosts whose pages could not be read (the fetch service said
// blocked or login for the address, or timed out on it twice), so later searches do not wait out the same
// deadline for them again (a Korean wiki that always times out, a shop that always blocks). Only a
// failure the target caused is noted (Fetched.Target): never our own cut-off, a page still queued when the
// deadline passed, a cancelled turn or the fetch service's failure. A page read resets its host.
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

// platformHosts hold the pages of many unrelated authors under one host: one private blog post (login)
// or one post the service would not show (blocked) says nothing about the others, and a slow minute at
// the platform needs three timeouts, not two, to count.
var platformHosts = map[string]bool{
	"blog.naver.com": true, "m.blog.naver.com": true, "news.naver.com": true, "n.news.naver.com": true,
	"v.daum.net": true, "brunch.co.kr": true, "velog.io": true, "medium.com": true,
}

const (
	timeoutStrikes         = 2
	platformTimeoutStrikes = 3
)

func hostOf(raw string) (host string, u *url.URL) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", nil
	}
	return strings.TrimPrefix(strings.ToLower(u.Hostname()), "www."), u
}

// hostKey is the host a timeout is counted against.
func hostKey(raw string) string {
	host, _ := hostOf(raw)
	return host
}

// blockKey is what blocked and login are counted against: the host, or for a platform host the host and
// the first path segment (the author's blog, the section).
func blockKey(raw string) string {
	host, u := hostOf(raw)
	if host == "" || !platformHosts[host] {
		return host
	}
	seg, _, _ := strings.Cut(strings.Trim(u.EscapedPath(), "/"), "/")
	return host + "/" + strings.ToLower(seg)
}

func (h *HostCache) strikesFor(host string) int {
	if platformHosts[host] {
		return platformTimeoutStrikes
	}
	return timeoutStrikes
}

// Skip reports why a page of this address is not worth asking for now: "blocked" or "timeout".
func (h *HostCache) Skip(rawURL string) (why string, skip bool) {
	if h == nil {
		return "", false
	}
	host := hostKey(rawURL)
	if host == "" {
		return "", false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.Now()
	if f, ok := h.m[blockKey(rawURL)]; ok && now.Before(f.until) && f.err == "blocked" {
		return "blocked", true
	}
	f, ok := h.m[host]
	if !ok || !now.Before(f.until) {
		return "", false
	}
	if f.err == "blocked" {
		return "blocked", true
	}
	if f.strikes < h.strikesFor(host) {
		return "", false // a timeout or two may be a slow minute, not the host
	}
	return "timeout", true
}

// Note records how a page of this address failed (Fetched.Err), for a failure the target caused. blocked
// and login are remembered at once (as "blocked") for the host, for a platform host for the author's
// part of it; a timeout after the second time within the hour (the third on a platform host).
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
	host := hostKey(rawURL)
	if host == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.Now()
	h.pruneLocked(now)
	if kind == "blocked" {
		h.m[blockKey(rawURL)] = hostFail{err: "blocked", strikes: 2, until: now.Add(h.TTL)}
		return
	}
	f := h.m[host]
	if f.err == "blocked" {
		return // a whole host that is blocked stays so
	}
	f.err = "timeout"
	f.strikes++
	f.until = now.Add(h.TTL)
	h.m[host] = f
}

// OK records that a page of this address was read: the host's timeouts and the block of the address's
// part of it are forgotten.
func (h *HostCache) OK(rawURL string) {
	if h == nil {
		return
	}
	host := hostKey(rawURL)
	if host == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.m, blockKey(rawURL))
	delete(h.m, host)
}

// pruneLocked drops the entries whose hour is over.
func (h *HostCache) pruneLocked(now time.Time) {
	for k, f := range h.m {
		if !now.Before(f.until) {
			delete(h.m, k)
		}
	}
}

// Len is how many hosts are remembered (expired ones not yet pruned included).
func (h *HostCache) Len() int {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.m)
}
