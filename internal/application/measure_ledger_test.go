package application_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"masamong/internal/application"
	"masamong/internal/chat"
	"masamong/internal/config"
)

// exaServer is a search service that reports what a search cost (Exa's costDollars).
func exaServer(t *testing.T, cost float64) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results":     []any{map[string]any{"title": "속보", "url": "https://news.example/1", "highlights": []string{"오늘 있었던 일"}}},
			"costDollars": map[string]any{"total": cost},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// measureSvc is loopSvc with web search on in chat, served by the fake Exa.
func measureSvc(t *testing.T, f *loopServer, searchURL string, mutate func(d *config.Document, hostURL string)) *application.Service {
	svc := setupSvc(t)
	host := f.start(t)
	t.Setenv("FAKE_LOOP_KEY", "k")
	applyDoc(t, svc, func(d *config.Document) {
		d.Providers["openrouter"] = config.Provider{Kind: config.KindOpenAI, BaseURL: host.URL, CredentialRef: "env:FAKE_LOOP_KEY"}
		d.Providers["openmeteo"] = config.Provider{Kind: config.KindOpenMeteo, Disabled: true}
		d.Models["planner"] = config.Model{Provider: "openrouter", Model: "fake/planner"}
		d.Models["response"] = config.Model{Provider: "openrouter", Model: "fake/response"}
		d.Memory.Retrieve = "off"
		d.Chat.ToolMode = "loop"
		if mutate != nil {
			mutate(d, host.URL)
		}
	})
	pointAt(t, svc, "exa", searchURL, "FAKE_EXA_KEY", "k")
	if r := exec(svc, "config.patch", map[string]any{"ops": []any{map[string]any{"path": []any{"web_search", "order"}, "value": []any{"exa"}}}}); !r.OK {
		t.Fatalf("order: %+v", r.Error)
	}
	if r := exec(svc, "channels.configure", map[string]any{"guild_id": "g", "id": "c", "persona_ref": "friendly_ko"}); !r.OK {
		t.Fatalf("%+v", r.Error)
	}
	return svc
}

func num(v any) float64 { f, _ := v.(float64); return f }

// The turn's cost is the sum of its ledger rows plus what the search service charged; the tool loop's
// rounds are rows of their own, the search row names the service that answered, and the turn record
// carries the queue wait, the first send, the time after sending and the spans.
func TestTurnCostEqualsLedgerRowsPlusSearch(t *testing.T) {
	const fee = 0.007
	f := &loopServer{plan: `{"memory_requirement":"none","tool_plan":[]}`, script: []map[string]any{
		toolCallMsg([2]string{"search__web", `{"query":"오늘 뉴스"}`}),
		{"role": "assistant", "content": "-"},
	}}
	svc := measureSvc(t, f, exaServer(t, fee).URL, nil)
	res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "m1", Message: "오늘 뉴스 뭐 있어?",
		Speaker: "영희", SpeakerID: "u1", DryRun: true,
		Deliver: func(ctx context.Context, r chat.Result) string {
			time.Sleep(40 * time.Millisecond) // the first bubble goes out 40 ms into delivery
			application.NoteFirstSend(ctx)
			time.Sleep(30 * time.Millisecond) // the rest of the bubbles
			return r.Answer
		}})
	if err != nil {
		t.Fatal(err)
	}
	if st := toolStates(res)["search.web"]; st != "ok" {
		t.Fatalf("search did not run: %+v", res.Tools)
	}
	turn, ledger := turnLedger(t, svc, res.TurnID, 4, 1)
	llm, tc := rows(ledger["llm_calls"]), rows(ledger["tool_calls"])
	roles := map[string]int{}
	var sum float64
	for _, r := range llm {
		roles[r["role"].(string)]++
		sum += num(r["cost_usd"])
	}
	// planner, two loop rounds (the search, then "nothing more"), answer — the loop's rounds used to be missing
	if roles["planner"] != 1 || roles["tool_loop"] != 2 || roles["response"] != 1 || len(llm) != 4 {
		t.Fatalf("model calls by role %v, want planner 1, tool_loop 2, response 1: %+v", roles, llm)
	}
	if len(tc) != 1 || tc[0]["tool"] != "search.web" || tc[0]["provider"] != "exa" || math.Abs(num(tc[0]["cost_usd"])-fee) > 1e-9 {
		t.Fatalf("search row: %+v", tc)
	}
	if want := sum + fee; math.Abs(num(turn["cost_usd"])-want) > 1e-9 || math.Abs(res.CostUSD-want) > 1e-9 {
		t.Fatalf("turn cost %v (result %v) != ledger rows %v + search %v", turn["cost_usd"], res.CostUSD, sum, fee)
	}

	body, _ := turn["body"].(map[string]any)
	timing, _ := body["timing_ms"].(map[string]any)
	for _, k := range []string{"queue_wait", "first_send", "post_send", "plan", "tool_loop", "answer"} {
		if _, ok := timing[k]; !ok {
			t.Errorf("timing_ms lacks %q: %v", k, timing)
		}
	}
	if first := num(timing["first_send"]); first < 40 || first > num(turn["latency_ms"]) {
		t.Errorf("first_send %v ms: the bubble went out 40 ms into delivery, and before the turn ended (%v ms)", first, turn["latency_ms"])
	}
	if num(timing["queue_wait"]) > 1000 || num(timing["post_send"]) < 0 {
		t.Errorf("timing_ms %v", timing)
	}
	spans, _ := body["spans"].([]any)
	names := map[string]map[string]any{}
	for _, s := range spans {
		m := s.(map[string]any)
		names[m["name"].(string)] = m
		if num(m["end_ms"]) < num(m["start_ms"]) {
			t.Errorf("span ends before it starts: %v", m)
		}
	}
	for _, n := range []string{"planner", "tool_loop", "answer"} {
		if names[n] == nil {
			t.Errorf("no %q span: %v", n, spans)
		}
	}
	if w := body["waited_on"]; w != "loop" && w != "planner" {
		t.Errorf("waited_on %v", w)
	}
	// the answer starts after both sides of the join are done
	if names["answer"] != nil && names["tool_loop"] != nil && names["planner"] != nil {
		if num(names["answer"]["start_ms"])+2 < math.Max(num(names["tool_loop"]["end_ms"]), num(names["planner"]["end_ms"])) {
			t.Errorf("answer starts before the join: %v", spans)
		}
	}
	// no conversation text or ids in the new fields
	raw, _ := json.Marshal([]any{body["spans"], body["waited_on"]})
	for _, banned := range []string{"오늘", "뉴스", "u1", "m1", "영희"} {
		if strings.Contains(string(raw), banned) {
			t.Errorf("spans carry %q: %s", banned, raw)
		}
	}
}

