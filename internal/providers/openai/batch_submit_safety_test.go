package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/providers"
)

// dropConnection answers nothing: the request arrived, the connection is closed before any answer (an
// accepted create whose answer is lost on the way back).
func dropConnection(t *testing.T) func(w http.ResponseWriter, _ fakeReq) {
	return func(w http.ResponseWriter, _ fakeReq) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("the test server cannot drop a connection")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.Close()
	}
}

// A batch create call is never retried, also on a client that retries everything else: a create the host
// took whose answer was lost would be a second paid batch. Such a call is an unknown outcome (not a
// refusal) that keeps the uploaded file, since the host may be reading it.
func TestBatchCreateIsNotRetriedAndALostAnswerIsUnknown(t *testing.T) {
	for _, tc := range []struct {
		name  string
		route func(t *testing.T) func(http.ResponseWriter, fakeReq)
	}{
		{"connection dropped", dropConnection},
		{"server error", func(*testing.T) func(http.ResponseWriter, fakeReq) {
			return func(w http.ResponseWriter, _ fakeReq) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(502)
				_, _ = w.Write([]byte(`{"error":{"message":"bad gateway"}}`))
			}
		}},
		{"answer without an id", func(*testing.T) func(http.ResponseWriter, fakeReq) {
			return func(w http.ResponseWriter, _ fakeReq) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"status":"validating"}`))
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			a := f.adapter(APIOpenAI, "/v1")
			a.Client.MaxRetries = 3 // every other call would be retried
			a.Client.RetryBase = time.Millisecond
			f.reply("POST", "/v1/files", 200, `{"id":"file-in"}`)
			var creates atomic.Int32
			route := tc.route(t)
			f.on("POST", "/v1/batches", func(w http.ResponseWriter, r fakeReq) { creates.Add(1); route(w, r) })
			f.reply("DELETE", "/v1/files/file-in", 200, `{}`)
			_, err := a.SubmitBatch(context.Background(), BatchChat, "gpt-x", []BatchItem{chatItem("a", "gpt-x")}, "gen-0")
			var su *SubmitUnknownError
			if !errors.Is(err, ErrBatchSubmitUnknown) || !errors.As(err, &su) {
				t.Fatalf("want an unknown outcome, got %v", err)
			}
			if creates.Load() != 1 {
				t.Fatalf("the create call was sent %d times", creates.Load())
			}
			if su.Ref.InputFile != "file-in" || su.Ref.SubmitKey != "gen-0" || su.Ref.BaseURL != a.BaseURL || su.Ref.ID != "" {
				t.Fatalf("the unknown submission keeps what is known of it: %+v", su.Ref)
			}
			if _, deleted := f.find("DELETE", "/v1/files/file-in"); deleted {
				t.Fatal("the input file of a batch that may be running must stay")
			}
			create := map[string]any{}
			_ = json.Unmarshal(f.must("POST", "/v1/batches").Body, &create)
			if md, _ := create["metadata"].(map[string]any); md[submitKeyField] != "gen-0" {
				t.Fatalf("OpenAI carries the submission key as metadata: %v", create)
			}
		})
	}
}

// A refusal (a 4xx, a rate limit) is definite: one call, not an unknown outcome, and the uploaded file is
// removed even when the caller's context ends while the removal is on its way.
func TestBatchRefusedCreateDeletesTheFileAfterTheCallerCancelled(t *testing.T) {
	f := newFake(t)
	a := f.adapter(APIOpenAI, "/v1")
	a.Client.MaxRetries = 3
	a.Client.RetryBase = time.Millisecond
	f.reply("POST", "/v1/files", 200, `{"id":"file-in"}`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var creates atomic.Int32
	f.on("POST", "/v1/batches", func(w http.ResponseWriter, _ fakeReq) {
		creates.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":{"message":"too many batches","type":"rate_limit"}}`))
	})
	var answered atomic.Bool
	f.on("DELETE", "/v1/files/file-in", func(w http.ResponseWriter, _ fakeReq) {
		cancel() // the job is cancelled while the file is being removed
		time.Sleep(50 * time.Millisecond)
		answered.Store(true)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	_, err := a.SubmitBatch(ctx, BatchEmbed, "emb", []BatchItem{{ID: "e", Input: []string{"x"}}}, "k")
	if err == nil || errors.Is(err, ErrBatchSubmitUnknown) || errors.Is(err, ErrBatchUnsupported) || creates.Load() != 1 {
		t.Fatalf("a 429 is a definite refusal sent once: %v (%d calls)", err, creates.Load())
	}
	if !answered.Load() {
		t.Fatal("the removal was cut off by the caller's cancel: the chat text stays at the host")
	}
}

// A lost submission is found again in the host's batch list: by the metadata key on OpenAI, by the
// input file on NanoGPT. A list that reaches past the submit time without it says it was never created.
func TestReconcileBatchFindsTheSubmission(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	t.Run("openai by key", func(t *testing.T) {
		f := newFake(t)
		a := f.adapter(APIOpenAI, "/v1")
		f.on("GET", "/v1/batches", func(w http.ResponseWriter, r fakeReq) {
			w.Header().Set("Content-Type", "application/json")
			if !strings.Contains(r.Query, "after=b2") {
				fmt.Fprintf(w, `{"data":[{"id":"b3","metadata":{"masamong_submit":"other"},"created_at":%d},{"id":"b2","created_at":%d}],"has_more":true,"last_id":"b2"}`, now.Unix(), now.Unix())
				return
			}
			fmt.Fprintf(w, `{"data":[{"id":"b1","input_file_id":"file-x","metadata":{"masamong_submit":"gen-1"},"created_at":%d}],"has_more":false}`, now.Unix())
		})
		ref := BatchRef{API: APIOpenAI, BaseURL: a.BaseURL, Kind: BatchChat, SubmitKey: "gen-1", SubmittedAt: now}
		got, err := a.ReconcileBatch(ctx, ref)
		if err != nil || got.ID != "b1" || got.InputFile != "file-x" || got.SubmitKey != "gen-1" {
			t.Fatalf("found %+v %v", got, err)
		}
		ref.SubmitKey = "gen-9"
		if _, err := a.ReconcileBatch(ctx, ref); !errors.Is(err, ErrBatchNotSubmitted) {
			t.Fatalf("a key nowhere in the list: %v", err)
		}
	})
	t.Run("nanogpt by input file", func(t *testing.T) {
		f := newFake(t)
		withHost(t, Host{API: APICompatible, ID: "nanotest", BaseURL: f.srv.URL + "/api/v1", Batch: &BatchHost{
			Style: "openai_files", Kinds: []string{"chat"}, Discount: 0.5, Reconcile: "input_file"}})
		a := f.adapter(APICompatible, "/api/v1")
		old := now.Add(-time.Hour).Unix()
		f.reply("GET", "/api/v1/batches", 200, fmt.Sprintf(`{"data":[{"id":"n2","input_file_id":"file-a","created_at":%d},{"id":"n1","input_file_id":"file-old","created_at":%d}],"has_more":true,"last_id":"n1"}`, now.Unix(), old))
		ref := BatchRef{API: APICompatible, BaseURL: a.BaseURL, Kind: BatchChat, InputFile: "file-a", SubmittedAt: now}
		if got, err := a.ReconcileBatch(ctx, ref); err != nil || got.ID != "n2" {
			t.Fatalf("found %+v %v", got, err)
		}
		ref.InputFile = "file-b"
		if _, err := a.ReconcileBatch(ctx, ref); !errors.Is(err, ErrBatchNotSubmitted) {
			t.Fatalf("the list went past the submit time: %v", err)
		}
		ref.InputFile = "" // the upload's answer was lost too: nothing to find it by
		if _, err := a.ReconcileBatch(ctx, ref); !errors.Is(err, ErrBatchSubmitUnknown) {
			t.Fatalf("no key and no file: unknown, %v", err)
		}
	})
	t.Run("a host without a list cannot tell", func(t *testing.T) {
		f := newFake(t)
		a := f.adapter(APIOpenRouter, "/api/v1")
		ref := BatchRef{API: APIOpenRouter, BaseURL: a.BaseURL, Kind: BatchChat, SubmitKey: "k", SubmittedAt: now}
		if _, err := a.ReconcileBatch(ctx, ref); !errors.Is(err, ErrBatchSubmitUnknown) || len(f.paths()) != 0 {
			t.Fatalf("OpenRouter batches carry no key: %v %v", err, f.paths())
		}
	})
	t.Run("a list that cannot be read", func(t *testing.T) {
		f := newFake(t)
		a := f.adapter(APIOpenAI, "/v1")
		f.reply("GET", "/v1/batches", 500, `{"error":{"message":"down"}}`)
		ref := BatchRef{API: APIOpenAI, BaseURL: a.BaseURL, Kind: BatchChat, SubmitKey: "k", SubmittedAt: now}
		if _, err := a.ReconcileBatch(ctx, ref); !errors.Is(err, ErrBatchSubmitUnknown) {
			t.Fatalf("unreadable list: %v", err)
		}
	})
}

// NanoGPT halves only OpenAI, Claude and Gemini rows; open-source rows (gpt-oss too) are billed at the
// live price. The engine estimate and the cost of unreported results use BatchDiscount.
func TestBatchDiscountPerModel(t *testing.T) {
	cred, _ := providers.Resolve("env:NOPE_UNSET")
	nano := New(providers.NewClient(time.Second, nil), "https://nano-gpt.com/api/v1", cred)
	nano.API = APICompatible
	for model, want := range map[string]float64{
		"gpt-5-nano": 0.5, "openai/gpt-5.1": 0.5, "claude-sonnet-5-5": 0.5, "anthropic/claude-haiku-5-5": 0.5,
		"gemini-3.8-flash": 0.5, "google/gemini-4-pro": 0.5, "GPT-5-mini": 0.5,
		"gpt-oss-120b": 1, "openai/gpt-oss-20b": 1, "deepseek/deepseek-v4-flash": 1, "qwen3-235b": 1, "zai-org/glm-5": 1,
	} {
		if got := nano.BatchDiscount(BatchChat, model); got != want {
			t.Errorf("nanogpt %s: %v, want %v", model, got, want)
		}
	}
	if got := nano.BatchDiscount(BatchEmbed, "gpt-x"); got != 1 {
		t.Errorf("nanogpt has no embeddings batch: %v", got)
	}
	oa := New(providers.NewClient(time.Second, nil), "https://api.openai.com/v1", cred)
	oa.API = APIOpenAI
	if got := oa.BatchDiscount(BatchEmbed, "text-embedding-3-small"); got != 0.5 {
		t.Errorf("openai: %v", got)
	}
	plain := New(providers.NewClient(time.Second, nil), "https://example.invalid/v1", cred)
	plain.API = APICompatible
	if got := plain.BatchDiscount(BatchChat, "gpt-x"); got != 1 {
		t.Errorf("a host without batches: %v", got)
	}
}

// OpenRouter answers 400 for a model without a batch variant (run it live) and for a bad request (a
// refusal of this batch, which must not turn into full-price live calls).
func TestSubmitInlineOnlyNoBatchVariantIsUnsupported(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		msg         string
		unsupported bool
	}{
		{"Model does not support batch", true},
		{"Model vendor/model has no batch variant", true},
		{"Batch is not supported for this model", true},
		{"Batch processing is not available for vendor/model", true},
		{"max_tokens must be a positive integer", false},
		{"Invalid request: requests[0].body.messages is required", false},
		{"Batch too large: at most 10000 requests", false},
	} {
		f := newFake(t)
		a := f.adapter(APIOpenRouter, "/api/v1")
		f.reply("POST", "/api/v1/batches", 400, fmt.Sprintf(`{"error":{"message":%q,"code":400}}`, tc.msg))
		_, err := a.SubmitBatch(ctx, BatchChat, "vendor/model", []BatchItem{chatItem("a", "vendor/model")}, "")
		if got := errors.Is(err, ErrBatchUnsupported); got != tc.unsupported || err == nil || errors.Is(err, ErrBatchSubmitUnknown) {
			t.Errorf("%q: unsupported=%v (want %v), err %v", tc.msg, got, tc.unsupported, err)
		}
	}
}

