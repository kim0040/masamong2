//go:build !noweb

package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/fsutil"
)

// Server is the daemon-embedded admin console: a Svelte single-page app (ui/, built into ui/dist and
// embedded) over a small JSON API that only speaks Application Actions, plus a no-JavaScript
// emergency page at /lite. It binds to loopback by default; remote access goes through SSH forwarding.
type Server struct {
	svc      *application.Service
	practice practiceHolder
	tailnet  tailnetName
	log      *slog.Logger
	http     *http.Server
	token    string
	sessions *sessionStore
	logins   *loginLimiter
	// remoteLogins counts failed sign-ins that came through an admin.web.hosts name (another device on
	// the tailnet), so guessing from there cannot lock this computer's own sign-in out
	remoteLogins *loginLimiter
	secretMu     sync.Mutex
	now          func() time.Time
	// keepSessions is set only for an intentional service restart. A stop, signal or crash must
	// require a fresh sign-in on the next start.
	keepSessions atomic.Bool
	// Force serves the console even when admin.web.enabled is false (the launcher passes --web).
	Force bool
	// OnListen reports the address actually bound (it differs from Bind when the port was taken).
	OnListen   func(addr string)
	addr       string // the address actually bound
	signin     map[string]string
	i18nMu     sync.Mutex
	i18nCache  map[string]i18nPayload
	ping       time.Duration // event stream keep-alive; the stream also rechecks its session then
	eventWrite time.Duration // per-write deadline for that stream; zero uses eventWriteTimeout
	streamsMu  sync.Mutex
	streams    []*liveEventStream
}

// A console event stream stays open for as long as the tab does. The cap is on how many of those
// handlers exist at once, not on how long one of them may live.
const (
	maxEventStreams   = 8
	eventWriteTimeout = 10 * time.Second
)

// liveEventStream is one open GET /api/events. cancel ends its loop; expire unblocks a write that
// is stuck in the socket so the handler can release its subscription.
type liveEventStream struct {
	session string
	cancel  context.CancelFunc
	expire  func()
}

func Embedded() bool { return true }

// New loads (or creates) the bootstrap token.
func New(svc *application.Service) *Server {
	s := &Server{
		svc:          svc,
		log:          svc.Log,
		sessions:     newSessionStore(),
		logins:       &loginLimiter{},
		remoteLogins: &loginLimiter{},
		now:          time.Now,
		ping:         15 * time.Second,
		eventWrite:   eventWriteTimeout,
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	tok, err := EnsureToken(svc.DataRoot)
	if err != nil {
		s.log.Error("web token", "err", err)
	}
	s.token = tok
	s.signin = signinCommands(svc.DataRoot)
	return s
}

// Handler exposes the routed mux (tests and the daemon share it).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", s.health)
	mux.HandleFunc("GET /api/session", s.session)
	mux.HandleFunc("POST /api/login", s.apiLogin)
	mux.HandleFunc("GET /api/practice", s.apiPractice)
	mux.HandleFunc("POST /api/practice", s.apiPractice)
	mux.HandleFunc("POST /api/logout", s.apiLogout)
	mux.HandleFunc("POST /api/action", s.apiAction)
	mux.HandleFunc("GET /api/i18n", s.apiI18n)
	mux.HandleFunc("GET /api/catalog", s.apiCatalog)
	mux.HandleFunc("GET /api/events", s.events)
	mux.HandleFunc("POST /api/upload", s.apiUpload)
	mux.HandleFunc("POST /api/secret", s.apiSecret)
	mux.HandleFunc("GET /api/export/config", s.exportConfig)
	notFound := func(w http.ResponseWriter, r *http.Request) {
		apiError(w, http.StatusNotFound, "NOT_FOUND", "no such endpoint")
	}
	mux.HandleFunc("GET /api/", notFound)
	mux.HandleFunc("POST /api/", notFound)
	mux.HandleFunc("GET /login", s.loginLink)
	mux.HandleFunc("GET /lite", s.litePage)
	mux.HandleFunc("POST /lite", s.liteSubmit)
	mux.HandleFunc("GET /", s.spa)
	return s.secure(mux)
}

// PageRoutes lists the console's page paths (catalog GUIRoutes must be among them).
func PageRoutes() []string { return spaRoutes() }

func (s *Server) enabled() bool {
	return s.Force || s.svc.Doc().Admin.Web.Enabled || !s.svc.Ready()
}

