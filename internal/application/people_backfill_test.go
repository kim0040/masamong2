package application

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/config"
	"masamong/internal/errcode"
	"masamong/internal/memory/corpus"
	"masamong/internal/memory/engine"
	"masamong/internal/memory/peoplebackfill"
	"masamong/internal/memory/store"
	"masamong/internal/providers/openai"
)

// A fake Anthropic host for the people backfill: the Messages API and Message Batches, with answers
// built from the person_extract prompt it receives. Synthetic data only.

type pbFakeHost struct {
	srv       *httptest.Server
	mu        sync.Mutex
	requests  int
	chats     []map[string]any // POST /v1/messages bodies
	creates   []map[string]any // POST /v1/messages/batches bodies
	batches   map[string][]string
	running   atomic.Bool // batches stay in progress while set
	fullUsage atomic.Bool // report the reserved maximum: every prompt byte + framing written to the cache, the wire's max_tokens out
	refuse    map[string]bool
	drop      int // the next chat requests are read and their connection closed without an answer
	fail500   int // the next chat requests are answered 500
	// rewrite replaces the answer to a prompt (Messages and batch items): its text and stop reason
	rewrite func(model, prompt string) (text, stop string, ok bool)
}

func newPBHost(t *testing.T) *pbFakeHost {
	t.Helper()
	h := &pbFakeHost{batches: map[string][]string{}, refuse: map[string]bool{}}
	h.srv = httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *pbFakeHost) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.requests
}

func (h *pbFakeHost) creations() []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.creates)
}

