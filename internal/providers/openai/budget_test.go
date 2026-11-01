package openai

import (
	"os"
	"slices"
	"testing"
)

func bp(b bool) *bool { return &b }

// A fixture list shaped like OpenRouter's (per-token price strings), without the network.
func budgetFixture() []Model {
	return []Model{
		{ID: "deepseek/deepseek-v4.1-flash", Name: "DeepSeek V4.1 Flash", Kind: "chat", ContextLength: 128000, PromptPrice: "0.00000014", CompletionPrice: "0.00000028", SupportsTools: bp(true), ImageInput: bp(false)},
		{ID: "deepseek/deepseek-v4.1-flash:free", Kind: "chat", ContextLength: 64000, PromptPrice: "0", CompletionPrice: "0", SupportsTools: bp(true)},
		{ID: "vendor/cheap-nobody", Kind: "chat", ContextLength: 128000, PromptPrice: "0.00000002", CompletionPrice: "0.00000004", SupportsTools: bp(true), ImageInput: bp(true)},
		{ID: "vendor/cheapest-notools", Kind: "chat", ContextLength: 128000, PromptPrice: "0.00000001", CompletionPrice: "0.00000001", SupportsTools: bp(false), ImageInput: bp(true)},
		{ID: "vendor/tiny-context", Kind: "chat", ContextLength: 8000, PromptPrice: "0.000000005", CompletionPrice: "0.000000005", SupportsTools: bp(true)},
		{ID: "vendor/unknown-tools", Kind: "chat", ContextLength: 0, PromptPrice: "0.00000003", CompletionPrice: "0.00000003"},
		{ID: "vendor/no-price", Kind: "chat", ContextLength: 128000, SupportsTools: bp(true), ImageInput: bp(true)},
		{ID: "openrouter/auto", Kind: "chat", ContextLength: 2000000, PromptPrice: "-1", CompletionPrice: "-1", SupportsTools: bp(true)},
		{ID: "openai/gpt-6-luna", Kind: "chat", ContextLength: 400000, PromptPrice: "0.0000001", CompletionPrice: "0.0000005", SupportsTools: bp(true), ImageInput: bp(true)},
		{ID: "google/gemini-3.1-flash", Kind: "chat", ContextLength: 1000000, PromptPrice: "0.0000003", CompletionPrice: "0.0000025", CachedPrice: "0.00000003", SupportsTools: bp(true), ImageInput: bp(true)},
		{ID: "google/gemini-3.1-flash-image", Kind: "chat", ContextLength: 65536, PromptPrice: "0.0000003", CompletionPrice: "0.00006", ImageOutput: true, ImageInput: bp(true)},
		{ID: "google/gemini-3.1-flash-lite-image", Kind: "chat", ContextLength: 65536, PromptPrice: "0.0000001", CompletionPrice: "0.0000004", ImageOutput: true, ImageInput: bp(true)},
		{ID: "qwen/qwen3-embedding-8b", Kind: "embedding", ContextLength: 32000, PromptPrice: "0.00000001", CompletionPrice: "0"},
		{ID: "vendor/embed-cheap", Kind: "embedding", ContextLength: 8000, PromptPrice: "0.000000005", CompletionPrice: "0"},
		{ID: "vendor/embed-unpriced", Kind: "embedding"},
		{ID: "voyageai/rerank-3-lite", Kind: "rerank", ContextLength: 32000, PromptPrice: "0.00000002", CompletionPrice: "0"},
	}
}

func ids(p []BudgetPick) []string {
	var out []string
	for _, b := range p {
		out = append(out, b.ID)
	}
	return out
}

