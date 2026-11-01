//go:build !noweb

package web

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"masamong/internal/action"
	"masamong/internal/catalog"
	"masamong/internal/fsutil"
	"masamong/internal/i18n"
	"masamong/internal/memory/corpus"
	"masamong/locales"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func apiError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": map[string]any{"code": code, "message": msg}})
}

// requireAPI checks the session, and for writes the X-CSRF-Token header.
func (s *Server) requireAPI(w http.ResponseWriter, r *http.Request, write bool) (string, session, bool) {
	id, sess, ok := s.currentSession(r)
	if !ok {
		apiError(w, http.StatusUnauthorized, "UNAUTHORIZED", "sign in required")
		return "", session{}, false
	}
	if write && !s.csrfOK(sess, r.Header.Get("X-CSRF-Token")) {
		apiError(w, http.StatusForbidden, "UNAUTHORIZED", "invalid csrf token")
		return "", session{}, false
	}
	return id, sess, true
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ready": s.svc.Ready(), "instance": s.svc.Doc().Instance.ID, "version": versionOf()})
}

func versionOf() string { return appVersion() }

// requestSource is "web" for the console, or "cli"/"tray" when the launcher's client says so (the change
// log names where a stop or restart came from). Other values are ignored.
func requestSource(r *http.Request) string {
	switch v := r.Header.Get("X-Masamong-Via"); v {
	case "cli", "tray":
		return v
	}
	return "web"
}

func (s *Server) lang(r *http.Request) string {
	if l := r.URL.Query().Get("lang"); l != "" {
		return s.svc.I18n.Resolve(l)
	}
	if l := r.Header.Get("Accept-Language"); l != "" {
		return s.svc.I18n.Resolve(strings.Split(strings.Split(l, ",")[0], ";")[0])
	}
	return s.svc.I18n.Resolve(s.svc.Doc().I18n.DefaultLocale)
}

func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	_, sess, ok := s.currentSession(r)
	// signin: the commands that sign a browser in again (shown on the sign-in screen)
	// platform: the operating system the service runs on, so the console words tray/menu bar, file paths
	// and where keys are kept for that system (the browser may be on another machine, over SSH)
	out := map[string]any{"authenticated": ok, "lang": s.uiLang(r), "locales": uiLocales(), "ready": s.svc.Ready(), "version": versionOf(), "signin": s.signin, "platform": runtime.GOOS}
	if ok {
		out["csrf"] = sess.CSRF
		out["practice"] = s.practice.status()
		out["instance"] = s.svc.Doc().Instance.ID
		out["revision"] = s.svc.Revision()
		out["data_root"] = s.svc.DataRoot
		out["bind"] = s.Bind()
		// hosts: the extra names the console answers to; tailnet: this computer's Tailscale name, offered
		// as one (tailscale serve forwards it here)
		out["hosts"] = append([]string{}, s.svc.Doc().Admin.Web.Hosts...)
		out["tailnet"] = s.tailnet.get()
		out["web_build"] = true
		out["bot_names"] = s.svc.Doc().BotNames()
		out["timezone"] = s.svc.Doc().I18n.Timezone
	}
	writeJSON(w, http.StatusOK, out)
}

// apiLogin accepts the bootstrap token (the fallback when the browser was not opened by the launcher).
func (s *Server) apiLogin(w http.ResponseWriter, r *http.Request) {
	logins := s.loginsFor(r)
	if !logins.allow(s.now()) {
		apiError(w, http.StatusTooManyRequests, "RATE_LIMITED", "too many attempts; wait a minute")
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
		apiError(w, http.StatusBadRequest, "INVALID_INPUT", "bad request")
		return
	}
	if !s.tokenMatches(strings.TrimSpace(body.Token)) {
		logins.fail(s.now())
		apiError(w, http.StatusUnauthorized, "UNAUTHORIZED", "token does not match")
		return
	}
	id, sess := s.sessions.create(s.now(), r.UserAgent())
	s.setSessionCookie(w, r, id, int(sessionTTL.Seconds()))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "csrf": sess.CSRF})
}

