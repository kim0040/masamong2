package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/memory/leaves"
	"masamong/internal/memory/search"
	"masamong/internal/memory/store"
	"masamong/internal/providers"
	"masamong/internal/providers/openai"
	"masamong/internal/storage"
)

// An operator's history cost real money to summarize and embed, and a release must never spend it again on its own.
// The recipe's label is stored with every leaf: changing it leaves the whole history waiting to be cut and embedded
// again. A change here needs the operator's decision and an entry in the release notes, not a new expected value.
func TestTheDefaultRecipeKeepsItsLabel(t *testing.T) {
	if got, want := leaves.DefaultRecipe().Version(), "leaf-v2:time_gap:8:gap30:min2"; got != want {
		t.Fatalf("the recipe label moved from %q to %q: every stored leaf would be embedded again", want, got)
	}
}

// Starting on a store that is already indexed (what an upgrade does) pays for nothing and rewrites nothing.
func TestARestartOnAnIndexedStoreReprocessesNothing(t *testing.T) {
	e, fp := newEngine(t, true)
	seed(t, e)
	ctx := context.Background()
	if _, err := e.IndexRooms(ctx, IndexOptions{BudgetUSD: 1}, nil); err != nil {
		t.Fatal(err)
	}
	embeds, chats := fp.embedCalls.Load(), fp.chatCalls.Load()
	leafRows := count(t, e, `SELECT COUNT(*) FROM memory_leaves`)
	if embeds == 0 || leafRows == 0 {
		t.Fatalf("nothing was indexed to begin with: embeds=%d leaves=%d", embeds, leafRows)
	}
	n := successor(e, e.Models)
	if _, err := n.Reload(); err != nil {
		t.Fatal(err)
	}
	rep, err := n.IndexRooms(ctx, IndexOptions{BudgetUSD: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Pending != 0 || fp.embedCalls.Load() != embeds || fp.chatCalls.Load() != chats {
		t.Fatalf("a restart spent again: %+v embeds %d→%d chats %d→%d", rep, embeds, fp.embedCalls.Load(), chats, fp.chatCalls.Load())
	}
	if got := count(t, n, `SELECT COUNT(*) FROM memory_leaves`); got != leafRows {
		t.Fatalf("a restart changed the leaves: %d→%d", leafRows, got)
	}
}

// tableRows reads a table's rows in order, every column, vectors included.
func tableRows(t *testing.T, db *storage.Store, table, where string) []string {
	t.Helper()
	rows, err := db.DB.Query(`SELECT * FROM ` + table + ` ` + where)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		var line strings.Builder
		for i, v := range vals {
			fmt.Fprintf(&line, "%s=%v|", cols[i], v)
		}
		out = append(out, line.String())
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}

// storedHistory is what cost money, or is the words it was made from.
var storedHistory = map[string]string{
	"memory_leaves":          "",
	"memory_observations":    "",
	"memory_message_vectors": "",
	"memory_embed_cache":     "WHERE kind<>''", // an untyped row goes once (migrateSummaryCache): no pass asks for it again
	"channel_episode":        "",
	"episode_item":           "",
	"channel_state":          "",
}

func snapshotHistory(t *testing.T, db *storage.Store) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for table, where := range storedHistory {
		var n int
		if err := db.DB.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			out[table] = tableRows(t, db, table, where)
		}
	}
	return out
}

func wantSameHistory(t *testing.T, step string, before, after map[string][]string) {
	t.Helper()
	for table, rows := range before {
		if !slices.Equal(rows, after[table]) {
			changed := 0
			for i := range min(len(rows), len(after[table])) {
				if rows[i] != after[table][i] {
					changed++
				}
			}
			t.Errorf("%s changed %s: %d rows then, %d now, %d differ", step, table, len(rows), len(after[table]), changed)
		}
	}
}

