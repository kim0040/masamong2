package application

import (
	"fmt"
	"sort"
	"time"

	"masamong/internal/action"
	"masamong/internal/errcode"
	"masamong/internal/storage"
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
	stamp := at.UTC().Format(time.RFC3339Nano)
	// Every observation counts, but the row keeps the newest one's name and time: messages can arrive
	// out of order (a relayed or retried one lands after a later one), and arrival order is not time.
	q := `INSERT INTO activity_counts(day, guild_id, channel_id, user_id, name, n, last_at) VALUES(?,?,?,?,?,1,?)
		ON CONFLICT(day, guild_id, channel_id, user_id) DO UPDATE SET n=activity_counts.n+1,
		name=CASE WHEN excluded.last_at>=activity_counts.last_at THEN excluded.name ELSE activity_counts.name END,
		last_at=MAX(activity_counts.last_at, excluded.last_at)`
	if !db.IsSQLite() {
		q = `INSERT INTO activity_counts(day, guild_id, channel_id, user_id, name, n, last_at) VALUES(?,?,?,?,?,1,?)
		ON DUPLICATE KEY UPDATE n=n+1,
		name=IF(VALUES(last_at)>=last_at, VALUES(name), name),
		last_at=GREATEST(last_at, VALUES(last_at))`
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
		return s.failErr(req, errcode.IO, "error.storage", err, nil)
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
	rows, err := db.DB.Query(`SELECT user_id, SUM(n), MAX(last_at) FROM activity_counts WHERE `+where+` GROUP BY user_id`, args...)
	if err != nil {
		return s.failErr(req, errcode.IO, "error.storage", err, nil)
	}
	defer rows.Close()
	// Read every row and close the cursor before MemoryAllowed: on SQLite the writer and reader share one
	// connection, and a cold opt-out cache queries it again, so checking inside the loop waits forever.
	type ranked struct {
		id, name, last string
		n              int
	}
	var found []ranked
	for rows.Next() {
		var r ranked
		if rows.Scan(&r.id, &r.n, &r.last) != nil {
			continue
		}
		found = append(found, r)
	}
	_ = rows.Close()
	// The total and the head count come from the same set as the list: someone who withdrew after being
	// counted is in neither (their rows are deleted on withdrawal; this covers the moments before that finishes).
	counted := found[:0]
	total := 0
	for _, r := range found {
		if !s.MemoryAllowed(r.id) {
			continue
		}
		counted = append(counted, r)
		total += r.n
	}
	people := len(counted)
	sort.Slice(counted, func(i, j int) bool {
		a, b := counted[i], counted[j]
		if a.n != b.n {
			return a.n > b.n
		}
		if a.last != b.last {
			return a.last > b.last
		}
		return a.id < b.id
	})
	if len(counted) > 10 {
		counted = counted[:10]
	}
	top := []map[string]any{}
	for _, r := range counted {
		share := 0.0
		if total > 0 {
			share = float64(r.n) * 100 / float64(total)
		}
		top = append(top, map[string]any{"rank": len(top) + 1, "name": s.activityName(db, r.id, where, args), "count": r.n, "share": fmt.Sprintf("%.1f", share)})
	}
	status := "ok"
	if len(top) == 0 {
		status = "no_activity"
	}
	return action.OK(req, map[string]any{"status": status, "period": period, "from": from, "total_messages": total, "people": people, "top": top})
}

// activityName is the name a person used last in the period: the row with the latest last_at, not the
// alphabetically greatest name. where and args are the ranking's filter (channel and period).
func (s *Service) activityName(db *storage.Store, userID, where string, args []any) string {
	var name string
	q := `SELECT name FROM activity_counts WHERE ` + where + ` AND user_id=? ORDER BY last_at DESC, day DESC LIMIT 1`
	if err := db.DB.QueryRow(q, append(append([]any{}, args...), userID)...).Scan(&name); err != nil {
		return ""
	}
	return name
}
