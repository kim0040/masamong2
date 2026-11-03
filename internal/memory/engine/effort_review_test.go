package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/providers"
	"masamong/internal/providers/openai"
)

// Regression tests for the effort-control review (2026-11-03, round 1), at the engine level.

// B: an MRL model listed late keeps the message width an install already stored for it (no new space,
// no paid embedding again); a new install and every MRL model listed from the start move to 512 as
// decided (MSG-512), and a leaf width set narrower than the stored rows cannot keep them.
func TestLateMRLModelKeepsStoredMessageWidth(t *testing.T) {
	for _, c := range []struct {
		name string
		late bool
		dims int
		rows []string // the embed_space of each stored message vector
		want int
	}{
		{"late model, rows at its full width", true, 0, []string{"m:3072:l2", "m:3072:l2", "m:1024:l2"}, 3072},
		{"late model, rows at the leaf width set", true, 1024, []string{"m:1024:l2"}, 1024},
		{"late model, a leaf width set below the rows", true, 2048, []string{"m:3072:l2"}, 512},
		{"late model, the default space in use", true, 0, []string{"m:3072:l2", "m:512:l2"}, 512},
		{"late model, a new install", true, 0, nil, 512},
		{"late model, only another model's rows", true, 0, []string{"other:3072:l2"}, 512},
		{"MRL model listed from the start", false, 0, []string{"m:1024:l2"}, 512},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, _ := messageEngine(t)
			e.Models = Models{Embed: "m", EmbedDims: c.dims, EmbedTruncatable: true, EmbedKeepsMessageWidth: c.late}
			for i, space := range c.rows {
				if _, err := e.Store.DB.DB.Exec(`INSERT INTO memory_message_vectors(observation_id, room, embed_space, text_hash, vec, updated_at) VALUES (?, 'r', ?, 'h', x'00', '2026-11-03T00:00:00Z')`,
					"o"+strconv.Itoa(i), space); err != nil {
					t.Fatal(err)
				}
			}
			if got := e.MessageVectorDims(); got != c.want {
				t.Fatalf("message width %d, want %d", got, c.want)
			}
		})
	}
}

// H: a rerank in parts (NanoGPT takes 1024 documents a call) whose second part fails: the first part
// was billed, and the engine charges it on its error path, in the call log and the spend ledger.
func TestRerankPartFailureIsCharged(t *testing.T) {
	e, _ := newEngine(t, false)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) > 1 {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":{"message":"upstream failed"}}`))
			return
		}
		var req struct {
			TopN int `json:"top_n"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		rows := []map[string]any{}
		for i := 0; i < req.TopN; i++ {
			rows = append(rows, map[string]any{"index": i, "relevance_score": 2.0 - float64(i)})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": rows, "usage": map[string]any{"total_tokens": 1024, "cost": 0.002}})
	}))
	t.Cleanup(srv.Close)
	nano, ok := openai.HostByID("nanogpt")
	if !ok {
		t.Fatal("nanogpt host")
	}
	// the adapter is NanoGPT's (its rerank limit and score scale); its requests reach the fake host
	target, _ := url.Parse(srv.URL)
	client := providers.NewClient(5*time.Second, nil)
	client.MaxRetries = 0
	client.HTTP.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.URL.Scheme, r.URL.Host, r.Host = target.Scheme, target.Host, target.Host
		return http.DefaultTransport.RoundTrip(r)
	})
	cred, _ := providers.Resolve("env:FAKE_KEY")
	e.RerankLLM = openai.New(client, nano.BaseURL, cred)
	var logged []CallInfo
	e.OnCall = func(_ context.Context, c CallInfo) { logged = append(logged, c) }
	docs := make([]string, 1100)
	for i := range docs {
		docs[i] = "doc " + strconv.Itoa(i)
	}
	if _, err := (reranker{e}).Rerank(context.Background(), "q", docs, 3); err == nil || calls.Load() != 2 {
		t.Fatalf("the second part fails: %v after %d calls", err, calls.Load())
	}
	if len(logged) != 1 || logged[0].CostUSD != 0.002 || logged[0].PromptTokens != 1024 {
		t.Fatalf("the call log keeps the billed part: %+v", logged)
	}
	spent, _, err := e.Store.SpendSince("2000-01-01")
	if err != nil || spent["rerank"] != 0.002 {
		t.Fatalf("spend %v %v", spent, err)
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
