package application_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"masamong/internal/application"
	memstore "masamong/internal/memory/store"
	"masamong/internal/storage"
)

// seedAllKinds writes rows that exercise every column kind a migration must carry: Korean and emoji
// text, NULL and non-NULL blobs, REAL, AUTOINCREMENT ids and a tool usage row.
func seedAllKinds(t *testing.T, svc *application.Service) {
	t.Helper()
	if _, err := memstore.Open(svc.Store); err != nil {
		t.Fatal(err)
	}
	db := svc.Store.DB
	must := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	must(`INSERT INTO memory_rooms(room, collection, platform, label, naive_timezone, updated_at) VALUES(?,?,?,?,?,?)`,
		"kakao:friends", "friends", "kakao", "친구들 🐶", "Asia/Seoul", "2026-09-23T00:00:00Z")
	must(`INSERT INTO memory_leaves(leaf_id, room, recipe, first_observation_id, observation_ids, n, body, body_hash, speakers,
		end_time, closed, summary, embed_space, leaf_vec, summary_vec, updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"leaf-1", "kakao:friends", "r1", "o1", `["o1","o2"]`, 2, "철수: 부산 가자\n영희: ㅇㅋ", "h", `["철수","영희"]`,
		"2026-09-01T00:00:00Z", 1, "", "space", []byte{0, 1, 2, 255, 0x3c}, nil, "2026-09-23T00:00:00Z")
	must(`INSERT INTO memory_spend(day, kind, usd, calls) VALUES(?,?,?,?)`, "2026-09-23", "chat", 0.0012345678901, 7)
	for i := 0; i < 3; i++ {
		must(`INSERT INTO chat_turns(channel_key, role, speaker, content, created_at) VALUES(?,?,?,?,?)`,
			"1:2", "user", "tester", fmt.Sprintf("안녕 %d", i), "2026-09-23T00:00:00Z")
	}
	if r := exec(svc, "tools.list", nil); !r.OK { // creates tool_usage
		t.Fatalf("tools.list: %+v", r)
	}
	must(`INSERT INTO tool_usage(day, tool, scope, subject, n, last_at) VALUES(?,?,?,?,?,?)`, "2026-09-23", "finance.fx", "all", "", 3, "2026-09-23T01:00:00Z")
}

func tableRows(t *testing.T, r map[string]any) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for k, v := range r["tables"].(map[string]any) {
		out[k] = int64(v.(float64))
	}
	return out
}

func statusOf(t *testing.T, svc *application.Service) map[string]any {
	t.Helper()
	r := exec(svc, "storage.status", nil)
	if !r.OK {
		t.Fatalf("storage.status: %+v", r)
	}
	return asMap(t, r.Output)
}

func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := jsonRoundTrip(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func checkSeeded(t *testing.T, db *sql.DB) {
	t.Helper()
	var vec []byte
	var sum sql.NullString
	var body string
	if err := db.QueryRow(`SELECT leaf_vec, summary_vec, body FROM memory_leaves WHERE leaf_id='leaf-1'`).Scan(&vec, &sum, &body); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(vec, []byte{0, 1, 2, 255, 0x3c}) || sum.Valid || !strings.Contains(body, "부산") {
		t.Fatalf("leaf not preserved: vec=%v summary_vec_valid=%v body=%q", vec, sum.Valid, body)
	}
	var usd float64
	if err := db.QueryRow(`SELECT usd FROM memory_spend WHERE day='2026-09-23'`).Scan(&usd); err != nil || usd != 0.0012345678901 {
		t.Fatalf("usd %v %v", usd, err)
	}
	var maxID int64
	if err := db.QueryRow(`SELECT MAX(id) FROM chat_turns`).Scan(&maxID); err != nil || maxID != 3 {
		t.Fatalf("chat_turns ids %d %v", maxID, err)
	}
}

// storage.migrate is off together with the TiDB backend (storage.TiDBEnabled): it refuses to run and
// says why, instead of moving anything.
func TestStorageMigrateIsDisabled(t *testing.T) {
	svc := setupSvc(t)
	r := exec(svc, "storage.migrate", map[string]any{"to": "sqlite", "sqlite_file": "moved.db", "confirm": true})
	if r.OK || r.Error == nil || r.Error.Code != "ACTION_UNAVAILABLE" || r.Error.Args["reason"] != "disabled_by_operator" {
		t.Fatalf("storage.migrate must be unavailable: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(svc.DataRoot, "moved.db")); !os.IsNotExist(err) {
		t.Fatalf("a disabled migrate must not create a file: %v", err)
	}
	st := statusOf(t, svc)
	if st["backend"] != "sqlite" {
		t.Fatalf("status backend: %v", st)
	}
}

// An instance whose storage.yaml still names TiDB does not start on SQLite behind the operator's back:
// it stops with the reason and the way over (restore a backup).
func TestTiDBStorageSettingsAreRefused(t *testing.T) {
	if storage.TiDBEnabled {
		t.Skip("TiDB backend enabled")
	}
	dir := t.TempDir()
	if err := os.WriteFile(storage.SettingsPath(dir), []byte("backend: tidb\ndsn_ref: env:MASAMONG_TIDB_DSN\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := application.Open(dir, nil, false); err == nil || !strings.Contains(err.Error(), "TiDB backend is disabled") {
		t.Fatalf("open with backend: tidb: %v", err)
	}
	t.Setenv("MASAMONG_DB_BACKEND", "tidb")
	if _, err := storage.OptionsFromEnv(t.TempDir(), storage.Settings{}); err == nil {
		t.Fatal("MASAMONG_DB_BACKEND=tidb must be refused")
	}
}

// TestStorageMigrateTiDBRoundTrip moves SQLite → TiDB → SQLite on a live server. It creates and drops
// its own database, so point MASAMONG_TIDB_TEST_DSN at a throwaway server (e.g. a local TiDB container).
func TestStorageMigrateTiDBRoundTrip(t *testing.T) {
	if !storage.TiDBEnabled {
		t.Skip("the TiDB backend is disabled (storage.TiDBEnabled)")
	}
	base := os.Getenv("MASAMONG_TIDB_TEST_DSN")
	if base == "" {
		t.Skip("set MASAMONG_TIDB_TEST_DSN to run the live SQLite ↔ TiDB migration test")
	}
	name := fmt.Sprintf("masamong_mig_%d", rand.Int63())
	admin, err := sql.Open("mysql", base)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec("DROP DATABASE " + name)
	slash := strings.LastIndex(base, "/")
	q := ""
	if i := strings.Index(base[slash:], "?"); i >= 0 {
		q = base[slash+i:]
	}
	t.Setenv("MASAMONG_TIDB_MIG_DSN", base[:slash+1]+name+q)

	svc := setupSvc(t)
	seedAllKinds(t, svc)
	before := tableRows(t, statusOf(t, svc))
	dir := svc.DataRoot
	up := exec(svc, "storage.migrate", map[string]any{"to": "tidb", "dsn_ref": "env:MASAMONG_TIDB_MIG_DSN", "confirm": true})
	if !up.OK || asMap(t, up.Output)["verified"] != true {
		t.Fatalf("sqlite → tidb: %+v", up)
	}
	_ = svc.Close()

	onTiDB, err := application.Open(dir, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if onTiDB.Store.Backend != storage.BackendTiDB || !onTiDB.Ready() {
		t.Fatalf("reopened on %s ready=%v", onTiDB.Store.Backend, onTiDB.Ready())
	}
	mid := tableRows(t, statusOf(t, onTiDB))
	for table, n := range before {
		if table == "audit_log" {
			continue
		}
		if mid[table] != n {
			t.Fatalf("tidb %s: %d rows, want %d", table, mid[table], n)
		}
	}
	checkSeeded(t, onTiDB.Store.DB)
	b := exec(onTiDB, "backup.create", nil)
	if !b.OK {
		t.Fatalf("tidb logical backup: %+v", b)
	}
	// damage the data, then restore the logical dump: vectors (BLOB) and every row must come back
	bname := b.Output["name"].(string)
	if _, err := onTiDB.Store.DB.Exec(`UPDATE memory_leaves SET leaf_vec = ? WHERE leaf_id='leaf-1'`, []byte("broken")); err != nil {
		t.Fatal(err)
	}
	if _, err := onTiDB.Store.DB.Exec(`DELETE FROM chat_turns`); err != nil {
		t.Fatal(err)
	}
	if r := exec(onTiDB, "backup.restore", map[string]any{"name": bname, "confirm": bname}); !r.OK {
		t.Fatalf("stage restore: %+v", r)
	}
	_ = onTiDB.Close()
	res := application.ApplyPendingRestore(dir, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	if res == nil || !res.OK || res.Method != "import_dump" {
		t.Fatalf("tidb restore: %+v", res)
	}
	onTiDB, err = application.Open(dir, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	checkSeeded(t, onTiDB.Store.DB)
	restored := tableRows(t, statusOf(t, onTiDB))
	for table, n := range mid {
		if table != "audit_log" && restored[table] != n {
			t.Fatalf("restored %s: %d rows, want %d", table, restored[table], n)
		}
	}
	down := exec(onTiDB, "storage.migrate", map[string]any{"to": "sqlite", "sqlite_file": "back.db", "confirm": true})
	if !down.OK || asMap(t, down.Output)["verified"] != true {
		t.Fatalf("tidb → sqlite: %+v", down)
	}
	_ = onTiDB.Close()

	back, err := application.Open(dir, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer back.Close()
	if back.Store.Path() != filepath.Join(dir, "back.db") {
		t.Fatalf("reopened on %s", back.Store.Path())
	}
	checkSeeded(t, back.Store.DB)
	end := tableRows(t, statusOf(t, back))
	for table, n := range before {
		if table == "audit_log" {
			continue
		}
		if end[table] != n {
			t.Fatalf("round trip %s: %d rows, want %d", table, end[table], n)
		}
	}
}

func jsonRoundTrip(v any) (map[string]any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	err = json.Unmarshal(raw, &out)
	return out, err
}
