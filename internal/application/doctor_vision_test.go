package application_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"masamong/internal/application"
	"masamong/internal/config"
)

// runtime.doctor reports the optional image-recognition role: unset is "off" (not a problem), set needs
// its provider's key, and a model that refused pictures lately shows as degraded, each with a reason a
// person can read.
func TestDoctorReportsTheVisionRole(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	svc := setupSvc(t)
	vision := func() map[string]any {
		t.Helper()
		res := exec(svc, "runtime.doctor", nil)
		if !res.OK {
			t.Fatalf("doctor: %+v", res.Error)
		}
		v, ok := res.Output["features"].(map[string]any)["vision"].(map[string]any)
		if !ok {
			t.Fatalf("no vision item in %v", res.Output["features"])
		}
		if reason, _ := v["reason"].(string); reason != "" && v["reason_text"] == nil {
			t.Fatalf("reason %q has no text: %v", reason, v)
		}
		return v
	}
	want := func(step, status, reason string) {
		t.Helper()
		if v := vision(); v["status"] != status || v["reason"] != reason {
			t.Fatalf("%s: %v, want %s/%s", step, v, status, reason)
		}
	}

	want("role unset", "off", "vision_unset")
	for _, a := range exec(svc, "runtime.doctor", nil).Output["attention"].([]map[string]any) {
		if a["item"] == "vision" {
			t.Fatalf("an unset optional role needs no attention: %v", a)
		}
	}

	applyDoc(t, svc, func(d *config.Document) {
		d.Providers["eyes"] = config.Provider{Kind: config.KindOpenAI, BaseURL: srv.URL, CredentialRef: "env:FAKE_EYES_KEY"}
		d.Models["vision"] = config.Model{Provider: "eyes", Model: "some/vision-model"}
	})
	want("provider without its key", "unconfigured", "provider_key_missing")

	t.Setenv("FAKE_EYES_KEY", "k")
	want("provider with its key", "ready", "configured")

	// the provider failed on the wire a moment ago: degraded while it cools down
	application.MarkVisionCoolingForTest(svc, "eyes", "some/vision-model")
	want("provider in its outage cool-down", "degraded", "vision_cooling")

	application.MarkVisionRefusedForTest(svc, "eyes", "some/vision-model")
	want("model that refused pictures", "degraded", "vision_unsupported")

	// the chat switch decides before anything else: the role may be set and fine, pictures are still not read
	applyDoc(t, svc, func(d *config.Document) { d.Chat.Vision = "off" })
	want("chat vision switched off", "off", "vision_off")
	for _, a := range exec(svc, "runtime.doctor", nil).Output["attention"].([]map[string]any) {
		if a["item"] == "vision" {
			t.Fatalf("a switch the operator turned off needs no attention: %v", a)
		}
	}
	applyDoc(t, svc, func(d *config.Document) { d.Chat.Vision = "" })

	// a credential that cannot be read at all is a configuration problem, not a missing key
	applyDoc(t, svc, func(d *config.Document) {
		d.Providers["eyes"] = config.Provider{Kind: config.KindOpenAI, BaseURL: srv.URL, CredentialRef: "file:" + t.TempDir()}
	})
	want("unreadable credential", "unconfigured", "needs_vision_model")
}