// Bind is the listen address. The console only ever listens on loopback: remote admins come in
// through an SSH tunnel (masamong connect), so a configured network address keeps only its port.
func (s *Server) Bind() string {
	if s.addr != "" {
		return s.addr
	}
	return loopbackBind(s.svc.Doc().Admin.Web.Bind)
}

func loopbackBind(bind string) string {
	if bind == "" {
		return "127.0.0.1:8787"
	}
	if isLoopback(bind) {
		return bind
	}
	port := "8787"
	if _, p, err := net.SplitHostPort(bind); err == nil && p != "" {
		port = p
	}
	return net.JoinHostPort("127.0.0.1", port)
}

// UseRestartBind keeps an intentional in-process restart on the address the browser already uses.
// The caller supplies the address reported by the previous listener before it shut down.
func (s *Server) UseRestartBind(bind string) {
	if bind == "" {
		return
	}
	if _, _, err := net.SplitHostPort(bind); err == nil {
		s.addr = bind
	}
}

func (s *Server) Listen(ctx context.Context) error {
	if !s.enabled() {
		<-ctx.Done()
		return ctx.Err()
	}
	if b := s.svc.Doc().Admin.Web.Bind; b != "" && !isLoopback(b) && s.addr == "" {
		s.log.Warn("admin.web.bind is not a loopback address; the console listens on 127.0.0.1 only (use SSH for remote access)", "configured", b)
	}
	ln, err := net.Listen("tcp", s.Bind())
	if err != nil && errors.Is(err, syscall.EADDRINUSE) && isLoopback(s.Bind()) {
		// Another program (or another instance) holds the port: take a free loopback port instead of
		// failing, and say so. The launcher finds the real address in daemon.json.
		s.log.Warn("console port in use; using a free port", "wanted", s.Bind())
		ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		return err
	}
	s.addr = ln.Addr().String()
	if s.OnListen != nil {
		s.OnListen(s.addr)
	}
	s.http = &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute, // no WriteTimeout: a live event stream would die. events sets a fresh per-write deadline instead
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	s.log.Info("web console listening", "bind", ln.Addr().String())
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.http.Shutdown(c)
		s.closePractice()
	}()
	s.sessions.load(s.svc.DataRoot, s.now())
	err = s.http.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		if s.keepSessions.Load() {
			if err := s.sessions.save(s.svc.DataRoot, s.now()); err != nil {
				s.log.Warn("sessions not kept for the next start", "err", err)
			}
		} else {
			_ = os.Remove(sessionsPath(s.svc.DataRoot))
		}
		return nil
	}
	return err
}

// PreserveSessionsForRestart marks this shutdown as an intentional restart. It is safe to call
// from the lifecycle goroutine while Listen is serving.
func (s *Server) PreserveSessionsForRestart() { s.keepSessions.Store(true) }

func (s *Server) eventWriteTimeout() time.Duration {
	if s.eventWrite > 0 {
		return s.eventWrite
	}
	return eventWriteTimeout
}

// beginEventStream records one event stream. Past the cap it closes the oldest stream of this
// session, or the global oldest when this session has none: a reloaded tab must get a stream even
// while the previous SSH tunnel is stuck in a write. Refusing the new one (429) would leave that
// tab frozen until the stuck write's deadline. The displaced client retries and resyncs from hello.
func (s *Server) beginEventStream(session string, cancel context.CancelFunc, expire func()) (end func()) {
	s.streamsMu.Lock()
	defer s.streamsMu.Unlock()
	slot := &liveEventStream{session: session, cancel: cancel, expire: expire}
	for len(s.streams) >= maxEventStreams {
		victim := 0
		for i, st := range s.streams {
			if st.session == session {
				victim = i
				break
			}
		}
		old := s.streams[victim]
		s.streams = append(s.streams[:victim], s.streams[victim+1:]...)
		if old.expire != nil {
			old.expire()
		}
		old.cancel()
	}
	s.streams = append(s.streams, slot)
	return func() {
		s.streamsMu.Lock()
		defer s.streamsMu.Unlock()
		for i, st := range s.streams {
			if st == slot {
				s.streams = append(s.streams[:i], s.streams[i+1:]...)
				return
			}
		}
	}
}

func (s *Server) openEventStreams() int {
	s.streamsMu.Lock()
	defer s.streamsMu.Unlock()
	return len(s.streams)
}

func isLoopback(bind string) bool {
	host, _, err := net.SplitHostPort(bind)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return host == "localhost" || (ip != nil && ip.IsLoopback())
}

// ---------------------------------------------------------------------------
// security middleware
// ---------------------------------------------------------------------------

const (
	sessionCookie = "masamong_session"
	sessionTTL    = 7 * 24 * time.Hour
)

