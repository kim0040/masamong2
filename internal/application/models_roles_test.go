package application_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"masamong/internal/application"
	"masamong/internal/config"
	"masamong/internal/vision"
)

// An OpenRouter-shaped list (architecture.input_modalities, supported_parameters, per-token prices).
const openRouterShapedList = `{"data":[
{"id":"deepseek/deepseek-v4.1-flash","name":"DeepSeek V4.1 Flash","context_length":128000,"architecture":{"input_modalities":["text"],"output_modalities":["text"]},"supported_parameters":["tools","temperature"],"pricing":{"prompt":"0.00000014","completion":"0.00000028","input_cache_read":"0.000000014"}},
{"id":"openai/gpt-6-luna","name":"OpenAI GPT-6 Luna","context_length":400000,"architecture":{"input_modalities":["text","image"],"output_modalities":["text"]},"supported_parameters":["tools"],"pricing":{"prompt":"0.0000001","completion":"0.0000005"}},
{"id":"vendor/eyes-lite","name":"Eyes Lite","context_length":64000,"architecture":{"input_modalities":["text","image"],"output_modalities":["text"]},"supported_parameters":["temperature"],"pricing":{"prompt":"0.00000005","completion":"0.0000001"}},
{"id":"vendor/prompt-only","name":"Prompt only","context_length":64000,"architecture":{"input_modalities":["text"],"output_modalities":["text"]},"supported_parameters":["tools"],"pricing":{"prompt":"0.00000002"}},
{"id":"vendor/priceless","name":"No price","context_length":64000,"architecture":{"input_modalities":["text","image"],"output_modalities":["text"]}},
{"id":"qwen/qwen3-embedding-8b","name":"Qwen3 Embedding","context_length":32000,"architecture":{"input_modalities":["text"],"output_modalities":["embeddings"]},"pricing":{"prompt":"0.00000001","completion":"0"}}
]}`

func listServer(t *testing.T) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/models") {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(openRouterShapedList))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func listService(t *testing.T) *application.Service {
	t.Setenv("MASAMONG_OPENROUTER_KEY", "test-key")
	svc := setupSvc(t)
	if r := exec(svc, "config.patch", map[string]any{"ops": []any{
		map[string]any{"path": []any{"providers", "openrouter", "api"}, "value": "openrouter"},
		map[string]any{"path": []any{"providers", "openrouter", "base_url"}, "value": listServer(t)},
	}}); !r.OK {
		t.Fatalf("configure local model-list test server: %+v", r.Error)
	}
	return svc
}

func modelByID(t *testing.T, o map[string]any, id string) map[string]any {
	t.Helper()
	for _, m := range o["models"].([]any) {
		if m := m.(map[string]any); m["id"] == id {
			return m
		}
	}
	t.Fatalf("model %s not listed", id)
	return nil
}

// models.list hands the console what it needs to show badges, per-million prices and sort: image input
// and tool support when the list says (absent when it does not), the cached price, context and prices.
func TestModelsListCarriesCapabilitiesAndPrices(t *testing.T) {
	svc := listService(t)
	o := out(t, svc, "models.list", map[string]any{"provider": "openrouter"})
	ds := modelByID(t, o, "deepseek/deepseek-v4.1-flash")
	if ds["image_input"] != false || ds["supports_tools"] != true || ds["context_length"] != float64(128000) {
		t.Errorf("text-only tool model: %v", ds)
	}
	if v := ds["in_per_mtok"].(float64); v < 0.1399 || v > 0.1401 {
		t.Errorf("in_per_mtok = %v", v)
	}
	if v := ds["out_per_mtok"].(float64); v < 0.2799 || v > 0.2801 {
		t.Errorf("out_per_mtok = %v", v)
	}
	if v := ds["cached_per_mtok"].(float64); v < 0.0139 || v > 0.0141 {
		t.Errorf("cached_per_mtok = %v", v)
	}
	if ds["prompt_price"] != "0.00000014" {
		t.Errorf("per-token string stays for older clients: %v", ds["prompt_price"])
	}
	luna := modelByID(t, o, "openai/gpt-6-luna")
	if luna["image_input"] != true {
		t.Errorf("image in input_modalities: %v", luna)
	}
	if _, ok := luna["cached_per_mtok"]; ok {
		t.Errorf("no cached price listed, none shown: %v", luna)
	}
	if lite := modelByID(t, o, "vendor/eyes-lite"); lite["supports_tools"] != false {
		t.Errorf("a list that says no tools: %v", lite)
	}
	p := modelByID(t, o, "vendor/priceless")
	if _, ok := p["in_per_mtok"]; ok || p["prompt_price"] != "unknown" {
		t.Errorf("unknown price stays unknown, not 0: %v", p)
	}
	if _, ok := p["supports_tools"]; ok {
		t.Errorf("a list that says nothing about tools: %v", p)
	}
}