func TestBudgetRoleFiltersAndRanking(t *testing.T) {
	got := Budget(APIOpenRouter, "https://openrouter.ai/api/v1", budgetFixture(), nil)

	// families the curated rules name (deepseek, gpt, qwen3, voyageai, gemini) come first by blended
	// price, then everything else; ":free", tiny context, unknown price and router prices never appear
	if want := []string{"deepseek/deepseek-v4.1-flash", "openai/gpt-6-luna", "vendor/cheap-nobody"}; !slices.Equal(ids(got["response"]), want) {
		t.Errorf("response = %v, want %v", ids(got["response"]), want)
	}
	// planner needs tools: the model that says "no tools" is out; the one that does not say stays eligible
	if slices.Contains(ids(got["planner"]), "vendor/cheapest-notools") {
		t.Errorf("planner took a model without tools: %v", ids(got["planner"]))
	}
	// summary needs no tools, so the cheapest unknown-family model is only a fallback behind known families
	if want := []string{"deepseek/deepseek-v4.1-flash", "openai/gpt-6-luna", "vendor/cheapest-notools"}; !slices.Equal(ids(got["summary"]), want) {
		t.Errorf("summary = %v, want %v", ids(got["summary"]), want)
	}
	// vision: only models whose list says image input; one that does not say (nil) or says no is out
	for _, id := range ids(got["vision"]) {
		if id == "deepseek/deepseek-v4.1-flash" || id == "vendor/unknown-tools" || id == "vendor/no-price" {
			t.Errorf("vision took %s", id)
		}
	}
	if want := []string{"openai/gpt-6-luna", "vendor/cheapest-notools", "google/gemini-3.1-flash"}; !slices.Equal(ids(got["vision"]), want) {
		t.Errorf("vision = %v, want %v", ids(got["vision"]), want)
	}
	// embed and rerank: their own kinds; image: models that write pictures, never a text role's pick
	if want := []string{"qwen/qwen3-embedding-8b", "vendor/embed-cheap"}; !slices.Equal(ids(got["embed"]), want) {
		t.Errorf("embed = %v, want %v", ids(got["embed"]), want)
	}
	if want := []string{"voyageai/rerank-3-lite"}; !slices.Equal(ids(got["rerank"]), want) {
		t.Errorf("rerank = %v, want %v", ids(got["rerank"]), want)
	}
	if want := []string{"google/gemini-3.1-flash-lite-image", "google/gemini-3.1-flash-image"}; !slices.Equal(ids(got["image"]), want) {
		t.Errorf("image = %v, want %v", ids(got["image"]), want)
	}
	for _, role := range []string{"response", "planner", "summary", "vision"} {
		for _, id := range ids(got[role]) {
			if id == "google/gemini-3.1-flash-image" || id == "google/gemini-3.1-flash-lite-image" {
				t.Errorf("%s took a picture-writing model %s", role, id)
			}
		}
	}
}

func TestBudgetTopThreeAndFamilyPreference(t *testing.T) {
	var models []Model
	for _, id := range []string{"a/one", "a/two", "a/three", "a/four", "a/five"} {
		models = append(models, Model{ID: id, Kind: "chat", ContextLength: 64000, PromptPrice: "0.000001", CompletionPrice: "0.000001", SupportsTools: bp(true)})
	}
	// cheaper than every unknown family, so without the family preference it would be first; a known family
	// that costs more still outranks the cheaper strangers
	models = append(models, Model{ID: "deepseek/deepseek-big", Kind: "chat", ContextLength: 64000, PromptPrice: "0.000005", CompletionPrice: "0.000005", SupportsTools: bp(true)})
	got := Budget(APIOpenRouter, "https://openrouter.ai/api/v1", models, nil)["response"]
	if len(got) != BudgetTop {
		t.Fatalf("top %d expected, got %v", BudgetTop, ids(got))
	}
	if got[0].ID != "deepseek/deepseek-big" || !got[0].Known {
		t.Errorf("a known family comes first: %+v", got[0])
	}
	if got[1].Known || got[1].BlendedPerMTok != 1 {
		t.Errorf("then the cheapest others: %+v", got[1])
	}
	// among equal prices the order is stable (by ID)
	if got[1].ID != "a/five" || got[2].ID != "a/four" {
		t.Errorf("ties by ID: %v", ids(got))
	}
}

