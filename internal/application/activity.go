package application

import (
	"fmt"
	"time"

	"masamong/internal/action"
	"masamong/internal/errcode"
)

// Activity ranking ("수다왕"): how many messages each person sent in a channel, for /랭킹. The previous
// bot's most used command after pictures (72 times in its logs). Only counts are kept — day, server,
// channel, person, the name they last used, how many — never the messages; a person who withdrew
// their memory consent is not counted. Rows follow retention.turn_days (400 days).

const activitySQLite = `CREATE TABLE IF NOT EXISTS activity_counts (
		day TEXT NOT NULL,
		guild_id TEXT NOT NULL,
		channel_id TEXT NOT NULL,
		user_id TEXT NOT NULL,
		name TEXT NOT NULL DEFAULT '',
		n INTEGER NOT NULL DEFAULT 0,
		last_at TEXT NOT NULL,
		PRIMARY KEY (day, guild_id, channel_id, user_id)
	)`

const activityTiDB = `CREATE TABLE IF NOT EXISTS activity_counts (
		day VARCHAR(10) NOT NULL,
		guild_id VARCHAR(64) NOT NULL,
		channel_id VARCHAR(64) NOT NULL,
		user_id VARCHAR(64) NOT NULL,
		name VARCHAR(255) NOT NULL DEFAULT '',
		n INT NOT NULL DEFAULT 0,
		last_at VARCHAR(64) NOT NULL,
		PRIMARY KEY (day, guild_id, channel_id, user_id)
	)`

// countActivity adds one message of a person to today's count for its channel.
func (s *Service) countActivity(m ObservedMessage) {
	if m.FromBot || m.GuildID == "" || m.AuthorID == "" || s.practice || !s.GuildAllowed(m.GuildID) || !s.MemoryAllowed(m.AuthorID) {
		return
	}
	db, err := s.adminDB()
	if err != nil {
		return
	}
	at := m.At
	if at.IsZero() {
		at = s.Clock.Now()
	}
	day := at.In(s.instanceZone()).Format("2006-01-02")
	stamp := at.UTC().Format(time.RFC3339)
	q := `INSERT INTO activity_counts(day, guild_id, channel_id, user_id, name, n, last_at) VALUES(?,?,?,?,?,1,?)
		ON CONFLICT(day, guild_id, channel_id, user_id) DO UPDATE SET n=activity_counts.n+1, name=excluded.name, last_at=excluded.last_at`
	if !db.IsSQLite() {
		q = `INSERT INTO activity_counts(day, guild_id, channel_id, user_id, name, n, last_at) VALUES(?,?,?,?,?,1,?)
		ON DUPLICATE KEY UPDATE n=n+1, name=VALUES(name), last_at=VALUES(last_at)`
	}
	if _, err := db.DB.Exec(q, day, m.GuildID, m.ChannelID, m.AuthorID, clip(m.AuthorName, 120), stamp); err != nil {
		s.Log.Debug("activity count failed", "err", err)
	}
}

// activityPeriods are the ranking periods, local calendar: today, this week (from Monday), this month,
// everything kept.
var activityPeriods = []string{"today", "week", "month", "all"}

// activityRanking answers activity.ranking: the ten people who sent the most messages in the calling
// channel over the period, with their share. Input: period (today|week|month|all, default week),
// scope (discord:<channel>, injected), guild_id.
func (s *Service) activityRanking(req action.Request) action.Result {
	in := inputMap(req)
	period := strVal(in, "period", "week")
	valid := false
	for _, p := range activityPeriods {
		valid = valid || p == period
	}
	if !valid {
		return action.Fail(req, errcode.InvalidInput, "generic.error", fmt.Sprintf("period must be one of %v", activityPeriods), nil)
	}
	channel := ""
	if sc := strVal(in, "scope", ""); len(sc) > len("discord:") && sc[:len("discord:")] == "discord:" {
		channel = sc[len("discord:"):]
	}
	if channel == "" && req.Invoker != nil {
		channel = req.Invoker.ChannelID
	}
	if channel == "" {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "a channel is required (run it in a channel)", nil)
	}
	db, err := s.adminDB()
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	now := s.Clock.Now().In(s.instanceZone())
	from := ""
	switch period {
	case "today":
		from = now.Format("2006-01-02")
	case "week":
		back := (int(now.Weekday()) + 6) % 7 // days since Monday
		from = now.AddDate(0, 0, -back).Format("2006-01-02")
	case "month":
		from = now.Format("2006-01") + "-01"
	}
	where, args := "channel_id=?", []any{channel}
	if from != "" {
		where += " AND day>=?"
		args = append(args, from)
	}
	var total, people int
	_ = db.DB.QueryRow(`SELECT COALESCE(SUM(n),0), COUNT(DISTINCT user_id) FROM activity_counts WHERE `+where, args...).Scan(&total, &people)
	rows, err := db.DB.Query(`SELECT user_id, MAX(name), SUM(n) AS c, MAX(last_at) AS l FROM activity_counts WHERE `+where+
		` GROUP BY user_id ORDER BY c DESC, l DESC LIMIT 10`, args...)
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	defer rows.Close()
	top := []map[string]any{}
	for rows.Next() {
		var id, name, last string
		var n int
		if rows.Scan(&id, &name, &n, &last) != nil {
			continue
		}
		if !s.MemoryAllowed(id) {
			continue // withdrew after being counted
		}
		share := 0.0
		if total > 0 {
			share = float64(n) * 100 / float64(total)
		}
		top = append(top, map[string]any{"rank": len(top) + 1, "name": name, "count": n, "share": fmt.Sprintf("%.1f", share)})
	}
	status := "ok"
	if len(top) == 0 {
		status = "no_activity"
	}
	return action.OK(req, map[string]any{"status": status, "period": period, "from": from, "total_messages": total, "people": people, "top": top})
}
