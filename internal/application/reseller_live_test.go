package application_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLiveResellers runs the previous bot's services on the product: CometAPI for chat, embeddings and
// images (the recommended picks), reference prices for a host without a price list, a Korean stock
// answered from search (Finnhub's plan has no KRX), and the tool keys it used. It spends cents, so it
// runs only when MASAMONG_LIVE_KEYS_DIR names a folder of key files (cometapi, linkup, kakao, kma,
// finnhub, exchangerate; never in CI):
//
//	MASAMONG_LIVE_KEYS_DIR=~/keys go test ./internal/application -run TestLiveResellers -v
func TestLiveResellers(t *testing.T) {
	dir := os.Getenv("MASAMONG_LIVE_KEYS_DIR")
	if dir == "" || os.Getenv("CI") != "" {
		t.Skip("set MASAMONG_LIVE_KEYS_DIR to a folder of key files")
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
	step("providers.add", map[string]any{"host": "cometapi"})
	for _, id := range []string{"cometapi", "linkup", "kakao", "kma", "finnhub", "exchangerate"} {
		step("providers.credentials", map[string]any{"id": id, "credential_ref": "file:" + filepath.Join(dir, id)})
	}
	if p := step("providers.test", map[string]any{"id": "cometapi"}); p["key_check"] != "list" {
		t.Fatalf("CometAPI's list needs the key: %v", p)
	}
	rec := step("models.recommend", map[string]any{"provider": "cometapi", "apply": true})
	picks := map[string]string{}
	for _, p := range rec["picks"].([]any) {
		m := p.(map[string]any)
		picks[m["role"].(string)] = m["model"].(string)
	}
	t.Logf("picks: %v", picks)
	// the newest of each line: deepseek flash, gpt nano, the memory recipe's embedding, a gemini image model
	if !strings.Contains(picks["response"], "deepseek") || !strings.Contains(picks["response"], "flash") || !strings.Contains(picks["planner"], "nano") ||
		picks["embed"] != "qwen3-embedding-8b" || !strings.Contains(picks["image"], "flash-image") || rec["rerank"] != false {
		t.Fatalf("recommended picks: %v (rerank %v)", picks, rec["rerank"])
	}
	// reference prices arrive in the background
	deadline := time.Now().Add(60 * time.Second)
	var prices map[string]any
	for time.Now().Before(deadline) {
		prices = step("providers.list", nil)["model_prices"].(map[string]any)
		if prices["response"].(map[string]any)["source"] == "reference" && prices["embed"].(map[string]any)["source"] == "reference" {
			break
		}
		time.Sleep(time.Second)
	}
	t.Logf("prices: %v", prices)
	for _, role := range []string{"response", "planner", "embed", "image"} {
		if prices[role].(map[string]any)["source"] != "reference" {
			t.Errorf("%s priced from the reference list: %v", role, prices[role])
		}
	}
	img := step("images.generate", map[string]any{"prompt": "a small white pomeranian waving, flat illustration", "aspect_ratio": "1:1"})
	if img["bytes"].(float64) < 10000 || img["cost_usd"] == nil || img["cost_usd"].(float64) <= 0 {
		t.Fatalf("image with a cost: %v", img)
	}
	t.Logf("image: %v bytes, $%v, %vms", img["bytes"], img["cost_usd"], img["elapsed_ms"])
	q := step("finance.quote", map[string]any{"symbol": "005930.KS"})
	if q["source"] != "search" || len(q["results"].([]any)) == 0 {
		t.Fatalf("Korean stock from search: %v", q)
	}
	if us := step("finance.quote", map[string]any{"symbol": "AAPL"}); us["source"] != "finnhub" {
		t.Fatalf("US stock live: %v", us)
	}
	chatIn := map[string]any{"guild_id": "g", "channel_id": "c", "message": "고양이 그림 하나 그려줘", "detail": true}
	res := step("chat.respond", chatIn)
	t.Logf("drawing in chat: tools=%v answer=%q", res["tools"], res["answer"])
	if !strings.Contains(strings.ToLower(jsonString(t, res["tools"])), "images.generate") {
		t.Fatalf("the planner draws in conversation: %v", res["tools"])
	}
}

func jsonString(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
