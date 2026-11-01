package application_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/application"
	"masamong/internal/errclass"
)

// usageServer answers every chat completion with content and a usage block that reports cached tokens.
func usageServer(t *testing.T, content string) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}},
			"usage": map[string]any{"prompt_tokens": 120, "completion_tokens": 30, "cost": 0.0002,
				"prompt_tokens_details": map[string]any{"cached_tokens": 40}},
		})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// failingServer answers every chat completion with the given status.
func failingServer(t *testing.T, status int) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":{"message":"nope","type":"invalid_request_error"}}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// turnLedger reads a turn through the console Action. The ledger rows are written off the turn's path, so
// it waits until llm calls rows (and tool rows) have arrived.
func turnLedger(t *testing.T, svc *application.Service, id string, wantLLM, wantTools int) (turn, ledger map[string]any) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		res := exec(svc, "turns.get", map[string]any{"id": id})
		if !res.OK {
			t.Fatalf("turns.get: %+v", res.Error)
		}
		out := wire(t, res.Output)
		turn, _ = out["turn"].(map[string]any)
		ledger, _ = out["ledger"].(map[string]any)
		llm, _ := ledger["llm_calls"].([]any)
		tc, _ := ledger["tool_calls"].([]any)
		if (len(llm) >= wantLLM && len(tc) >= wantTools) || time.Now().After(deadline) {
			return turn, ledger
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// wire is an Action's output as the console receives it (JSON).
func wire(t *testing.T, res map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func rows(v any) []map[string]any {
	var out []map[string]any
	for _, it := range v.([]any) {
		out = append(out, it.(map[string]any))
	}
	return out
}

func TestChatTurnWritesCallLedgers(t *testing.T) {
	var n atomic.Int32
	planURL := chatServer(t, `{"memory_requirement":"none","tool_plan":[{"tool_name":"weather","parameters":{"region":"Atlantis","source":"global"}}]}`, "stop", &n)
	respURL := usageServer(t, "맑아요")
	svc := routedSvc(t, planURL, respURL)
	res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "m1", Message: "What is the weather like?", Speaker: "Chulsoo", SpeakerID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	_, ledger := turnLedger(t, svc, res.TurnID, 2, 1)
	llm := rows(ledger["llm_calls"])
	if len(llm) != 2 {
		t.Fatalf("llm calls: %+v", ledger["llm_calls"])
	}
	byRole := map[string]map[string]any{}
	for _, r := range llm {
		byRole[r["role"].(string)] = r
	}
	resp := byRole["response"]
	if resp == nil || resp["model"] != "shared/model" || resp["provider"] != "resp-host" || resp["call_count"] != float64(1) || resp["prompt_tokens"] != float64(120) ||
		resp["completion_tokens"] != float64(30) || resp["cached_tokens"] != float64(40) || resp["class"] != "" || resp["fallback"] != false || resp["finish_reason"] != "stop" {
		t.Fatalf("response call: %+v", resp)
	}
	if byRole["planner"] == nil || byRole["planner"]["prompt_tokens"] != float64(10) {
		t.Fatalf("planner call: %+v", byRole["planner"])
	}
	tc := rows(ledger["tool_calls"])
	if len(tc) != 1 || tc[0]["tool"] != "weather.query" || tc[0]["guard"] != "allowed" || tc[0]["status"] == "" || tc[0]["class"] == "" {
		t.Fatalf("tool calls: %+v", ledger["tool_calls"])
	}
	if keys, _ := tc[0]["param_keys"].([]any); len(keys) != 2 || keys[0] != "_other" || keys[1] != "region" { // "source" is not a declared input of weather.query
		t.Fatalf("param keys: %+v", tc[0])
	}
	if src, _ := tc[0]["param_sources"].(map[string]any); len(src) != 0 {
		t.Fatalf("param sources: %+v", tc[0])
	}
	raw, _ := json.Marshal(ledger)
	for _, secret := range []string{"Atlantis", "weather like", "Chulsoo", "u1"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("ledger holds %q: %s", secret, raw)
		}
	}
	// the tables carry the indexes the retention pass and the turn view rely on
	for _, idx := range []string{"idx_llm_calls_turn", "idx_llm_calls_created", "idx_tool_calls_turn", "idx_tool_calls_created"} {
		var name string
		if err := svc.Store.DB.QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND name=?`, idx).Scan(&name); err != nil {
			t.Fatalf("index %s: %v", idx, err)
		}
	}
}

// When the planner's model is gone, the response model plans; the ledger tells the stand-in from the rest.
func TestFallbackCallIsMarkedInTheLedger(t *testing.T) {
	planURL := failingServer(t, 404)
	respURL := usageServer(t, `{"memory_requirement":"none","tool_plan":[]}`)
	svc := routedSvc(t, planURL, respURL)
	res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", Message: "안녕", Speaker: "u", SpeakerID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	_, ledger := turnLedger(t, svc, res.TurnID, 3, 0)
	var planner []map[string]any
	for _, r := range rows(ledger["llm_calls"]) {
		if r["role"] == "planner" {
			planner = append(planner, r)
		}
	}
	if len(planner) != 2 {
		t.Fatalf("planner calls: %+v", ledger["llm_calls"])
	}
	if planner[0]["fallback"] != false || planner[0]["class"] != string(errclass.NotFound) || planner[0]["provider"] != "plan-host" {
		t.Fatalf("the failed call: %+v", planner[0])
	}
	if planner[1]["fallback"] != true || planner[1]["class"] != "" || planner[1]["provider"] != "resp-host" {
		t.Fatalf("the stand-in call: %+v", planner[1])
	}
}

// A channel's logging.level decides how much of its turns the console keeps (D9).
func TestChannelLoggingLevelDecidesWhatTheTurnHistoryKeeps(t *testing.T) {
	var n atomic.Int32
	planURL := chatServer(t, `{"memory_requirement":"none","tool_plan":[]}`, "stop", &n)
	respURL := usageServer(t, "대답이에요")
	svc := routedSvc(t, planURL, respURL)
	for ch, level := range map[string]string{"ops-room": "ops", "err-room": "errors", "off-room": "off"} {
		if r := exec(svc, "channels.configure", map[string]any{"guild_id": "g", "id": ch, "logging_level": level}); !r.OK {
			t.Fatalf("%s: %+v", ch, r.Error)
		}
	}
	ask := func(ch string) (string, string) {
		res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: ch, MessageID: "m-" + ch, Message: "안녕 " + ch, Speaker: "철수", SpeakerID: "u1"})
		if err != nil {
			t.Fatal(err)
		}
		return res.TurnID, res.Answer
	}
	body := func(id string) map[string]any {
		r := exec(svc, "turns.get", map[string]any{"id": id})
		if !r.OK {
			t.Fatalf("%+v", r.Error)
		}
		return wire(t, r.Output)["turn"].(map[string]any)["body"].(map[string]any)
	}
	id, _ := ask("ops-room")
	if b := body(id); b["message"] != "안녕 ops-room" || b["answer"] != "대답이에요" || b["speaker"] != "철수" || b["body_free"] != nil {
		t.Fatalf("ops keeps the turn whole: %+v", b)
	}
	for _, ch := range []string{"err-room", "off-room"} { // an answered turn is not an error: both drop the words
		id, _ := ask(ch)
		b := body(id)
		for _, k := range []string{"message", "answer", "answer_preview", "speaker"} {
			if v, ok := b[k]; ok {
				t.Fatalf("%s kept %s=%v", ch, k, v)
			}
		}
		if b["message_id"] != "m-"+ch || b["timing_ms"] == nil || b["body_free"] == nil {
			t.Fatalf("%s lost the body-free fields: %+v", ch, b)
		}
		list := exec(svc, "turns.list", map[string]any{"channel_id": ch})
		if !list.OK || len(wire(t, list.Output)["turns"].([]any)) != 1 {
			t.Fatalf("%s: the turn must still be listed: %+v", ch, list)
		}
	}
	// a failed turn keeps its words under "errors" but not under "off"
	bad := routedSvc(t, failingServer(t, 404), failingServer(t, 404))
	for ch, level := range map[string]string{"err-room": "errors", "off-room": "off"} {
		if r := exec(bad, "channels.configure", map[string]any{"guild_id": "g", "id": ch, "logging_level": level}); !r.OK {
			t.Fatalf("%+v", r.Error)
		}
		res, err := bad.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: ch, Message: "안녕 " + ch, Speaker: "철수", SpeakerID: "u1"})
		if err == nil {
			t.Fatalf("%s: expected a failed turn", ch)
		}
		r := exec(bad, "turns.get", map[string]any{"id": res.TurnID})
		b := wire(t, r.Output)["turn"].(map[string]any)["body"].(map[string]any)
		if level == "errors" && (b["message"] != "안녕 "+ch || !strings.Contains(asStr(b["error"]), "nope")) {
			t.Fatalf("errors keeps a failed turn whole: %+v", b)
		}
		if level == "off" {
			if _, ok := b["message"]; ok {
				t.Fatalf("off kept the message of a failed turn: %+v", b)
			}
			// the provider's error text can echo the question: only its class is kept
			if b["error"] != string(errclass.NotFound) {
				t.Fatalf("off kept the error text: %+v", b)
			}
		}
	}
}

// The turn detail Action returns the delivery rows with the call ledgers, and the context attribution
// stays in the turn body.
func TestTurnDetailReturnsDeliveryAndAttribution(t *testing.T) {
	var n atomic.Int32
	planURL := chatServer(t, `{"memory_requirement":"none","tool_plan":[]}`, "stop", &n)
	respURL := usageServer(t, "네")
	svc := routedSvc(t, planURL, respURL)
	res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", Message: "안녕", Speaker: "u", SpeakerID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	svc.NoteDelivery(res.TurnID, "g", "c", application.DeliveryReport{Parts: 2, Sent: 1, Retries: 3, Class: errclass.RateLimited})
	turn, ledger := turnLedger(t, svc, res.TurnID, 2, 0)
	del := rows(ledger["delivery"])
	if len(del) != 1 || del[0]["parts"] != float64(2) || del[0]["class"] != "rate_limited" {
		t.Fatalf("delivery: %+v", ledger["delivery"])
	}
	ctxBlock, _ := turn["body"].(map[string]any)["context"].(map[string]any)
	if _, ok := ctxBlock["attribution"]; !ok {
		t.Fatalf("the turn body lost context.attribution: %+v", ctxBlock)
	}
	empty := exec(svc, "turns.get", map[string]any{"id": "turn_missing"})
	if empty.OK {
		t.Fatal("an unknown turn must fail")
	}
}

func asStr(v any) string { s, _ := v.(string); return s }
