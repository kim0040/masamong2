package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/providers"
)

func TestDetectAPIByHost(t *testing.T) {
	for url, want := range map[string]API{
		"https://openrouter.ai/api/v1":                              APIOpenRouter,
		"https://api.openai.com/v1":                                 APIOpenAI,
		"https://generativelanguage.googleapis.com/v1beta/openai":   APIGemini,
		"https://api.deepseek.com/v1":                               APICompatible,
		"https://proxy.example/openrouter.ai/api/v1":                APICompatible, // a path is not a host
		"http://127.0.0.1:9999/v1":                                  APICompatible,
		"https://generativelanguage.googleapis.com.evil.example/v1": APICompatible,
	} {
		if got := DetectAPI(url); got != want {
			t.Errorf("%s: %s want %s", url, got, want)
		}
	}
	if ResolveAPI("gemini", "http://127.0.0.1/v1") != APIGemini || ResolveAPI("", "https://api.openai.com/v1") != APIOpenAI {
		t.Fatal("configured family wins, else detected")
	}
}

// Gemini and OpenAI list names only; the kind comes from the name.
func TestModelKindFromNames(t *testing.T) {
	for id, want := range map[string]string{
		"gemini-3.8-flash": "chat", "gemini-3.5-flash-lite": "chat", "gemini-3.1-pro-preview": "chat", "gpt-5.4-mini": "chat",
		"gemini-embedding-2": "embedding", "text-embedding-3-large": "embedding",
		"gemini-3.1-flash-image": "image", "gpt-image-1": "image", "dall-e-3": "image", "imagen-4.0-generate-001": "image",
		"gemini-3.8-flash-tts": "other", "gemini-3.8-live": "other", "gemini-2.5-flash-native-audio-latest": "other",
		"veo-3.1-generate-preview": "other", "lyria-3.5": "other", "whisper-1": "other", "gpt-4o-realtime-preview": "other",
		"omni-moderation-latest": "other", "deep-research-preview-04-2026": "other", "gemini-flash-latest": "chat",
	} {
		if got, _ := ModelKind(id); got != want {
			t.Errorf("%s: %s want %s", id, got, want)
		}
	}
}

type recorded struct {
	mu     sync.Mutex
	bodies []map[string]any
}

func (r *recorded) add(req *http.Request) map[string]any {
	raw, _ := io.ReadAll(req.Body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	r.mu.Lock()
	r.bodies = append(r.bodies, m)
	r.mu.Unlock()
	return m
}

func testAdapter(t *testing.T, api API, h http.HandlerFunc) *Adapter {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := providers.NewClient(5*time.Second, nil)
	c.MaxRetries = 0
	t.Setenv("K", "test-key")
	cred, _ := providers.Resolve("env:K")
	a := New(c, srv.URL+"/v1beta/openai", cred)
	a.API = api
	return a
}

const geminiChatOK = `{"choices":[{"finish_reason":"stop","index":0,"message":{"content":"하늘은 파래요.","extra_content":{"google":{"thought_signature":"xx"}},"role":"assistant"}}],"created":1,"id":"a","model":"gemini-3.8-flash","object":"chat.completion","usage":{"completion_tokens":9,"prompt_tokens":3,"total_tokens":12}}`

// Recorded from Gemini (2026-09-30): the reasoning object OpenRouter takes is refused in a list-wrapped
// error, and a level the model does not have is refused without naming the field.
func TestGeminiRefusalsAreRetriedWithoutTheFieldAndRemembered(t *testing.T) {
	var rec recorded
	a := testAdapter(t, APIGemini, func(w http.ResponseWriter, r *http.Request) {
		body := rec.add(r)
		if _, ok := body["reasoning_effort"]; ok {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`[{"error":{"code":400,"message":"Request contains an invalid argument.","status":"INVALID_ARGUMENT"}}]`))
			return
		}
		_, _ = w.Write([]byte(geminiChatOK))
	})
	temp := 0.3
	req := ChatRequest{Model: "gemini-3.5-flash-lite", Messages: []Message{{Role: "user", Content: "hi"}}, MaxTokens: 50, Temperature: &temp, Effort: "low"}
	res, err := a.Chat(context.Background(), req)
	if err != nil || res.Content != "하늘은 파래요." {
		t.Fatalf("chat: %v %+v", err, res)
	}
	if len(rec.bodies) != 2 || rec.bodies[0]["reasoning_effort"] != "low" || rec.bodies[0]["max_tokens"] != float64(50) || rec.bodies[1]["temperature"] != 0.3 {
		t.Fatalf("first try has the Gemini fields, the retry drops only the effort: %v", rec.bodies)
	}
	if _, ok := rec.bodies[0]["reasoning"]; ok {
		t.Fatal("the OpenRouter reasoning object is never sent to Gemini")
	}
	if _, err := a.Chat(context.Background(), req); err != nil || len(rec.bodies) != 3 {
		t.Fatalf("the model's shape is remembered: one request, got %d (%v)", len(rec.bodies), err)
	}
}

