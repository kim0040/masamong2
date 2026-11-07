package application

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"masamong/internal/memory/store"
	"masamong/internal/storage"
)

// Erasing copies of a message the bot once retrieved (edit, deletion, a person withdrawing consent).
//
// A turn the bot answered keeps references to the messages it used (observation IDs, native message IDs,
// one-way speaker references), never their text. When a source message changes, those turns, and the
// console's lab reviews, memory reviews and eval runs that copied it, have to be found and redacted.
// Scanning the whole history for every Discord edit or delete event cost seconds to minutes on a busy
// server, and held the lock chat replies need. So:
//
//   - evidence_refs is an index written when a turn is recorded: source ID -> the turn that holds it.
//     Finding what to redact is a handful of indexed lookups.
//   - the work runs in a durable queue (redaction_jobs): the event path only inserts one row. A job
//     survives a crash and is deleted when its redaction is done, so a restart cannot skip it. One
//     background worker takes all pending jobs at once, so a bulk delete is one pass.
//   - turns written before the index existed are classified once, in the background
//     (turn_log.evidence_state: 0 not looked at yet, 1 indexed, 2 text only). Rows that kept only text
//     (an older release) are the only ones found by matching text, in batches, and only they cost more.
//
// Source text needed for legacy matching is stored only in the pending job, which is deleted after
// source erasure and every derivative cleanup have completed.

var evidenceDDL = []string{
	`CREATE TABLE IF NOT EXISTS redaction_jobs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		payload TEXT NOT NULL,
		created_at TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_turn_log_evidence ON turn_log(evidence_state) WHERE evidence_state<>1`,
}

const (
	holderTurnEvidence = "turn_ev" // a turn's hits, tools or plan name the source
	holderLab          = "lab"
	holderReview       = "review"
	holderEval         = "eval"
	holderTurnMessage  = "turn_msg" // the turn answered the source message and kept its text

	evidenceUnclassified = 0
	evidenceIndexed      = 1
	evidenceLegacyText   = 2
)

// migrateEvidence adds the index tables (SQLite only, like the other tables added after TiDB was disabled).
func migrateEvidence(st *storage.Store) error {
	if !st.IsSQLite() {
		return nil
	}
	if err := store.MigratePrivacy(st); err != nil {
		return err
	}
	for _, table := range []string{"lab_reviews", "memory_reviews", "eval_runs"} {
		if !st.HasColumn(table, "evidence_state") {
			if _, err := st.DB.Exec(`ALTER TABLE ` + table + ` ADD COLUMN evidence_state INTEGER NOT NULL DEFAULT 0`); err != nil {
				return err
			}
		}
	}
	for _, q := range evidenceDDL {
		if _, err := st.DB.Exec(q); err != nil {
			return fmt.Errorf("evidence index: %w", err)
		}
	}
	return nil
}

// provenanceFields name the values in a turn's evidence that identify a source.
var provenanceFields = map[string]bool{"observation_ids": true, "visible_observation_ids": true, "leaf_observation_ids": true,
	"observation_id": true, "message_ids": true, "speaker_refs": true, "doc_id": true, "doc_ids": true, "leaf_id": true, "review_id": true, "top": true, "merged_leaf_ids": true, "message_id": true, "turn_id": true}

// textFields hold copied text: a record with one is only findable by matching that text.
var textFields = map[string]bool{"excerpt": true, "summary": true}

// evidenceOf lists the source IDs a turn body names in its hits, tools and plan, and whether any of them
// still carries copied text.
func evidenceOf(body map[string]any) (keys []string, copiedText bool) {
	seen := map[string]bool{}
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, child := range x {
				if textFields[k] {
					if str, ok := child.(string); ok && str != "" {
						copiedText = true
					}
				}
				if provenanceFields[k] {
					switch a := child.(type) {
					case string:
						if a != "" && !seen[a] {
							seen[a] = true
							keys = append(keys, a)
						}
					case []string:
						for _, str := range a {
							if str != "" && !seen[str] {
								seen[str] = true
								keys = append(keys, str)
							}
						}
					case []any:
						for _, id := range a {
							if str, ok := id.(string); ok && str != "" && !seen[str] {
								seen[str] = true
								keys = append(keys, str)
							}
						}
					}
					continue
				}
				walk(child)
			}
		case []any:
			for _, child := range x {
				walk(child)
			}
		}
	}
	for _, field := range []string{"hits", "tools", "plan", "context"} {
		walk(body[field])
	}
	return keys, copiedText
}

