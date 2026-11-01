package application

import (
	"encoding/json"
	"time"

	"masamong/internal/action"
	"masamong/internal/errcode"
)

// conversation.log is the console's read-only view of conversation_log for one channel: how often the
// bot's name-call check, the follow-up window and the thread offers decided what, in the last days, and
// the most recent rows. The table holds no message text, so neither does this view. Like
// participation.mode it belongs to the console and is refused to a chat user.
//
//	{guild_id, channel_id, days?, limit?}
//	→ {days, counts: {kind: {decision: n}}, outcomes: {kind: {outcome: n}}, rows: [...]}
func (s *Service) conversationLog(req action.Request) action.Result {
	if req.Invoker != nil && req.Invoker.UserID != "" {
		return action.Fail(req, errcode.PermissionDenied, "generic.error", "the conversation log is read in the console", nil)
	}
	in := inputMap(req)
	g, c := strVal(in, "guild_id", ""), strVal(in, "channel_id", "")
	if c == "" {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "channel_id is required", nil)
	}
	db, err := s.adminDB()
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	days := min(max(intVal(in, "days", 7), 1), 90)
	limit := min(max(intVal(in, "limit", 20), 1), 200)
	since := s.Clock.Now().AddDate(0, 0, -days).UTC().Format(time.RFC3339Nano)
	counts, outcomes := map[string]map[string]int{}, map[string]map[string]int{}
	bump := func(m map[string]map[string]int, kind, key string, n int) {
		if key == "" {
			return
		}
		if m[kind] == nil {
			m[kind] = map[string]int{}
		}
		m[kind][key] += n
	}
	rows, err := db.DB.Query(`SELECT kind, decision, outcome, COUNT(*) FROM conversation_log WHERE guild_id=? AND channel_id=? AND created_at>=?
		GROUP BY kind, decision, outcome`, g, c, since)
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	for rows.Next() {
		var kind, decision, outcome string
		var n int
		if rows.Scan(&kind, &decision, &outcome, &n) == nil {
			bump(counts, kind, decision, n)
			bump(outcomes, kind, outcome, n)
		}
	}
	rows.Close()
	last := []map[string]any{}
	rows, err = db.DB.Query(`SELECT id, created_at, kind, decision, mode, score, features, outcome, outcome_at, cost_usd FROM conversation_log
		WHERE guild_id=? AND channel_id=? AND created_at>=? ORDER BY created_at DESC LIMIT ?`, g, c, since, limit)
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	for rows.Next() {
		var id, at, kind, decision, mode, feats, outcome, outcomeAt string
		var score, cost float64
		if rows.Scan(&id, &at, &kind, &decision, &mode, &score, &feats, &outcome, &outcomeAt, &cost) != nil {
			continue
		}
		var features map[string]any
		_ = json.Unmarshal([]byte(feats), &features)
		last = append(last, map[string]any{"id": id, "at": at, "kind": kind, "decision": decision, "mode": mode, "score": score,
			"features": features, "outcome": outcome, "outcome_at": outcomeAt, "cost_usd": cost})
	}
	rows.Close()
	return action.OK(req, map[string]any{"days": days, "counts": counts, "outcomes": outcomes, "rows": last})
}
