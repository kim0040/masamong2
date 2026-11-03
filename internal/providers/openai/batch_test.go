package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"masamong/internal/errclass"
	"masamong/internal/providers"
)

type fakeReq struct {
	Method, Path, Query string
	Header              http.Header
	Body                []byte
}

// fake is a host that records every request and answers from routes ("METHOD /path").
type fake struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	reqs   []fakeReq
	routes map[string]func(w http.ResponseWriter, r fakeReq)
}

func newFake(t *testing.T) *fake {
	f := &fake{t: t, routes: map[string]func(http.ResponseWriter, fakeReq){}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fr := fakeReq{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Header: r.Header.Clone(), Body: body}
		f.mu.Lock()
		f.reqs = append(f.reqs, fr)
		route := f.routes[r.Method+" "+r.URL.Path]
		f.mu.Unlock()
		if route == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":{"message":"no route"}}`))
			return
		}
		route(w, fr)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) on(method, path string, fn func(w http.ResponseWriter, r fakeReq)) {
	f.mu.Lock()
	f.routes[method+" "+path] = fn
	f.mu.Unlock()
}

func (f *fake) reply(method, path string, status int, body string) {
	f.on(method, path, func(w http.ResponseWriter, _ fakeReq) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
}

func (f *fake) find(method, path string) (fakeReq, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.reqs {
		if r.Method == method && r.Path == path {
			return r, true
		}
	}
	return fakeReq{}, false
}

func (f *fake) must(method, path string) fakeReq {
	f.t.Helper()
	r, ok := f.find(method, path)
	if !ok {
		f.t.Fatalf("no %s %s request; got %v", method, path, f.paths())
	}
	return r
}

func (f *fake) paths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var p []string
	for _, r := range f.reqs {
		p = append(p, r.Method+" "+r.Path)
	}
	return p
}

// adapter is an adapter of family api whose base URL is the fake plus basePath.
func (f *fake) adapter(api API, basePath string) *Adapter {
	f.t.Helper()
	c := providers.NewClient(5*time.Second, nil)
	c.MaxRetries = 0
	f.t.Setenv("K", "test-key")
	cred, _ := providers.Resolve("env:K")
	a := New(c, f.srv.URL+basePath, cred)
	a.API = api
	return a
}

// withHost lists an extra host for the test (a compatible host with a batch API).
func withHost(t *testing.T, h Host) {
	old := hostsData.Hosts
	hostsData.Hosts = append(append([]Host(nil), old...), h)
	t.Cleanup(func() { hostsData.Hosts = old })
}

func chatItem(id, model string) BatchItem {
	return BatchItem{ID: id, Chat: &ChatRequest{Model: model, MaxTokens: 16, Messages: []Message{{Role: "user", Content: "hi " + id}}}}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func decodeLines(t *testing.T, b []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range splitJSONL(b) {
		var m map[string]any
		if err := json.Unmarshal(l, &m); err != nil {
			t.Fatalf("bad jsonl line %q: %v", l, err)
		}
		out = append(out, m)
	}
	return out
}

func multipartParts(t *testing.T, r fakeReq) map[string][]byte {
	t.Helper()
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("content type %q: %v", r.Header.Get("Content-Type"), err)
	}
	mr := multipart.NewReader(bytes.NewReader(r.Body), params["boundary"])
	parts := map[string][]byte{}
	for i := 0; ; i++ {
		p, err := mr.NextPart()
		if err != nil {
			break
		}
		b, _ := io.ReadAll(p)
		key := p.FormName()
		if key == "" {
			key = p.Header.Get("Content-Type")
		}
		parts[key] = b
	}
	return parts
}

// ---------------------------------------------------------------------------
// Support and limits
// ---------------------------------------------------------------------------

func TestBatchSupportByHost(t *testing.T) {
	withHost(t, Host{API: APICompatible, ID: "nanotest", BaseURL: "https://nano.example/api/v1", Batch: &BatchHost{
		Style: "openai_files", BaseURL: "https://batch.nano.example/api/v1", Kinds: []string{"chat"}, MaxItems: 77, Discount: 0.5}})
	for _, tc := range []struct {
		url          string
		api          API
		chat, embed  bool
		max          int
		maxEmbedItem int
	}{
		{"https://api.openai.com/v1", APIOpenAI, true, true, 50000, 50000},
		{"https://openrouter.ai/api/v1", APIOpenRouter, true, true, 10000, 10000},
		{"https://api.anthropic.com/v1", APIAnthropic, true, false, 100000, 0},
		{"https://generativelanguage.googleapis.com/v1beta/openai", APIGemini, true, true, 10000, 10000},
		{"https://nano.example/api/v1", APICompatible, true, false, 77, 0},
		{"https://batch.nano.example/api/v1", APICompatible, true, false, 77, 0},
		{"https://my-proxy.example/v1", APIOpenAI, true, true, 50000, 50000}, // a custom address keeps the family's format
		{"https://api.cometapi.com/v1", APICompatible, false, false, 0, 0},
		{"https://apis.opengateway.ai/v1", APICompatible, false, false, 0, 0},
		{"https://api.deepseek.com/v1", APICompatible, false, false, 0, 0},
	} {
		a := New(providers.NewClient(time.Second, nil), tc.url, providers.Offline())
		a.API = tc.api
		if a.BatchSupport(BatchChat) != tc.chat || a.BatchSupport(BatchEmbed) != tc.embed {
			t.Errorf("%s: support chat=%v embed=%v want %v %v", tc.url, a.BatchSupport(BatchChat), a.BatchSupport(BatchEmbed), tc.chat, tc.embed)
		}
		if a.BatchMaxItems(BatchChat) != tc.max || a.BatchMaxItems(BatchEmbed) != tc.maxEmbedItem {
			t.Errorf("%s: max chat=%d embed=%d want %d %d", tc.url, a.BatchMaxItems(BatchChat), a.BatchMaxItems(BatchEmbed), tc.max, tc.maxEmbedItem)
		}
	}
}

