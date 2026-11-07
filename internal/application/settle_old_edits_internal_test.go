package application

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/memory/corpus"
	"masamong/internal/memory/engine"
	"masamong/internal/memory/store"
)

// A database from before privacy_settled has tombstones of finished edits, deletions and withdrawals, all alike:
// that release wrote no 'erased' tombstone. These rows are what it left behind (enqueueRedactionTx, EditMessage,
// DeleteMessages and processWithdrawal of that release): the tombstones say nothing, the rows they name do.

const oldAt = "2026-10-01T00:00:00.000000000Z"

type oldData struct {
	t  *testing.T
	s  *Service
	en *engine.Engine
}

func (o oldData) capture(message, author string) string {
	o.t.Helper()
	if _, err := o.en.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", MessageID: message, AuthorID: author, AuthorName: author, Content: "old words of " + message, At: time.Now()}); err != nil {
		o.t.Fatal(err)
	}
	return corpus.DiscordLiveObservationID(engine.RoomKey("g", "c"), message)
}

func (o oldData) tombstones(message string, others ...string) {
	o.t.Helper()
	db, err := o.s.adminDB()
	if err != nil {
		o.t.Fatal(err)
	}
	if err := db.WithWriter(func(tx *sql.Tx) error {
		if err := store.Tombstone(tx, "message", message, oldAt); err != nil {
			return err
		}
		for _, id := range append(others, message) {
			if err := store.Tombstone(tx, "source", id, oldAt); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		o.t.Fatal(err)
	}
}

func (o oldData) exec(query string, args ...any) {
	o.t.Helper()
	if _, err := o.s.Store.DB.Exec(query, args...); err != nil {
		o.t.Fatal(err)
	}
}

func (o oldData) userTurn(message, speaker, words string) {
	o.t.Helper()
	o.exec(`INSERT INTO chat_turns(channel_key,role,speaker,content,created_at,native_id,speaker_id,evidence_state) VALUES('discord:g:c','user',?,?,?,?,?,1)`, speaker, words, oldAt, message, speaker)
}

// editedStored: the old release's finished edit of a stored message.
func (o oldData) editedStored(message string) string {
	obs := o.capture(message, "alice")
	o.tombstones(message, obs)
	o.exec(`UPDATE memory_observations SET content=?, edited_at=? WHERE observation_id=?`, "new words of "+message, oldAt, obs)
	o.userTurn(message, "alice", "new words of "+message)
	return obs
}

// deletedStored: the old release's finished deletion of a stored message. The rows' words are gone.
func (o oldData) deletedStored(message string) string {
	obs := o.capture(message, "alice")
	o.tombstones(message, obs)
	o.exec(`UPDATE memory_observations SET content='', deleted_at=? WHERE observation_id=?`, oldAt, obs)
	return obs
}

// withdrawnStored: the old release's finished withdrawal, with the person's message in the batch.
func (o oldData) withdrawnStored(message, person string) string {
	obs := o.capture(message, person)
	o.tombstones(message, obs)
	o.exec(`UPDATE memory_observations SET content='', deleted_at=? WHERE observation_id=?`, oldAt, obs)
	db, _ := o.s.adminDB()
	if err := db.WithWriter(func(tx *sql.Tx) error { return store.Tombstone(tx, "account", person, oldAt) }); err != nil {
		o.t.Fatal(err)
	}
	return obs
}

// holdOldEdits keeps the service's own pass out of a test that calls its parts.
func holdOldEdits(t *testing.T, s *Service) {
	t.Helper()
	if err := s.kvSet(oldEditsKey, map[string]any{"done": true}); err != nil {
		t.Fatal(err)
	}
}

func runOldEdits(t *testing.T, s *Service) {
	t.Helper()
	after := ""
	for range 100 {
		last, err := settleOldEditBatch(context.Background(), s.Store, after)
		if err != nil {
			t.Fatal(err)
		}
		if last == "" {
			return
		}
		after = last
	}
	t.Fatal("the pass over old edits did not end")
}

func TestOldEditsAreSettledAndNothingElseIs(t *testing.T) {
	s := privacyService(t)
	holdOldEdits(t, s)
	e, err := s.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	o := oldData{t, s, e}

	// what may be read again
	editedObs := o.editedStored("edited-stored")
	o.tombstones("edited-off") // capture off: only the turn, with the new words
	o.userTurn("edited-off", "bob", "new words of edited-off")

	// what never may
	deletedObs := o.deletedStored("deleted-stored")
	o.tombstones("deleted-off") // capture off: DeleteMessages took the turn in the transaction of the tombstone
	withdrawnObs := o.withdrawnStored("withdrawn-stored", "carol")
	o.tombstones("withdrawn-off") // capture off: DELETE FROM chat_turns WHERE speaker_id took the turn
	// the withdrawal's tombstone is there and the turn still is (nothing should leave this, but it must not excuse)
	o.tombstones("withdrawn-row")
	o.userTurn("withdrawn-row", "carol", "words of carol")
	// a live stored row, but of a person who withdrew
	liveObs := o.capture("withdrawn-live", "carol")
	o.tombstones("withdrawn-live", liveObs)
	o.exec(`UPDATE memory_observations SET edited_at=? WHERE observation_id=?`, oldAt, liveObs)
	// a turn written after the tombstone is not what an edit left, whatever it holds
	o.tombstones("written-later")
	o.exec(`INSERT INTO chat_turns(channel_key,role,speaker,content,created_at,native_id,speaker_id,evidence_state) VALUES('discord:g:c','user','bob','words','2026-10-02T00:00:00Z','written-later','bob',1)`)
	// a stored row older than its tombstone: a change not applied, or the old release's edit that kept the words.
	// The rows cannot tell, so it stays blocked.
	staleObs := o.capture("stale", "alice")
	o.tombstones("stale", staleObs)
	// a leaf held a message that changed. Which change is not recorded: a leaf is cut again under the same ID
	// without what was deleted from it.
	o.exec(`INSERT INTO privacy_tombstones(kind,source_id,at) VALUES('source','some-leaf',?)`, oldAt)
	// a change marked by this release as an erasure
	erasedObs := o.editedStored("erased")
	db, _ := s.adminDB()
	if err := db.WithWriter(func(tx *sql.Tx) error { return store.Tombstone(tx, "erased", erasedObs, oldAt) }); err != nil {
		t.Fatal(err)
	}

	runOldEdits(t, s)

	settled := settledSet(s)
	for _, id := range []string{"edited-stored", editedObs, "edited-off"} {
		if !settled[id] {
			t.Errorf("%s of a finished edit was not settled: %v", id, settled)
		}
	}
	for _, id := range []string{"deleted-stored", deletedObs, "deleted-off", "withdrawn-stored", withdrawnObs, "withdrawn-off", "withdrawn-row",
		"withdrawn-live", liveObs, "written-later", "stale", staleObs, "some-leaf", "erased", erasedObs} {
		if settled[id] {
			t.Errorf("%s was settled", id)
		}
	}

	// and what that means for a reply: the promise for the deleted and withdrawn stays, the edited ones work again
	mark := store.SettledMark(s.Store)
	if mark == 0 {
		t.Fatal("nothing settled")
	}
	blocked := func(id string) bool {
		t.Helper()
		var out bool
		if err := db.WithWriter(func(tx *sql.Tx) error {
			var err error
			out, err = store.SourcesBlockedAsOf(tx, []string{id}, mark)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	for _, id := range []string{"edited-stored", editedObs, "edited-off"} {
		if blocked(id) {
			t.Errorf("%s still blocks a new reply", id)
		}
	}
	for _, id := range []string{"deleted-stored", deletedObs, "deleted-off", "withdrawn-stored", withdrawnObs, "withdrawn-off", "withdrawn-row", "stale", "some-leaf", erasedObs} {
		if !blocked(id) {
			t.Errorf("%s no longer blocks a new reply", id)
		}
	}
}

// A message with a job in the queue is the job's: the job settles it if it is an edit, and if it is a deletion the
// stored row still looks current until the worker reaches it.
func TestOldEditsLeaveAMessageWithAJobWaiting(t *testing.T) {
	s := privacyService(t)
	holdOldEdits(t, s)
	e, err := s.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	o := oldData{t, s, e}
	o.editedStored("deleted-next")
	o.editedStored("edited-next")
	s.redaction.running.Store(true)
	if err := s.queueSourceChange("g", "c", []string{"deleted-next"}, "delete", "", s.Clock.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.queueSourceChange("g", "c", []string{"edited-next"}, "edit", "newer words", s.Clock.Now()); err != nil {
		t.Fatal(err)
	}
	runOldEdits(t, s)
	if settled := settledSet(s); len(settled) != 0 {
		t.Fatalf("settled while their jobs wait: %v", settled)
	}
	s.redaction.running.Store(false)
	if err := s.DrainRedactions(context.Background()); err != nil {
		t.Fatal(err)
	}
	settled := settledSet(s)
	if settled["deleted-next"] {
		t.Fatalf("the deleted message was settled: %v", settled)
	}
	if !settled["edited-next"] {
		t.Fatalf("the job did not settle its edit: %v", settled)
	}
}

// A deletion the old release queued and had not carried out: nothing marks it as one but its job, and the turn is still
// there because the crash came before the foreground step and the worker has not reached it.
func TestOldEditsLeaveAnOldDeletionStillQueued(t *testing.T) {
	s := privacyService(t)
	holdOldEdits(t, s)
	e, err := s.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	o := oldData{t, s, e}
	o.tombstones("deleted-soon")
	o.userTurn("deleted-soon", "bob", "words")
	s.redaction.running.Store(true)
	defer s.redaction.running.Store(false)
	o.exec(`INSERT INTO redaction_jobs(payload,created_at) VALUES(?,?)`, `{"messages":["deleted-soon"],"operation":"delete","at":"`+oldAt+`"}`, oldAt)
	runOldEdits(t, s)
	if settled := settledSet(s); len(settled) != 0 {
		t.Fatalf("settled while its deletion waits in the queue: %v", settled)
	}
}

// A change that came in and is not in the queue yet has only its event file: nothing is judged until it is.
func TestOldEditsWaitForAChangeNotYetInTheQueue(t *testing.T) {
	s := privacyService(t)
	holdOldEdits(t, s)
	e, err := s.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	o := oldData{t, s, e}
	o.editedStored("edited")
	if err := s.kvSet(oldEditsKey, map[string]any{}); err != nil {
		t.Fatal(err)
	}
	s.redaction.running.Store(true)
	defer s.redaction.running.Store(false)
	if _, err := s.Store.EnqueueEvent("source-change", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if done, err := s.settleOldEdits(context.Background()); err != nil || done {
		t.Fatalf("with a change waiting: done %v, %v", done, err)
	}
	if settled := settledSet(s); len(settled) != 0 {
		t.Fatalf("settled with a change not yet in the queue: %v", settled)
	}
}

// The pass never holds the write connection for the whole history: it goes in batches, it resumes after a stop, and
// an edit settled by the worker keeps its number (a new one would refuse the turns under way).
func TestOldEditsGoInBatchesAndKeepNumbers(t *testing.T) {
	s := privacyService(t)
	holdOldEdits(t, s)
	e, err := s.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	o := oldData{t, s, e}
	n := oldEditsBatch*2 + 1
	for i := range n {
		id := fmt.Sprintf("edited-%04d", i)
		o.tombstones(id)
		o.userTurn(id, "bob", "new words")
	}
	db, _ := s.adminDB()
	if err := db.WithWriter(func(tx *sql.Tx) error { return store.SettleSources(tx, []string{"edited-0000"}) }); err != nil {
		t.Fatal(err)
	}
	seq := func() (v int64) {
		t.Helper()
		if err := s.Store.DB.QueryRow(`SELECT seq FROM privacy_settled WHERE source_id='edited-0000'`).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	before := seq()
	messages := func() (m int) {
		t.Helper()
		if err := s.Store.DB.QueryRow(`SELECT COUNT(*) FROM privacy_settled WHERE source_id GLOB 'edited-*'`).Scan(&m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	last, err := settleOldEditBatch(context.Background(), s.Store, "")
	if err != nil || last == "" {
		t.Fatalf("first batch: %q, %v", last, err)
	}
	if got := messages(); got != oldEditsBatch {
		t.Fatalf("one batch left %d messages settled, want %d", got, oldEditsBatch)
	}
	if got := seq(); got != before {
		t.Fatalf("a settled edit took a new number: %d, was %d", got, before)
	}
	// the pass ends and remembers where it was; a stop in between loses at most what it would look at again
	if err := s.kvSet(oldEditsKey, map[string]any{"after": last}); err != nil {
		t.Fatal(err)
	}
	for {
		done, err := s.settleOldEdits(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if done {
			break
		}
	}
	if got := messages(); got != n {
		t.Fatalf("after the pass: %d settled, want %d", got, n)
	}
	if got := seq(); got != before {
		t.Fatalf("a settled edit took a new number: %d, was %d", got, before)
	}
	// once: a later call looks at nothing, so a tombstone written since is not judged by these rules
	o.tombstones("edited-after-the-pass")
	o.userTurn("edited-after-the-pass", "bob", "words")
	if done, err := s.settleOldEdits(context.Background()); err != nil || !done {
		t.Fatalf("after the pass: done %v, %v", done, err)
	}
	if settledSet(s)["edited-after-the-pass"] {
		t.Fatal("the pass over old edits ran again")
	}
}

// The whole way: a database as the old release left it, opened by this one, answers on the edited message again.
func TestAnUpgradedDatabaseKeepsRepliesOnAnOldEdit(t *testing.T) {
	dir := t.TempDir()
	first, err := Open(dir, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if r := first.Execute(context.Background(), action.Request{Action: "setup.resume", Input: []byte(`{"instance":"upgrade"}`)}); !r.OK {
		t.Fatal(r.Error)
	}
	e, err := first.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	o := oldData{t, first, e}
	o.editedStored("edited-stored")
	o.deletedStored("deleted-stored")
	if settled := settledSet(first); len(settled) != 0 {
		t.Fatalf("settled before the upgrade: %v", settled)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	oldEditsDelay = 0
	t.Cleanup(func() { oldEditsDelay = 30 * time.Second })
	s, err := Open(dir, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	end := time.Now().Add(20 * time.Second)
	for !settledSet(s)["edited-stored"] {
		if time.Now().After(end) {
			t.Fatal("the start of the service did not settle the old edit")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if settledSet(s)["deleted-stored"] {
		t.Fatal("the start of the service settled a deletion")
	}
}

// BenchmarkOldEditBatch is how long one batch holds the write connection, on a store with many messages and turns.
func BenchmarkOldEditBatch(b *testing.B) {
	s, err := Open(b.TempDir(), nil, false)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })
	if r := s.Execute(context.Background(), action.Request{Action: "setup.resume", Input: []byte(`{"instance":"bench"}`)}); !r.OK {
		b.Fatal(r.Error)
	}
	if err := s.kvSet(oldEditsKey, map[string]any{"done": true}); err != nil {
		b.Fatal(err)
	}
	const messages, chatRows = 5000, 5000
	err = s.Store.WithWriter(func(tx *sql.Tx) error {
		for i := range messages {
			id := fmt.Sprintf("old-%05d", i)
			if err := store.Tombstone(tx, "message", id, oldAt); err != nil {
				return err
			}
			if err := store.Tombstone(tx, "source", id, oldAt); err != nil {
				return err
			}
			if i < chatRows {
				if _, err := tx.Exec(`INSERT INTO chat_turns(channel_key,role,speaker,content,created_at,native_id,speaker_id,evidence_state) VALUES('discord:g:c','user','bob','words',?,?,'bob',1)`, oldAt, id); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for range b.N {
		b.StopTimer()
		if _, err := s.Store.DB.Exec(`DELETE FROM privacy_settled`); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		last, err := settleOldEditBatch(context.Background(), s.Store, "")
		if err != nil || last == "" {
			b.Fatalf("%q, %v", last, err)
		}
	}
}
