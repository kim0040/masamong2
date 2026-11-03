package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"strings"
	"testing"
	"time"

	"masamong/internal/providers"
)

// TestLiveAnthropicNative calls Claude Haiku 5.5 through the Messages path: a plain answer at low and at
// high effort (thinking tokens), one tool round trip with adaptive thinking whose thinking blocks go back
// unchanged, a conversation that starts with the model's turn, and a picture. About six tiny calls
// (max_tokens ≤ 300 before the thinking room). It runs only with MASAMONG_LIVE_ANTHROPIC_KEY_FILE naming
// a key file; the key is never printed.
//
//	MASAMONG_LIVE_ANTHROPIC_KEY_FILE=.private/test-keys/anthropic go test -run TestLiveAnthropicNative -v ./internal/providers/openai
func TestLiveAnthropicNative(t *testing.T) {
	file := os.Getenv("MASAMONG_LIVE_ANTHROPIC_KEY_FILE")
	if file == "" {
		t.Skip("set MASAMONG_LIVE_ANTHROPIC_KEY_FILE to a key file")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MASAMONG_LIVE_ANTHROPIC", strings.TrimSpace(string(raw)))
	cred, _ := providers.Resolve("env:MASAMONG_LIVE_ANTHROPIC")
	c := providers.NewClient(60*time.Second, nil)
	c.MaxRetries = 1
	a := New(c, "https://api.anthropic.com/v1", cred)
	a.Price = func(model string) (float64, float64, bool) {
		p, ok := Published(APIAnthropic, model)
		return p.In, p.Out, ok
	}
	ctx := context.Background()
	const model = "claude-haiku-5-5"
	caps := a.CapsOf(ctx, model)
	t.Logf("caps: reasoning=%v efforts=%v thinking=%v off=%s", deref(caps.Reasoning), caps.Efforts, caps.Thinking, caps.ThinkingOff())
	report := func(name string, res ChatResult, err error) {
		cost := -1.0
		if res.CostUSD != nil {
			cost = *res.CostUSD
		}
		var blocks []string
		var msg struct {
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
		}
		_ = json.Unmarshal(res.Message, &msg)
		for _, b := range msg.Content {
			blocks = append(blocks, b.Type)
		}
		t.Logf("%s: err=%v finish=%s in=%d out=%d thinking=%d cached=%d blocks=%v calls=%d cost=$%.6f text=%q", name, err, res.FinishReason,
			res.PromptTokens, res.CompletionTokens, res.ReasoningTokens, res.CachedTokens, blocks, len(res.ToolCalls), cost, clipText(res.Content, 80))
	}
	puzzle := "A bat and a ball cost 1.10 in total. The bat costs 1.00 more than the ball. How much is the ball? Answer with the number only."
	for _, effort := range []string{"low", "high"} {
		res, err := a.Chat(ctx, ChatRequest{Model: model, MaxTokens: 60, Effort: effort, Messages: []Message{{Role: "user", Content: puzzle}}})
		report("plain "+effort, res, err)
		if err != nil {
			t.Fatalf("plain %s: %v", effort, err)
		}
	}
	tool := Tool{Function: ToolFunction{Name: "weather_now", Description: "Current weather of a city",
		Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`)}}
	msgs := []Message{{Role: "system", Content: "Use the tool for weather questions. Answer in one short sentence."}, {Role: "user", Content: "What is the weather in Paris now?"}}
	first, err := a.Chat(ctx, ChatRequest{Model: model, MaxTokens: 300, Effort: "high", Tools: []Tool{tool}, ToolChoice: "auto", Messages: msgs})
	report("tool round 1", first, err)
	if err != nil || len(first.ToolCalls) == 0 {
		t.Fatalf("no tool call: %v", err)
	}
	msgs = append(msgs, Message{Role: "assistant", Raw: first.Message, ToolCalls: first.ToolCalls})
	for _, tc := range first.ToolCalls {
		msgs = append(msgs, Message{Role: "tool", ToolCallID: tc.ID, Content: `{"city":"Paris","temp_c":14,"sky":"light rain"}`})
	}
	second, err := a.Chat(ctx, ChatRequest{Model: model, MaxTokens: 300, Effort: "high", Tools: []Tool{tool}, ToolChoice: "auto", Messages: msgs})
	report("tool round 2", second, err)
	if err != nil || strings.TrimSpace(second.Content) == "" {
		t.Fatalf("no answer after the tool: %v", err)
	}
	asst, err := a.Chat(ctx, ChatRequest{Model: model, MaxTokens: 40, Effort: "none", Messages: []Message{
		{Role: "system", Content: "Be brief."}, {Role: "assistant", Content: "Hello, I am the bot."}, {Role: "user", Content: "What did you just say? Quote it."}}})
	report("assistant first", asst, err)
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for x := 0; x < 16; x++ {
		for y := 0; y < 16; y++ {
			img.Set(x, y, color.RGBA{R: 220, A: 255})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	pic, err := a.Chat(ctx, ChatRequest{Model: model, MaxTokens: 20, Effort: "none", Messages: []Message{
		{Role: "user", Content: "What color is this square? One word.", Images: []ImagePart{{MIME: "image/png", Data: buf.Bytes()}}}}})
	report("image", pic, err)
}

func deref(b *bool) any {
	if b == nil {
		return nil
	}
	return *b
}

func clipText(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// TestLiveAnthropicThinkingToolLoop is a tool round trip where the model thinks before the call (effort
// max on a question that needs a plan), so the thinking block with its signature goes back unchanged in
// the continuation. Two tiny calls; same key variable as TestLiveAnthropicNative.
func TestLiveAnthropicThinkingToolLoop(t *testing.T) {
	file := os.Getenv("MASAMONG_LIVE_ANTHROPIC_KEY_FILE")
	if file == "" {
		t.Skip("set MASAMONG_LIVE_ANTHROPIC_KEY_FILE to a key file")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MASAMONG_LIVE_ANTHROPIC", strings.TrimSpace(string(raw)))
	cred, _ := providers.Resolve("env:MASAMONG_LIVE_ANTHROPIC")
	c := providers.NewClient(60*time.Second, nil)
	a := New(c, "https://api.anthropic.com/v1", cred)
	ctx := context.Background()
	tool := Tool{Function: ToolFunction{Name: "temperature_c", Description: "Current temperature of a city in Celsius",
		Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`)}}
	msgs := []Message{{Role: "user", Content: "Think step by step about which tool calls you need first. Is it warmer now in Oslo or in Cairo, and by how many degrees? Use the tool, one call per city."}}
	first, err := a.Chat(ctx, ChatRequest{Model: "claude-haiku-5-5", MaxTokens: 300, Effort: "max", Tools: []Tool{tool}, ToolChoice: "auto", Messages: msgs})
	var m struct {
		Content []struct {
			Type      string `json:"type"`
			Signature string `json:"signature"`
		} `json:"content"`
	}
	_ = json.Unmarshal(first.Message, &m)
	var kinds []string
	for _, b := range m.Content {
		k := b.Type
		if b.Signature != "" {
			k += "(signed)"
		}
		kinds = append(kinds, k)
	}
	t.Logf("round 1: err=%v finish=%s out=%d thinking=%d blocks=%v calls=%d", err, first.FinishReason, first.CompletionTokens, first.ReasoningTokens, kinds, len(first.ToolCalls))
	if err != nil || len(first.ToolCalls) == 0 {
		t.Fatalf("no tool call: %v", err)
	}
	msgs = append(msgs, Message{Role: "assistant", Raw: first.Message, ToolCalls: first.ToolCalls})
	for i, tc := range first.ToolCalls {
		msgs = append(msgs, Message{Role: "tool", ToolCallID: tc.ID, Content: []string{`{"temp_c":4}`, `{"temp_c":27}`}[min(i, 1)]})
	}
	second, err := a.Chat(ctx, ChatRequest{Model: "claude-haiku-5-5", MaxTokens: 300, Effort: "max", Tools: []Tool{tool}, ToolChoice: "auto", Messages: msgs})
	t.Logf("round 2: err=%v finish=%s out=%d thinking=%d calls=%d text=%q", err, second.FinishReason, second.CompletionTokens, second.ReasoningTokens, len(second.ToolCalls), clipText(second.Content, 100))
	if err != nil {
		t.Fatal(err)
	}
}

