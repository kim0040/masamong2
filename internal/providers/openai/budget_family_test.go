package openai

import (
	"slices"
	"testing"
)

func chatModel(id string, in, out string) Model {
	return Model{ID: id, Name: id, Kind: "chat", ContextLength: 128000, PromptPrice: in, CompletionPrice: out, SupportsTools: bp(true), ImageInput: bp(true)}
}

const orURL = "https://openrouter.ai/api/v1"

// Known families belong to the role: a family the embed rule names is not "known" for chat roles, and the
// chat roles use the chat-kind rules (response, planner, summary).
func TestBudgetFamiliesFollowTheRole(t *testing.T) {
	models := []Model{
		chatModel("deepseek/deepseek-v4-flash", "0.0000002", "0.0000004"),
		chatModel("qwen/qwen3-chat", "0.00000001", "0.00000002"), // cheaper, but qwen3 is the embedding rule's family
		{ID: "qwen/qwen3-embedding-8b", Kind: "embedding", ContextLength: 32000, PromptPrice: "0.00000001", CompletionPrice: "0"},
		{ID: "vendor/embed-cheaper", Kind: "embedding", ContextLength: 32000, PromptPrice: "0.000000005", CompletionPrice: "0"},
	}
	got := Budget(APIOpenRouter, orURL, models, nil)
	if want := []string{"deepseek/deepseek-v4-flash", "qwen/qwen3-chat"}; !slices.Equal(ids(got["response"]), want) {
		t.Errorf("response = %v, want %v (qwen3 is not a chat family)", ids(got["response"]), want)
	}
	if got["response"][1].Known {
		t.Errorf("qwen3 chat model marked known: %+v", got["response"][1])
	}
	if want := []string{"qwen/qwen3-embedding-8b", "vendor/embed-cheaper"}; !slices.Equal(ids(got["embed"]), want) || !got["embed"][0].Known {
		t.Errorf("embed = %v, want %v (qwen3 is the embed family)", ids(got["embed"]), want)
	}
}

// A family appears once, with the cheapest model of its newest generation: an old cheap model does not
// win on price, a parameter count is not a generation, and the top three are three families.
func TestBudgetOneModelPerFamilyNewestGeneration(t *testing.T) {
	models := []Model{
		chatModel("deepseek/deepseek-v3-flash", "0.00000001", "0.00000002"), // old and cheapest
		chatModel("deepseek/deepseek-v4-pro", "0.0000006", "0.0000012"),
		chatModel("deepseek/deepseek-v4-flash", "0.0000002", "0.0000004"),
		chatModel("openai/gpt-5-nano", "0.00000002", "0.00000004"),
		chatModel("openai/gpt-oss-20b", "0.00000003", "0.00000005"), // "20b" is a size, not generation 20
		chatModel("openai/gpt-6-luna", "0.0000001", "0.0000005"),
		chatModel("openai/gpt-6.1-sol", "0.000002", "0.00001"), // a dear point release is the same generation
		chatModel("acme/zeta-2", "0.0000004", "0.0000008"),
		chatModel("acme/zeta-1", "0.00000002", "0.00000004"),
		chatModel("other/omega", "0.0000009", "0.0000018"),
	}
	got := Budget(APIOpenRouter, orURL, models, nil)["response"]
	want := []string{"openai/gpt-6-luna", "deepseek/deepseek-v4-flash", "acme/zeta-2"}
	if !slices.Equal(ids(got), want) {
		t.Fatalf("response = %v, want %v", ids(got), want)
	}
}

// With fewer families than slots the rest of the slots are filled from the remaining models in rank order.
func TestBudgetFillsSlotsWhenFewFamilies(t *testing.T) {
	models := []Model{
		chatModel("deepseek/deepseek-v4-flash", "0.0000002", "0.0000004"),
		chatModel("deepseek/deepseek-v4-lite", "0.0000001", "0.0000002"),
		chatModel("deepseek/deepseek-v3-flash", "0.00000005", "0.0000001"),
		chatModel("deepseek/deepseek-v4-max", "0.000001", "0.000002"),
	}
	got := Budget(APIOpenRouter, orURL, models, nil)["response"]
	if want := []string{"deepseek/deepseek-v4-lite", "deepseek/deepseek-v3-flash", "deepseek/deepseek-v4-flash"}; !slices.Equal(ids(got), want) {
		t.Fatalf("response = %v, want %v", ids(got), want)
	}
}

// OpenRouter routing suffixes are not different models: :online/:nitro/:thinking/:extended are skipped, and
// any other :suffix whose base is listed too leaves the base. A suffix without its base stays. Preview and
// experimental models are skipped however cheap.
func TestBudgetSkipsSuffixVariants(t *testing.T) {
	models := []Model{
		chatModel("x/m", "0.000001", "0.000002"),
		chatModel("x/m:online", "0.0000001", "0.0000002"),
		chatModel("x/m:nitro", "0.0000001", "0.0000002"),
		chatModel("x/m:thinking", "0.0000001", "0.0000002"),
		chatModel("x/m:extended", "0.0000001", "0.0000002"),
		chatModel("x/m:beta", "0.0000001", "0.0000002"),
		chatModel("x/m-preview", "0.00000001", "0.00000002"),
		chatModel("x/m-vision-exp", "0.00000001", "0.00000002"),
		chatModel("y/n:beta", "0.0000003", "0.0000006"),
	}
	got := Budget(APIOpenRouter, orURL, models, nil)["response"]
	if want := []string{"y/n:beta", "x/m"}; !slices.Equal(ids(got), want) {
		t.Fatalf("response = %v, want %v", ids(got), want)
	}
}

// The ranking reads prices through the caller's resolver: a model the list does not price can be priced from
// elsewhere (the reference list), and a chat model with no known output price is skipped.
func TestBudgetUsesTheCallersPriceResolver(t *testing.T) {
	models := []Model{
		{ID: "vendor/bare", Kind: "chat", ContextLength: 64000},
		{ID: "vendor/half", Kind: "chat", ContextLength: 64000},
	}
	price := func(m Model) (ModelPrice, bool) {
		switch m.ID {
		case "vendor/bare":
			return ModelPrice{In: 0.2, Out: 0.4, OutKnown: true, Source: "reference"}, true
		case "vendor/half":
			return ModelPrice{In: 0.1, OutKnown: false, Source: "list"}, true
		}
		return ModelPrice{}, false
	}
	got := Budget(APICompatible, "https://example.com/v1", models, price)["response"]
	if len(got) != 1 || got[0].ID != "vendor/bare" || got[0].PriceSource != "reference" || got[0].OutPerMTok != 0.4 {
		t.Fatalf("response = %+v", got)
	}
}
