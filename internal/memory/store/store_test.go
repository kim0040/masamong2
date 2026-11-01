package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"masamong/internal/memory/corpus"
	"masamong/internal/memory/leaves"
	"masamong/internal/storage"
)

func openStore(t *testing.T) *Store {
	t.Helper()
	db, err := storage.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(db); err != nil { // idempotent migration
		t.Fatal(err)
	}
	return st
}

func mk(i int, at time.Time) corpus.Observation {
	return corpus.Observation{ObservationID: "o" + string(rune('a'+i)), Platform: "discord", Collection: "d", SourceRoom: "discord:1:2",
		NativeMessageID: "m" + string(rune('a'+i)), ActorLabel: "철수", Content: "말 " + string(rune('a'+i)), ParsedTime: at.UTC().Format(time.RFC3339),
		RecordKind: corpus.HumanText}
}

func TestInsertDedupByIDAndNativeMessage(t *testing.T) {
	st := openStore(t)
	o := mk(0, time.Now())
	r, err := st.InsertObservations([]corpus.Observation{o, o}, nil)
	if err != nil || r.Inserted != 1 || r.Duplicate != 1 {
		t.Fatalf("%+v %v", r, err)
	}
	live := o
	live.ObservationID = "different-derivation"
	r, _ = st.InsertObservations([]corpus.Observation{live}, nil)
	if r.Inserted != 0 {
		t.Fatal("same Discord message from another source must not be stored twice")
	}
	marker := mk(1, time.Now())
	marker.RecordKind = corpus.DeleteMarker
	r, _ = st.InsertObservations([]corpus.Observation{marker}, nil)
	if r.Skipped != 1 {
		t.Fatal("markers are not memory")
	}
}

func TestSearchMessagesScopesRanksAndEscapesWildcards(t *testing.T) {
	st := openStore(t)
	now := time.Now()
	first := mk(0, now.Add(-time.Minute))
	first.Content = "텐트는 민수가 챙겨. 텐트 확인 완료"
	first.ActorAccount = "private-platform-id"
	second := mk(1, now)
	second.Content = "텐트는 창고에 있어"
	other := mk(2, now)
	other.SourceRoom = "discord:1:other"
	other.Content = "텐트 텐트 텐트"
	literal := mk(3, now.Add(time.Minute))
	literal.Content = "진행률은 50%이고 under_score도 확인"
	if _, err := st.InsertObservations([]corpus.Observation{first, second, other, literal}, nil); err != nil {
		t.Fatal(err)
	}
	hits, err := st.SearchMessages([]string{"discord:1:2"}, "텐트", 10)
	if err != nil || len(hits) != 2 || hits[0].Message.ObservationID != first.ObservationID {
		t.Fatalf("scoped ranked search: %+v %v", hits, err)
	}
	if hits[0].Message.ActorAccount != "" {
		t.Fatal("raw search must not expose actor account IDs")
	}
	for _, q := range []string{"%", "_"} {
		hits, err = st.SearchMessages([]string{"discord:1:2"}, q, 10)
		if err != nil || len(hits) != 1 || hits[0].Message.ObservationID != literal.ObservationID {
			t.Fatalf("literal wildcard %q: %+v %v", q, hits, err)
		}
	}
}

func TestSyncLeavesStableIDsAndChanges(t *testing.T) {
	st := openStore(t)
	old := time.Now().Add(-24 * time.Hour)
	var batch []corpus.Observation
	for i := 0; i < 10; i++ {
		batch = append(batch, mk(i, old.Add(time.Duration(i)*time.Minute)))
	}
	if _, err := st.InsertObservations(batch, nil); err != nil {
		t.Fatal(err)
	}
	r := leaves.DefaultRecipe()
	s1, err := st.SyncLeaves("discord:1:2", r, time.Now(), nil)
	if err != nil || s1.Leaves != 2 || s1.New != 2 || s1.Closed != 2 {
		t.Fatalf("%+v %v", s1, err)
	}
	s2, _ := st.SyncLeaves("discord:1:2", r, time.Now(), nil)
	if s2.New != 0 || s2.Changed != 0 || s2.Removed != 0 {
		t.Fatalf("resync must be a no-op: %+v", s2)
	}
	// one more message extends the tail leaf (same first message → same ID, new body)
	if _, err := st.InsertObservations([]corpus.Observation{mk(10, old.Add(11*time.Minute))}, nil); err != nil {
		t.Fatal(err)
	}
	s3, _ := st.SyncLeaves("discord:1:2", r, time.Now(), nil)
	if s3.New != 0 || s3.Changed != 1 {
		t.Fatalf("tail extension: %+v", s3)
	}
	p, _ := st.Pending([]string{"discord:1:2"}, "space", false, 0)
	if len(p) != 2 {
		t.Fatalf("pending %d", len(p))
	}
	if err := st.SaveLeafVector(p[0].LeafID, "space", []byte{0, 0}); err != nil {
		t.Fatal(err)
	}
	p, _ = st.Pending([]string{"discord:1:2"}, "space", false, 0)
	if len(p) != 1 {
		t.Fatalf("pending after save %d", len(p))
	}
}

