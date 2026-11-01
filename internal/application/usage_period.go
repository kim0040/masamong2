package application

import (
	"time"

	"masamong/internal/action"
	"masamong/internal/errcode"
)

// usagePeriods are the windows the console's cost and limits page offers next to "today": the day
// before, and the last 7 and 30 days counting today. Days are the instance's time zone, as the daily
// limits are.
var usagePeriods = map[string]struct{ from, to int }{ // days back from today, inclusive
	"today": {0, 0}, "yesterday": {1, 1}, "7d": {6, 0}, "30d": {29, 0},
}

// usagePeriod (usage.period) totals one window: calls per tool and per provider (the counters the
// daily limits use) and spend per kind.
func (s *Service) usagePeriod(req action.Request) action.Result {
	in := inputMap(req)
	period := strVal(in, "period", "today")
	w, ok := usagePeriods[period]
	if !ok {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "period must be today, yesterday, 7d or 30d", nil)
	}
	loc := s.instanceZone()
	today := s.Clock.Now().In(loc)
	from := today.AddDate(0, 0, -w.from).Format(time.DateOnly)
	to := today.AddDate(0, 0, -w.to).Format(time.DateOnly)
	out := map[string]any{"period": period, "from": from, "to": to, "timezone": loc.String(), "days": w.from - w.to + 1}
	tools, providers, spend, calls := map[string]int{}, map[string]int{}, map[string]float64{}, map[string]int{}
	total := 0.0
	db, err := s.adminDB()
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	for scope, into := range map[string]map[string]int{"all": tools, "provider": providers} {
		rows, err := db.DB.Query(`SELECT tool, SUM(n) FROM tool_usage WHERE scope=? AND subject='' AND day>=? AND day<=? GROUP BY tool`, scope, from, to)
		if err != nil {
			continue
		}
		for rows.Next() {
			var id string
			var n int
			if rows.Scan(&id, &n) == nil {
				into[id] = n
			}
		}
		rows.Close()
	}
	if rows, err := db.DB.Query(`SELECT kind, SUM(usd), SUM(calls) FROM memory_spend WHERE day>=? AND day<=? GROUP BY kind`, from, to); err == nil {
		for rows.Next() {
			var kind string
			var usd float64
			var n int
			if rows.Scan(&kind, &usd, &n) == nil {
				spend[kind], calls[kind] = usd, n
				total += usd
			}
		}
		rows.Close()
	}
	out["tools"], out["providers"], out["spend_usd"], out["spend_calls"], out["spent_usd"] = tools, providers, spend, calls, total
	return action.OK(req, out)
}