func (h *pbFakeHost) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.requests++
	path := r.URL.Path
	switch {
	case r.Method == http.MethodPost && path == "/v1/messages":
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		h.chats = append(h.chats, req)
		if h.drop > 0 {
			h.drop--
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				_ = conn.Close() // accepted and maybe billed, but no answer arrives
			}
			return
		}
		if h.fail500 > 0 {
			h.fail500--
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"internal"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(h.message(req, false))
	case r.Method == http.MethodPost && path == "/v1/messages/batches":
		var req struct {
			Requests []struct {
				CustomID string         `json:"custom_id"`
				Params   map[string]any `json:"params"`
			} `json:"requests"`
		}
		_ = json.Unmarshal(body, &req)
		var raw map[string]any
		_ = json.Unmarshal(body, &raw)
		h.creates = append(h.creates, raw)
		id := fmt.Sprintf("msgbatch_%d", len(h.creates))
		var rows []string
		for _, it := range req.Requests {
			row, _ := json.Marshal(map[string]any{"custom_id": it.CustomID,
				"result": map[string]any{"type": "succeeded", "message": h.message(it.Params, h.refuse[it.CustomID])}})
			rows = append(rows, string(row))
		}
		// the host's order is not the items'
		rnd := rand.New(rand.NewPCG(7, uint64(len(h.creates))))
		rnd.Shuffle(len(rows), func(i, j int) { rows[i], rows[j] = rows[j], rows[i] })
		h.batches[id] = rows
		_ = json.NewEncoder(w).Encode(h.batchJSON(id))
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/messages/batches/"):
		id, results := strings.CutSuffix(strings.TrimPrefix(path, "/v1/messages/batches/"), "/results")
		rows, ok := h.batches[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"not_found_error","message":"no such batch"}}`))
			return
		}
		if results {
			_, _ = w.Write([]byte(strings.Join(rows, "\n") + "\n"))
			return
		}
		_ = json.NewEncoder(w).Encode(h.batchJSON(id))
	case r.Method == http.MethodDelete:
		_, _ = w.Write([]byte(`{}`))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (h *pbFakeHost) batchJSON(id string) map[string]any {
	n := len(h.batches[id])
	if h.running.Load() {
		return map[string]any{"id": id, "type": "message_batch", "processing_status": "in_progress", "request_counts": map[string]int{"processing": n}}
	}
	return map[string]any{"id": id, "type": "message_batch", "processing_status": "ended", "request_counts": map[string]int{"succeeded": n},
		"results_url": h.srv.URL + "/v1/messages/batches/" + id + "/results"}
}

func (h *pbFakeHost) message(req map[string]any, refuse bool) map[string]any {
	model, _ := req["model"].(string)
	var all []string
	pbStrings(req["system"], "system", &all)
	pbStrings(req["messages"], "messages", &all)
	bytes := 0
	for _, s := range all {
		bytes += len(s)
	}
	in, write, out := 400, 0, 60
	if h.fullUsage.Load() {
		maxOut, _ := req["max_tokens"].(float64)
		in, write, out = 0, bytes+pbFraming, int(maxOut)
	}
	text, stop := pbFakeAnswer(model, strings.Join(all, "\n")), "end_turn"
	if h.rewrite != nil {
		if t, st, ok := h.rewrite(model, strings.Join(all, "\n")); ok {
			text, stop = t, st
		}
	}
	if refuse {
		text, stop = "", "refusal"
	}
	return map[string]any{"id": "msg_fake", "type": "message", "role": "assistant", "model": model,
		"content": []any{map[string]any{"type": "text", "text": text}}, "stop_reason": stop,
		"usage": map[string]any{"input_tokens": in, "cache_creation_input_tokens": write, "output_tokens": out}}
}

// pbStrings collects the text of a request: string system and content values and text blocks.
func pbStrings(v any, key string, out *[]string) {
	switch x := v.(type) {
	case string:
		if key == "text" || key == "system" || key == "content" {
			*out = append(*out, x)
		}
	case []any:
		for _, e := range x {
			pbStrings(e, key, out)
		}
	case map[string]any:
		for k, e := range x {
			pbStrings(e, k, out)
		}
	}
}

var pbLineRE = regexp.MustCompile(`(?m)^\[(m\d+)\] (?:\(\+\d+m\) )?[^\n]*?\((p\d+)\): (.*)$`)

// pbFakeAnswer answers a person_extract prompt: "I really like X" is a like of the speaker; "my friend
// drinks X" is hearsay about the other person in Haiku's answer and about the speaker in Sonnet's (a
// disagreement between the two); "my buddy eats X" is about the other person in both. A prompt line can
// hold several messages of one speaker (merged), so each phrase is found anywhere in it.
var pbPhraseRE = regexp.MustCompile(`(I really like|my friend drinks|my buddy eats) (\S+)`)

func pbFakeAnswer(model, prompt string) string {
	lines := pbLineRE.FindAllStringSubmatch(prompt, -1)
	var tokens []string
	for _, m := range lines {
		if !slices.Contains(tokens, m[2]) {
			tokens = append(tokens, m[2])
		}
	}
	other := func(tok string) string {
		for _, o := range tokens {
			if o != tok {
				return o
			}
		}
		return tok
	}
	people := []map[string]any{}
	for _, m := range lines {
		label, tok := m[1], m[2]
		for _, ph := range pbPhraseRE.FindAllStringSubmatch(m[3], -1) {
			topic := ph[2]
			switch ph[1] {
			case "I really like":
				people = append(people, map[string]any{"about": tok, "kind": "like", "topic": topic, "text": "{" + tok + "} likes " + topic,
					"by": []string{tok}, "ev": []string{label}})
			case "my friend drinks":
				about := tok
				if strings.Contains(model, "haiku") {
					about = other(tok)
				}
				people = append(people, map[string]any{"about": about, "kind": "trait", "topic": topic, "text": "{" + about + "} drinks " + topic,
					"by": []string{tok}, "ev": []string{label}})
			case "my buddy eats":
				if o := other(tok); o != tok {
					people = append(people, map[string]any{"about": o, "kind": "like", "topic": topic, "text": "{" + o + "} eats " + topic,
						"by": []string{tok}, "ev": []string{label}})
				}
			}
		}
	}
	raw, _ := json.Marshal(map[string]any{"people": people})
	return string(raw)
}

// ---------------------------------------------------------------------------
// the service
// ---------------------------------------------------------------------------

func pbConfigure(s *Service, h *pbFakeHost, api string) {
	d := s.Doc()
	d.Guilds = map[string]config.GuildSettings{"g1": {Channels: map[string]config.ChannelPolicy{"c1": {Capture: true}}}}
	d.Providers = map[string]config.Provider{"claude": {Kind: config.KindOpenAI, API: api, BaseURL: h.srv.URL + "/v1", CredentialRef: "env:FAKE_CLAUDE_KEY"}}
	s.setDoc(d, s.Revision())
}

func pbService(t *testing.T, h *pbFakeHost, api string) *Service {
	t.Helper()
	s, err := Open(t.TempDir(), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if r := s.Execute(t.Context(), action.Request{Action: "setup.resume", Input: []byte(`{"instance":"pb"}`)}); !r.OK {
		t.Fatal(r.Error)
	}
	s.SetEnvForTest(t, "FAKE_CLAUDE_KEY", "test-key")
	pbConfigure(s, h, api)
	s.SetDiscord(resolverGateway{})
	pbPollEvery.Store(int64(20 * time.Millisecond))
	pbWindowLines = 3        // one synthetic segment per window
	pbTestOrigin = h.srv.URL // the fake host stands in for api.anthropic.com
	t.Cleanup(func() { pbPollEvery.Store(0); pbWindowLines = 0; pbTestOrigin = "" })
	return s
}

// pbSeed captures segments three hours apart: Mina and Joon each say what they like; every fifth has
// a hearsay line the two models read differently, every sixth one they agree on (both name the other
// person by mention), every seventh a bot line.
func pbSeed(t *testing.T, s *Service, segments int) {
	t.Helper()
	e, err := s.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 9, 1, 1, 0, 0, 0, time.UTC)
	for i := range segments {
		at := base.Add(time.Duration(i) * 3 * time.Hour)
		msgs := []engine.LiveMessage{
			{MessageID: fmt.Sprintf("%d1", 1000+i), AuthorID: "101", AuthorName: "Mina", Content: fmt.Sprintf("I really like topic%da today", i), At: at},
			{MessageID: fmt.Sprintf("%d2", 1000+i), AuthorID: "102", AuthorName: "Joon", Content: fmt.Sprintf("I really like topic%db as well", i), At: at.Add(time.Minute)},
		}
		if i%5 == 0 {
			msgs = append(msgs, engine.LiveMessage{MessageID: fmt.Sprintf("%d3", 1000+i), AuthorID: "102", AuthorName: "Joon",
				Content: fmt.Sprintf("my friend drinks tea%d daily <@101>", i), At: at.Add(2 * time.Minute)})
		}
		if i%6 == 0 {
			msgs = append(msgs, engine.LiveMessage{MessageID: fmt.Sprintf("%d5", 1000+i), AuthorID: "101", AuthorName: "Mina",
				Content: fmt.Sprintf("my buddy eats snack%d often <@102>", i), At: at.Add(150 * time.Second)})
		}
		if i%7 == 0 {
			msgs = append(msgs, engine.LiveMessage{MessageID: fmt.Sprintf("%d4", 1000+i), AuthorID: "999", AuthorName: "bot", Bot: true,
				Content: "noted, thank you both", At: at.Add(3 * time.Minute)})
		}
		for _, m := range msgs {
			m.GuildID, m.ChannelID = "g1", "c1"
			if m.Bot {
				// the bot's own lines are stored by its reply path, not by capture
				room := engine.RoomKey("g1", "c1")
				if _, err := s.Store.DB.Exec(`INSERT INTO memory_observations(observation_id,platform,collection,source_room,room,native_message_id,actor_label,actor_account,content,parsed_time,record_kind,imported_at)
					VALUES(?,'discord','discord',?,?,?,?,?,?,?,?,?)`, corpus.DiscordLiveObservationID(room, m.MessageID), room, room, m.MessageID, m.AuthorName, m.AuthorID,
					m.Content, m.At.UTC().Format("2006-01-02T15:04:05+00:00"), corpus.BotRecord, m.At.UTC().Format(time.RFC3339)); err != nil {
					t.Fatal(err)
				}
				continue
			}
			if ok, err := e.Capture(m); err != nil || !ok {
				t.Fatal(ok, err)
			}
		}
	}
}

func pbExec(t *testing.T, s *Service, name string, in map[string]any) (action.Result, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(in)
	res := s.Execute(t.Context(), action.Request{Action: name, Input: raw})
	var out map[string]any
	b, _ := json.Marshal(res.Output)
	_ = json.Unmarshal(b, &out)
	return res, out
}

func pbDry(t *testing.T, s *Service, in map[string]any) map[string]any {
	t.Helper()
	if in == nil {
		in = map[string]any{}
	}
	in["mode"] = "dry_run"
	res, out := pbExec(t, s, "memory.people.backfill", in)
	if !res.OK {
		t.Fatalf("dry run: %+v", res.Error)
	}
	return out
}

func pbWait(t *testing.T, s *Service, what string, ok func(store.BackfillRun) bool) store.BackfillRun {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		runs, err := store.BackfillRuns(s.Store.Reader(), "g1")
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) > 0 && ok(runs[0]) {
			return runs[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: timed out; runs %+v", what, runs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// pbWaitSent waits until the run's first batch is known to the host.
func pbWaitSent(t *testing.T, s *Service) {
	t.Helper()
	pbWait(t, s, "batch sent", func(r store.BackfillRun) bool {
		bs := pbDecodeBatches(r.Batches)
		return len(bs) > 0 && bs[0].State == pbBatchSubmitted
	})
}

func pbState(state string) func(store.BackfillRun) bool {
	return func(r store.BackfillRun) bool { return r.State == state }
}

// pbPiloted runs a dry run and a pilot to its end.
func pbPiloted(t *testing.T, s *Service, in map[string]any) (string, store.BackfillRun) {
	t.Helper()
	dry := pbDry(t, s, in)
	token := dry["token"].(string)
	res, out := pbExec(t, s, "memory.people.backfill", map[string]any{"mode": "pilot", "confirm": token, "guild_id": "g1"})
	if !res.OK {
		t.Fatalf("pilot: %+v", res.Error)
	}
	if out["pilot_windows"].(float64) != peoplebackfill.PilotSize {
		t.Fatalf("pilot windows: %v", out["pilot_windows"])
	}
	return token, pbWait(t, s, "pilot", pbState(store.BackfillPiloted))
}

func pbCountClaims(t *testing.T, s *Service, where string, args ...any) int {
	t.Helper()
	var n int
	if err := s.Store.Reader().QueryRow(`SELECT count(*) FROM person_claim WHERE `+where, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// ---------------------------------------------------------------------------
// tests
// ---------------------------------------------------------------------------

func TestPeopleBackfillDryRunIsFreeAndDeterministic(t *testing.T) {
	h := newPBHost(t)
	s := pbService(t, h, "anthropic")
	pbSeed(t, s, 70)
	first := pbDry(t, s, nil)
	second := pbDry(t, s, nil)
	if first["windows"].(float64) != 70 || first["skipped_windows"].(float64) != 0 || first["pilot_windows"].(float64) != peoplebackfill.PilotSize ||
		first["model_calls"].(float64) != 0 || first["provider_ready"] != true || first["guild_id"] != "g1" {
		t.Fatalf("dry run: %v", first)
	}
	if first["until_from"] != "now" || second["until_from"] != "plan" {
		t.Fatalf("a second dry run plans the same range: %v %v", first["until_from"], second["until_from"])
	}
	delete(first, "until_from")
	delete(second, "until_from")
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(second)
	if string(a) != string(b) {
		t.Fatalf("not deterministic:\n%s\n%s", a, b)
	}
	if h.count() != 0 {
		t.Fatalf("a dry run called the host %d times", h.count())
	}
	// operator only: no Discord command and no chat tool reaches it
	raw, _ := json.Marshal(map[string]any{"mode": "dry_run"})
	res := s.Execute(t.Context(), action.Request{Action: "memory.people.backfill", Input: raw, Invoker: &action.Invoker{Via: "command", GuildID: "g1", UserID: "101", Manager: true}})
	if res.OK || res.Error.Code != errcode.PermissionDenied {
		t.Fatalf("a Discord invoker: %+v", res)
	}
	if !readForm(action.Request{Action: "memory.people.backfill", Input: raw}) || readForm(action.Request{Action: "memory.people.backfill", Input: []byte(`{"mode":"run"}`)}) {
		t.Fatal("dry run is a read; run is a change")
	}
}

func TestPeopleBackfillPlanChangeRefuses(t *testing.T) {
	h := newPBHost(t)
	s := pbService(t, h, "anthropic")
	pbSeed(t, s, 4)
	dry := pbDry(t, s, nil)
	if res, _ := pbExec(t, s, "memory.people.backfill", map[string]any{"mode": "pilot"}); res.OK || res.Error.MessageKey != "people_backfill.confirm_needed" {
		t.Fatalf("no token: %+v", res.Error)
	}
	// an old message captured after the dry run changes the plan
	e, _ := s.MemoryEngine()
	if _, err := e.Capture(engine.LiveMessage{GuildID: "g1", ChannelID: "c1", MessageID: "late1", AuthorID: "103", AuthorName: "Sora",
		Content: "I really like latecomer stories", At: time.Date(2026, 9, 1, 1, 2, 30, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	res, _ := pbExec(t, s, "memory.people.backfill", map[string]any{"mode": "pilot", "confirm": dry["token"]})
	if res.OK || res.Error.MessageKey != "people_backfill.plan_changed" || res.Error.Code != errcode.StaleRevision {
		t.Fatalf("changed plan: %+v", res.Error)
	}
	fresh, _ := res.Error.Args["dry_run"].(map[string]any)
	if fresh == nil || fresh["token"] == dry["token"] || fresh["model_calls"] != 0 {
		t.Fatalf("the refusal carries the new plan: %v", res.Error.Args)
	}
	// the new token is the stored one now; another stale token is still refused
	if res, _ := pbExec(t, s, "memory.people.backfill", map[string]any{"mode": "run", "model": "haiku", "confirm": dry["token"]}); res.OK || res.Error.MessageKey != "people_backfill.plan_changed" {
		t.Fatalf("stale run token: %+v", res.Error)
	}
	if h.count() != 0 {
		t.Fatalf("a refused plan called the host %d times", h.count())
	}
}

func TestPeopleBackfillRunNeedsPilot(t *testing.T) {
	h := newPBHost(t)
	s := pbService(t, h, "anthropic")
	pbSeed(t, s, 4)
	dry := pbDry(t, s, nil)
	res, _ := pbExec(t, s, "memory.people.backfill", map[string]any{"mode": "run", "model": "haiku", "confirm": dry["token"]})
	if res.OK || res.Error.MessageKey != "people_backfill.no_pilot" {
		t.Fatalf("run without pilot: %+v", res.Error)
	}
	if res, _ := pbExec(t, s, "memory.people.backfill", map[string]any{"mode": "run", "model": "opus", "confirm": dry["token"]}); res.OK || res.Error.Code != errcode.InvalidInput {
		t.Fatalf("unknown model: %+v", res.Error)
	}
	if h.count() != 0 {
		t.Fatalf("host called %d times", h.count())
	}
}

func TestPeopleBackfillNeedsNativeAnthropic(t *testing.T) {
	h := newPBHost(t)
	s := pbService(t, h, "compatible")
	pbSeed(t, s, 4)
	dry := pbDry(t, s, nil)
	if dry["provider_ready"] != false {
		t.Fatalf("a compatible host is not ready: %v", dry)
	}
	res, _ := pbExec(t, s, "memory.people.backfill", map[string]any{"mode": "pilot", "confirm": dry["token"]})
	if res.OK || res.Error.Code != errcode.ProviderUnconfigured || res.Error.MessageKey != "people_backfill.no_anthropic" {
		t.Fatalf("non-Anthropic provider: %+v", res.Error)
	}
	// two Anthropic providers need a name
	d := s.Doc()
	d.Providers["claude2"] = config.Provider{Kind: config.KindOpenAI, API: "anthropic", BaseURL: h.srv.URL + "/v1", CredentialRef: "env:FAKE_CLAUDE_KEY"}
	d.Providers["claude"] = config.Provider{Kind: config.KindOpenAI, API: "anthropic", BaseURL: h.srv.URL + "/v1", CredentialRef: "env:FAKE_CLAUDE_KEY"}
	s.setDoc(d, s.Revision())
	if res, _ := pbExec(t, s, "memory.people.backfill", map[string]any{"mode": "pilot", "confirm": dry["token"]}); res.OK || res.Error.MessageKey != "people_backfill.provider_ambiguous" {
		t.Fatalf("two providers: %+v", res.Error)
	}
	// a key that is not there
	d.Providers = map[string]config.Provider{"claude": {Kind: config.KindOpenAI, API: "anthropic", BaseURL: h.srv.URL + "/v1", CredentialRef: "env:NO_SUCH_KEY"}}
	s.setDoc(d, s.Revision())
	if res, _ := pbExec(t, s, "memory.people.backfill", map[string]any{"mode": "pilot", "confirm": dry["token"]}); res.OK || res.Error.Code != errcode.ProviderUnconfigured {
		t.Fatalf("no key: %+v", res.Error)
	}
	// the Messages wire format on another host is not Anthropic: a relay, plain HTTP to Anthropic's host,
	// user info or another port, each with a key; the only such provider is refused before any request
	for _, base := range []string{"https://relay.invalid/v1", "http://api.anthropic.com/v1", "https://user@api.anthropic.com/v1",
		"https://api.anthropic.com:8443/v1", "https://api.anthropic.com.relay.invalid/v1"} {
		d.Providers = map[string]config.Provider{"claude": {Kind: config.KindOpenAI, API: "anthropic", BaseURL: base, CredentialRef: "env:FAKE_CLAUDE_KEY"}}
		s.setDoc(d, s.Revision())
		if dry := pbDry(t, s, nil); dry["provider_ready"] != false {
			t.Fatalf("%s is ready: %v", base, dry)
		}
		if res, _ := pbExec(t, s, "memory.people.backfill", map[string]any{"mode": "pilot", "confirm": dry["token"]}); res.OK || res.Error.MessageKey != "people_backfill.no_anthropic" {
			t.Fatalf("%s: %+v", base, res.Error)
		}
	}
	// the native host is: HTTPS to api.anthropic.com (no request is made by the check)
	d.Providers = map[string]config.Provider{"claude": {Kind: config.KindOpenAI, BaseURL: "https://api.anthropic.com/v1", CredentialRef: "env:FAKE_CLAUDE_KEY"}}
	s.setDoc(d, s.Revision())
	if id, _, err := s.pbAdapter(s.Doc(), ""); err != nil || id != "claude" {
		t.Fatalf("api.anthropic.com: %q %v", id, err)
	}
	if h.count() != 0 {
		t.Fatalf("host called %d times", h.count())
	}
}

// Two foreign origins in the Anthropic wire format: neither is eligible, so the refusal is "no native
// Anthropic", not "name one" (no credential lookup, no request).
func TestPeopleBackfillRejectsForeignOrigins(t *testing.T) {
	s := &Service{}
	doc := config.Document{Providers: map[string]config.Provider{
		"a": {Kind: config.KindOpenAI, API: "anthropic", BaseURL: "https://a.invalid/v1"},
		"b": {Kind: config.KindOpenAI, API: "anthropic", BaseURL: "https://b.invalid/v1"},
	}}
	if _, _, err := s.pbAdapter(doc, ""); !errors.Is(err, errPBNoAnthropic) {
		t.Fatalf("neither origin is Anthropic; got %v", err)
	}
	if _, _, err := s.pbAdapter(doc, "a"); !errors.Is(err, errPBNoAnthropic) {
		t.Fatalf("a named foreign origin; got %v", err)
	}
}

func TestPeopleBackfillPilotAndRun(t *testing.T) {
	h := newPBHost(t)
	s := pbService(t, h, "anthropic")
	pbSeed(t, s, 70)
	token, run := pbPiloted(t, s, nil)
	// both models, standard API, every pilot window once each; no temperature, no cache_control, an output
	// limit the reservation covers
	h.mu.Lock()
	chats := slices.Clone(h.chats)
	h.mu.Unlock()
	if len(chats) != 2*peoplebackfill.PilotSize {
		t.Fatalf("pilot calls: %d", len(chats))
	}
	models := map[string]int{}
	for _, c := range chats {
		models[c["model"].(string)]++
		if _, ok := c["temperature"]; ok {
			t.Fatalf("a request with temperature: %v", c["temperature"])
		}
		if _, ok := c["cache_control"]; ok {
			t.Fatalf("a request with cache_control: %v", c["cache_control"])
		}
		if mt, _ := c["max_tokens"].(float64); mt < pbMaxTokens || mt > pbWireOut {
			t.Fatalf("max_tokens on the wire %v, reserved %d", c["max_tokens"], pbWireOut)
		}
	}
	if models[string(peoplebackfill.ModelHaiku)] != peoplebackfill.PilotSize || models[string(peoplebackfill.ModelSonnet)] != peoplebackfill.PilotSize {
		t.Fatalf("models: %v", models)
	}
	// stored, not applied; counts only in the report; spend recorded
	if n := pbCountClaims(t, s, `source LIKE 'backfill:%'`); n != 0 {
		t.Fatalf("the pilot wrote %d claims", n)
	}
	var rep map[string]any
	if err := json.Unmarshal(run.Pilot, &rep); err != nil {
		t.Fatal(err)
	}
	haiku := rep["models"].(map[string]any)["haiku"].(map[string]any)
	if haiku["answered"].(float64) != peoplebackfill.PilotSize || haiku["facts"].(float64) < 2*peoplebackfill.PilotSize || rep["disagreements"].(float64) == 0 || rep["hearsay"].(float64) == 0 {
		t.Fatalf("pilot report: %v", rep)
	}
	if strings.Contains(string(run.Pilot), "topic") || strings.Contains(string(run.Pilot), "Mina") {
		t.Fatalf("the report carries text: %s", run.Pilot)
	}
	if run.SpentUSD <= 0 {
		t.Fatalf("pilot spend: %v", run.SpentUSD)
	}
	var spend float64
	if err := s.Store.Reader().QueryRow(`SELECT COALESCE(SUM(usd),0) FROM memory_spend WHERE kind=?`, store.SpendPeopleBackfill).Scan(&spend); err != nil || spend <= 0 {
		t.Fatalf("memory_spend: %v %v", spend, err)
	}
	// the review set: rendered labels, no stored references
	res, out := pbExec(t, s, "memory.people.backfill.review", map[string]any{"guild_id": "g1"})
	if !res.OK || out["available"] != true {
		t.Fatalf("review: %+v %v", res.Error, out)
	}
	items := out["items"].([]any)
	if len(items) == 0 || float64(len(items)) != rep["review_items"].(float64) {
		t.Fatalf("review items: %d vs %v", len(items), rep["review_items"])
	}
	for _, it := range items {
		m := it.(map[string]any)
		if strings.Contains(m["text"].(string), "{@") || (m["subject"] != "Mina" && m["subject"] != "Joon") || m["part"] == "" {
			t.Fatalf("review item: %v", m)
		}
	}
	// the run: pilot windows from the stored Haiku answers, the other 10 in one batch (results shuffled)
	before := len(chats)
	res, _ = pbExec(t, s, "memory.people.backfill", map[string]any{"mode": "run", "model": "haiku", "confirm": token})
	if !res.OK {
		t.Fatalf("run: %+v", res.Error)
	}
	done := pbWait(t, s, "run", pbState(store.BackfillDone))
	h.mu.Lock()
	after := len(h.chats)
	h.mu.Unlock()
	if after != before {
		t.Fatalf("the run made %d live calls", after-before)
	}
	creates := h.creations()
	if len(creates) != 1 {
		t.Fatalf("batches: %d", len(creates))
	}
	reqs := creates[0]["requests"].([]any)
	if len(reqs) != 70-peoplebackfill.PilotSize {
		t.Fatalf("batch items: %d", len(reqs))
	}
	for _, r := range reqs {
		p := r.(map[string]any)["params"].(map[string]any)
		if _, ok := p["temperature"]; ok || p["model"] != string(peoplebackfill.ModelHaiku) {
			t.Fatalf("batch params: %v", p)
		}
		if _, ok := p["cache_control"]; ok {
			t.Fatalf("batch item with cache_control: %v", p)
		}
		if mt, _ := p["max_tokens"].(float64); mt < pbMaxTokens || mt > pbWireOut {
			t.Fatalf("batch max_tokens %v, reserved %d", p["max_tokens"], pbWireOut)
		}
	}
	c, _ := store.CountBackfillWindows(s.Store.Reader(), done.ID)
	if c.States[store.WindowApplied] != 70 || c.Claims == 0 {
		t.Fatalf("windows: %+v", c)
	}
	// every window's two likes are claims, written by their own speakers
	if n := pbCountClaims(t, s, `source=? AND kind='like' AND link='self' AND subject_ref=speaker_ref`, "backfill:"+done.ID); n != 140 {
		t.Fatalf("like claims: %d", n)
	}
	// review is gone once the run started; the stored plan is cleared when the run is done
	if _, out := pbExec(t, s, "memory.people.backfill.review", map[string]any{"guild_id": "g1"}); out["available"] != false {
		t.Fatalf("review after the run: %v", out)
	}
	var plan pbParams
	if ok, _ := s.kvGet(pbPlanKey+"g1", &plan); ok {
		t.Fatalf("the plan is kept after the run: %+v", plan)
	}
	if done.SpentUSD <= run.SpentUSD {
		t.Fatalf("batch spend not settled: %v → %v", run.SpentUSD, done.SpentUSD)
	}
	// running the plan again writes nothing new (the gate's ids are the same)
	if res, _ := pbExec(t, s, "memory.people.backfill", map[string]any{"mode": "status", "guild_id": "g1"}); !res.OK {
		t.Fatal(res.Error)
	}
}

// pbRunAll queues every window of a finished pilot run as the run would (pilot answers dropped), so
// the batch path can be checked on its own.
func pbBatchOnly(t *testing.T, s *Service, run store.BackfillRun) {
	t.Helper()
	if _, err := s.Store.DB.Exec(`UPDATE people_backfill_window SET result='' WHERE run_id=?`, run.ID); err != nil {
		t.Fatal(err)
	}
}

// pbWindowMax is the reserved maximum of a queued window on Haiku batch.
func pbWindowMax(t *testing.T, s *Service, run store.BackfillRun) []float64 {
	t.Helper()
	ws, err := store.BackfillQueue(s.Store.Reader(), run.ID, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var out []float64
	for _, w := range ws {
		bp, err := s.pbBuildPrompt(s.Store, "g1", w)
		if err != nil || !bp.ok {
			t.Fatal(err, bp.ok)
		}
		out = append(out, pbMaxCost(bp, peoplebackfill.ModelHaiku, peoplebackfill.ModeBatch))
	}
	return out
}

func pbSetSpent(t *testing.T, s *Service, id string, v float64) {
	t.Helper()
	if _, err := s.Store.DB.Exec(`UPDATE people_backfill_run SET spent_usd=? WHERE id=?`, v, id); err != nil {
		t.Fatal(err)
	}
}

func TestPeopleBackfillBudgetStopsBeforeSubmit(t *testing.T) {
	h := newPBHost(t)
	s := pbService(t, h, "anthropic")
	pbSeed(t, s, 70)
	token, run := pbPiloted(t, s, nil)
	pbBatchOnly(t, s, run)
	maxes := pbWindowMax(t, s, run)
	// less than one window's maximum is left
	pbSetSpent(t, s, run.ID, run.BudgetUSD-maxes[0]/2)
	if res, _ := pbExec(t, s, "memory.people.backfill", map[string]any{"mode": "run", "model": "haiku", "confirm": token}); !res.OK {
		t.Fatal(res.Error)
	}
	stopped := pbWait(t, s, "budget stop", pbState(store.BackfillStopped))
	if stopped.StoppedBy != "budget" || len(h.creations()) != 0 {
		t.Fatalf("stopped by %q after %d batches", stopped.StoppedBy, len(h.creations()))
	}
	c, _ := store.CountBackfillWindows(s.Store.Reader(), run.ID)
	if c.States[store.WindowQueued] != 70 {
		t.Fatalf("the rest stays queued: %+v", c)
	}
}

func TestPeopleBackfillBudgetStopsAfterSettlement(t *testing.T) {
	h := newPBHost(t)
	s := pbService(t, h, "anthropic")
	pbSeed(t, s, 70)
	token, run := pbPiloted(t, s, nil)
	pbBatchOnly(t, s, run)
	maxes := pbWindowMax(t, s, run)
	room := 0.0
	for _, v := range maxes[:5] {
		room += v
	}
	room += maxes[5] / 2 // five windows fit, the sixth does not
	pbSetSpent(t, s, run.ID, run.BudgetUSD-room)
	h.fullUsage.Store(true) // each answer costs its reserved maximum
	if res, _ := pbExec(t, s, "memory.people.backfill", map[string]any{"mode": "run", "model": "haiku", "confirm": token}); !res.OK {
		t.Fatal(res.Error)
	}
	stopped := pbWait(t, s, "budget stop", pbState(store.BackfillStopped))
	creates := h.creations()
	if stopped.StoppedBy != "budget" || len(creates) != 1 || len(creates[0]["requests"].([]any)) != 5 {
		t.Fatalf("stopped by %q after %d batches", stopped.StoppedBy, len(creates))
	}
	c, _ := store.CountBackfillWindows(s.Store.Reader(), run.ID)
	if c.States[store.WindowApplied] != 5 || c.States[store.WindowQueued] != 65 {
		t.Fatalf("windows: %+v", c)
	}
	if stopped.SpentUSD > stopped.BudgetUSD+1e-9 {
		t.Fatalf("spent %v over the cap %v", stopped.SpentUSD, stopped.BudgetUSD)
	}
}

func TestPeopleBackfillRestartPollsOnly(t *testing.T) {
	h := newPBHost(t)
	s := pbService(t, h, "anthropic")
	pbSeed(t, s, 70)
	token, run := pbPiloted(t, s, nil)
	h.running.Store(true)
	if res, _ := pbExec(t, s, "memory.people.backfill", map[string]any{"mode": "run", "model": "haiku", "confirm": token}); !res.OK {
		t.Fatal(res.Error)
	}
	pbWaitSent(t, s)
	if len(h.creations()) != 1 {
		t.Fatal("no batch sent")
	}
	// a pilot cut by the restart: another server's, seeded as it would be left
	if err := store.InsertBackfillRun(s.Store.DB, store.BackfillRun{ID: "pb_cut", GuildID: "g2", State: store.BackfillPiloting, Token: "t", Params: []byte(`{}`),
		Provider: "claude", BudgetUSD: 1, CreatedAt: "2026-11-01T00:00:00Z", UpdatedAt: "2026-11-01T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if s.pbActive(run.ID) {
		t.Fatal("the worker outlived Close")
	}
	s2, err := ReopenForTest(s, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s2.Close() })
	pbConfigure(s2, h, "anthropic")
	s2.SetDiscord(resolverGateway{})
	requests := h.count()
	s2.ResumeJobs(t.Context())
	if cut, _, _ := store.LoadBackfillRun(s2.Store.Reader(), "pb_cut"); cut.State != store.BackfillStopped || cut.StoppedBy != "restart" {
		t.Fatalf("a cut pilot: %+v", cut)
	}
	h.running.Store(false)
	done := pbWait(t, s2, "resumed run", pbState(store.BackfillDone))
	if len(h.creations()) != 1 {
		t.Fatalf("a restart sent %d batches", len(h.creations())-1)
	}
	h.mu.Lock()
	chats := len(h.chats)
	h.mu.Unlock()
	if chats != 2*peoplebackfill.PilotSize || h.count() == requests {
		t.Fatalf("after the restart: %d live calls, %d requests", chats, h.count()-requests)
	}
	c, _ := store.CountBackfillWindows(s2.Store.Reader(), done.ID)
	if c.States[store.WindowApplied] != 70 {
		t.Fatalf("shuffled results not all applied: %+v", c)
	}
	if cut, _, _ := store.LoadBackfillRun(s2.Store.Reader(), "pb_cut"); cut.State != store.BackfillStopped {
		t.Fatalf("a cut pilot is never resumed by a restart: %+v", cut)
	}
}

func TestPeopleBackfillResumedWorkerSendsNothingNew(t *testing.T) {
	h := newPBHost(t)
	s := pbService(t, h, "anthropic")
	pbSeed(t, s, 70)
	token, run := pbPiloted(t, s, nil)
	pbBatchOnly(t, s, run)
	h.running.Store(true)
	if res, _ := pbExec(t, s, "memory.people.backfill", map[string]any{"mode": "run", "model": "haiku", "confirm": token}); !res.OK {
		t.Fatal(res.Error)
	}
	pbWaitSent(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := ReopenForTest(s, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s2.Close() })
	// the one batch covers every window; mark ten as never sent, as after a cap stop
	if _, err := s2.Store.DB.Exec(`UPDATE people_backfill_window SET state='queued', batch='' WHERE run_id=? AND seq>=60`, run.ID); err != nil {
		t.Fatal(err)
	}
	pbConfigure(s2, h, "anthropic")
	s2.ResumeJobs(t.Context())
	h.running.Store(false)
	stopped := pbWait(t, s2, "resumed run", pbState(store.BackfillStopped))
	if stopped.StoppedBy != "restart" || len(h.creations()) != 1 {
		t.Fatalf("a resumed worker: stopped by %q, %d batches", stopped.StoppedBy, len(h.creations()))
	}
	c, _ := store.CountBackfillWindows(s2.Store.Reader(), run.ID)
	if c.States[store.WindowQueued] != 10 || c.States[store.WindowApplied] != 60 {
		t.Fatalf("windows: %+v", c)
	}
	// the operator continues it: only now is the rest sent. A message deleted meanwhile changes the plan,
	// not the started run, which is continued by its own token.
	if _, err := s2.Store.DB.Exec(`UPDATE memory_observations SET deleted_at='2026-11-01T00:00:00Z' WHERE observation_id=(SELECT observation_id FROM memory_observations ORDER BY parsed_time LIMIT 1)`); err != nil {
		t.Fatal(err)
	}
	if res, _ := pbExec(t, s2, "memory.people.backfill", map[string]any{"mode": "run", "model": "sonnet", "confirm": token}); res.OK || res.Error.Code != errcode.InvalidInput {
		t.Fatalf("another model for a started run: %+v", res.Error)
	}
	if res, _ := pbExec(t, s2, "memory.people.backfill", map[string]any{"mode": "run", "model": "haiku", "confirm": token}); !res.OK {
		t.Fatal(res.Error)
	}
	pbWait(t, s2, "continued run", pbState(store.BackfillDone))
	if len(h.creations()) != 2 {
		t.Fatalf("batches: %d", len(h.creations()))
	}
}

func TestPeopleBackfillApplyGuard(t *testing.T) {
	h := newPBHost(t)
	s := pbService(t, h, "anthropic")
	pbSeed(t, s, 70)
	token, run := pbPiloted(t, s, nil)
	pbBatchOnly(t, s, run)
	h.running.Store(true)
	if res, _ := pbExec(t, s, "memory.people.backfill", map[string]any{"mode": "run", "model": "haiku", "confirm": token}); !res.OK {
		t.Fatal(res.Error)
	}
	pbWaitSent(t, s)
	if len(h.creations()) != 1 {
		t.Fatal("no batch sent")
	}
	ws, err := store.BackfillWindows(s.Store.Reader(), run.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	// while the batch runs: a line is deleted, a line is edited, Mina withdraws consent in one window's
	// time (she is in every window, so all her claims go), a tombstone and a forget hit Joon's claims
	deleted, edited := ws[3].Obs[0], ws[4].Obs[0]
	db := s.Store.DB
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE memory_observations SET deleted_at='2026-11-01T00:00:00Z' WHERE observation_id=?`, []any{deleted}},
		{`UPDATE memory_observations SET content='I really like something else now' WHERE observation_id=?`, []any{edited}},
		{`INSERT INTO consent_state(user_id,scope,status,updated_at) VALUES('101','memory','withdrawn','2026-11-01T00:00:00Z')`, nil},
		{`INSERT INTO person_memory_suppress(guild_id,person_ref,role,scope,before_ts,by,created_at) VALUES('g1',?,'subject','all','9999-12-31T00:00:00.000000000Z','self','x')`, []any{store.PersonRef("102")}},
	} {
		if _, err := db.Exec(q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	h.running.Store(false)
	done := pbWait(t, s, "guarded run", func(r store.BackfillRun) bool {
		return r.State == store.BackfillDone || r.State == store.BackfillStopped
	})
	if done.State != store.BackfillDone {
		t.Fatalf("run: %+v", done)
	}
	if n := pbCountClaims(t, s, `source=?`, "backfill:"+run.ID); n != 0 {
		t.Fatalf("claims of a withdrawn and a suppressed person were written: %d", n)
	}
	if n := pbCountClaims(t, s, `evidence LIKE ?`, "%"+deleted+"%"); n != 0 {
		t.Fatal("a claim on a deleted line")
	}
	// the windows whose prompt changed went back to the queue and out again: Mina's withdrawal takes her
	// lines out of every prompt, so all of them, built again without her lines
	creates := h.creations()
	if len(creates) != 2 || len(creates[1]["requests"].([]any)) != len(ws) {
		t.Fatalf("changed windows are sent again: %d batches", len(creates))
	}
	var again []string
	pbStrings(creates[1], "", &again)
	if body := strings.Join(again, "\n"); strings.Contains(body, "a today") || !strings.Contains(body, "b as well") {
		t.Fatal("a withdrawn person's lines were sent again")
	}
	w3, _, _ := store.LoadBackfillWindow(s.Store.Reader(), run.ID, ws[3].ID)
	w5, _, _ := store.LoadBackfillWindow(s.Store.Reader(), run.ID, ws[5].ID)
	if w3.Attempts != 2 || w3.State != store.WindowApplied || w5.Attempts != 2 || w5.Rejected == 0 {
		t.Fatalf("windows: %+v %+v", w3, w5)
	}
}

func TestPeopleBackfillRefusalIsCountedAndSkipped(t *testing.T) {
	h := newPBHost(t)
	s := pbService(t, h, "anthropic")
	pbSeed(t, s, 70)
	token, run := pbPiloted(t, s, nil)
	pbBatchOnly(t, s, run)
	ws, _ := store.BackfillWindows(s.Store.Reader(), run.ID, false)
	h.mu.Lock()
	h.refuse[ws[2].ID] = true
	h.mu.Unlock()
	if res, _ := pbExec(t, s, "memory.people.backfill", map[string]any{"mode": "run", "model": "haiku", "confirm": token}); !res.OK {
		t.Fatal(res.Error)
	}
	done := pbWait(t, s, "run", pbState(store.BackfillDone))
	if len(h.creations()) != 1 {
		t.Fatalf("a refused window is never sent again: %d batches", len(h.creations()))
	}
	w, _, _ := store.LoadBackfillWindow(s.Store.Reader(), run.ID, ws[2].ID)
	if w.State != store.WindowRefused || w.CostUSD <= 0 || w.Claims != 0 {
		t.Fatalf("refused window: %+v", w)
	}
	c, _ := store.CountBackfillWindows(s.Store.Reader(), done.ID)
	if c.States[store.WindowRefused] != 1 || c.States[store.WindowApplied] != 69 {
		t.Fatalf("windows: %+v", c)
	}
}

// TestPeopleBackfillNotStartedByPasses: a stored plan and a finished pilot are left alone by a restart,
// the room-memory pass and the memory pass; nothing reaches the host.
func TestPeopleBackfillNotStartedByPasses(t *testing.T) {
	h := newPBHost(t)
	s := pbService(t, h, "anthropic")
	pbSeed(t, s, 70)
	_, run := pbPiloted(t, s, nil)
	requests := h.count()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := ReopenForTest(s, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s2.Close() })
	pbConfigure(s2, h, "anthropic")
	s2.ResumeJobs(t.Context())
	s2.RunRoomMemory(t.Context())
	s2.memoryPass(t.Context())
	time.Sleep(100 * time.Millisecond)
	if h.count() != requests {
		t.Fatalf("passes called the host %d times", h.count()-requests)
	}
	after, _, _ := store.LoadBackfillRun(s2.Store.Reader(), run.ID)
	if after.State != store.BackfillPiloted || after.UpdatedAt != run.UpdatedAt || len(after.Batches) != 0 || s2.pbActive(run.ID) {
		t.Fatalf("run touched: %+v", after)
	}
	c, _ := store.CountBackfillWindows(s2.Store.Reader(), run.ID)
	if c.States[store.WindowQueued] != 70 {
		t.Fatalf("windows touched: %+v", c)
	}
}

