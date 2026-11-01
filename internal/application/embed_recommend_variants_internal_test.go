package application

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"masamong/internal/action"
	"masamong/internal/config"
	"masamong/internal/hostprofile"
)

// rerankPickService is a service whose rerank role points at the default OpenRouter provider (usable only
// when rerankKey is set), plus an OpenRouter-style "pick-fixture" provider that is configured and lists
// the recommended embedding and rerank models: picking its rerank model is what makes reranking usable.
func rerankPickService(t *testing.T, tier string, rerankKey string, embed config.Model) *Service {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"qwen/qwen3-embedding-8b","object":"model"},{"id":"voyageai/rerank-2.5-lite","object":"model"}]}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("PICK_FIXTURE_KEY", "fixture-only")
	t.Setenv("MASAMONG_OPENROUTER_KEY", rerankKey)
	old := hostprofile.Current()
	hostprofile.Apply(hostprofile.Profile{Tier: tier})
	t.Cleanup(func() { hostprofile.Apply(old) })
	s := privacyService(t)
	r := s.mutateConfig(action.Request{}, func(d *config.Document) error {
		d.Providers["pick-fixture"] = config.Provider{Kind: config.KindOpenAI, API: "openrouter", BaseURL: srv.URL, CredentialRef: "env:PICK_FIXTURE_KEY"}
		d.Models["embed"] = embed
		return nil
	})
	if !r.OK {
		t.Fatal(r.Error)
	}
	return s
}

func widthProposal(t *testing.T, r action.Result, key string) map[string]any {
	t.Helper()
	m, ok := r.Output[key].(map[string]any)
	if !ok {
		t.Fatalf("%s missing from %v", key, r.Output)
	}
	return m
}

func TestProvidersListReportsRerankUsable(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
		edit func(*config.Document)
		want bool
	}{
		{"configured-rerank-host", "fixture-only", func(*config.Document) {}, true},
		{"no-key", "", func(*config.Document) {}, false},
		{"no-model", "fixture-only", func(d *config.Document) {
			m := d.Models["rerank"]
			m.Model = ""
			d.Models["rerank"] = m
		}, false},
		{"host-without-rerank", "fixture-only", func(d *config.Document) {
			d.Providers["no-rerank"] = config.Provider{Kind: config.KindOpenAI, API: "openai", BaseURL: "https://api.openai.com/v1", CredentialRef: "env:MASAMONG_OPENROUTER_KEY"}
			m := d.Models["rerank"]
			m.Provider = "no-rerank"
			d.Models["rerank"] = m
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MASAMONG_OPENROUTER_KEY", tc.key)
			s := privacyService(t)
			if r := s.mutateConfig(action.Request{}, func(d *config.Document) error { tc.edit(d); return nil }); !r.OK {
				t.Fatal(r.Error)
			}
			r := widthExecute(s, "providers.list", nil)
			info, _ := r.Output["embed_width"].(map[string]any)
			got, present := info["rerank_usable"].(bool)
			if !r.OK || !present || got != tc.want {
				t.Fatalf("rerank_usable = %v (present %v), want %v", got, present, tc.want)
			}
		})
	}
}

func TestModelsRecommendPreviewOffersWidthWithoutRerankPick(t *testing.T) {
	stale := config.Model{Provider: "openrouter", Model: "text-embedding-3-large", Dimensions: 1024}
	for _, tc := range []struct {
		name        string
		tier        string
		rerankKey   string
		with        map[string]any
		withoutPick map[string]any
	}{
		{"pick-makes-rerank-usable", hostprofile.TierMedium, "",
			map[string]any{"dimensions": 1024, "reason": "default"},
			map[string]any{"dimensions": 0, "reason": "no_rerank", "full_width": 4096}},
		{"small-host", hostprofile.TierSmall, "",
			map[string]any{"dimensions": 1024, "reason": "default"},
			map[string]any{"dimensions": 1024, "reason": "small_host"}},
		{"retained-rerank-already-usable", hostprofile.TierMedium, "fixture-only",
			map[string]any{"dimensions": 1024, "reason": "default"},
			map[string]any{"dimensions": 1024, "reason": "default"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := rerankPickService(t, tc.tier, tc.rerankKey, stale)
			r := widthExecute(s, "models.recommend", map[string]any{"provider": "pick-fixture"})
			if !r.OK {
				t.Fatal(r.Error)
			}
			if r.Output["embed_dimensions"] != tc.with["dimensions"] || r.Output["embed_dimensions_reason"] != tc.with["reason"] {
				t.Fatalf("with every pick: %v / %v", r.Output["embed_dimensions"], r.Output["embed_dimensions_reason"])
			}
			alt := widthProposal(t, r, "embed_width_without_rerank_pick")
			if len(alt) != len(tc.withoutPick) {
				t.Fatalf("without the rerank pick: %v, want %v", alt, tc.withoutPick)
			}
			for k, v := range tc.withoutPick {
				if alt[k] != v {
					t.Fatalf("without the rerank pick: %s = %v, want %v (%v)", k, alt[k], v, alt)
				}
			}
			// the server stays the truth: applying exactly the kept picks stores the width of that set
			picks := map[string]any{"embed": "qwen/qwen3-embedding-8b"}
			r = widthExecute(s, "models.recommend", map[string]any{"provider": "pick-fixture", "apply": true, "picks": picks})
			if !r.OK || r.Output["embed_dimensions"] != tc.withoutPick["dimensions"] || s.Doc().Models["embed"].Dimensions != tc.withoutPick["dimensions"] {
				t.Fatal("apply without the rerank pick must store the width the alternative proposal showed")
			}
			if _, present := r.Output["embed_width_without_rerank_pick"]; present {
				t.Fatal("an apply result carries the width it used, not the alternative")
			}
		})
	}
}