func TestBatchHostsJSONFacts(t *testing.T) {
	nano, ok := HostByID("nanogpt")
	if !ok || nano.Batch == nil || nano.Batch.BaseURL != "https://api.nano-gpt.com/api/v1" || !nano.Batch.RequireMaxTokens {
		t.Fatalf("nanogpt batch entry: %+v", nano.Batch)
	}
	a := New(providers.NewClient(time.Second, nil), "https://nano-gpt.com/api/v1", providers.Offline())
	a.API = APICompatible
	s, ok := a.batchSpec()
	if !ok || s.base != "https://api.nano-gpt.com/api/v1" || s.has(BatchEmbed) {
		t.Fatalf("nanogpt spec: %+v %v", s, ok)
	}
	for _, id := range []string{"cometapi", "opengateway"} {
		if h, _ := HostByID(id); h.Batch != nil {
			t.Errorf("%s must have no batch entry", id)
		}
	}
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

func TestBatchValidation(t *testing.T) {
	f := newFake(t)
	a := f.adapter(APIOpenAI, "/v1")
	ctx := context.Background()
	img := chatItem("img", "m")
	img.Chat.Messages[0].Images = []ImagePart{{MIME: "image/png", Data: []byte("x")}}
	tool := chatItem("tool", "m")
	tool.Chat.Tools = []Tool{{Type: "function"}}
	other := chatItem("other", "x")
	for name, tc := range map[string]struct {
		kind  BatchKind
		model string
		items []BatchItem
		want  string
	}{
		"empty":        {BatchChat, "m", nil, "no items"},
		"no model":     {BatchChat, "", []BatchItem{chatItem("a", "m")}, "needs a model"},
		"bad id":       {BatchChat, "m", []BatchItem{chatItem("has space", "m")}, "must be 1-64"},
		"long id":      {BatchChat, "m", []BatchItem{chatItem(strings.Repeat("a", 65), "m")}, "must be 1-64"},
		"duplicate":    {BatchChat, "m", []BatchItem{chatItem("a", "m"), chatItem("a", "m")}, "used twice"},
		"other model":  {BatchChat, "m", []BatchItem{other}, "asks model"},
		"tools":        {BatchChat, "m", []BatchItem{tool}, "tools"},
		"images":       {BatchChat, "m", []BatchItem{img}, "pictures"},
		"no chat":      {BatchChat, "m", []BatchItem{{ID: "a"}}, "no chat request"},
		"embed empty":  {BatchEmbed, "m", []BatchItem{{ID: "a"}}, "no inputs"},
		"unknown kind": {BatchKind("x"), "m", []BatchItem{chatItem("a", "m")}, ""},
	} {
		_, err := a.SubmitBatch(ctx, tc.kind, tc.model, tc.items, "")
		if err == nil || (tc.want != "" && !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: err %v want %q", name, err, tc.want)
		}
		if errors.Is(err, ErrBatchUnsupported) && tc.want != "" {
			t.Errorf("%s: a bad item is not ErrBatchUnsupported", name)
		}
	}
	// too many inputs in one embeddings batch (the 50,000 limit counts inputs)
	big := make([]BatchItem, 3)
	for i := range big {
		big[i] = BatchItem{ID: string(rune('a' + i)), Input: make([]string, 20000)}
	}
	if _, err := a.SubmitBatch(ctx, BatchEmbed, "m", big, ""); err == nil || !strings.Contains(err.Error(), "inputs") {
		t.Errorf("embed input cap: %v", err)
	}
	// more items than the host takes
	tooMany := make([]BatchItem, 50001)
	for i := range tooMany {
		tooMany[i] = BatchItem{ID: "x", Input: []string{"a"}}
	}
	if _, err := a.SubmitBatch(ctx, BatchEmbed, "m", tooMany, ""); err == nil || !strings.Contains(err.Error(), "split") {
		t.Errorf("max items: %v", err)
	}
	if len(f.paths()) != 0 {
		t.Fatalf("nothing may be sent for an invalid batch: %v", f.paths())
	}
}

func TestBatchUnsupportedHostAndKind(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	comet := f.adapter(APICompatible, "/v1") // an unlisted compatible host
	if _, err := comet.SubmitBatch(ctx, BatchChat, "m", []BatchItem{chatItem("a", "m")}, ""); !errors.Is(err, ErrBatchUnsupported) {
		t.Fatalf("unlisted host: %v", err)
	}
	if _, err := comet.PollBatch(ctx, BatchRef{ID: "b", BaseURL: comet.BaseURL}); !errors.Is(err, ErrBatchUnsupported) {
		t.Fatalf("poll on unlisted host: %v", err)
	}
	anth := f.adapter(APIAnthropic, "/v1")
	if _, err := anth.SubmitBatch(ctx, BatchEmbed, "m", []BatchItem{{ID: "a", Input: []string{"x"}}}, ""); !errors.Is(err, ErrBatchUnsupported) {
		t.Fatalf("anthropic embed: %v", err)
	}
	if len(f.paths()) != 0 {
		t.Fatalf("unexpected traffic %v", f.paths())
	}
}

func TestBatchGateOncePerSubmitAndAuth(t *testing.T) {
	f := newFake(t)
	a := f.adapter(APIOpenAI, "/v1")
	calls := 0
	a.Gate = func() error { calls++; return nil }
	f.reply("POST", "/v1/files", 200, `{"id":"file-in"}`)
	f.reply("POST", "/v1/batches", 200, `{"id":"batch_1","status":"validating"}`)
	f.reply("DELETE", "/v1/files/file-in", 200, `{}`)
	items := []BatchItem{chatItem("a", "m"), chatItem("b", "m"), chatItem("c", "m")}
	if _, err := a.SubmitBatch(context.Background(), BatchChat, "m", items, ""); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("gate asked %d times, want once", calls)
	}
	// a poll does not go through the gate
	f.reply("GET", "/v1/batches/batch_1", 200, `{"id":"batch_1","status":"in_progress"}`)
	if _, err := a.PollBatch(context.Background(), BatchRef{ID: "batch_1", BaseURL: a.BaseURL}); err != nil || calls != 1 {
		t.Fatalf("poll: %v calls=%d", err, calls)
	}
	// a closed gate ends the submit before any request
	a.Gate = func() error { return errors.New("daily limit") }
	n := len(f.paths())
	if _, err := a.SubmitBatch(context.Background(), BatchChat, "m", items, ""); err == nil || len(f.paths()) != n {
		t.Fatalf("gate error must stop the submit: %v", err)
	}
	// a refused key stays ErrAuth, on submit and on poll
	f2 := newFake(t)
	b := f2.adapter(APIOpenAI, "/v1")
	f2.reply("POST", "/v1/files", 401, `{"error":{"message":"Incorrect API key provided","type":"invalid_request_error"}}`)
	if _, err := b.SubmitBatch(context.Background(), BatchChat, "m", items, ""); !errors.Is(err, ErrAuth) {
		t.Fatalf("submit auth: %v", err)
	}
	f2.reply("GET", "/v1/batches/x", 401, `{"error":{"message":"bad key"}}`)
	if _, err := b.PollBatch(context.Background(), BatchRef{ID: "x", BaseURL: b.BaseURL}); !errors.Is(err, ErrAuth) {
		t.Fatalf("poll auth: %v", err)
	}
}

func TestBatchRefOfAnotherHostIsRefused(t *testing.T) {
	f := newFake(t)
	a := f.adapter(APIOpenAI, "/v1")
	if _, err := a.PollBatch(context.Background(), BatchRef{ID: "b", BaseURL: "https://elsewhere.example/v1"}); err == nil {
		t.Fatal("a reference of another host must not be polled")
	}
	if _, err := a.PollBatch(context.Background(), BatchRef{BaseURL: a.BaseURL}); err == nil {
		t.Fatal("a reference without an id must be refused")
	}
	if len(f.paths()) != 0 {
		t.Fatal("no request expected")
	}
}

// ---------------------------------------------------------------------------
// Submit: the wire shape of every style
// ---------------------------------------------------------------------------

