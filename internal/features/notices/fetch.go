package notices

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/htmlindex"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/transform"
)

// Fetcher reads public board pages politely. Every request goes to one of the source's allowed hosts,
// which must resolve to a public address; robots.txt is honored; requests to one host are spaced; one
// run makes at most MaxRequests; bodies are capped. Pages are decoded from their charset (Korean
// university sites still serve EUC-KR).
type Fetcher struct {
	UserAgent    string
	Timeout      time.Duration
	MaxBytes     int64
	MaxRequests  int
	HostInterval time.Duration
	// AllowPrivate lets tests reach a local server; production never sets it.
	AllowPrivate bool

	mu       sync.Mutex
	client   *http.Client
	requests int
	last     map[string]time.Time
	robots   map[string]*robotsRules
}

// hostsKey carries a source's allowed hosts to the redirect check.
type hostsKey struct{}

// ErrRobots means robots.txt does not allow reading the page.
var ErrRobots = errors.New("robots.txt disallows this page")

// ErrBudget means the run's request budget is used up.
var ErrBudget = errors.New("request budget of this run used up")

func (f *Fetcher) init() {
	if f.client != nil {
		return
	}
	if f.Timeout <= 0 {
		f.Timeout = 20 * time.Second
	}
	if f.MaxBytes <= 0 {
		f.MaxBytes = 3 << 20
	}
	if f.MaxRequests <= 0 {
		f.MaxRequests = 60
	}
	if f.UserAgent == "" {
		f.UserAgent = "Mozilla/5.0 (compatible; masamong; +public-notices)"
	}
	f.last, f.robots = map[string]time.Time{}, map[string]*robotsRules{}
	dialer := &net.Dialer{Timeout: 8 * time.Second}
	allowPrivate := f.AllowPrivate
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			for _, ip := range ips {
				if !allowPrivate && !public(ip.IP) {
					return nil, fmt.Errorf("%s resolves to a non-public address", host)
				}
			}
			if len(ips) == 0 {
				return nil, fmt.Errorf("%s does not resolve", host)
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
		},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		MaxIdleConnsPerHost:   2,
	}
	f.client = &http.Client{Transport: tr, Timeout: f.Timeout, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		// a redirect stays on the source's hosts (robots.txt was read for those)
		if hosts, ok := req.Context().Value(hostsKey{}).([]string); ok && !hostAllowed(req.URL.Hostname(), hosts) {
			return fmt.Errorf("redirect to %s is not one of the source's hosts", req.URL.Hostname())
		}
		if req.URL.Scheme != "https" && req.URL.Scheme != "http" {
			return errors.New("redirect to a non-web address")
		}
		return nil
	}}
}

func public(ip net.IP) bool {
	return !(ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast())
}

func offline() bool { return os.Getenv("MASAMONG_OFFLINE") == "1" }

// Get reads one page of a source (allowed hosts, robots.txt, pacing, budget).
func (f *Fetcher) Get(ctx context.Context, src Source, rawURL string) (string, error) {
	f.mu.Lock()
	f.init()
	f.mu.Unlock()
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return "", fmt.Errorf("not a web address")
	}
	if !hostAllowed(u.Hostname(), src.AllowedHosts) {
		return "", fmt.Errorf("%s is not one of the source's hosts", u.Hostname())
	}
	if offline() && !f.AllowPrivate {
		return "", fmt.Errorf("offline")
	}
	ctx = context.WithValue(ctx, hostsKey{}, src.AllowedHosts)
	if ok, err := f.allowedByRobots(ctx, u); err != nil {
		return "", err
	} else if !ok {
		return "", ErrRobots
	}
	body, ctype, status, err := f.fetch(ctx, u.String())
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("status %d", status)
	}
	return decode(body, ctype), nil
}