// budget_usd is the server's cap: $3 by default, at most $6, kept to the cent below; anything malformed,
// zero, negative or over the ceiling is refused, never replaced by the default.
func TestPeopleBackfillBudgetInput(t *testing.T) {
	h := newPBHost(t)
	s := pbService(t, h, "anthropic")
	pbSeed(t, s, 4)
	for _, c := range []struct {
		in   any
		want float64
	}{{nil, 3}, {"", 3}, {"4", 4}, {5, 5}, {6.0, 6}, {"2.999", 2.99}, {"0.29", 0.29}, {"1", 1}} {
		in := map[string]any{}
		if c.in != nil {
			in["budget_usd"] = c.in
		}
		if out := pbDry(t, s, in); out["budget_usd"].(float64) != c.want {
			t.Fatalf("budget %v: %v", c.in, out["budget_usd"])
		}
	}
	for _, bad := range []any{"7", 6.01, 100, "-1", -1, "0", 0, "abc", true, map[string]any{"usd": 1}, "NaN", "Inf", "0.001"} {
		res, _ := pbExec(t, s, "memory.people.backfill", map[string]any{"mode": "dry_run", "budget_usd": bad})
		if res.OK || res.Error.Code != errcode.InvalidInput || res.Error.Args["field"] != "budget_usd" {
			t.Fatalf("budget %v: %+v", bad, res)
		}
	}
	if h.count() != 0 {
		t.Fatalf("host called %d times", h.count())
	}
}