// A primary that is down and a fallback that answers the tool loop: the loop's row names the fallback and
// is marked as one, as the planner's and the answer's rows are, so the log's "model failed, its fallback
// answers" lines can be matched with the ledger.
func TestToolLoopFallbackIsInTheLedger(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"error":{"message":"overloaded"}}`))
	}))
	t.Cleanup(down.Close)
	f := &loopServer{plan: `{"memory_requirement":"none","tool_plan":[]}`, script: []map[string]any{
		toolCallMsg([2]string{"search__web", `{"query":"오늘 뉴스"}`}),
		{"role": "assistant", "content": "-"},
	}}
	svc := measureSvc(t, f, exaServer(t, 0.007).URL, func(d *config.Document, hostURL string) {
		d.Providers["down"] = config.Provider{Kind: config.KindOpenAI, BaseURL: down.URL, CredentialRef: "env:FAKE_LOOP_KEY"}
		d.Models["planner"] = config.Model{Provider: "down", Model: "fake/planner", Fallbacks: []config.ModelFallback{{Provider: "openrouter", Model: "fake/planner"}}}
	})
	res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "m1", Message: "오늘 뉴스 뭐 있어?",
		Speaker: "영희", SpeakerID: "u1", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	_, ledger := turnLedger(t, svc, res.TurnID, 4, 1)
	var loops int
	for _, r := range rows(ledger["llm_calls"]) {
		if r["role"] != "tool_loop" {
			continue
		}
		loops++
		if r["fallback"] != true || r["provider"] != "openrouter" || r["model"] != "fake/planner" {
			t.Errorf("a loop round the fallback answered: %+v", r)
		}
	}
	if loops != 2 {
		t.Fatalf("tool_loop rows %d: %+v", loops, ledger["llm_calls"])
	}
}

// A second turn in the channel waits for the first; the wait is in its record.
func TestQueueWaitIsRecorded(t *testing.T) {
	f := &loopServer{plan: `{"memory_requirement":"none","tool_plan":[]}`}
	svc := measureSvc(t, f, exaServer(t, 0).URL, nil)
	var wg sync.WaitGroup
	started := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "m1", Message: "첫째", Speaker: "영희", SpeakerID: "u1", DryRun: true,
			Deliver: func(_ context.Context, r chat.Result) string {
				close(started)
				time.Sleep(200 * time.Millisecond) // the channel stays taken while the first reply goes out
				return r.Answer
			}})
		if err != nil {
			t.Error(err)
		}
	}()
	<-started
	res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "m2", Message: "둘째", Speaker: "영희", SpeakerID: "u1", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if got := res.Timing["queue_wait"]; got < 100 {
		t.Fatalf("queue_wait %d ms, the second turn waited most of 200 ms: %v", got, res.Timing)
	}
	if _, ok := res.Timing["first_send"]; ok {
		t.Errorf("first_send without a delivery: %v", res.Timing)
	}
}

// The alert loop re-reads the dashboard and the doctor for itself: a slow one is info there, and the
// console's own read stays a warning. The alerts do not depend on the line.
func TestSlowBackgroundReadIsNotAWarning(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	svc := setupSvcLog(t, func(*application.Service) *slog.Logger {
		return slog.New(slog.NewTextHandler(syncWriter{&buf, &mu}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})
	ctx := context.Background()
	svc.SlowReadForTest(ctx, false)
	svc.SlowReadForTest(ctx, true)
	mu.Lock()
	logged := buf.String()
	mu.Unlock()
	if strings.Count(logged, "level=WARN") != 1 || !strings.Contains(logged, "level=INFO msg=\"probe: slow\"") {
		t.Fatalf("one warning (the read someone waits on) and one info line expected:\n%s", logged)
	}
}

type syncWriter struct {
	b  *bytes.Buffer
	mu *sync.Mutex
}

func (w syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

// A tag after a follow-up the bot skipped is noted ("retagged"); so is a tag after one it answered
// ("retagged_after_answer"), so the two rates can be compared. Only the first tag counts, and only within
// the window.
func TestRetagAfterAnswerIsRecordedBesideRetagAfterSkip(t *testing.T) {
	var plan, resp recordingServer
	planURL := plan.start(t, func(string) string { return `{"memory_requirement":"none","tool_plan":[]}` })
	respURL := resp.start(t, func(string) string { return "내일은 맑아. 우산 필요해?" })
	svc := routedSvc(t, planURL, respURL)
	applyDoc(t, svc, func(d *config.Document) {
		d.Guilds = map[string]config.GuildSettings{"g": {Channels: map[string]config.ChannelPolicy{"c": {Respond: "mention",
			Conversation: config.ChannelConversation{Follow: "window"}}}}}
	})
	turn := func(id string) {
		t.Helper()
		svc.Observe(application.ObservedMessage{GuildID: "g", ChannelID: "c", MessageID: id, AuthorID: "uc", AuthorName: "Chris",
			Content: "@마사몽 내일 날씨", At: time.Now(), Mentions: []string{"bot"}})
		if _, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: id,
			Message: "@마사몽 내일 날씨", Speaker: "Chris", SpeakerID: "uc",
			Deliver: func(_ context.Context, r chat.Result) string { return r.Answer }}); err != nil {
			t.Fatal(err)
		}
	}
	follow := func(id, text string) bool {
		m := application.ObservedMessage{GuildID: "g", ChannelID: "c", MessageID: id, AuthorID: "uc", AuthorName: "Chris", Content: text, At: time.Now()}
		svc.Observe(m)
		return svc.FollowUp(context.Background(), m)
	}
	outcomes := func(want map[string]int) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for {
			r := exec(svc, "conversation.log", map[string]any{"guild_id": "g", "channel_id": "c"})
			if !r.OK {
				t.Fatalf("%+v", r.Error)
			}
			raw, _ := json.Marshal(r.Output)
			var out struct {
				Outcomes map[string]map[string]int `json:"outcomes"`
			}
			_ = json.Unmarshal(raw, &out)
			got := out.Outcomes["follow"]
			if got["retagged"] == want["retagged"] && got["retagged_after_answer"] == want["retagged_after_answer"] {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("follow outcomes %v, want %v", got, want)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	turn("1")
	if !follow("2", "응 챙길게") {
		t.Fatal("window mode answers a clear follow-up")
	}
	svc.NoteDirectCall("g", "c", "uc") // tags the bot again after the answer
	outcomes(map[string]int{"retagged_after_answer": 1})
	svc.NoteDirectCall("g", "c", "uc") // a second tag is not the same event
	outcomes(map[string]int{"retagged_after_answer": 1})

	turn("3")
	if follow("4", "👍") {
		t.Fatal("a message without words is skipped")
	}
	svc.NoteDirectCall("g", "c", "uc")
	outcomes(map[string]int{"retagged": 1, "retagged_after_answer": 1})
}
