package application_test

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/application"
	"masamong/internal/config"
)

// warmProvider answers embeddings, the planner and the answer of whole chat turns. holdPlan makes the
// planner wait for an embedding of the message, which only an embedding started ahead of the plan can be.
type warmProvider struct {
	plan     atomic.Pointer[string]
	message  atomic.Pointer[string]
	asked    atomic.Int32 // embeddings of the message
	holdPlan atomic.Bool
}

func (f *warmProvider) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/embeddings":
			if msg := f.message.Load(); msg != nil && strings.Contains(string(body), *msg) {
				f.asked.Add(1)
			}
			var req struct {
				Input []string `json:"input"`
			}
			_ = json.Unmarshal(body, &req)
			data := make([]map[string]any, len(req.Input))
			for i := range req.Input {
				v := make([]float32, 8)
				v[i%len(v)] = 1
				data[i] = map[string]any{"index": i, "embedding": v}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "usage": map[string]any{"prompt_tokens": 8, "cost": 0.000001}})
		case "/chat/completions":
			answer := "응"
			if strings.Contains(string(body), "tool_plan") {
				answer = *f.plan.Load()
				for deadline := time.Now().Add(5 * time.Second); f.holdPlan.Load() && f.asked.Load() == 0 && time.Now().Before(deadline); {
					time.Sleep(5 * time.Millisecond)
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": answer}, "finish_reason": "stop"}},
				"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "cost": 0.00001}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func warmSvc(t *testing.T, plan string) (*application.Service, *warmProvider) {
	t.Helper()
	f := &warmProvider{}
	f.plan.Store(&plan)
	t.Setenv("FAKE_OR_KEY", "k")
	svc := embedSvc(t, t.TempDir(), f.server(t).URL)
	applyDoc(t, svc, func(d *config.Document) {
		d.Models["planner"] = config.Model{Provider: "openrouter", Model: "fake/planner"}
		d.Models["response"] = config.Model{Provider: "openrouter", Model: "fake/response"}
		d.Chat.ToolMode = "planner" // one planner call and no loop: the embedding is the point here
	})
	capture(t, svc, "c", 9)
	svc.MemoryPassForTest(context.Background())
	eng, err := svc.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	if eng.Index() == nil {
		t.Fatal("no index")
	}
	return svc, f
}

const (
	warmNone   = `{"memory_requirement":"none","tool_plan":[]}`
	warmSearch = `{"memory_requirement":"required","question_kind":"event","person_targets":[],"tool_plan":[]}`
)

func warmTurn(t *testing.T, svc *application.Service, f *warmProvider, msg string, dry bool) {
	t.Helper()
	f.message.Store(&msg)
	if _, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", Message: msg, Speaker: "영희", SpeakerID: "u1", DryRun: dry}); err != nil {
		t.Fatal(err)
	}
}

// A real turn embeds the message while the planner thinks, though this plan then searches nothing.
func TestChatTurnEmbedsTheMessageAheadOfThePlan(t *testing.T) {
	svc, f := warmSvc(t, warmNone)
	f.holdPlan.Store(true)
	warmTurn(t, svc, f, "미리 임베딩할 질문", false)
	if f.asked.Load() == 0 {
		t.Fatal("the message was not embedded ahead of the plan")
	}
}

// A dry run has no reply to hurry: only a plan that searches sends the message out, as before.
func TestDryRunTurnEmbedsOnlyWhenThePlanSearches(t *testing.T) {
	svc, f := warmSvc(t, warmNone)
	warmTurn(t, svc, f, "드라이런 질문", true)
	if n := f.asked.Load(); n != 0 {
		t.Fatalf("a dry run that searches nothing embedded the message %d times", n)
	}
	searches := warmSearch
	f.plan.Store(&searches)
	warmTurn(t, svc, f, "드라이런 질문", true)
	if n := f.asked.Load(); n != 1 {
		t.Fatalf("a dry run that searches embedded the message %d times", n)
	}
}

// A daily limit on the embedding provider is the operator's: calls nobody may need do not use it up.
func TestDailyLimitedProviderEmbedsOnlyWhenThePlanSearches(t *testing.T) {
	svc, f := warmSvc(t, warmNone)
	applyDoc(t, svc, func(d *config.Document) {
		p := d.Providers["openrouter"]
		p.DailyLimit = 1000
		d.Providers["openrouter"] = p
	})
	warmTurn(t, svc, f, "한도 있는 질문", false)
	if n := f.asked.Load(); n != 0 {
		t.Fatalf("embedded the message %d times", n)
	}
}

// A person who withdrew consent to being remembered: their message leaves only if a search needs it.
func TestWithdrawnConsentEmbedsOnlyWhenThePlanSearches(t *testing.T) {
	svc, f := warmSvc(t, warmNone)
	if r := as(svc, "privacy.consent", map[string]any{"scope": "memory", "decision": "withdraw"}, inChannel("u1")); !r.OK {
		t.Fatal(r.Error)
	}
	warmTurn(t, svc, f, "동의 철회한 사람의 질문", false)
	if n := f.asked.Load(); n != 0 {
		t.Fatalf("embedded the message %d times", n)
	}
}

// The embedding of the message started ahead of the plan is charged to the turn that asked for it: the
// turn's cost is its ledger rows (the embedding's among them) and the same amount reaches the limits'
// view of the turn's spending. A plan that searches too (the search joins the embedding) pays it once.
func TestTurnCostIncludesTheEmbeddingAheadOfThePlan(t *testing.T) {
	for _, tc := range []struct {
		name, plan string
	}{{"plan searches nothing", warmNone}, {"plan searches", warmSearch}} {
		t.Run(tc.name, func(t *testing.T) {
			svc, f := warmSvc(t, tc.plan)
			f.holdPlan.Store(true)
			msg := "미리 임베딩할 질문"
			f.message.Store(&msg)
			res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "m-" + tc.name, Message: msg,
				Speaker: "영희", SpeakerID: "u1"})
			if err != nil {
				t.Fatal(err)
			}
			turn, ledger := turnLedger(t, svc, res.TurnID, 3, 0)
			var sum, embed float64
			embeds := 0
			for _, r := range rows(ledger["llm_calls"]) {
				if r["role"] == "rerank" {
					continue // the fake provider has no rerank endpoint: its row is an estimate for a call that failed
				}
				sum += num(r["cost_usd"])
				if r["role"] == "embed" {
					embeds++
					embed += num(r["cost_usd"])
				}
			}
			if embeds != 1 || math.Abs(embed-0.000001) > 1e-12 {
				t.Fatalf("embedding rows: %d costing %v, want one costing the provider's 0.000001: %+v", embeds, embed, ledger["llm_calls"])
			}
			if math.Abs(num(turn["cost_usd"])-sum) > 1e-9 || math.Abs(res.CostUSD-sum) > 1e-9 {
				t.Fatalf("turn cost %v (result %v) != ledger rows %v", turn["cost_usd"], res.CostUSD, sum)
			}
		})
	}
}
