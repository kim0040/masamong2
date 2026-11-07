package application

import (
	"context"
	"fmt"
	"strings"
	"time"

	"masamong/internal/memory/engine"
)

// Retention keeps a long-running instance's database from growing with every reply. Each reply writes
// a turn row (with its message, answer and evidence), a trace and tool counters, and most questions add
// a cached query embedding; none of that was ever removed. The daily usage_gc schedule prunes them by
// age (config retention.*). Memory itself (captured messages, leaves, summaries) is never pruned by age.
// The first pass also drops embedding-cache entries that duplicate a stored leaf vector.

// retentionEvery is how often the scheduler runs the pass; the first run waits retentionDelay after
// start so it never competes with start-up work.
const (
	retentionEvery   = 24 * time.Hour
	retentionDelay   = 5 * time.Minute
	pruneBatch       = 2000
	jobRetentionDays = 30
)

type pruneRule struct {
	name, table, column, extra string
	days                       string // key in config.Retention.Days()
	dayOnly                    bool   // column holds YYYY-MM-DD, not a timestamp
}

var pruneRules = []pruneRule{
	{name: "turns", table: "turn_log", column: "at", days: "turns"},
	{name: "traces", table: "traces", column: "created_at", days: "traces"},
	{name: "audit", table: "audit_log", column: "at", days: "audit"},
	{name: "channel_ops", table: "channel_ops_log", column: "created_at", days: "audit"},
	{name: "usage_detail", table: "tool_usage", column: "day", extra: "scope<>'all'", days: "usage", dayOnly: true},
	{name: "usage_totals", table: "tool_usage", column: "day", extra: "scope='all'", days: "turns", dayOnly: true},
	{name: "query_cache", table: "memory_embed_cache", column: "created_at", extra: "kind='query'", days: "query_cache"},
	// reminders that are over (and their text) go with turn details; plan offers once their time passed
	{name: "reminders", table: "reminders", column: "updated_at", extra: "status IN ('sent','cancelled','missed','failed','unknown')", days: "turn_detail"},
	{name: "reminders_stale", table: "reminders", column: "updated_at", extra: "status='proposed'", days: "usage"},
	{name: "reminder_offers", table: "reminder_offers", column: "expires_at", days: "usage"},
	{name: "participation_log", table: "participation_log", column: "created_at", days: "turn_detail"},
	{name: "activity", table: "activity_counts", column: "day", days: "turns", dayOnly: true},
	// the replies' feedback and which message was which reply, with the turn records they belong to
	{name: "feedback", table: "turn_feedback", column: "at", days: "turns"},
	{name: "reply_messages", table: "reply_messages", column: "at", days: "turn_detail"},
	// the bot's conversation context per channel is trimmed to its last turns on every reply; a channel
	// that went quiet (chat turned off, the bot left the server) keeps them until they age out here
	{name: "chat_context", table: "chat_turns", column: "created_at", days: "turn_detail"},
	// how each reply was delivered (not configurable: a fixed 90 days, see deliveryKeepDays)
	{name: "delivery_ledger", table: "delivery_ledger", column: "created_at", days: "delivery"},
	// the bot's conversation decisions (body-free reference data for tuning the experimental features)
	{name: "conversation_log", table: "conversation_log", column: "created_at", days: "turns"},
	// closed threads the bot made (an open one is kept: it is still answered)
	{name: "channel_threads", table: "channel_threads", column: "last_at", days: "turns", extra: "state<>'open'"},
	// the model and tool call ledgers (body-free), likewise a fixed 90 days
	{name: "llm_calls", table: "llm_calls", column: "created_at", days: "ledger"},
	{name: "tool_calls", table: "tool_calls", column: "created_at", days: "ledger"},
}

