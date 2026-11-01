package application_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/storage"
)

// setupTiDBSvc opens a service on a fresh database of the MASAMONG_TIDB_TEST_DSN server.
func setupTiDBSvc(t *testing.T) *application.Service {
	t.Helper()
	if !storage.TiDBEnabled {
		t.Skip("the TiDB backend is disabled (storage.TiDBEnabled)")
	}
	base := os.Getenv("MASAMONG_TIDB_TEST_DSN")
	if base == "" {
		t.Skip("set MASAMONG_TIDB_TEST_DSN to run against a live TiDB")
	}
	name := fmt.Sprintf("masamong_live_%d", rand.Int63())
	admin, err := sql.Open("mysql", base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec("DROP DATABASE " + name) })
	slash := strings.LastIndex(base, "/")
	q := ""
	if i := strings.Index(base[slash:], "?"); i >= 0 {
		q = base[slash+i:]
	}
	t.Setenv("MASAMONG_TIDB_LIVE_DSN", base[:slash+1]+name+q)
	dir := t.TempDir()
	if err := storage.SaveSettings(dir, storage.Settings{Backend: storage.BackendTiDB, DSNRef: "env:MASAMONG_TIDB_LIVE_DSN"}); err != nil {
		t.Fatal(err)
	}
	svc, err := application.Open(dir, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	raw, _ := json.Marshal(map[string]any{"instance": "gate"})
	if res := svc.Execute(context.Background(), action.Request{Action: "setup.resume", Input: raw, Locale: "en"}); !res.OK {
		t.Fatalf("setup: %+v", res)
	}
	return svc
}

// On TiDB writes are not serialized in the process: simultaneous button votes (applied to the latest
// revision) must still all be counted, each exactly once.
func TestTiDBSimultaneousVotesAllCount(t *testing.T) {
	svc := setupTiDBSvc(t)
	if svc.Store.Backend != storage.BackendTiDB {
		t.Fatalf("backend %s", svc.Store.Backend)
	}
	created := exec(svc, "polls.create", map[string]any{"question": "q", "options": []string{"a", "b"}, "scope": "discord:c"})
	if !created.OK {
		t.Fatalf("%+v", created.Error)
	}
	id := created.Output["id"].(string)
	const voters = 16
	var wg sync.WaitGroup
	var failed atomic.Int32
	for i := 0; i < voters; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := exec(svc, "polls.vote", map[string]any{"id": id, "scope": "discord:c", "participant": fmt.Sprintf("p%d", i),
				"option_ids": []string{"a"}, "latest": true})
			if !r.OK {
				failed.Add(1)
				t.Errorf("vote %d: %+v", i, r.Error)
			}
		}(i)
	}
	wg.Wait()
	final := exec(svc, "polls.result", map[string]any{"id": id})
	if got := intVal(final.Output, "unique_voters"); got != voters-int(failed.Load()) || got != voters {
		t.Fatalf("%d of %d votes stored", got, voters)
	}
}