func TestBudgetBlendedPriceAndMetadata(t *testing.T) {
	// input 2, output 10 per Mtok: (3×2 + 10) / 4 = 4
	models := []Model{
		{ID: "x/in-heavy", Name: "In heavy", Kind: "chat", ContextLength: 100000, PromptPrice: "0.000002", CompletionPrice: "0.00001", CachedPrice: "0.0000005", SupportsTools: bp(true), ImageInput: bp(true)},
		{ID: "x/out-heavy", Kind: "chat", ContextLength: 100000, PromptPrice: "0.000001", CompletionPrice: "0.000015", SupportsTools: bp(true)}, // (3+15)/4 = 4.5
	}
	got := Budget(APIOpenRouter, "https://openrouter.ai/api/v1", models, nil)["response"]
	if len(got) != 2 || got[0].ID != "x/in-heavy" || got[1].ID != "x/out-heavy" {
		t.Fatalf("ranked by blended price, not by input alone: %v", ids(got))
	}
	b := got[0]
	if b.BlendedPerMTok != 4 || b.InPerMTok != 2 || b.OutPerMTok != 10 || b.CachedPerMTok != 0.5 || b.ContextLength != 100000 || b.PriceSource != "list" ||
		b.SupportsTools == nil || !*b.SupportsTools || b.ImageInput == nil || !*b.ImageInput || b.Name != "In heavy" {
		t.Errorf("metadata: %+v", b)
	}
}

// Name-based lists (OpenAI, Gemini) carry no prices: a model the family's pricing page lists is ranked at
// that price, one it does not list is skipped rather than failing or counting as free.
func TestBudgetNameBasedListUsesPublishedPricesOrSkips(t *testing.T) {
	models := []Model{
		{ID: "models/gemini-3.8-flash", Kind: "chat"},
		{ID: "models/gemini-made-up-by-nobody", Kind: "chat"},
	}
	got := Budget(APIGemini, "https://generativelanguage.googleapis.com/v1beta/openai", models, nil)
	if pub, ok := Published(APIGemini, "models/gemini-3.8-flash"); ok {
		r := got["response"]
		if len(r) != 1 || r[0].ID != "models/gemini-3.8-flash" || r[0].PriceSource != "published" || r[0].InPerMTok != pub.In*1e6 {
			t.Errorf("published price used: %+v", r)
		}
	}
	for role, picks := range got {
		for _, p := range picks {
			if p.ID == "models/gemini-made-up-by-nobody" {
				t.Errorf("%s ranked a model with no known price", role)
			}
		}
	}
	// the list cannot say whether pictures are accepted; hosts.json names the family's multimodal models
	if v := got["vision"]; len(v) != 1 || v[0].ID != "models/gemini-3.8-flash" || v[0].ImageInput == nil || !*v[0].ImageInput {
		t.Errorf("vision takes the published multimodal model only: %v", v)
	}
}

// hosts.json's image_input flag fills in for a list that does not say, never overrides one that does, and
// never names an embedding or picture-writing model; a family without the table stays unknown.
func TestBudgetVisionUsesPublishedImageInput(t *testing.T) {
	models := []Model{
		{ID: "gpt-5.4-nano", Kind: "chat"},
		{ID: "gpt-5-mini", Kind: "chat", ImageInput: bp(false)}, // the list says no: it wins
		{ID: "text-embedding-3-small", Kind: "embedding"},
		{ID: "gpt-image-1-mini", Kind: "chat", ImageOutput: true},
		{ID: "gpt-5.2", Kind: "chat"},
	}
	got := Budget(APIOpenAI, "https://api.openai.com/v1", models, nil)
	if want := []string{"gpt-5.4-nano", "gpt-5.2"}; !slices.Equal(ids(got["vision"]), want) {
		t.Errorf("vision = %v, want %v", ids(got["vision"]), want)
	}
	for api, id := range map[API]string{APIOpenAI: "gpt-5.4-nano", APIGemini: "models/gemini-2.5-flash-lite"} {
		if !PublishedImageInput(api, id) {
			t.Errorf("%s is a multimodal model of its family", id)
		}
	}
	for _, id := range []string{"text-embedding-3-small", "gpt-image-2", "gemini-embedding-2", "gemini-3.1-flash-image"} {
		if PublishedImageInput(APIOpenAI, id) || PublishedImageInput(APIGemini, id) {
			t.Errorf("%s is not a picture-reading chat model", id)
		}
	}
	reseller := []Model{{ID: "gpt-5.4-nano", Kind: "chat", PromptPrice: "0.0000002", CompletionPrice: "0.000001"}}
	if got := Budget(APICompatible, "https://example.com/v1", reseller, nil); len(got["vision"]) != 0 {
		t.Errorf("a reseller's list is not the family's: %v", ids(got["vision"]))
	}
}