// A model's plain chat shape is learned by its first live success; a batch item's field refusal teaches
// it the same way a live retry does.
func TestShapeLearnedAndLearnRefusal(t *testing.T) {
	f := newFake(t)
	a := f.adapter(APIOpenAI, "/v1")
	var calls atomic.Int32
	f.on("POST", "/v1/chat/completions", func(w http.ResponseWriter, _ fakeReq) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`))
	})
	if a.ShapeLearned("gpt-x") {
		t.Fatal("a fresh adapter knows no shape")
	}
	temp := 0.2
	req := ChatRequest{Model: "gpt-x", MaxTokens: 16, Temperature: &temp, Messages: []Message{{Role: "user", Content: "hi"}}}
	if _, err := a.Chat(context.Background(), req); err != nil || !a.ShapeLearned("gpt-x") {
		t.Fatalf("learned after a live success: %v %v", a.ShapeLearned("gpt-x"), err)
	}
	refusal := &APIError{Status: 400, Message: "Unsupported parameter: 'temperature' is not supported with this model."}
	if !a.LearnRefusal(req, refusal) || a.shapeOf(req).NoTemperature != true {
		t.Fatalf("a refused temperature is learned: %+v", a.shapeOf(req))
	}
	if a.LearnRefusal(req, refusal) {
		t.Fatal("learning the same refusal again changes nothing")
	}
	body, err := a.batchChatBody(BatchItem{ID: "x", Chat: &req})
	if err != nil || strings.Contains(string(body), "temperature") {
		t.Fatalf("the next batch leaves the field out: %s %v", body, err)
	}
}

// An embeddings batch item the host reports no usage for (Gemini) is priced like a live call without
// usage: the inputs' estimated tokens at the catalog price, times the batch discount.
func TestBatchEmbedCostFromEstimatedTokens(t *testing.T) {
	cred, _ := providers.Resolve("env:NOPE_UNSET")
	a := New(providers.NewClient(time.Second, nil), "https://api.openai.com/v1", cred)
	a.API = APIOpenAI
	if c := a.BatchEmbedCost("unpriced-model", 100, time.Now()); c != nil {
		t.Fatalf("no price, no cost: %v", *c)
	}
	a.Price = func(model string) (in, out float64, ok bool) { return 0.02 / 1e6, 0, model == "emb" }
	c := a.BatchEmbedCost("emb", 1000, time.Now())
	if c == nil || *c < 0.01e-3-1e-15 || *c > 0.01e-3+1e-15 {
		t.Fatalf("1000 tokens at $0.02/M halved: %v", c)
	}
	if a.BatchEmbedCost("emb", 0, time.Now()) != nil {
		t.Fatal("no tokens, no estimate")
	}
}

// A batch total spread over outcomes that reported no tokens is split evenly instead of dropped.
func TestShareBatchCostWithoutTokens(t *testing.T) {
	out := []BatchOutcome{{ID: "a", Embed: &EmbedResult{}}, {ID: "b", Embed: &EmbedResult{}}}
	shareBatchCost(out, 0.004)
	for _, o := range out {
		if o.Embed.CostUSD == nil || *o.Embed.CostUSD != 0.002 || !o.Embed.CostEstimated {
			t.Fatalf("even share: %+v", o.Embed)
		}
	}
}