// The cap is a total across every run of the server: an earlier run's spend counts against a new plan's
// pilot, and dry run and status show it next to the cap.
func TestPeopleBackfillCapIsServerTotal(t *testing.T) {
	h := newPBHost(t)
	s := pbService(t, h, "anthropic")
	pbSeed(t, s, 70)
	if err := store.InsertBackfillRun(s.Store.DB, store.BackfillRun{ID: "pb_old", GuildID: "g1", State: store.BackfillSuperseded, Token: "t", Params: []byte(`{}`),
		Provider: "claude", BudgetUSD: 3, SpentUSD: 2.95, CreatedAt: "2026-11-01T00:00:00Z", UpdatedAt: "2026-11-01T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	dry := pbDry(t, s, nil)
	if dry["spent_usd"].(float64) != 2.95 || dry["left_usd"].(float64) != 0.05 {
		t.Fatalf("dry run spend: %v %v", dry["spent_usd"], dry["left_usd"])
	}
	if w := dry["within_budget"].(map[string]any); w["haiku"] != false || w["sonnet"] != false {
		t.Fatalf("within budget with $0.05 left: %v", w)
	}
	res, _ := pbExec(t, s, "memory.people.backfill", map[string]any{"mode": "pilot", "confirm": dry["token"]})
	if res.OK || res.Error.Code != errcode.BudgetExceeded || res.Error.Args["spent_usd"].(float64) != 2.95 {
		t.Fatalf("pilot over the server's total: %+v", res)
	}
	if h.count() != 0 {
		t.Fatalf("host called %d times", h.count())
	}
	// a cap of $6 leaves room: the same server total is shown in status
	dry = pbDry(t, s, map[string]any{"budget_usd": "6"})
	if res, _ := pbExec(t, s, "memory.people.backfill", map[string]any{"mode": "pilot", "confirm": dry["token"]}); !res.OK {
		t.Fatalf("pilot with room: %+v", res.Error)
	}
	run := pbWait(t, s, "pilot", pbState(store.BackfillPiloted))
	_, out := pbExec(t, s, "memory.people.backfill", map[string]any{"mode": "status", "guild_id": "g1"})
	st := out["run"].(map[string]any)
	if got := st["guild_spent_usd"].(float64); got < 2.95+run.SpentUSD-1e-6 || got > 2.95+run.SpentUSD+1e-6 {
		t.Fatalf("status server total %v, run %v", got, run.SpentUSD)
	}
}

// The reservation of a window is the most it can be billed: every input token written to the cache
// (1.25 of the input price) and the whole output limit on the wire, whatever the text's language.
func TestPeopleBackfillReservesCacheWrites(t *testing.T) {
	for _, bp := range []pbPrompt{
		{user: strings.Repeat("a", 4000-64)},
		{user: strings.Repeat("🧪", 2000)},
		{system: strings.Repeat("s", 1100), user: strings.Repeat("u", 6000)},
		{system: "시스템", user: strings.Repeat("가나다라 ", 900)},
	} {
		for _, model := range []peoplebackfill.Model{peoplebackfill.ModelHaiku, peoplebackfill.ModelSonnet} {
			for _, mode := range []peoplebackfill.Mode{peoplebackfill.ModeBatch, peoplebackfill.ModeStandard} {
				reserved := pbMaxCost(bp, model, mode)
				// a token is at least one byte: the prompt's bytes bound its tokens, all billed as cache writes
				in := len(bp.system) + len(bp.user) + pbFraming
				for _, res := range []*openai.ChatResult{
					{PromptTokens: 4000, CacheWriteTokens: 4000, CompletionTokens: 1400},
					{PromptTokens: in, CacheWriteTokens: in, CompletionTokens: pbWireOut},
					{PromptTokens: in, CompletionTokens: pbWireOut},
				} {
					if res.PromptTokens > in {
						continue
					}
					if billed := pbSettle(res, model, mode); billed > reserved+1e-12 {
						t.Fatalf("%s %s: reserved %.6f, billable %.6f (%+v)", model, mode, reserved, billed, res)
					}
				}
			}
		}
	}
}

// A pilot send whose answer never arrives (the host read it and the connection closed) may have been
// billed: it is charged at its reserved maximum, the pilot stops until the operator goes on, and the
// operator's resume asks it again under a new reservation; it is never resent by the client.
func TestPeopleBackfillPilotUnknownSendIsHeld(t *testing.T) {
	h := newPBHost(t)
	s := pbService(t, h, "anthropic")
	pbSeed(t, s, 70)
	dry := pbDry(t, s, nil)
	h.mu.Lock()
	h.drop = 1
	h.mu.Unlock()
	if res, _ := pbExec(t, s, "memory.people.backfill", map[string]any{"mode": "pilot", "confirm": dry["token"], "guild_id": "g1"}); !res.OK {
		t.Fatalf("pilot: %+v", res.Error)
	}
	stopped := pbWait(t, s, "unknown stop", pbState(store.BackfillStopped))
	if stopped.StoppedBy != "submit_unknown" || stopped.PilotDone {
		t.Fatalf("pilot after an unknown send: %+v", stopped)
	}
	ws, _ := store.BackfillWindows(s.Store.Reader(), stopped.ID, true)
	var unknown []pbAnswer
	held := 0.0
	for _, w := range ws {
		for m, a := range pbAnswers(w.Result) {
			if a.Unknown {
				unknown = append(unknown, a)
				bp, err := s.pbBuildPrompt(s.Store, "g1", w)
				if err != nil {
					t.Fatal(err)
				}
				held = pbMaxCost(bp, peoplebackfill.Model(m), peoplebackfill.ModeStandard)
			}
			if a.Intent != 0 {
				t.Fatalf("an intent left after the pilot stopped: %+v", a)
			}
		}
	}
	if len(unknown) != 1 || unknown[0].Failed != pbUnknownClass || unknown[0].USD != held {
		t.Fatalf("unknown answers: %+v (held %v)", unknown, held)
	}
	var usage float64
	if err := s.Store.Reader().QueryRow(`SELECT COALESCE(SUM(usd),0) FROM memory_spend WHERE kind=?`, store.SpendPeopleBackfill).Scan(&usage); err != nil {
		t.Fatal(err)
	}
	if stopped.SpentUSD < held || stopped.SpentUSD-held > usage+1e-9 {
		t.Fatalf("run spend %v: held %v, reported usage %v", stopped.SpentUSD, held, usage)
	}
	h.mu.Lock()
	sentBefore := len(h.chats)
	h.mu.Unlock()
	if sentBefore >= 2*peoplebackfill.PilotSize {
		t.Fatalf("the pilot went on after an unknown send: %d calls", sentBefore)
	}
	// the operator resumes: only the missing and the unknown answers are asked, each once
	if res, _ := pbExec(t, s, "memory.people.backfill", map[string]any{"mode": "pilot", "confirm": dry["token"], "guild_id": "g1"}); !res.OK {
		t.Fatalf("resume: %+v", res.Error)
	}
	done := pbWait(t, s, "resumed pilot", pbState(store.BackfillPiloted))
	h.mu.Lock()
	total := len(h.chats)
	h.mu.Unlock()
	if total != 2*peoplebackfill.PilotSize+1 {
		t.Fatalf("calls: %d, want every answer once plus the lost one", total)
	}
	if done.SpentUSD < stopped.SpentUSD {
		t.Fatalf("the held maximum was released: %v → %v", stopped.SpentUSD, done.SpentUSD)
	}
}

// A send cut by a crash between its stored intent and its answer is charged at its maximum when the pilot
// is resumed, then asked again.
func TestPeopleBackfillPilotIntentSurvivesCrash(t *testing.T) {
	h := newPBHost(t)
	s := pbService(t, h, "anthropic")
	pbSeed(t, s, 70)
	_, run := pbPiloted(t, s, nil)
	ws, _ := store.BackfillWindows(s.Store.Reader(), run.ID, true)
	w := ws[0]
	have := pbAnswers(w.Result)
	have[string(peoplebackfill.ModelSonnet)] = pbAnswer{Hash: "h", Intent: 0.5}
	w.Result, _ = json.Marshal(have)
	if err := store.SaveBackfillWindow(s.Store.DB, w); err != nil {
		t.Fatal(err)
	}
	// as a restart leaves a cut pilot
	if _, err := s.Store.DB.Exec(`UPDATE people_backfill_run SET state='stopped', stopped_by='restart', pilot_done=0 WHERE id=?`, run.ID); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	before := len(h.chats)
	h.mu.Unlock()
	if res, _ := pbExec(t, s, "memory.people.backfill", map[string]any{"mode": "pilot", "confirm": run.Token, "guild_id": "g1"}); !res.OK {
		t.Fatalf("resume: %+v", res.Error)
	}
	done := pbWait(t, s, "resumed pilot", pbState(store.BackfillPiloted))
	h.mu.Lock()
	after := len(h.chats)
	h.mu.Unlock()
	if after-before != 1 {
		t.Fatalf("resume asked %d calls, want the cut one only", after-before)
	}
	if done.SpentUSD < run.SpentUSD+0.5-1e-9 {
		t.Fatalf("the cut send is not charged at its maximum: %v → %v", run.SpentUSD, done.SpentUSD)
	}
}

// Fable R8: a legitimate answer whose whole prompt was written to the cache (usage
// cache_creation_input_tokens equal to the prompt) settles above a reservation that priced input at the
// base rate; the reservation must still be an upper bound.
func TestPeopleBackfillReservationIsAnUpperBound(t *testing.T) {
	bp := pbPrompt{system: strings.Repeat("s", 1100), user: strings.Repeat("u", 6000)}
	for _, model := range []peoplebackfill.Model{peoplebackfill.ModelHaiku, peoplebackfill.ModelSonnet} {
		for _, mode := range []peoplebackfill.Mode{peoplebackfill.ModeBatch, peoplebackfill.ModeStandard} {
			reserved := pbMaxCost(bp, model, mode)
			prompt := 1100 + 6000 + 64
			// the adapter reports PromptTokens = input + cache_read + cache_creation
			res := &openai.ChatResult{PromptTokens: prompt, CacheWriteTokens: prompt, CompletionTokens: pbMaxTokens}
			if settled := pbSettle(res, model, mode); settled > reserved {
				t.Errorf("%s %s: settled %.6f > reserved %.6f (+%.0f%%)", model, mode, settled, reserved, (settled/reserved-1)*100)
			}
		}
	}
}

// An answer cut at its output limit, a JSON object that is not the people list, or plain text is a
// failed window (billed, sent again up to the attempt limit, then failed), never an applied window with
// nothing to say; a complete empty list is.
func TestPeopleBackfillRejectsIncompleteAnswer(t *testing.T) {
	h := newPBHost(t)
	s := pbService(t, h, "anthropic")
	pbSeed(t, s, 70)
	token, run := pbPiloted(t, s, nil)
	pbBatchOnly(t, s, run)
	full := `{"people":[{"about":"p1","kind":"like","topic":"x","text":"{p1} likes x","by":["p1"],"ev":["m1"]}]}`
	answers := map[string][2]string{
		"topic3a today": {full[:len(full)-20], "max_tokens"},
		"topic4a today": {`{"answer":"nothing here"}`, "end_turn"},
		"topic8a today": {"I cannot find anything about people.", "end_turn"},
		"topic9a today": {`{"people":[]}`, "end_turn"},
	}
	h.mu.Lock()
	h.rewrite = func(_, prompt string) (string, string, bool) {
		for k, v := range answers {
			if strings.Contains(prompt, k) {
				return v[0], v[1], true
			}
		}
		return "", "", false
	}
	h.mu.Unlock()
	if res, _ := pbExec(t, s, "memory.people.backfill", map[string]any{"mode": "run", "model": "haiku", "confirm": token}); !res.OK {
		t.Fatal(res.Error)
	}
	done := pbWait(t, s, "run", pbState(store.BackfillDone))
	ws, _ := store.BackfillWindows(s.Store.Reader(), done.ID, false)
	for _, i := range []int{3, 4, 8} {
		if w := ws[i]; w.State != store.WindowFailed || w.Attempts != pbMaxAttempts || w.Claims != 0 || w.CostUSD <= 0 {
			t.Fatalf("window %d: %+v", i, w)
		}
	}
	if w := ws[9]; w.State != store.WindowApplied || w.Claims != 0 || w.Attempts != 1 {
		t.Fatalf("a complete empty answer: %+v", w)
	}
	c, _ := store.CountBackfillWindows(s.Store.Reader(), done.ID)
	if c.States[store.WindowFailed] != 3 || c.States[store.WindowApplied] != 67 {
		t.Fatalf("windows: %+v", c)
	}
}

// A pilot with a failed answer does not pass the gate: it stops (failed) and the run is refused until a
// resumed pilot has every answer; the cut answer is billed and counted, not taken as an empty window.
func TestPeopleBackfillFailedPilotDoesNotPass(t *testing.T) {
	h := newPBHost(t)
	s := pbService(t, h, "anthropic")
	pbSeed(t, s, 70)
	cut := true
	h.mu.Lock()
	h.rewrite = func(model, prompt string) (string, string, bool) {
		if cut && strings.Contains(model, "haiku") && strings.Contains(prompt, "topic0a today") {
			return `{"people":[{"about":"p1","kind":"li`, "max_tokens", true
		}
		return "", "", false
	}
	h.mu.Unlock()
	dry := pbDry(t, s, nil)
	if res, _ := pbExec(t, s, "memory.people.backfill", map[string]any{"mode": "pilot", "confirm": dry["token"], "guild_id": "g1"}); !res.OK {
		t.Fatal(res.Error)
	}
	stopped := pbWait(t, s, "failed pilot", pbState(store.BackfillStopped))
	if stopped.StoppedBy != "failed" || stopped.PilotDone {
		t.Fatalf("pilot with a cut answer: %+v", stopped)
	}
	var rep map[string]any
	_ = json.Unmarshal(stopped.Pilot, &rep)
	if haiku := rep["models"].(map[string]any)["haiku"].(map[string]any); haiku["failed"].(float64) != 1 || haiku["answered"].(float64) != peoplebackfill.PilotSize-1 {
		t.Fatalf("report: %v", haiku)
	}
	if res, _ := pbExec(t, s, "memory.people.backfill", map[string]any{"mode": "run", "model": "haiku", "confirm": dry["token"]}); res.OK || res.Error.MessageKey != "people_backfill.no_pilot" {
		t.Fatalf("run after a failed pilot: %+v", res)
	}
	h.mu.Lock()
	cut = false
	before := len(h.chats)
	h.mu.Unlock()
	if res, _ := pbExec(t, s, "memory.people.backfill", map[string]any{"mode": "pilot", "confirm": dry["token"], "guild_id": "g1"}); !res.OK {
		t.Fatal(res.Error)
	}
	pbWait(t, s, "resumed pilot", pbState(store.BackfillPiloted))
	h.mu.Lock()
	after := len(h.chats)
	h.mu.Unlock()
	if after-before != 1 {
		t.Fatalf("the resume asked %d calls, want the failed one", after-before)
	}
}

// An all-error pilot (the host answers 500) passes nothing: each request is sent once (no client retry),
// nothing is charged, and run is refused.
func TestPeopleBackfillAllErrorPilot(t *testing.T) {
	h := newPBHost(t)
	s := pbService(t, h, "anthropic")
	pbSeed(t, s, 70)
	h.mu.Lock()
	h.fail500 = 1 << 20
	h.mu.Unlock()
	dry := pbDry(t, s, nil)
	if res, _ := pbExec(t, s, "memory.people.backfill", map[string]any{"mode": "pilot", "confirm": dry["token"], "guild_id": "g1"}); !res.OK {
		t.Fatal(res.Error)
	}
	stopped := pbWait(t, s, "failed pilot", pbState(store.BackfillStopped))
	h.mu.Lock()
	chats := len(h.chats)
	h.mu.Unlock()
	if stopped.StoppedBy != "failed" || stopped.PilotDone || chats != 2*peoplebackfill.PilotSize || stopped.SpentUSD != 0 {
		t.Fatalf("all-error pilot: %+v after %d calls", stopped, chats)
	}
	if res, _ := pbExec(t, s, "memory.people.backfill", map[string]any{"mode": "run", "model": "haiku", "confirm": dry["token"]}); res.OK || res.Error.MessageKey != "people_backfill.no_pilot" {
		t.Fatalf("run after an all-error pilot: %+v", res)
	}
}

// A window one pilot model refused is refused, even when the run's model answered it.
func TestPeopleBackfillPilotRefusalOfEitherModel(t *testing.T) {
	h := newPBHost(t)
	s := pbService(t, h, "anthropic")
	pbSeed(t, s, 70)
	h.mu.Lock()
	h.rewrite = func(model, prompt string) (string, string, bool) {
		if strings.Contains(model, "sonnet") && strings.Contains(prompt, "topic0a today") {
			return "", "refusal", true
		}
		return "", "", false
	}
	h.mu.Unlock()
	token, run := pbPiloted(t, s, nil)
	if res, _ := pbExec(t, s, "memory.people.backfill", map[string]any{"mode": "run", "model": "haiku", "confirm": token}); !res.OK {
		t.Fatal(res.Error)
	}
	done := pbWait(t, s, "run", pbState(store.BackfillDone))
	ws, _ := store.BackfillWindows(s.Store.Reader(), run.ID, false)
	if w := ws[0]; w.State != store.WindowRefused || w.Claims != 0 {
		t.Fatalf("a window Sonnet refused, run on Haiku: %+v", w)
	}
	for _, id := range ws[0].Obs {
		if n := pbCountClaims(t, s, `source=? AND evidence LIKE ?`, "backfill:"+done.ID, "%"+id+"%"); n != 0 {
			t.Fatalf("a claim from the refused window")
		}
	}
}

// pbBodies is the text of every request the host received: Messages bodies and batch create bodies.
func (h *pbFakeHost) bodies() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var all []string
	for _, c := range h.chats {
		pbStrings(c, "", &all)
	}
	for _, c := range h.creates {
		pbStrings(c, "", &all)
	}
	return strings.Join(all, "\n")
}

// No line of a withdrawn, blocked or tombstoned author, and no line with a deletion pending, leaves the
// process: such lines are left out when a prompt is built, and a line tombstoned between the build and the
// send (in the pilot and in a batch) is caught by the check in the intent transaction, so the window is
// built again without it. Live lines of the same windows are the control.
func TestPeopleBackfillDoesNotSendTombstonedText(t *testing.T) {
	h := newPBHost(t)
	s := pbService(t, h, "anthropic")
	pbSeed(t, s, 70)
	e, err := s.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 9, 1, 1, 0, 0, 0, time.UTC)
	for i := range 70 {
		at := base.Add(time.Duration(i)*3*time.Hour + 140*time.Second)
		for _, m := range []engine.LiveMessage{
			{MessageID: fmt.Sprintf("%d6", 1000+i), AuthorID: "103", AuthorName: "Sora", Content: fmt.Sprintf("withdrawnsentinel%d is my hobby", i), At: at},
			{MessageID: fmt.Sprintf("%d7", 1000+i), AuthorID: "104", AuthorName: "Hana", Content: fmt.Sprintf("blockedsentinel%d is my hobby", i), At: at.Add(time.Second)},
		} {
			m.GuildID, m.ChannelID = "g1", "c1"
			if ok, err := e.Capture(m); err != nil || !ok {
				t.Fatal(ok, err)
			}
		}
	}
	db := s.Store.DB
	stamp := "2026-11-01T00:00:00.000000000Z"
	// before the pilot: Sora withdraws memory, Hana's account is tombstoned, one of Joon's lines has a
	// deletion pending (its tombstone is committed, the stored row not yet redacted)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO consent_state(user_id,scope,status,updated_at) VALUES('103','memory','withdrawn',?)`, []any{stamp}},
		{`INSERT INTO privacy_tombstones(kind,source_id,at) VALUES('account','104',?)`, []any{stamp}},
		{`INSERT INTO privacy_tombstones(kind,source_id,at) VALUES('message','10032',?)`, []any{stamp}},
	} {
		if _, err := db.Exec(q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	// between the build and the send: the first window built in each phase loses Mina's line
	var mu sync.Mutex
	phase := "pilot"
	hit := map[string]bool{}
	var gone, kept []string
	pbBuilt = func(windowID string) {
		mu.Lock()
		defer mu.Unlock()
		if hit[phase] {
			return
		}
		hit[phase] = true
		runs, err := store.BackfillRuns(s.Store.Reader(), "g1")
		if err != nil || len(runs) == 0 {
			t.Error("no run", err)
			return
		}
		w, _, err := store.LoadBackfillWindow(s.Store.Reader(), runs[0].ID, windowID)
		if err != nil {
			t.Error(err)
			return
		}
		raw, _ := json.Marshal(w.Obs)
		var native, mina, joon string
		if err := s.Store.Reader().QueryRow(`SELECT native_message_id,content FROM memory_observations WHERE observation_id IN (SELECT value FROM json_each(?)) AND actor_account='101' ORDER BY parsed_time LIMIT 1`,
			string(raw)).Scan(&native, &mina); err != nil {
			t.Error(err)
			return
		}
		if err := s.Store.Reader().QueryRow(`SELECT content FROM memory_observations WHERE observation_id IN (SELECT value FROM json_each(?)) AND actor_account='102' ORDER BY parsed_time LIMIT 1`,
			string(raw)).Scan(&joon); err != nil {
			t.Error(err)
			return
		}
		if _, err := db.Exec(`INSERT INTO privacy_tombstones(kind,source_id,at) VALUES('message',?,?)`, native, stamp); err != nil {
			t.Error(err)
		}
		gone, kept = append(gone, mina), append(kept, joon)
	}
	t.Cleanup(func() { pbBuilt = nil })

	token, _ := pbPiloted(t, s, nil)
	mu.Lock()
	phase = "run"
	mu.Unlock()
	if res, _ := pbExec(t, s, "memory.people.backfill", map[string]any{"mode": "run", "model": "haiku", "confirm": token}); !res.OK {
		t.Fatal(res.Error)
	}
	pbWait(t, s, "run", pbState(store.BackfillDone))
	mu.Lock()
	defer mu.Unlock()
	if len(gone) != 2 {
		t.Fatalf("the hook ran %d times, want once per phase", len(gone))
	}
	sent := h.bodies()
	for _, sentinel := range append([]string{"withdrawnsentinel", "blockedsentinel", "topic3b as well"}, gone...) {
		if strings.Contains(sent, sentinel) {
			t.Fatalf("%q left the process", sentinel)
		}
	}
	for _, control := range append([]string{"topic3a today", "topic40b as well"}, kept...) {
		if !strings.Contains(sent, control) {
			t.Fatalf("control line %q was not sent", control)
		}
	}
	if len(h.creations()) == 0 || len(h.chats) == 0 {
		t.Fatal("neither the pilot nor a batch was sent")
	}
}

// pbSegment captures one more segment at seed slot i (three hours after slot i-1): each line one minute
// after the last, as {author id, name, text}.
func pbSegment(t *testing.T, s *Service, i int, lines ...[3]string) []string {
	t.Helper()
	e, err := s.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 1, 1, 0, 0, 0, time.UTC).Add(time.Duration(i) * 3 * time.Hour)
	var ids []string
	for k, l := range lines {
		m := engine.LiveMessage{GuildID: "g1", ChannelID: "c1", MessageID: fmt.Sprintf("9%03d%02d", i, k), AuthorID: l[0], AuthorName: l[1], Content: l[2],
			At: at.Add(time.Duration(k) * time.Minute)}
		if ok, err := e.Capture(m); err != nil || !ok {
			t.Fatal(ok, err)
		}
		ids = append(ids, corpus.DiscordLiveObservationID(engine.RoomKey("g1", "c1"), m.MessageID))
	}
	return ids
}

