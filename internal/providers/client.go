package providers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"masamong/internal/errclass"
)

// Client wraps net/http with the safety rules every adapter must follow (10장, 11장):
// bounded timeout, bounded retries on transient status, response size cap, and URL redaction in errors.
type Client struct {
	HTTP       *http.Client
	UserAgent  string
	MaxRetries int           // total attempts = MaxRetries+1
	RetryBase  time.Duration // backoff base; attempt n waits RetryBase*2^(n-1), the upper half jittered
	MaxBody    int64
	Log        *slog.Logger
	Sleep      func(context.Context, time.Duration) error // test hook
	Jitter     func() float64                             // test hook: [0,1) (nil = math/rand)
	Now        func() time.Time                           // test hook: the clock a Retry-After date is read against
	// SecretParams lists query parameter names whose values are redacted in errors and logs.
	SecretParams []string
	// InProcess marks a client whose transport never leaves the process (the practice model), so
	// MASAMONG_OFFLINE does not apply to it.
	InProcess bool
	// Gate, when set, bounds the requests in flight through this client (WithGate): a host that is
	// strict about bursts gets no more at once however many tool calls run together. A slot is held for
	// one attempt and given back while a retry waits.
	Gate     chan struct{}
	governor *governor
}

// governors outlive registries and services. Only the credential fingerprint is retained here.
// provider is the canonical provider kind, so two config entries for one host/key share the limit.
var governors = struct {
	sync.Mutex
	byKey map[governorKey]*governor
}{byKey: make(map[governorKey]*governor)}

type governorKey struct {
	provider, host string
	credential     [32]byte
}

type governor struct {
	mu            sync.Mutex
	limit, active int
	changed       chan struct{}
}

func (g *governor) signal() {
	if g.changed != nil {
		close(g.changed)
		g.changed = nil
	}
}

// WithProviderGate shares a process-wide limit across all registries using this provider/host/key.
// Updating the capacity keeps old requests counted, including when the new capacity is smaller.
func (c *Client) WithProviderGate(provider, baseURL string, cred Credential, n int) *Client {
	if n < 1 {
		return c
	}
	u, _ := url.Parse(baseURL)
	host := ""
	if u != nil {
		host = strings.ToLower(u.Hostname())
		if port := u.Port(); port != "" && port != "80" && port != "443" {
			host = net.JoinHostPort(host, port)
		}
	}
	key := governorKey{provider: provider, host: host, credential: sha256.Sum256([]byte(cred.Value()))}
	governors.Lock()
	g := governors.byKey[key]
	if g == nil {
		g = &governor{}
		governors.byKey[key] = g
	}
	g.mu.Lock()
	g.limit = n
	g.signal()
	g.mu.Unlock()
	governors.Unlock()
	copy := *c
	copy.Gate, copy.governor = nil, g
	return &copy
}

// GateState reports capacity and requests in flight, without exposing the governor's identity.
func (c *Client) GateState() (limit, active int) {
	if g := c.governor; g != nil {
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.limit, g.active
	}
	return cap(c.Gate), len(c.Gate)
}

// WithGate is a copy of c that sends at most n requests at once (n < 1: no bound, c itself). The copy
// shares c's connections.
func (c *Client) WithGate(n int) *Client {
	if n < 1 {
		return c
	}
	g := *c
	g.Gate = make(chan struct{}, n)
	g.governor = nil
	return &g
}