func TestSubmitOpenAIWireShape(t *testing.T) {
	f := newFake(t)
	a := f.adapter(APIOpenAI, "/v1")
	f.reply("POST", "/v1/files", 200, `{"id":"file-in"}`)
	f.reply("POST", "/v1/batches", 200, `{"id":"batch_1","status":"validating"}`)
	req := chatItem("a", "m")
	req.Chat.Effort = "low"
	temp := 0.2
	req.Chat.Temperature = &temp
	ref, err := a.SubmitBatch(context.Background(), BatchChat, "m", []BatchItem{req, chatItem("b-2_x", "m")}, "")
	if err != nil {
		t.Fatal(err)
	}
	if ref.ID != "batch_1" || ref.InputFile != "file-in" || ref.Count != 2 || ref.Kind != BatchChat || ref.Model != "m" ||
		ref.API != APIOpenAI || ref.BaseURL != a.BaseURL || ref.SubmittedAt.IsZero() {
		t.Fatalf("ref %+v", ref)
	}
	up := f.must("POST", "/v1/files")
	if got := up.Header.Get("Authorization"); got != "Bearer test-key" {
		t.Fatalf("auth header %q", got)
	}
	parts := multipartParts(t, up)
	if string(parts["purpose"]) != "batch" {
		t.Fatalf("purpose %q", parts["purpose"])
	}
	rows := decodeLines(t, parts["file"])
	if len(rows) != 2 || rows[0]["custom_id"] != "a" || rows[0]["method"] != "POST" || rows[0]["url"] != "/v1/chat/completions" {
		t.Fatalf("rows %v", rows)
	}
	body := rows[0]["body"].(map[string]any)
	// exactly the body a live call writes: OpenAI's token field, effort and temperature
	var live map[string]any
	_ = json.Unmarshal(mustJSON(t, a.payload(*req.Chat, shape{})), &live)
	want, _ := json.Marshal(live)
	got, _ := json.Marshal(body)
	if string(want) != string(got) {
		t.Fatalf("body differs from the live payload:\n got %s\nwant %s", got, want)
	}
	if body["max_completion_tokens"] != float64(16) || body["reasoning_effort"] != "low" || body["model"] != "m" {
		t.Fatalf("body %v", body)
	}
	create := map[string]any{}
	if err := json.Unmarshal(f.must("POST", "/v1/batches").Body, &create); err != nil {
		t.Fatal(err)
	}
	if create["input_file_id"] != "file-in" || create["endpoint"] != "/v1/chat/completions" || create["completion_window"] != "24h" {
		t.Fatalf("create %v", create)
	}
}

func TestSubmitUsesLearnedShape(t *testing.T) {
	f := newFake(t)
	a := f.adapter(APIOpenAI, "/v1")
	a.caps().setShape("m", shape{NoTemperature: true, NoEffort: true})
	f.reply("POST", "/v1/files", 200, `{"id":"file-in"}`)
	f.reply("POST", "/v1/batches", 200, `{"id":"b1","status":"validating"}`)
	it := chatItem("a", "m")
	it.Chat.Effort = "low"
	temp := 0.5
	it.Chat.Temperature = &temp
	if _, err := a.SubmitBatch(context.Background(), BatchChat, "m", []BatchItem{it}, ""); err != nil {
		t.Fatal(err)
	}
	body := decodeLines(t, multipartParts(t, f.must("POST", "/v1/files"))["file"])[0]["body"].(map[string]any)
	if _, ok := body["temperature"]; ok {
		t.Fatalf("a refused temperature must stay out: %v", body)
	}
	if _, ok := body["reasoning_effort"]; ok {
		t.Fatalf("a refused effort must stay out: %v", body)
	}
}

func TestSubmitOpenAIEmbeddingsAndOrphanCleanup(t *testing.T) {
	f := newFake(t)
	a := f.adapter(APIOpenAI, "/v1")
	f.reply("POST", "/v1/files", 200, `{"id":"file-in"}`)
	f.reply("POST", "/v1/batches", 400, `{"error":{"message":"invalid model","type":"invalid_request_error"}}`)
	f.reply("DELETE", "/v1/files/file-in", 200, `{}`)
	if _, err := a.SubmitBatch(context.Background(), BatchEmbed, "emb", []BatchItem{{ID: "e1", Input: []string{"a", "b"}}}, ""); err == nil {
		t.Fatal("expected the create failure")
	}
	row := decodeLines(t, multipartParts(t, f.must("POST", "/v1/files"))["file"])[0]
	if row["url"] != "/v1/embeddings" {
		t.Fatalf("row url %v", row["url"])
	}
	b := row["body"].(map[string]any)
	if b["model"] != "emb" || len(b["input"].([]any)) != 2 || len(b) != 2 {
		t.Fatalf("embeddings body %v", b)
	}
	f.must("DELETE", "/v1/files/file-in") // the uploaded file does not stay behind
	create := map[string]any{}
	_ = json.Unmarshal(f.must("POST", "/v1/batches").Body, &create)
	if create["endpoint"] != "/v1/embeddings" {
		t.Fatalf("create %v", create)
	}
}

func TestSubmitNanoGPTUsesItsBatchHost(t *testing.T) {
	f := newFake(t)
	withHost(t, Host{API: APICompatible, ID: "nanotest", BaseURL: f.srv.URL + "/api/v1", Batch: &BatchHost{
		Style: "openai_files", BaseURL: f.srv.URL + "/batchhost/api/v1", Kinds: []string{"chat"}, MaxItems: 10000, RequireMaxTokens: true, Discount: 0.5}})
	a := f.adapter(APICompatible, "/api/v1")
	f.reply("POST", "/batchhost/api/v1/files", 200, `{"id":"file-n"}`)
	f.reply("POST", "/batchhost/api/v1/batches", 200, `{"id":"batch_n","status":"validating"}`)
	ctx := context.Background()
	noLimit := chatItem("a", "m")
	noLimit.Chat.MaxTokens = 0
	if _, err := a.SubmitBatch(ctx, BatchChat, "m", []BatchItem{noLimit}, ""); err == nil || !strings.Contains(err.Error(), "output limit") {
		t.Fatalf("missing max_tokens must be refused: %v", err)
	}
	if _, err := a.SubmitBatch(ctx, BatchEmbed, "m", []BatchItem{{ID: "e", Input: []string{"x"}}}, ""); !errors.Is(err, ErrBatchUnsupported) {
		t.Fatalf("nanogpt has no embeddings: %v", err)
	}
	ref, err := a.SubmitBatch(ctx, BatchChat, "m", []BatchItem{chatItem("a", "m")}, "")
	if err != nil {
		t.Fatal(err)
	}
	if ref.BaseURL != a.BaseURL || ref.ID != "batch_n" {
		t.Fatalf("ref %+v", ref)
	}
	row := decodeLines(t, multipartParts(t, f.must("POST", "/batchhost/api/v1/files"))["file"])[0]
	if row["url"] != "/v1/chat/completions" || row["body"].(map[string]any)["max_tokens"] != float64(16) {
		t.Fatalf("row %v", row)
	}
	// later calls also go to the dedicated host
	f.reply("GET", "/batchhost/api/v1/batches/batch_n", 200, `{"id":"batch_n","status":"in_progress","request_counts":{"total":1}}`)
	if st, err := a.PollBatch(ctx, ref); err != nil || st.State != BatchRunning {
		t.Fatalf("poll %+v %v", st, err)
	}
}

