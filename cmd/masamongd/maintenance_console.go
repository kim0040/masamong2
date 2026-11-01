package main

import (
	"bufio"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/config"
	"masamong/internal/i18n"
	"masamong/internal/transport/web"
)

func sameDataRoot(a, b string) bool {
	return a == b || (runtime.GOOS == "windows" && strings.EqualFold(a, b))
}

// maintenanceConsoleBind prefers the re-exec address, then the saved console bind. The latter stays
// available when a journal is damaged but config.yaml itself is still readable.
func maintenanceConsoleBind(root, requested string) string {
	if strings.HasPrefix(requested, "127.0.0.1:") {
		return requested
	}
	if raw, err := os.ReadFile(filepath.Join(root, "config.yaml")); err == nil {
		if doc, err := config.Parse(raw); err == nil && strings.HasPrefix(doc.Admin.Web.Bind, "127.0.0.1:") {
			return doc.Admin.Web.Bind
		}
	}
	return "127.0.0.1:8787"
}

// maintenanceHandler is the small console served while the daemon is stopped for maintenance. It never
// opens the SQL store. The normal clients work against it:
//
//	GET /api/health            {"ok":true,"maintenance":true,"ready":false}: the launcher, the tray and
//	                           `masamong status` see a daemon in a distinct maintenance state, not "not ready"
//	GET /                      a plain HTML page (no script) for a browser: what happened, that the bot is
//	                           stopped, and exactly which commands can work (JSON for clients that ask for it)
//	POST /api/maintenance/stop stops this daemon (`masamong stop`, the tray's Quit); needs the admin token
//	POST /api/action           runtime.status and runtime.doctor only
//
// Every request keeps the loopback Host check and every write the same-origin check.
func maintenanceHandler(root string, stop func()) http.Handler {
	mux := http.NewServeMux()
	catalog := i18n.New()
	guard := func(w http.ResponseWriter, r *http.Request) bool {
		if !web.LoopbackHost(r.Host) {
			http.Error(w, "loopback host required", http.StatusMisdirectedRequest)
			return false
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !web.SameOrigin(r) {
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return false
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		return true
	}
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(v)
	}
	state := func(r *http.Request) map[string]any {
		st := application.MaintenanceStatus(root)
		st["maintenance"], st["ready"] = true, false
		st["version"] = application.Version
		// Raw recovery errors can contain paths, URLs or credential-bearing configuration.
		st["error"] = catalog.T(application.AcceptedLocale(r.Header.Get("Accept-Language")), "maintenance.recovery_required")
		return st
	}
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		if guard(w, r) {
			writeJSON(w, map[string]any{"ok": true, "maintenance": true, "ready": false, "version": application.Version})
		}
	})
	mux.HandleFunc("POST /api/maintenance/stop", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r) {
			return
		}
		var in struct {
			Token    string `json:"token"`
			DataRoot string `json:"data_root"`
			PID      int    `json:"pid"`
		}
		want, err := web.EnsureToken(root)
		if json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in) != nil || err != nil || subtle.ConstantTimeCompare([]byte(in.Token), []byte(want)) != 1 {
			http.Error(w, "sign-in refused", http.StatusUnauthorized)
			return
		}
		// A supervisor additionally pins its own child and folder, so a stale port
		// cannot stop another maintenance instance with a copied authentication file.
		if in.DataRoot != "" {
			got, gotErr := filepath.Abs(in.DataRoot)
			wantRoot, wantErr := filepath.Abs(root)
			if gotErr != nil || wantErr != nil || !sameDataRoot(got, wantRoot) || in.PID != os.Getpid() {
				http.Error(w, "instance identity mismatch", http.StatusConflict)
				return
			}
		}
		writeJSON(w, map[string]any{"ok": true, "stopping": true})
		if stop != nil {
			// let the reply reach the client before the listener closes
			go func() { time.Sleep(200 * time.Millisecond); stop() }()
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r) {
			return
		}
		if r.URL.Path == "/api/action" {
			var req action.Request
			if err := json.NewDecoder(io.LimitReader(r.Body, 65536)).Decode(&req); err != nil {
				http.Error(w, "invalid action", http.StatusBadRequest)
				return
			}
			st := state(r)
			if req.Action == "runtime.status" || req.Action == "runtime.doctor" {
				writeJSON(w, action.OK(req, st))
				return
			}
			writeJSON(w, action.Fail(req, "IO", "generic.error", st["error"].(string), st))
			return
		}
		if r.URL.Path == "/" && strings.Contains(r.Header.Get("Accept"), "text/html") {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'")
			_, _ = io.WriteString(w, application.MaintenancePage(root, application.AcceptedLocale(r.Header.Get("Accept-Language"))))
			return
		}
		writeJSON(w, state(r))
	})
	return mux
}

// abandonUpdate is the operator's exit from a program-update record nothing else can settle (the
// programs were reinstalled, so neither retry nor rollback applies). It deletes nothing and asks first.
func abandonUpdate(yes bool) error {
	state := application.UpdateJournalState()
	if state == "" {
		return errors.New("there is no program update record to abandon")
	}
	if !yes {
		fmt.Fprintln(os.Stderr, "This keeps every program file exactly as it is (the installed programs, the set-aside old ones and any")
		fmt.Fprintln(os.Stderr, "staged new ones) and only moves the update record aside. Use it after reinstalling Masamong.")
		fmt.Fprint(os.Stderr, "Type abandon to continue: ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if strings.TrimSpace(line) != "abandon" {
			return errors.New("not confirmed; nothing was changed (use --yes to confirm without asking)")
		}
	}
	kept, err := application.AbandonPendingUpdate()
	if err == nil {
		fmt.Fprintln(os.Stderr, "update record kept as", kept)
	}
	return err
}