func TestPayloadPerFamily(t *testing.T) {
	temp := 0.2
	req := ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}, MaxTokens: 80, Temperature: &temp, Effort: "low"}
	or := (&Adapter{API: APIOpenRouter}).payload(req, shape{})
	oa := (&Adapter{API: APIOpenAI}).payload(req, shape{})
	gm := (&Adapter{API: APIGemini}).payload(req, shape{})
	if or["reasoning"].(map[string]any)["effort"] != "low" || or["max_tokens"] != 80 {
		t.Fatalf("openrouter: %v", or)
	}
	if oa["reasoning_effort"] != "low" || oa["max_completion_tokens"] != 80 || oa["max_tokens"] != nil {
		t.Fatalf("openai: %v", oa)
	}
	if gm["reasoning_effort"] != "low" || gm["max_tokens"] != 80 {
		t.Fatalf("gemini: %v", gm)
	}
}

// OpenAI's non-reasoning models refuse reasoning_effort by name; reasoning models refuse temperature.
func TestOpenAINamedRefusals(t *testing.T) {
	var rec recorded
	a := testAdapter(t, APIOpenAI, func(w http.ResponseWriter, r *http.Request) {
		body := rec.add(r)
		switch {
		case body["reasoning_effort"] != nil:
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":{"message":"Unrecognized request argument supplied: reasoning_effort","type":"invalid_request_error","param":null,"code":null}}`))
		case body["temperature"] != nil:
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":{"message":"Unsupported value: 'temperature' does not support 0.2 with this model.","type":"invalid_request_error","param":"temperature","code":"unsupported_value"}}`))
		default:
			_, _ = w.Write([]byte(geminiChatOK))
		}
	})
	temp := 0.2
	if _, err := a.Chat(context.Background(), ChatRequest{Model: "x", Messages: []Message{{Role: "user", Content: "x"}}, Temperature: &temp, Effort: "low"}); err != nil {
		t.Fatal(err)
	}
	if len(rec.bodies) != 3 || rec.bodies[2]["temperature"] != nil || rec.bodies[2]["reasoning_effort"] != nil {
		t.Fatalf("two named refusals, two retries: %v", rec.bodies)
	}
}

// A refused key is never retried, and Gemini's 400 "valid API key" is a refused key.
func TestGeminiBadKeyIsAuthNotRetried(t *testing.T) {
	var rec recorded
	a := testAdapter(t, APIGemini, func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`[{"error":{"code":400,"message":"Please pass a valid API key","status":"INVALID_ARGUMENT"}}]`))
	})
	_, err := a.Chat(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}, Effort: "low"})
	if !errors.Is(err, ErrAuth) || len(rec.bodies) != 1 {
		t.Fatalf("want ErrAuth after one request, got %v after %d", err, len(rec.bodies))
	}
	if err := a.CheckKey(context.Background()); !errors.Is(err, ErrAuth) {
		t.Fatalf("key check: %v", err)
	}
}