func TestSubmitOpenRouterFieldOrderAndUnsupportedModel(t *testing.T) {
	f := newFake(t)
	a := f.adapter(APIOpenRouter, "/api/v1")
	f.reply("POST", "/api/v1/batches", 202, `{"id":"orb_1","status":"validating","request_counts":{"total":2,"completed":0,"failed":0},"results":null}`)
	ctx := context.Background()
	ref, err := a.SubmitBatch(ctx, BatchChat, "vendor/model", []BatchItem{chatItem("a", "vendor/model"), chatItem("b", "vendor/model")}, "")
	if err != nil {
		t.Fatal(err)
	}
	if ref.ID != "orb_1" || ref.InputFile != "" {
		t.Fatalf("ref %+v", ref)
	}
	raw := string(f.must("POST", "/api/v1/batches").Body)
	if !strings.HasPrefix(raw, `{"endpoint":"/v1/chat/completions","model":"vendor/model","requests":[{"custom_id":"a","body":{`) {
		t.Fatalf("field order: %.160s", raw)
	}
	var sent struct {
		Requests []struct {
			CustomID string         `json:"custom_id"`
			Body     map[string]any `json:"body"`
		} `json:"requests"`
	}
	if err := json.Unmarshal([]byte(raw), &sent); err != nil || len(sent.Requests) != 2 || sent.Requests[1].Body["max_tokens"] != float64(16) {
		t.Fatalf("requests %v %v", sent, err)
	}
	// embeddings use the same envelope
	if _, err := a.SubmitBatch(ctx, BatchEmbed, "emb/model", []BatchItem{{ID: "e", Input: []string{"x"}}}, ""); err != nil {
		t.Fatal(err)
	}
	reqs := f.reqs
	if last := string(reqs[len(reqs)-1].Body); !strings.HasPrefix(last, `{"endpoint":"/v1/embeddings","model":"emb/model","requests":[{"custom_id":"e","body":{"input":["x"],"model":"emb/model"}}]`) {
		t.Fatalf("embed envelope %s", last)
	}
	// a model without a batch variant answers 400: run it live
	f.reply("POST", "/api/v1/batches", 400, `{"error":{"message":"Model does not support batch","code":400}}`)
	if _, err := a.SubmitBatch(ctx, BatchChat, "vendor/model", []BatchItem{chatItem("a", "vendor/model")}, ""); !errors.Is(err, ErrBatchUnsupported) {
		t.Fatalf("400 must be ErrBatchUnsupported, got %v", err)
	}
}

func TestSubmitAnthropicNativeTranslation(t *testing.T) {
	f := newFake(t)
	a := f.adapter(APIAnthropic, "/v1")
	f.reply("POST", "/v1/messages/batches", 200, `{"id":"msgbatch_1","type":"message_batch","processing_status":"in_progress","results_url":null}`)
	temp := 0.7
	it := BatchItem{ID: "a", Chat: &ChatRequest{Model: "claude-x", MaxTokens: 50, Effort: "low", Temperature: &temp, Messages: []Message{
		{Role: "system", Content: "be brief"}, {Role: "system", Content: "in Korean"},
		{Role: "user", Content: "hello"}, {Role: "assistant", Content: "hi"}, {Role: "user", Content: "bye"}}}}
	high := BatchItem{ID: "b", Chat: &ChatRequest{Model: "claude-x", MaxTokens: 50, Effort: "high", Messages: []Message{{Role: "user", Content: "q"}}}}
	ref, err := a.SubmitBatch(context.Background(), BatchChat, "claude-x", []BatchItem{it, high}, "")
	if err != nil {
		t.Fatal(err)
	}
	if ref.ID != "msgbatch_1" || ref.Count != 2 {
		t.Fatalf("ref %+v", ref)
	}
	r := f.must("POST", "/v1/messages/batches")
	// a test address is not Anthropic's own host, so the key also goes as a bearer token
	// (TestAnthropicFormatOnAnyHost: x-api-key alone on api.anthropic.com)
	if r.Header.Get("x-api-key") != "test-key" || r.Header.Get("anthropic-version") != "2023-06-01" || r.Header.Get("Authorization") != "Bearer test-key" {
		t.Fatalf("headers %v", r.Header)
	}
	var sent struct {
		Requests []struct {
			CustomID string         `json:"custom_id"`
			Params   map[string]any `json:"params"`
		} `json:"requests"`
	}
	if err := json.Unmarshal(r.Body, &sent); err != nil || len(sent.Requests) != 2 {
		t.Fatalf("body %s %v", r.Body, err)
	}
	p := sent.Requests[0].Params
	msgs := p["messages"].([]any)
	if p["model"] != "claude-x" || p["system"] != "be brief\n\nin Korean" || len(msgs) != 3 ||
		msgs[0].(map[string]any)["role"] != "user" || msgs[1].(map[string]any)["role"] != "assistant" {
		t.Fatalf("params %v", p)
	}
	// the live Messages request: effort low thinks adaptively at low, with room for the thinking
	if th, _ := p["thinking"].(map[string]any); th["type"] != "adaptive" || p["output_config"].(map[string]any)["effort"] != "low" || p["max_tokens"] != float64(50+thinkingHeadroom) {
		t.Fatalf("effort low: adaptive thinking at low with headroom: %v", p)
	}
	if _, ok := p["temperature"]; ok {
		t.Fatalf("Anthropic gets no temperature live, so none here: %v", p)
	}
	hp := sent.Requests[1].Params
	if th, _ := hp["thinking"].(map[string]any); th["type"] != "adaptive" || hp["output_config"].(map[string]any)["effort"] != "high" || hp["max_tokens"] != float64(50+4096) {
		t.Fatalf("a high effort thinks adaptively at high: %v", hp)
	}
	if _, ok := hp["system"]; ok {
		t.Fatalf("no system, no field: %v", hp)
	}
	// translation errors
	ctx := context.Background()
	bad := chatItem("c", "claude-x")
	// the bot's line a conversation starts with is left out (as live); one with no user turn is refused
	bad.Chat.Messages = []Message{{Role: "system", Content: "s"}, {Role: "assistant", Content: "first"}}
	if _, err := a.SubmitBatch(ctx, BatchChat, "claude-x", []BatchItem{{ID: "c", Chat: &ChatRequest{Model: "claude-x", MaxTokens: 5, Messages: bad.Chat.Messages}}}, ""); err == nil || !strings.Contains(err.Error(), "user message") {
		t.Fatalf("first message must be the user's: %v", err)
	}
	noMax := chatItem("d", "claude-x")
	noMax.Chat.MaxTokens = 0
	if _, err := a.SubmitBatch(ctx, BatchChat, "claude-x", []BatchItem{noMax}, ""); err == nil || !strings.Contains(err.Error(), "output limit") {
		t.Fatalf("max_tokens is required natively: %v", err)
	}
}

