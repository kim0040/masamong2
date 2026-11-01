package application

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/clock"
)

func spendResetTestService(t *testing.T, now time.Time) *Service {
	t.Helper()
	s := jobTestSvc(t)
	s.Clock = clock.Fixed{T: now}
	return s
}

func addSpendForTest(t *testing.T, s *Service, day, kind string, usd float64) {
	t.Helper()
	_, err := s.Store.DB.Exec(`INSERT INTO memory_spend(day, kind, usd, calls) VALUES(?,?,?,1)
		ON CONFLICT(day, kind) DO UPDATE SET usd=memory_spend.usd+excluded.usd, calls=memory_spend.calls+excluded.calls`, day, kind, usd)
	if err != nil {
		t.Fatal(err)
	}
}

func usageResetForTest(s *Service, input string) action.Result {
	return s.Execute(context.Background(), action.Request{Action: "usage.reset", Input: []byte(input), RequestID: "cli"})
}

func ledgerSpendForTest(t *testing.T, s *Service) (int, float64) {
	t.Helper()
	var rows int
	var total float64
	if err := s.Store.DB.QueryRow(`SELECT COUNT(*), COALESCE(SUM(usd),0) FROM memory_spend`).Scan(&rows, &total); err != nil {
		t.Fatal(err)
	}
	return rows, total
}

type spendRowSnapshot struct {
	Day, Kind string
	USD       float64
	Calls     int
}

func ledgerRowForTest(t *testing.T, s *Service) spendRowSnapshot {
	t.Helper()
	var row spendRowSnapshot
	if err := s.Store.DB.QueryRow(`SELECT day, kind, usd, calls FROM memory_spend`).Scan(&row.Day, &row.Kind, &row.USD, &row.Calls); err != nil {
		t.Fatal(err)
	}
	return row
}

