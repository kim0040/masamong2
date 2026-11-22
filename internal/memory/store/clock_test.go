package store

import (
	"testing"
	"time"

	"masamong/internal/memory/corpus"
)

// R07: every stored timestamp the store writes comes from Store.Now (nil = wall clock). The fixed
// times here are synthetic and far from now, so a wall-clock write would stand out.
func TestStoredTimesComeFromTheInjectedClock(t *testing.T) {
	st := openStore(t)
	fixed := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	st.Now = func() time.Time { return fixed }

	if _, err := st.InsertObservations([]corpus.Observation{mk(0, fixed.Add(-time.Hour))}, nil); err != nil {
		t.Fatal(err)
	}
	var roomAt string
	if err := st.DB.DB.QueryRow(`SELECT updated_at FROM memory_rooms WHERE room=?`, "discord:1:2").Scan(&roomAt); err != nil {
		t.Fatal(err)
	}
	if want := fixed.Format(time.RFC3339); roomAt != want {
		t.Fatalf("room updated_at %q, want %q", roomAt, want)
	}

	if err := st.AddSpend("test", 0.001, 1); err != nil {
		t.Fatal(err)
	}
	var usd float64
	if err := st.DB.DB.QueryRow(`SELECT usd FROM memory_spend WHERE day=?`, fixed.Format("2006-01-02")).Scan(&usd); err != nil {
		t.Fatalf("spend did not land on the injected UTC day: %v", err)
	}

	later := fixed.Add(2 * time.Hour)
	st.Now = func() time.Time { return later }
	if err := st.SetRoomLabel("discord:1:2", "renamed"); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.DB.QueryRow(`SELECT updated_at FROM memory_rooms WHERE room=?`, "discord:1:2").Scan(&roomAt); err != nil {
		t.Fatal(err)
	}
	if want := later.Format(time.RFC3339); roomAt != want {
		t.Fatalf("relabeled room updated_at %q, want %q", roomAt, want)
	}
}
