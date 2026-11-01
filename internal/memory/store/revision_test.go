package store

import (
	"strings"
	"testing"
	"time"

	"masamong/internal/memory/corpus"
	"masamong/internal/memory/leaves"
)

func TestEditAndDeleteFollowTheAuthor(t *testing.T) {
	st := openStore(t)
	at := time.Now().Add(-2 * time.Hour)
	var batch []corpus.Observation
	for i := 0; i < 3; i++ {
		batch = append(batch, mk(i, at.Add(time.Duration(i)*time.Minute)))
	}
	if _, err := st.InsertObservations(batch, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendChatTurns("1:2", []ChatTurn{{Role: "user", Speaker: "철수", Content: "말 a", NativeID: "ma"}, {Role: "assistant", Content: "응"}}, 40); err != nil {
		t.Fatal(err)
	}

	rev, ok, err := st.EditMessage("discord", "ma", "고친 말", time.Now().UTC().Format(time.RFC3339))
	if err != nil || !ok || rev.Room != "discord:1:2" || rev.ObservationID != "oa" {
		t.Fatalf("edit: %+v %v %v", rev, ok, err)
	}
	if _, ok, _ := st.EditMessage("discord", "ma", "고친 말", ""); ok {
		t.Fatal("the same text again is not a change")
	}
	if _, ok, _ := st.EditMessage("discord", "unknown", "x", ""); ok {
		t.Fatal("a message that was never stored is not a change")
	}
	msgs, _ := st.Messages([]string{"discord:1:2"})
	if msgs[0].Content != "고친 말" {
		t.Fatalf("edited text not stored: %q", msgs[0].Content)
	}
	turns, _ := st.RecentChatTurns("1:2", 10)
	if turns[0].Content != "고친 말" {
		t.Fatalf("the chat turn keeps the old text: %+v", turns)
	}

	revs, err := st.DeleteMessages("discord", []string{"ma", "mb", "never"}, time.Now().UTC().Format(time.RFC3339))
	if err != nil || len(revs) != 2 || !revs[0].Deleted {
		t.Fatalf("delete: %+v %v", revs, err)
	}
	msgs, _ = st.Messages([]string{"discord:1:2"})
	if len(msgs) != 1 || msgs[0].ObservationID != "oc" {
		t.Fatalf("deleted messages still read: %+v", msgs)
	}
	var body string
	if err := st.DB.DB.QueryRow(`SELECT content FROM memory_observations WHERE observation_id='oa'`).Scan(&body); err != nil || body != "" {
		t.Fatalf("a tombstone keeps no text: %q %v", body, err)
	}
	if turns, _ = st.RecentChatTurns("1:2", 10); len(turns) != 1 || turns[0].Role != "assistant" {
		t.Fatalf("the deleted message's chat turn must go: %+v", turns)
	}
	if hits, _ := st.SearchMessages([]string{"discord:1:2"}, "말", 10); len(hits) != 1 {
		t.Fatalf("raw search shows deleted messages: %+v", hits)
	}
	rooms, _ := st.Rooms()
	if rooms[0].Messages != 1 {
		t.Fatalf("counts include deleted messages: %+v", rooms)
	}
	// an archive import of the same channel later cannot bring the text back
	again := mk(0, at)
	again.ObservationID = "archive-derivation"
	if r, _ := st.InsertObservations([]corpus.Observation{again}, nil); r.Inserted != 0 {
		t.Fatal("a deleted message came back through an import")
	}
	if _, ok, _ := st.EditMessage("discord", "ma", "부활", ""); ok {
		t.Fatal("an edit event after the deletion must not restore the text")
	}
	// an edit to empty text is a deletion
	if rev, ok, err := st.EditMessage("discord", "mc", "  ", ""); err != nil || !ok || !rev.Deleted {
		t.Fatalf("empty edit: %+v %v %v", rev, ok, err)
	}
	if id, err := st.LatestNativeID("discord:1:2"); err != nil || id != "mc" {
		t.Fatalf("latest native id counts tombstones: %q %v", id, err)
	}
}

func TestWithdrawalRemovesImportedAccountWithoutNativeMessageID(t *testing.T) {
	st := openStore(t)
	o := mk(0, time.Now())
	o.NativeMessageID = ""
	o.ActorAccount = "alice"
	if _, err := st.InsertObservations([]corpus.Observation{o}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SyncLeaves(o.SourceRoom, leaves.DefaultRecipe(), time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	revs, err := st.DeleteAccountMessages("discord", "alice", time.Now().Format(time.RFC3339))
	if err != nil || len(revs) != 1 {
		t.Fatalf("%+v %v", revs, err)
	}
	var content string
	_ = st.DB.DB.QueryRow(`SELECT content FROM memory_observations WHERE observation_id=?`, o.ObservationID).Scan(&content)
	if content != "" {
		t.Fatal("imported account text survived withdrawal")
	}
	var n int
	_ = st.DB.DB.QueryRow(`SELECT COUNT(*) FROM memory_leaves WHERE body<>''`).Scan(&n)
	if n != 0 {
		t.Fatal("leaf copy survived withdrawal")
	}
}

func TestLatestNativeIDOrdersSnowflakesNumerically(t *testing.T) {
	st := openStore(t)
	a, b := mk(0, time.Now()), mk(1, time.Now())
	a.NativeMessageID, b.NativeMessageID = "999", "1000"
	if _, err := st.InsertObservations([]corpus.Observation{a, b}, nil); err != nil {
		t.Fatal(err)
	}
	if id, _ := st.LatestNativeID("discord:1:2"); id != "1000" {
		t.Fatalf("got %q", id)
	}
	if id, _ := st.LatestNativeID("discord:1:3"); id != "" {
		t.Fatalf("empty room: %q", id)
	}
}

func TestResyncAfterDeleteRecutsLeaves(t *testing.T) {
	st := openStore(t)
	at := time.Now().Add(-3 * time.Hour)
	var batch []corpus.Observation
	for i := 0; i < 4; i++ {
		batch = append(batch, mk(i, at.Add(time.Duration(i)*time.Minute)))
	}
	if _, err := st.InsertObservations(batch, nil); err != nil {
		t.Fatal(err)
	}
	r := leaves.DefaultRecipe()
	if _, err := st.SyncLeaves("discord:1:2", r, time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeleteMessages("discord", []string{"mb"}, ""); err != nil {
		t.Fatal(err)
	}
	sync, err := st.SyncLeaves("discord:1:2", r, time.Now(), nil)
	if err != nil || sync.Changed != 1 {
		t.Fatalf("%+v %v", sync, err)
	}
	var body string
	_ = st.DB.DB.QueryRow(`SELECT body FROM memory_leaves WHERE room='discord:1:2'`).Scan(&body)
	if body == "" || strings.Contains(body, "말 b") {
		t.Fatalf("leaf still holds the deleted text: %q", body)
	}
}