// requestTexts are the prompts the host received, one per Messages call or batch item.
func (h *pbFakeHost) requestTexts() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	add := func(v any) {
		var all []string
		pbStrings(v, "", &all)
		out = append(out, strings.Join(all, "\n"))
	}
	for _, c := range h.chats {
		add(c)
	}
	for _, c := range h.creates {
		for _, it := range c["requests"].([]any) {
			add(it.(map[string]any)["params"])
		}
	}
	return out
}

// pbLineOf finds the prompt line holding text: its label and its speaker's token.
func pbLineOf(prompt, text string) (label, token string) {
	for _, m := range pbLineRE.FindAllStringSubmatch(prompt, -1) {
		if strings.Contains(m[3], text) {
			return m[1], m[2]
		}
	}
	return "", ""
}

// The prompt carries the merged lines the plan counted: a burst of one speaker is one line (each message
// clipped on its own), and two short messages that reach the meaningful-rune floor together are sent, not
// skipped. A claim citing the merged line has every observation of it as evidence.
func TestPeopleBackfillSendsPlannedMergedLines(t *testing.T) {
	h := newPBHost(t)
	s := pbService(t, h, "anthropic")
	pbSeed(t, s, 70)
	burst := pbSegment(t, s, 70,
		[3]string{"101", "Mina", "I really like hiking70 so much"}, [3]string{"101", "Mina", "burstword one here"},
		[3]string{"101", "Mina", "burstword two here"}, [3]string{"101", "Mina", "burstword three here"},
		[3]string{"101", "Mina", "burstword four here"}, [3]string{"102", "Joon", "I really like topic70b as well"},
		[3]string{"101", "Mina", "and that is all from me"})
	short := pbSegment(t, s, 71, [3]string{"102", "Joon", "gnu"}, [3]string{"102", "Joon", "yak"})
	dry := pbDry(t, s, nil)
	if dry["windows"].(float64) != 72 || dry["skipped_windows"].(float64) != 0 {
		t.Fatalf("plan: %v windows, %v skipped", dry["windows"], dry["skipped_windows"])
	}
	token, run := pbPiloted(t, s, nil)
	if res, _ := pbExec(t, s, "memory.people.backfill", map[string]any{"mode": "run", "model": "haiku", "confirm": token}); !res.OK {
		t.Fatal(res.Error)
	}
	pbWait(t, s, "run", pbState(store.BackfillDone))
	sawBurst, sawShort := false, false
	for _, text := range h.requestTexts() {
		if strings.Contains(text, "burstword") {
			sawBurst = true
			if n := len(regexp.MustCompile(`(?m)^\[m\d+\] `).FindAllString(text, -1)); n != 3 {
				t.Fatalf("a burst, an answer and one more line are %d prompt lines, want 3:\n%s", n, text)
			}
		}
		sawShort = sawShort || strings.Contains(text, "gnu yak")
	}
	if !sawBurst || !sawShort {
		t.Fatalf("burst sent %v, short pair sent %v", sawBurst, sawShort)
	}
	ws, _ := store.BackfillWindows(s.Store.Reader(), run.ID, false)
	for _, w := range ws {
		if slices.Contains(w.Obs, short[0]) && w.State != store.WindowApplied {
			t.Fatalf("the short pair's window: %s", w.State)
		}
	}
	var ev string
	if err := s.Store.Reader().QueryRow(`SELECT evidence FROM person_claim WHERE topic='hiking70'`).Scan(&ev); err != nil {
		t.Fatal(err)
	}
	var got []string
	_ = json.Unmarshal([]byte(ev), &got)
	slices.Sort(got)
	want := slices.Clone(burst[:5])
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("evidence of the merged line: %v, want %v", got, want)
	}
}

