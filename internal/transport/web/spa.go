//go:build !noweb

package web

import (
	"bytes"
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"

	"masamong/internal/application"
)

// The console is built from ui/ (Svelte + Vite) into ui/dist, which is committed so `go build` never
// needs Node. Hashed files under /assets are immutable; index.html is revalidated on every load.

//go:embed all:ui/dist
var dist embed.FS

//go:embed ui/src/routes.json
var routesJSON []byte

func appVersion() string { return application.Version }

func distFS() fs.FS {
	sub, _ := fs.Sub(dist, "ui/dist")
	return sub
}

func spaRoutes() []string {
	var r struct {
		Routes []string `json:"routes"`
	}
	_ = json.Unmarshal(routesJSON, &r)
	return r.Routes
}

var bootTime = time.Now()

// spa serves a built file when one exists, else index.html (client-side routing, including the
// app's own not-found page).
func (s *Server) spa(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		apiError(w, http.StatusNotFound, "NOT_FOUND", "unknown endpoint")
		return
	}
	fsys := distFS()
	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if name != "" && name != "index.html" {
		if st, err := fs.Stat(fsys, name); err == nil && !st.IsDir() {
			if strings.HasPrefix(name, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else if strings.HasPrefix(name, "brand/") && r.URL.Query().Get("v") != "" {
				// the console asks for brand files with ?v=<hash of the file> (virtual:brand at build
				// time), so a replaced picture has a new address and the old one can be kept forever
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else if strings.HasPrefix(name, "brand/") {
				// Pose files keep their names, so a day-long cache hides a replaced picture.
				w.Header().Set("Cache-Control", "public, max-age=300")
			} else {
				w.Header().Set("Cache-Control", "public, max-age=86400")
			}
			serveMaybeCompressed(w, r, fsys, name)
			return
		}
	}
	raw, err := fs.ReadFile(fsys, "index.html")
	if err != nil {
		http.Error(w, "console not built", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	http.ServeContent(w, r, "index.html", bootTime, bytes.NewReader(raw))
}

// serveMaybeCompressed serves name.br or name.gz (precompressed at build time) when the browser accepts it.
func serveMaybeCompressed(w http.ResponseWriter, r *http.Request, fsys fs.FS, name string) {
	ae := r.Header.Get("Accept-Encoding")
	for _, enc := range []struct{ ext, name string }{{".br", "br"}, {".gz", "gzip"}} {
		if !strings.Contains(ae, enc.name) {
			continue
		}
		raw, err := fs.ReadFile(fsys, name+enc.ext)
		if err != nil {
			continue
		}
		if ct := contentType(name); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.Header().Set("Content-Encoding", enc.name)
		w.Header().Add("Vary", "Accept-Encoding")
		http.ServeContent(w, r, "", bootTime, bytes.NewReader(raw))
		return
	}
	http.ServeFileFS(w, r, fsys, name)
}

func contentType(name string) string {
	switch path.Ext(name) {
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".json", ".webmanifest":
		return "application/json"
	case ".woff2":
		return "font/woff2"
	}
	return ""
}
