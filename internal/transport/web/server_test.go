//go:build !noweb

package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/fsutil"
	"masamong/internal/secrets"
)

func testServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	svc, err := application.Open(dir, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	raw, _ := json.Marshal(map[string]any{"instance": "web"})
	if res := svc.Execute(context.Background(), action.Request{Action: "setup.resume", Input: raw}); !res.OK {
		t.Fatalf("setup %+v", res)
	}
	s := New(svc)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	t.Cleanup(s.closePractice)
	return s, ts
}

type cookieJar struct{ cookies []*http.Cookie }

func (j *cookieJar) SetCookies(_ *url.URL, cs []*http.Cookie) {
	for _, c := range cs {
		replaced := false
		for i, old := range j.cookies {
			if old.Name == c.Name {
				j.cookies[i], replaced = c, true
			}
		}
		if !replaced {
			j.cookies = append(j.cookies, c)
		}
	}
}

func (j *cookieJar) Cookies(*url.URL) []*http.Cookie {
	var out []*http.Cookie
	for _, c := range j.cookies {
		if c.MaxAge >= 0 && c.Value != "" {
			out = append(out, c)
		}
	}
	return out
}

func client() *http.Client {
	return &http.Client{Jar: &cookieJar{}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func do(t *testing.T, c *http.Client, method, u string, body any, headers map[string]string) (int, map[string]any, http.Header) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, u, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	if len(out) == 0 {
		out["_raw"] = string(raw)
	}
	return res.StatusCode, out, res.Header
}

// signIn logs in through the JSON API and returns the CSRF token.
func signIn(t *testing.T, s *Server, ts *httptest.Server, c *http.Client) string {
	t.Helper()
	code, out, _ := do(t, c, "POST", ts.URL+"/api/login", map[string]any{"token": s.token}, nil)
	if code != 200 || out["csrf"] == nil {
		t.Fatalf("login %d %v", code, out)
	}
	return out["csrf"].(string)
}

func act(t *testing.T, c *http.Client, ts *httptest.Server, csrf, id string, in map[string]any) (int, map[string]any) {
	t.Helper()
	code, out, _ := do(t, c, "POST", ts.URL+"/api/action", map[string]any{"action": id, "input": in}, map[string]string{"X-CSRF-Token": csrf})
	return code, out
}

func TestSessionLoginAndRateLimit(t *testing.T) {
	s, ts := testServer(t)
	c := client()
	_, out, _ := do(t, c, "GET", ts.URL+"/api/session", nil, nil)
	if out["authenticated"] != false || out["csrf"] != nil || out["data_root"] != nil {
		t.Fatalf("anonymous session leaks: %v", out)
	}
	for i := 0; i < 10; i++ {
		if code, _, _ := do(t, c, "POST", ts.URL+"/api/login", map[string]any{"token": "wrong"}, nil); code != 401 {
			t.Fatalf("wrong token %d", code)
		}
	}
	if code, _, _ := do(t, c, "POST", ts.URL+"/api/login", map[string]any{"token": s.token}, nil); code != 429 {
		t.Fatalf("rate limit %d", code)
	}
	s.logins = &loginLimiter{}
	csrf := signIn(t, s, ts, c)
	_, out, _ = do(t, c, "GET", ts.URL+"/api/session", nil, nil)
	if out["authenticated"] != true || out["csrf"] != csrf {
		t.Fatalf("session: %v", out)
	}
}

func TestActionNeedsSessionCSRFAndSameOrigin(t *testing.T) {
	s, ts := testServer(t)
	c := client()
	if code, _ := act(t, c, ts, "", "help.version", nil); code != 401 {
		t.Fatalf("no session %d", code)
	}
	csrf := signIn(t, s, ts, c)
	if code, _ := act(t, c, ts, "bad", "help.version", nil); code != 403 {
		t.Fatalf("bad csrf %d", code)
	}
	code, _, _ := do(t, c, "POST", ts.URL+"/api/action", map[string]any{"action": "help.version"},
		map[string]string{"X-CSRF-Token": csrf, "Origin": "https://evil.example"})
	if code != 403 {
		t.Fatalf("cross-origin %d", code)
	}
	code, out := act(t, c, ts, csrf, "help.version", nil)
	if code != 200 || out["ok"] != true {
		t.Fatalf("action %d %v", code, out)
	}
	if code, out := act(t, c, ts, csrf, "messages.search", nil); code != 422 || out["ok"] != false {
		t.Fatalf("message search validation %d %v", code, out)
	}
	if code, out := act(t, c, ts, csrf, "storage.migrate", nil); code != 409 || out["ok"] != false { // off with the TiDB backend
		t.Fatalf("deferred %d %v", code, out)
	}
	code, out = act(t, c, ts, csrf, "config.patch", map[string]any{"ops": []any{map[string]any{"path": []any{"limits", "monthly_budget_usd"}, "value": "2.50"}}})
	if code != 200 || s.svc.Doc().Limits.MonthlyBudgetUSD != "2.50" {
		t.Fatalf("patch through API %d %v", code, out)
	}
}

func TestOneTimeLoginLinkWorksOnce(t *testing.T) {
	s, ts := testServer(t)
	code, err := CreateOnce(s.svc.DataRoot)
	if err != nil {
		t.Fatal(err)
	}
	c := client()
	res, err := c.Get(ts.URL + "/login?once=" + code + "&next=/tools")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 303 || res.Header.Get("Location") != "/tools" {
		t.Fatalf("once link %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	c2 := client()
	res, _ = c2.Get(ts.URL + "/login?once=" + code)
	res.Body.Close()
	if !strings.Contains(res.Header.Get("Location"), "error=token") {
		t.Fatal("a code works only once")
	}
	for _, next := range []string{
		"//evil.example", `/\evil.example`, `/%5Cevil.example`, `/%5c%5cevil.example`,
		`/%2f%2fevil.example`, `/%255Cevil.example`, "https://evil.example/path", "/api/admin",
	} {
		res, err = client().Get(ts.URL + "/login?token=" + s.token + "&next=" + url.QueryEscape(next))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.Header.Get("Location") != "/" {
			t.Errorf("unsafe next %q redirected to %q", next, res.Header.Get("Location"))
		}
	}
	res, err = client().Get(ts.URL + "/login?token=" + s.token + "&next=" + url.QueryEscape("/tools?tab=memory"))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.Header.Get("Location") != "/tools?tab=memory" {
		t.Fatalf("same-origin path was not retained: %q", res.Header.Get("Location"))
	}
	stale, _ := CreateOnce(s.svc.DataRoot)
	old := time.Now().Add(-5 * time.Minute)
	_ = os.Chtimes(filepath.Join(onceDir(s.svc.DataRoot), stale), old, old)
	if ConsumeOnce(s.svc.DataRoot, stale, time.Now()) {
		t.Fatal("stale codes expire")
	}
}

func TestSPAFallbackHeadersAndI18n(t *testing.T) {
	_, ts := testServer(t)
	c := client()
	for _, p := range []string{"/", "/tools/finance.fx", "/rooms/1/2", "/nope"} {
		res, err := c.Get(ts.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != 200 || !strings.Contains(string(body), "<") {
			t.Fatalf("%s → %d", p, res.StatusCode)
		}
		if !strings.Contains(res.Header.Get("Content-Security-Policy"), "script-src 'self'") || res.Header.Get("X-Frame-Options") != "DENY" {
			t.Fatalf("security headers on %s", p)
		}
	}
	if code, out, _ := do(t, c, "GET", ts.URL+"/api/nope", nil, nil); code != 404 || out["ok"] != false {
		t.Fatalf("unknown api %d", code)
	}
	_, out, _ := do(t, c, "GET", ts.URL+"/api/i18n?lang=ko", nil, nil)
	if out["lang"] != "ko" || out["strings"] == nil {
		t.Fatalf("i18n %v", out)
	}
	if got := matchUILocale("ko-KR", []string{"en", "ko"}); got != "ko" {
		t.Fatalf("regional UI locale = %q", got)
	}
	if got := matchUILocale("ar-EG", []string{"en", "ko", "ar"}); got != "ar" {
		t.Fatalf("file-discovered UI locale = %q", got)
	}
	if got := matchUILocale("fr-FR", []string{"en", "ko"}); got != "en" {
		t.Fatalf("unsupported UI locale = %q", got)
	}
}

func TestAPII18nSyntheticLocale(t *testing.T) {
	s, _ := testServer(t)
	bundles := fstest.MapFS{
		"ui/en.json":      {Data: []byte(`{"save":"Save","cancel":"Cancel"}`)},
		"ui/fr.json":      {Data: []byte(`{"save":"Enregistrer"}`)},
		"ui/zh-Hans.json": {Data: []byte(`{"save":"保存"}`)},
	}
	if got := uiLocalesFS(bundles); !slices.Equal(got, []string{"en", "fr", "zh-Hans"}) {
		t.Fatalf("discovered UI locales = %v", got)
	}
	for _, tc := range []struct {
		requested, language, save string
	}{
		{"fr-CA", "fr", "Enregistrer"},
		{"zh-CN", "zh-Hans", "保存"},
		{"zh-TW", "en", "Save"},
		{"de-DE", "en", "Save"},
	} {
		t.Run(tc.requested, func(t *testing.T) {
			w := httptest.NewRecorder()
			s.apiI18nFS(w, httptest.NewRequest("GET", "/api/i18n?lang="+tc.requested, nil), bundles)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d", w.Code)
			}
			var response struct {
				Lang    string            `json:"lang"`
				Strings map[string]string `json:"strings"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Lang != tc.language || response.Strings["save"] != tc.save || response.Strings["cancel"] != "Cancel" {
				t.Fatalf("resolved %s: lang %q, fallback/candidate mismatch", tc.requested, response.Lang)
			}
		})
	}
}

func TestUploadAndSecretStayOnServer(t *testing.T) {
	s, ts := testServer(t)
	c := client()
	csrf := signIn(t, s, ts, c)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "../../카톡 대화.csv")
	_, _ = fw.Write([]byte("Date,User,Message\n"))
	_ = mw.Close()
	req, _ := http.NewRequest("POST", ts.URL+"/api/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-CSRF-Token", csrf)
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var up map[string]any
	_ = json.NewDecoder(res.Body).Decode(&up)
	res.Body.Close()
	p, _ := up["path"].(string)
	if res.StatusCode != 200 || !strings.HasPrefix(p, filepath.Join(s.svc.DataRoot, "imports")+string(os.PathSeparator)) {
		t.Fatalf("upload %d %v", res.StatusCode, up)
	}
	if err := fsutil.VerifyPrivateFile(p); err != nil {
		t.Fatalf("upload file permissions: %v", err)
	}
	var staged bytes.Buffer
	stagedWriter := multipart.NewWriter(&staged)
	stagedFile, _ := stagedWriter.CreateFormFile("file", "notes.md")
	_, _ = stagedFile.Write([]byte("channel notes"))
	_ = stagedWriter.Close()
	stagedReq, _ := http.NewRequest("POST", ts.URL+"/api/upload?purpose=attachment", &staged)
	stagedReq.Header.Set("Content-Type", stagedWriter.FormDataContentType())
	stagedReq.Header.Set("X-CSRF-Token", csrf)
	stagedRes, err := c.Do(stagedReq)
	if err != nil {
		t.Fatal(err)
	}
	var stagedOut map[string]any
	_ = json.NewDecoder(stagedRes.Body).Decode(&stagedOut)
	stagedRes.Body.Close()
	stagedPath, _ := stagedOut["path"].(string)
	if stagedRes.StatusCode != 200 || !strings.HasPrefix(stagedPath, filepath.Join(s.svc.DataRoot, "rag_staging")+string(os.PathSeparator)) {
		t.Fatalf("attachment upload %d %v", stagedRes.StatusCode, stagedOut)
	}

	code, out, _ := do(t, c, "POST", ts.URL+"/api/secret", map[string]any{"target": "provider", "id": "finnhub", "value": "sk-test-secret-value"},
		map[string]string{"X-CSRF-Token": csrf})
	raw, _ := json.Marshal(out)
	if code != 200 || strings.Contains(string(raw), "sk-test-secret-value") {
		t.Fatalf("secret %d %s", code, raw)
	}
	ref := s.svc.Doc().Providers["finnhub"].CredentialRef
	if !strings.HasPrefix(ref, "file:") {
		t.Fatalf("credential ref %s", ref)
	}
	if err := fsutil.VerifyPrivateFile(strings.TrimPrefix(ref, "file:")); err != nil {
		t.Fatalf("secret file permissions: %v", err)
	}
	_, audit := act(t, c, ts, csrf, "audit.list", nil)
	raw, _ = json.Marshal(audit)
	if strings.Contains(string(raw), "sk-test-secret-value") {
		t.Fatal("secret in change log")
	}
	if code, _, _ := do(t, c, "POST", ts.URL+"/api/secret", map[string]any{"target": "provider", "id": "../x", "value": "v"},
		map[string]string{"X-CSRF-Token": csrf}); code != 422 {
		t.Fatalf("bad id %d", code)
	}
	oldPath := strings.TrimPrefix(ref, "file:")
	oldValue, err := os.ReadFile(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	if code, _, _ := do(t, c, "POST", ts.URL+"/api/secret", map[string]any{"target": "unknown", "id": "finnhub", "value": "replacement-must-not-win"},
		map[string]string{"X-CSRF-Token": csrf}); code != 422 {
		t.Fatalf("bad target %d", code)
	}
	kept, err := os.ReadFile(oldPath)
	if err != nil || !bytes.Equal(kept, oldValue) {
		t.Fatalf("failed secret update changed the existing key: %q %v", kept, err)
	}
}

func TestPracticeUploadAndImportCannotReachRealFiles(t *testing.T) {
	s, ts := testServer(t)
	c := client()
	csrf := signIn(t, s, ts, c)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "practice.csv")
	_, _ = fw.Write([]byte("Date,User,Message\n2024-01-01 12:00:00,A,hello\n"))
	_ = mw.Close()
	req, _ := http.NewRequest("POST", ts.URL+"/api/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-CSRF-Token", csrf)
	req.Header.Set(practiceHeader, "1")
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var up map[string]any
	_ = json.NewDecoder(res.Body).Decode(&up)
	res.Body.Close()
	p, _ := up["path"].(string)
	practiceImports := filepath.Join(application.PracticeRoot(s.svc.DataRoot), "imports") + string(os.PathSeparator)
	if res.StatusCode != 200 || !strings.HasPrefix(p, practiceImports) {
		t.Fatalf("practice upload escaped its root: %d %v", res.StatusCode, up)
	}

	realPath := filepath.Join(s.svc.DataRoot, "imports", "real.csv")
	if err := os.MkdirAll(filepath.Dir(realPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(realPath, []byte("Date,User,Message\n2024-01-01 12:00:00,A,real\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ := do(t, c, "POST", ts.URL+"/api/action", map[string]any{"action": "imports.preview", "input": map[string]any{"path": realPath, "room": "kakao:x"}},
		map[string]string{"X-CSRF-Token": csrf, practiceHeader: "1"})
	if code != http.StatusForbidden || out["ok"] != false {
		t.Fatalf("practice read real import: %d %v", code, out)
	}
}

// There is one admin and no accounts: the account, invite and session-list endpoints are gone, a
// name/password sign-in is refused, and a logged-out session is not carried across a restart.
func TestSingleAdminNoAccountsOrSessionLists(t *testing.T) {
	s, ts := testServer(t)
	a := client()
	csrfA := signIn(t, s, ts, a)
	for _, path := range []string{"/api/sessions", "/api/invite?code=x", "/api/nothing"} {
		code, _, _ := do(t, a, "GET", ts.URL+path, nil, nil)
		if code != 404 {
			t.Fatalf("%s must be 404, got %d", path, code)
		}
	}
	if code, _, _ := do(t, client(), "POST", ts.URL+"/api/login", map[string]any{"name": "someone", "password": "correct horse battery"}, nil); code != 401 {
		t.Fatalf("password sign-in must be refused, got %d", code)
	}
	if code, _ := act(t, a, ts, csrfA, "admins.list", nil); code == 200 {
		t.Fatal("admins.list must no longer exist")
	}
	if code, _, _ := do(t, a, "POST", ts.URL+"/api/logout", nil, map[string]string{"X-CSRF-Token": csrfA}); code != 200 {
		t.Fatalf("logout %d", code)
	}
	if err := s.sessions.save(s.svc.DataRoot, s.now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sessionsPath(s.svc.DataRoot)); !os.IsNotExist(err) {
		t.Fatal("logged-out session was persisted")
	}
}

func TestLitePageAndEvents(t *testing.T) {
	s, ts := testServer(t)
	c := client()
	res, _ := c.Get(ts.URL + "/lite")
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if strings.Contains(string(body), `name="csrf"`) {
		t.Fatal("lite page shows controls without a session")
	}
	signIn(t, s, ts, c)
	res, _ = c.Get(ts.URL + "/lite")
	body, _ = io.ReadAll(res.Body)
	res.Body.Close()
	page := string(body)
	i := strings.Index(page, `name="csrf" value="`)
	if i < 0 || strings.Contains(page, "<script") {
		t.Fatal("lite page: controls and no script")
	}
	csrf := page[i+19 : i+19+32]
	form := url.Values{"csrf": {csrf}, "do": {"pause"}}
	req, _ := http.NewRequest("POST", ts.URL+"/lite", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, _ = c.Do(req)
	res.Body.Close()
	if !s.svc.Paused("1", "2") {
		t.Fatal("lite pause")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ = http.NewRequestWithContext(ctx, "GET", ts.URL+"/api/events", nil)
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	rd := bufio.NewReader(res.Body)
	go s.svc.Execute(context.Background(), action.Request{Action: "runtime.pause", Input: json.RawMessage(`{"scope":"all","paused":"false"}`)})
	var got string
	for i := 0; i < 20; i++ {
		line, err := rd.ReadString('\n')
		if err != nil {
			break
		}
		if strings.HasPrefix(line, "data:") && strings.Contains(line, `"runtime"`) {
			got = line
			break
		}
	}
	if got == "" {
		t.Fatal("no change event")
	}
}

// On a Mac the console files keys in the login keychain; switching a provider from a console-written
// key file to the keychain removes the old file, and the provider resolves the new reference.
func TestSecretGoesToKeychainAndReplacesOldFile(t *testing.T) {
	store := map[string]string{}
	t.Cleanup(secrets.UseFakeKeychain(store))
	t.Setenv("MASAMONG_SECRET_STORE", "file")
	s, ts := testServer(t)
	c := client()
	csrf := signIn(t, s, ts, c)
	if code, _, _ := do(t, c, "POST", ts.URL+"/api/secret", map[string]any{"target": "provider", "id": "finnhub", "value": "first-file-key"},
		map[string]string{"X-CSRF-Token": csrf}); code != 200 {
		t.Fatalf("file store %d", code)
	}
	oldRef := s.svc.Doc().Providers["finnhub"].CredentialRef
	oldPath, ok := strings.CutPrefix(oldRef, "file:")
	if !ok {
		t.Fatalf("expected a file ref, got %s", oldRef)
	}
	t.Setenv("MASAMONG_SECRET_STORE", "")
	code, out, _ := do(t, c, "POST", ts.URL+"/api/secret", map[string]any{"target": "provider", "id": "finnhub", "value": `k"ey\with $odd chars`},
		map[string]string{"X-CSRF-Token": csrf})
	raw, _ := json.Marshal(out)
	if code != 200 || strings.Contains(string(raw), "odd chars") || !strings.Contains(string(raw), `"stored_in":"keychain"`) {
		t.Fatalf("keychain store %d %s", code, raw)
	}
	ref := s.svc.Doc().Providers["finnhub"].CredentialRef
	if !strings.HasPrefix(ref, "keychain:") || !strings.HasSuffix(ref, "/finnhub") {
		t.Fatalf("ref %s", ref)
	}
	if store[strings.TrimPrefix(ref, "keychain:")] != `k"ey\with $odd chars` {
		t.Fatalf("keychain value not stored intact")
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("the replaced key file must be removed: %v", err)
	}
	if st, ok := s.svc.Registry().Status("finnhub"); !ok || !st.Configured {
		t.Fatalf("provider must resolve the keychain ref: %+v", st)
	}
}

// The console never listens on a network interface; remote access is an SSH tunnel.
func TestConsoleBindsLoopbackOnly(t *testing.T) {
	for in, want := range map[string]string{
		"":               "127.0.0.1:8787",
		"127.0.0.1:9000": "127.0.0.1:9000",
		"localhost:9001": "localhost:9001",
		"[::1]:9002":     "[::1]:9002",
		"0.0.0.0:9003":   "127.0.0.1:9003",
		":9004":          "127.0.0.1:9004",
		"192.168.0.5:80": "127.0.0.1:80",
		"example.com":    "127.0.0.1:8787",
	} {
		if got := loopbackBind(in); got != want {
			t.Errorf("loopbackBind(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestConsoleRefusesAnotherHostName(t *testing.T) {
	for host, want := range map[string]bool{
		"127.0.0.1:8787": true, "localhost:8787": true, "LOCALHOST": true, "[::1]:8787": true, "127.0.0.2:9": true,
		"evil.example:8787": false, "192.168.0.10:8787": false, "": false, "localhost.evil.example": false,
	} {
		if got := loopbackHost(host); got != want {
			t.Errorf("loopbackHost(%q) = %v, want %v", host, got, want)
		}
	}
}
