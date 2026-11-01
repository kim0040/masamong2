package application_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"masamong/internal/application"
	"masamong/internal/config"
	"masamong/internal/errcode"
	"masamong/internal/hostprofile"
)

// A Gemini provider added from the console: listed host, key reference, recommended models applied in
// one change, rerank left where it was (Gemini has none), and the embedding width reset for the new
// model.
func TestAddGeminiAndFillRoles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/models") || r.Header.Get("Authorization") != "Bearer g-key" {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`[{"error":{"code":400,"message":"Please pass a valid API key","status":"INVALID_ARGUMENT"}}]`))
			return
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"models/gemini-3.8-flash","object":"model"},{"id":"models/gemini-3.5-flash-lite","object":"model"},{"id":"models/gemini-embedding-2","object":"model"},{"id":"models/gemini-3.1-flash-image","object":"model"},{"id":"models/gemini-3.8-flash-tts","object":"model"}]}`))
	}))
	defer srv.Close()
	svc := setupSvc(t)
	if r := exec(svc, "providers.add", map[string]any{"api": "gemini"}); !r.OK || r.Output["id"] != "gemini" {
		t.Fatalf("add listed host: %+v", r)
	}
	doc := out(t, svc, "config.document", nil)["document"].(map[string]any)
	g := doc["providers"].(map[string]any)["gemini"].(map[string]any)
	if g["base_url"] != "https://generativelanguage.googleapis.com/v1beta/openai" || g["credential_ref"] != "env:MASAMONG_GEMINI_KEY" || g["api"] != nil {
		t.Fatalf("listed host needs no api field (its address says it): %v", g)
	}
	if r := exec(svc, "providers.add", map[string]any{"api": "gemini"}); r.OK {
		t.Fatal("the same id twice")
	}
	// a test double at another address says its family explicitly
	if r := exec(svc, "providers.add", map[string]any{"api": "gemini", "id": "gem", "base_url": srv.URL + "/v1beta/openai"}); !r.OK {
		t.Fatalf("add at an address: %+v", r)
	}
	t.Setenv("GEM_KEY", "g-key")
	if r := exec(svc, "providers.credentials", map[string]any{"id": "gem", "credential_ref": "env:GEM_KEY"}); !r.OK {
		t.Fatalf("key ref: %+v", r)
	}
	before := out(t, svc, "config.document", nil)["document"].(map[string]any)["models"].(map[string]any)
	preview := out(t, svc, "models.recommend", map[string]any{"provider": "gem"})
	if preview["embed_changes"] != true || preview["rerank"] != false {
		t.Fatalf("preview: %v", preview)
	}
	picks := map[string]string{}
	for _, p := range preview["picks"].([]any) {
		m := p.(map[string]any)
		picks[m["role"].(string)] = m["model"].(string)
	}
	if picks["response"] != "gemini-3.8-flash" || picks["summary"] != "gemini-3.5-flash-lite" || picks["embed"] != "gemini-embedding-2" || picks["image"] != "gemini-3.1-flash-image" {
		t.Fatalf("picks: %v", picks)
	}
	// the console applies the picks it showed, without listing the models again
	srv.Close()
	if r := exec(svc, "models.recommend", map[string]any{"provider": "gem", "apply": true, "picks": map[string]any{"response": "gemini-3.8-flash", "planner": "gemini-3.8-flash",
		"summary": "gemini-3.5-flash-lite", "embed": "gemini-embedding-2", "image": "gemini-3.1-flash-image"}}); !r.OK || r.Output["embed_changes"] != true {
		t.Fatalf("apply: %+v", r)
	}
	if r := exec(svc, "models.recommend", map[string]any{"provider": "gem", "apply": true, "picks": map[string]any{"nope": "x"}}); r.OK {
		t.Fatal("unknown roles are refused")
	}
	models := out(t, svc, "config.document", nil)["document"].(map[string]any)["models"].(map[string]any)
	embed := models["embed"].(map[string]any)
	if embed["provider"] != "gem" || embed["model"] != "gemini-embedding-2" || embed["dimensions"] != nil {
		t.Fatalf("embed role: %v (the old model's dimensions must not stay)", embed)
	}
	if models["rerank"].(map[string]any)["provider"] != before["rerank"].(map[string]any)["provider"] {
		t.Fatal("rerank keeps its provider")
	}
	// OpenRouter still has no key; the chat model is ready because the response role is on Gemini
	feats := out(t, svc, "runtime.doctor", nil)["features"].(map[string]any)
	if feats["llm"].(map[string]any)["status"] != "ready" || feats["fortune"].(map[string]any)["status"] != "ready" {
		t.Fatalf("doctor follows the roles, not one provider per kind: %v", feats["llm"])
	}
	list := out(t, svc, "providers.list", nil)
	caps := list["model_providers"].(map[string]any)["gem"].(map[string]any)
	if caps["api"] != "gemini" || caps["rerank"] != false {
		t.Fatalf("capabilities: %v", caps)
	}
	prices := list["model_prices"].(map[string]any)
	if r := prices["response"].(map[string]any); r["source"] != "published" || r["checked"] == nil || r["in_per_mtok"].(float64) != 0.75 {
		t.Fatalf("published price: %v", r)
	}
	if r := exec(svc, "providers.remove", map[string]any{"id": "gem"}); r.OK {
		t.Fatal("a provider in use is not removed")
	}
	if r := exec(svc, "providers.remove", map[string]any{"id": "gemini"}); !r.OK {
		t.Fatalf("an unused one is: %+v", r)
	}
}

func TestModelsRecommendExplainsWhenNoRuleMatches(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/models") {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer srv.Close()
	t.Setenv("MASAMONG_OPENROUTER_KEY", "test-key")
	svc := setupSvc(t)
	if r := exec(svc, "config.patch", map[string]any{"ops": []any{
		map[string]any{"path": []any{"providers", "openrouter", "api"}, "value": "openrouter"},
		map[string]any{"path": []any{"providers", "openrouter", "base_url"}, "value": srv.URL},
	}}); !r.OK {
		t.Fatalf("configure local model-list test server: %+v", r.Error)
	}
	r := exec(svc, "models.recommend", map[string]any{"provider": "openrouter"})
	if r.OK || r.Error == nil || r.Error.Code != errcode.NoRecommendation {
		t.Fatalf("empty recommendations must fail explicitly: %+v", r)
	}
	if r.Error.MessageKey != "models.recommend.none" || r.Error.Args["reason"] == nil {
		t.Fatalf("missing no-recommendation reason: %+v", r.Error)
	}
}

// While a memory index exists, a new embedding model would leave memory search empty until every leaf
// is embedded again: the preview says what that costs, apply refuses it unless the operator confirms,
// recommend --apply keeps the indexed model, and the doctor reports the gap instead of "ready".
func TestEmbeddingSwitchWaitsForConfirmationWhileIndexed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"models/gemini-3.8-flash"},{"id":"models/gemini-embedding-2"}]}`))
	}))
	defer srv.Close()
	svc := setupSvc(t)
	if r := exec(svc, "providers.add", map[string]any{"api": "gemini", "id": "gem", "base_url": srv.URL + "/v1beta/openai"}); !r.OK {
		t.Fatalf("add: %+v", r)
	}
	t.Setenv("GEM_KEY", "g-key")
	t.Setenv("MASAMONG_OPENROUTER_KEY", "or-key") // the indexed model's provider has its key
	out(t, svc, "providers.credentials", map[string]any{"id": "gem", "credential_ref": "env:GEM_KEY"})
	eng, err := svc.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	db := svc.Store.DB
	for _, q := range []string{
		`INSERT INTO memory_rooms(room,collection,platform,updated_at,active_recipe)
			VALUES('kakao:friends','c','kakao','2026-09-01T00:00:00Z','r1')`,
		`INSERT INTO memory_observations(observation_id, platform, collection, source_room, room, native_message_id, actor_label, content, parsed_time, record_kind, imported_at)
			VALUES('o1','kakao','c','kakao:friends','kakao:friends','n1','철수','부산 가자','2026-09-01T00:00:00Z','message','2026-09-01T00:00:00Z')`,
		`INSERT INTO memory_leaves(leaf_id, room, recipe, first_observation_id, observation_ids, n, body, body_hash, speakers, closed, summary, embed_space, leaf_vec, updated_at)
			VALUES('leaf-1','kakao:friends','r1','o1','["o1"]',1,'철수: 부산 가자','h','["철수"]',1,'여행','` + eng.EmbedSpace() + `',x'00',  '2026-09-01T00:00:00Z')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	preview := out(t, svc, "models.recommend", map[string]any{"provider": "gem"})
	re, _ := preview["reindex"].(map[string]any)
	if preview["embed_changes"] != true || re["leaves"] != float64(1) || re["tokens"] != float64(len([]rune("철수: 부산 가자"))+2) || re["usd"] == nil {
		t.Fatalf("preview must say what re-embedding takes: %v", preview)
	}
	picks := map[string]any{"response": "gemini-3.8-flash", "embed": "gemini-embedding-2"}
	// the refusal carries a stable message key (the console asks the operator instead of showing the text)
	if r := exec(svc, "models.recommend", map[string]any{"provider": "gem", "apply": true, "picks": picks}); r.OK || r.Error == nil || r.Error.MessageKey != application.EmbedReindexRequiredKey || r.Error.Args["reindex"] == nil {
		t.Fatalf("changing an indexed embedding model needs include_embed (stable key and the reindex estimate): %+v", r)
	}
	// models.select is the same rule on the server side: refused with the reindex estimate, and the
	// configuration stays as it was
	r := exec(svc, "models.select", map[string]any{"role": "embed", "provider": "gem", "model": "gemini-embedding-2"})
	if r.OK || r.Error == nil || r.Error.Code != errcode.InvalidInput || r.Error.MessageKey != application.EmbedReindexRequiredKey || r.Error.Args["reindex"] == nil || !strings.Contains(r.Error.Message, "include_embed=true") {
		t.Fatalf("models.select must refuse an indexed embedding switch: %+v", r)
	}
	if m := out(t, svc, "config.document", nil)["document"].(map[string]any)["models"].(map[string]any); m["embed"].(map[string]any)["provider"] == "gem" {
		t.Fatalf("the refused select changed the embedding model: %v", m["embed"])
	}
	cur := out(t, svc, "config.document", nil)["document"].(map[string]any)["models"].(map[string]any)["embed"].(map[string]any)
	if r := exec(svc, "models.select", map[string]any{"role": "embed", "provider": cur["provider"], "model": cur["model"]}); !r.OK {
		t.Fatalf("selecting the model already in use is no switch: %+v", r.Error)
	}
	if r := exec(svc, "models.select", map[string]any{"role": "response", "provider": "gem", "model": "gemini-3.8-flash"}); !r.OK {
		t.Fatalf("other roles are not guarded: %+v", r.Error)
	}
	// recommend --apply (no picks) keeps the indexed embedding model and says so
	if r := exec(svc, "models.recommend", map[string]any{"provider": "gem", "apply": true}); !r.OK || r.Output["embed_kept"] != true || r.Output["embed_changes"] != false {
		t.Fatalf("apply without picks: %+v", r)
	}
	models := out(t, svc, "config.document", nil)["document"].(map[string]any)["models"].(map[string]any)
	if models["embed"].(map[string]any)["provider"] == "gem" || models["response"].(map[string]any)["provider"] != "gem" {
		t.Fatalf("only the other roles moved: %v", models)
	}
	feats := out(t, svc, "runtime.doctor", nil)["features"].(map[string]any)
	if feats["memory"].(map[string]any)["status"] != "ready" {
		t.Fatalf("unchanged model: memory keeps its index: %v", feats["memory"])
	}
	// message vectors are on by default, so the doctor lists them next to a usable memory
	if m, _ := feats["message_vectors"].(map[string]any); m == nil || (m["status"] != "ready" && m["status"] != "pending_index") {
		t.Fatalf("the doctor does not list the message vectors: %v", feats["message_vectors"])
	}
	if r := exec(svc, "models.recommend", map[string]any{"provider": "gem", "apply": true, "include_embed": true, "picks": picks}); !r.OK || r.Output["embed_changes"] != true {
		t.Fatalf("confirmed switch: %+v", r)
	}
	feats = out(t, svc, "runtime.doctor", nil)["features"].(map[string]any)
	if m := feats["memory"].(map[string]any); m["status"] != "pending_index" || m["reason"] != "embed_model_changed" {
		t.Fatalf("doctor must not call a changed model's empty index ready: %v", m)
	}
	var alerted bool
	for _, raw := range out(t, svc, "dashboard.overview", nil)["attention"].([]any) {
		alerted = alerted || raw.(map[string]any)["key"] == "embed_model_changed"
	}
	if !alerted {
		t.Fatal("the dashboard points at re-indexing")
	}
	// the confirmed select goes through
	if r := exec(svc, "models.select", map[string]any{"role": "embed", "provider": "gem", "model": "gemini-embedding-3", "include_embed": true}); !r.OK {
		t.Fatalf("confirmed models.select: %+v", r.Error)
	}
}

// Without a memory index there is nothing to empty: models.select switches the embedding model freely.
func TestEmbeddingSelectWithoutIndexIsNotGuarded(t *testing.T) {
	svc := setupSvc(t)
	if r := exec(svc, "providers.add", map[string]any{"api": "gemini", "id": "gem"}); !r.OK {
		t.Fatalf("add: %+v", r)
	}
	if r := exec(svc, "models.select", map[string]any{"role": "embed", "provider": "gem", "model": "gemini-embedding-2"}); !r.OK {
		t.Fatalf("no index, no guard: %+v", r.Error)
	}
}

func TestModelsSelectEmbeddingDimensionsFollowKnownMRLWidths(t *testing.T) {
	svc := setupSvc(t)
	applyDoc(t, svc, func(d *config.Document) {
		d.Models["embed"] = config.Model{Provider: "openrouter", Model: "vendor/old-embed", Dimensions: 2048}
	})
	selectEmbed := func(model string) {
		t.Helper()
		if r := exec(svc, "models.select", map[string]any{"role": "embed", "provider": "openrouter", "model": model}); !r.OK {
			t.Fatalf("select %s: %+v", model, r.Error)
		}
	}
	embed := func() map[string]any {
		t.Helper()
		return out(t, svc, "config.document", nil)["document"].(map[string]any)["models"].(map[string]any)["embed"].(map[string]any)
	}
	selectEmbed("qwen/qwen3-embedding-8b")
	if got := embed()["dimensions"]; got != float64(config.DefaultEmbedDimensions) {
		t.Fatalf("qwen 8b dimensions = %v, want %d", got, config.DefaultEmbedDimensions)
	}
	selectEmbed("text-embedding-3-large")
	if got := embed()["dimensions"]; got != float64(config.DefaultEmbedDimensions) {
		t.Fatalf("text-embedding-3-large dimensions = %v, want %d", got, config.DefaultEmbedDimensions)
	}
	selectEmbed("qwen/qwen3-embedding-0.6b")
	if got := embed()["dimensions"]; got != nil {
		t.Fatalf("an MRL model whose full width is at most the default must use its full width: %v", got)
	}
	selectEmbed("vendor/unknown-embed")
	if got := embed()["dimensions"]; got != nil {
		t.Fatalf("unknown model must keep its full width: %v", got)
	}
	applyDoc(t, svc, func(d *config.Document) {
		d.Models["embed"] = config.Model{Provider: "openrouter", Model: "qwen/qwen3-embedding-8b", Dimensions: 2048}
	})
	selectEmbed("qwen/qwen3-embedding-8b")
	if got := embed()["dimensions"]; got != float64(2048) {
		t.Fatalf("reselecting the same model changed operator dimensions: %v", got)
	}
}

func TestModelsRecommendEmbeddingDimensionsAndApplySuggestedWidth(t *testing.T) {
	svc := listService(t)
	applyDoc(t, svc, func(d *config.Document) {
		d.Models["rerank"] = config.Model{}
		d.Models["embed"] = config.Model{Provider: "openrouter", Model: "qwen/qwen3-embedding-4b", Dimensions: 2048}
	})
	preview := out(t, svc, "models.recommend", map[string]any{"provider": "openrouter"})
	if preview["embed_dimensions"] != float64(0) || preview["embed_full_width"] != float64(4096) || preview["embed_dimensions_reason"] != "no_rerank" {
		t.Fatalf("no-rerank recommendation: %v", preview)
	}
	picks := map[string]any{}
	for _, raw := range preview["picks"].([]any) {
		row := raw.(map[string]any)
		picks[row["role"].(string)] = row["model"].(string)
	}
	if r := exec(svc, "models.recommend", map[string]any{"provider": "openrouter", "apply": true, "include_embed": true, "picks": picks}); !r.OK {
		t.Fatalf("apply recommendation: %+v", r.Error)
	}
	embed := out(t, svc, "config.document", nil)["document"].(map[string]any)["models"].(map[string]any)["embed"].(map[string]any)
	if embed["dimensions"] != nil {
		t.Fatalf("applied no-rerank width = %v, want native width 0", embed)
	}
	applyDoc(t, svc, func(d *config.Document) {
		d.Models["embed"] = config.Model{Provider: "openrouter", Model: "qwen/qwen3-embedding-8b", Dimensions: 2048}
	})
	if r := exec(svc, "models.recommend", map[string]any{"provider": "openrouter", "apply": true, "include_embed": true, "picks": picks}); !r.OK {
		t.Fatalf("reapply the same embedding model: %+v", r.Error)
	}
	embed = out(t, svc, "config.document", nil)["document"].(map[string]any)["models"].(map[string]any)["embed"].(map[string]any)
	if embed["dimensions"] != float64(2048) {
		t.Fatalf("reapplying the same embedding model changed dimensions: %v", embed)
	}
}

func TestModelsRecommendEmbeddingDimensionsUseDefaultOrSmallHost(t *testing.T) {
	t.Run("rerank uses default", func(t *testing.T) {
		svc := listService(t)
		applyDoc(t, svc, func(d *config.Document) {
			d.Models["embed"] = config.Model{Provider: "openrouter", Model: "qwen/qwen3-embedding-4b", Dimensions: 2048}
		})
		preview := out(t, svc, "models.recommend", map[string]any{"provider": "openrouter"})
		if preview["embed_dimensions"] != float64(config.DefaultEmbedDimensions) || preview["embed_dimensions_reason"] != "default" {
			t.Fatalf("rerank recommendation: %v", preview)
		}
	})
	t.Run("small host keeps default without rerank", func(t *testing.T) {
		old := hostprofile.Current()
		hostprofile.Apply(hostprofile.Profile{Tier: hostprofile.TierSmall})
		t.Cleanup(func() { hostprofile.Apply(old) })
		svc := listService(t)
		applyDoc(t, svc, func(d *config.Document) {
			d.Models["rerank"] = config.Model{}
			d.Models["embed"] = config.Model{Provider: "openrouter", Model: "qwen/qwen3-embedding-4b", Dimensions: 2048}
		})
		preview := out(t, svc, "models.recommend", map[string]any{"provider": "openrouter"})
		if preview["embed_dimensions"] != float64(config.DefaultEmbedDimensions) || preview["embed_dimensions_reason"] != "small_host" {
			t.Fatalf("small-host recommendation: %v", preview)
		}
		picks := map[string]any{}
		for _, raw := range preview["picks"].([]any) {
			row := raw.(map[string]any)
			picks[row["role"].(string)] = row["model"].(string)
		}
		if r := exec(svc, "models.recommend", map[string]any{"provider": "openrouter", "apply": true, "include_embed": true, "picks": picks}); !r.OK {
			t.Fatalf("apply small-host recommendation: %+v", r.Error)
		}
		embed := out(t, svc, "config.document", nil)["document"].(map[string]any)["models"].(map[string]any)["embed"].(map[string]any)
		if embed["dimensions"] != float64(config.DefaultEmbedDimensions) {
			t.Fatalf("applied small-host width = %v, want %d", embed, config.DefaultEmbedDimensions)
		}
	})
}