// indexTurn records which sources a turn holds. The turn row and its references are written together.
func indexTurnRefs(tx *sql.Tx, turnID string, keys []string, messageID string) error {
	for _, k := range keys {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO evidence_refs(source_id, holder_kind, holder_id) VALUES(?,?,?)`, k, holderTurnEvidence, turnID); err != nil {
			return err
		}
	}
	if messageID != "" {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO evidence_refs(source_id, holder_kind, holder_id) VALUES(?,?,?)`, messageID, holderTurnMessage, turnID); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// the durable queue
// ---------------------------------------------------------------------------

// redactionJob is one change of a source: a message edited or deleted, or a person who withdrew.
type redactionJob struct {
	id        int64    // its row, when it came from the queue
	Guild     string   `json:"guild,omitempty"`
	Channel   string   `json:"channel,omitempty"`
	Messages  []string `json:"messages,omitempty"` // native Discord message IDs
	Obs       []string `json:"obs,omitempty"`      // observation IDs of the stored messages
	Texts     []string `json:"texts,omitempty"`    // start of each message's text, for copies that kept only text
	User      string   `json:"user,omitempty"`     // a person who withdrew: their messages are found by account
	Leaves    []string `json:"leaves,omitempty"`
	Operation string   `json:"operation,omitempty"`
	Content   string   `json:"content,omitempty"`
	At        string   `json:"at,omitempty"`
	Ref       string   `json:"ref,omitempty"` // personRef of that person
}

// redactionState tells whether the worker goroutine exists.
type redactionState struct{ running atomic.Bool }

// kickRedaction starts the worker if it is not running. It drains the queue and ends: no idle goroutine.
func (s *Service) kickRedaction() {
	if s.practice || !s.redaction.running.CompareAndSwap(false, true) {
		return
	}
	s.ownedGo("evidence redaction", func(ctx context.Context) {
		for {
			drained := s.drainRedactions(ctx)
			s.redaction.running.Store(false)
			if !drained {
				return
			}
			// a job inserted after the last look must not wait for the next event
			if ctx.Err() != nil {
				return
			}
			pending, err := s.redactionPending()
			if err != nil {
				s.Log.Warn("evidence index: could not check pending work", "err", err)
				return
			}
			if !pending || !s.redaction.running.CompareAndSwap(false, true) {
				return
			}
		}
	})
}

// redactionPending reports whether there is a job or an unclassified turn.
func (s *Service) redactionPending() (bool, error) {
	db, err := s.adminDB()
	if err != nil {
		return false, err
	}
	for _, query := range []string{
		`SELECT 1 FROM redaction_jobs LIMIT 1`,
		`SELECT 1 FROM turn_log WHERE evidence_state=0 LIMIT 1`,
		`SELECT 1 FROM lab_reviews WHERE evidence_state=0 UNION ALL SELECT 1 FROM memory_reviews WHERE evidence_state=0 UNION ALL SELECT 1 FROM eval_runs WHERE evidence_state=0 UNION ALL SELECT 1 FROM chat_turns WHERE evidence_state=0 UNION ALL SELECT 1 FROM participation_log WHERE evidence_state=0 UNION ALL SELECT 1 FROM traces WHERE evidence_state=0 UNION ALL SELECT 1 FROM channel_summary WHERE evidence_state=0 LIMIT 1`,
	} {
		var one int
		err := db.Reader().QueryRow(query).Scan(&one)
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return false, err
		}
	}
	return false, nil
}

