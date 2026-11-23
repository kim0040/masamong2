package application

import (
	"context"
	"sort"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/catalog"
	"masamong/internal/errcode"
)

// The catalog (internal/catalog), the handler table (dispatch.go) and the transports are hand-kept
// lists of the same Actions. These tests keep the catalog and the table together: a catalog ID that
// Execute does not know answers ACTION_UNKNOWN on every surface, and nothing else would notice.

// A catalog Action that can run on the practice instance (offline providers, its own data root) must
// reach a handler. An empty input is enough: a handler may refuse it (INVALID_INPUT, FEATURE_DISABLED,
// JOB_NOT_FOUND, ...), but not as an unknown Action.
func TestEveryCatalogActionReachesAHandler(t *testing.T) {
	t.Parallel()
	p, err := OpenPractice(context.Background(), t.TempDir(), "en", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	ran := 0
	for _, d := range catalog.All() {
		if d.Availability != catalog.Executable || practiceBlocked[d.ID] {
			continue // checked against the source below: they never reach a handler on the practice instance
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		res := p.Execute(ctx, action.Request{Action: d.ID, Input: []byte(`{}`)})
		cancel()
		if res.Error != nil && res.Error.Code == errcode.ActionUnknown {
			t.Errorf("catalog Action %s answers %s: the Action table has no handler for it", d.ID, errcode.ActionUnknown)
		}
		ran++
	}
	if ran < 100 {
		t.Fatalf("only %d catalog Actions ran; the catalog or the filter changed", ran)
	}
}

// The Actions the practice instance refuses (practiceBlocked) and the ones switched off in the catalog
// stop before the handler table, so running them proves nothing (and some would restart or reset a real
// instance). For those, the table (dispatch.go) must still list the ID.
func TestPracticeBlockedAndDeferredActionsAreInTheHandlerTable(t *testing.T) {
	t.Parallel()
	check := map[string]string{}
	for _, d := range catalog.All() {
		if d.Availability != catalog.Executable {
			check[d.ID] = "deferred in the catalog"
		}
	}
	for id := range practiceBlocked {
		if _, ok := catalog.Lookup(id); !ok {
			t.Errorf("practiceBlocked names %q, which is not a catalog Action", id)
			continue
		}
		check[id] = "blocked in practice mode"
	}
	ids := make([]string, 0, len(check))
	for id := range check {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if _, ok := handlerFor(id); !ok {
			t.Errorf("%s (%s) has no handler in the Action table", id, check[id])
		}
	}
}

// Every catalog Action has a handler, and every handler is for a catalog Action: an entry the catalog
// does not list can never be reached (Service.execute looks the ID up in the catalog first), so it is a
// leftover. Operator-defined HTTP tools run before the table and are not in it.
func TestActionTableMatchesTheCatalog(t *testing.T) {
	t.Parallel()
	table := buildActionTable()
	if len(table) < 100 {
		t.Fatalf("the Action table has %d entries; dispatch.go changed shape", len(table))
	}
	inCatalog := map[string]bool{}
	for _, d := range catalog.All() {
		inCatalog[d.ID] = true
		if _, ok := table[d.ID]; !ok {
			t.Errorf("catalog Action %s has no handler in the Action table", d.ID)
		}
	}
	for id, h := range table {
		if h == nil {
			t.Errorf("Action table entry %s has a nil handler", id)
		}
		if !inCatalog[id] {
			t.Errorf("Action table entry %s is not a catalog Action", id)
		}
	}
}