// runRetention deletes records older than their retention and strips old turn details. It works in
// small batches with pauses, so replies waiting for the database are never held up for long.
func (s *Service) runRetention(ctx context.Context) (map[string]int64, error) {
	if s.Store == nil {
		return nil, nil
	}
	if _, err := s.adminDB(); err != nil { // turn_log and audit_log live in the admin tables
		return nil, err
	}
	eng, err := s.MemoryEngine() // makes sure the embed cache has its kind column
	if err != nil {
		return nil, err
	}
	days := s.Doc().Retention.Days()
	days["delivery"], days["ledger"] = deliveryKeepDays, ledgerKeepDays
	now := s.Clock.Now().UTC()
	out := map[string]int64{}
	if n := s.pruneImages(); n > 0 {
		out["images"] = int64(n)
	}
	for _, r := range pruneRules {
		d := days[r.days]
		if d < 0 {
			continue
		}
		cut := now.AddDate(0, 0, -d)
		cutoff := cut.Format(time.RFC3339)
		if r.dayOnly {
			cutoff = cut.In(s.instanceZone()).Format("2006-01-02")
		}
		where := r.column + " < ?"
		if r.extra != "" {
			where += " AND " + r.extra
		}
		n, err := s.pruneDelete(ctx, r.table, where, cutoff)
		out[r.name] = n
		if err != nil {
			return out, fmt.Errorf("%s: %w", r.name, err)
		}
	}
	// Participants are retained with their thread; once retention removes a closed thread, its raw
	// participant account IDs must leave with it as well.
	if n, err := s.pruneOrphanThreadParticipants(ctx); err != nil {
		out["thread_participants_orphan"] = n
		return out, fmt.Errorf("thread_participants_orphan: %w", err)
	} else {
		out["thread_participants_orphan"] = n
	}
	if n, err := s.pruneFinishedJobs(ctx, now.AddDate(0, 0, -jobRetentionDays).Format(time.RFC3339)); err != nil {
		out["jobs"] = n
		return out, fmt.Errorf("jobs: %w", err)
	} else {
		out["jobs"] = n
	}
	// the conversation lines of a reminder that is over (or was pruned above) go with it
	n, err := s.pruneDelete(ctx, "reminder_context", `reminder_id NOT IN
		(SELECT id FROM reminders WHERE status NOT IN ('sent','cancelled','missed','failed','unknown'))`)
	if n > 0 {
		out["reminder_context"] = n
	}
	if err != nil {
		return out, fmt.Errorf("reminder_context: %w", err)
	}
	n, err = s.pruneDelete(ctx, "reminder_context", `at<>'' AND at < ?`, now.Add(-contextKeep).Format(time.RFC3339))
	if n > 0 {
		out["reminder_context_old"] = n
	}
	if err != nil {
		return out, fmt.Errorf("reminder_context_old: %w", err)
	}
	// turn rows stay for stats and reports; their full detail (message, answer, evidence) goes sooner
	if d := days["turn_detail"]; d >= 0 {
		cutoff := now.AddDate(0, 0, -d).Format(time.RFC3339)
		n, err := s.batched(ctx, func() (int64, error) {
			q := `UPDATE turn_log SET body='{}' WHERE rowid IN (SELECT rowid FROM turn_log WHERE at < ? AND body <> '{}' LIMIT ?)`
			if !s.Store.IsSQLite() {
				q = `UPDATE turn_log SET body='{}' WHERE at < ? AND body <> '{}' LIMIT ?`
			}
			res, err := s.Store.DB.ExecContext(ctx, q, cutoff, pruneBatch)
			if err != nil {
				return 0, err
			}
			return res.RowsAffected()
		})
		out["turn_detail"] = n
		if err != nil {
			return out, fmt.Errorf("turn_detail: %w", err)
		}
	}
	if s.Store.IsSQLite() {
		// Trace dependencies are shared with their turn. Retire a trace if that turn never finished
		// committing or its details have expired, before removing the turn's evidence links.
		if _, err := s.updateRowsBatched(ctx, "traces", `body='{}'`, `body<>'{}' AND COALESCE(json_extract(body,'$.turn_id'),'')<>'' AND NOT EXISTS(SELECT 1 FROM turn_log t WHERE t.id=json_extract(traces.body,'$.turn_id') AND t.body<>'{}')`); err != nil {
			return out, err
		}
		// References expire in the same pass as the rows and details that held them.
		n, err := s.batched(ctx, func() (int64, error) {
			res, err := s.Store.DB.ExecContext(ctx, `DELETE FROM evidence_links WHERE (source,holder) IN
				(SELECT links.source,links.holder FROM evidence_links links JOIN evidence_holders r ON r.id=links.holder WHERE
				(r.holder_kind IN ('turn_ev','turn_msg') AND NOT EXISTS (SELECT 1 FROM turn_log t WHERE t.id=r.holder_id AND t.body<>'{}')) OR
				(r.holder_kind='participation' AND NOT EXISTS (SELECT 1 FROM participation_log p WHERE p.id=r.holder_id)) OR
				(r.holder_kind='cache' AND NOT EXISTS (SELECT 1 FROM memory_embed_cache c WHERE c.cache_key=r.holder_id)) OR
				(r.holder_kind='lab' AND NOT EXISTS (SELECT 1 FROM lab_reviews l WHERE l.id=r.holder_id)) OR
				(r.holder_kind='review' AND NOT EXISTS (SELECT 1 FROM memory_reviews m WHERE m.leaf_id=r.holder_id)) OR
				(r.holder_kind='trace' AND NOT EXISTS (SELECT 1 FROM traces t WHERE t.id=r.holder_id)) OR
				(r.holder_kind='channel_summary' AND NOT EXISTS (SELECT 1 FROM channel_summary c WHERE 'summary:'||c.guild_id||':'||c.channel_id=r.holder_id)) OR
				(r.holder_kind='eval' AND NOT EXISTS (SELECT 1 FROM eval_runs e WHERE e.id=r.holder_id)) LIMIT ?)`, pruneBatch)
			if err != nil {
				return 0, err
			}
			return res.RowsAffected()
		})
		out["evidence_refs"] = n
		if err != nil {
			return out, fmt.Errorf("evidence_refs: %w", err)
		}
		// After the turn entries above: a chat turn stays indexed while a turn record links to it too.
		n, err = eng.Store.PruneChatHolders(ctx, pruneBatch)
		out["evidence_refs"] += n
		// Tombstones survive pending jobs, then expire with turn retention. Consent and observation
		// tombstones continue to prevent captured or imported messages from reappearing.
		if d := days["turns"]; d >= 0 && !s.Store.HasPendingEvents("capture") && !s.Store.HasPendingEvents("source-change") {
			cutoff := now.AddDate(0, 0, -d).Format(time.RFC3339)
			removed, err := s.batched(ctx, func() (int64, error) {
				res, err := s.Store.DB.ExecContext(ctx, `DELETE FROM privacy_tombstones WHERE (kind,source_id) IN (SELECT kind,source_id FROM privacy_tombstones WHERE at < ? AND NOT EXISTS (SELECT 1 FROM redaction_jobs) LIMIT ?)`, cutoff, pruneBatch)
				if err != nil {
					return 0, err
				}
				return res.RowsAffected()
			})
			out["privacy_tombstones"] = removed
			if err != nil {
				return out, err
			}
			// a settled edit means nothing once its tombstones are gone
			if _, err := s.Store.DB.ExecContext(ctx, `DELETE FROM privacy_settled WHERE NOT EXISTS (SELECT 1 FROM privacy_tombstones t WHERE t.kind IN ('source','message') AND t.source_id=privacy_settled.source_id)`); err != nil {
				return out, err
			}
		}
		if err != nil {
			return out, fmt.Errorf("evidence_refs: %w", err)
		}
	}
	// once: earlier versions kept a second copy of every leaf and summary vector in the embedding cache
	var dedup bool
	if ok, _ := s.kvGet("memory:cache_dedup", &dedup); !ok || !dedup {
		n, err := eng.Store.DropCachedDuplicates(engine.SpaceModel, func() error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(50 * time.Millisecond):
				return nil
			}
		})
		out["embed_cache_duplicates"] = n
		if err != nil {
			return out, fmt.Errorf("embed_cache_duplicates: %w", err)
		}
		_ = s.kvSet("memory:cache_dedup", true)
	}
	if s.Store.IsSQLite() {
		// keep the planner's statistics fresh as tables grow and shrink (cheap; SQLite skips unchanged tables)
		_, _ = s.Store.DB.ExecContext(ctx, `PRAGMA optimize`)
	}
	return out, nil
}

