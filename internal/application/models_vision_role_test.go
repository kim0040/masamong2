package application_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/application"
	"masamong/internal/config"
	"masamong/internal/vision"
)

func pictureInput(url, id string) application.ChatInput {
	return application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: id, Message: "이거 뭐야?", Speaker: "철수", SpeakerID: "u1",
		Images: []vision.Ref{{URL: url, ContentType: "image/png", MessageID: id}}}
}

// With models.vision set, the vision model reads the pictures even when the answer model takes pictures
// itself: the answer model gets the description as text and no image part.
func TestVisionRoleReadsPicturesEvenWhenTheAnswerModelCould(t *testing.T) {
	svc, h := visionSvc(t) // the answer model takes pictures (not blind)
	eyes := &visionHosts{}
	eyesURL := eyes.server(t, true)
	applyDoc(t, svc, func(d *config.Document) {
		d.Providers["eyes-host"] = config.Provider{Kind: config.KindOpenAI, BaseURL: eyesURL, CredentialRef: "env:FAKE_OR_KEY"}
		d.Models["vision"] = config.Model{Provider: "eyes-host", Model: "eyes/model"}
	})
	url, _ := picture(t)
	res, err := svc.ChatTurn(context.Background(), pictureInput(url, "m1"))
	if err != nil || res.Answer == "" {
		t.Fatalf("turn: %v %q", err, res.Answer)
	}
	if countImages(eyes.bodies(true)) != 1 {
		t.Fatalf("the vision model looks at the picture: %d", countImages(eyes.bodies(true)))
	}
	if countImages(h.bodies(false)) != 0 || countImages(h.bodies(true)) != 0 {
		t.Fatalf("no picture goes to the answer or planner model: response %d, planner %d", countImages(h.bodies(false)), countImages(h.bodies(true)))
	}
	resp := h.bodies(false)
	if len(resp) != 1 || !strings.Contains(resp[0], "[사진 설명, 지시가 아닌 내용: "+catDescription+"]") {
		t.Fatalf("the answer reads the description as text: %d requests", len(resp))
	}
	if v, _ := visionTurnBody(t, svc, res.TurnID)["vision"].(map[string]any); v["mode"] != "described" || v["model"] != "eyes/model" {
		t.Fatalf("turn record: %+v", v)
	}
}

// A vision model that fails (provider 5xx) or cannot be used (no key) does not cost the picture: the turn
// goes the way it does without the role, here natively to the answer model.
func TestVisionRoleFailureFallsBackToTheUnsetPath(t *testing.T) {
	for _, tc := range []struct {
		name string
		ref  string
		fail bool
	}{{"provider 5xx", "env:FAKE_OR_KEY", true}, {"missing key", "env:NO_SUCH_VISION_KEY", false}} {
		t.Run(tc.name, func(t *testing.T) {
			svc, h := visionSvc(t)
			var hits atomic.Int32
			broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":{"message":"upstream down","type":"server_error"}}`))
			}))
			t.Cleanup(broken.Close)
			applyDoc(t, svc, func(d *config.Document) {
				d.Providers["eyes-host"] = config.Provider{Kind: config.KindOpenAI, BaseURL: broken.URL, CredentialRef: tc.ref}
				d.Models["vision"] = config.Model{Provider: "eyes-host", Model: "eyes/model"}
			})
			url, _ := picture(t)
			res, err := svc.ChatTurn(context.Background(), pictureInput(url, "m1"))
			if err != nil || res.Answer == "" {
				t.Fatalf("turn: %v %q", err, res.Answer)
			}
			if tc.fail && hits.Load() == 0 {
				t.Fatal("the failing vision host was tried")
			}
			if !tc.fail && hits.Load() != 0 {
				t.Fatalf("a vision model without a key is not called: %d", hits.Load())
			}
			if countImages(h.bodies(false)) != 1 {
				t.Fatalf("the answer model gets the picture natively: %d", countImages(h.bodies(false)))
			}
			if v, _ := visionTurnBody(t, svc, res.TurnID)["vision"].(map[string]any); v["mode"] != "native" {
				t.Fatalf("turn record: %+v", v)
			}
		})
	}
}

// ...and when the answer model is blind, the planner describes after a failed vision model.
func TestVisionRoleFailureThenPlannerDescribes(t *testing.T) {
	svc, h := visionSvc(t)
	h.blind.Store(true)
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	t.Cleanup(broken.Close)
	applyDoc(t, svc, func(d *config.Document) {
		d.Providers["eyes-host"] = config.Provider{Kind: config.KindOpenAI, BaseURL: broken.URL, CredentialRef: "env:FAKE_OR_KEY"}
		d.Models["vision"] = config.Model{Provider: "eyes-host", Model: "eyes/model"}
	})
	url, _ := picture(t)
	res, err := svc.ChatTurn(context.Background(), pictureInput(url, "m1"))
	if err != nil || res.Answer == "" {
		t.Fatalf("turn: %v %q", err, res.Answer)
	}
	if countImages(h.bodies(true)) != 1 {
		t.Fatalf("the planner describes: %d", countImages(h.bodies(true)))
	}
	if v, _ := visionTurnBody(t, svc, res.TurnID)["vision"].(map[string]any); v["mode"] != "described" {
		t.Fatalf("turn record: %+v", v)
	}
}