func TestModelsRecommendAppliesWidthOfTheKeptPicks(t *testing.T) {
	stale := config.Model{Provider: "openrouter", Model: "text-embedding-3-large", Dimensions: 1024}
	s := rerankPickService(t, hostprofile.TierMedium, "", stale)
	picks := map[string]any{"embed": "qwen/qwen3-embedding-8b", "rerank": "voyageai/rerank-2.5-lite"}
	r := widthExecute(s, "models.recommend", map[string]any{"provider": "pick-fixture", "apply": true, "picks": picks})
	if !r.OK || r.Output["embed_dimensions"] != 1024 || s.Doc().Models["embed"].Dimensions != 1024 {
		t.Fatal("embed and rerank picked together: reranking becomes usable, so the default width")
	}
	if _, present := r.Output["embed_width_without_rerank_pick"]; present {
		t.Fatal("an apply result carries the width it used, not the alternative")
	}
}

func TestModelsRecommendWithoutRerankPickOnlyForAnEmbeddingChange(t *testing.T) {
	s := rerankPickService(t, hostprofile.TierMedium, "", config.Model{Provider: "pick-fixture", Model: "qwen/qwen3-embedding-8b", Dimensions: 1024})
	r := widthExecute(s, "models.recommend", map[string]any{"provider": "pick-fixture"})
	if !r.OK || r.Output["embed_changes"] != false {
		t.Fatal("fixture must keep the embedding")
	}
	assertNoRecommendedWidth(t, r)
	if _, present := r.Output["embed_width_without_rerank_pick"]; present {
		t.Fatal("no alternative width without an embedding change")
	}
}

func TestModelsRecommendSameModelOtherProviderKeepsWidth(t *testing.T) {
	for _, dims := range []int{0, 1024, 2048} {
		s := rerankPickService(t, hostprofile.TierMedium, "", config.Model{Provider: "openrouter", Model: "qwen/qwen3-embedding-8b", Dimensions: dims})
		r := widthExecute(s, "models.recommend", map[string]any{"provider": "pick-fixture"})
		if !r.OK || r.Output["embed_changes"] != true {
			t.Fatal("a different provider is still an embedding change for the reindex gate")
		}
		assertNoRecommendedWidth(t, r)
		if _, present := r.Output["embed_width_without_rerank_pick"]; present {
			t.Fatal("the width is kept, so no proposal")
		}
		r = widthExecute(s, "models.recommend", map[string]any{"provider": "pick-fixture", "apply": true})
		m := s.Doc().Models["embed"]
		if !r.OK || m.Provider != "pick-fixture" || m.Model != "qwen/qwen3-embedding-8b" || m.Dimensions != dims {
			t.Fatalf("provider-only apply must keep the operator's width %d: %+v", dims, m)
		}
		assertNoRecommendedWidth(t, r)
		// a different model still takes the proposal (the apply above moved reranking to the fixture too)
		r = widthExecute(s, "models.recommend", map[string]any{"provider": "pick-fixture", "apply": true, "picks": map[string]any{"embed": "text-embedding-3-large"}})
		if !r.OK || s.Doc().Models["embed"].Dimensions != 1024 || r.Output["embed_dimensions"] != 1024 {
			t.Fatal("a new model keeps taking the width proposal")
		}
	}
}