func TestBudgetEmptyAndUnpricedLists(t *testing.T) {
	if got := Budget(APIOpenRouter, "", nil, nil); len(got) != 0 {
		t.Errorf("empty list: %v", got)
	}
	unpriced := []Model{{ID: "m1", Kind: "chat", ContextLength: 64000}, {ID: "m2", Kind: "chat", PromptPrice: "0", CompletionPrice: "0"}}
	if got := Budget(APICompatible, "https://example.com/v1", unpriced, nil); len(got) != 0 {
		t.Errorf("no prices, no ranking (and no error): %v", got)
	}
}

// Variants a bot cannot run on are never offered, however cheap: batch-only, moving aliases, and a chat
// model with no output price (an embedding model listed as chat). The batch variant of an embedding
// model is skipped too.
func TestBudgetSkipsBatchAliasesAndOutputlessChatModels(t *testing.T) {
	tools := bp(true)
	models := []Model{
		{ID: "openai/gpt-oss-20b:batch", Kind: "chat", ContextLength: 131072, PromptPrice: "0.00000001", CompletionPrice: "0.00000005", SupportsTools: tools},
		{ID: "~deepseek/deepseek-flash-latest", Kind: "chat", ContextLength: 131072, PromptPrice: "0.00000001", CompletionPrice: "0.00000005", SupportsTools: tools, ImageInput: tools},
		{ID: "voyageai/voyage-4-lite", Kind: "chat", ContextLength: 32000, PromptPrice: "0.00000002", CompletionPrice: "0"},
		{ID: "google/gemini-embedding-2:batch", Kind: "embedding", PromptPrice: "0.00000001"},
		{ID: "openai/gpt-oss-20b", Kind: "chat", ContextLength: 131072, PromptPrice: "0.000000018", CompletionPrice: "0.00000009", SupportsTools: tools},
		{ID: "qwen/qwen3-embedding-8b", Kind: "embedding", PromptPrice: "0.00000001"},
	}
	got := Budget(APIOpenRouter, "", models, nil)
	for _, role := range []string{"response", "planner", "summary"} {
		if want := []string{"openai/gpt-oss-20b"}; !slices.Equal(ids(got[role]), want) {
			t.Errorf("%s = %v, want %v", role, ids(got[role]), want)
		}
	}
	if len(got["vision"]) != 0 {
		t.Errorf("vision offered an alias: %v", ids(got["vision"]))
	}
	if want := []string{"qwen/qwen3-embedding-8b"}; !slices.Equal(ids(got["embed"]), want) {
		t.Errorf("embed = %v, want %v", ids(got["embed"]), want)
	}
}

// The OpenRouter-shaped fixture: input_modalities becomes ImageInput (true, false), and a model whose
// entry names none stays unknown (nil).
func TestParseModelsImageInput(t *testing.T) {
	body, err := os.ReadFile("testdata/openrouter_models.json")
	if err != nil {
		t.Fatal(err)
	}
	models, err := ParseModels("application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]Model{}
	for _, m := range models {
		by[m.ID] = m
	}
	if m := by["openai/gpt-6-luna"]; m.ImageInput == nil || !*m.ImageInput {
		t.Errorf("image in input_modalities: %+v", m.ImageInput)
	}
	if m := by["deepseek/deepseek-v4.1-flash"]; m.ImageInput == nil || *m.ImageInput {
		t.Errorf("text-only input_modalities: %+v", m.ImageInput)
	}
	named, err := ParseModels("application/json", []byte(`{"data":[{"id":"gpt-5-mini","object":"model"},{"id":"x/y","context_length":8000,"architecture":{"output_modalities":["text"]}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range named {
		if m.ImageInput != nil {
			t.Errorf("%s: a list that does not say is unknown, got %v", m.ID, *m.ImageInput)
		}
	}
}