// DrainRedactions waits until every queued redaction is done (tests, shutdown checks, the CLI).
func (s *Service) DrainRedactions(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		pending, err := s.redactionPending()
		if err != nil {
			return fmt.Errorf("application: check redaction queue: %w", err)
		}
		if !pending && !s.redaction.running.Load() {
			return nil
		}
		if s.practice {
			s.drainRedactions(ctx)
		} else {
			s.kickRedaction()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func (s *Service) drainRedactions(ctx context.Context) bool {
	db, err := s.adminDB()
	if err != nil {
		return false
	}
	for ctx.Err() == nil {
		// turns written before the index are classified first, so no job can miss one
		if n, err := s.classifyTurns(ctx, db); err != nil {
			s.Log.Warn("evidence index: classification stopped", "err", err)
			return false
		} else if n > 0 {
			continue
		}
		if n, err := s.classifyCopies(ctx, db); err != nil {
			s.Log.Warn("evidence copy classification stopped", "err", err)
			return false
		} else if n > 0 {
			continue
		}
		rows, err := db.Reader().Query(`SELECT id, payload FROM redaction_jobs ORDER BY id LIMIT 200`)
		if err != nil {
			return false
		}
		var ids []int64
		var jobs []redactionJob
		for rows.Next() {
			var id int64
			var raw string
			var j redactionJob
			if err := rows.Scan(&id, &raw); err != nil {
				rows.Close()
				return false
			}
			if err := json.Unmarshal([]byte(raw), &j); err != nil {
				rows.Close()
				s.Log.Warn("invalid redaction intent retained", "job", id, "err", err)
				return false
			}
			j.id = id
			ids = append(ids, id)
			jobs = append(jobs, j)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return false
		}
		rows.Close()
		if len(ids) == 0 {
			return true
		}
		if err := s.processRedactions(ctx, db, jobs); err != nil {
			// the jobs stay queued and are tried again at the next event or start
			s.Log.Warn("evidence redaction failed; it is retried", "err", err)
			return false
		}
		for _, id := range ids {
			if _, err := db.DB.Exec(`DELETE FROM redaction_jobs WHERE id=?`, id); err != nil {
				return false
			}
		}
	}
	return false
}

// classifyTurns indexes a batch of turns written before evidence_refs existed and returns how many it
// looked at.
func (s *Service) classifyTurns(ctx context.Context, db *storage.Store) (int, error) {
	rows, err := db.Reader().Query(`SELECT id, body FROM turn_log WHERE evidence_state=0 LIMIT 300`)
	if err != nil {
		return 0, err
	}
	type row struct {
		id, body string
	}
	var batch []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.body); err != nil {
			rows.Close()
			return 0, err
		}
		batch = append(batch, r)
	}
	rows.Close()
	if len(batch) == 0 || ctx.Err() != nil {
		return 0, ctx.Err()
	}
	err = db.WithWriter(func(tx *sql.Tx) error {
		for _, r := range batch {
			var body map[string]any
			state := evidenceIndexed
			var keys []string
			msgID := ""
			if json.Unmarshal([]byte(r.body), &body) == nil {
				var copied bool
				keys, copied = evidenceOf(body)
				msgID, _ = body["message_id"].(string)
				if copied {
					state = evidenceLegacyText
				}
			}
			var err error
			keys, err = copySources(tx, keys)
			if err != nil {
				return err
			}
			if err := indexTurnRefs(tx, r.id, keys, msgID); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE turn_log SET evidence_state=? WHERE id=?`, state, r.id); err != nil {
				return err
			}
		}
		return nil
	})
	return len(batch), err
}

// ---------------------------------------------------------------------------
// the redaction itself
// ---------------------------------------------------------------------------

const redactChunk = 400

func inList(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

func anyOf(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// leavesHolding lists the leaves that hold any of the observations: a substring scan for a few IDs,
// one JSON pass for many.
func leavesHolding(db *storage.Store, obs []string) ([]string, error) {
	if len(obs) == 0 {
		return nil, nil
	}
	want := map[string]bool{}
	for _, o := range obs {
		want[o] = true
	}
	var rows *sql.Rows
	var err error
	if len(obs) <= 32 {
		var cond []string
		var args []any
		for _, o := range obs {
			quoted, _ := json.Marshal(o)
			cond = append(cond, `instr(observation_ids, ?)>0`)
			args = append(args, string(quoted))
		}
		rows, err = db.Reader().Query(`SELECT leaf_id, observation_ids FROM memory_leaves WHERE `+strings.Join(cond, " OR "), args...)
	} else {
		rows, err = db.Reader().Query(`SELECT leaf_id, observation_ids FROM memory_leaves`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, raw string
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		var ids []string
		_ = json.Unmarshal([]byte(raw), &ids)
		for _, o := range ids {
			if want[o] {
				out = append(out, id)
				break
			}
		}
	}
	return out, rows.Err()
}

// processRedactions redacts the copies of everything the jobs name, in one pass for all of them.
func (s *Service) processRedactions(ctx context.Context, db *storage.Store, queued []redactionJob) error {
	jobs, err := s.unfinishedJobs(db, queued)
	if err != nil {
		return err
	}
	keys := map[string]bool{}
	var texts, obs, people []string
	withdrawn := false // a batch with a withdrawal erases by person: no collected row is spared then
	type candidate struct{ guild, channel, message string }
	var candidates []candidate
	// messages only edited in this batch: their collected rows hold no text, so an edit leaves them
	editedOnly := map[string]bool{}
	erased := map[string]bool{}
	for _, j := range jobs {
		if j.Operation == "" && j.User != "" {
			j.Operation = "withdraw"
		}
		if j.Operation != "" {
			eng, err := s.MemoryEngine()
			if err != nil {
				return err
			}
			at, err := time.Parse(time.RFC3339Nano, j.At)
			if err != nil {
				at = s.Clock.Now()
			}
			switch j.Operation {
			case "delete":
				_, err = eng.DeleteContext(ctx, j.Messages, at)
			case "edit":
				if len(j.Messages) > 0 {
					_, err = eng.EditContext(ctx, j.Messages[0], j.Content, at)
				}
			case "withdraw":
				_, err = s.processWithdrawal(ctx, db, j)
			case "withdraw_batch":
				_, err = eng.DeleteObservations(ctx, j.Obs, at)
			}
			if err != nil {
				return err
			}
		}
		for _, id := range j.Leaves {
			keys[id] = true
		}
		for _, id := range j.Messages {
			keys[id] = true
			if j.Channel != "" {
				candidates = append(candidates, candidate{j.Guild, j.Channel, id})
			}
			if j.Operation == "edit" {
				editedOnly[id] = true
			} else {
				erased[id] = true
			}
		}
		if j.Operation == "withdraw" || j.Operation == "withdraw_batch" || j.User != "" || j.Ref != "" {
			withdrawn = true
		}
		for _, id := range j.Obs {
			keys[id] = true
			obs = append(obs, id)
		}
		texts = append(texts, j.Texts...)
		if j.User != "" {
			people = append(people, j.User)
		}
		if j.Ref != "" {
			keys[j.Ref] = true
		}
	}
	// a person's messages are found by account; the rows stay after the text is deleted
	for _, u := range people {
		rows, err := db.Reader().Query(`SELECT observation_id, COALESCE(native_message_id,'') FROM memory_observations WHERE platform='discord' AND actor_account=?`, u)
		if err != nil {
			return err
		}
		for rows.Next() {
			var o, n string
			if rows.Scan(&o, &n) == nil {
				keys[o] = true
				obs = append(obs, o)
				if n != "" {
					keys[n] = true
				}
			}
		}
		rows.Close()
	}
	// the lines a reminder took from a message, or from a person who withdrew, go with them
	if err := s.redactReminderContext(db, keys, people); err != nil {
		return err
	}
	// the participation log kept a message as a candidate: a collected row (an ID and features only) goes
	// with its source, the others lose their text
	for id := range erased {
		delete(editedOnly, id)
	}
	if withdrawn {
		editedOnly = nil
	}
	for _, c := range candidates {
		if !editedOnly[c.message] {
			if _, err := db.DB.Exec(`DELETE FROM participation_log WHERE mode='collect' AND guild_id=? AND channel_id=? AND message_id=?`, c.guild, c.channel, c.message); err != nil {
				s.Log.Debug("participation delete failed", "err", err.Error())
			}
		}
		if _, err := db.DB.Exec(`UPDATE participation_log SET message='',draft='' WHERE guild_id=? AND channel_id=? AND message_id=?`, c.guild, c.channel, c.message); err != nil {
			s.Log.Debug("participation redaction failed", "err", err.Error())
		}
	}
	delete(keys, "")
	if len(keys) == 0 && len(texts) == 0 {
		return nil
	}
	leaves, err := leavesHolding(db, uniqueStrings(obs))
	if err != nil {
		return err
	}
	for _, l := range leaves {
		keys[l] = true
	}
	if err := s.closeChatLineage(ctx, db, keys); err != nil {
		return err
	}
	words := newTextMatcher(uniqueStrings(texts))
	match := func(raw string) bool {
		var v any
		if json.Unmarshal([]byte(raw), &v) == nil && (evidenceMatches(v, keys) || words.inValues(v)) {
			return true
		}
		return words.in(raw)
	}
	if err := s.redactIndexedTurns(ctx, db, keys, match); err != nil {
		return err
	}
	if len(texts) > 0 {
		if err := s.redactLegacyTurns(ctx, db, match); err != nil {
			return err
		}
	}
	if err := s.invalidateRoomStyle(ctx, db, jobs); err != nil {
		return err
	}
	if err := s.invalidateRoomMemory(ctx, db, jobs); err != nil {
		return err
	}
	if err := s.redactSmallTables(db, keys, match, editedOnly); err != nil {
		return err
	}
	return s.settleEdits(ctx, db, jobs, queued)
}

// unfinishedJobs leaves out the edits that are already finished. A job that runs again after its edit settled
// (a crash, a replay) must not repeat it: the walk from the message goes through the user turn that stayed, to
// every reply built on it, and by now those are replies on the new words. Writing the tombstones again would also
// undo the settling and take a new number, refusing turns under way. Any change of the message since then has
// removed the settled row, so such a job is not taken for finished.
func (s *Service) unfinishedJobs(db *storage.Store, jobs []redactionJob) ([]redactionJob, error) {
	if !db.IsSQLite() {
		return jobs, nil
	}
	out := make([]redactionJob, 0, len(jobs))
	for _, j := range jobs {
		if j.Operation == "edit" && len(j.Messages) == 1 && strings.TrimSpace(j.Content) != "" {
			finished, err := store.EditFinished(db, j.Messages[0], j.At)
			if err != nil {
				return nil, err
			}
			if finished {
				continue
			}
		}
		out = append(out, j)
	}
	return out, nil
}

// settleEdits runs last, after everything an edit asks for: from here on a new turn reads only the edited
// text, and its reply is not refused for the tombstones (store.SettleSources has the reasoning). A message
// that has another change waiting in the queue, or that is no longer current, is left blocked. The commit also
// removes the batch's jobs (queued, all of them are done by now), so a job cannot run again after a reply on the
// new words has been kept.
func (s *Service) settleEdits(ctx context.Context, db *storage.Store, jobs, queued []redactionJob) error {
	if !db.IsSQLite() {
		return nil
	}
	type edit struct {
		message string
		leaves  []string
	}
	var edits []edit
	var last int64
	var done []any
	for _, j := range queued {
		last = max(last, j.id)
		if j.id != 0 {
			done = append(done, j.id)
		}
	}
	for _, j := range jobs {
		if j.Operation != "edit" || strings.TrimSpace(j.Content) == "" || len(j.Messages) != 1 {
			continue
		}
		leaves, err := leavesHolding(db, j.Obs)
		if err != nil {
			return err
		}
		edits = append(edits, edit{j.Messages[0], uniqueStrings(append(leaves, j.Leaves...))})
	}
	if len(edits) == 0 {
		return nil
	}
	return db.WithWriterContext(ctx, func(tx *sql.Tx) error {
		waiting, err := sourcesInJobsAfter(tx, last)
		if err != nil {
			return err
		}
		var settled []string
		for _, e := range edits {
			sources, ok, err := editedSources(tx, e.message)
			if err != nil {
				return err
			}
			for _, id := range sources {
				ok = ok && !waiting[id]
			}
			if !ok {
				continue
			}
			settled = append(settled, sources...)
			for _, leaf := range e.leaves {
				if !waiting[leaf] {
					settled = append(settled, leaf)
				}
			}
		}
		if err := store.SettleSources(tx, settled); err != nil {
			return err
		}
		if len(done) == 0 {
			return nil
		}
		_, err = tx.Exec(`DELETE FROM redaction_jobs WHERE id IN (`+inList(len(done))+`)`, done...)
		return err
	})
}

// sourcesInJobsAfter lists what the jobs queued after the given one name.
func sourcesInJobsAfter(tx *sql.Tx, last int64) (map[string]bool, error) {
	rows, err := tx.Query(`SELECT payload FROM redaction_jobs WHERE id>?`, last)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names := map[string]bool{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var j redactionJob
		if err := json.Unmarshal([]byte(raw), &j); err != nil {
			return nil, err
		}
		for _, list := range [][]string{j.Messages, j.Obs, j.Leaves} {
			for _, id := range list {
				names[id] = true
			}
		}
	}
	return names, rows.Err()
}

// editedSources lists what an edit of the message leaves in place with the new text: the message, its stored
// copy and the user turns that quoted it (the rows EditMessage changed). ok is false when the stored copy
// is not at the latest revision or has been deleted.
func editedSources(tx *sql.Tx, message string) (sources []string, ok bool, err error) {
	read := func(query string, each func(*sql.Rows) error) error {
		rows, err := tx.Query(query, message)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			if err := each(rows); err != nil {
				return err
			}
		}
		return rows.Err()
	}
	var observations []string
	err = read(`SELECT observation_id FROM memory_observations WHERE platform='discord' AND native_message_id=?`, func(r *sql.Rows) error {
		var id string
		err := r.Scan(&id)
		observations = append(observations, id)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	for _, id := range observations {
		current, err := store.ObservationCurrent(tx, id)
		if err != nil || !current {
			return nil, false, err
		}
	}
	sources = append(append(sources, message), observations...)
	var speakers []string
	err = read(`SELECT id,speaker_id FROM chat_turns WHERE native_id=? AND role='user'`, func(r *sql.Rows) error {
		var id int64
		var speaker string
		err := r.Scan(&id, &speaker)
		sources = append(sources, store.ChatLink(id))
		speakers = append(speakers, speaker)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	for _, speaker := range speakers { // a channel without capture has no stored copy to say this
		withdrawn, err := store.MessageBlocked(tx, "", speaker)
		if err != nil || withdrawn {
			return nil, false, err
		}
	}
	return sources, true, nil
}

// A database from before privacy_settled holds tombstones of edits that are finished, and nothing says so: the
// replies on those messages would stay refused until the tombstones expire. settleOldEdits goes through them once.
// Those tombstones do not say what they were for (that release wrote no 'erased' tombstone), so a deletion or a
// withdrawal must be told from an edit by what is left of the message, with no job in the queue for it:
//
//   - A deletion empties and marks the stored row (deleted_at), so ObservationCurrent refuses it. A withdrawal does
//     the same and leaves an account tombstone. A change not applied yet is older in the row than in the tombstone.
//     The queue keeps a job until its change is applied, so a job for the message keeps it out of this pass.
//   - In a channel without capture there is no stored row. DeleteMessages removes the message's turns in the
//     transaction that writes its tombstone, a withdrawal removes the person's turns, and from then on the tombstone
//     refuses a new turn of that message (it did in that release too). So a user turn still there, written before
//     the tombstone, means an edit (UPDATE, no DELETE), unless its speaker has an account tombstone.
//   - With neither row there is nothing to read and nothing to tell: it stays blocked, as does any row these checks
//     refuse. That includes an edit that kept the words (that release left the stored row's time as it was), and
//     every leaf: a leaf is cut again under the same ID without what was deleted from it, so its tombstone cannot
//     say that no deletion passed through it. They only leave the turn history's answer empty until they expire
//     (the chat context does not hold leaves, see lookedUp).
//
// It runs once, in batches, some time after a start (oldEditsDelay), and only when nothing is waiting.
const (
	oldEditsKey   = "privacy:old_edits_settled"
	oldEditsBatch = 200
)

// settleOldEdits does one batch and reports whether the pass is over.
func (s *Service) settleOldEdits(ctx context.Context) (bool, error) {
	db, err := s.adminDB()
	if err != nil {
		return false, err
	}
	if s.practice || !db.IsSQLite() {
		return true, nil
	}
	var state struct {
		After string `json:"after"`
		Done  bool   `json:"done"`
	}
	if _, err := s.kvGet(oldEditsKey, &state); err != nil {
		return false, err
	}
	if state.Done {
		return true, nil
	}
	if s.Store.HasPendingEvents("source-change") {
		return false, nil // a change not yet in the queue may be for one of these messages
	}
	last, err := settleOldEditBatch(ctx, db, state.After)
	if err != nil {
		return false, err
	}
	state.After, state.Done = last, last == ""
	return state.Done, s.kvSet(oldEditsKey, state)
}

// settleOldEditBatch settles the finished edits among the next messages with a tombstone and returns the last
// message it looked at, "" when there are none left.
func settleOldEditBatch(ctx context.Context, db *storage.Store, after string) (string, error) {
	rows, err := db.Reader().QueryContext(ctx, `SELECT source_id FROM privacy_tombstones WHERE kind='message' AND source_id>? ORDER BY source_id LIMIT ?`, after, oldEditsBatch)
	if err != nil {
		return "", err
	}
	var messages []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return "", err
		}
		messages = append(messages, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(messages) == 0 {
		return "", err
	}
	err = db.WithWriterContext(ctx, func(tx *sql.Tx) error {
		waiting, err := sourcesInJobsAfter(tx, 0)
		if err != nil {
			return err
		}
		var settled []string
		for _, message := range messages {
			sources, ok, err := editedSources(tx, message)
			if err != nil {
				return err
			}
			// the message alone is no proof of anything: no stored row and no turn is also what a deletion leaves
			if !ok || len(sources) < 2 {
				continue
			}
			for _, id := range sources {
				ok = ok && !waiting[id]
			}
			if !ok {
				continue
			}
			before, err := store.TurnsBeforeTombstone(tx, message)
			if err != nil {
				return err
			}
			if !before {
				continue
			}
			var done int
			raw, _ := json.Marshal(sources)
			// already settled: a new number would refuse the turns under way. Marked as erased: never.
			if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM privacy_settled WHERE source_id=?) OR EXISTS(SELECT 1 FROM privacy_tombstones WHERE kind='erased' AND source_id IN (SELECT value FROM json_each(?)))`, message, string(raw)).Scan(&done); err != nil {
				return err
			}
			if done == 0 {
				settled = append(settled, sources...)
			}
		}
		return store.SettleSources(tx, settled)
	})
	if err != nil {
		return "", err
	}
	return messages[len(messages)-1], nil
}

