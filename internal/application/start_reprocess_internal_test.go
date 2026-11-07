package application

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/memory/corpus"
	"masamong/internal/memory/leaves"
	"masamong/internal/storage"
)

// memoryRows reads what cost money or holds the words, every column of every row.
func memoryRows(t *testing.T, db *storage.Store) string {
	t.Helper()
	var out strings.Builder
	for _, table := range []string{"memory_leaves", "memory_observations", "memory_message_vectors"} {
		rows, err := db.DB.Query(`SELECT * FROM ` + table + ` ORDER BY 1`)
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
			fmt.Fprintln(&out, table, vals)
		}
		rows.Close()
	}
	return out.String()
}

// The start of the whole service on the file of an older release, and the passes that follow it: not one stored row of
// the history changes. internal/memory/engine/no_reprocess_test.go also shows that no model is asked anything.
func TestAStartOnAnOlderReleasesStoreLeavesItsRows(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if r := s.Execute(context.Background(), action.Request{Action: "setup.resume", Input: []byte(`{"instance":"old"}`)}); !r.OK {
		t.Fatal(r.Error)
	}
	e, err := s.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	const room = "discord:1:10"
	base := time.Now().Add(-48 * time.Hour)
	var batch []corpus.Observation
	for i := range 24 {
		batch = append(batch, corpus.Observation{ObservationID: fmt.Sprintf("%s#%d", room, i), Platform: corpus.PlatformOfRoom(room), Collection: "t", SourceRoom: room,
			ActorLabel: "철수", ActorAccount: "acct", Content: fmt.Sprintf("말 %d", i), ParsedTime: base.Add(time.Duration(i) * time.Minute).UTC().Format(time.RFC3339), RecordKind: corpus.HumanText})
	}
	if _, err := e.Store.InsertObservations(batch, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Store.SyncLeaves(room, leaves.DefaultRecipe(), time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`UPDATE memory_leaves SET leaf_vec=X'01020304', summary=substr(body,1,180), summary_vec=X'01020304', embed_space='old-model:2:l2'`,
		`INSERT INTO memory_message_vectors(observation_id,room,embed_space,text_hash,label,fmt,vec,updated_at)
			SELECT observation_id,room,'old-model:2:l2','hash','',0,X'01020304','2026-01-01' FROM memory_observations`,
		`DELETE FROM memory_privacy_migration`,
		`DELETE FROM memory_message_vector_migration`,
		`DELETE FROM memory_identity_migration`,
		`DELETE FROM memory_leaf_sync`,
	} {
		if _, err := s.Store.DB.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	before := memoryRows(t, s.Store)
	if !strings.Contains(before, "말 0") || !strings.Contains(before, "old-model") {
		t.Fatalf("nothing stored to begin with: %q", before)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(dir, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	check := func(step string) {
		t.Helper()
		if after := memoryRows(t, s.Store); after != before {
			t.Errorf("%s changed the stored history", step)
		}
	}
	check("opening the service")
	s.BackupOnUpgrade()
	check("the backup of an upgrade")
	if _, err := s.runRetention(context.Background()); err != nil {
		t.Fatal(err)
	}
	check("the retention pass")
	s.memoryPass(context.Background())
	check("the memory pass")
}
