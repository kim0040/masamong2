package application

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/errclass"
	"masamong/internal/tools"
)

func TestCallLedgersKeepNinetyDays(t *testing.T) {
	s, clk := deliveryService(t)
	db, err := s.adminDB()
	if err != nil {
		t.Fatal(err)
	}
	for _, age := range []time.Duration{91 * 24 * time.Hour, 89 * 24 * time.Hour} {
		at := clk.t.Add(-age).Format(time.RFC3339)
		if _, err := db.DB.Exec(`INSERT INTO llm_calls(turn_id, role, created_at) VALUES('t', 'response', ?)`, at); err != nil {
			t.Fatal(err)
		}
		if _, err := db.DB.Exec(`INSERT INTO tool_calls(turn_id, tool, created_at) VALUES('t', 'weather.query', ?)`, at); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := s.runRetention(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if removed["llm_calls"] != 1 || removed["tool_calls"] != 1 {
		t.Fatalf("removed = %v", removed)
	}
	for _, table := range []string{"llm_calls", "tool_calls"} {
		var n int
		if err := db.DB.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil || n != 1 {
			t.Fatalf("%s kept %d rows (%v)", table, n, err)
		}
	}
}

// A model or tool call outside a turn or job still gets a row, with empty ids; a service without a
// store records nothing and does not fail.
func TestLedgerWriteWithoutTurn(t *testing.T) {
	s, _ := deliveryService(t)
	s.noteLLMCall(context.Background(), llmCall{Role: "summary", Model: "m", CostUSD: 0.5})
	db, _ := s.adminDB()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var n int
		_ = db.DB.QueryRow(`SELECT COUNT(*) FROM llm_calls WHERE turn_id='' AND role='summary'`).Scan(&n)
		if n == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the call was not recorded")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A turn the channel's logging level kept body-free is not rebuilt from the memory on read (D9).
func TestBodyFreeTurnIsNotResolvedOnRead(t *testing.T) {
	s, _ := deliveryService(t)
	if _, err := s.MemoryEngine(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.DB.Exec(`INSERT INTO memory_observations(observation_id, platform, collection, source_room, room, native_message_id, actor_label, content, parsed_time, record_kind, imported_at)
		VALUES('o1','discord','c','discord:g:c','discord:g:c','n1','Alice','private canary','2026-09-01T00:00:00Z','message','2026-09-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	body := func(level string) map[string]any {
		b := map[string]any{"message": "q", "answer": "a", "error": "provider says: private canary",
			"hits":    []any{map[string]any{"rank": 1, "doc_id": "leaf-x", "visible_observation_ids": []any{"o1"}, "observation_ids": []any{"o1"}, "excerpt": "private canary"}},
			"context": map[string]any{"observation_ids": []any{"o1"}, "attribution": map[string]any{"mode": "on"}}}
		return s.withLoggingLevel(level, "answered", errors1(), b)
	}
	get := func(id string) string {
		r := s.Execute(context.Background(), action.Request{Action: "turns.get", Input: []byte(`{"id":"` + id + `"}`)})
		if !r.OK {
			t.Fatalf("%+v", r.Error)
		}
		raw, _ := json.Marshal(r.Output)
		return string(raw)
	}
	s.recordTurn(TurnRecord{ID: "off", GuildID: "g", ChannelID: "c", Outcome: "answered", Lab: true, Body: body("off")})
	s.recordTurn(TurnRecord{ID: "ops", GuildID: "g", ChannelID: "c", Outcome: "answered", Lab: true, Body: body("ops")})
	if shown := get("off"); strings.Contains(shown, "private canary") || strings.Contains(shown, "Alice") || strings.Contains(shown, `"excerpt"`) || strings.Contains(shown, `"speakers"`) {
		t.Fatalf("a body-free turn was rebuilt on read: %s", shown)
	} else if !strings.Contains(shown, `"error":"not_found"`) || !strings.Contains(shown, `"attribution"`) {
		t.Fatalf("expected the error class and the attribution to stay: %s", shown)
	}
	if shown := get("ops"); !strings.Contains(shown, "private canary") {
		t.Fatalf("a whole turn must still resolve its evidence: %s", shown)
	}
}

func errors1() error { return errNotFound{} }

type errNotFound struct{}

func (errNotFound) Error() string            { return "provider says: private canary" }
func (errNotFound) ErrClass() errclass.Class { return errclass.NotFound }

func TestDeclaredParamKeys(t *testing.T) {
	decl := []tools.Input{{Name: "region"}, {Name: "day_offset"}}
	got := declaredParamKeys(decl, map[string]any{"region": "x", "day_offset": 1, "free text key": 2, "another": 3})
	if strings.Join(got, ",") != "day_offset,region,_other" {
		t.Fatalf("keys = %v", got)
	}
	if got := declaredParamKeys(nil, map[string]any{"a": 1}); strings.Join(got, ",") != "_other" {
		t.Fatalf("undeclared tool: %v", got)
	}
	if got := declaredParamKeys(decl, nil); len(got) != 0 {
		t.Fatalf("no params: %v", got)
	}
}

// The writer holds what is queued when the service closes, and a full queue drops rows and counts them.
func TestLedgerWriterFlushesOnCloseAndDropsWhenFull(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if r := s.Execute(context.Background(), action.Request{Action: "setup.resume", Input: []byte(`{"instance":"ledger"}`)}); !r.OK {
		t.Fatal(r.Error)
	}
	for i := 0; i < 5; i++ {
		s.noteLLMCall(context.Background(), llmCall{Role: "response", Model: "m"})
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	db, err := s2.adminDB()
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM llm_calls`).Scan(&n); err != nil || n != 5 {
		t.Fatalf("rows after close = %d (%v)", n, err)
	}

	full := &ledgerWriter{rows: make(chan ledgerRow)} // nobody reads: every add finds it full
	full.once.Do(func() {})
	for i := 0; i < 3; i++ {
		full.add(s2, `INSERT INTO llm_calls(role, created_at) VALUES('x','y')`)
	}
	if full.dropped.Load() != 3 {
		t.Fatalf("dropped = %d", full.dropped.Load())
	}
}