func TestChatTurnsTrimmed(t *testing.T) {
	st := openStore(t)
	for i := 0; i < 5; i++ {
		if err := st.AppendChatTurns("c", []ChatTurn{{Role: "user", Content: "q"}, {Role: "assistant", Content: "a"}}, 4); err != nil {
			t.Fatal(err)
		}
	}
	turns, _ := st.RecentChatTurns("c", 100)
	if len(turns) != 4 || turns[3].Role != "assistant" {
		t.Fatalf("%+v", turns)
	}
}

func TestRoomSpansRenameAndDelete(t *testing.T) {
	st := openStore(t)
	kakao := func(id, room, who, text, at string) corpus.Observation {
		return corpus.Observation{ObservationID: id, Platform: "kakao", Collection: "k", SourceRoom: room, ActorLabel: who,
			Content: text, ParsedTime: at, RecordKind: corpus.HumanText}
	}
	obs := []corpus.Observation{
		kakao("o1", "kakao:a", "민지", "안녕", "2024-01-01T10:00:00"),
		kakao("o2", "kakao:a", "철수", "반가워", "2024-03-02T11:00:00"),
		kakao("o3", "kakao:b", "영희", "다른 방", "2024-02-01T09:00:00"),
	}
	if _, err := st.InsertObservations(obs, nil); err != nil {
		t.Fatal(err)
	}
	spans, err := st.RoomSpans()
	if err != nil {
		t.Fatal(err)
	}
	if sp := spans["kakao:a"]; sp.Speakers != 2 || !strings.HasPrefix(sp.First, "2024-01-01") || !strings.HasPrefix(sp.Last, "2024-03-02") {
		t.Fatalf("span %+v", sp)
	}
	if err := st.SetRoomLabel("kakao:a", "동아리 방"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetRoomLabel("kakao:none", "x"); !errors.Is(err, ErrNoRoom) {
		t.Fatalf("unknown room: %v", err)
	}
	rooms, _ := st.Rooms()
	for _, r := range rooms {
		if r.Room.Room == "kakao:a" && r.Label != "동아리 방" {
			t.Fatalf("label %q", r.Label)
		}
	}
	n, err := st.DeleteRoom("kakao:a")
	if err != nil || n != 2 {
		t.Fatalf("delete %d %v", n, err)
	}
	rooms, _ = st.Rooms()
	if len(rooms) != 1 || rooms[0].Room.Room != "kakao:b" {
		t.Fatalf("other rooms must stay: %+v", rooms)
	}
	if _, err := st.DeleteRoom("kakao:a"); !errors.Is(err, ErrNoRoom) {
		t.Fatalf("second delete: %v", err)
	}
	// importing the same file again restores the room (deduplication does not remember deleted rows)
	if res, err := st.InsertObservations(obs[:2], nil); err != nil || res.Inserted != 2 {
		t.Fatalf("reimport %+v %v", res, err)
	}
}

// A room whose last messages are recent keeps its tail leaf open (not embedded until it closes); the
// room's stats say so, so the tail does not count against the room being ready.
func TestRoomsCountOpenTailLeaves(t *testing.T) {
	st := openStore(t)
	recent := time.Now().Add(-5 * time.Minute)
	var batch []corpus.Observation
	for i := 0; i < 10; i++ {
		batch = append(batch, mk(i, recent.Add(time.Duration(i)*time.Second)))
	}
	if _, err := st.InsertObservations(batch, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SyncLeaves("discord:1:2", leaves.DefaultRecipe(), time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	rooms, err := st.Rooms()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rooms {
		if r.Room.Room == "discord:1:2" {
			if r.Leaves != 2 || r.Open != 1 {
				t.Fatalf("leaves %d open %d, want 2 and 1 (a full leaf, then an open tail)", r.Leaves, r.Open)
			}
			return
		}
	}
	t.Fatal("room not listed")
}

// A Kakao export is finished: its tail leaf closes at once even when the last messages are recent, so
// they are embedded and searchable (a Discord channel keeps the tail open until it goes quiet).
func TestKakaoExportClosesItsTail(t *testing.T) {
	st := openStore(t)
	recent := time.Now().Add(-5 * time.Minute)
	var batch []corpus.Observation
	for i := 0; i < 10; i++ {
		o := mk(i, recent.Add(time.Duration(i)*time.Second))
		o.Platform, o.Collection, o.SourceRoom, o.NativeMessageID = "kakao", "k", "kakao:room", "k"+o.NativeMessageID
		o.ObservationID = "k" + o.ObservationID
		batch = append(batch, o)
	}
	if _, err := st.InsertObservations(batch, nil); err != nil {
		t.Fatal(err)
	}
	s, err := st.SyncLeaves("kakao:room", leaves.DefaultRecipe(), time.Now(), nil)
	if err != nil || s.Leaves != 2 || s.Closed != 2 {
		t.Fatalf("%+v %v, want 2 leaves both closed", s, err)
	}
}
