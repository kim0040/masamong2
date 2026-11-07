package application_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/application"
	"masamong/internal/config"
	"masamong/internal/errcode"
	"masamong/internal/tools"
)

// slowLLM answers every chat completion after delay and records the most calls it saw at once.
type slowLLM struct {
	delay     time.Duration
	cur, peak atomic.Int32
	calls     atomic.Int32
}

func (f *slowLLM) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := f.cur.Add(1)
		defer f.cur.Add(-1)
		f.calls.Add(1)
		for p := f.peak.Load(); n > p && !f.peak.CompareAndSwap(p, n); p = f.peak.Load() {
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		time.Sleep(f.delay)
		content := "응 알겠어"
		if body["model"] == "fake/planner" {
			content = `{"memory_requirement":"none","tool_plan":[]}`
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]any{"content": content}}},
			"usage": map[string]any{"cost": 0.0001}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func slowChatSvc(t *testing.T, delay time.Duration, edit func(d *config.Document)) (*application.Service, *slowLLM) {
	svc := setupSvc(t)
	f := &slowLLM{delay: delay}
	srv := f.server(t)
	t.Setenv("FAKE_OR_KEY", "k")
	applyDoc(t, svc, func(d *config.Document) {
		d.Providers["openrouter"] = config.Provider{Kind: config.KindOpenAI, BaseURL: srv.URL, CredentialRef: "env:FAKE_OR_KEY"}
		d.Providers["openmeteo"] = config.Provider{Kind: config.KindOpenMeteo, Disabled: true}
		d.Models["planner"] = config.Model{Provider: "openrouter", Model: "fake/planner"}
		d.Models["response"] = config.Model{Provider: "openrouter", Model: "fake/response"}
		d.Memory.Retrieve = "off"
		d.Chat.ToolMode = "planner" // the planner pipeline; the tool loop has its own tests
		d.Tools = map[string]tools.Settings{"search.web": {Chat: boolp(false)}}
		if edit != nil {
			edit(d)
		}
	})
	return svc, f
}

// turnsAt sends one message per entry at the same moment and returns each turn's error.
func turnsAt(svc *application.Service, channels []string) []error {
	errs := make([]error, len(channels))
	start := make(chan struct{})
	var ready sync.WaitGroup
	ready.Add(len(channels))
	var wg sync.WaitGroup
	for i, ch := range channels {
		wg.Add(1)
		go func(i int, ch string) {
			defer wg.Done()
			ready.Done()
			<-start
			_, errs[i] = svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: ch,
				Message: fmt.Sprintf("message %d", i), Speaker: "u", SpeakerID: fmt.Sprintf("u%d", i)})
		}(i, ch)
	}
	ready.Wait()
	close(start)
	wg.Wait()
	return errs
}

func TestChatTurnsInDifferentChannelsRunInParallel(t *testing.T) {
	const delay = 150 * time.Millisecond
	svc, f := slowChatSvc(t, delay, nil)
	channels := []string{"a", "b", "c", "d", "e", "f"}
	for i, err := range turnsAt(svc, channels) {
		if err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
	}
	if f.peak.Load() < 2 {
		t.Fatalf("peak concurrent model calls = %d", f.peak.Load())
	}
}

func TestChatTurnsInOneChannelRunOneAtATime(t *testing.T) {
	svc, f := slowChatSvc(t, 40*time.Millisecond, nil)
	channels := []string{"room", "room", "room", "room", "room"}
	for i, err := range turnsAt(svc, channels) {
		if err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
	}
	if p := f.peak.Load(); p != 1 {
		t.Fatalf("turns in one channel overlapped: peak %d concurrent model calls", p)
	}
	// every exchange is in the channel history: none was built on a history missing an earlier turn
	res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "room", Message: "last", Speaker: "u", SpeakerID: "u9", DryRun: true})
	if err != nil || res.Answer == "" {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestMaxConcurrentTurnsBoundsModelCalls(t *testing.T) {
	svc, f := slowChatSvc(t, 60*time.Millisecond, func(d *config.Document) { d.Limits.MaxConcurrentTurns = 2 })
	channels := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	for i, err := range turnsAt(svc, channels) {
		if err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
	}
	if p := f.peak.Load(); p > 2 {
		t.Fatalf("limits.max_concurrent_turns=2 but %d model calls ran at once", p)
	}
}

func TestChannelBacklogRefusesInsteadOfQueueingForever(t *testing.T) {
	svc, _ := slowChatSvc(t, 150*time.Millisecond, nil)
	channels := make([]string, 9)
	for i := range channels {
		channels[i] = "spam"
	}
	busy, ok := 0, 0
	for _, err := range turnsAt(svc, channels) {
		switch {
		case errors.Is(err, application.ErrChatBusy):
			busy++
		case err == nil:
			ok++
		default:
			t.Fatal(err)
		}
	}
	// one running and four waiting are accepted; the rest are refused at once
	if ok != 5 || busy != 4 {
		t.Fatalf("answered %d, refused %d", ok, busy)
	}
}

func TestChatTurnsWhileConfigChanges(t *testing.T) {
	svc, _ := slowChatSvc(t, 20*time.Millisecond, nil)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: fmt.Sprintf("c%d", i%3),
				Message: fmt.Sprintf("hi %d", i), Speaker: "u", SpeakerID: "u"}); err != nil {
				t.Error(err)
			}
		}(i)
	}
	// operators editing rooms from the console and from Discord at the same time
	var edits sync.WaitGroup
	for i := 0; i < 6; i++ {
		edits.Add(1)
		go func(i int) {
			defer edits.Done()
			if r := exec(svc, "channels.configure", map[string]any{"guild_id": "g", "id": fmt.Sprintf("x%d", i), "persona_ref": "friendly_en"}); !r.OK {
				t.Errorf("configure %d: %+v", i, r.Error)
			}
		}(i)
	}
	edits.Wait()
	wg.Wait()
	// no edit was lost to another one based on the same revision
	for i := 0; i < 6; i++ {
		if _, ok := svc.Doc().Guilds["g"].Channels[fmt.Sprintf("x%d", i)]; !ok {
			t.Fatalf("channel x%d missing after concurrent edits: %+v", i, svc.Doc().Guilds["g"].Channels)
		}
	}
}

