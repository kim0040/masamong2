package main

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"time"
)

// fakeLLM is an OpenAI-compatible server (chat, embeddings, rerank, models) with configurable latency
// and failure rate. It stands in for OpenRouter so a run costs nothing and never leaves the machine.
type fakeLLM struct {
	latency, jitter time.Duration
	errRate         float64
	dims            int

	inflight, peak atomic.Int64
	calls, errors  atomic.Int64
	conns          atomic.Int64    // TCP connections opened by clients (connection reuse)
	byPath         [4]atomic.Int64 // chat, embeddings, rerank, other
}

func (f *fakeLLM) start() *httptest.Server {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(f.serve))
	srv.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		if st == http.StateNew {
			f.conns.Add(1)
		}
	}
	srv.Start()
	return srv
}

func (f *fakeLLM) serve(w http.ResponseWriter, r *http.Request) {
	n := f.inflight.Add(1)
	defer f.inflight.Add(-1)
	for p := f.peak.Load(); n > p && !f.peak.CompareAndSwap(p, n); p = f.peak.Load() {
	}
	f.calls.Add(1)
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	path := r.URL.Path
	switch {
	case strings.HasSuffix(path, "/chat/completions"):
		f.byPath[0].Add(1)
	case strings.HasSuffix(path, "/embeddings"):
		f.byPath[1].Add(1)
	case strings.HasSuffix(path, "/rerank"):
		f.byPath[2].Add(1)
	default:
		f.byPath[3].Add(1)
	}
	if strings.HasSuffix(path, "/models") {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
		return
	}
	d := f.latency
	if f.jitter > 0 {
		d += time.Duration(rand.Int64N(int64(f.jitter)))
	}
	select {
	case <-time.After(d):
	case <-r.Context().Done():
		return
	}
	if f.errRate > 0 && rand.Float64() < f.errRate {
		f.errors.Add(1)
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":{"message":"injected failure"}}`))
		return
	}
	switch {
	case strings.HasSuffix(path, "/chat/completions"):
		f.chat(w, body)
	case strings.HasSuffix(path, "/embeddings"):
		f.embed(w, body)
	case strings.HasSuffix(path, "/rerank"):
		f.rerank(w, body)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeLLM) chat(w http.ResponseWriter, body map[string]any) {
	model, _ := body["model"].(string)
	content := "그때 캠핑 가자고 했잖아 ㅋㅋ 이번 주말에 다시 얘기해 보자"
	if raw, _ := json.Marshal(body["messages"]); strings.Contains(string(raw), `\"speak\"`) {
		// the participation judge: mostly silence, as intended
		content = `{"speak": false, "kind": "none", "score": 0.2, "reason": "chatter"}`
		if rand.IntN(3) == 0 {
			content = `{"speak": true, "kind": "answer", "score": 0.85, "reason": "unanswered question"}`
		}
		model = ""
	}
	switch model {
	case "fake/planner":
		// ask for memory on most turns so retrieval, embedding and the index are exercised
		req := "required"
		if rand.IntN(4) == 0 {
			req = "none"
		}
		content = `{"memory_requirement":"` + req + `","question_kind":"other","time_scope":"any","tool_plan":[]}`
	case "fake/summary":
		// distinct per leaf, as real summaries are (identical ones would be embedded once)
		raw, _ := json.Marshal(body["messages"])
		h := fnv.New32a()
		h.Write(raw)
		content = fmt.Sprintf("친구들이 주말 캠핑 장소와 준비물을 정했다 (%08x).", h.Sum32())
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": content}}},
		"usage":   map[string]any{"prompt_tokens": 800, "completion_tokens": 60, "cost": 0.0002},
	})
}

// embed returns a deterministic unit vector per input, so equal texts embed equally (cache hits are real).
func (f *fakeLLM) embed(w http.ResponseWriter, body map[string]any) {
	var inputs []string
	switch v := body["input"].(type) {
	case string:
		inputs = []string{v}
	case []any:
		for _, x := range v {
			s, _ := x.(string)
			inputs = append(inputs, s)
		}
	}
	data := make([]map[string]any, len(inputs))
	for i, in := range inputs {
		data[i] = map[string]any{"index": i, "embedding": vectorFor(in, f.dims)}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "usage": map[string]any{"prompt_tokens": 20 * len(inputs), "cost": 0.000002 * float64(len(inputs))}})
}

func (f *fakeLLM) rerank(w http.ResponseWriter, body map[string]any) {
	docs, _ := body["documents"].([]any)
	out := make([]map[string]any, len(docs))
	for i := range docs {
		out[i] = map[string]any{"index": i, "relevance_score": 1 / float64(i+1)}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"results": out, "usage": map[string]any{"cost": 0.00005}})
}

func vectorFor(text string, dims int) []float32 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(text))
	r := rand.New(rand.NewPCG(h.Sum64(), 0x9e3779b97f4a7c15))
	v := make([]float32, dims)
	var norm float64
	for i := range v {
		x := r.NormFloat64()
		v[i] = float32(x)
		norm += x * x
	}
	norm = math.Sqrt(norm)
	for i := range v {
		v[i] = float32(float64(v[i]) / norm)
	}
	return v
}