func (s *Service) pruneOrphanThreadParticipants(ctx context.Context) (int64, error) {
	return s.batched(ctx, func() (int64, error) {
		q := `DELETE FROM thread_participants WHERE rowid IN (
			SELECT p.rowid FROM thread_participants p WHERE NOT EXISTS (
				SELECT 1 FROM channel_threads t WHERE t.thread_id=p.thread_id
			) LIMIT ?)`
		if !s.Store.IsSQLite() {
			q = `DELETE FROM thread_participants WHERE NOT EXISTS (
				SELECT 1 FROM channel_threads t WHERE t.thread_id=thread_participants.thread_id
			) LIMIT ?`
		}
		res, err := s.Store.DB.ExecContext(ctx, q, pruneBatch)
		if err != nil {
			return 0, err
		}
		return res.RowsAffected()
	})
}

func (s *Service) pruneDelete(ctx context.Context, table, where string, args ...any) (int64, error) {
	return s.batched(ctx, func() (int64, error) {
		q := `DELETE FROM ` + table + ` WHERE rowid IN (SELECT rowid FROM ` + table + ` WHERE ` + where + ` LIMIT ?)`
		if !s.Store.IsSQLite() {
			q = `DELETE FROM ` + table + ` WHERE ` + where + ` LIMIT ?`
		}
		stepArgs := append(append([]any(nil), args...), pruneBatch)
		res, err := s.Store.DB.ExecContext(ctx, q, stepArgs...)
		if err != nil {
			return 0, err
		}
		return res.RowsAffected()
	})
}

