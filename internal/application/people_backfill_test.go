package application

import (
	"encoding/json"
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
	"unicode/utf8"

	"masamong/internal/action"
	"masamong/internal/config"
	"masamong/internal/errcode"
	"masamong/internal/memory/corpus"
	"masamong/internal/memory/engine"
	"masamong/internal/memory/peoplebackfill"
	"masamong/internal/memory/store"
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
	fullUsage atomic.Bool // report the reserved maximum: every prompt rune + 64 in, 1400 out
	refuse    map[string]bool
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
	runes := 0
	for _, s := range all {
		runes += utf8.RuneCountInString(s)
	}
	in, out := 400, 60
	if h.fullUsage.Load() {
		in, out = runes+64, pbMaxTokens
	}
	text, stop := pbFakeAnswer(model, strings.Join(all, "\n")), "end_turn"
	if refuse {
		text, stop = "", "refusal"
	}
	return map[string]any{"id": "msg_fake", "type": "message", "role": "assistant", "model": model,
		"content": []any{map[string]any{"type": "text", "text": text}}, "stop_reason": stop,
		"usage": map[string]any{"input_tokens": in, "output_tokens": out}}
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
// disagreement between the two).
func pbFakeAnswer(model, prompt string) string {
	lines := pbLineRE.FindAllStringSubmatch(prompt, -1)
	var tokens []string
	for _, m := range lines {
		if !slices.Contains(tokens, m[2]) {
			tokens = append(tokens, m[2])
		}
	}
	people := []map[string]any{}
	for _, m := range lines {
		label, tok, text := m[1], m[2], m[3]
		if t, ok := strings.CutPrefix(text, "I really like "); ok {
			topic := strings.Fields(t)[0]
			people = append(people, map[string]any{"about": tok, "kind": "like", "topic": topic, "text": "{" + tok + "} likes " + topic,
				"by": []string{tok}, "ev": []string{label}})
		}
		if t, ok := strings.CutPrefix(text, "my friend drinks "); ok {
			topic := strings.Fields(t)[0]
			about := tok
			if strings.Contains(model, "haiku") {
				for _, o := range tokens {
					if o != tok {
						about = o
						break
					}
				}
			}
			people = append(people, map[string]any{"about": about, "kind": "trait", "topic": topic, "text": "{" + about + "} drinks " + topic,
				"by": []string{tok}, "ev": []string{label}})
		}
		if t, ok := strings.CutPrefix(text, "my buddy eats "); ok {
			topic := strings.Fields(t)[0]
			for _, o := range tokens {
				if o != tok {
					people = append(people, map[string]any{"about": o, "kind": "like", "topic": topic, "text": "{" + o + "} eats " + topic,
						"by": []string{tok}, "ev": []string{label}})
					break
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
	pbWindowLines = 3 // one synthetic segment per window
	t.Cleanup(func() { pbPollEvery.Store(0); pbWindowLines = 0 })
	return s
}

// pbSeed captures segments three hours apart: Mina and Joon each say what they like; every fifth has
// a hearsay line the two models read differently, every sixth one they agree on, every seventh a bot line.
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
				Content: fmt.Sprintf("my friend drinks tea%d daily", i), At: at.Add(2 * time.Minute)})
		}
		if i%6 == 0 {
			msgs = append(msgs, engine.LiveMessage{MessageID: fmt.Sprintf("%d5", 1000+i), AuthorID: "101", AuthorName: "Mina",
				Content: fmt.Sprintf("my buddy eats snack%d often", i), At: at.Add(150 * time.Second)})
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
	if h.count() != 0 {
		t.Fatalf("host called %d times", h.count())
	}
}

func TestPeopleBackfillPilotAndRun(t *testing.T) {
	h := newPBHost(t)
	s := pbService(t, h, "anthropic")
	pbSeed(t, s, 70)
	token, run := pbPiloted(t, s, nil)
	// both models, standard API, every pilot window once each; no temperature, top-level cache_control
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
		cc, _ := c["cache_control"].(map[string]any)
		if cc["type"] != "ephemeral" {
			t.Fatalf("no ephemeral cache_control: %v", c)
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
		if cc, _ := p["cache_control"].(map[string]any); cc["type"] != "ephemeral" {
			t.Fatalf("batch item without cache_control: %v", p)
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
		out = append(out, pbMaxCost(bp, w.Lines, peoplebackfill.ModelHaiku, peoplebackfill.ModeBatch))
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
	deadline := time.Now().Add(10 * time.Second)
	for len(h.creations()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
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
	deadline := time.Now().Add(10 * time.Second)
	for len(h.creations()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
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
	// the operator continues it: only now is the rest sent
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
	deadline := time.Now().Add(10 * time.Second)
	for len(h.creations()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
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
	// the windows whose lines changed went back to the queue and out again
	if creates := h.creations(); len(creates) != 2 || len(creates[1]["requests"].([]any)) != 2 {
		t.Fatalf("changed windows are sent again: %d batches", len(creates))
	}
	w3, _, _ := store.LoadBackfillWindow(s.Store.Reader(), run.ID, ws[3].ID)
	w5, _, _ := store.LoadBackfillWindow(s.Store.Reader(), run.ID, ws[5].ID)
	if w3.Attempts != 2 || w3.State != store.WindowApplied || w5.Attempts != 1 || w5.Rejected == 0 {
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