func TestSubmitGeminiUsesTheNativeFilesAPI(t *testing.T) {
	f := newFake(t)
	a := f.adapter(APIGemini, "/v1beta/openai")
	// the docs' resumable upload: a start request answered with the upload address, then the bytes
	var start, upload fakeReq
	f.on("POST", "/upload/v1beta/files", func(w http.ResponseWriter, r fakeReq) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("X-Goog-Upload-Command") == "start" {
			start = r
			w.Header().Set("X-Goog-Upload-URL", f.srv.URL+"/upload/v1beta/files?upload_id=u1")
			_, _ = w.Write([]byte(`{}`))
			return
		}
		upload = r
		_, _ = w.Write([]byte(`{"file":{"name":"files/abc123","state":"ACTIVE"}}`))
	})
	f.reply("POST", "/v1beta/openai/batches", 200, `{"id":"batches/xyz","status":"validating"}`)
	it := chatItem("a", "gemini-x")
	it.Chat.Effort = "low"
	ref, err := a.SubmitBatch(context.Background(), BatchChat, "gemini-x", []BatchItem{it}, "")
	if err != nil {
		t.Fatal(err)
	}
	if ref.ID != "batches/xyz" || ref.InputFile != "files/abc123" {
		t.Fatalf("ref %+v", ref)
	}
	if start.Header.Get("X-Goog-Upload-Protocol") != "resumable" || start.Header.Get("x-goog-api-key") != "test-key" ||
		start.Header.Get("X-Goog-Upload-Header-Content-Type") != geminiBatchMIME || !strings.Contains(string(start.Body), `"display_name"`) ||
		start.Header.Get("X-Goog-Upload-Header-Content-Length") != fmt.Sprint(len(upload.Body)) {
		t.Fatalf("start request %v %s", start.Header, start.Body)
	}
	if upload.Query != "upload_id=u1" || upload.Header.Get("X-Goog-Upload-Command") != "upload, finalize" || upload.Header.Get("X-Goog-Upload-Offset") != "0" {
		t.Fatalf("upload request %q %v", upload.Query, upload.Header)
	}
	rows := decodeLines(t, upload.Body)
	if len(rows) != 1 || rows[0]["url"] != "/v1/chat/completions" || rows[0]["body"].(map[string]any)["reasoning_effort"] != "low" {
		t.Fatalf("rows %v", rows)
	}
	create := map[string]any{}
	_ = json.Unmarshal(f.must("POST", "/v1beta/openai/batches").Body, &create)
	if create["input_file_id"] != "files/abc123" || create["endpoint"] != "/v1/chat/completions" {
		t.Fatalf("create %v", create)
	}
	// the id Google names "batches/xyz" is addressed without doubling the prefix
	f.reply("GET", "/v1beta/openai/batches/xyz", 200, `{"id":"batches/xyz","status":"in_progress"}`)
	if st, err := a.PollBatch(context.Background(), ref); err != nil || st.State != BatchRunning {
		t.Fatalf("poll %+v %v", st, err)
	}
}

// ---------------------------------------------------------------------------
// Poll
// ---------------------------------------------------------------------------

func TestPollStatusMapping(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	for _, tc := range []struct {
		api  API
		path string
		base string
		get  string
		want map[string]BatchState
	}{
		{APIOpenAI, "/v1", "/v1/batches/b1", `{"id":"b1","status":"%s","request_counts":{"total":5,"completed":3,"failed":1},"expires_at":1893456000}`, map[string]BatchState{
			"validating": BatchPending, "in_progress": BatchRunning, "finalizing": BatchRunning, "cancelling": BatchRunning,
			"completed": BatchDone, "failed": BatchFailed, "expired": BatchExpired, "cancelled": BatchCancelled}},
		{APIOpenRouter, "/api/v1", "/api/v1/batches/b1", `{"id":"b1","status":"%s","request_counts":{"total":5,"completed":3,"failed":1},"results":[],"expires_at":"2030-01-01T00:00:00Z"}`, map[string]BatchState{
			"validating": BatchPending, "in_progress": BatchRunning, "finalizing": BatchRunning,
			"completed": BatchDone, "failed": BatchFailed, "expired": BatchExpired, "cancelled": BatchCancelled}},
	} {
		a := f.adapter(tc.api, tc.path)
		for status, want := range tc.want {
			f.reply("GET", tc.base, 200, strings.Replace(tc.get, "%s", status, 1))
			st, err := a.PollBatch(ctx, BatchRef{ID: "b1", BaseURL: a.BaseURL})
			if err != nil || st.State != want || st.Total != 5 || st.Completed != 3 || st.Failed != 1 || st.ExpiresAt.Year() != 2030 {
				t.Errorf("%s %s: %+v %v want %s", tc.api, status, st, err, want)
			}
			if want.Final() != st.State.Final() {
				t.Errorf("%s %s final mismatch", tc.api, status)
			}
		}
		f.reply("GET", tc.base, 200, `{"id":"b1","status":"mystery"}`)
		if _, err := a.PollBatch(ctx, BatchRef{ID: "b1", BaseURL: a.BaseURL}); err == nil {
			t.Errorf("%s: an unknown status must be an error, not a silent wait", tc.api)
		}
	}
	// a failed batch says why
	a := f.adapter(APIOpenAI, "/v1")
	f.reply("GET", "/v1/batches/b1", 200, `{"id":"b1","status":"failed","errors":{"object":"list","data":[{"code":"invalid_json_line","message":"line 3 is not JSON","line":3}]}}`)
	if st, _ := a.PollBatch(ctx, BatchRef{ID: "b1", BaseURL: a.BaseURL}); st.State != BatchFailed || !strings.Contains(st.Error, "line 3") {
		t.Errorf("failed status %+v", st)
	}
	// OpenRouter: completed without results is not ready yet
	or := f.adapter(APIOpenRouter, "/api/v1")
	f.reply("GET", "/api/v1/batches/b1", 200, `{"id":"b1","status":"completed","results":null,"request_counts":{"total":1,"completed":1}}`)
	if st, err := or.PollBatch(ctx, BatchRef{ID: "b1", BaseURL: or.BaseURL}); err != nil || st.State != BatchRunning {
		t.Errorf("completed with null results: %+v %v", st, err)
	}
	// Anthropic
	an := f.adapter(APIAnthropic, "/v1")
	for _, tc := range []struct {
		body string
		want BatchState
	}{
		{`{"id":"b1","processing_status":"in_progress","request_counts":{"processing":2,"succeeded":1,"errored":0,"canceled":0,"expired":0}}`, BatchRunning},
		{`{"id":"b1","processing_status":"canceling","request_counts":{"processing":2}}`, BatchRunning},
		{`{"id":"b1","processing_status":"ended","request_counts":{"succeeded":2,"errored":1}}`, BatchDone},
		{`{"id":"b1","processing_status":"ended","cancel_initiated_at":"2026-11-01T00:00:00Z","request_counts":{"succeeded":1,"canceled":2}}`, BatchCancelled},
		{`{"id":"b1","processing_status":"ended","request_counts":{"succeeded":1,"expired":2}}`, BatchExpired},
	} {
		f.reply("GET", "/v1/messages/batches/b1", 200, tc.body)
		st, err := an.PollBatch(ctx, BatchRef{ID: "b1", BaseURL: an.BaseURL})
		if err != nil || st.State != tc.want {
			t.Errorf("anthropic %s: %+v %v want %s", tc.body, st, err, tc.want)
		}
	}
	f.reply("GET", "/v1/messages/batches/b1", 200, `{"id":"b1","processing_status":"in_progress","expires_at":"2026-11-03T10:00:00Z","request_counts":{"processing":2,"succeeded":1,"errored":1,"canceled":0,"expired":0}}`)
	if st, _ := an.PollBatch(ctx, BatchRef{ID: "b1", BaseURL: an.BaseURL}); st.Total != 4 || st.Completed != 1 || st.Failed != 1 || st.ExpiresAt.Day() != 3 {
		t.Errorf("anthropic counts %+v", st)
	}
}