// Recorded from Gemini: "models/" prefixed IDs with display names and no kind.
func TestGeminiModelList(t *testing.T) {
	a := testAdapter(t, APIGemini, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"models/gemini-3.8-flash","object":"model","owned_by":"google","display_name":"Gemini 3.8 Flash"},{"id":"models/gemini-embedding-2","object":"model","owned_by":"google","display_name":"Gemini Embedding 2"},{"id":"models/gemini-3.1-flash-image","object":"model","owned_by":"google","display_name":"Nano Banana 2"},{"id":"models/gemini-3.8-flash-tts","object":"model","owned_by":"google"}]}`))
	})
	ms, err := a.ListModels(context.Background())
	if err != nil || len(ms) != 4 {
		t.Fatalf("%v %v", err, ms)
	}
	got := map[string]Model{}
	for _, m := range ms {
		got[m.ID] = m
	}
	if got["gemini-3.8-flash"].Kind != "chat" || got["gemini-3.8-flash"].Name != "Gemini 3.8 Flash" || got["gemini-embedding-2"].Kind != "embedding" ||
		!got["gemini-3.1-flash-image"].ImageOutput || got["gemini-3.8-flash-tts"].Kind != "other" {
		t.Fatalf("kinds: %+v", got)
	}
}

// Recorded from Gemini: index 0 is omitted and there is no usage, so the cost is estimated.
func TestGeminiEmbeddingsWithoutUsage(t *testing.T) {
	a := testAdapter(t, APIGemini, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"object":"list","data":[{"embedding":[0.1,0.2],"object":"embedding"},{"embedding":[0.3,0.4],"index":1,"object":"embedding"}],"model":"gemini-embedding-2"}`))
	})
	a.Price = func(string) (float64, float64, bool) { return 0.2 / 1e6, 0, true }
	res, err := a.Embed(context.Background(), "gemini-embedding-2", []string{"안녕하세요", "camping tent"})
	if err != nil || res.Vectors[0][0] != 0.1 || res.Vectors[1][0] != 0.3 {
		t.Fatalf("%v %+v", err, res)
	}
	if res.CostUSD == nil || !res.CostEstimated || *res.CostUSD <= 0 {
		t.Fatalf("a cost is estimated from the text: %+v", res)
	}
	if n := EstimateTokens("안녕하세요", "camping tent"); n != 5+3 {
		t.Fatalf("estimate: %d", n)
	}
}

func TestRerankOnlyWhereOffered(t *testing.T) {
	a := testAdapter(t, APIGemini, func(w http.ResponseWriter, r *http.Request) { t.Fatal("no request") })
	if a.CanRerank() {
		t.Fatal("gemini has no rerank endpoint")
	}
	if _, err := a.Rerank(context.Background(), "m", "q", []string{"a"}, 1); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("%v", err)
	}
}

// a 1×1 PNG
var tinyPNG, _ = base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==")

// Recorded from Gemini's native generateContent (image part, then a thought signature; usage).
func TestGeminiImageNative(t *testing.T) {
	var path, key string
	var rec recorded
	a := testAdapter(t, APIGemini, func(w http.ResponseWriter, r *http.Request) {
		path, key = r.URL.Path, r.Header.Get("x-goog-api-key")
		rec.add(r)
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"` + base64.StdEncoding.EncodeToString(tinyPNG) + `"},"thoughtSignature":"xx"}],"role":"model"},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":14,"candidatesTokenCount":1422,"totalTokenCount":1436},"modelVersion":"gemini-3.1-flash-image"}`))
	})
	a.Price = func(string) (float64, float64, bool) { return 0.5 / 1e6, 60 / 1e6, true }
	res, err := a.GenerateImage(context.Background(), "models/gemini-3.1-flash-image", "an apple", "1:1")
	if err != nil || res.MIME != "image/png" || len(res.Data) != len(tinyPNG) {
		t.Fatalf("%v %+v", err, res)
	}
	if path != "/v1beta/models/gemini-3.1-flash-image:generateContent" || key != "test-key" {
		t.Fatalf("native endpoint next to the compatible one: %s key=%q", path, key)
	}
	gc := rec.bodies[0]["generationConfig"].(map[string]any)
	if gc["imageConfig"].(map[string]any)["aspectRatio"] != "1:1" {
		t.Fatalf("aspect ratio: %v", gc)
	}
	if res.CostUSD == nil || *res.CostUSD < 0.085 || *res.CostUSD > 0.086 {
		t.Fatalf("cost from usage: %v", res.CostUSD)
	}
}