func (s *Service) updateRowsBatched(ctx context.Context, table, set, where string, args ...any) (int64, error) {
	return s.batched(ctx, func() (int64, error) {
		q := `UPDATE ` + table + ` SET ` + set + ` WHERE rowid IN (SELECT rowid FROM ` + table + ` WHERE ` + where + ` LIMIT ?)`
		if !s.Store.IsSQLite() {
			q = `UPDATE ` + table + ` SET ` + set + ` WHERE ` + where + ` LIMIT ?`
		}
		stepArgs := append(append([]any(nil), args...), pruneBatch)
		res, err := s.Store.DB.ExecContext(ctx, q, stepArgs...)
		if err != nil {
			return 0, err
		}
		return res.RowsAffected()
	})
}

// pruneFinishedJobs bounds the durable job history while keeping the newest prepare job that backs
// each room's displayed status (the same set latestPrepareJobs returns). Queued and running work is
// never eligible. The kept ids are computed once, in one pass on the read pool; the walk then reads a
// batch of candidates by insertion order and deletes the ones not kept, so the writer connection is
// held only for the short delete of each batch. Age is judged by updated_at: a job created long ago
// that finished yesterday is still recent history.
func (s *Service) pruneFinishedJobs(ctx context.Context, cutoff string) (int64, error) {
	kept, err := latestPrepareJobIDs(s.Store)
	if err != nil {
		return 0, err
	}
	key := jobInsertionOrder(s.Store)
	var cursor any = int64(0)
	if !s.Store.IsSQLite() {
		cursor = ""
	}
	var total int64
	for {
		rows, err := s.Store.Reader().QueryContext(ctx, `SELECT `+key+`, id FROM jobs WHERE `+key+` > ? AND status IN (`+finishedJobStates+`) AND updated_at < ? ORDER BY `+key+` LIMIT ?`,
			cursor, cutoff, pruneBatch)
		if err != nil {
			return total, err
		}
		var ids []any
		scanned := 0
		for rows.Next() {
			var id string
			if err := rows.Scan(&cursor, &id); err != nil {
				rows.Close()
				return total, err
			}
			scanned++
			if _, keep := kept[id]; !keep {
				ids = append(ids, id)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return total, err
		}
		if len(ids) > 0 {
			// status and age are checked again: a job retried and finished after the batch was read is
			// recent history now and must stay
			res, err := s.Store.DB.ExecContext(ctx, `DELETE FROM jobs WHERE status IN (`+finishedJobStates+`) AND updated_at < ? AND id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+`)`, append([]any{cutoff}, ids...)...)
			if err != nil {
				return total, err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return total, err
			}
			total += n
		}
		if scanned < pruneBatch {
			return total, nil
		}
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// batched repeats step until it touches fewer rows than a batch, pausing between batches.
func (s *Service) batched(ctx context.Context, step func() (int64, error)) (int64, error) {
	var total int64
	for {
		n, err := step()
		total += n
		if err != nil || n < pruneBatch {
			return total, err
		}
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// runScheduledRetention is the usage_gc schedule: prune and record the outcome for the console.
func (s *Service) runScheduledRetention(ctx context.Context) scheduleState {
	started := s.Clock.Now()
	removed, err := s.runRetention(ctx)
	st := scheduleState{LastRun: started.UTC().Format(time.RFC3339), OK: err == nil,
		DurationMS: s.Clock.Now().Sub(started).Milliseconds(), Detail: map[string]any{"removed": removed}}
	if err != nil {
		st.Error = err.Error()
		s.Log.Warn("retention pass failed", "err", err)
	} else {
		var total int64
		for _, n := range removed {
			total += n
		}
		if total > 0 {
			s.Log.Info("retention pass", "removed", removed, "duration_ms", st.DurationMS)
		}
	}
	_ = s.kvSet("schedule:usage_gc", st)
	return st
}

// retentionDue reports whether the daily pass should run now.
func (s *Service) retentionDue(now, started time.Time) bool {
	if now.Sub(started) < retentionDelay {
		return false
	}
	var st scheduleState
	_, _ = s.kvGet("schedule:usage_gc", &st)
	last, err := time.Parse(time.RFC3339, st.LastRun)
	return err != nil || now.Sub(last) >= retentionEvery
}