// closeChatLineage adds to keys every chat turn built on them, and tombstones those turns in the same
// write transaction: a reply committed before it is found by the walk, one committed after it links to a
// tombstoned turn and refuses itself. So this comes before anything looks up holders by these keys.
func (s *Service) closeChatLineage(ctx context.Context, db *storage.Store, keys map[string]bool) error {
	if !db.IsSQLite() {
		return nil
	}
	all := make([]string, 0, len(keys))
	for k := range keys {
		all = append(all, k)
	}
	return db.WithWriterContext(ctx, func(tx *sql.Tx) error {
		lineage, err := store.ChatLineage(tx, all)
		if err != nil {
			return err
		}
		if err := store.TombstoneSources(tx, lineage, s.nowString()); err != nil {
			return err
		}
		for _, id := range lineage {
			keys[id] = true
		}
		return nil
	})
}

// redactTurnBody clears the fields of a turn body that match and marks it.
func redactTurnBody(raw string, match func(string) bool) (string, bool) {
	var b map[string]any
	if json.Unmarshal([]byte(raw), &b) != nil {
		return "", false
	}
	changed := false
	for _, field := range []string{"hits", "tools", "plan", "context"} {
		part, _ := json.Marshal(b[field])
		if match(string(part)) {
			b[field] = []any{}
			changed = true
		}
	}
	if !changed {
		return "", false
	}
	if v, ok := b["vision"].(map[string]any); ok && v["mode"] == "described" {
		// the message carries a description of a picture from a source that changed (vision.go)
		b["message"], b["message_deleted"] = "", true
	}
	b["evidence_redacted"] = true
	b["answer"], b["answer_preview"] = "", ""
	out, _ := json.Marshal(b)
	return string(out), true
}

