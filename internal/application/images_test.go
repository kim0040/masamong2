package application_test

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"masamong/internal/action"
	"masamong/internal/config"
)

func imageServer(t *testing.T, calls *atomic.Int32) string {
	png := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("\x89PNG fake"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["modalities"] == nil {
			t.Errorf("image request without modalities: %v", req)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "",
			"images": []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": png}}}}}}, "usage": map[string]any{"cost": 0.03}})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestImagesGenerateSavesThePicture(t *testing.T) {
	var calls atomic.Int32
	url := imageServer(t, &calls)
	svc := setupSvc(t)
	serveAnyServer(t, svc)
	u := &action.Invoker{Via: "command", GuildID: "g", ChannelID: "c", UserID: "u1"}
	if r := as(svc, "images.generate", map[string]any{"prompt": "고양이"}, u); !r.OK || r.Output["status"] != "unconfigured" {
		t.Fatalf("without an image model: %+v", r)
	}
	t.Setenv("FAKE_IMG_KEY", "k")
	applyDoc(t, svc, func(d *config.Document) {
		d.Providers["img"] = config.Provider{Kind: config.KindOpenAI, BaseURL: url, CredentialRef: "env:FAKE_IMG_KEY"}
		d.Models["image"] = config.Model{Provider: "img", Model: "google/gemini-image"}
	})
	if r := as(svc, "images.generate", map[string]any{"prompt": "벗은 사람"}, u); r.OK || r.Error.MessageKey != "image.blocked" || calls.Load() != 0 {
		t.Fatalf("blocked words are refused before any call: %+v", r)
	}
	if r := as(svc, "images.generate", map[string]any{"prompt": "a skilled painter"}, u); !r.OK {
		t.Fatalf("a Latin blocked word only matches whole (skill is not kill): %+v", r)
	}
	r := as(svc, "images.generate", map[string]any{"prompt": "우주복 입은 햄스터", "aspect_ratio": "16:9"}, u)
	if !r.OK || r.Output["status"] != "ok" {
		t.Fatalf("%+v", r)
	}
	f, mime, err := svc.ImageFile(r.Output["image_id"].(string))
	if err != nil || mime != "image/png" {
		t.Fatalf("file: %v %s", err, mime)
	}
	data, _ := io.ReadAll(f)
	f.Close()
	if string(data) != "\x89PNG fake" {
		t.Fatalf("saved bytes %q", data)
	}
	if _, _, err := svc.ImageFile("../../etc/passwd"); err == nil {
		t.Fatal("ids are validated")
	}
	if r := as(svc, "images.generate", map[string]any{"prompt": "x", "aspect_ratio": "7:1"}, u); r.OK {
		t.Fatal("bad aspect ratio")
	}
}
