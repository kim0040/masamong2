package store

import (
	"database/sql"
	"slices"
	"testing"
	"time"

	"masamong/internal/memory/corpus"
)

func TestNameVariants(t *testing.T) {
	for key, want := range map[string][]string{
		"민수":    {"민수"},
		"minsu": {"minsu", "mİnsu"},
		"kim":   {"kim", "kİm", "Kim", "Kİm"},
		"émile": {"émile", "Émile", "ém\u0130le", "Ém\u0130le"},
	} {
		got, ok := nameVariants(key)
		slices.Sort(got)
		slices.Sort(want)
		if !ok || !slices.Equal(got, want) {
			t.Errorf("%q: %q %t, want %q", key, got, ok, want)
		}
	}
	if _, ok := nameVariants("ééééé"); ok {
		t.Error("32 variants were not refused")
	}
}

func TestOtherPersonNamed(t *testing.T) {
	s := openStore(t)
	at := time.Now()
	put := func(i int, room, account, label string) {
		t.Helper()
		o := mk(i, at)
		o.SourceRoom, o.ActorAccount, o.ActorLabel = room, account, label
		if _, err := s.InsertObservations([]corpus.Observation{o}, nil); err != nil {
			t.Fatal(err)
		}
	}
	put(0, "discord:1:2", "a", "민수")
	put(1, "discord:1:other", "b", "민수")  // a room outside the checked set
	put(2, "discord:1:2", "c", "　Émile ") // trimmed, with a non-ASCII capital
	put(3, "discord:1:2", "d", "KIM")     // Kelvin sign: folds to "kim"
	put(4, "discord:1:2", "e", "삭제됨")
	if _, err := s.DB.DB.Exec(`UPDATE memory_observations SET deleted_at='x' WHERE actor_account='e'`); err != nil {
		t.Fatal(err)
	}
	rooms := `{"discord:1:2":true}`
	check := func(labels []string, account string, want bool) {
		t.Helper()
		tx, err := s.DB.Reader().Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		got, err := OtherPersonNamed(tx, labels, account, rooms)
		if err != nil || got != want {
			t.Errorf("%q for %s: %t %v, want %t", labels, account, got, err, want)
		}
	}
	check([]string{"민수"}, "a", false)
	check([]string{"민수"}, "z", true)
	check([]string{"émile"}, "z", true)
	check([]string{"émile"}, "c", false)
	check([]string{"kim"}, "z", true)
	check([]string{"삭제됨"}, "z", false)
	check([]string{"없는이름", "민수"}, "z", true)
	check(nil, "z", false)
	for i := range maxNameAccounts {
		put(10+i, "discord:9:9", "x"+string(rune('a'+i)), "흔한이름")
	}
	check([]string{"흔한이름"}, "z", true) // too many accounts elsewhere to check
	if _, err := s.DB.DB.Exec(`DROP INDEX idx_memory_obs_person_name`); err != nil {
		t.Fatal(err)
	}
	tx, err := s.DB.Reader().Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if got, err := OtherPersonNamed(tx, []string{"민수"}, "a", rooms); !got || err == nil {
		t.Fatalf("missing index: %t %v", got, err)
	}
}

func personNameIndexed(t testing.TB, db interface{ Reader() *sql.DB }) bool {
	t.Helper()
	var ok bool
	if err := db.Reader().QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='idx_memory_obs_person_name')`).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

// An index built over every row (before it was limited to live rows) is
// dropped and rebuilt by the background fill; no observation is written.
func TestPersonNameIndexRebuiltForLiveRows(t *testing.T) {
	s := openStore(t)
	o := mk(0, time.Now())
	o.SourceRoom, o.ActorAccount, o.ActorLabel = "discord:1:2", "a", "민수"
	if _, err := s.InsertObservations([]corpus.Observation{o}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.DB.Exec(`DROP INDEX IF EXISTS idx_memory_obs_person_name;
		CREATE INDEX idx_memory_obs_person_name ON memory_observations(` + personNameExpr + ` COLLATE NOCASE,actor_account,source_room);
		CREATE TRIGGER no_insert BEFORE INSERT ON memory_observations BEGIN SELECT RAISE(ABORT,'observation insert'); END;
		CREATE TRIGGER no_update BEFORE UPDATE ON memory_observations BEGIN SELECT RAISE(ABORT,'observation update'); END;
		CREATE TRIGGER no_delete BEFORE DELETE ON memory_observations BEGIN SELECT RAISE(ABORT,'observation delete'); END;`); err != nil {
		t.Fatal(err)
	}
	partial := func() (p bool) {
		t.Helper()
		if err := s.DB.Reader().QueryRow(`SELECT partial FROM pragma_index_list('memory_observations') WHERE name='idx_memory_obs_person_name'`).Scan(&p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if partial() {
		t.Fatal("fixture index is already partial")
	}
	for range 2 { // the second run keeps the rebuilt index
		if err := FillPersonAliases(t.Context(), s.DB); err != nil {
			t.Fatal(err)
		}
		if !partial() {
			t.Fatal("the name index still holds every row")
		}
	}
	tx, err := s.DB.Reader().Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if taken, err := OtherPersonNamed(tx, []string{"민수"}, "z", `{"discord:1:2":true}`); !taken || err != nil {
		t.Fatalf("rebuilt index: %t %v", taken, err)
	}
}