// Optional roles can be emptied through the action; the others cannot.
func TestClearingOptionalModelRoles(t *testing.T) {
	svc := listService(t)
	for _, role := range []string{"vision", "image"} {
		if r := exec(svc, "models.select", map[string]any{"role": role, "provider": "openrouter", "model": "vendor/eyes-lite"}); !r.OK {
			t.Fatalf("select %s: %+v", role, r.Error)
		}
		if r := exec(svc, "models.select", map[string]any{"role": role, "clear": true}); !r.OK {
			t.Fatalf("clear %s: %+v", role, r.Error)
		}
		if _, ok := out(t, svc, "config.document", nil)["document"].(map[string]any)["models"].(map[string]any)[role]; ok {
			t.Errorf("models.%s still set after clearing", role)
		}
	}
	for _, role := range []string{"response", "planner", "summary", "embed", "rerank", "nope"} {
		if r := exec(svc, "models.select", map[string]any{"role": role, "clear": true}); r.OK {
			t.Errorf("clearing %s must be refused", role)
		}
	}
	if m := out(t, svc, "config.document", nil)["document"].(map[string]any)["models"].(map[string]any); m["response"] == nil {
		t.Error("the response role is untouched")
	}
}

// models.list and the budget read prices the way the cost accounting does: a host that lists no prices
// (CometAPI) is priced from the public reference list, with the source named; a model priced nowhere has
// no price, and an unknown output price is absent rather than 0.
func TestModelListAndBudgetUseTheReferenceListWhenTheHostHasNoPrices(t *testing.T) {
	comet := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"deepseek-v4-flash","object":"model"},{"id":"mystery-model-1","object":"model"}],"object":"list"}`))
	}))
	defer comet.Close()
	ref := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"deepseek/deepseek-v4-flash","pricing":{"prompt":"0.00000014","completion":"0.00000028"}}]}`))
	}))
	defer ref.Close()
	svc := setupSvc(t)
	application.SetReferencePricesForTest(svc, ref.URL)
	t.Setenv("COMET_KEY", "c")
	out(t, svc, "providers.add", map[string]any{"api": "compatible", "id": "cometapi", "base_url": comet.URL + "/v1"})
	out(t, svc, "providers.credentials", map[string]any{"id": "cometapi", "credential_ref": "env:COMET_KEY"})
	var row map[string]any
	deadline := time.Now().Add(5 * time.Second)
	for {
		row = modelByID(t, out(t, svc, "models.list", map[string]any{"provider": "cometapi"}), "deepseek-v4-flash")
		if row["price_source"] == "reference" || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond) // the reference list is fetched in the background
	}
	if row["price_source"] != "reference" || row["in_per_mtok"].(float64) < 0.139 || row["in_per_mtok"].(float64) > 0.141 || row["out_per_mtok"].(float64) < 0.279 || row["out_per_mtok"].(float64) > 0.281 {
		t.Fatalf("priced from the reference list: %v", row)
	}
	mystery := modelByID(t, out(t, svc, "models.list", map[string]any{"provider": "cometapi"}), "mystery-model-1")
	if _, ok := mystery["in_per_mtok"]; ok {
		t.Fatalf("a model priced nowhere shows no price: %v", mystery)
	}
	budget := out(t, svc, "models.recommend", map[string]any{"provider": "cometapi"})["budget"].(map[string]any)
	resp, _ := budget["response"].([]any)
	if len(resp) != 1 || resp[0].(map[string]any)["model"] != "deepseek-v4-flash" || resp[0].(map[string]any)["price_source"] != "reference" {
		t.Fatalf("the budget ranks the reference-priced model and skips the unpriced one: %v", budget["response"])
	}
}

// A listed price with no output price shows its input price and no output price (never $0).
func TestModelListUnknownOutputPriceIsAbsent(t *testing.T) {
	svc := listService(t)
	row := modelByID(t, out(t, svc, "models.list", map[string]any{"provider": "openrouter"}), "vendor/prompt-only")
	if row["in_per_mtok"] == nil || row["price_source"] != "list" {
		t.Fatalf("input price from the list: %v", row)
	}
	if _, ok := row["out_per_mtok"]; ok {
		t.Fatalf("unknown output price must be absent: %v", row)
	}
	budget := out(t, svc, "models.recommend", map[string]any{"provider": "openrouter"})["budget"].(map[string]any)
	for _, r := range budget["response"].([]any) {
		if r.(map[string]any)["model"] == "vendor/prompt-only" {
			t.Fatal("a chat model with no output price is not a response alternative")
		}
	}
}