func TestOpenAIImageGeneration(t *testing.T) {
	var rec recorded
	a := testAdapter(t, APIOpenAI, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/images/generations") {
			t.Errorf("path %s", r.URL.Path)
		}
		rec.add(r)
		_, _ = w.Write([]byte(`{"created":1,"data":[{"b64_json":"` + base64.StdEncoding.EncodeToString(tinyPNG) + `"}],"usage":{"input_tokens":20,"output_tokens":1056,"total_tokens":1076}}`))
	})
	res, err := a.GenerateImage(context.Background(), "gpt-image-1", "an apple", "16:9")
	if err != nil || res.MIME != "image/png" || rec.bodies[0]["size"] != "1536x1024" {
		t.Fatalf("%v %+v %v", err, res, rec.bodies)
	}
}

// The Gemini list as served on 2026-09-30 (kinds as ListModels sets them).
func TestRecommendFromTheGeminiList(t *testing.T) {
	var ms []Model
	for _, id := range []string{"gemini-2.5-flash", "gemini-2.5-pro", "gemini-2.5-flash-preview-tts", "gemma-4-31b-it", "gemini-flash-latest",
		"gemini-flash-lite-latest", "gemini-2.5-flash-lite", "gemini-2.5-flash-image", "gemini-3-flash-preview", "gemini-3.1-pro-preview",
		"gemini-3.1-flash-lite-preview", "gemini-3.1-flash-lite", "gemini-3-pro-image-preview", "gemini-3-pro-image", "gemini-3.1-flash-image-preview",
		"gemini-3.1-flash-image", "gemini-3.1-flash-lite-image", "gemini-3.5-flash", "gemini-3.5-flash-lite", "gemini-3.6-flash", "gemini-3.7-flash",
		"gemini-3.8-flash", "gemini-3.8-flash-tts", "gemini-3.8-flash-lite-tts", "gemini-embedding-001", "gemini-embedding-2-preview", "gemini-embedding-2",
		"gemini-3.8-live", "deep-research-preview-04-2026"} {
		k, img := ModelKind(id)
		ms = append(ms, Model{ID: "models/" + id, Kind: k, ImageOutput: img})
	}
	got := Recommend(APIGemini, "", ms)
	want := map[string]string{"response": "gemini-3.8-flash", "planner": "gemini-3.8-flash", "summary": "gemini-3.5-flash-lite",
		"embed": "gemini-embedding-2", "image": "gemini-3.1-flash-image"}
	for role, id := range want {
		if got[role] != id {
			t.Errorf("%s: %q want %q", role, got[role], id)
		}
	}
	if _, ok := got["rerank"]; ok {
		t.Error("no rerank on Gemini")
	}
}

func TestRecommendOpenRouterMatchesProductDefaults(t *testing.T) {
	ids := []string{
		"deepseek/deepseek-v4.1-flash",
		"openai/gpt-5.6-luna",
		"qwen/qwen3-embedding-8b",
		"voyageai/rerank-2.5-lite",
		"google/gemini-3.1-flash-image",
	}
	models := make([]Model, 0, len(ids))
	for _, id := range ids {
		kind, image := ModelKind(id)
		models = append(models, Model{ID: id, Kind: kind, ImageOutput: image})
	}
	got := Recommend(APIOpenRouter, "https://openrouter.ai/api/v1", models)
	want := map[string]string{
		"response": "deepseek/deepseek-v4.1-flash",
		"planner":  "openai/gpt-5.6-luna",
		"summary":  "deepseek/deepseek-v4.1-flash",
		"embed":    "qwen/qwen3-embedding-8b",
		"rerank":   "voyageai/rerank-2.5-lite",
		"image":    "google/gemini-3.1-flash-image",
	}
	for role, model := range want {
		if got[role] != model {
			t.Errorf("%s recommendation = %q, want %q (all: %v)", role, got[role], model, got)
		}
	}
}

func TestRecommendFromAnOpenAIList(t *testing.T) {
	var ms []Model
	for _, id := range []string{"gpt-5-mini", "gpt-5.4-mini", "gpt-5.4-nano", "gpt-5-nano", "gpt-5.4", "gpt-4o-mini-audio-preview", "gpt-4o-mini-search-preview",
		"text-embedding-3-small", "text-embedding-3-large", "text-embedding-ada-002", "gpt-image-1", "gpt-image-1-mini", "gpt-image-2", "dall-e-3", "whisper-1"} {
		k, img := ModelKind(id)
		ms = append(ms, Model{ID: id, Kind: k, ImageOutput: img})
	}
	got := Recommend(APIOpenAI, "", ms)
	want := map[string]string{"response": "gpt-5.4-mini", "summary": "gpt-5.4-nano", "embed": "text-embedding-3-large", "image": "gpt-image-2"}
	for role, id := range want {
		if got[role] != id {
			t.Errorf("%s: %q want %q", role, got[role], id)
		}
	}
}

