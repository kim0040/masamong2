package openai_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"masamong/internal/chat"
	"masamong/internal/lang"
	"masamong/internal/providers"
	"masamong/internal/providers/openai"
)

// toTestServer rewrites every request onto the test server while the adapter keeps addressing the
// real host — the hosts.json flags the call runs under (OpenRouter's cache breakpoints) are the ones
// under test.
type toTestServer struct{ host string }

func (t toTestServer) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = "http"
	req.URL.Host = t.host
	return http.DefaultTransport.RoundTrip(req)
}

// The whole chain on the wire, two turns running: the answer prompt built the way the application
// builds it (chat.BuildAnswerMessages) and sent through an adapter that resolves the real OpenRouter
// host — only the transport is redirected to a test server. With the cache markers removed, every
// message of the shared prefix — the system and the history the first turn ended on — is the same
// bytes in the second turn's request; separately, exactly one breakpoint sits on the last message
// before each request's own ask.
func TestAnswerPrefixIsStableOnTheWire(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &b)
		bodies = append(bodies, b)
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","index":0,"message":{"content":"거기 진짜 맛있어","role":"assistant"}}],` +
			`"usage":{"completion_tokens":2,"prompt_tokens":10,"total_tokens":12}}`))
	}))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	c := providers.NewClient(5*time.Second, nil)
	c.MaxRetries = 0
	c.HTTP.Transport = toTestServer{host}
	a := openai.New(c, "https://openrouter.ai/api/v1", providers.Offline())

	pack, _ := lang.Get("ko")
	v := chat.Voice{Pack: pack}
	loc := time.FixedZone("KST", 9*3600)
	now := time.Date(2026, 10, 31, 15, 4, 0, 0, loc)
	history := []chat.Turn{
		{Role: "user", Speaker: "철수", Content: "점심 뭐 먹지", At: now.Add(-11 * time.Minute)},
		{Role: "assistant", Content: "나는 김치찌개 먹었어", At: now.Add(-10 * time.Minute)},
		{Role: "user", Speaker: "영희", Content: "나는 국수", At: now.Add(-4 * time.Minute)},
		{Role: "user", Speaker: "철수", Content: "국수 좋다", At: now.Add(-2 * time.Minute)},
	}
	send := func(in chat.AnswerInput, images []chat.Image) {
		ms := chat.BuildAnswerMessages(v, in)
		om := make([]openai.Message, len(ms))
		for i, m := range ms {
			om[i] = openai.Message{Role: m.Role, Content: m.Content, CachePoint: m.CachePoint}
			for _, img := range m.Images {
				om[i].Images = append(om[i].Images, openai.ImagePart{MIME: img.MIME, Data: img.Data})
			}
		}
		for _, img := range images { // the turn's pictures ride on its own message, past the prefix
			om[len(om)-1].Images = append(om[len(om)-1].Images, openai.ImagePart{MIME: img.MIME, Data: img.Data})
		}
		if _, err := a.Chat(context.Background(), openai.ChatRequest{Model: "google/gemini-3.8-flash", Messages: om, MaxTokens: 50}); err != nil {
			t.Fatalf("chat: %v", err)
		}
	}
	send(chat.AnswerInput{System: "persona", Speaker: "영희", Message: "그럼 국수로 가자", History: history, Now: now}, nil)
	// the stored conversation grows by the first turn's own exchange, and the next ask attaches an image
	grown := append(append([]chat.Turn(nil), history...),
		chat.Turn{Role: "user", Speaker: "영희", Content: "그럼 국수로 가자", At: now},
		chat.Turn{Role: "assistant", Content: "거기 진짜 맛있어", At: now.Add(time.Minute)})
	send(chat.AnswerInput{System: "persona", Speaker: "철수", Message: "거기 예약됐어", History: grown, Now: now.Add(3 * time.Minute)},
		[]chat.Image{{MIME: "image/png", Data: []byte("fixture")}})
	if len(bodies) != 2 {
		t.Fatalf("two requests: %d", len(bodies))
	}

	// the wire content of a message is its role and text, or its parts with the cache metadata off
	norm := func(b map[string]any) []string {
		var out []string
		for _, mm := range b["messages"].([]any) {
			m := mm.(map[string]any)
			var cs string
			switch c := m["content"].(type) {
			case string:
				cs = c
			case []any:
				// a marked text message goes as parts; with the marker off it is the same content
				// as the plain string it would have been (the captured body is not mutated — marker
				// placement is checked on it below)
				allText := true
				for _, p := range c {
					if p.(map[string]any)["type"] != "text" {
						allText = false
					}
				}
				var parts []string
				for _, p := range c {
					pm := p.(map[string]any)
					if allText {
						parts = append(parts, pm["text"].(string))
						continue
					}
					clean := make(map[string]any, len(pm))
					for k, v := range pm {
						if k != "cache_control" {
							clean[k] = v
						}
					}
					raw, _ := json.Marshal(clean)
					parts = append(parts, string(raw))
				}
				cs = strings.Join(parts, "")
				if !allText {
					cs = "[" + strings.Join(parts, ",") + "]"
				}
			}
			out = append(out, fmt.Sprint(m["role"])+"|"+cs)
		}
		return out
	}
	markedAt := func(b map[string]any) int {
		for i, mm := range b["messages"].([]any) {
			parts, _ := mm.(map[string]any)["content"].([]any)
			for _, p := range parts {
				if p.(map[string]any)["cache_control"] != nil {
					return i
				}
			}
		}
		return -1
	}
	first, second := norm(bodies[0]), norm(bodies[1])
	for i := 0; i < len(first)-1; i++ {
		if first[i] != second[i] {
			t.Fatalf("prefix message %d differs between turns:\n%s\n%s", i, first[i], second[i])
		}
	}
	// each request marks exactly the last message before its own ask
	if got := markedAt(bodies[0]); got != len(first)-2 {
		t.Fatalf("turn one's breakpoint at message %d, want %d (the last before the ask)", got, len(first)-2)
	}
	if got := markedAt(bodies[1]); got != len(second)-2 {
		t.Fatalf("turn two's breakpoint at message %d, want %d", got, len(second)-2)
	}
}