func (c *Client) enter(ctx context.Context) error {
	if g := c.governor; g != nil {
		for {
			g.mu.Lock()
			if ctx.Err() != nil {
				g.mu.Unlock()
				return ctx.Err()
			}
			if g.active < g.limit {
				g.active++
				g.mu.Unlock()
				return nil
			}
			if g.changed == nil {
				g.changed = make(chan struct{})
			}
			changed := g.changed
			g.mu.Unlock()
			select {
			case <-changed:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	if c.Gate == nil {
		return nil
	}
	select {
	case c.Gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) leave() {
	if g := c.governor; g != nil {
		g.mu.Lock()
		g.active--
		g.signal()
		g.mu.Unlock()
		return
	}
	if c.Gate != nil {
		<-c.Gate
	}
}

// Response is the decoded transport result. Body is fully read and capped.
type Response struct {
	Status      int
	ContentType string
	Body        []byte
	URL         string // redacted
	Attempts    int
	Elapsed     time.Duration
	Header      http.Header // the answer's headers (a resumable upload's address comes in one)
}

// Error classifies transport failures so features can render 'unknown' vs 'failed' honestly (05장).
type Error struct {
	Kind     string // timeout | connection | status | too_large | cancelled
	Status   int
	URL      string
	Attempts int
	Err      error
	// NotSent: the request never left (the connection or the host's address could not be made), so the
	// host cannot have acted on it. Unset when that is not certain.
	NotSent bool
}

func (e *Error) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("provider %s: status %d after %d attempt(s): %s", e.Kind, e.Status, e.Attempts, e.URL)
	}
	return fmt.Sprintf("provider %s after %d attempt(s): %s: %v", e.Kind, e.Attempts, e.URL, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// ErrClass gives the failure its shared class (internal/errclass) so ledgers and retry decisions do
// not each re-derive it from Kind and Status. An empty class lets errclass.Of look at the wrapped error.
func (e *Error) ErrClass() errclass.Class {
	switch e.Kind {
	case "timeout":
		return errclass.Timeout
	case "connection", "offline":
		return errclass.Network
	case "cancelled":
		return errclass.Canceled
	case "status":
		return errclass.FromStatus(e.Status)
	}
	return ""
}

// Retryable reports whether the failure was transient (429/5xx/timeout/connection).
func (e *Error) Retryable() bool {
	switch e.Kind {
	case "timeout", "connection":
		return true
	case "status":
		return e.Status == 429 || e.Status >= 500
	}
	return false
}

// ErrOffline is returned for every non-loopback request while MASAMONG_OFFLINE=1 (test suites and
// air-gapped runs): nothing leaves the machine, and callers see a clear, non-retryable error.
var ErrOffline = errors.New("network disabled (MASAMONG_OFFLINE=1)")

func offline() bool { return os.Getenv("MASAMONG_OFFLINE") == "1" }

func loopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	h := u.Hostname()
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// ErrCrossHostRedirect is returned when a server redirects to another host. It is not retried.
var ErrCrossHostRedirect = errors.New("cross-host redirect refused")

// transport is shared by every provider client. Chat turns run in parallel (limits.max_concurrent_turns)
// against one model host; Go's default keeps only 2 idle connections per host, so most parallel calls
// would open (and TLS-handshake) a new connection and then drop it.
var transport = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = 128
	t.MaxIdleConnsPerHost = 32
	t.IdleConnTimeout = 90 * time.Second
	return t
}()

// CloseIdleConnections drops the shared transport's idle connections (tests, load runs).
func CloseIdleConnections() { transport.CloseIdleConnections() }

func NewClient(timeout time.Duration, log *slog.Logger) *Client {
	if log == nil {
		log = slog.Default()
	}
	return &Client{
		HTTP: &http.Client{
			Transport: transport,
			Timeout:   timeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 3 {
					return errors.New("too many redirects")
				}
				// Never follow a redirect to a different host with the original credentials.
				if req.URL.User != nil || req.URL.Scheme != "http" && req.URL.Scheme != "https" ||
					via[0].URL.Scheme == "https" && req.URL.Scheme != "https" ||
					redirectOrigin(req.URL) != redirectOrigin(via[0].URL) {
					return fmt.Errorf("%w (moved to %s; update the URL if you trust it)", ErrCrossHostRedirect, req.URL.Hostname())
				}
				return nil
			},
		},
		UserAgent:    "masamong/2 (+https://github.com/kim0040/masamong2)",
		MaxRetries:   2,
		RetryBase:    500 * time.Millisecond,
		MaxBody:      4 << 20,
		Log:          log,
		SecretParams: []string{"authKey", "serviceKey", "token", "key", "apikey", "api_key"},
	}
}

// NewDownloadClient is NewClient for a file download whose server answers with a redirect to a file
// host (GitHub release assets are served from another domain). The provider client refuses every
// cross-origin redirect so a credential is never sent elsewhere; this one follows a cross-origin
// redirect only to a host allowHost accepts, never leaves https, never follows a redirect that carries
// userinfo, and sends nothing but User-Agent and Accept to the new host. Use it only for requests that
// carry no credential.
func NewDownloadClient(timeout time.Duration, log *slog.Logger, allowHost func(host string) bool) *Client {
	c := NewClient(timeout, log)
	c.HTTP.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		first := via[0].URL
		if req.URL.User != nil || req.URL.Scheme != "http" && req.URL.Scheme != "https" || first.Scheme == "https" && req.URL.Scheme != "https" {
			return fmt.Errorf("%w (moved to %s)", ErrCrossHostRedirect, req.URL.Hostname())
		}
		if redirectOrigin(req.URL) != redirectOrigin(first) {
			if allowHost == nil || !allowHost(strings.ToLower(req.URL.Hostname())) {
				return fmt.Errorf("%w (moved to %s, which this download may not use)", ErrCrossHostRedirect, req.URL.Hostname())
			}
			for k := range req.Header {
				if !strings.EqualFold(k, "User-Agent") && !strings.EqualFold(k, "Accept") {
					req.Header.Del(k)
				}
			}
		}
		return nil
	}
	return c
}

func redirectOrigin(u *url.URL) string {
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return strings.ToLower(u.Scheme) + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

// Get performs a GET with retries. Headers may carry credentials; they are never logged.
func (c *Client) Get(ctx context.Context, rawURL string, headers map[string]string) (*Response, error) {
	return c.do(ctx, http.MethodGet, rawURL, headers, nil, "")
}

// PostJSON performs a POST with a JSON body.
func (c *Client) PostJSON(ctx context.Context, rawURL string, headers map[string]string, body []byte) (*Response, error) {
	return c.do(ctx, http.MethodPost, rawURL, headers, body, "application/json")
}

// Post performs a POST with a body of any content type (a multipart upload).
func (c *Client) Post(ctx context.Context, rawURL string, headers map[string]string, body []byte, contentType string) (*Response, error) {
	return c.do(ctx, http.MethodPost, rawURL, headers, body, contentType)
}

// Delete performs a DELETE.
func (c *Client) Delete(ctx context.Context, rawURL string, headers map[string]string) (*Response, error) {
	return c.do(ctx, http.MethodDelete, rawURL, headers, nil, "")
}

// WithMaxBody is a copy of c that reads responses up to n bytes (batch result files are far larger than
// a chat answer). The copy shares c's connections and gates.
func (c *Client) WithMaxBody(n int64) *Client {
	if n <= c.MaxBody {
		return c
	}
	g := *c
	g.MaxBody = n
	return &g
}

func (c *Client) do(ctx context.Context, method, rawURL string, headers map[string]string, body []byte, contentType string) (*Response, error) {
	redacted := c.Redact(rawURL)
	if offline() && !c.InProcess && !loopbackURL(rawURL) {
		return nil, &Error{Kind: "offline", URL: redacted, Attempts: 0, Err: ErrOffline}
	}
	attempts := c.MaxRetries + 1
	if attempts < 1 {
		attempts = 1
	}
	start := time.Now()
	var last *Error
	for attempt := 1; attempt <= attempts; attempt++ {
		var rdr io.Reader
		if body != nil {
			rdr = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, rawURL, rdr)
		if err != nil {
			return nil, &Error{Kind: "connection", URL: redacted, Attempts: attempt, Err: err}
		}
		req.Header.Set("User-Agent", c.UserAgent)
		req.Header.Set("Accept", "application/json, text/plain;q=0.9, */*;q=0.5")
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, data, readErr, err := c.attempt(req)
		if err != nil {
			last = classify(err, redacted, attempt)
			if last.Kind == "cancelled" || last.Kind == "redirect" || attempt == attempts {
				return nil, last
			}
			if err := c.wait(ctx, attempt); err != nil {
				return nil, &Error{Kind: waitKind(err), URL: redacted, Attempts: attempt, Err: err}
			}
			continue
		}
		if readErr != nil {
			last = classify(readErr, redacted, attempt)
			if last.Kind == "cancelled" || attempt == attempts {
				return nil, last
			}
			if err := c.wait(ctx, attempt); err != nil {
				return nil, &Error{Kind: waitKind(err), URL: redacted, Attempts: attempt, Err: err}
			}
			continue
		}
		if int64(len(data)) > c.MaxBody {
			return nil, &Error{Kind: "too_large", URL: redacted, Attempts: attempt, Status: resp.StatusCode, Err: fmt.Errorf("body exceeds %d bytes", c.MaxBody)}
		}
		out := &Response{Status: resp.StatusCode, ContentType: resp.Header.Get("Content-Type"), Body: data, URL: redacted, Attempts: attempt, Elapsed: time.Since(start), Header: resp.Header}
		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			last = &Error{Kind: "status", Status: resp.StatusCode, URL: redacted, Attempts: attempt}
			if attempt == attempts {
				return out, last
			}
			delay := c.backoff(attempt)
			if ra, ok := parseRetryAfter(resp.Header.Get("Retry-After"), c.now()); ok {
				delay = ra
			}
			if err := c.sleep(ctx, delay); err != nil {
				return out, &Error{Kind: waitKind(err), URL: redacted, Attempts: attempt, Err: err}
			}
			continue
		}
		if resp.StatusCode >= 400 {
			return out, &Error{Kind: "status", Status: resp.StatusCode, URL: redacted, Attempts: attempt}
		}
		return out, nil
	}
	return nil, last
}

// attempt holds one slot only for the HTTP attempt. Defers release it and close a received body on
// success, error and panic; retry sleeps are in do, after both defers have run.
func (c *Client) attempt(req *http.Request) (resp *http.Response, data []byte, readErr, err error) {
	if err = c.enter(req.Context()); err != nil {
		return
	}
	defer c.leave()
	if err = req.Context().Err(); err != nil {
		return
	}
	resp, err = c.HTTP.Do(req)
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		return
	}
	data, readErr = io.ReadAll(io.LimitReader(resp.Body, c.MaxBody+1))
	return
}