// Scripts only from this origin; inline style attributes are allowed because the app animates with
// element styles (no inline script is ever allowed).
const csp = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; " +
	"connect-src 'self'; font-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'; manifest-src 'self'"

func (s *Server) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		if !loopbackHost(r.Host) && !s.listedHost(r.Host) {
			http.Error(w, "this console answers only on this computer (127.0.0.1 or localhost) and the names in admin.web.hosts", http.StatusMisdirectedRequest)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if !sameOrigin(r) {
				http.Error(w, "cross-origin request refused", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// loopbackHost reports whether a Host header names this computer (127.0.0.1, ::1 or localhost, any
// port). The console listens on loopback only, so every real request says so; a web page that points
// its own domain at 127.0.0.1 (DNS rebinding) sends that domain instead and is refused before it reaches
// even the sign-in endpoint (whose failure limit it could otherwise use up).
func loopbackHost(host string) bool { return LoopbackHost(host) }

// listedHost reports whether a Host header is one the operator listed in admin.web.hosts (any port): a
// proxy on this computer, such as tailscale serve, forwards that name to the loopback listener.
func (s *Server) listedHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	return host != "" && slices.Contains(s.svc.Doc().Admin.Web.Hosts, host)
}

// sameOrigin requires an exact scheme+host match when the browser sends Origin, and falls back to
// Sec-Fetch-Site. Requests with neither header (curl, tests) are allowed; the CSRF token still applies.
func sameOrigin(r *http.Request) bool { return SameOrigin(r) }

// ---------------------------------------------------------------------------
// sessions
// ---------------------------------------------------------------------------

// session is one signed-in browser. There is one admin — whoever can reach the console through the
// launcher's one-time link (this machine, or an SSH tunnel) or read the token file — so a session
// carries no identity beyond its CSRF secret.
type session struct {
	CSRF    string
	Created time.Time
	Expires time.Time
	Agent   string
}

type sessionStore struct {
	mu   sync.Mutex
	byID map[string]session
}

func newSessionStore() *sessionStore { return &sessionStore{byID: map[string]session{}} }

func (st *sessionStore) create(now time.Time, agent string) (id string, sess session) {
	id = randomHex(24)
	sess = session{CSRF: randomHex(16), Created: now, Expires: now.Add(sessionTTL), Agent: agentLabel(agent)}
	st.mu.Lock()
	for k, v := range st.byID {
		if !now.Before(v.Expires) {
			delete(st.byID, k)
		}
	}
	// A script that signs in on every run must not grow the store for a week: past the cap the oldest
	// sign-in goes (a browser in daily use is rarely the oldest).
	for len(st.byID) >= maxSessions {
		oldest, at := "", now
		for k, v := range st.byID {
			if !v.Created.After(at) {
				oldest, at = k, v.Created
			}
		}
		delete(st.byID, oldest)
	}
	st.byID[id] = sess
	st.mu.Unlock()
	return id, sess
}

// maxSessions bounds live sign-ins (browsers, the menu bar icon, CLI commands).
const maxSessions = 64

func (st *sessionStore) len() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.byID)
}

func (st *sessionStore) get(id string, now time.Time) (session, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	sess, ok := st.byID[id]
	if !ok || !now.Before(sess.Expires) {
		delete(st.byID, id)
		return session{}, false
	}
	return sess, true
}

func (st *sessionStore) drop(id string) {
	st.mu.Lock()
	delete(st.byID, id)
	st.mu.Unlock()
}

// Sessions outlive only an intentional restart (including restore): that shutdown writes them to
// sessions.json, and the next start reads and deletes it. The file is 0600 in the data folder, next
// to the bootstrap token that already grants owner access; a stop, signal or crash loses them.
func sessionsPath(dataRoot string) string { return filepath.Join(dataRoot, "sessions.json") }

