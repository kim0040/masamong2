package store

import (
	"database/sql"
	"errors"
	"slices"
	"testing"
)

func openBackfillStore(t *testing.T) *Store {
	t.Helper()
	st := openStore(t)
	for _, q := range PeopleBackfillDDL {
		if _, err := st.DB.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range PeopleBackfillDDL { // idempotent, as at every start
		if _, err := st.DB.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

func TestBackfillRunRoundTripAndStateCheck(t *testing.T) {
	st := openBackfillStore(t)
	db := st.DB.DB
	r := BackfillRun{ID: "pb_1", GuildID: "g1", State: BackfillPiloting, Token: "tok", Params: []byte(`{"rooms":["discord:g1:c1"]}`),
		Provider: "claude", BudgetUSD: 3, Windows: 4, CreatedAt: "2026-11-01T00:00:00Z", UpdatedAt: "2026-11-01T00:00:00Z"}
	if err := InsertBackfillRun(db, r); err != nil {
		t.Fatal(err)
	}
	got, ok, err := LoadBackfillRun(db, "pb_1")
	if err != nil || !ok || got.Token != "tok" || string(got.Params) != string(r.Params) || got.PilotDone || got.BudgetUSD != 3 {
		t.Fatalf("load: %+v %v %v", got, ok, err)
	}
	got.State, got.PilotDone, got.Pilot = BackfillPiloted, true, []byte(`{"windows":4}`)
	if err := SaveBackfillRun(db, got, BackfillPiloting); err != nil {
		t.Fatal(err)
	}
	// a writer that still thinks the run is piloting moves nothing
	stale := got
	stale.State = BackfillStopped
	if err := SaveBackfillRun(db, stale, BackfillPiloting); !errors.Is(err, ErrBackfillRunState) {
		t.Fatalf("a stale write: %v", err)
	}
	if err := SaveBackfillRun(db, BackfillRun{ID: "gone", State: BackfillDone}, BackfillPiloting); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("a missing run: %v", err)
	}
	if err := AddBackfillRunSpend(db, "pb_1", 0.25, "2026-11-01T00:01:00Z"); err != nil {
		t.Fatal(err)
	}
	// a save made from a copy read before the spend keeps the spend
	if err := SaveBackfillRun(db, got, BackfillPiloted); err != nil {
		t.Fatal(err)
	}
	got, _, _ = LoadBackfillRun(db, "pb_1")
	if got.State != BackfillPiloted || !got.PilotDone || got.SpentUSD != 0.25 || string(got.Pilot) != `{"windows":4}` {
		t.Fatalf("after save and spend: %+v", got)
	}
	if err := InsertBackfillRun(db, BackfillRun{ID: "pb_2", GuildID: "g2", State: BackfillDone, Params: []byte(`{}`), CreatedAt: "2026-11-02T00:00:00Z", UpdatedAt: "x"}); err != nil {
		t.Fatal(err)
	}
	runs, err := BackfillRuns(db, "", BackfillPiloted, BackfillDone)
	if err != nil || len(runs) != 2 || runs[0].ID != "pb_2" {
		t.Fatalf("list newest first: %+v %v", runs, err)
	}
	if runs, _ := BackfillRuns(db, "g1"); len(runs) != 1 || runs[0].ID != "pb_1" {
		t.Fatalf("one server: %+v", runs)
	}
}

func TestBackfillWindowsAndSpend(t *testing.T) {
	st := openBackfillStore(t)
	db := st.DB.DB
	ws := []BackfillWindow{
		{RunID: "pb_1", ID: "w2", Room: "discord:g1:c1", Seq: 1, Lines: 3, Obs: []string{"o3", "o4"}, State: WindowQueued, UpdatedAt: "x"},
		{RunID: "pb_1", ID: "w1", Room: "discord:g1:c1", Seq: 0, Lines: 2, Obs: []string{"o1", "o2"}, State: WindowQueued, Pilot: true, UpdatedAt: "x"},
		{RunID: "pb_1", ID: "w3", Room: "discord:g1:c2", Seq: 2, Lines: 1, Obs: []string{"o5"}, State: WindowSkipped, UpdatedAt: "x"},
	}
	if err := InsertBackfillWindows(db, ws); err != nil {
		t.Fatal(err)
	}
	all, err := BackfillWindows(db, "pb_1", false)
	if err != nil || len(all) != 3 || all[0].ID != "w1" || !slices.Equal(all[0].Obs, []string{"o1", "o2"}) || !all[0].Pilot {
		t.Fatalf("plan order: %+v %v", all, err)
	}
	if pilot, _ := BackfillWindows(db, "pb_1", true); len(pilot) != 1 || pilot[0].ID != "w1" {
		t.Fatalf("pilot only: %+v", pilot)
	}
	w := all[1]
	w.State, w.Claims, w.Rejected, w.Batch, w.CostUSD = WindowApplied, 2, 1, "b1", 0.01
	if err := SaveBackfillWindow(db, w); err != nil {
		t.Fatal(err)
	}
	if got, ok, _ := LoadBackfillWindow(db, "pb_1", "w2"); !ok || got.State != WindowApplied || got.Claims != 2 || got.Batch != "b1" {
		t.Fatalf("saved window: %+v", got)
	}
	c, err := CountBackfillWindows(db, "pb_1")
	if err != nil || c.States[WindowQueued] != 1 || c.States[WindowApplied] != 1 || c.States[WindowSkipped] != 1 || c.Claims != 2 || c.Rejected != 1 {
		t.Fatalf("counts: %+v %v", c, err)
	}
	if queued, _ := BackfillWindows(db, "pb_1", false, WindowQueued); len(queued) != 1 || queued[0].ID != "w1" {
		t.Fatalf("by state: %+v", queued)
	}
	w1 := all[0]
	w1.Result = []byte(`{"m":{}}`)
	if err := SaveBackfillWindow(db, w1); err != nil {
		t.Fatal(err)
	}
	if err := ClearBackfillResults(db, "pb_1", "y"); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := LoadBackfillWindow(db, "pb_1", "w1"); len(got.Result) != 0 {
		t.Fatalf("results kept: %s", got.Result)
	}
	for range 2 {
		if err := AddBackfillSpend(db, "2026-11-23", 0.5, 1); err != nil {
			t.Fatal(err)
		}
	}
	by, _, err := st.SpendSince("2026-11-01")
	if err != nil || by[SpendPeopleBackfill] != 1 {
		t.Fatalf("spend: %v %v", by, err)
	}
}