// CometAPI's Gemini image models answer chat with the picture as a markdown data URI in the text
// (recorded 2026-10-01; bytes shortened).
func TestCompatibleImageInMarkdown(t *testing.T) {
	png := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nrest"))
	a := testAdapter(t, APICompatible, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"x","model":"gemini-2.5-flash-image","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"![image](data:image/png;base64,` + png + `)"},"finish_reason":"stop"}],"usage":{"prompt_tokens":15,"completion_tokens":1290,"total_tokens":1305}}`))
	})
	a.Price = func(string) (float64, float64, bool) { return 0.0000005, 0.00006, true } // image output priced per token
	out, err := a.GenerateImage(context.Background(), "gemini-2.5-flash-image", "a dog", "1:1")
	if err != nil || out.MIME != "image/png" || !strings.HasPrefix(string(out.Data), "\x89PNG") || out.Text != "" {
		t.Fatalf("markdown image: %+v %v", out, err)
	}
	if out.CostUSD == nil || *out.CostUSD < 0.0774 || *out.CostUSD > 0.0775 || !out.CostEstimated {
		t.Fatalf("no cost in the answer: priced from its 15+1290 tokens: %v", out.CostUSD)
	}
}

// NanoGPT's image models live on /images/generations (docs.nano-gpt.com, image-generation-openai):
// the chat request is refused, the adapter asks there instead and remembers it for the model. A
// wrong key is a key problem, never a reason to switch.
func TestCompatibleImageFallsBackToGenerations(t *testing.T) {
	var chats, gens int
	var genBody map[string]any
	a := testAdapter(t, APICompatible, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			chats++
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":{"message":"Model qwen-image does not support chat completions","type":"invalid_request_error","code":"model_not_supported"}}`))
		case strings.HasSuffix(r.URL.Path, "/images/generations"):
			gens++
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &genBody)
			_, _ = w.Write([]byte(`{"created":1,"data":[{"b64_json":"` + base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nx")) + `"}]}`))
		}
	})
	for i := 0; i < 2; i++ {
		out, err := a.GenerateImage(context.Background(), "qwen-image", "a dog", "4:3")
		if err != nil || out.MIME != "image/png" {
			t.Fatalf("fallback: %+v %v", out, err)
		}
	}
	if chats != 1 || gens != 2 || genBody["aspect_ratio"] != "4:3" || genBody["size"] != nil {
		t.Fatalf("chat once, then generations with the ratio: chats=%d gens=%d body=%v", chats, gens, genBody)
	}
	bad := testAdapter(t, APICompatible, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":{"message":"Invalid session","type":"invalid_api_key","code":"invalid_api_key","status":401}}`))
	})
	_, err := bad.GenerateImage(context.Background(), "qwen-image", "a dog", "1:1")
	if !errors.Is(err, ErrAuth) || !strings.Contains(err.Error(), "Invalid session") {
		t.Fatalf("a refused key says why (numeric status in the error object): %v", err)
	}
}

// OpenRouter prices an image model's picture tokens apart from its text (recorded 2026-10-01):
// the dearer image_output is the output price, so a picture is not priced as text.
func TestImageOutputPriceInList(t *testing.T) {
	ms, err := ParseModels("application/json", []byte(`{"data":[{"id":"google/gemini-3.1-flash-image","pricing":{"prompt":"0.0000005","completion":"0.000003","image_output":"0.00006"}},{"id":"x/text","pricing":{"prompt":"0.000001","completion":"0.000002"}}]}`))
	if err != nil || ms[0].CompletionPrice != "0.00006" || ms[1].CompletionPrice != "0.000002" {
		t.Fatalf("%+v %v", ms, err)
	}
}