func (s *Server) apiLogout(w http.ResponseWriter, r *http.Request) {
	id, _, ok := s.requireAPI(w, r, true)
	if !ok {
		return
	}
	s.sessions.drop(id)
	s.setSessionCookie(w, r, "", -1)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type actionBody struct {
	Action       string         `json:"action"`
	Input        map[string]any `json:"input"`
	BaseRevision int            `json:"base_revision"`
}

func (s *Server) apiAction(w http.ResponseWriter, r *http.Request) {
	_, sess, ok := s.requireAPI(w, r, true)
	if !ok {
		return
	}
	var body actionBody
	dec := json.NewDecoder(io.LimitReader(r.Body, 2<<20))
	dec.UseNumber()
	if err := dec.Decode(&body); err != nil || body.Action == "" {
		apiError(w, http.StatusBadRequest, "INVALID_INPUT", "action required")
		return
	}
	if d, known := catalog.Lookup(body.Action); known && d.Availability != catalog.Executable {
		writeJSON(w, http.StatusConflict, action.Result{OK: false, Action: body.Action,
			Error: &action.Error{Code: "ACTION_UNAVAILABLE", Message: s.svc.Msg(s.lang(r), "action.unavailable"), Args: map[string]any{"reason": d.DeferredReason}}})
		return
	}
	if body.Input == nil {
		body.Input = map[string]any{}
	}
	svc, err := s.target(r, body.Action)
	if err != nil {
		apiError(w, http.StatusServiceUnavailable, "IO", "practice instance: "+err.Error())
		return
	}
	raw, _ := json.Marshal(body.Input)
	req := action.Request{Action: body.Action, Locale: s.langFor(r), Input: raw, RequestID: requestSource(r), BaseRevision: body.BaseRevision, Operator: operator(sess)}
	res := svc.Execute(r.Context(), req)
	writeJSON(w, statusFor(res), res)
}

// langFor is the console language the client asked for (X-Lang), so server messages match the UI.
func (s *Server) langFor(r *http.Request) string {
	if l := r.Header.Get("X-Lang"); l != "" {
		return s.svc.I18n.Resolve(l)
	}
	return s.lang(r)
}

func statusFor(res action.Result) int {
	if res.Error == nil {
		return http.StatusOK
	}
	switch res.Error.Code {
	case "CONFIG_CONFLICT", "STALE_REVISION", "SCOPE_MISMATCH", "ACTION_UNAVAILABLE", "ACTION_UNKNOWN", "LEASE_MISMATCH", "JOB_IDEMPOTENCY_CONFLICT":
		return http.StatusConflict
	case "JOB_NOT_FOUND":
		return http.StatusNotFound
	case "INVALID_INPUT", "CONFIG_INVALID", "PLAN_REJECTED", "BACKUP_VERIFY_FAILED":
		return http.StatusUnprocessableEntity
	case "UNAUTHORIZED", "PERMISSION_DENIED":
		return http.StatusForbidden
	case "NOT_SETUP":
		return http.StatusPreconditionFailed
	case "FEATURE_DISABLED", "TOOL_LIMITED", "BUDGET_EXCEEDED", "PROVIDER_RATE_LIMITED":
		return http.StatusTooManyRequests
	case "PROVIDER_UNCONFIGURED", "PROVIDER_FAKE_ONLY", "PROVIDER_REJECTED", "DISCORD_NOT_CONNECTED":
		// an expected state of the setup (no key yet, a refused key, no bot connected): the request was
		// fine but something it depends on is not ready. Not a bad gateway, so it is not a console error.
		return http.StatusFailedDependency
	case "IO":
		return http.StatusInternalServerError
	}
	// PROVIDER_UNAVAILABLE, PROVIDER_FORMAT and PROVIDER_ERROR: a service the action asked failed
	return http.StatusBadGateway
}

// apiI18n serves the console strings (locales/ui/<lang>.json) merged over English.
func (s *Server) apiI18n(w http.ResponseWriter, r *http.Request) {
	lang := s.uiLang(r)
	s.i18nMu.Lock()
	p, ok := s.i18nCache[lang]
	if !ok {
		p = buildI18n(locales.UIFS, lang)
		if s.i18nCache == nil {
			s.i18nCache = map[string]i18nPayload{}
		}
		s.i18nCache[lang] = p
	}
	s.i18nMu.Unlock()
	serveI18n(w, r, p)
}

// apiI18nFS serves strings from another file system (tests).
func (s *Server) apiI18nFS(w http.ResponseWriter, r *http.Request, fsys fs.FS) {
	serveI18n(w, r, buildI18n(fsys, s.uiLangFS(r, fsys)))
}

// i18nPayload is one language's answer, built once: the strings are embedded in the program and do
// not change while it runs. The tag is a hash of the body, so a browser keeps its copy until a new
// build changes a string.
type i18nPayload struct {
	body, gz []byte
	etag     string
}

func buildI18n(fsys fs.FS, lang string) i18nPayload {
	out := map[string]string{}
	for _, l := range []string{"en", lang} {
		raw, err := fs.ReadFile(fsys, "ui/"+l+".json")
		if err != nil {
			continue
		}
		m := map[string]string{}
		if json.Unmarshal(raw, &m) == nil {
			for k, v := range m {
				out[k] = v
			}
		}
	}
	body, _ := json.Marshal(map[string]any{"lang": lang, "strings": out})
	sum := sha256.Sum256(body)
	var zipped bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&zipped, gzip.BestCompression)
	_, _ = zw.Write(body)
	_ = zw.Close()
	return i18nPayload{body: body, gz: zipped.Bytes(), etag: `W/"` + hex.EncodeToString(sum[:8]) + `"`}
}

