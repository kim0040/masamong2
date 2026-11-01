package tools

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"masamong/internal/storage"
)

// Usage counts tool calls per day in the instance DB, so limits survive restarts. Users are stored
// as a salted hash (never the platform ID); channels as their scope key.
type Usage struct {
	db *storage.Store
}

const usageDDL = `CREATE TABLE IF NOT EXISTS tool_usage (
	day VARCHAR(10) NOT NULL,
	tool VARCHAR(128) NOT NULL,
	scope VARCHAR(16) NOT NULL,
	subject VARCHAR(191) NOT NULL,
	n INTEGER NOT NULL,
	last_at VARCHAR(40) NOT NULL,
	PRIMARY KEY(day, tool, scope, subject)
)`

// OpenUsage creates the table if needed.
func OpenUsage(db *storage.Store) (*Usage, error) {
	if db == nil {
		return nil, errors.New("tools: no store")
	}
	if _, err := db.DB.Exec(usageDDL); err != nil {
		return nil, fmt.Errorf("tools: usage table: %w", err)
	}
	return &Usage{db: db}, nil
}

// Caller identifies who is using a tool. Empty fields are unknown (CLI/GUI operator calls).
type Caller struct {
	UserID  string // platform user ID; hashed before storage
	Channel string // scope key, e.g. discord:<guild>:<channel>
}

// Decision is the outcome of Reserve.
type Decision struct {
	OK         bool          `json:"ok"`
	Scope      string        `json:"scope,omitempty"` // per_day | per_user_per_day | per_channel_per_day | cooldown
	Used       int           `json:"used,omitempty"`
	Limit      int           `json:"limit,omitempty"`
	RetryAfter time.Duration `json:"retry_after,omitempty"`
}

func userKey(id string) string {
	sum := sha256.Sum256([]byte("masamong-tool-usage:" + id))
	return hex.EncodeToString(sum[:8])
}

type row struct {
	scope, subject string
	n              int
	last           time.Time
}

// Reservation is one of the quotas taken together by ReserveBatch.
type Reservation struct {
	Tool   string
	Limits Limits
	Caller Caller
}

// Reserve checks lim for tool and counts every known scope when allowed. day is the caller's
// calendar day (YYYY-MM-DD in the instance zone). Refused calls are not counted.
func (u *Usage) Reserve(day string, now time.Time, tool string, lim Limits, c Caller) (Decision, error) {
	_, dec, err := u.ReserveBatch(day, now, []Reservation{{Tool: tool, Limits: lim, Caller: c}})
	return dec, err
}

// ReserveBatch counts every reservation atomically, or none when any quota refuses.
func (u *Usage) ReserveBatch(day string, now time.Time, reservations []Reservation) (string, Decision, error) {
	var refused string
	dec := Decision{OK: true}
	denied := errors.New("quota refused")
	err := u.db.WithWriter(func(tx *sql.Tx) error {
		for _, r := range reservations {
			next, err := u.reserveTx(tx, day, now, r.Tool, r.Limits, r.Caller)
			if err != nil {
				return err
			}
			if !next.OK {
				refused, dec = r.Tool, next
				return denied
			}
		}
		return nil
	})
	if errors.Is(err, denied) {
		err = nil
	}
	return refused, dec, err
}