// The live OpenRouter catalog lists picture models as chat models whose output modalities include
// "image": the image rule has to match what ParseModels really returns, not hand-built models.
func TestRecommendOpenRouterFromTheLiveShapedCatalog(t *testing.T) {
	body, err := os.ReadFile("testdata/openrouter_models.json")
	if err != nil {
		t.Fatal(err)
	}
	models, err := ParseModels("application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	var image Model
	for _, m := range models {
		if m.ID == "google/gemini-3.1-flash-image" {
			image = m
		}
	}
	if image.Kind != "chat" || !image.ImageOutput {
		t.Fatalf("fixture must be shaped like the live list (chat + image output): %+v", image)
	}
	got := Recommend(APIOpenRouter, "https://openrouter.ai/api/v1", models)
	want := map[string]string{
		"response": "deepseek/deepseek-v4.1-flash",
		"planner":  "openai/gpt-6-luna",
		"embed":    "qwen/qwen3-embedding-8b",
		"rerank":   "voyageai/rerank-3-lite",
		"image":    "google/gemini-3.1-flash-image",
	}
	for role, id := range want {
		if got[role] != id {
			t.Errorf("%s = %q, want %q (all: %v)", role, got[role], id, got)
		}
	}
	// a chat role never lands on a picture model even when its name matches
	for _, role := range []string{"response", "summary", "planner"} {
		if strings.Contains(got[role], "image") {
			t.Errorf("%s picked a picture model: %q", role, got[role])
		}
	}
}

// Recorded from DeepSeek V4.1 Flash on OpenRouter (2026-10-14): at "low" it thought through all 120
// output tokens and returned no text. The adapter asks once more without thinking and charges both.
func TestThinkingThatUsesTheWholeBudgetIsAskedAgainWithout(t *testing.T) {
	var rec recorded
	a := testAdapter(t, APIOpenRouter, func(w http.ResponseWriter, r *http.Request) {
		body := rec.add(r)
		if body["reasoning"].(map[string]any)["effort"] == "low" {
			_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"length","message":{"role":"assistant","content":"","reasoning":"..."}}],"usage":{"prompt_tokens":300,"completion_tokens":120,"cost":0.0001}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"주말 캠핑 약속"}}],"usage":{"prompt_tokens":300,"completion_tokens":20,"cost":0.00003}}`))
	})
	out, err := a.Chat(context.Background(), ChatRequest{Model: "deepseek/deepseek-v4.1-flash", Messages: []Message{{Role: "user", Content: "요약"}}, MaxTokens: 120, Effort: "low"})
	if err != nil || out.Content != "주말 캠핑 약속" || len(rec.bodies) != 2 {
		t.Fatalf("asked again without thinking: %q %v after %d", out.Content, err, len(rec.bodies))
	}
	if out.CostUSD == nil || *out.CostUSD < 0.000129 || *out.CostUSD > 0.000131 || out.CompletionTokens != 140 {
		t.Fatalf("both calls are charged: %+v", out)
	}

	if _, err := a.Chat(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}, MaxTokens: 120, Effort: "none"}); err != nil || len(rec.bodies) != 3 {
		t.Fatalf("a request without thinking is not repeated: %d %v", len(rec.bodies), err)
	}
}

// A model that takes "low" but refuses "none": the no-thinking retry goes out without the field for that
// one request, and later requests still ask for "low". A failed retry returns its error.
func TestNoThinkingRetryDoesNotChangeTheModelsShape(t *testing.T) {
	var rec recorded
	var cancelled atomic.Bool
	a := testAdapter(t, APIOpenRouter, func(w http.ResponseWriter, r *http.Request) {
		body := rec.add(r)
		eff, _ := body["reasoning"].(map[string]any)
		switch {
		case eff != nil && eff["effort"] == "none":
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":{"message":"reasoning effort 'none' is not supported for this model"}}`))
		case eff != nil:
			_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"length","message":{"role":"assistant","content":""}}],"usage":{"prompt_tokens":10,"completion_tokens":120,"cost":0.0001}}`))
		case cancelled.Load():
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"error":{"message":"User not found."}}`))
		default:
			_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"요약"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"cost":0.00001}}`))
		}
	})
	req := ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}, MaxTokens: 120, Effort: "low"}
	if out, err := a.Chat(context.Background(), req); err != nil || out.Content != "요약" || len(rec.bodies) != 3 {
		t.Fatalf("low (empty) → none (refused) → no field: %q %v after %d", out.Content, err, len(rec.bodies))
	}
	if a.shapeOf(ChatRequest{Model: "m"}).NoEffort {
		t.Fatal("the one-off retry's shape is not kept for the model")
	}
	cancelled.Store(true)
	out, err := a.Chat(context.Background(), req)
	if eff, _ := rec.bodies[3]["reasoning"].(map[string]any); eff == nil || eff["effort"] != "low" {
		t.Fatalf("a later request still asks for low: %v", rec.bodies[3])
	}
	if err == nil || out.CostUSD == nil {
		t.Fatalf("the retry's failure is returned with the first call's cost: %+v %v", out, err)
	}
}

// Recorded from Claude Sonnet 5.5 on OpenRouter (2026-10-30): "none" is refused because the model
// always thinks. Leaving the field out thinks at the default (255 tokens on a classifier), so "none"
// is sent as "minimal" from then on, which it answers without thinking.
func TestMandatoryReasoningSendsMinimalForNone(t *testing.T) {
	var rec recorded
	a := testAdapter(t, APIOpenRouter, func(w http.ResponseWriter, r *http.Request) {
		body := rec.add(r)
		if eff, _ := body["reasoning"].(map[string]any); eff == nil || eff["effort"] == "none" {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":{"message":"Reasoning is mandatory for this endpoint and cannot be disabled.","code":400}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"{\"to\":\"bot\"}"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	})
	req := ChatRequest{Model: "anthropic/claude-sonnet-5.5", Messages: []Message{{Role: "user", Content: "x"}}, MaxTokens: 60, Effort: "none"}
	if out, err := a.Chat(context.Background(), req); err != nil || out.Content == "" || len(rec.bodies) != 2 {
		t.Fatalf("none refused → minimal: %q %v after %d", out.Content, err, len(rec.bodies))
	}
	if _, err := a.Chat(context.Background(), req); err != nil || len(rec.bodies) != 3 || rec.bodies[2]["reasoning"].(map[string]any)["effort"] != "minimal" {
		t.Fatalf("the next none goes out as minimal at once: %d %v", len(rec.bodies), err)
	}
	low := req
	low.Effort = "low"
	if _, err := a.Chat(context.Background(), low); err != nil || rec.bodies[3]["reasoning"].(map[string]any)["effort"] != "low" {
		t.Fatalf("other levels are unchanged: %v %v", rec.bodies[3], err)
	}
}

// A model that thinks before every answer whatever it is asked (Claude Opus 5.5: about 35 tokens before
// a 13-token answer; Anthropic's own API ignores the effort field) spends a 40-token limit thinking.
// The adapter asks once more with room past the thinking, charges both, and keeps the room.
func TestThinkingAtTheLowestEffortGetsHeadroom(t *testing.T) {
	var rec recorded
	a := testAdapter(t, APICompatible, func(w http.ResponseWriter, r *http.Request) {
		body := rec.add(r)
		if body["max_tokens"].(float64) < 100 {
			_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"length","message":{"role":"assistant","content":""}}],"usage":{"prompt_tokens":10,"completion_tokens":40}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"{\"to\":\"about_bot\"}"}}],"usage":{"prompt_tokens":10,"completion_tokens":48}}`))
	})
	req := ChatRequest{Model: "claude-opus-5-5", Messages: []Message{{Role: "user", Content: "x"}}, MaxTokens: 40, Effort: "none"}
	out, err := a.Chat(context.Background(), req)
	if err != nil || out.Content != `{"to":"about_bot"}` || len(rec.bodies) != 2 || rec.bodies[1]["max_tokens"].(float64) != 40+thinkingHeadroom {
		t.Fatalf("asked again with headroom: %q %v after %d", out.Content, err, len(rec.bodies))
	}
	if out.CompletionTokens != 88 {
		t.Fatalf("both calls are charged: %+v", out)
	}
	if _, err := a.Chat(context.Background(), req); err != nil || len(rec.bodies) != 3 || rec.bodies[2]["max_tokens"].(float64) != 40+thinkingHeadroom {
		t.Fatalf("the room is kept: %d %v", len(rec.bodies), err)
	}
}

