package application

import (
	"strings"
	"testing"
	"time"
)

// threadRowCols and threadRow.fields name the same columns in the same order, and a row becomes the
// entry the thread table holds: the owner counts as a person in the thread.
func TestThreadRowMatchesItsColumns(t *testing.T) {
	var r threadRow
	if cols, fields := strings.Split(threadRowCols, ","), r.fields(); len(cols) != len(fields) {
		t.Fatalf("%d columns, %d scan targets", len(cols), len(fields))
	}
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	info := threadRow{guild: "g", parent: "p", owner: "o", origin: "turn", state: "open", bot: 1}.info(at)
	if info.Guild != "g" || info.Parent != "p" || !info.BotOwned || info.Owner != "o" || info.OriginTurn != "turn" || !info.Open || !info.LastAt.Equal(at) ||
		!info.participants["o"] || !info.humans["o"] {
		t.Fatalf("entry: %+v", info)
	}
	if info := (threadRow{state: "closed"}).info(at); info.Open || info.BotOwned || len(info.participants) != 0 || info.humans == nil {
		t.Fatalf("ownerless closed entry: %+v", info)
	}
}