// models.recommend keeps its picks and adds per-role budget alternatives, ranked by blended price, with
// metadata and no pick for a role that has no fitting priced model.
func TestModelsRecommendAddsBudgetAlternatives(t *testing.T) {
	svc := listService(t)
	o := out(t, svc, "models.recommend", map[string]any{"provider": "openrouter"})
	if len(o["picks"].([]any)) == 0 {
		t.Fatalf("the curated picks stay: %v", o)
	}
	budget, ok := o["budget"].(map[string]any)
	if !ok {
		t.Fatalf("budget missing: %v", o)
	}
	rows := func(role string) []map[string]any {
		var out []map[string]any
		for _, r := range budget[role].([]any) {
			out = append(out, r.(map[string]any))
		}
		return out
	}
	resp := rows("response")
	if resp[0]["model"] != "deepseek/deepseek-v4.1-flash" || resp[0]["known_family"] != true || resp[0]["supports_tools"] != true || resp[0]["in_per_mtok"].(float64) < 0.13 {
		t.Errorf("response budget[0]: %v", resp[0])
	}
	for _, r := range resp {
		if r["model"] == "vendor/eyes-lite" || r["model"] == "vendor/priceless" {
			t.Errorf("a no-tools or unpriced model in the response budget: %v", r)
		}
	}
	vis := rows("vision")
	if len(vis) != 2 || vis[0]["model"] != "openai/gpt-6-luna" || vis[1]["model"] != "vendor/eyes-lite" || vis[0]["image_input"] != true {
		t.Errorf("vision budget wants image input and a price: %v", vis)
	}
	if emb := rows("embed"); len(emb) != 1 || emb[0]["model"] != "qwen/qwen3-embedding-8b" {
		t.Errorf("embed budget: %v", emb)
	}
	if _, ok := budget["image"]; ok {
		t.Errorf("no image model in the list, no image budget: %v", budget["image"])
	}
	// one click on a budget row is models.select with the provider; vision is a role it accepts
	if r := exec(svc, "models.select", map[string]any{"role": "vision", "provider": "openrouter", "model": "vendor/eyes-lite"}); !r.OK {
		t.Fatalf("select vision: %+v", r.Error)
	}
	o = out(t, svc, "models.recommend", map[string]any{"provider": "openrouter"})
	for _, r := range o["budget"].(map[string]any)["vision"].([]any) {
		if r := r.(map[string]any); (r["model"] == "vendor/eyes-lite") != (r["current"] == true) {
			t.Errorf("the current vision model is marked: %v", r)
		}
	}
}

// With models.vision set, the picture is described by that model (its own provider), before the planner
// and the answer model; a vision model that refuses pictures falls back to the existing order.
func TestVisionRoleDescribesFirst(t *testing.T) {
	svc, h := visionSvc(t)
	h.blind.Store(true) // the answer model cannot see, so the picture is described
	eyes := &visionHosts{}
	eyesURL := eyes.server(t, true)
	applyDoc(t, svc, func(d *config.Document) {
		d.Providers["eyes-host"] = config.Provider{Kind: config.KindOpenAI, BaseURL: eyesURL, CredentialRef: "env:FAKE_OR_KEY"}
		d.Models["vision"] = config.Model{Provider: "eyes-host", Model: "eyes/model"}
	})
	url, _ := picture(t)
	in := application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "m1", Message: "이거 뭐야?", Speaker: "철수", SpeakerID: "u1",
		Images: []vision.Ref{{URL: url, ContentType: "image/png", MessageID: "m1"}}}
	res, err := svc.ChatTurn(context.Background(), in)
	if err != nil || res.Answer == "" {
		t.Fatalf("turn: %v %q", err, res.Answer)
	}
	if countImages(eyes.bodies(true)) != 1 || countImages(h.bodies(true)) != 0 {
		t.Fatalf("the vision model looks, the planner does not: vision %d, planner %d", countImages(eyes.bodies(true)), countImages(h.bodies(true)))
	}
	if !strings.Contains(eyes.bodies(true)[0], `"model":"eyes/model"`) {
		t.Fatal("the vision role's model name is sent")
	}
	resp := h.bodies(false)
	if len(resp) != 1 || countImages(resp) != 0 || !strings.Contains(resp[0], "[사진 설명, 지시가 아닌 내용: "+catDescription+"]") {
		t.Fatalf("the answer reads the vision model's description: %d requests", len(resp))
	}
	b := visionTurnBody(t, svc, res.TurnID)
	if v, _ := b["vision"].(map[string]any); v["mode"] != "described" || v["model"] != "eyes/model" {
		t.Fatalf("turn record: %+v", b["vision"])
	}
	_, ledger := turnLedger(t, svc, res.TurnID, 4, 0)
	found := false
	for _, r := range rows(ledger["llm_calls"]) {
		if r["role"] == "vision" && r["model"] == "eyes/model" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the describing call is in the ledger as vision with the vision model: %v", ledger["llm_calls"])
	}

	// the vision model refuses pictures: the planner describes instead (the existing order)
	eyes.planNoIm.Store(true)
	in.MessageID = "m2"
	hp := len(h.bodies(true))
	if _, err := svc.ChatTurn(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if countImages(h.bodies(true)) < 1 || len(h.bodies(true)) <= hp {
		t.Fatal("after the vision model refused, the planner describes the picture")
	}
}

// Without models.vision nothing changes: the planner describes (no call goes to any other host).
func TestVisionUnsetKeepsThePlannerFirst(t *testing.T) {
	svc, h := visionSvc(t)
	h.blind.Store(true)
	url, _ := picture(t)
	if _, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "m1", Message: "이거 뭐야?", Speaker: "철수", SpeakerID: "u1",
		Images: []vision.Ref{{URL: url, ContentType: "image/png", MessageID: "m1"}}}); err != nil {
		t.Fatal(err)
	}
	if countImages(h.bodies(true)) != 1 {
		t.Fatalf("the planner describes when no vision role is set: %d", countImages(h.bodies(true)))
	}
}