// ---------------------------------------------------------------------------
// Results
// ---------------------------------------------------------------------------

const chatBody = `{"id":"x","model":"m","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"%s"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`

func chatRow(id, text string) string {
	return `{"custom_id":"` + id + `","response":{"status_code":200,"body":` + strings.Replace(chatBody, "%s", text, 1) + `},"error":null}`
}

func outcomeByID(outs []BatchOutcome) map[string]BatchOutcome {
	m := map[string]BatchOutcome{}
	for _, o := range outs {
		m[o.ID] = o
	}
	return m
}

func TestResultsOpenAIRowsErrorsAndCost(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	a := f.adapter(APIOpenAI, "/v1")
	a.Price = func(model string) (float64, float64, bool) { return 1e-6, 2e-6, true }
	f.reply("GET", "/v1/batches/b1", 200, `{"id":"b1","status":"completed","request_counts":{"total":4,"completed":2,"failed":2},"output_file_id":"out1","error_file_id":"err1"}`)
	f.on("GET", "/v1/files/out1/content", func(w http.ResponseWriter, _ fakeReq) {
		// out of input order, one row reporting its own cost
		reported := `{"custom_id":"c","response":{"status_code":200,"body":{"model":"m","choices":[{"finish_reason":"length","message":{"content":"cut"}}],"usage":{"prompt_tokens":4,"completion_tokens":4,"cost":0.123}}},"error":null}`
		_, _ = w.Write([]byte(chatRow("b", "second") + "\n\n" + reported + "\n" + chatRow("a", "first") + "\n"))
	})
	f.on("GET", "/v1/files/err1/content", func(w http.ResponseWriter, _ fakeReq) {
		_, _ = w.Write([]byte(`{"custom_id":"d","response":{"status_code":400,"body":{"error":{"message":"bad request body","type":"invalid_request_error"}}},"error":null}` + "\n" +
			`{"custom_id":"e","response":null,"error":{"code":"batch_expired","message":"expired before it ran"}}` + "\n"))
	})
	ref := BatchRef{ID: "b1", BaseURL: a.BaseURL, Kind: BatchChat, Model: "m", SubmittedAt: time.Now()}
	outs, err := a.BatchResults(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(outs) != 5 || outs[0].ID != "b" {
		t.Fatalf("outcomes in the host's order: %+v", outs)
	}
	m := outcomeByID(outs)
	first := m["a"]
	if first.Chat == nil || first.Chat.Content != "first" || first.Chat.FinishReason != "stop" || first.Chat.PromptTokens != 10 {
		t.Fatalf("a: %+v", first)
	}
	// 10 in × 1e-6 + 5 out × 2e-6 = 2e-5, at half price
	if first.Chat.CostUSD == nil || math.Abs(*first.Chat.CostUSD-1e-5) > 1e-12 || !first.Chat.CostEstimated {
		t.Fatalf("estimated cost: %+v", first.Chat.CostUSD)
	}
	if c := m["c"].Chat; c == nil || c.FinishReason != "length" || c.CostUSD == nil || *c.CostUSD != 0.123 || c.CostEstimated {
		t.Fatalf("a reported cost stays: %+v", m["c"])
	}
	var ae *APIError
	if !errors.As(m["d"].Err, &ae) || ae.Status != 400 || m["d"].Chat != nil {
		t.Fatalf("d: %+v", m["d"])
	}
	if m["e"].Err == nil || !strings.Contains(m["e"].Err.Error(), "expired before") {
		t.Fatalf("e: %+v", m["e"])
	}
	// the error file of a refused key keeps its class
	f.on("GET", "/v1/files/err1/content", func(w http.ResponseWriter, _ fakeReq) {
		_, _ = w.Write([]byte(`{"custom_id":"d","response":{"status_code":401,"body":{"error":{"message":"Incorrect API key provided"}}},"error":null}` + "\n"))
	})
	outs, err = a.BatchResults(ctx, ref)
	if err != nil || !errors.Is(outcomeByID(outs)["d"].Err, ErrAuth) {
		t.Fatalf("auth row: %v %+v", err, outs)
	}
}

func TestResultsNotFinalFailedAndPartial(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	a := f.adapter(APIOpenAI, "/v1")
	ref := BatchRef{ID: "b1", BaseURL: a.BaseURL, Kind: BatchChat, Model: "m", SubmittedAt: time.Now()}
	f.reply("GET", "/v1/batches/b1", 200, `{"id":"b1","status":"in_progress"}`)
	if _, err := a.BatchResults(ctx, ref); err == nil || !strings.Contains(err.Error(), "not finished") {
		t.Fatalf("running: %v", err)
	}
	f.reply("GET", "/v1/batches/b1", 200, `{"id":"b1","status":"failed","errors":{"data":[{"message":"input file is empty"}]}}`)
	if _, err := a.BatchResults(ctx, ref); err == nil || !strings.Contains(err.Error(), "input file is empty") {
		t.Fatalf("failed: %v", err)
	}
	for _, status := range []string{"expired", "cancelled"} {
		f.reply("GET", "/v1/batches/b1", 200, `{"id":"b1","status":"`+status+`","request_counts":{"total":3,"completed":1,"failed":0},"output_file_id":"out1"}`)
		f.on("GET", "/v1/files/out1/content", func(w http.ResponseWriter, _ fakeReq) { _, _ = w.Write([]byte(chatRow("a", "partial") + "\n")) })
		outs, err := a.BatchResults(ctx, ref)
		if err != nil || len(outs) != 1 || outs[0].Chat == nil || outs[0].Chat.Content != "partial" {
			t.Fatalf("%s with partial rows: %v %+v", status, err, outs)
		}
	}
	// expired with nothing finished: no rows, no error
	f.reply("GET", "/v1/batches/b1", 200, `{"id":"b1","status":"expired","request_counts":{"total":3}}`)
	if outs, err := a.BatchResults(ctx, ref); err != nil || len(outs) != 0 {
		t.Fatalf("expired empty: %v %+v", err, outs)
	}
	// completed with no file at all is wrong
	f.reply("GET", "/v1/batches/b1", 200, `{"id":"b1","status":"completed","request_counts":{"total":3,"completed":3}}`)
	if _, err := a.BatchResults(ctx, ref); err == nil {
		t.Fatal("completed without an output file must be an error")
	}
	// a damaged row is an error, not a silent skip
	f.reply("GET", "/v1/batches/b1", 200, `{"id":"b1","status":"completed","request_counts":{"total":1},"output_file_id":"out1"}`)
	f.on("GET", "/v1/files/out1/content", func(w http.ResponseWriter, _ fakeReq) { _, _ = w.Write([]byte(`{"custom_id":"a","resp`)) })
	if _, err := a.BatchResults(ctx, ref); err == nil {
		t.Fatal("a truncated row must be an error")
	}
}

func TestResultsEmbeddingsKnowTheirOwnCount(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	a := f.adapter(APIGemini, "/v1beta/openai")
	a.Price = func(string) (float64, float64, bool) { return 2e-7, 0, true }
	f.reply("GET", "/v1beta/openai/batches/xyz", 200, `{"id":"batches/xyz","status":"completed","request_counts":{"total":2},"output_file_id":"files/out9"}`)
	f.on("GET", "/download/v1beta/files/out9:download", func(w http.ResponseWriter, r fakeReq) {
		emb := func(id, vecs string) string {
			return `{"custom_id":"` + id + `","response":{"status_code":200,"body":{"data":[` + vecs + `],"usage":{"prompt_tokens":100}}}}`
		}
		_, _ = w.Write([]byte(emb("e2", `{"index":0,"embedding":[0.5,0.25]}`) + "\n" +
			emb("e1", `{"index":0,"embedding":[1,2]},{"index":1,"embedding":[3,4]}`) + "\n"))
	})
	ref := BatchRef{ID: "batches/xyz", BaseURL: a.BaseURL, Kind: BatchEmbed, Model: "emb", SubmittedAt: time.Now()}
	outs, err := a.BatchResults(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	m := outcomeByID(outs)
	if e := m["e1"].Embed; e == nil || len(e.Vectors) != 2 || e.Vectors[1][1] != 4 || e.PromptTokens != 100 {
		t.Fatalf("e1 %+v", m["e1"])
	}
	if e := m["e2"].Embed; e == nil || len(e.Vectors) != 1 || e.CostUSD == nil || math.Abs(*e.CostUSD-1e-5) > 1e-12 || !e.CostEstimated {
		t.Fatalf("e2 %+v", m["e2"])
	}
	dl := f.must("GET", "/download/v1beta/files/out9:download")
	if dl.Query != "alt=media" || dl.Header.Get("x-goog-api-key") != "test-key" {
		t.Fatalf("download request %q %v", dl.Query, dl.Header)
	}
}

func TestResultsInlineWaitsForResultsAndSharesTheTotal(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	a := f.adapter(APIOpenRouter, "/api/v1")
	ref := BatchRef{ID: "b1", BaseURL: a.BaseURL, Kind: BatchChat, Model: "m", SubmittedAt: time.Now()}
	f.reply("GET", "/api/v1/batches/b1", 200, `{"id":"b1","status":"completed","results":null}`)
	if _, err := a.BatchResults(ctx, ref); err == nil || !strings.Contains(err.Error(), "not finished") {
		t.Fatalf("null results are not ready: %v", err)
	}
	rows := `[` + chatRow("b", "two") + `,` + chatRow("a", "one") + `,{"custom_id":"c","response":{"status_code":502,"body":{"error":{"message":"upstream down"}}},"error":null}]`
	f.reply("GET", "/api/v1/batches/b1", 200, `{"id":"b1","status":"completed","results":`+rows+`,"usage":{"cost":0.02}}`)
	outs, err := a.BatchResults(ctx, ref)
	if err != nil || len(outs) != 3 || outs[0].ID != "b" {
		t.Fatalf("%v %+v", err, outs)
	}
	m := outcomeByID(outs)
	for _, id := range []string{"a", "b"} {
		c := m[id].Chat
		if c == nil || c.CostUSD == nil || math.Abs(*c.CostUSD-0.01) > 1e-12 {
			t.Fatalf("%s share of the batch cost: %+v", id, m[id])
		}
	}
	if m["c"].Err == nil || errclass.Of(m["c"].Err) != errclass.Overloaded && errclass.Of(m["c"].Err) != errclass.Unknown {
		t.Fatalf("c: %+v", m["c"])
	}
	// expired with partial results
	f.reply("GET", "/api/v1/batches/b1", 200, `{"id":"b1","status":"expired","results":[`+chatRow("a", "one")+`]}`)
	if outs, err := a.BatchResults(ctx, ref); err != nil || len(outs) != 1 {
		t.Fatalf("expired partial: %v %+v", err, outs)
	}
	f.reply("GET", "/api/v1/batches/b1", 200, `{"id":"b1","status":"cancelled","results":null}`)
	if outs, err := a.BatchResults(ctx, ref); err != nil || len(outs) != 0 {
		t.Fatalf("cancelled empty: %v %+v", err, outs)
	}
}

func TestResultsAnthropicNative(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	a := f.adapter(APIAnthropic, "/v1")
	a.Price = func(string) (float64, float64, bool) { return 1e-6, 4e-6, true }
	f.reply("GET", "/v1/messages/batches/b1", 200, `{"id":"b1","processing_status":"ended","results_url":"`+f.srv.URL+`/v1/messages/batches/b1/results","request_counts":{"succeeded":2,"errored":1,"canceled":1,"expired":0}}`)
	f.on("GET", "/v1/messages/batches/b1/results", func(w http.ResponseWriter, _ fakeReq) {
		ok := func(id, stop string) string {
			return `{"custom_id":"` + id + `","result":{"type":"succeeded","message":{"model":"claude-x","content":[{"type":"thinking","thinking":"hm"},{"type":"text","text":"answer "},{"type":"text","text":"` + id + `"}],"stop_reason":"` + stop + `","usage":{"input_tokens":10,"output_tokens":5}}}}`
		}
		_, _ = w.Write([]byte(ok("b", "max_tokens") + "\n" + ok("a", "end_turn") + "\n" +
			`{"custom_id":"c","result":{"type":"errored","error":{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens too large"}}}}` + "\n" +
			`{"custom_id":"d","result":{"type":"canceled"}}` + "\n" +
			`{"custom_id":"e","result":{"type":"expired"}}` + "\n"))
	})
	ref := BatchRef{ID: "b1", BaseURL: a.BaseURL, Kind: BatchChat, Model: "claude-x", SubmittedAt: time.Now()}
	outs, err := a.BatchResults(ctx, ref)
	if err != nil || len(outs) != 5 || outs[0].ID != "b" {
		t.Fatalf("%v %+v", err, outs)
	}
	m := outcomeByID(outs)
	if c := m["a"].Chat; c == nil || c.Content != "answer a" || c.FinishReason != "stop" || c.PromptTokens != 10 || c.CompletionTokens != 5 || c.Model != "claude-x" {
		t.Fatalf("a %+v", m["a"])
	} else if c.CostUSD == nil || math.Abs(*c.CostUSD-1.5e-5) > 1e-12 || !c.CostEstimated { // (10×1e-6 + 5×4e-6) × 0.5
		t.Fatalf("a cost %v", c.CostUSD)
	}
	if m["b"].Chat == nil || m["b"].Chat.FinishReason != "length" {
		t.Fatalf("b %+v", m["b"])
	}
	var ae *APIError
	if !errors.As(m["c"].Err, &ae) || ae.Status != 400 || ae.Type != "invalid_request_error" || errclass.Of(ae) != errclass.BadRequest && errclass.Of(ae) != errclass.Unknown {
		t.Fatalf("c %+v", m["c"])
	}
	if errclass.Of(m["d"].Err) != errclass.Canceled || errclass.Of(m["e"].Err) != errclass.Timeout {
		t.Fatalf("d %v e %v", m["d"].Err, m["e"].Err)
	}
	r := f.must("GET", "/v1/messages/batches/b1/results")
	if r.Header.Get("x-api-key") != "test-key" || r.Header.Get("anthropic-version") == "" {
		t.Fatalf("results headers %v", r.Header)
	}
	// a results address on another host is never followed with the key
	evil := newFake(t)
	evil.reply("GET", "/steal", 200, "")
	f.reply("GET", "/v1/messages/batches/b1", 200, `{"id":"b1","processing_status":"ended","results_url":"`+evil.srv.URL+`/steal","request_counts":{"succeeded":0}}`)
	if _, err := a.BatchResults(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if len(evil.paths()) != 0 {
		t.Fatalf("the key went to another host: %v", evil.paths())
	}
	// running: not final
	f.reply("GET", "/v1/messages/batches/b1", 200, `{"id":"b1","processing_status":"in_progress","request_counts":{"processing":1}}`)
	if _, err := a.BatchResults(ctx, ref); err == nil {
		t.Fatal("running batch must refuse results")
	}
}

// ---------------------------------------------------------------------------
// Cancel and cleanup
// ---------------------------------------------------------------------------

func TestCancelAndCleanup(t *testing.T) {
	ctx := context.Background()
	// OpenAI: cancel, then every file goes (a 404 is "already gone")
	f := newFake(t)
	a := f.adapter(APIOpenAI, "/v1")
	ref := BatchRef{ID: "b1", BaseURL: a.BaseURL, Kind: BatchChat, Model: "m", InputFile: "in1"}
	f.reply("POST", "/v1/batches/b1/cancel", 200, `{"id":"b1","status":"cancelling"}`)
	if err := a.CancelBatch(ctx, ref); err != nil {
		t.Fatal(err)
	}
	f.reply("GET", "/v1/batches/b1", 200, `{"id":"b1","status":"completed","output_file_id":"out1","error_file_id":"err1"}`)
	f.reply("DELETE", "/v1/files/in1", 200, `{"deleted":true}`)
	f.reply("DELETE", "/v1/files/out1", 200, `{"deleted":true}`)
	// err1 has no route: 404
	if err := a.CleanupBatch(ctx, ref); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	for _, id := range []string{"in1", "out1", "err1"} {
		f.must("DELETE", "/v1/files/"+id)
	}
	f.reply("DELETE", "/v1/files/out1", 500, `{"error":{"message":"boom"}}`)
	if err := a.CleanupBatch(ctx, ref); err == nil {
		t.Fatal("a real failure is reported")
	}

	// OpenRouter: no cancel call; cleanup deletes the stored batch
	g := newFake(t)
	or := g.adapter(APIOpenRouter, "/api/v1")
	oref := BatchRef{ID: "b1", BaseURL: or.BaseURL, Kind: BatchChat, Model: "m"}
	if err := or.CancelBatch(ctx, oref); err != nil || len(g.paths()) != 0 {
		t.Fatalf("openrouter cancel is a no-op: %v %v", err, g.paths())
	}
	g.reply("DELETE", "/api/v1/batches/b1", 200, `{}`)
	if err := or.CleanupBatch(ctx, oref); err != nil {
		t.Fatal(err)
	}
	g.must("DELETE", "/api/v1/batches/b1")

	// Anthropic
	h := newFake(t)
	an := h.adapter(APIAnthropic, "/v1")
	aref := BatchRef{ID: "b1", BaseURL: an.BaseURL, Kind: BatchChat, Model: "m"}
	h.reply("POST", "/v1/messages/batches/b1/cancel", 200, `{"id":"b1","processing_status":"canceling"}`)
	h.reply("DELETE", "/v1/messages/batches/b1", 200, `{"id":"b1","type":"message_batch_deleted"}`)
	if err := an.CancelBatch(ctx, aref); err != nil {
		t.Fatal(err)
	}
	if err := an.CleanupBatch(ctx, aref); err != nil {
		t.Fatal(err)
	}
	if r := h.must("DELETE", "/v1/messages/batches/b1"); r.Header.Get("x-api-key") != "test-key" {
		t.Fatalf("headers %v", r.Header)
	}

	// Gemini: files through the native API
	k := newFake(t)
	ge := k.adapter(APIGemini, "/v1beta/openai")
	gref := BatchRef{ID: "batches/xyz", BaseURL: ge.BaseURL, Kind: BatchChat, Model: "m", InputFile: "files/in9"}
	k.reply("GET", "/v1beta/openai/batches/xyz", 200, `{"id":"batches/xyz","status":"completed","output_file_id":"files/out9"}`)
	k.reply("DELETE", "/v1beta/files/in9", 200, `{}`)
	k.reply("DELETE", "/v1beta/files/out9", 200, `{}`)
	if err := ge.CleanupBatch(ctx, gref); err != nil {
		t.Fatal(err)
	}
	k.must("DELETE", "/v1beta/files/out9")
}

func TestBatchWireNeverCarriesTheKeyInErrors(t *testing.T) {
	f := newFake(t)
	a := f.adapter(APIOpenAI, "/v1")
	f.reply("POST", "/v1/files", 500, `{"error":{"message":"server broke"}}`)
	_, err := a.SubmitBatch(context.Background(), BatchChat, "m", []BatchItem{chatItem("a", "m")}, "")
	if err == nil || strings.Contains(err.Error(), "test-key") {
		t.Fatalf("error %v", err)
	}
}

// OpenRouter answered 404 for a batch it had just accepted (live, 2026-11-02): a young batch the host
// cannot find yet is pending, an old one that is gone is an error.
func TestPollYoungBatchNotFoundIsPending(t *testing.T) {
	ctx := context.Background()
	f := newFake(t)
	a := f.adapter(APIOpenRouter, "/api/v1")
	f.reply("GET", "/api/v1/batches/b1", 404, `{"error":{"code":404,"message":"Batch job b1 not found."}}`)
	young := BatchRef{ID: "b1", BaseURL: a.BaseURL, Kind: BatchChat, Model: "m", Count: 2, SubmittedAt: time.Now().Add(-30 * time.Second)}
	st, err := a.PollBatch(ctx, young)
	if err != nil || st.State != BatchPending || st.Total != 2 {
		t.Fatalf("young: %+v %v", st, err)
	}
	old := young
	old.SubmittedAt = time.Now().Add(-batchVisibleWithin - time.Minute)
	if _, err := a.PollBatch(ctx, old); err == nil {
		t.Fatal("an old batch the host lost must be an error")
	}
}