// redactIndexedTurns finds the turns that hold a source through the index.
func (s *Service) redactIndexedTurns(ctx context.Context, db *storage.Store, keys map[string]bool, match func(string) bool) error {
	all := make([]string, 0, len(keys))
	for k := range keys {
		all = append(all, k)
	}
	holders := map[string]map[string]bool{holderTurnEvidence: {}, holderTurnMessage: {}}
	for lo := 0; lo < len(all); lo += redactChunk {
		chunk := all[lo:min(len(all), lo+redactChunk)]
		rows, err := db.Reader().Query(`SELECT holder_kind, holder_id FROM evidence_refs WHERE source_id IN (`+inList(len(chunk))+`)`, anyOf(chunk)...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var kind, id string
			if rows.Scan(&kind, &id) == nil && holders[kind] != nil {
				holders[kind][id] = true
			}
		}
		rows.Close()
	}
	return db.WithWriter(func(tx *sql.Tx) error {
		for id := range holders[holderTurnEvidence] {
			keys[id] = true // traces refer to the turn's complete source index
			if err := ctx.Err(); err != nil {
				return err
			}
			var raw string
			if err := tx.QueryRow(`SELECT body FROM turn_log WHERE id=?`, id).Scan(&raw); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					continue
				}
				return err
			}
			if out, ok := redactTurnBody(raw, func(string) bool { return true }); ok {
				if _, err := tx.Exec(`UPDATE turn_log SET body=? WHERE id=?`, out, id); err != nil {
					return err
				}
			}
		}
		for id := range holders[holderTurnMessage] {
			keys[id] = true
			if _, err := tx.Exec(`UPDATE turn_log SET body=json_set(body, '$.message', '', '$.message_deleted', json('true')) WHERE id=?`, id); err != nil {
				return err
			}
		}
		return nil
	})
}

