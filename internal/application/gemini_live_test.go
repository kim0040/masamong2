package application_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLiveGemini runs the product end to end on Google's real Gemini API: add the provider, prove the
// key, fill every role it can serve with the recommended models, import the practice sample chat,
// index it (embeddings and summaries), search it, answer a question from it, and draw one image.
// It spends a few cents, so it runs only when MASAMONG_LIVE_GEMINI_KEY_FILE names a file holding a
// key (never in CI):
//
//	MASAMONG_LIVE_GEMINI_KEY_FILE=~/gemini.key go test ./internal/application -run TestLiveGemini -v
func TestLiveGemini(t *testing.T) {
	keyFile := os.Getenv("MASAMONG_LIVE_GEMINI_KEY_FILE")
	if keyFile == "" || os.Getenv("CI") != "" {
		t.Skip("set MASAMONG_LIVE_GEMINI_KEY_FILE to a file with a Gemini API key")
	}
	t.Setenv("MASAMONG_OFFLINE", "") // the package's tests run offline; this one is the exception
	svc := setupSvc(t)
	step := func(act string, in map[string]any) map[string]any {
		t.Helper()
		r := exec(svc, act, in)
		if !r.OK {
			t.Fatalf("%s: %+v", act, r.Error)
		}
		m, err := jsonRoundTrip(r.Output)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	step("providers.add", map[string]any{"api": "gemini"})
	step("providers.credentials", map[string]any{"id": "gemini", "credential_ref": "file:" + keyFile})
	probe := step("providers.test", map[string]any{"id": "gemini"})
	t.Logf("key check: %v models, %vms", probe["models"], probe["latency_ms"])

	rec := step("models.recommend", map[string]any{"provider": "gemini", "apply": true})
	picks, _ := json.Marshal(rec["picks"])
	t.Logf("roles filled: %s", picks)

	step("channels.configure", map[string]any{"guild_id": "g", "id": "c", "respond": "always", "persona_ref": "friendly_ko", "memory_use": "on"})
	sample, err := filepath.Abs("practice/sample-ko.csv")
	if err != nil {
		t.Fatal(err)
	}
	step("imports.start", map[string]any{"source": "kakao_csv", "path": sample, "room": "kakao:live", "label": "live sample", "timezone": "Asia/Seoul"})
	idx := step("memory.reindex", map[string]any{"rooms": []string{"kakao:live"}})
	idxJSON, _ := json.Marshal(idx)
	t.Logf("index: %s", idxJSON)
	blocks := step("memory.blocks.list", nil)
	for _, b := range blocks["blocks"].([]any) {
		bm := b.(map[string]any)
		if bm["source_room"] == "kakao:live" {
			step("channels.view.attach", map[string]any{"guild_id": "g", "channel_id": "c", "block_id": bm["block_id"]})
		}
	}

	found := step("memory.search", map[string]any{"query": "텐트 누가 챙겨", "guild_id": "g", "channel_id": "c", "top_k": 3})
	foundJSON, _ := json.Marshal(found["hits"])
	t.Logf("search: %.600s", foundJSON)
	if !strings.Contains(string(foundJSON), "텐트") {
		t.Errorf("the tent messages are found: %s", foundJSON)
	}

	ans := step("chat.respond", map[string]any{"guild_id": "g", "channel_id": "c", "message": "캠핑 갈 때 텐트는 누가 챙기기로 했지?", "detail": true})
	t.Logf("answer: %v | rag=%v hits=%v cost=%v timing=%v", ans["answer"], ans["rag_state"], ans["hits"], ans["cost_usd"], ans["timing_ms"])
	if strings.TrimSpace(ans["answer"].(string)) == "" {
		t.Error("an answer")
	}

	// the planner picks tools on Gemini too: keyless weather, and reading a linked page
	for _, q := range []struct{ msg, tool string }{
		{"오늘 서울 날씨 어때?", "weather.query"},
		{"이 링크에 뭐라고 써 있어? https://example.com", "web.read"},
	} {
		a := step("chat.respond", map[string]any{"guild_id": "g", "channel_id": "c", "message": q.msg, "detail": true})
		used, _ := json.Marshal(a["tools"])
		t.Logf("%s → %v | tools %s", q.msg, a["answer"], used)
		if !strings.Contains(string(used), q.tool) {
			t.Errorf("%q should use %s: %s", q.msg, q.tool, used)
		}
	}

	img := exec(svc, "images.generate", map[string]any{"prompt": "a small camping tent under the stars, flat illustration", "aspect_ratio": "1:1"})
	if !img.OK {
		t.Errorf("image: %+v", img.Error)
	} else {
		t.Logf("image: %v bytes %v, cost %v", img.Output["bytes"], img.Output["mime"], img.Output["cost_usd"])
	}

	doc := step("runtime.doctor", nil)
	for _, f := range []string{"llm", "memory", "retrieval", "images"} {
		if st := doc["features"].(map[string]any)[f].(map[string]any)["status"]; st != "ready" {
			t.Errorf("doctor %s: %v", f, st)
		}
	}
	feats, _ := json.Marshal(doc["features"])
	t.Logf("doctor: %s", feats)
	usage := step("usage.budgets", nil)
	usageJSON, _ := json.Marshal(usage)
	t.Logf("usage: %.800s", usageJSON)
}
