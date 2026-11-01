//go:build !noweb

package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"

	"masamong/internal/application"
)

// practiceHeader switches one request to the practice instance (application.OpenPractice). The console
// sends it while practice mode is on; everything else about the request is unchanged.
const practiceHeader = "X-Masamong-Practice"

// alwaysReal are Actions about the admin, not the instance: they always use the real instance.
var alwaysReal = map[string]bool{"admin.prefs": true, "admin.prefs.set": true}

type practiceHolder struct {
	mu  sync.Mutex
	svc *application.Service
}

func (p *practiceHolder) get(ctx context.Context, real *application.Service, lang string) (*application.Service, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.svc != nil {
		return p.svc, nil
	}
	svc, err := application.OpenPractice(context.WithoutCancel(ctx), real.DataRoot, lang, real.Log)
	if err != nil {
		return nil, err
	}
	p.svc = svc
	return svc, nil
}

func (p *practiceHolder) status() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	return map[string]any{"open": p.svc != nil}
}

func (p *practiceHolder) reset(real *application.Service) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.svc != nil {
		_ = p.svc.Close()
		p.svc = nil
	}
	return application.ResetPractice(real.DataRoot)
}

// target is the instance a request acts on.
func (s *Server) target(r *http.Request, actionID string) (*application.Service, error) {
	if r.Header.Get(practiceHeader) != "1" || alwaysReal[actionID] {
		return s.svc, nil
	}
	return s.practice.get(r.Context(), s.svc, s.langFor(r))
}

// apiPractice opens (GET/POST {"do":"open"}) or resets (POST {"do":"reset"}) the practice instance.
func (s *Server) apiPractice(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.requireAPI(w, r, r.Method == http.MethodPost); !ok {
		return
	}
	var body struct {
		Do string `json:"do"`
	}
	if r.Method == http.MethodPost {
		_ = json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&body)
	}
	switch body.Do {
	case "reset":
		if err := s.practice.reset(s.svc); err != nil {
			apiError(w, http.StatusInternalServerError, "IO", err.Error())
			return
		}
		fallthrough
	case "open":
		p, err := s.practice.get(r.Context(), s.svc, s.langFor(r))
		if err != nil {
			apiError(w, http.StatusInternalServerError, "IO", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "open": true, "revision": p.Revision(), "instance": p.Doc().Instance.ID,
			"guild_id": application.PracticeGuild, "channel_id": application.PracticeChannel})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": s.practice.status(),
		"guild_id": application.PracticeGuild, "channel_id": application.PracticeChannel})
}

// closePractice releases the practice instance with the server.
func (s *Server) closePractice() {
	s.practice.mu.Lock()
	defer s.practice.mu.Unlock()
	if s.practice.svc != nil {
		_ = s.practice.svc.Close()
		s.practice.svc = nil
	}
}