func TestAnthropicRecommendsTheNewestModels(t *testing.T) {
	var models []Model
	for _, id := range []string{"claude-haiku-5-5", "claude-sonnet-5-5", "claude-opus-5-5", "claude-sonnet-5", "claude-sonnet-4-6", "claude-haiku-4-5-20251001"} {
		models = append(models, Model{ID: id, Kind: "chat"})
	}
	got := Recommend(APIAnthropic, "https://api.anthropic.com/v1", models)
	if got["response"] != "claude-sonnet-5-5" || got["planner"] != "claude-haiku-5-5" || got["summary"] != "claude-haiku-5-5" {
		t.Fatalf("recommend: %v", got)
	}
	if DetectAPI("https://api.anthropic.com/v1") != APIAnthropic || APIAnthropic.Rerank() {
		t.Fatal("api.anthropic.com is the anthropic family, which has no rerank")
	}
	if p, ok := Published(APIAnthropic, "claude-sonnet-5-5"); !ok || p.In*1e6 != 2 || p.Out*1e6 != 10 {
		t.Fatalf("published price: %+v %v", p, ok)
	}
}

func TestAnthropicListSendsVersionAndPageSize(t *testing.T) {
	a := testAdapter(t, APIAnthropic, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("anthropic-version") == "" || r.URL.Query().Get("limit") != "1000" {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"anthropic-version: header is required"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"type":"model","id":"claude-haiku-5-5","display_name":"Claude Haiku 5.5","created_at":"2026-09-01T00:00:00Z","max_input_tokens":1000000}],"has_more":false}`))
	})
	models, err := a.ListModels(context.Background())
	if err != nil || len(models) != 1 || models[0].Kind != "chat" || models[0].ContextLength != 1000000 || models[0].Name != "Claude Haiku 5.5" {
		t.Fatalf("list: %+v %v", models, err)
	}
	for url, want := range map[string]API{"https://api.anthropic.com/v1": APIAnthropic, "https://api.anthropic.com.evil.example/v1": APICompatible} {
		if got := DetectAPI(url); got != want {
			t.Fatalf("DetectAPI(%s) = %s, want %s", url, got, want)
		}
	}
}

// A refusal learned from a "none" request is kept for every field but the effort one: the temperature
// a model refuses it refuses at every level, while a model that refuses "none" may take "low".
func TestNoneRequestKeepsAllButTheEffortLesson(t *testing.T) {
	var rec recorded
	a := testAdapter(t, APIOpenAI, func(w http.ResponseWriter, r *http.Request) {
		body := rec.add(r)
		switch {
		case body["temperature"] != nil:
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":{"message":"Unsupported parameter: 'temperature'","code":"unsupported_parameter"}}`))
		case body["reasoning_effort"] == "none":
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":{"message":"Unsupported value: 'reasoning_effort' does not support 'none' with this model.","code":"unsupported_value"}}`))
		default:
			_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`))
		}
	})
	temp := 0.2
	req := ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}, MaxTokens: 40, Effort: "none", Temperature: &temp}
	if _, err := a.Chat(context.Background(), req); err != nil || len(rec.bodies) != 3 {
		t.Fatalf("none: %v after %d", err, len(rec.bodies))
	}
	low := req
	low.Effort = "low"
	if _, err := a.Chat(context.Background(), low); err != nil || len(rec.bodies) != 4 || rec.bodies[3]["temperature"] != nil || rec.bodies[3]["reasoning_effort"] != "low" {
		t.Fatalf("low: no temperature, its own effort, one call: %v %v", err, rec.bodies[len(rec.bodies)-1])
	}
}

// The headroom is kept only when it brought an answer; a key probe (one token) is not retried at all.
func TestHeadroomKeptOnlyWhenItHelps(t *testing.T) {
	var rec recorded
	a := testAdapter(t, APICompatible, func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"length","message":{"role":"assistant","content":""}}]}`))
	})
	req := ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}, MaxTokens: 40, Effort: "none"}
	if _, err := a.Chat(context.Background(), req); err != nil || len(rec.bodies) != 2 {
		t.Fatalf("one headroom try: %v after %d", err, len(rec.bodies))
	}
	if a.shapeOf(req).Headroom {
		t.Fatal("a headroom that did not help is not kept")
	}
	probe := req
	probe.MaxTokens, probe.Effort = 1, ""
	n := len(rec.bodies)
	if err := a.ProbeChat(context.Background(), "m"); err != nil || len(rec.bodies) != n+1 {
		t.Fatalf("a probe is one call: %v, %d", err, len(rec.bodies)-n)
	}
}