func serveI18n(w http.ResponseWriter, r *http.Request, p i18nPayload) {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	// revalidated on every load (cheap: a 304), so an updated build is picked up at once
	h.Set("Cache-Control", "no-cache")
	h.Set("ETag", p.etag)
	h.Add("Vary", "Accept-Encoding")
	h.Add("Vary", "Accept-Language")
	if strings.Contains(r.Header.Get("If-None-Match"), p.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	body := p.body
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		h.Set("Content-Encoding", "gzip")
		body = p.gz
	}
	h.Set("Content-Length", fmt.Sprint(len(body)))
	_, _ = w.Write(body)
}

func uiLocales() []string { return uiLocalesFS(locales.UIFS) }

func uiLocalesFS(fsys fs.FS) []string {
	entries, _ := fs.ReadDir(fsys, "ui")
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			out = append(out, strings.TrimSuffix(e.Name(), ".json"))
		}
	}
	sort.Strings(out)
	return out
}

// uiLang resolves against the UI bundles themselves. The bot message catalog and the console can
// grow independently, so a newly added locales/ui/<tag>.json does not require a Go allowlist edit.
func (s *Server) uiLang(r *http.Request) string { return s.uiLangFS(r, locales.UIFS) }

func (s *Server) uiLangFS(r *http.Request, fsys fs.FS) string {
	available := uiLocalesFS(fsys)
	if requested := r.URL.Query().Get("lang"); requested != "" {
		return matchUILocale(requested, available)
	}
	for _, part := range strings.Split(r.Header.Get("Accept-Language"), ",") {
		if tag := strings.TrimSpace(strings.Split(part, ";")[0]); tag != "" {
			if matched := findUILocale(tag, available); matched != "" {
				return matched
			}
		}
	}
	return matchUILocale(s.svc.Doc().I18n.DefaultLocale, available)
}

func findUILocale(requested string, available []string) string {
	requested = strings.ReplaceAll(strings.TrimSpace(requested), "_", "-")
	for _, locale := range available {
		if strings.EqualFold(locale, requested) {
			return locale
		}
	}
	if base := i18n.Normalize(requested); base != "" && !strings.EqualFold(base, requested) {
		for _, locale := range available {
			if strings.EqualFold(locale, base) {
				return locale
			}
		}
	}
	return ""
}

func matchUILocale(requested string, available []string) string {
	if locale := findUILocale(requested, available); locale != "" {
		return locale
	}
	for _, locale := range available {
		if locale == "en" {
			return locale
		}
	}
	if len(available) > 0 {
		return available[0]
	}
	return "en"
}

func (s *Server) apiCatalog(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.requireAPI(w, r, false); !ok {
		return
	}
	list := []map[string]any{}
	for _, d := range catalog.All() {
		list = append(list, map[string]any{"id": d.ID, "cli": d.CLI, "route": d.GUIRoute, "availability": d.Availability,
			"effects": d.Effects, "reason": d.DeferredReason})
	}
	writeJSON(w, http.StatusOK, map[string]any{"actions": list})
}

var safeName = regexp.MustCompile(`[^A-Za-z0-9._\-가-힣]+`)

