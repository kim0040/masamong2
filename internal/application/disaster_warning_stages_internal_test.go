package application

import (
	"reflect"
	"testing"

	"masamong/internal/providers/kma"
)

// The decision stages of a warning check work on values alone: a later release ends its level while
// another level stays, deliveries survive only for rooms still watching and the level in force, and a
// sea area is left out of the cards unless marine is on.
func TestWarningDecisionStages(t *testing.T) {
	w := func(code, kind, level, cmd, announced string) kma.Warning {
		return kma.Warning{RegionCode: code, Region: code, Kind: kind, KindName: kind, Level: level, LevelName: level, Command: cmd, Announced: announced, Effective: "202610011200"}
	}
	current, cancelled := activeWarnings([]kma.Warning{
		w("L1", "R", "3", "발표", "202610011100"),
		w("L1", "R", "3", "해제", "202610011130"), // released after its issue
		w("L1", "R", "2", "발표", "202610011100"), // the advisory stays
		w("L2", "W", "3", "발표", "202610011100"),
		w("L3", "Q", "9", "발표", "202610011100"), // an unknown level is ignored
	})
	if got := map[string]string{"L1|R": current["L1|R"].Level, "L2|W": current["L2|W"].Level}; !reflect.DeepEqual(got, map[string]string{"L1|R": "2", "L2|W": "3"}) || len(current) != 2 {
		t.Fatalf("in force: %v", current)
	}
	if len(cancelled) != 1 || cancelled["L1|R"].Level != "3" {
		t.Fatalf("cancelled: %v", cancelled)
	}

	told := toldWatching(map[string]map[string]string{"L1|R": {"a": "3", "gone": "3"}, "L2|W": {"a": "3", "b": "2"}}, map[string]bool{"a": true, "b": true})
	if want := map[string]map[string]string{"L1|R": {"a": "3"}, "L2|W": {"a": "3", "b": "2"}}; !reflect.DeepEqual(told, want) {
		t.Fatalf("told: %v", told)
	}
	if got, want := deliveriesInForce(told, current), map[string]map[string]string{"L2|W": {"a": "3"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("kept: %v, want %v", got, want)
	}
	if got := deliveriesInForce(told, warningSnapshot{}); got != nil {
		t.Fatalf("nothing in force keeps nothing: %v", got)
	}

	sea := warningSnapshot{"S1|V": w("S1", "V", "3", "발표", "202610011100")}
	if got := warningChanges(sea, warningSnapshot{}, warningSnapshot{}, false); len(got) != 0 {
		t.Fatalf("a sea area with marine off: %v", got)
	}
	if got := warningChanges(sea, warningSnapshot{}, warningSnapshot{}, true); len(got) != 1 || got[0].change != "issued" {
		t.Fatalf("a sea area with marine on: %v", got)
	}
}
