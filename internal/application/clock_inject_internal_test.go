package application

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/clock"
)

// R07: persisted times come from the service clock, not the wall clock. The fixed times here are
// synthetic and far from now, so a wall-clock write would stand out.
func TestJobRowsStampTheServiceClock(t *testing.T) {
	t0 := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	svc, err := Open(t.TempDir(), clock.Fixed{T: t0}, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	input, _ := json.Marshal(map[string]any{"instance": "clock-test"})
	if res := svc.Execute(context.Background(), action.Request{Action: "setup.resume", Input: input, Locale: "en"}); !res.OK {
		t.Fatalf("setup: %+v", res)
	}

	j, err := svc.Enqueue("index", "clock-job", "{}")
	if err != nil {
		t.Fatal(err)
	}
	got, err := getJob(svc.Store, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := t0.Format(time.RFC3339)
	if got.CreatedAt != want || got.UpdatedAt != want {
		t.Fatalf("inserted at created_at=%q updated_at=%q, want %q for both", got.CreatedAt, got.UpdatedAt, want)
	}

	later := t0.Add(2 * time.Hour)
	svc.Clock = clock.Fixed{T: later}
	if err := svc.updateJob(j.ID, `payload=?`, []any{`{"p":1}`}, "", "", 0); err != nil {
		t.Fatal(err)
	}
	got, err = getJob(svc.Store, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CreatedAt != want || got.UpdatedAt != later.Format(time.RFC3339) {
		t.Fatalf("updated row created_at=%q updated_at=%q, want %q/%q", got.CreatedAt, got.UpdatedAt, want, later.Format(time.RFC3339))
	}
}

func TestSetupRevisionStampsTheServiceClock(t *testing.T) {
	t0 := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	svc, err := Open(t.TempDir(), clock.Fixed{T: t0}, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	input, _ := json.Marshal(map[string]any{"instance": "clock-test"})
	if res := svc.Execute(context.Background(), action.Request{Action: "setup.resume", Input: input, Locale: "en"}); !res.OK {
		t.Fatalf("setup: %+v", res)
	}
	var at string
	if err := svc.Store.DB.QueryRow(`SELECT created_at FROM config_revisions WHERE revision=1`).Scan(&at); err != nil {
		t.Fatal(err)
	}
	if want := t0.Format(time.RFC3339); at != want {
		t.Fatalf("revision created_at %q, want %q", at, want)
	}
}

func TestLiveWindowRevisedAtStampsTheServiceClock(t *testing.T) {
	t0 := time.Date(2026, 3, 4, 5, 6, 7, 123456789, time.UTC)
	s := privacyService(t)
	s.Clock = clock.Fixed{T: t0}
	m := ObservedMessage{GuildID: "g", ChannelID: "c", MessageID: "m", AuthorID: "person", Content: "one", At: t0.Add(-time.Minute)}
	s.noteLive(m)
	if got := s.liveLines("g", "c")[0].RevisedAt; !got.IsZero() {
		t.Fatalf("a first sighting is not a revision: %v", got)
	}
	m.Content = "two"
	s.noteLive(m)
	if got := s.liveLines("g", "c")[0].RevisedAt; !got.Equal(t0) {
		t.Fatalf("a re-observed change revised at %v, want %v", got, t0)
	}
	later := t0.Add(time.Hour)
	s.Clock = clock.Fixed{T: later}
	s.liveEdit("g", "c", "m", "three")
	if got := s.liveLines("g", "c")[0].RevisedAt; !got.Equal(later) {
		t.Fatalf("an edit revised at %v, want %v", got, later)
	}
}