func (c *Client) wait(ctx context.Context, attempt int) error {
	return c.sleep(ctx, c.backoff(attempt))
}

// maxRetryWait caps one wait: a longer Retry-After is ignored (the backoff is used instead), and the
// backoff never grows past it.
const maxRetryWait = 30 * time.Second

// backoff is the wait before retry n: exponential (RetryBase*2^(n-1)) with "equal jitter", half fixed
// and half random, so clients that failed together do not retry together. Each wait is at most what the
// old linear backoff waited for the default two retries (RetryBase, then 2*RetryBase), so the total stays
// bounded as before.
func (c *Client) backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := c.RetryBase
	for i := 1; i < attempt && d < maxRetryWait; i++ {
		d *= 2
	}
	d = min(d, maxRetryWait)
	j := rand.Float64()
	if c.Jitter != nil {
		j = c.Jitter()
	}
	j = min(max(j, 0), 1)
	return d/2 + time.Duration(float64(d/2)*j)
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// parseRetryAfter reads a Retry-After header: delay seconds or an HTTP date. It accepts a wait above zero
// and at most maxRetryWait; anything else (absent, malformed, in the past, too long) is not used.
func parseRetryAfter(v string, now time.Time) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	var d time.Duration
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 || secs > int(maxRetryWait/time.Second) {
			return 0, false
		}
		d = time.Duration(secs) * time.Second
	} else if t, err := http.ParseTime(v); err == nil {
		d = t.Sub(now)
	} else {
		return 0, false
	}
	if d <= 0 || d > maxRetryWait {
		return 0, false
	}
	return d, true
}