// A fact cited from two people's lines is not attributable: Mina only asked, Joon said it. It does not
// become Mina's self claim with her question as provenance, nor is it split into a claim of each.
func TestPeopleBackfillMixedAuthorsDoNotBecomeSelf(t *testing.T) {
	h := newPBHost(t)
	s := pbService(t, h, "anthropic")
	pbSeed(t, s, 70)
	pbSegment(t, s, 70, [3]string{"101", "Mina", "What?"}, [3]string{"102", "Joon", "Mina moved to sentinelcity last week"})
	h.mu.Lock()
	h.rewrite = func(model, prompt string) (string, string, bool) {
		if !strings.Contains(prompt, "sentinelcity") {
			return "", "", false
		}
		q, mina := pbLineOf(prompt, "What?")
		a, joon := pbLineOf(prompt, "sentinelcity")
		raw, _ := json.Marshal(map[string]any{"people": []any{map[string]any{"about": mina, "kind": "status", "topic": "home",
			"text": "{" + mina + "} moved to sentinelcity", "by": []string{mina, joon}, "ev": []string{q, a}}}})
		return string(raw), "end_turn", true
	}
	h.mu.Unlock()
	token, _ := pbPiloted(t, s, nil)
	if res, _ := pbExec(t, s, "memory.people.backfill", map[string]any{"mode": "run", "model": "haiku", "confirm": token}); !res.OK {
		t.Fatal(res.Error)
	}
	pbWait(t, s, "run", pbState(store.BackfillDone))
	if n := pbCountClaims(t, s, `text LIKE '%sentinelcity%' AND subject_ref=speaker_ref`); n != 0 {
		t.Fatal("a fact Joon said became Mina's self claim")
	}
	if n := pbCountClaims(t, s, `text LIKE '%sentinelcity%'`); n != 0 {
		t.Fatalf("a mixed-author fact was split into %d claims", n)
	}
}

