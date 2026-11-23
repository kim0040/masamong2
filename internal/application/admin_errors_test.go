package application_test

import (
	"testing"
)

// A series that cannot be read fails the Action: a chart with a missing series would pass for a quiet
// week (error-drop rule, fix-plan R04).
func TestStatsSeriesFailsWhenASeriesTableIsGone(t *testing.T) {
	svc := setupSvc(t)
	out(t, svc, "stats.series", map[string]any{"days": 3}) // healthy first, so the failure below is the table
	if _, err := svc.Store.DB.Exec(`DROP TABLE memory_spend`); err != nil {
		t.Fatal(err)
	}
	if r := exec(svc, "stats.series", map[string]any{"days": 3}); r.OK {
		t.Fatalf("stats.series answered without its cost series: %+v", r.Output)
	}
}

// The dashboard still draws when a section cannot be read, and says so instead of showing zeros.
func TestDashboardReportsAnUnreadableSection(t *testing.T) {
	svc := setupSvc(t)
	hasDBError := func() bool {
		for _, raw := range out(t, svc, "dashboard.overview", nil)["attention"].([]any) {
			if raw.(map[string]any)["key"] == "db_error" {
				return true
			}
		}
		return false
	}
	if hasDBError() {
		t.Fatal("a healthy database raises no db_error")
	}
	if _, err := svc.Store.DB.Exec(`DROP TABLE jobs`); err != nil {
		t.Fatal(err)
	}
	if !hasDBError() {
		t.Fatal("an unreadable jobs table is shown as db_error, not as no jobs")
	}
}