// redactLegacyTurns matches text in the turns that kept only text (an older release), in batches.
func (s *Service) redactLegacyTurns(ctx context.Context, db *storage.Store, match func(string) bool) error {
	after := ""
	for ctx.Err() == nil {
		rows, err := db.Reader().Query(`SELECT id, body FROM turn_log WHERE evidence_state=? AND id>? ORDER BY id LIMIT 200`, evidenceLegacyText, after)
		if err != nil {
			return err
		}
		type change struct{ id, body string }
		var changes []change
		n := 0
		for rows.Next() {
			var id, raw string
			if err := rows.Scan(&id, &raw); err != nil {
				rows.Close()
				return err
			}
			n++
			after = id
			if out, ok := redactTurnBody(raw, match); ok {
				changes = append(changes, change{id, out})
			}
		}
		rows.Close()
		if n == 0 {
			return nil
		}
		if len(changes) > 0 {
			err := db.WithWriter(func(tx *sql.Tx) error {
				for _, c := range changes {
					var body map[string]any
					state := evidenceIndexed
					if json.Unmarshal([]byte(c.body), &body) == nil {
						if _, copied := evidenceOf(body); copied {
							state = evidenceLegacyText
						}
					}
					if _, err := tx.Exec(`UPDATE turn_log SET body=?, evidence_state=? WHERE id=?`, c.body, state, c.id); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}

// redactSmallTables handles what the console's operators curate by hand: lab reviews, memory reviews and
// eval runs. They are small, so they are scanned once per batch of jobs rather than indexed.
// keepCollected names messages that were only edited: the collected rows of those stay (they hold no text).
func (s *Service) redactSmallTables(db *storage.Store, keys map[string]bool, match func(string) bool, keepCollected map[string]bool) error {
	// Transitive review -> eval links must be captured before deleting a review.
	frontier := make([]string, 0, len(keys))
	for key := range keys {
		frontier = append(frontier, key)
	}
	for len(frontier) > 0 {
		raw, _ := json.Marshal(frontier)
		frontier = nil
		rows, err := db.Reader().Query(`SELECT DISTINCT holder_kind,holder_id FROM evidence_refs WHERE source_id IN (SELECT value FROM json_each(?))`, string(raw))
		if err != nil {
			return err
		}
		for rows.Next() {
			var kind, id string
			if err := rows.Scan(&kind, &id); err != nil {
				rows.Close()
				return err
			}
			if (kind == holderLab || kind == holderReview || kind == holderEval || kind == "cache" || kind == "participation" || kind == "trace" || kind == "channel_summary") && !keys[id] {
				keys[id] = true
				frontier = append(frontier, id)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
	}
	all := make([]string, 0, len(keys))
	for k := range keys {
		all = append(all, k)
	}
	rawKeys, _ := json.Marshal(all)
	if _, err := db.DB.Exec(`DELETE FROM chat_turns WHERE role<>'user' AND 'chat:'||id IN (SELECT holder_id FROM evidence_refs WHERE holder_kind='chat' AND source_id IN (SELECT value FROM json_each(?)))`, string(rawKeys)); err != nil {
		return err
	}
	// a user turn whose text was partly made from another message (a picture's description, vision.go)
	if _, err := db.DB.Exec(`DELETE FROM chat_turns WHERE 'chat:'||id IN (SELECT holder_id FROM evidence_refs WHERE holder_kind='chat_from' AND source_id IN (SELECT value FROM json_each(?)))`, string(rawKeys)); err != nil {
		return err
	}
	if _, err := db.DB.Exec(`UPDATE traces SET body='{}' WHERE id IN (SELECT value FROM json_each(?))`, string(rawKeys)); err != nil {
		return err
	}
	if _, err := db.DB.Exec(`DELETE FROM channel_summary WHERE 'summary:'||guild_id||':'||channel_id IN (SELECT value FROM json_each(?))`, string(rawKeys)); err != nil {
		return err
	}

	spared := make([]string, 0, len(keepCollected))
	for id := range keepCollected {
		spared = append(spared, id)
	}
	rawSpared, _ := json.Marshal(spared)
	if _, err := db.DB.Exec(`DELETE FROM participation_log WHERE mode='collect' AND id IN (SELECT value FROM json_each(?)) AND message_id NOT IN (SELECT value FROM json_each(?))`, string(rawKeys), string(rawSpared)); err != nil {
		return err
	}
	if _, err := db.DB.Exec(`UPDATE participation_log SET message='',draft='' WHERE id IN (SELECT value FROM json_each(?))`, string(rawKeys)); err != nil {
		return err
	}

	if _, err := db.DB.Exec(`DELETE FROM memory_embed_cache WHERE cache_key IN (SELECT value FROM json_each(?))`, string(rawKeys)); err != nil {
		return err
	}
	reviews, err := db.Reader().Query(`SELECT id, question, answer, note, meta FROM lab_reviews`)
	if err != nil {
		return err
	}
	var remove []string
	for reviews.Next() {
		var id, q, a, note, meta string
		if err := reviews.Scan(&id, &q, &a, &note, &meta); err != nil {
			reviews.Close()
			return err
		}
		if keys[id] || match(meta) || match(q) || match(a) || match(note) {
			remove = append(remove, id)
		}
	}
	err = reviews.Err()
	reviews.Close()
	if err != nil {
		return err
	}
	for _, id := range remove {
		keys[id] = true
		if err := db.WithWriter(func(tx *sql.Tx) error {
			if err := store.Tombstone(tx, "source", id, s.nowString()); err != nil {
				return err
			}
			_, err := tx.Exec(`DELETE FROM lab_reviews WHERE id=?`, id)
			return err
		}); err != nil {
			return err
		}
	}
	var exists int
	_ = db.Reader().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='memory_reviews'`).Scan(&exists)
	if exists > 0 {
		rows, err := db.Reader().Query(`SELECT leaf_id, excerpt, note FROM memory_reviews`)
		if err != nil {
			return err
		}
		var docs []string
		for rows.Next() {
			var id, excerpt, note string
			if err := rows.Scan(&id, &excerpt, &note); err != nil {
				rows.Close()
				return err
			}
			if keys[id] || match(excerpt) || match(note) {
				docs = append(docs, id)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, id := range uniqueStrings(docs) {
			if _, err := db.DB.Exec(`DELETE FROM memory_reviews WHERE leaf_id=?`, id); err != nil {
				return err
			}
		}
	}
	_ = db.Reader().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='eval_runs'`).Scan(&exists)
	if exists > 0 {
		runs, err := db.Reader().Query(`SELECT id, items FROM eval_runs`)
		if err != nil {
			return err
		}
		var clear []string
		for runs.Next() {
			var id, items string
			if err := runs.Scan(&id, &items); err != nil {
				runs.Close()
				return err
			}
			if keys[id] || match(items) {
				clear = append(clear, id)
			}
		}
		err = runs.Err()
		runs.Close()
		if err != nil {
			return err
		}
		for _, id := range clear {
			if _, err := db.DB.Exec(`UPDATE eval_runs SET items='[]' WHERE id=?`, id); err != nil {
				return err
			}
		}
	}
	return nil
}

// textMatcher finds the start of any of many source texts inside a record. Comparing every text with
// every record is quadratic (a person's 5,000 messages against thousands of records); instead each
// text is filed under its first prefixRunes runes and a record is looked at once, window by window.
type textMatcher struct{ byPrefix map[string][]string }

const prefixRunes = 12 // texts shorter than this are never matched (too common to mean a copy)

func newTextMatcher(texts []string) *textMatcher {
	m := &textMatcher{byPrefix: map[string][]string{}}
	for _, t := range texts {
		t = strings.TrimSuffix(t, "…")
		r := []rune(t)
		if len(r) >= prefixRunes {
			k := string(r[:prefixRunes])
			m.byPrefix[k] = append(m.byPrefix[k], t)
		}
	}
	return m
}

func (m *textMatcher) in(s string) bool {
	if len(m.byPrefix) == 0 {
		return false
	}
	r := []rune(s)
	for i := 0; i+prefixRunes <= len(r); i++ {
		if cands, ok := m.byPrefix[string(r[i:i+prefixRunes])]; ok {
			for _, c := range cands {
				if strings.Contains(s, c) {
					return true
				}
			}
		}
	}
	return false
}

func (m *textMatcher) inValues(v any) bool {
	switch x := v.(type) {
	case string:
		return m.in(x)
	case []any:
		for _, c := range x {
			if m.inValues(c) {
				return true
			}
		}
	case map[string]any:
		for _, c := range x {
			if m.inValues(c) {
				return true
			}
		}
	}
	return false
}
