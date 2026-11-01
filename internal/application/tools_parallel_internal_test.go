package application

import (
	"context"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"masamong/internal/chat"
	"masamong/internal/config"
	"masamong/internal/lang"
	"masamong/internal/tools"
)

func TestChatToolLedgerIncludesPanicAndPartialCost(t *testing.T) {
	for _, outcome := range []string{"ok", "error", "panic"} {
		t.Run(outcome, func(t *testing.T) {
			s, _ := deliveryService(t)
			t.Setenv("MASAMONG_P2_LEDGER_TEST_KEY", "synthetic-test-credential")
			doc := s.Doc()
			doc.Features = maps.Clone(doc.Features)
			doc.Features["finance"] = true
			doc.Providers = map[string]config.Provider{"fh": {Kind: config.KindFinnhub, BaseURL: "https://ledger-gate.test", CredentialRef: "env:MASAMONG_P2_LEDGER_TEST_KEY"}}
			s.setDoc(doc, s.Revision()+1)
			c := s.Registry().Finnhub.Client
			c.InProcess, c.MaxRetries = true, 0
			c.HTTP.Transport = registryGateTransport(func(req *http.Request) (*http.Response, error) {
				chat.GuardFrom(req.Context()).Spend(0.01)
				time.Sleep(2 * time.Millisecond)
				if outcome == "panic" {
					panic("synthetic tool panic")
				}
				status, body := 200, `{"c":100,"d":1,"dp":1,"h":101,"l":99,"o":99,"pc":99}`
				if outcome == "error" {
					status = 400
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			g := &chat.Guard{}
			ctx := chat.WithGuard(context.Background(), g)
			voice := chat.Voice{Pack: lang.For("en")}
			model := &quotaLoopModel{calls: []chat.LoopCall{{ID: "a", Name: "finance__quote", Arguments: `{"symbol":"AAPL"}`}}}
			loop := chat.ToolLoop{LLM: model, Run: s.chatTool(ChatInput{}, voice), Voice: voice, Guard: g, Role: "planner"}
			set := chat.Toolset{Known: map[string]bool{"finance.quote": true}, Enabled: []chat.ToolInfo{{ID: "finance.quote", Params: []chat.ToolParam{{Name: "symbol"}}}}}
			results, _, loopErr := loop.Do(ctx, set, nil, "q", "")
			if loopErr != nil || len(results) != 1 {
				t.Fatal("tool loop did not return one result")
			}
			result := results[0] // runTool supplied a child Guard; ledger cost must be that child's cost
			if _, cost, _ := g.Totals(); math.Abs(cost-0.01) > 1e-12 {
				t.Errorf("parent guard cost=%g", cost)
			}
			if outcome == "ok" && !result.OK {
				t.Fatal("successful tool failed")
			}
			if outcome != "ok" && result.OK {
				t.Fatal("failed tool succeeded")
			}
			db, err := s.adminDB()
			if err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(3 * time.Second)
			var count int
			for {
				if err := db.DB.QueryRow(`SELECT COUNT(*) FROM tool_calls WHERE tool='finance.quote'`).Scan(&count); err != nil {
					t.Fatal(err)
				}
				if count > 0 || time.Now().After(deadline) {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if count != 1 {
				t.Fatalf("ledger has %d rows, want exactly one", count)
			}
			var cost float64
			var status, class string
			var latency int
			if err := db.DB.QueryRow(`SELECT status,class,cost_usd,latency_ms FROM tool_calls WHERE tool='finance.quote'`).Scan(&status, &class, &cost, &latency); err != nil {
				t.Fatal(err)
			}
			if math.Abs(cost-0.01) > 1e-12 || latency < 2 {
				t.Errorf("ledger cost=%g latency=%d", cost, latency)
			}
			if outcome == "panic" && (status != "unavailable" || class != "unknown") {
				t.Errorf("panic status=%s class=%s", status, class)
			}
			if outcome == "ok" && (status != "ok" || class != "") {
				t.Errorf("success status=%s class=%s", status, class)
			}
			if outcome == "error" && class == "" {
				t.Error("error class missing")
			}
		})
	}
}

type quotaLoopModel struct{ calls []chat.LoopCall }

func (*quotaLoopModel) ToolsReady(string) bool { return true }

func (f *quotaLoopModel) ChatTools(context.Context, string, []chat.LoopMsg, []chat.ToolSpec, int) (chat.LoopReply, error) {
	if f.calls == nil {
		return chat.LoopReply{Text: "done"}, nil
	}
	r := chat.LoopReply{Calls: f.calls}
	f.calls = nil
	return r, nil
}

func TestSameToolQuotaWinnerKeepsModelOrder(t *testing.T) {
	for _, limit := range []tools.Limits{{PerDay: 1}, {CooldownSec: 60}} {
		t.Run(fmt.Sprintf("daily=%d", limit.PerDay), func(t *testing.T) {
			s, _ := deliveryService(t)
			doc := s.Doc()
			doc.Tools = maps.Clone(doc.Tools)
			doc.Tools["help.version"] = tools.Settings{Limits: limit}
			s.setDoc(doc, s.Revision()+1)
			run := s.chatTool(ChatInput{SpeakerID: "synthetic-caller"}, chat.Voice{Pack: lang.For("en")})
			second := make(chan struct{})
			wrapped := func(ctx context.Context, name string, p map[string]any) chat.ToolResult {
				if p["ordinal"] == "first" {
					// Reorder goroutines deliberately on the old implementation. Serial execution lets
					// the first request reserve the real usage table before the second can run.
					select {
					case <-second:
					case <-time.After(100 * time.Millisecond):
					}
				} else {
					close(second)
				}
				return run(ctx, name, p)
			}
			model := &quotaLoopModel{calls: []chat.LoopCall{
				{ID: "a", Name: "help__version", Arguments: `{"ordinal":"first"}`},
				{ID: "b", Name: "help__version", Arguments: `{"ordinal":"second"}`},
			}}
			l := chat.ToolLoop{LLM: model, Run: wrapped, Role: "planner", Voice: chat.Voice{Pack: lang.For("en")}}
			set := chat.Toolset{Known: map[string]bool{"help.version": true}, Enabled: []chat.ToolInfo{{ID: "help.version", Params: []chat.ToolParam{{Name: "ordinal"}}}}}
			out, _, err := l.Do(context.Background(), set, nil, "q", "")
			if err != nil {
				t.Fatal(err)
			}
			if len(out) != 2 || !out[0].OK || out[1].OK {
				t.Error("the first model call did not win the real quota/cooldown reservation")
			}
		})
	}
}

func TestChatModelsReserveCallsAtStart(t *testing.T) {
	for _, mode := range []string{"planner", "loop", "response"} {
		t.Run(mode, func(t *testing.T) {
			s, _ := deliveryService(t)
			t.Setenv("MASAMONG_P2_MODEL_TEST_KEY", "synthetic-test-credential")
			doc := s.Doc()
			doc.Providers = map[string]config.Provider{"model": {Kind: config.KindOpenAI, BaseURL: "https://model-reservation.test", CredentialRef: "env:MASAMONG_P2_MODEL_TEST_KEY"}}
			s.setDoc(doc, s.Revision()+1)
			a := s.Registry().LLM["model"]
			a.Client.InProcess = true
			entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			a.Client.HTTP.Transport = registryGateTransport(func(*http.Request) (*http.Response, error) {
				close(entered)
				<-release
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))}, nil
			})
			model := chatLLM{s: s, routes: map[string]chatRoute{mode: {model: "m", provider: "model", a: a}}}
			g := &chat.Guard{}
			ctx := chat.WithGuard(context.Background(), g)
			go func() {
				if mode == "loop" {
					_, err := model.ChatTools(ctx, mode, []chat.LoopMsg{{Role: "user", Content: "q"}}, nil, 10)
					done <- err
				} else {
					_, _, err := model.Chat(ctx, mode, []chat.Msg{{Role: "user", Content: "q"}}, 10, 0)
					done <- err
				}
			}()
			select {
			case <-entered:
			case err := <-done:
				t.Fatalf("model did not start: %v", err)
			case <-time.After(3 * time.Second):
				t.Fatal("model did not start")
			}
			if calls, _, _ := g.Totals(); calls != 1 {
				t.Errorf("model call in flight not reserved: %d", calls)
			}
			close(release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if calls, _, _ := g.Totals(); calls != 1 {
				t.Errorf("completed model counted twice: %d", calls)
			}
		})
	}
}