// apiUpload stores one file under <data-root>/imports for imports.start. The console never reads it
// back; only the server-side path is returned.
func (s *Server) apiUpload(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.requireAPI(w, r, true); !ok {
		return
	}
	actionID, subdir := "imports.start", "imports"
	if r.URL.Query().Get("purpose") == "attachment" {
		actionID, subdir = "channels.attachments.add", "rag_staging"
	}
	svc, err := s.target(r, actionID)
	if err != nil {
		apiError(w, http.StatusServiceUnavailable, "IO", "practice instance: "+err.Error())
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, corpus.MaxImportBytes+(1<<20)) // bounded multipart overhead
	if r.ContentLength > corpus.MaxImportBytes+(1<<20) {
		apiError(w, http.StatusRequestEntityTooLarge, "INVALID_INPUT", svc.Msg(s.langFor(r), "imports.upload_limit"))
		return
	}
	mr, err := r.MultipartReader()
	if err != nil {
		apiError(w, http.StatusBadRequest, "INVALID_INPUT", "multipart upload expected")
		return
	}
	part, err := mr.NextPart()
	if err != nil || part.FileName() == "" {
		apiError(w, http.StatusBadRequest, "INVALID_INPUT", "file required")
		return
	}
	defer part.Close()
	name := safeName.ReplaceAllString(filepath.Base(part.FileName()), "_")
	if name == "" || name == "." || name == ".." {
		name = "upload"
	}
	dir := filepath.Join(svc.DataRoot, subdir)
	if err := fsutil.PrivateDir(dir); err != nil {
		apiError(w, http.StatusInternalServerError, "IO", "cannot create imports folder")
		return
	}
	target := filepath.Join(dir, s.now().UTC().Format("20060102T150405")+"-"+name)
	f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "IO", "cannot store file")
		return
	}
	if err := fsutil.PrivateFile(target); err != nil {
		_ = f.Close()
		_ = os.Remove(target)
		apiError(w, http.StatusInternalServerError, "IO", "cannot protect uploaded file")
		return
	}
	n, err := io.Copy(f, io.LimitReader(&uploadReader{ctx: r.Context(), r: part}, corpus.MaxImportBytes+1))
	if err == nil && n > corpus.MaxImportBytes {
		err = corpus.ErrImportTooLarge
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(target)
		apiError(w, http.StatusRequestEntityTooLarge, "INVALID_INPUT", svc.Msg(s.langFor(r), "imports.upload_limit"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": target, "name": name, "size": n})
}

type uploadReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *uploadReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func (s *Server) exportConfig(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.requireAPI(w, r, false); !ok {
		return
	}
	res := s.svc.Execute(r.Context(), action.Request{Action: "config.show", Locale: s.lang(r), RequestID: "web"})
	yaml, _ := res.Output["yaml"].(string)
	if !res.OK || yaml == "" {
		apiError(w, http.StatusBadGateway, "IO", "config unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="masamong-config.yaml"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, yaml)
}

// events streams change notifications (Server-Sent Events). Only kind/ref/revision cross the wire;
// the app refetches what changed.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	id, _, ok := s.requireAPI(w, r, false)
	if !ok {
		return
	}
	if _, ok := w.(http.Flusher); !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	src := s.svc
	if r.URL.Query().Get("practice") == "1" {
		if p, err := s.practice.get(r.Context(), s.svc, s.langFor(r)); err == nil {
			src = p
		}
	}
	// The stream context is cancelled when the client goes away or when a newer stream
	// evicts this one. A write deadline is set only around each write: one deadline for
	// the whole stream would cut a healthy tab, and a deadline left running expires
	// while the handler waits for the next event (HTTP/2 then refuses to extend it).
	streamCtx, cancel := context.WithCancel(r.Context())
	defer cancel()
	rc := http.NewResponseController(w)
	end := s.beginEventStream(id, cancel, func() { _ = rc.SetWriteDeadline(time.Now()) })
	defer end()
	ch, unsub := src.Subscribe()
	defer unsub()
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	// Eviction cancels streamCtx and sets the write deadline to now. Check before
	// every write: if this stream is already done, a ping that won the select must
	// not replace that deadline with a fresh timeout.
	write := func(payload string) error {
		if streamCtx.Err() != nil {
			return streamCtx.Err()
		}
		return writeEvent(rc, w, payload, s.eventWriteTimeout())
	}
	if err := write(fmt.Sprintf("retry: 3000\n\nevent: hello\ndata: {\"revision\":%d}\n\n", src.Revision())); err != nil {
		return
	}
	ping := time.NewTicker(s.ping)
	defer ping.Stop()
	for {
		select {
		case <-streamCtx.Done():
			return
		case <-ping.C:
			if _, ok := s.sessions.get(id, s.now()); !ok {
				return // signed out or expired: the stream ends with the session
			}
			if err := write(": ping\n\n"); err != nil {
				return
			}
		case ev, ok := <-ch:
			if !ok {
				return
			}
			data, _ := json.Marshal(map[string]any{"kind": ev.Kind, "ref": ev.Ref, "revision": ev.Revision})
			if err := write(fmt.Sprintf("id: %d\nevent: change\ndata: %s\n\n", ev.ID, data)); err != nil {
				return
			}
		}
	}
}

// writeEvent bounds one SSE write. The deadline is cleared afterwards so the idle
// gap until the next event does not fire it.
func writeEvent(rc *http.ResponseController, w http.ResponseWriter, payload string, timeout time.Duration) error {
	if err := rc.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
		return err
	}
	if _, err := io.WriteString(w, payload); err != nil {
		return err
	}
	if err := rc.Flush(); err != nil {
		return err
	}
	return rc.SetWriteDeadline(time.Time{})
}