func closeSpend(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestUsageResetReopensBudgetAndLeavesSpendRowsUntouched(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 30, 0, 0, time.UTC)
	s := spendResetTestService(t, now)
	addSpendForTest(t, s, now.Format("2006-01-02"), "chat", 11)
	rowsBefore, totalBefore := ledgerSpendForTest(t, s)
	rowBefore := ledgerRowForTest(t, s)

	if _, _, err := s.featureText(context.Background(), "response", "reply", "system", "prompt", 8, 0); !errors.Is(err, ErrChatBudget) {
		t.Fatalf("monthly budget gate before reset = %v, want ErrChatBudget", err)
	}
	reset := usageResetForTest(s, `{}`)
	if !reset.OK {
		t.Fatalf("usage.reset: %+v", reset.Error)
	}
	if reset.Output["spent_usd"] != "0.0000" || reset.Output["before_usd"] != "11.0000" || reset.Output["at"] != now.Format(time.RFC3339) {
		t.Fatalf("reset output = %+v", reset.Output)
	}
	by, spent, ok := s.monthSpend()
	if !ok || !closeSpend(spent, 0) || !closeSpend(by["chat"], 0) {
		t.Fatalf("month spend after reset = %v, %v, %v", by, spent, ok)
	}
	rowsAfter, totalAfter := ledgerSpendForTest(t, s)
	if rowsAfter != rowsBefore || !closeSpend(totalAfter, totalBefore) || ledgerRowForTest(t, s) != rowBefore {
		t.Fatalf("reset changed raw spend rows: before=(%d, %v), after=(%d, %v)", rowsBefore, totalBefore, rowsAfter, totalAfter)
	}
	if _, _, err := s.featureText(context.Background(), "response", "reply", "system", "prompt", 8, 0); errors.Is(err, ErrChatBudget) {
		t.Fatal("monthly budget gate stayed closed after reset")
	}

	addSpendForTest(t, s, now.Format("2006-01-02"), "chat", 0.25)
	if _, spent, ok := s.monthSpend(); !ok || !closeSpend(spent, 0.25) {
		t.Fatalf("post-reset spend = %v, %v", spent, ok)
	}
	undo := usageResetForTest(s, `{"undo":true}`)
	if !undo.OK {
		t.Fatalf("usage.reset undo: %+v", undo.Error)
	}
	if _, spent, ok := s.monthSpend(); !ok || !closeSpend(spent, 11.25) {
		t.Fatalf("month spend after undo = %v, %v", spent, ok)
	}
	var audited int
	if err := s.Store.DB.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action='usage.reset'`).Scan(&audited); err != nil || audited != 2 {
		t.Fatalf("usage.reset audit rows = %d (%v), want reset and undo", audited, err)
	}
}

func TestMonthSpendIgnoresPriorMonthResetAndClampsKinds(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 30, 0, 0, time.UTC)
	s := spendResetTestService(t, now)
	day := now.Format("2006-01-02")
	addSpendForTest(t, s, day, "chat", 0.25)
	addSpendForTest(t, s, day, "embed", 0.5)

	reset := spendResetSnapshot{Month: "2026-09", At: "2026-09-15T00:00:00Z", ByKind: map[string]float64{"chat": 1}, Total: 1, Day: "2026-09-15", DayTotal: 1}
	if err := s.kvSet(spendResetKey, reset); err != nil {
		t.Fatal(err)
	}
	by, spent, ok := s.monthSpend()
	if !ok || !closeSpend(spent, 0.75) || !closeSpend(by["chat"], 0.25) {
		t.Fatalf("prior-month reset affected spend: %v, %v, %v", by, spent, ok)
	}

	reset.Month = "2026-10"
	reset.At = now.Format(time.RFC3339)
	reset.ByKind = map[string]float64{"chat": 0.5, "embed": 0.1}
	reset.Total = 0.6
	reset.Day = day
	reset.DayTotal = 0.6
	if err := s.kvSet(spendResetKey, reset); err != nil {
		t.Fatal(err)
	}
	by, spent, ok = s.monthSpend()
	if !ok || !closeSpend(spent, 0.4) || !closeSpend(by["chat"], 0) || !closeSpend(by["embed"], 0.4) {
		t.Fatalf("reset clamp = %v, %v, %v", by, spent, ok)
	}
}

func TestMonthSpendIgnoresUnreadableResetSnapshot(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 30, 0, 0, time.UTC)
	s := spendResetTestService(t, now)
	addSpendForTest(t, s, now.Format("2006-01-02"), "chat", 0.25)
	if _, err := s.Store.DB.Exec(`INSERT INTO admin_state(k, v, updated_at) VALUES(?,?,?)`, spendResetKey, "{", now.Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	by, spent, ok := s.monthSpend()
	if !ok || !closeSpend(spent, 0.25) || !closeSpend(by["chat"], 0.25) {
		t.Fatalf("unreadable reset snapshot changed spend: %v, %v, %v", by, spent, ok)
	}
}

func TestDashboardUsesResetForTodayAndShowsActiveReset(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 30, 0, 0, time.UTC)
	s := spendResetTestService(t, now)
	day := now.Format("2006-01-02")
	addSpendForTest(t, s, day, "chat", 6)
	if r := usageResetForTest(s, `{}`); !r.OK {
		t.Fatalf("usage.reset: %+v", r.Error)
	}
	usage := s.Execute(context.Background(), action.Request{Action: "usage.budgets"})
	if !usage.OK {
		t.Fatalf("usage.budgets: %+v", usage.Error)
	}
	usageReset, ok := usage.Output["reset"].(map[string]any)
	if !ok || usageReset["at"] != now.Format(time.RFC3339) || !closeSpend(usageReset["before_usd"].(float64), 6) {
		t.Fatalf("usage reset metadata: %+v", usage.Output)
	}
	dash := s.Execute(context.Background(), action.Request{Action: "dashboard.overview"})
	if !dash.OK {
		t.Fatalf("dashboard.overview: %+v", dash.Error)
	}
	month := dash.Output["month"].(map[string]any)
	today := dash.Output["today"].(map[string]any)
	reset := month["reset"].(map[string]any)
	if !closeSpend(month["spent_usd"].(float64), 0) || !closeSpend(today["cost_usd"].(float64), 0) || reset["at"] != now.Format(time.RFC3339) || !closeSpend(reset["before_usd"].(float64), 6) {
		t.Fatalf("dashboard reset view: month=%v today=%v", month, today)
	}
	addSpendForTest(t, s, day, "chat", 0.75)
	dash = s.Execute(context.Background(), action.Request{Action: "dashboard.overview"})
	if !dash.OK {
		t.Fatalf("dashboard.overview: %+v", dash.Error)
	}
	month = dash.Output["month"].(map[string]any)
	today = dash.Output["today"].(map[string]any)
	if !closeSpend(month["spent_usd"].(float64), 0.75) || !closeSpend(today["cost_usd"].(float64), 0.75) {
		t.Fatalf("post-reset dashboard spend: month=%v today=%v", month, today)
	}
}