// What an upgrade does: the file of an older release, with none of the markers of the steps that run once, is opened by
// this binary, and the background pass comes round. Nothing it holds is cut, cleared or queued, no model is asked
// anything. A failure here is the money the operator already spent, spent again: stop and ask, do not change it.
func TestOpeningAnOlderReleasesStoreReprocessesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	open := func() (*storage.Store, *Engine, *fakeProvider) {
		t.Helper()
		db, err := storage.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		st, err := store.Open(db) // the migrations run here, as at every start
		if err != nil {
			db.Close()
			t.Fatal(err)
		}
		fp := &fakeProvider{}
		t.Setenv("FAKE_KEY", "k")
		srv := httptest.NewServer(fp.handler())
		t.Cleanup(srv.Close)
		cred, _ := providers.Resolve("env:FAKE_KEY")
		a := openai.New(providers.NewClient(5*time.Second, nil), srv.URL, cred)
		return db, New(st, a, Models{Embed: "fake-embed", Rerank: "fake-rerank", Summary: "fake-summary"},
			Settings{Search: search.Default(), Recipe: leaves.DefaultRecipe(), Summaries: true, MessageVectors: true, RefTZ: time.UTC, HedgeAfter: time.Second}), fp
	}
	db, e, fp := open()
	seed(t, e)
	ctx := context.Background()
	if _, err := e.IndexRooms(ctx, IndexOptions{BudgetUSD: 1}, nil); err != nil {
		t.Fatal(err)
	}
	if fp.embedCalls.Load() == 0 || fp.chatCalls.Load() == 0 {
		t.Fatalf("nothing was indexed to begin with: embeds=%d chats=%d", fp.embedCalls.Load(), fp.chatCalls.Load())
	}
	// as an older release left it: summaries that are the body's opening (postmortem 0005) with their vectors, vectors
	// made before the label was kept, no marker of any step that runs once, no checkpoint, every room waiting to be cut
	for _, q := range []string{
		`UPDATE memory_leaves SET summary=substr(body,1,180) WHERE summary<>''`,
		`UPDATE memory_message_vectors SET label=''`,
		`DELETE FROM memory_privacy_migration`,
		`DELETE FROM memory_message_vector_migration`,
		`DELETE FROM memory_identity_migration`,
		`DELETE FROM memory_leaf_sync`,
		`UPDATE memory_rooms SET pending_sync=1`,
		`DROP TABLE message_kept_revisions`,
		`DROP TRIGGER privacy_settled_insert`,
		`DROP TRIGGER privacy_settled_update`,
		`DROP TABLE privacy_settled`,
	} {
		if _, err := db.DB.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	var fallback, vectors int
	if err := db.DB.QueryRow(`SELECT COUNT(*), COUNT(summary_vec) FROM memory_leaves WHERE summary<>'' AND summary=substr(body,1,180)`).Scan(&fallback, &vectors); err != nil {
		t.Fatal(err)
	}
	if fallback == 0 || vectors != fallback {
		t.Fatalf("the store is not as an older release left it: %d summaries, %d vectors", fallback, vectors)
	}
	before := snapshotHistory(t, db)
	if len(before["memory_leaves"]) == 0 || len(before["memory_message_vectors"]) == 0 {
		t.Fatalf("nothing stored to begin with: %d leaves, %d message vectors", len(before["memory_leaves"]), len(before["memory_message_vectors"]))
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, n, fp := open()
	defer db.Close()
	wantSameHistory(t, "opening the store", before, snapshotHistory(t, db))
	if _, err := n.Reload(); err != nil {
		t.Fatal(err)
	}
	if rooms, err := n.LeafWidthRepairRooms(); err != nil || len(rooms) != 0 {
		t.Fatalf("rooms to repair: %v %v", rooms, err)
	}
	wantSameHistory(t, "loading the index", before, snapshotHistory(t, db))
	dry, err := n.IndexRooms(ctx, IndexOptions{DryRun: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if dry.Pending != 0 {
		t.Errorf("queued after an upgrade: %+v", dry)
	}
	rep, err := n.IndexRooms(ctx, IndexOptions{BudgetUSD: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Pending != 0 || rep.SpentUSD != 0 || fp.embedCalls.Load() != 0 || fp.chatCalls.Load() != 0 {
		t.Errorf("an upgrade spent: %+v embeds %d chats %d", rep, fp.embedCalls.Load(), fp.chatCalls.Load())
	}
	wantSameHistory(t, "the pass after the start", before, snapshotHistory(t, db))
}

// The paid people backfill (person memory v1.7.0) is started by an operator only. A store holding a finished pilot
// and a run with a batch in flight goes through a restart and the index passes: no backfill row moves, nothing is
// sent anywhere, no model is asked anything. (The service's own restart and room-memory pass are checked in the
// application package: TestPeopleBackfillNotStartedByPasses and TestPeopleBackfillRestartPollsOnly.)
func TestARestartNeverStartsAPeopleBackfill(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	var requests atomic.Int32
	open := func() (*storage.Store, *Engine, *fakeProvider) {
		t.Helper()
		db, err := storage.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		st, err := store.Open(db)
		if err != nil {
			db.Close()
			t.Fatal(err)
		}
		for _, q := range store.PeopleBackfillDDL { // the application creates them at every start
			if _, err := db.DB.Exec(q); err != nil {
				db.Close()
				t.Fatal(err)
			}
		}
		fp := &fakeProvider{}
		t.Setenv("FAKE_KEY", "k")
		inner := fp.handler()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			inner.ServeHTTP(w, r)
		}))
		t.Cleanup(srv.Close)
		cred, _ := providers.Resolve("env:FAKE_KEY")
		a := openai.New(providers.NewClient(5*time.Second, nil), srv.URL, cred)
		return db, New(st, a, Models{Embed: "fake-embed", Rerank: "fake-rerank", Summary: "fake-summary"},
			Settings{Search: search.Default(), Recipe: leaves.DefaultRecipe(), Summaries: true, MessageVectors: true, RefTZ: time.UTC, HedgeAfter: time.Second}), fp
	}
	db, e, _ := open()
	seed(t, e)
	ctx := context.Background()
	if _, err := e.IndexRooms(ctx, IndexOptions{BudgetUSD: 1}, nil); err != nil {
		t.Fatal(err)
	}
	at := "2026-11-01T00:00:00Z"
	for _, r := range []store.BackfillRun{
		{ID: "pb_pilot", GuildID: "g1", State: store.BackfillPiloted, Token: "t1", Params: []byte(`{}`), Provider: "claude", BudgetUSD: 3, PilotDone: true,
			Pilot: []byte(`{"windows":1}`), Windows: 1, CreatedAt: at, UpdatedAt: at},
		{ID: "pb_run", GuildID: "g2", State: store.BackfillSubmitted, Token: "t2", Params: []byte(`{}`), Model: "claude-haiku-5-5", Provider: "claude",
			BudgetUSD: 3, PilotDone: true, Batches: []byte(`[{"key":"pbb_1","state":"submitted","model":"claude-haiku-5-5","windows":1,"max_usd":0.01,"at":"` + at + `"}]`),
			Windows: 2, CreatedAt: at, UpdatedAt: at},
	} {
		if err := store.InsertBackfillRun(db.DB, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.InsertBackfillWindows(db.DB, []store.BackfillWindow{
		{RunID: "pb_pilot", ID: "w1", Room: "discord:g1:c1", Obs: []string{"o1"}, State: store.WindowQueued, Pilot: true, Result: []byte(`{"m":{}}`), UpdatedAt: at},
		{RunID: "pb_run", ID: "w2", Room: "discord:g2:c1", Obs: []string{"o2"}, State: store.WindowSubmitted, Batch: "pbb_1", Attempts: 1, UpdatedAt: at},
		{RunID: "pb_run", ID: "w3", Room: "discord:g2:c1", Seq: 1, Obs: []string{"o3"}, State: store.WindowQueued, UpdatedAt: at},
	}); err != nil {
		t.Fatal(err)
	}
	backfill := func() map[string][]string {
		return map[string][]string{"runs": tableRows(t, db, "people_backfill_run", ""), "windows": tableRows(t, db, "people_backfill_window", ""),
			"spend": tableRows(t, db, "memory_spend", "WHERE kind='"+store.SpendPeopleBackfill+"'")}
	}
	before := backfill()
	history := snapshotHistory(t, db)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	requests.Store(0)
	db, n, fp := open()
	defer db.Close()
	if _, err := n.Reload(); err != nil {
		t.Fatal(err)
	}
	if _, err := n.IndexRooms(ctx, IndexOptions{BudgetUSD: 1}, nil); err != nil {
		t.Fatal(err)
	}
	after := backfill()
	for k, rows := range before {
		if !slices.Equal(rows, after[k]) {
			t.Errorf("a restart moved the people backfill's %s:\n%v\n%v", k, rows, after[k])
		}
	}
	wantSameHistory(t, "a restart with a people backfill stored", history, snapshotHistory(t, db))
	if requests.Load() != 0 || fp.chatCalls.Load() != 0 || fp.embedCalls.Load() != 0 {
		t.Fatalf("a restart sent %d requests (chat %d, embed %d)", requests.Load(), fp.chatCalls.Load(), fp.embedCalls.Load())
	}
}