// TestLiveAnthropicFormatThirdParty calls a third-party Anthropic-format endpoint (CometAPI documents
// its Messages route at https://api.cometapi.com, /v1/messages): the model list (free), then a Haiku
// answer with thinking off and one at effort low. Two tiny calls (more only if the host refuses a field
// and the adapter learns its shape). The key file is read from MASAMONG_LIVE_COMETAPI_KEY_FILE and never
// printed; MASAMONG_LIVE_COMETAPI_MODEL picks another model.
func TestLiveAnthropicFormatThirdParty(t *testing.T) {
	file := os.Getenv("MASAMONG_LIVE_COMETAPI_KEY_FILE")
	if file == "" {
		t.Skip("set MASAMONG_LIVE_COMETAPI_KEY_FILE to a key file")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MASAMONG_LIVE_COMETAPI", strings.TrimSpace(string(raw)))
	cred, _ := providers.Resolve("env:MASAMONG_LIVE_COMETAPI")
	c := providers.NewClient(60*time.Second, nil)
	c.MaxRetries = 0
	a := New(c, "https://api.cometapi.com", cred)
	a.API = APIAnthropic
	ctx := context.Background()
	model := os.Getenv("MASAMONG_LIVE_COMETAPI_MODEL")
	if model == "" {
		model = "claude-haiku-4-5-20251001"
	}
	ms, err := a.ListModels(ctx)
	listed, withCaps := false, 0
	for _, m := range ms {
		listed = listed || m.ID == model
		if m.Caps().Known() {
			withCaps++
		}
	}
	t.Logf("list: err=%v models=%d listed=%v with_caps=%d caps=%+v", err, len(ms), listed, withCaps, a.CapsOf(ctx, model))
	for _, effort := range []string{"none", "low"} {
		res, err := a.Chat(ctx, ChatRequest{Model: model, MaxTokens: 40, Effort: effort, Messages: []Message{
			{Role: "system", Content: "Answer with one word."}, {Role: "user", Content: "What colour is a clear daytime sky?"}}})
		var blocks []string
		var msg struct {
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
		}
		_ = json.Unmarshal(res.Message, &msg)
		for _, b := range msg.Content {
			blocks = append(blocks, b.Type)
		}
		t.Logf("%s: err=%v finish=%s in=%d out=%d thinking=%d blocks=%v text=%q", effort, err, res.FinishReason, res.PromptTokens, res.CompletionTokens, res.ReasoningTokens, blocks, clipText(res.Content, 40))
		if err != nil {
			t.Fatalf("%s: %v", effort, err)
		}
	}
}