func TestConcurrentPollVotesAreNeverLost(t *testing.T) {
	svc := setupSvc(t)
	created := exec(svc, "polls.create", map[string]any{"question": "q", "options": []string{"a", "b"}, "scope": "discord:c"})
	if !created.OK {
		t.Fatalf("%+v", created.Error)
	}
	id := created.Output["id"].(string)
	const voters = 12
	var wg sync.WaitGroup
	var accepted, stale atomic.Int32
	for i := 0; i < voters; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rev := intVal(exec(svc, "polls.result", map[string]any{"id": id}).Output, "revision")
			r := exec(svc, "polls.vote", map[string]any{"id": id, "scope": "discord:c", "participant": fmt.Sprintf("p%d", i), "option_ids": []string{"a"}, "base_revision": rev})
			switch {
			case r.OK:
				accepted.Add(1)
			case r.Error.Code == errcode.StaleRevision:
				stale.Add(1)
			default:
				t.Errorf("vote %d: %+v", i, r.Error)
			}
		}(i)
	}
	wg.Wait()
	final := exec(svc, "polls.result", map[string]any{"id": id})
	if got := intVal(final.Output, "unique_voters"); got != int(accepted.Load()) {
		t.Fatalf("%d votes reported as counted but %d stored (%d stale)", accepted.Load(), got, stale.Load())
	}
	if got := intVal(final.Output, "revision"); got != 1+int(accepted.Load()) {
		t.Fatalf("revision %d after %d accepted votes", got, accepted.Load())
	}
}

// A config change that does not touch memory (a channel's rules) keeps the engine and its loaded
// index: with a long history a rebuild takes seconds and briefly doubles the index in memory. A memory
// setting, or a key replaced under the same reference, does get a new engine.
func TestMemoryEngineSurvivesUnrelatedConfigChanges(t *testing.T) {
	svc, _ := slowChatSvc(t, 0, nil)
	first, err := svc.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	if r := exec(svc, "channels.configure", map[string]any{"guild_id": "g", "id": "c", "rules": "짧게"}); !r.OK {
		t.Fatal(r.Error)
	}
	if again, _ := svc.MemoryEngine(); again != first {
		t.Fatal("a channel rule change rebuilt the memory engine")
	}
	t.Setenv("FAKE_OR_KEY", "rotated")
	if r := exec(svc, "channels.configure", map[string]any{"guild_id": "g", "id": "c", "rules": "길게"}); !r.OK {
		t.Fatal(r.Error)
	}
	rotated, _ := svc.MemoryEngine()
	if rotated == first {
		t.Fatal("a rotated model key must reach the memory engine")
	}
	applyDoc(t, svc, func(d *config.Document) { d.Memory.Summaries = !d.Memory.Summaries })
	if changed, _ := svc.MemoryEngine(); changed == rotated {
		t.Fatal("memory.summaries changed but the engine was kept")
	}
}

// queuedTurn starts a turn in the channel "room", waits until it holds the channel, then runs a second
// turn behind it with the given limits and returns the second turn's error and the first one's.
func queuedTurn(t *testing.T, delay time.Duration, behind application.ChatInput) (second, first error) {
	t.Helper()
	svc, _ := slowChatSvc(t, delay, nil)
	done := make(chan error, 1)
	go func() {
		_, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "room", Message: "first", Speaker: "u", SpeakerID: "u1"})
		done <- err
	}()
	time.Sleep(30 * time.Millisecond)
	behind.GuildID, behind.ChannelID, behind.Message, behind.Speaker, behind.SpeakerID = "g", "room", "second", "u", "u2"
	_, second = svc.ChatTurn(context.Background(), behind)
	return second, <-done
}

// The turn's time limit starts when the turn has its place in the channel: a turn that waited behind
// another one still gets its whole share (it used to be spent in the queue, and the model call that
// followed failed at once).
func TestTurnTimeoutStartsAfterTheWait(t *testing.T) {
	// a turn takes two model calls (about 160 ms); the second waits about as long as the first runs,
	// so waiting and running together overrun 280 ms but running alone does not
	second, first := queuedTurn(t, 80*time.Millisecond, application.ChatInput{TurnTimeout: 280 * time.Millisecond})
	if first != nil || second != nil {
		t.Fatalf("first: %v, second: %v", first, second)
	}
	// the limit still holds once the turn runs
	second, _ = queuedTurn(t, 80*time.Millisecond, application.ChatInput{TurnTimeout: 40 * time.Millisecond})
	if !errors.Is(second, context.DeadlineExceeded) {
		t.Fatalf("a turn past its own limit must fail with a timeout: %v", second)
	}
}

// A call that waited longer than MaxWait is stale: it is refused as busy, and the turn ahead of it is
// not disturbed.
func TestStaleQueuedTurnIsRefusedAsBusy(t *testing.T) {
	second, first := queuedTurn(t, 80*time.Millisecond, application.ChatInput{MaxWait: 40 * time.Millisecond})
	if !errors.Is(second, application.ErrChatBusy) || first != nil {
		t.Fatalf("second: %v, first: %v", second, first)
	}
}
