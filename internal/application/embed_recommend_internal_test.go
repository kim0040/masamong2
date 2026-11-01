package application

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"testing"

	"masamong/internal/action"
	"masamong/internal/config"
	"masamong/internal/hostprofile"
	"masamong/internal/providers/openai"
)

func TestModelsRecommendWidthUsesAppliedRerankAvailability(t *testing.T) {
	for _, hostID := range []string{"openai", "gemini", "cometapi"} {
		for _, mode := range []string{"unconfigured", "configured", "small", "picked-unusable"} {
			t.Run(hostID+"/"+mode, func(t *testing.T) {
				host, _ := openai.HostByID(hostID)
				t.Setenv(host.KeyEnv, "fixture-only")
				rerankKey := ""
				if mode == "configured" || mode == "picked-unusable" {
					rerankKey = "fixture-only"
				}
				t.Setenv("MASAMONG_OPENROUTER_KEY", rerankKey)
				old := hostprofile.Current()
				tier := hostprofile.TierMedium
				if mode == "small" {
					tier = hostprofile.TierSmall
				}
				hostprofile.Apply(hostprofile.Profile{Tier: tier})
				t.Cleanup(func() { hostprofile.Apply(old) })
				s := privacyService(t)
				r := s.mutateConfig(action.Request{}, func(d *config.Document) error {
					d.Providers[hostID] = config.Provider{Kind: config.KindOpenAI, API: string(host.API), BaseURL: host.BaseURL, CredentialRef: "env:" + host.KeyEnv}
					// a different model than every pick below: the same model on another host keeps its width
					d.Models["embed"] = config.Model{Provider: "openrouter", Model: "text-embedding-3-small"}
					return nil
				})
				if !r.OK {
					t.Fatal(r.Error)
				}
				model := "text-embedding-3-large"
				if hostID == "gemini" {
					model = "gemini-embedding-001"
				} else if hostID == "cometapi" {
					model = "qwen/qwen3-embedding-8b"
				}
				picks := map[string]any{"embed": model}
				if mode == "picked-unusable" {
					picks["rerank"] = "voyageai/rerank-2.5-lite"
				}
				r = widthExecute(s, "models.recommend", map[string]any{"provider": hostID, "apply": true, "picks": picks})
				dims, reason := 0, "no_rerank"
				if mode == "configured" {
					dims, reason = 1024, "default"
				} else if mode == "small" {
					dims, reason = 1024, "small_host"
				}
				if !r.OK || r.Output["embed_dimensions"] != dims || r.Output["embed_dimensions_reason"] != reason || s.Doc().Models["embed"].Dimensions != dims {
					t.Fatal("recommendation disagrees with applied rerank availability")
				}
				if dims == 0 {
					full, _ := openai.EmbedFullWidth(model)
					if r.Output["embed_full_width"] != full {
						t.Fatal("native recommendation must include numeric full width")
					}
					e, err := s.MemoryEngine()
					if err != nil || e.EmbedSpace() != model+":0:l2" {
						t.Fatal("native recommendation must use the zero-width engine space")
					}
				}
			})
		}
	}
}

func TestModelsSelectProviderOnlyPreservesEmbedWidth(t *testing.T) {
	for _, dims := range []int{0, 1024, 2048, 4096} {
		t.Run(strconv.Itoa(dims), func(t *testing.T) {
			s := privacyService(t)
			r := s.mutateConfig(action.Request{}, func(d *config.Document) error {
				host, _ := openai.HostByID("cometapi")
				d.Providers[host.ID] = config.Provider{Kind: config.KindOpenAI, BaseURL: host.BaseURL}
				d.Models["embed"] = config.Model{Provider: "openrouter", Model: "qwen/qwen3-embedding-8b", Dimensions: dims, PriceInPerMTok: 1, PriceOutPerMTok: 2}
				return nil
			})
			if !r.OK {
				t.Fatal(r.Error)
			}
			r = widthExecute(s, "models.select", map[string]any{"role": "embed", "provider": "cometapi", "model": "qwen/qwen3-embedding-8b"})
			m := s.Doc().Models["embed"]
			if !r.OK || m.Provider != "cometapi" || m.Dimensions != dims || m.PriceInPerMTok != 0 || m.PriceOutPerMTok != 0 {
				t.Fatal("provider-only selection must preserve dimensions and reset provider pricing")
			}
		})
	}
}

func TestModelsRecommendWidthOutputOnlyForEmbedChange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"text-embedding-3-large","object":"model"}]}`))
	}))
	defer srv.Close()
	t.Setenv("WIDTH_RECOMMEND_KEY", "fixture-only")
	t.Setenv("MASAMONG_OPENROUTER_KEY", "")
	old := hostprofile.Current()
	hostprofile.Apply(hostprofile.Profile{Tier: hostprofile.TierMedium})
	t.Cleanup(func() { hostprofile.Apply(old) })
	s := privacyService(t)
	r := s.mutateConfig(action.Request{}, func(d *config.Document) error {
		d.Providers["width-fixture"] = config.Provider{Kind: config.KindOpenAI, API: "openai", BaseURL: srv.URL, CredentialRef: "env:WIDTH_RECOMMEND_KEY"}
		return nil
	})
	if !r.OK {
		t.Fatal(r.Error)
	}
	widthRoom(t, s)
	r = widthExecute(s, "models.recommend", map[string]any{"provider": "width-fixture"})
	if !r.OK || r.Output["embed_dimensions"] != 0 || r.Output["embed_full_width"] != 3072 || r.Output["embed_dimensions_reason"] != "no_rerank" {
		t.Fatal("preview must expose native width and full width for an embedding change")
	}
	before := s.Doc().Models["embed"]
	r = widthExecute(s, "models.recommend", map[string]any{"provider": "width-fixture", "apply": true})
	if !r.OK || r.Output["embed_kept"] != true || !reflect.DeepEqual(s.Doc().Models["embed"], before) {
		t.Fatal("unconfirmed automatic apply must retain the indexed embedding")
	}
	assertNoRecommendedWidth(t, r)
	r = widthExecute(s, "models.recommend", map[string]any{"provider": "width-fixture", "apply": true, "include_embed": true, "picks": map[string]any{"embed": "text-embedding-3-large"}})
	if !r.OK || s.Doc().Models["embed"].Dimensions != 0 || r.Output["embed_full_width"] != 3072 {
		t.Fatal("confirmed full-width recommendation must store zero")
	}
	for _, apply := range []bool{false, true} {
		r = widthExecute(s, "models.recommend", map[string]any{"provider": "width-fixture", "apply": apply})
		if !r.OK || r.Output["embed_changes"] != false {
			t.Fatal("same embedding recommendation must report no change")
		}
		assertNoRecommendedWidth(t, r)
	}
}

func TestModelsRecommendSmallFullWidthUsesZero(t *testing.T) {
	s := privacyService(t)
	r := widthExecute(s, "models.recommend", map[string]any{"provider": "openrouter", "apply": true, "picks": map[string]any{"embed": "qwen/qwen3-embedding-0.6b"}})
	if !r.OK || r.Output["embed_dimensions"] != 0 || r.Output["embed_full_width"] != 1024 || s.Doc().Models["embed"].Dimensions != 0 {
		t.Fatal("1024 full-width model must use native width zero")
	}
}

func assertNoRecommendedWidth(t *testing.T, r action.Result) {
	t.Helper()
	for _, key := range []string{"embed_dimensions", "embed_dimensions_reason", "embed_full_width"} {
		if _, present := r.Output[key]; present {
			t.Fatalf("unchanged embedding must omit %s", key)
		}
	}
}