func (u *Usage) reserveTx(tx *sql.Tx, day string, now time.Time, tool string, lim Limits, c Caller) (Decision, error) {
	rows := []*row{{scope: "all", subject: ""}}
	var user, channel *row
	if c.UserID != "" {
		user = &row{scope: "user", subject: userKey(c.UserID)}
		rows = append(rows, user)
	}
	if c.Channel != "" {
		channel = &row{scope: "channel", subject: c.Channel}
		rows = append(rows, channel)
	}
	var dec Decision
	// One read of every counter the call checks and one write of them all: a quota check costs the same
	// few round trips however many scopes apply. On TiDB, concurrent calls lock the counters they read
	// (FOR UPDATE, always the same keys in the same order) so two of them cannot both take the last
	// allowed call; SQLite's single writer already serializes them.
	// full primary keys, so TiDB reads them as point gets and locks even the ones not written yet today
	var args []any
	for _, r := range rows {
		args = append(args, day, tool, r.scope, r.subject)
	}
	sel := `SELECT scope, subject, n, last_at FROM tool_usage WHERE (day, tool, scope, subject) IN (` +
		strings.TrimSuffix(strings.Repeat("(?,?,?,?),", len(rows)), ",") + `)`
	if !u.db.IsSQLite() {
		sel += ` FOR UPDATE`
	}
	err := func() error {
		rs, err := tx.Query(sel, args...)
		if err != nil {
			return err
		}
		for rs.Next() {
			var scope, subject, last string
			var n int
			if err := rs.Scan(&scope, &subject, &n, &last); err != nil {
				rs.Close()
				return err
			}
			for _, r := range rows {
				if r.scope == scope && r.subject == subject {
					r.n = n
					r.last, _ = time.Parse(time.RFC3339Nano, last)
				}
			}
		}
		rs.Close()
		if err := rs.Err(); err != nil {
			return err
		}
		switch {
		case lim.PerDay > 0 && rows[0].n >= lim.PerDay:
			dec = Decision{Scope: "per_day", Used: rows[0].n, Limit: lim.PerDay}
		case user != nil && lim.PerUserPerDay > 0 && user.n >= lim.PerUserPerDay:
			dec = Decision{Scope: "per_user_per_day", Used: user.n, Limit: lim.PerUserPerDay}
		case channel != nil && lim.PerChannelPerDay > 0 && channel.n >= lim.PerChannelPerDay:
			dec = Decision{Scope: "per_channel_per_day", Used: channel.n, Limit: lim.PerChannelPerDay}
		case user != nil && lim.CooldownSec > 0 && !user.last.IsZero() && now.Sub(user.last) < time.Duration(lim.CooldownSec)*time.Second:
			wait := time.Duration(lim.CooldownSec)*time.Second - now.Sub(user.last)
			dec = Decision{Scope: "cooldown", Limit: lim.CooldownSec, RetryAfter: wait.Round(time.Second)}
		default:
			dec = Decision{OK: true}
		}
		if !dec.OK {
			return nil
		}
		stamp := now.UTC().Format(time.RFC3339Nano)
		one := strings.TrimSuffix(strings.Repeat("(?,?,?,?,1,?),", len(rows)), ",")
		q := `INSERT INTO tool_usage(day, tool, scope, subject, n, last_at) VALUES ` + one + `
			ON CONFLICT(day, tool, scope, subject) DO UPDATE SET n=tool_usage.n+1, last_at=excluded.last_at`
		if !u.db.IsSQLite() {
			q = `INSERT INTO tool_usage(day, tool, scope, subject, n, last_at) VALUES ` + one + `
			ON DUPLICATE KEY UPDATE n=n+1, last_at=VALUES(last_at)`
		}
		ins := make([]any, 0, 5*len(rows))
		for _, r := range rows {
			ins = append(ins, day, tool, r.scope, r.subject, stamp)
		}
		_, err = tx.Exec(q, ins...)
		return err
	}()
	return dec, err
}

// Today returns the day's total calls per tool.
func (u *Usage) Today(day string) (map[string]int, error) {
	rs, err := u.db.DB.Query(`SELECT tool, n FROM tool_usage WHERE day=? AND scope='all'`, day)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := map[string]int{}
	for rs.Next() {
		var tool string
		var n int
		if err := rs.Scan(&tool, &n); err != nil {
			return nil, err
		}
		out[tool] = n
	}
	return out, rs.Err()
}

// Prune deletes counters of days before `before` (YYYY-MM-DD).
func (u *Usage) Prune(before string) error {
	return u.db.WithWriter(func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM tool_usage WHERE day < ?`, before)
		return err
	})
}

// Counter rows share the table with tool usage under their own scope (e.g. "provider" for daily
// external API calls), so they survive restarts and move with storage migrations.

// Bump counts one event of name under scope for day.
func (u *Usage) Bump(day, scope, name string, now time.Time) error {
	q := `INSERT INTO tool_usage(day, tool, scope, subject, n, last_at) VALUES(?,?,?,'',1,?)
		ON CONFLICT(day, tool, scope, subject) DO UPDATE SET n=tool_usage.n+1, last_at=excluded.last_at`
	if !u.db.IsSQLite() {
		q = `INSERT INTO tool_usage(day, tool, scope, subject, n, last_at) VALUES(?,?,?,'',1,?)
		ON DUPLICATE KEY UPDATE n=n+1, last_at=VALUES(last_at)`
	}
	_, err := u.db.DB.Exec(q, day, name, scope, now.UTC().Format(time.RFC3339Nano)) // one atomic statement
	return err
}

// Counts returns the day's counters under scope.
func (u *Usage) Counts(day, scope string) (map[string]int, error) {
	rs, err := u.db.DB.Query(`SELECT tool, n FROM tool_usage WHERE day=? AND scope=? AND subject=''`, day, scope)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := map[string]int{}
	for rs.Next() {
		var name string
		var n int
		if err := rs.Scan(&name, &n); err != nil {
			return nil, err
		}
		out[name] = n
	}
	return out, rs.Err()
}
