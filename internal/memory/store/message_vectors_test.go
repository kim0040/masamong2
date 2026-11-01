package store

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"masamong/internal/memory/corpus"
	"masamong/internal/memory/leaves"
	"masamong/internal/memory/search"
	"masamong/internal/storage"
)

const msgRoom = "discord:1:2"
const msgSpace = "embed-model:1024:l2"

// seedMsgs stores n long-enough messages written by two accounts and cuts the room's leaves. Messages
// older than the leaf gap close their leaves; pass a recent time for an open tail.
func seedMsgs(t *testing.T, st *Store, room string, n int, at time.Time) []corpus.Observation {
	t.Helper()
	var batch []corpus.Observation
	for i := 0; i < n; i++ {
		batch = append(batch, corpus.Observation{
			ObservationID: fmt.Sprintf("%s-o%02d", room, i), Platform: "discord", Collection: "d", SourceRoom: room,
			NativeMessageID: fmt.Sprintf("%s-m%02d", room, i), ActorLabel: fmt.Sprintf("friend%d", i%2),
			ActorAccount: fmt.Sprintf("acct%d", i%2), Content: fmt.Sprintf("message number %02d about the weekend trip in %s", i, room),
			ParsedTime: at.Add(time.Duration(i) * time.Minute).UTC().Format(time.RFC3339), RecordKind: corpus.HumanText})
	}
	if _, err := st.InsertObservations(batch, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SyncLeaves(room, leaves.DefaultRecipe(), time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	return batch
}

func vecOf(x float32) []byte { return search.EncodeQuantizedRow([]float32{x, 1 - x}) }

// saveAll stores a vector for each pending message.
func saveAll(t *testing.T, st *Store, ps []PendingMessage, space string) int {
	t.Helper()
	items := make([]MessageVector, len(ps))
	for i, p := range ps {
		items[i] = MessageVector{ObservationID: p.ObservationID, Space: space, TextHash: p.TextHash, Vec: vecOf(0.5)}
	}
	n, err := st.SaveMessageVectors(items)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func countVectors(t *testing.T, st *Store, where string, args ...any) int {
	t.Helper()
	var n int
	if err := st.DB.DB.QueryRow(`SELECT COUNT(*) FROM memory_message_vectors WHERE `+where, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// countCache counts the message rows in the embedding cache: message vectors must never be written there.
func countCache(t *testing.T, st *Store) int {
	t.Helper()
	var n int
	if err := st.DB.DB.QueryRow(`SELECT COUNT(*) FROM memory_embed_cache WHERE kind='message'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPendingMessagesSkipsOpenLeavesAndShortMessages(t *testing.T) {
	st := openStore(t)
	old := time.Now().Add(-48 * time.Hour)
	// 11 messages -> one closed leaf of 8 and a tail of 3 closed by idleness
	obs := seedMsgs(t, st, msgRoom, 11, old)
	// a short line inside a closed leaf: laughter and an emoji say nothing a vector could find
	short := corpus.Observation{ObservationID: "short", Platform: "discord", Collection: "d", SourceRoom: msgRoom, NativeMessageID: "short-m",
		ActorLabel: "friend0", ActorAccount: "acct0", Content: "ㅋㅋㅋㅋㅋㅋㅋ 👍", ParsedTime: old.Add(30 * time.Second).UTC().Format(time.RFC3339), RecordKind: corpus.HumanText}
	if _, err := st.InsertObservations([]corpus.Observation{short}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SyncLeaves(msgRoom, leaves.DefaultRecipe(), time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	ps, err := st.PendingMessages([]string{msgRoom}, msgSpace, 0, leaves.DefaultRecipe().Version())
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != len(obs) {
		t.Fatalf("pending %d, want the %d long messages (the short line is excluded)", len(ps), len(obs))
	}
	for _, p := range ps {
		if p.ObservationID == "short" {
			t.Fatal("a message under the meaningful-rune floor was picked")
		}
	}
	// counts agree with the list, a limit cuts it, an unrelated room is not read
	if n, _ := st.CountPendingMessages([]string{msgRoom}, msgSpace); n != len(obs) {
		t.Fatalf("count %d", n)
	}
	if got, _ := st.PendingMessages([]string{msgRoom}, msgSpace, 4); len(got) != 4 {
		t.Fatalf("limit: %d", len(got))
	}
	if got, _ := st.PendingMessages([]string{"discord:other"}, msgSpace, 0); len(got) != 0 {
		t.Fatalf("other room: %d", len(got))
	}
	if saved := saveAll(t, st, ps, msgSpace); saved != len(ps) {
		t.Fatalf("saved %d of %d", saved, len(ps))
	}
	if got, _ := st.PendingMessages([]string{msgRoom}, msgSpace, 0); len(got) != 0 {
		t.Fatalf("nothing is pending once every message has a vector, got %d (the short one must not come back)", len(got))
	}
	// another embedding space starts over: the old rows do not count for it
	if got, _ := st.PendingMessages([]string{msgRoom}, "other-model:1024:l2", 0); len(got) != len(obs) {
		t.Fatalf("new space pending %d", len(got))
	}
	total, in, err := st.MessageVectorCounts(msgSpace)
	if err != nil || total != len(obs) || in != len(obs) {
		t.Fatalf("counts %d %d %v", total, in, err)
	}
}

func TestPendingMessagesWaitForOpenLeaves(t *testing.T) {
	st := openStore(t)
	// 8 old messages close a leaf; 3 messages from now sit in the open tail leaf
	seedMsgs(t, st, msgRoom, 8, time.Now().Add(-48*time.Hour))
	recent := []corpus.Observation{}
	for i := 0; i < 3; i++ {
		recent = append(recent, corpus.Observation{ObservationID: fmt.Sprintf("recent%d", i), Platform: "discord", Collection: "d", SourceRoom: msgRoom,
			NativeMessageID: fmt.Sprintf("recent-m%d", i), ActorLabel: "friend0", ActorAccount: "acct0", Content: "an unfinished conversation tail",
			ParsedTime: time.Now().Add(-time.Duration(3-i) * time.Second).UTC().Format(time.RFC3339), RecordKind: corpus.HumanText})
	}
	if _, err := st.InsertObservations(recent, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SyncLeaves(msgRoom, leaves.DefaultRecipe(), time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	ps, err := st.PendingMessages([]string{msgRoom}, msgSpace, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 8 {
		t.Fatalf("pending %d: only the closed leaf's 8 messages, the open tail waits", len(ps))
	}
	for _, p := range ps {
		if len(p.ObservationID) >= 6 && p.ObservationID[:6] == "recent" {
			t.Fatal("a message of an open leaf was picked")
		}
	}
}

func TestSaveMessageVectorsRefusesStaleText(t *testing.T) {
	st := openStore(t)
	seedMsgs(t, st, msgRoom, 8, time.Now().Add(-48*time.Hour))
	ps, _ := st.PendingMessages([]string{msgRoom}, msgSpace, 0)
	if len(ps) != 8 {
		t.Fatalf("pending %d", len(ps))
	}
	// the author edits one message while the embedding of the old text is in flight
	if _, ok, err := st.EditMessage("discord", "discord:1:2-m03", "a different sentence entirely", time.Now().UTC().Format(time.RFC3339Nano)); err != nil || !ok {
		t.Fatalf("edit %v %v", ok, err)
	}
	if n := saveAll(t, st, ps, msgSpace); n != 7 {
		t.Fatalf("saved %d, want 7: the edited message's vector was made from text it no longer has", n)
	}
	if countVectors(t, st, `observation_id=?`, msgRoom+"-o03") != 0 {
		t.Fatal("a vector of the old text was stored")
	}
	if got, _ := st.PendingMessages([]string{msgRoom}, msgSpace, 0); len(got) != 1 || got[0].ObservationID != msgRoom+"-o03" {
		t.Fatalf("the edited message is pending again with its new text: %+v", got)
	}
}

// Every path that changes or erases a message's text removes its vector. Each case first proves the
// vector exists, so a passing "gone" cannot come from a vector that was never stored (postmortem 0003).
func TestMessageVectorsAreErasedWithTheirMessage(t *testing.T) {
	setup := func(t *testing.T) (*Store, []PendingMessage) {
		st := openStore(t)
		seedMsgs(t, st, msgRoom, 16, time.Now().Add(-48*time.Hour))
		ps, err := st.PendingMessages([]string{msgRoom}, msgSpace, 0)
		if err != nil || len(ps) != 16 {
			t.Fatalf("pending %d %v", len(ps), err)
		}
		if n := saveAll(t, st, ps, msgSpace); n != 16 {
			t.Fatalf("saved %d", n)
		}
		if countVectors(t, st, `room=?`, msgRoom) != 16 || countCache(t, st) != 0 {
			t.Fatalf("precondition: vectors %d, message rows in the embedding cache %d (want none)", countVectors(t, st, `room=?`, msgRoom), countCache(t, st))
		}
		return st, ps
	}
	target := msgRoom + "-o05"
	has := func(t *testing.T, st *Store, id string) bool { return countVectors(t, st, `observation_id=?`, id) == 1 }

	t.Run("edit", func(t *testing.T) {
		st, _ := setup(t)
		if !has(t, st, target) {
			t.Fatal("precondition: vector exists")
		}
		if _, ok, err := st.EditMessage("discord", msgRoom+"-m05", "edited text of the fifth message", time.Now().UTC().Format(time.RFC3339Nano)); err != nil || !ok {
			t.Fatalf("%v %v", ok, err)
		}
		if has(t, st, target) {
			t.Fatal("edited message kept its old vector")
		}
		if !has(t, st, msgRoom+"-o06") {
			t.Fatal("an unrelated message lost its vector")
		}
	})
	t.Run("delete", func(t *testing.T) {
		st, _ := setup(t)
		if !has(t, st, target) {
			t.Fatal("precondition: vector exists")
		}
		if revs, err := st.DeleteMessages("discord", []string{msgRoom + "-m05"}, time.Now().UTC().Format(time.RFC3339)); err != nil || len(revs) != 1 {
			t.Fatalf("%v %v", revs, err)
		}
		if has(t, st, target) {
			t.Fatal("deleted message kept its vector")
		}
	})
	t.Run("account withdrawal", func(t *testing.T) {
		st, _ := setup(t)
		if countVectors(t, st, `observation_id IN (SELECT observation_id FROM memory_observations WHERE actor_account='acct1')`) != 8 {
			t.Fatal("precondition: the account's vectors exist")
		}
		if _, err := st.DeleteAccountMessages("discord", "acct1", time.Now().UTC().Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}
		if n := countVectors(t, st, `observation_id IN (SELECT observation_id FROM memory_observations WHERE actor_account='acct1')`); n != 0 {
			t.Fatalf("%d vectors of the withdrawn account remain", n)
		}
		if n := countVectors(t, st, `room=?`, msgRoom); n != 8 {
			t.Fatalf("the other account's vectors: %d, want 8", n)
		}
	})
	t.Run("observation batch erasure", func(t *testing.T) {
		st, _ := setup(t)
		ids := []string{msgRoom + "-o01", msgRoom + "-o02", msgRoom + "-o03"}
		for _, id := range ids {
			if !has(t, st, id) {
				t.Fatal("precondition: vector exists")
			}
		}
		if revs, err := st.DeleteObservationsContext(t.Context(), ids, time.Now().UTC().Format(time.RFC3339)); err != nil || len(revs) != 3 {
			t.Fatalf("%v %v", revs, err)
		}
		for _, id := range ids {
			if has(t, st, id) {
				t.Fatalf("%s kept its vector", id)
			}
		}
	})
	t.Run("account without native IDs", func(t *testing.T) {
		st := openStore(t)
		var batch []corpus.Observation
		for i := 0; i < 8; i++ {
			batch = append(batch, corpus.Observation{ObservationID: fmt.Sprintf("k%02d", i), Platform: "kakao", Collection: "k", SourceRoom: "kakao:room", ActorLabel: "friend", ActorAccount: "kacct",
				Content: "a line of an imported conversation", ParsedTime: time.Now().Add(-72*time.Hour + time.Duration(i)*time.Minute).UTC().Format(time.RFC3339), RecordKind: corpus.HumanText})
		}
		if _, err := st.InsertObservations(batch, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := st.SyncLeaves("kakao:room", leaves.DefaultRecipe(), time.Now(), nil); err != nil {
			t.Fatal(err)
		}
		ps, _ := st.PendingMessages([]string{"kakao:room"}, msgSpace, 0)
		if n := saveAll(t, st, ps, msgSpace); n != 8 {
			t.Fatalf("saved %d", n)
		}
		if countVectors(t, st, `room='kakao:room'`) != 8 {
			t.Fatal("precondition: vectors exist")
		}
		if _, err := st.DeleteAccountMessages("kakao", "kacct", time.Now().UTC().Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}
		if n := countVectors(t, st, `room='kakao:room'`); n != 0 {
			t.Fatalf("%d vectors remain", n)
		}
	})
	t.Run("room deletion", func(t *testing.T) {
		st, _ := setup(t)
		seedMsgs(t, st, "discord:9:9", 8, time.Now().Add(-48*time.Hour))
		other, _ := st.PendingMessages([]string{"discord:9:9"}, msgSpace, 0)
		if n := saveAll(t, st, other, msgSpace); n != 8 {
			t.Fatalf("saved %d", n)
		}
		if countVectors(t, st, `room=?`, msgRoom) != 16 || countVectors(t, st, `room=?`, "discord:9:9") != 8 {
			t.Fatal("precondition: vectors exist")
		}
		if _, err := st.DeleteRoom(msgRoom); err != nil {
			t.Fatal(err)
		}
		if n := countVectors(t, st, `room=?`, msgRoom); n != 0 {
			t.Fatalf("%d vectors of the deleted room remain", n)
		}
		if countVectors(t, st, `room=?`, "discord:9:9") != 8 {
			t.Fatal("another room lost its vectors")
		}
	})
}

func TestScanMessageVectorsIsScopedAndOrdered(t *testing.T) {
	st := openStore(t)
	seedMsgs(t, st, msgRoom, 8, time.Now().Add(-48*time.Hour))
	seedMsgs(t, st, "discord:9:9", 8, time.Now().Add(-48*time.Hour))
	for _, room := range []string{msgRoom, "discord:9:9"} {
		ps, _ := st.PendingMessages([]string{room}, msgSpace, 0)
		saveAll(t, st, ps, msgSpace)
	}
	var got []string
	if err := st.ScanMessageVectors([]string{msgRoom}, msgSpace, func(v MessageVector) error {
		got = append(got, v.ObservationID)
		if v.Room != msgRoom || len(v.Vec) != 6 || v.TextHash == "" {
			t.Fatalf("row %+v", v)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 8 || got[0] != msgRoom+"-o00" || got[7] != msgRoom+"-o07" {
		t.Fatalf("rows %v", got)
	}
	n := 0
	_ = st.ScanMessageVectors([]string{msgRoom}, "another:1:l2", func(MessageVector) error { n++; return nil })
	if n != 0 {
		t.Fatalf("another space returned %d rows", n)
	}
}

// The speaker's label is part of the embedded text. A renamed speaker leaves the message and its leaf
// as they were, so nothing invalidates the vector; the pending query has to notice the label.
func TestRenamedSpeakerMakesItsMessagesPendingAgain(t *testing.T) {
	st := openStore(t)
	seedMsgs(t, st, msgRoom, 8, time.Now().Add(-48*time.Hour))
	ps, _ := st.PendingMessages([]string{msgRoom}, msgSpace, 0)
	if len(ps) != 8 || saveAll(t, st, ps, msgSpace) != 8 {
		t.Fatalf("setup: %d pending", len(ps))
	}
	if n, _ := st.CountPendingMessages([]string{msgRoom}, msgSpace); n != 0 {
		t.Fatalf("pending after saving: %d", n)
	}
	// friend1 (the odd messages) is renamed
	if _, err := st.DB.DB.Exec(`UPDATE memory_observations SET actor_label='renamed' WHERE actor_label='friend1'`); err != nil {
		t.Fatal(err)
	}
	got, err := st.PendingMessages([]string{msgRoom}, msgSpace, 0)
	if err != nil || len(got) != 4 {
		t.Fatalf("pending %d %v, want the 4 messages of the renamed speaker", len(got), err)
	}
	if n, _ := st.CountPendingMessages(nil, msgSpace); n != 4 {
		t.Fatalf("count %d", n)
	}
	for _, p := range got {
		if p.Text[:9] != "renamed: " {
			t.Fatalf("pending text %q does not carry the new label", p.Text)
		}
	}
	// embedding them again replaces the row: the hash follows the new text and nothing is pending
	if n := saveAll(t, st, got, msgSpace); n != 4 {
		t.Fatalf("saved %d", n)
	}
	if n, _ := st.CountPendingMessages([]string{msgRoom}, msgSpace); n != 0 {
		t.Fatalf("still pending after re-embedding: %d", n)
	}
	if countVectors(t, st, `text_hash=?`, got[0].TextHash) != 1 || countVectors(t, st, `1=1`) != 8 {
		t.Fatal("the row was not replaced in place")
	}
}

// A database made before the label column existed keeps its vectors and gets the column on open; those
// rows are pending once and carry the label afterwards.
func TestMessageVectorLabelColumnIsMigrated(t *testing.T) {
	st := openStore(t)
	seedMsgs(t, st, msgRoom, 8, time.Now().Add(-48*time.Hour))
	ps, _ := st.PendingMessages([]string{msgRoom}, msgSpace, 0)
	saveAll(t, st, ps, msgSpace)
	if _, err := st.DB.DB.Exec(`ALTER TABLE memory_message_vectors DROP COLUMN label`); err != nil {
		t.Skipf("this SQLite cannot drop a column: %v", err)
	}
	if st.DB.HasColumn("memory_message_vectors", "label") {
		t.Fatal("setup: column still there")
	}
	if err := migrateMessageVectors(st.DB); err != nil {
		t.Fatal(err)
	}
	if !st.DB.HasColumn("memory_message_vectors", "label") {
		t.Fatal("the label column was not added")
	}
	if n, _ := st.CountPendingMessages([]string{msgRoom}, msgSpace); n != 8 {
		t.Fatalf("old rows without a label are pending once, got %d", n)
	}
}

// The scan reads the live message: a vector is Current only while the speaker label it was made with is
// still the message's label and the message is not erased.
func TestScanMessageVectorsFlagsRenamedAndErasedMessages(t *testing.T) {
	st := openStore(t)
	seedMsgs(t, st, msgRoom, 8, time.Now().Add(-48*time.Hour))
	ps, err := st.PendingMessages([]string{msgRoom}, msgSpace, 0)
	if err != nil || len(ps) == 0 {
		t.Fatalf("%d %v", len(ps), err)
	}
	saveAll(t, st, ps, msgSpace)
	scan := func() map[string]bool {
		got := map[string]bool{}
		if err := st.ScanMessageVectors([]string{msgRoom}, msgSpace, func(v MessageVector) error { got[v.ObservationID] = v.Current; return nil }); err != nil {
			t.Fatal(err)
		}
		return got
	}
	for id, cur := range scan() {
		if !cur {
			t.Fatalf("%s is flagged outdated before anything changed", id)
		}
	}
	renamed, erased := ps[0].ObservationID, ps[1].ObservationID
	if _, err := st.DB.DB.Exec(`UPDATE memory_observations SET actor_label='renamed' WHERE observation_id=?`, renamed); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.DB.Exec(`UPDATE memory_observations SET deleted_at='2026-01-01T00:00:00Z' WHERE observation_id=?`, erased); err != nil {
		t.Fatal(err)
	}
	got := scan()
	if got[renamed] || got[erased] || len(got) != len(ps) {
		t.Fatalf("renamed %v erased %v of %d rows", got[renamed], got[erased], len(got))
	}
	for _, p := range ps[2:] {
		if !got[p.ObservationID] {
			t.Fatalf("%s lost its flag", p.ObservationID)
		}
	}
}

// Eligible messages that are in no closed leaf of the active recipe are counted (an upper bound for the
// console's estimate); once the leaves exist they are not.
func TestCountUnleafedMessages(t *testing.T) {
	st := openStore(t)
	var batch []corpus.Observation
	for i := 0; i < 5; i++ {
		batch = append(batch, corpus.Observation{ObservationID: fmt.Sprintf("u%02d", i), Platform: "discord", Collection: "d", SourceRoom: msgRoom,
			NativeMessageID: fmt.Sprintf("n%02d", i), ActorLabel: "a", ActorAccount: "a", Content: "a message long enough to count",
			ParsedTime: time.Now().Add(-48 * time.Hour).Add(time.Duration(i) * time.Minute).UTC().Format(time.RFC3339), RecordKind: corpus.HumanText})
	}
	batch = append(batch, corpus.Observation{ObservationID: "short", Platform: "discord", Collection: "d", SourceRoom: msgRoom, NativeMessageID: "ns",
		ActorLabel: "a", ActorAccount: "a", Content: "ㅋㅋㅋ", ParsedTime: time.Now().Add(-47 * time.Hour).UTC().Format(time.RFC3339), RecordKind: corpus.HumanText})
	if _, err := st.InsertObservations(batch, nil); err != nil {
		t.Fatal(err)
	}
	recipe := leaves.DefaultRecipe()
	if n, err := st.CountUnleafedMessages(nil, recipe.Version()); err != nil || n != 5 {
		t.Fatalf("before any leaf: %d %v", n, err)
	}
	if _, err := st.SyncLeaves(msgRoom, recipe, time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	if n, err := st.CountUnleafedMessages(nil, recipe.Version()); err != nil || n != 0 {
		t.Fatalf("after the leaves closed: %d %v", n, err)
	}
	other := recipe
	other.Size += 2
	if n, err := st.CountUnleafedMessages([]string{msgRoom}, other.Version()); err != nil || n != 5 {
		t.Fatalf("another recipe: %d %v", n, err)
	}
}

// A message vector is stored in the form the index holds it in (int8 + scale), once, in its own table: the
// row is the in-memory quantisation of the vector, and nothing of it reaches the embedding cache.
func TestSavedMessageVectorIsTheQuantisedRowAndNotCached(t *testing.T) {
	st := openStore(t)
	seedMsgs(t, st, msgRoom, 8, time.Now().Add(-48*time.Hour))
	ps, err := st.PendingMessages([]string{msgRoom}, msgSpace, 0)
	if err != nil || len(ps) != 8 {
		t.Fatalf("%d %v", len(ps), err)
	}
	vec := []float32{0.6, -0.8, 0, 0.0123}
	raw := search.EncodeQuantizedRow(vec)
	items := make([]MessageVector, len(ps))
	for i, p := range ps {
		items[i] = MessageVector{ObservationID: p.ObservationID, Space: msgSpace, TextHash: p.TextHash, Vec: raw}
	}
	if n, err := st.SaveMessageVectors(items); err != nil || n != 8 {
		t.Fatalf("%d %v", n, err)
	}
	if n := countCache(t, st); n != 0 {
		t.Fatalf("%d message rows in the embedding cache", n)
	}
	var evidence int
	if err := st.DB.DB.QueryRow(`SELECT COUNT(*) FROM evidence_refs WHERE holder_kind='cache'`).Scan(&evidence); err != nil || evidence != 0 {
		t.Fatalf("%d cache evidence refs for message vectors (%v)", evidence, err)
	}
	var blob int
	if err := st.DB.DB.QueryRow(`SELECT MIN(LENGTH(vec)) FROM memory_message_vectors`).Scan(&blob); err != nil || blob != len(vec)+4 {
		t.Fatalf("stored row is %d bytes, want dims+4 = %d (%v)", blob, len(vec)+4, err)
	}
	if err := st.ScanMessageVectors([]string{msgRoom}, msgSpace, func(v MessageVector) error {
		q, scale, ok := search.DecodeQuantizedRow(v.Vec, nil)
		want := search.NewQuantized(len(vec), 1)
		want.AppendQuantized(vec)
		wq, ws := want.RowQ(0)
		if v.Format != MessageFormatInt8 || !ok || scale != ws || len(q) != len(wq) {
			t.Fatalf("format %d ok %v scale %v want %v", v.Format, ok, scale, ws)
		}
		for i := range q {
			if q[i] != wq[i] {
				t.Fatalf("component %d: stored %d, in-memory quantisation %d", i, q[i], wq[i])
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// legacyMessageVectorStore opens a store and puts it in the state the first version of the message vector
// table left: no label/format column, no cleanup marker, one float16 vector, and a message-kind and a
// leaf-kind cache row.
func legacyMessageVectorStore(t *testing.T) *storage.Store {
	t.Helper()
	db, err := storage.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := Open(db); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`DROP TABLE memory_message_vector_migration`,
		`DROP TABLE memory_message_vectors`,
		`CREATE TABLE memory_message_vectors (observation_id TEXT PRIMARY KEY, room TEXT NOT NULL, embed_space TEXT NOT NULL, text_hash TEXT NOT NULL, label TEXT NOT NULL DEFAULT '', vec BLOB NOT NULL, updated_at TEXT NOT NULL)`,
		`INSERT INTO memory_message_vectors(observation_id, room, embed_space, text_hash, label, vec, updated_at) VALUES('o1','r','s','h','x',X'00003c00','t')`,
		`INSERT INTO memory_embed_cache(cache_key, vec, created_at, kind) VALUES('k-message', X'00003c00', 't', 'message')`,
		`INSERT INTO memory_embed_cache(cache_key, vec, created_at, kind) VALUES('k-leaf', X'00003c00', 't', '')`,
	} {
		if _, err := db.DB.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	return db
}

func cacheRows(t *testing.T, db *storage.Store) (message, other int) {
	t.Helper()
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM memory_embed_cache WHERE kind='message'`).Scan(&message); err != nil {
		t.Fatal(err)
	}
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM memory_embed_cache WHERE kind<>'message'`).Scan(&other); err != nil {
		t.Fatal(err)
	}
	return message, other
}

func cleanupMarker(t *testing.T, db *storage.Store) int {
	t.Helper()
	var n int
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM memory_message_vector_migration WHERE id=1 AND done=1`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A store made by the first version of the table has no format column and cached every message vector.
// Opening it adds the column (existing rows are float16), removes those cache copies and leaves the
// other cache entries and the vectors alone.
func TestOpenMigratesTheFirstVersionOfTheMessageVectorTable(t *testing.T) {
	db := legacyMessageVectorStore(t)
	if _, err := Open(db); err != nil {
		t.Fatal(err)
	}
	var format int
	if err := db.DB.QueryRow(`SELECT fmt FROM memory_message_vectors WHERE observation_id='o1'`).Scan(&format); err != nil || format != MessageFormatFloat16 {
		t.Fatalf("existing row: format %d (%v), want float16", format, err)
	}
	if message, other := cacheRows(t, db); message != 0 || other != 1 {
		t.Fatalf("cache: %d message rows left, %d others (want 0 and 1)", message, other)
	}
	if cleanupMarker(t, db) != 1 {
		t.Fatal("no completion marker after a finished cleanup")
	}
	// a second open changes nothing more
	if _, err := db.DB.Exec(`INSERT INTO memory_embed_cache(cache_key, vec, created_at, kind) VALUES('k2', X'00003c00', 't', 'message')`); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(db); err != nil {
		t.Fatal(err)
	}
	if message, _ := cacheRows(t, db); message != 1 {
		t.Fatalf("the migration ran again: %d", message)
	}
}

// The format column exists as soon as ALTER TABLE has run, long before the delete: a delete that fails, or
// a process that stops between the two, must not count as a finished cleanup. Every Open repeats it until
// the marker exists, then never again; vectors and other cache kinds stay.
func TestOpenRetriesTheMessageCacheCleanupUntilItCompleted(t *testing.T) {
	db := legacyMessageVectorStore(t)
	// the delete fails after the column was added (a lock, an I/O error)
	if _, err := db.DB.Exec(`CREATE TRIGGER fail_message_cache_delete BEFORE DELETE ON memory_embed_cache WHEN old.kind='message' BEGIN SELECT RAISE(ABORT, 'injected'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(db); err == nil {
		t.Fatal("Open succeeded although the cache cleanup failed")
	}
	if !db.HasColumn("memory_message_vectors", "fmt") {
		t.Fatal("the format column was not added before the failure")
	}
	if cleanupMarker(t, db) != 0 {
		t.Fatal("marker written although the delete failed")
	}
	if message, other := cacheRows(t, db); message != 1 || other != 1 {
		t.Fatalf("after the failed delete: %d message rows, %d others (want 1 and 1)", message, other)
	}
	// the column is there now, which must not be taken for a finished cleanup
	if _, err := db.DB.Exec(`DROP TRIGGER fail_message_cache_delete`); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(db); err != nil {
		t.Fatal(err)
	}
	if message, other := cacheRows(t, db); message != 0 || other != 1 {
		t.Fatalf("after the retry: %d message rows, %d others (want 0 and 1)", message, other)
	}
	if cleanupMarker(t, db) != 1 {
		t.Fatal("no marker after the retry")
	}
	var vectors int
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM memory_message_vectors`).Scan(&vectors); err != nil || vectors != 1 {
		t.Fatalf("message vectors: %d (%v), want the one that was there", vectors, err)
	}
	// the third open does nothing: a row of that kind written now survives
	if _, err := db.DB.Exec(`INSERT INTO memory_embed_cache(cache_key, vec, created_at, kind) VALUES('k3', X'00003c00', 't', 'message')`); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(db); err != nil {
		t.Fatal(err)
	}
	if message, _ := cacheRows(t, db); message != 1 {
		t.Fatalf("the third open deleted again: %d message rows", message)
	}
}

// TestPendingMessageQueryWalksLeavesFirst pins the join order of the pending query: leaves outside and
// each message found by its primary key. With the messages outside, the query walked every closed leaf
// for every message (49 s at 10,000 messages) while holding a connection the whole bot shares.
func TestPendingMessageQueryWalksLeavesFirst(t *testing.T) {
	st := openStore(t)
	old := time.Now().Add(-48 * time.Hour)
	for i := 0; i < 3; i++ {
		seedMsgs(t, st, fmt.Sprintf("discord:1:%d", 10+i), 40, old)
	}
	if _, err := st.DB.DB.Exec(`ANALYZE`); err != nil { // the plan a long-running database gets
		t.Fatal(err)
	}
	for _, rooms := range [][]string{nil, {msgRoom}} {
		q, args := pendingMessageQuery(rooms, msgSpace, []string{leaves.DefaultRecipe().Version()})
		rows, err := st.DB.DB.Query(`EXPLAIN QUERY PLAN `+q, args...)
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		rows.Close()
		if len(plan) == 0 || !strings.HasPrefix(plan[0], "SCAN memory_leaves") && !strings.HasPrefix(plan[0], "SEARCH memory_leaves") {
			t.Fatalf("rooms %v: the leaves must be the outer loop, plan %q", rooms, plan)
		}
		found := false
		for _, d := range plan {
			if strings.HasPrefix(d, "SEARCH o USING ") && strings.Contains(d, "observation_id=?") {
				found = true
			}
		}
		if !found {
			t.Fatalf("rooms %v: each message must be looked up by its key, plan %q", rooms, plan)
		}
	}
}