// The link comes from the lines a fact cites: Mina names Joon without a mention, and Joon (an author of
// the window) only greets. The claim is a name link (reviewed in the pilot), not a mention.
func TestPeopleBackfillNameLinkFromCitedLines(t *testing.T) {
	h := newPBHost(t)
	s := pbService(t, h, "anthropic")
	pbSeed(t, s, 70)
	pbSegment(t, s, 70, [3]string{"101", "Mina", "Joon is into namehike these days"}, [3]string{"102", "Joon", "hello there everyone"})
	h.mu.Lock()
	h.rewrite = func(model, prompt string) (string, string, bool) {
		if !strings.Contains(prompt, "namehike") {
			return "", "", false
		}
		ev, mina := pbLineOf(prompt, "namehike")
		_, joon := pbLineOf(prompt, "hello there")
		raw, _ := json.Marshal(map[string]any{"people": []any{map[string]any{"about": joon, "kind": "like", "topic": "namehike",
			"text": "{" + joon + "} likes namehike", "by": []string{mina}, "ev": []string{ev}}}})
		return string(raw), "end_turn", true
	}
	h.mu.Unlock()
	token, _ := pbPiloted(t, s, nil)
	if res, _ := pbExec(t, s, "memory.people.backfill", map[string]any{"mode": "run", "model": "haiku", "confirm": token}); !res.OK {
		t.Fatal(res.Error)
	}
	pbWait(t, s, "run", pbState(store.BackfillDone))
	var link string
	if err := s.Store.Reader().QueryRow(`SELECT link FROM person_claim WHERE topic='namehike'`).Scan(&link); err != nil || link != store.LinkName {
		t.Fatalf("link %q (%v), want name", link, err)
	}
}