func (st *sessionStore) save(dataRoot string, now time.Time) error {
	st.mu.Lock()
	live := map[string]session{}
	for id, sess := range st.byID {
		if now.Before(sess.Expires) {
			live[id] = sess
		}
	}
	st.mu.Unlock()
	if len(live) == 0 {
		if err := os.Remove(sessionsPath(dataRoot)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	raw, err := json.Marshal(live)
	if err != nil {
		return err
	}
	path := sessionsPath(dataRoot)
	if err := fsutil.PrivateFile(path); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := fsutil.PrivateFile(path); err != nil {
		_ = f.Close()
		return err
	}
	// OpenFile does not narrow an existing file's mode, so enforce it before writing secrets.
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(raw); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func (st *sessionStore) load(dataRoot string, now time.Time) {
	raw, err := os.ReadFile(sessionsPath(dataRoot))
	if err != nil {
		return
	}
	_ = os.Remove(sessionsPath(dataRoot))
	var saved map[string]session
	if json.Unmarshal(raw, &saved) != nil {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	for id, sess := range saved {
		if now.Before(sess.Expires) {
			st.byID[id] = sess
		}
	}
}

// agentLabel keeps only a browser/OS hint, never the full user agent string.
func agentLabel(ua string) string {
	browser, osName := "browser", ""
	for _, b := range []string{"Edg", "Firefox", "Chrome", "Safari"} {
		if strings.Contains(ua, b) {
			browser = map[string]string{"Edg": "Edge"}[b]
			if browser == "" {
				browser = b
			}
			break
		}
	}
	for _, o := range []string{"Mac OS", "Windows", "Android", "iPhone", "Linux"} {
		if strings.Contains(ua, o) {
			osName = o
			break
		}
	}
	return strings.TrimSpace(browser + " " + osName)
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) loginsFor(r *http.Request) *loginLimiter {
	if loopbackHost(r.Host) {
		return s.logins
	}
	return s.remoteLogins
}

// loginLimiter slows token guessing: after 10 failures in a minute the endpoint answers 429.
type loginLimiter struct {
	mu       sync.Mutex
	failures int
	window   time.Time
}

func (l *loginLimiter) allow(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.window) > time.Minute {
		l.failures, l.window = 0, now
	}
	return l.failures < 10
}

func (l *loginLimiter) fail(now time.Time) {
	l.mu.Lock()
	if now.Sub(l.window) > time.Minute {
		l.failures, l.window = 0, now
	}
	l.failures++
	l.mu.Unlock()
}

func (s *Server) currentSession(r *http.Request) (string, session, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return "", session{}, false
	}
	sess, ok := s.sessions.get(c.Value, s.now())
	return c.Value, sess, ok
}

// operator is the action.Operator for a session: always the installation's one admin.
func operator(session) *action.Operator {
	return &action.Operator{ID: "owner"}
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, id string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: id, Path: "/", HttpOnly: true, Secure: r.TLS != nil,
		SameSite: http.SameSiteStrictMode, MaxAge: maxAge,
	})
}

func (s *Server) tokenMatches(candidate string) bool {
	if s.token == "" || candidate == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(s.token)) == 1
}

func (s *Server) csrfOK(sess session, got string) bool {
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(sess.CSRF)) == 1
}

// loginLink consumes a one-shot link (?once= from the launcher, or ?token= printed by `admin open`)
// and redirects so the secret does not stay in the address bar or history. Without one it serves the
// app, which shows the sign-in screen.
func (s *Server) loginLink(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	once, tok := q.Get("once"), q.Get("token")
	if once == "" && tok == "" {
		s.spa(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if !s.logins.allow(s.now()) {
		http.Redirect(w, r, "/login?error=rate", http.StatusSeeOther)
		return
	}
	ok := false
	if once != "" {
		ok = ConsumeOnce(s.svc.DataRoot, once, s.now())
	} else {
		ok = s.tokenMatches(tok)
	}
	if !ok {
		s.logins.fail(s.now())
		http.Redirect(w, r, "/login?error=token", http.StatusSeeOther)
		return
	}
	id, _ := s.sessions.create(s.now(), r.UserAgent())
	s.setSessionCookie(w, r, id, int(sessionTTL.Seconds()))
	next := loginNext(q.Get("next"))
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// loginNext accepts only a same-origin absolute path. Backslashes and encoded path separators are
// rejected because browsers can interpret them as URL authority separators after the redirect.
func loginNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.Contains(next, "\\") {
		return "/"
	}
	decoded := next
	for i := 0; i < 5; i++ {
		u, err := url.Parse(decoded)
		if err != nil || u.IsAbs() || u.Host != "" || u.Opaque != "" || !strings.HasPrefix(u.Path, "/") || strings.HasPrefix(u.Path, "//") || strings.Contains(u.Path, "\\") {
			return "/"
		}
		if u.Path == "/api" || strings.HasPrefix(u.Path, "/api/") {
			return "/"
		}
		unescaped, err := url.PathUnescape(decoded)
		if err != nil {
			return "/"
		}
		if unescaped == decoded {
			return next
		}
		decoded = unescaped
	}
	// Reject unusually deeply encoded paths instead of guessing how a downstream parser will read them.
	return "/"
}
