package application_test

import (
	"context"
	"testing"
	"time"

	"masamong/internal/application"
	"masamong/internal/config"
)

// A long-running instance keeps turn rows for stats but drops old details, traces, per-user counters
// and question embeddings; leaf embeddings (which save paid re-embedding) and memory are never pruned.
func TestRetentionPrunesOldOperationalRecords(t *testing.T) {
	svc, _ := slowChatSvc(t, 0, nil)
	// one real turn creates the admin tables, a turn row, a trace, counters and a query embedding
	if _, err := svc.ChatTurn(context.Background(), chatIn("c", "hello")); err != nil {
		t.Fatal(err)
	}
	db := svc.Store.DB
	old := time.Now().UTC().AddDate(0, 0, -45).Format(time.RFC3339)
	ancient := time.Now().UTC().AddDate(-2, 0, 0).Format(time.RFC3339)
	oldDay := time.Now().AddDate(0, 0, -45).Format("2006-01-02")
	must := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	must(`INSERT INTO turn_log(id, at, day, hour, guild_id, channel_id, outcome, tools, hits, cost_usd, latency_ms, lab, body)
		VALUES('old', ?, ?, 0, 'g', 'c', 'answered', '', 0, 0, 0, 0, '{"answer":"secret detail"}')`, old, oldDay)
	must(`INSERT INTO turn_log(id, at, day, hour, guild_id, channel_id, outcome, tools, hits, cost_usd, latency_ms, lab, body)
		VALUES('ancient', ?, '2000-01-01', 0, 'g', 'c', 'answered', '', 0, 0, 0, 0, '{}')`, ancient)
	must(`INSERT INTO traces(id, body, created_at) VALUES('tr_old', '{}', ?)`, old)
	must(`INSERT INTO audit_log(at, action, via, ok) VALUES(?, 'config.apply', 'cli', 1)`, ancient)
	must(`INSERT INTO tool_usage(day, tool, scope, subject, n, last_at) VALUES(?, 'chat.respond', 'user', 'h1', 3, ?)`, oldDay, old)
	must(`INSERT INTO tool_usage(day, tool, scope, subject, n, last_at) VALUES(?, 'chat.respond', 'all', '', 3, ?)`, oldDay, old)
	must(`INSERT INTO memory_embed_cache(cache_key, vec, created_at, kind) VALUES('q_old', x'00', ?, 'query')`, old)
	must(`INSERT INTO memory_embed_cache(cache_key, vec, created_at, kind) VALUES('leaf_old', x'00', ?, '')`, ancient)
	must(`INSERT INTO chat_turns(channel_key, role, speaker, content, created_at) VALUES('discord:g:quiet', 'user', '철수', 'old context', ?)`, old)

	res := exec(svc, "schedules.run", map[string]any{"id": "usage_gc"})
	if !res.OK {
		t.Fatalf("%+v", res.Error)
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := db.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	checks := []struct {
		what string
		got  int
		want int
	}{
		{"turn older than turn_days", count(`SELECT COUNT(*) FROM turn_log WHERE id='ancient'`), 0},
		{"turn within turn_days kept", count(`SELECT COUNT(*) FROM turn_log WHERE id='old'`), 1},
		{"old turn detail stripped", count(`SELECT COUNT(*) FROM turn_log WHERE id='old' AND body='{}'`), 1},
		{"recent turn detail kept", count(`SELECT COUNT(*) FROM turn_log WHERE id<>'old' AND body<>'{}'`), 1},
		{"old trace", count(`SELECT COUNT(*) FROM traces WHERE id='tr_old'`), 0},
		{"recent trace", count(`SELECT COUNT(*) FROM traces`), 1},
		{"old audit", count(`SELECT COUNT(*) FROM audit_log WHERE at=?`, ancient), 0},
		{"old per-user counter", count(`SELECT COUNT(*) FROM tool_usage WHERE day=? AND scope='user'`, oldDay), 0},
		{"old daily total kept for stats", count(`SELECT COUNT(*) FROM tool_usage WHERE day=? AND scope='all'`, oldDay), 1},
		{"old question embedding", count(`SELECT COUNT(*) FROM memory_embed_cache WHERE cache_key='q_old'`), 0},
		{"leaf embedding kept", count(`SELECT COUNT(*) FROM memory_embed_cache WHERE cache_key='leaf_old'`), 1},
		{"old chat context of a quiet channel", count(`SELECT COUNT(*) FROM chat_turns WHERE channel_key='discord:g:quiet'`), 0},
		{"recent chat context kept", count(`SELECT COUNT(*) FROM chat_turns WHERE channel_key<>'discord:g:quiet'`), 2},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: got %d, want %d", c.what, c.got, c.want)
		}
	}
	list := exec(svc, "schedules.list", nil)
	for _, it := range list.Output["schedules"].([]map[string]any) {
		if it["id"] == "usage_gc" && it["next_run"] == nil {
			t.Fatalf("usage_gc has no next run after running: %+v", it)
		}
	}

	// -1 keeps a kind forever
	applyDoc(t, svc, func(d *config.Document) { d.Retention.TraceDays = -1 })
	must(`INSERT INTO traces(id, body, created_at) VALUES('tr_keep', '{}', ?)`, ancient)
	if r := exec(svc, "schedules.run", map[string]any{"id": "usage_gc"}); !r.OK {
		t.Fatal(r.Error)
	}
	if count(`SELECT COUNT(*) FROM traces WHERE id='tr_keep'`) != 1 {
		t.Fatal("trace_days: -1 must keep traces")
	}
}

func TestRetentionPrunesOrphanThreadParticipants(t *testing.T) {
	svc := setupSvc(t)
	if _, err := svc.Store.DB.Exec(`INSERT INTO thread_participants(thread_id,user_id,added_at) VALUES(?,?,?)`,
		"missing-thread", "participant", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal("could not seed an orphan participant")
	}
	if r := exec(svc, "schedules.run", map[string]any{"id": "usage_gc"}); !r.OK {
		t.Fatal("retention failed")
	}
	var remaining int
	if err := svc.Store.DB.QueryRow(`SELECT COUNT(*) FROM thread_participants WHERE thread_id=?`, "missing-thread").Scan(&remaining); err != nil {
		t.Fatal("could not inspect orphan participant")
	}
	if remaining != 0 {
		t.Fatal("retention did not remove the orphan participant row")
	}
}

func chatIn(channel, msg string) application.ChatInput {
	return application.ChatInput{GuildID: "g", ChannelID: channel, Message: msg, Speaker: "u", SpeakerID: "u"}
}