func (c *Client) sleep(ctx context.Context, d time.Duration) error {
	if c.Sleep != nil {
		return c.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// waitKind names a context that ended while a retry waited: a deadline that passed is a timeout (the
// caller's limit on the call), anything else a cancel.
func waitKind(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "cancelled"
}

func classify(err error, redacted string, attempt int) *Error {
	if errors.Is(err, context.Canceled) {
		return &Error{Kind: "cancelled", URL: redacted, Attempts: attempt, Err: err}
	}
	if errors.Is(err, ErrCrossHostRedirect) {
		msg := err.Error()
		if i := strings.Index(msg, ErrCrossHostRedirect.Error()); i >= 0 {
			msg = msg[i:]
		}
		return &Error{Kind: "redirect", URL: redacted, Attempts: attempt, Err: errors.New(msg)}
	}
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return &Error{Kind: "timeout", URL: redacted, Attempts: attempt, Err: errors.New("timeout"), NotSent: notSent(err)}
	}
	return &Error{Kind: "connection", URL: redacted, Attempts: attempt, Err: errors.New(redactMessage(err.Error(), redacted)), NotSent: notSent(err)}
}

// notSent reports an error from before the request could be written: the dial or the name lookup failed.
func notSent(err error) bool {
	var oe *net.OpError
	if errors.As(err, &oe) && oe.Op == "dial" {
		return true
	}
	var de *net.DNSError
	return errors.As(err, &de)
}

// Redact masks secret query parameters so a URL can appear in logs and errors.
func (c *Client) Redact(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "<unparseable url>"
	}
	q := u.Query()
	for _, name := range c.SecretParams {
		for k := range q {
			if strings.EqualFold(k, name) {
				q.Set(k, "REDACTED")
			}
		}
	}
	u.RawQuery = q.Encode()
	u.User = nil
	return u.String()
}

func redactMessage(msg, redacted string) string {
	// net errors echo the full URL; keep only the host portion.
	if i := strings.Index(msg, "://"); i >= 0 {
		return "connection error to " + hostOf(redacted)
	}
	return msg
}

func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "?"
	}
	return u.Host
}