func (f *Fetcher) fetch(ctx context.Context, rawURL string) ([]byte, string, int, error) {
	u, _ := url.Parse(rawURL)
	if err := f.pace(ctx, u.Hostname()); err != nil {
		return nil, "", 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", 0, err
	}
	req.Header.Set("User-Agent", f.UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml;q=0.9,*/*;q=0.5")
	req.Header.Set("Accept-Language", "ko,en;q=0.8")
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, "", 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, f.MaxBytes+1))
	if err != nil {
		return nil, "", resp.StatusCode, err
	}
	if int64(len(body)) > f.MaxBytes {
		return nil, "", resp.StatusCode, fmt.Errorf("page larger than %d bytes", f.MaxBytes)
	}
	return body, resp.Header.Get("Content-Type"), resp.StatusCode, nil
}

func (f *Fetcher) pace(ctx context.Context, host string) error {
	f.mu.Lock()
	if f.requests >= f.MaxRequests {
		f.mu.Unlock()
		return ErrBudget
	}
	f.requests++
	wait := time.Until(f.last[host].Add(f.HostInterval))
	next := time.Now()
	if wait > 0 {
		next = next.Add(wait)
	}
	f.last[host] = next
	f.mu.Unlock()
	if wait <= 0 {
		return nil
	}
	select {
	case <-time.After(wait):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// decode turns a page into text: its declared charset, else UTF-8 when valid, else EUC-KR (CP949).
func decode(body []byte, ctype string) string {
	charset := ""
	if i := strings.Index(strings.ToLower(ctype), "charset="); i >= 0 {
		charset = strings.Trim(ctype[i+len("charset="):], `"' ;`)
	} else if m := metaCharset(body); m != "" {
		charset = m
	}
	if charset != "" && !strings.EqualFold(charset, "utf-8") && !strings.EqualFold(charset, "utf8") {
		if enc, err := htmlindex.Get(charset); err == nil {
			if out, _, err := transform.Bytes(enc.NewDecoder(), body); err == nil {
				return string(out)
			}
		}
	}
	if utf8.Valid(body) {
		return string(body)
	}
	if out, _, err := transform.Bytes(korean.EUCKR.NewDecoder(), body); err == nil {
		return string(out)
	}
	return strings.ToValidUTF8(string(body), "�")
}

// metaCharset finds <meta charset=...> or its http-equiv form in the first bytes of a page.
func metaCharset(body []byte) string {
	head := bytes.ToLower(body[:min(len(body), 2048)])
	i := bytes.Index(head, []byte("charset="))
	if i < 0 {
		return ""
	}
	rest := head[i+len("charset="):]
	rest = bytes.TrimLeft(rest, `"' `)
	end := bytes.IndexAny(rest, `"' ;/>`)
	if end < 0 {
		return ""
	}
	return string(rest[:end])
}

// robotsRules are the Allow/Disallow lines that apply to us on one host.
type robotsRules struct {
	allow, disallow []string
	denyAll         bool // robots.txt could not be read for a reason that means "stay out"
}

func (f *Fetcher) allowedByRobots(ctx context.Context, u *url.URL) (bool, error) {
	host := strings.ToLower(u.Host)
	f.mu.Lock()
	rules, ok := f.robots[host]
	f.mu.Unlock()
	if !ok {
		body, _, status, err := f.fetch(ctx, u.Scheme+"://"+u.Host+"/robots.txt")
		switch {
		case err != nil && (errors.Is(err, ErrBudget) || ctx.Err() != nil):
			return false, err
		case err != nil:
			rules = &robotsRules{denyAll: true}
		case status == http.StatusUnauthorized || status == http.StatusForbidden:
			rules = &robotsRules{denyAll: true}
		case status >= 400:
			rules = &robotsRules{} // no robots.txt: everything allowed
		default:
			rules = parseRobots(string(body), "masamong")
		}
		f.mu.Lock()
		f.robots[host] = rules
		f.mu.Unlock()
	}
	return rules.allows(u.RequestURI()), nil
}

// parseRobots keeps the group for our agent token, else the "*" group.
func parseRobots(text, agent string) *robotsRules {
	type group struct {
		agents         []string
		allow, disallw []string
	}
	var groups []*group
	var cur *group
	lastWasAgent := false
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k, v = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)
		switch k {
		case "user-agent":
			if !lastWasAgent || cur == nil {
				cur = &group{}
				groups = append(groups, cur)
			}
			cur.agents = append(cur.agents, strings.ToLower(v))
			lastWasAgent = true
			continue
		case "allow":
			if cur != nil && v != "" {
				cur.allow = append(cur.allow, v)
			}
		case "disallow":
			if cur != nil && v != "" {
				cur.disallw = append(cur.disallw, v)
			}
		}
		lastWasAgent = false
	}
	var star, mine *group
	for _, g := range groups {
		for _, a := range g.agents {
			if a == "*" && star == nil {
				star = g
			}
			if strings.Contains(agent, a) && a != "*" && mine == nil {
				mine = g
			}
		}
	}
	g := mine
	if g == nil {
		g = star
	}
	if g == nil {
		return &robotsRules{}
	}
	return &robotsRules{allow: g.allow, disallow: g.disallw}
}

// allows applies the longest matching rule (Allow wins a tie); * and a trailing $ are understood.
func (r *robotsRules) allows(path string) bool {
	if r.denyAll {
		return false
	}
	best, allowed := -1, true
	check := func(rules []string, verdict bool) {
		for _, rule := range rules {
			if robotsMatch(rule, path) && (len(rule) > best || (len(rule) == best && verdict)) {
				best, allowed = len(rule), verdict
			}
		}
	}
	check(r.disallow, false)
	check(r.allow, true)
	return allowed
}

func robotsMatch(rule, path string) bool {
	anchored := strings.HasSuffix(rule, "$")
	rule = strings.TrimSuffix(rule, "$")
	parts := strings.Split(rule, "*")
	pos := 0
	for i, p := range parts {
		if i == 0 {
			if !strings.HasPrefix(path, p) {
				return false
			}
			pos = len(p)
			continue
		}
		j := strings.Index(path[pos:], p)
		if j < 0 {
			return false
		}
		pos += j + len(p)
	}
	return !anchored || pos == len(path) || (len(parts) > 1 && parts[len(parts)-1] == "")
}
