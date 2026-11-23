package application_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/application"
	"masamong/internal/config"
	"masamong/internal/storage"
)

// personHistoryRows reads what cost money or holds the words, plus the episodes person memory is derived
// from: every column of every row, in order.
func personHistoryRows(t *testing.T, db *storage.Store) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, table := range []string{"memory_leaves", "memory_observations", "memory_message_vectors", "memory_embed_cache", "channel_episode", "episode_item", "episode_leaf"} {
		rows, err := db.DB.Query(`SELECT * FROM ` + table)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals, ptrs := make([]any, len(cols)), make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			out[table] = append(out[table], fmt.Sprint(vals))
		}
		rows.Close()
		slices.Sort(out[table])
	}
	return out
}

// Contract person-memory-v170 §2.7: a store indexed by the release before (no person claims, no
// derivation cursor) restarted with this one and run through the room-memory pass and the memory pass
// makes no chat and no embedding call, leaves every leaf, vector and episode row as it was, and derives
// the person claims from the stored items.
func TestPersonMemoryRestartReprocessesNothing(t *testing.T) {
	t.Parallel()
	var chats, embeds atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/embeddings":
			embeds.Add(1)
			var req struct {
				Input []string `json:"input"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			var data []map[string]any
			for i := range req.Input {
				v := make([]float32, 8)
				v[i%8] = 1
				data = append(data, map[string]any{"index": i, "embedding": v})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "usage": map[string]any{"cost": 0.000001}})
		case "/chat/completions":
			chats.Add(1)
			var req struct {
				Messages []struct{ Role, Content string } `json:"messages"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			prompt := req.Messages[len(req.Messages)-1].Content
			answer := "응"
			switch {
			case strings.Contains(prompt, `"threads"`):
				answer = `{"threads":[]}`
			case strings.Contains(prompt, `"episodes"`):
				answer = climbFact
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": answer}, "finish_reason": "stop"}},
				"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "cost": 0.00001},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	svc := embedSvc(t, t.TempDir(), srv.URL)
	svc.SetDiscord(&personGateway{})
	serveAnyServer(t, svc)
	applyDoc(t, svc, func(d *config.Document) {
		d.Models["summary"] = config.Model{Provider: "openrouter", Model: "fake/chat"}
		d.Models["response"] = config.Model{Provider: "openrouter", Model: "fake/chat"}
		d.Guilds = map[string]config.GuildSettings{"g": {Channels: map[string]config.ChannelPolicy{"c": {Respond: "mention", Capture: true}}}}
	})
	eng, err := svc.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var lines []said
	for i := range 10 {
		user, name := "u1", "철수"
		if i%2 == 1 {
			user, name = "u2", "영희"
		}
		lines = append(lines, said{fmt.Sprintf("70%02d", i), user, name, fmt.Sprintf("클라이밍 얘기 %d", i)})
	}
	sayIn(eng, time.Now().Add(-3*time.Hour), lines...)
	svc.MemoryPassForTest(ctx)
	svc.RunRoomMemory(ctx)
	svc.MemoryPassForTest(ctx)
	var leavesN, vectors, items, claims int
	if err := svc.Store.DB.QueryRow(`SELECT (SELECT COUNT(*) FROM memory_leaves),(SELECT COUNT(*) FROM memory_message_vectors),
		(SELECT COUNT(*) FROM episode_item),(SELECT COUNT(*) FROM person_claim)`).Scan(&leavesN, &vectors, &items, &claims); err != nil {
		t.Fatal(err)
	}
	if leavesN == 0 || vectors == 0 || items == 0 || claims == 0 {
		t.Fatalf("nothing stored to begin with: leaves %d, vectors %d, items %d, claims %d", leavesN, vectors, items, claims)
	}
	// as the release before left it: no person claim, no derivation cursor, no relation fill
	for _, q := range []string{
		`DELETE FROM evidence_refs WHERE holder_kind='person_claim'`,
		`DELETE FROM person_claim`,
		`DELETE FROM person_memory_suppress`,
		`DELETE FROM admin_state WHERE k LIKE 'people:%'`,
	} {
		if _, err := svc.Store.DB.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	before := personHistoryRows(t, svc.Store)
	chats0, embeds0 := chats.Load(), embeds.Load()
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}

	next, err := application.ReopenForTest(svc, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = next.Close() })
	next.SetDiscord(&personGateway{})
	check := func(step string) {
		t.Helper()
		after := personHistoryRows(t, next.Store)
		for table, rows := range before {
			if !slices.Equal(rows, after[table]) {
				t.Errorf("%s changed %s: %d rows then, %d now", step, table, len(rows), len(after[table]))
			}
		}
		if chats.Load() != chats0 || embeds.Load() != embeds0 {
			t.Errorf("%s called a model: chats %d→%d, embeds %d→%d", step, chats0, chats.Load(), embeds0, embeds.Load())
		}
	}
	check("opening the service")
	next.RunRoomMemory(ctx)
	check("the room-memory pass")
	next.MemoryPassForTest(ctx)
	check("the memory pass")
	next.RunRoomMemory(ctx)
	check("a second room-memory pass")
	if err := next.Store.DB.QueryRow(`SELECT COUNT(*) FROM person_claim`).Scan(&claims); err != nil || claims == 0 {
		t.Fatalf("the stored items gave no person claims: %d (%v)", claims, err)
	}
}
