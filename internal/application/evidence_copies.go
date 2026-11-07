package application

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"masamong/internal/action"
	"masamong/internal/errcode"
	"masamong/internal/memory/store"
	"masamong/internal/storage"
)

// copySources expands leaf and review references while they still exist. Persisted references survive
// a leaf resync and are also tombstoned, so a later writer cannot resurrect a completed deletion.
func copySources(tx *sql.Tx, values ...any) ([]string, error) {
	seen := map[string]bool{}
	var visit func(any)
	visit = func(v any) {
		switch x := v.(type) {
		case string:
			if x != "" {
				seen[x] = true
			}
		case []string:
			for _, s := range x {
				visit(s)
			}
		case []any:
			for _, v := range x {
				visit(v)
			}
		case map[string]any:
			for k, v := range x {
				if provenanceFields[k] {
					visit(v)
				} else {
					switch v.(type) {
					case map[string]any, []any:
						visit(v)
					}
				}
			}
		}
	}
	for _, v := range values {
		visit(v)
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	var memoryExists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='memory_observations'`).Scan(&memoryExists); err != nil {
		return nil, err
	}
	if memoryExists == 0 {
		return keys, nil
	}
	for after := 0; after < len(keys); {
		frontier := keys[after:]
		after = len(keys)
		raw, _ := json.Marshal(frontier)
		// Expand a whole frontier in set-based queries, rather than scanning leaves per observation.
		// A link to a chat turn stays a link: copying what it holds would rebuild the flat list, and a
		// deletion follows the link instead (store.ChatLineage).
		rows, err := tx.Query(`SELECT j.value FROM memory_leaves l,json_each(l.observation_ids) j WHERE l.leaf_id IN (SELECT value FROM json_each(?))
			UNION SELECT source_id FROM evidence_refs WHERE holder_id IN (SELECT value FROM json_each(?) WHERE value NOT GLOB 'chat:*')
			UNION SELECT COALESCE(native_message_id,'') FROM memory_observations WHERE observation_id IN (SELECT value FROM json_each(?))`, string(raw), string(raw), string(raw))
		if err != nil {
			return nil, err
		}
		var more []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			more = append(more, id)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
		rows, err = tx.Query(`SELECT DISTINCT actor_account FROM memory_observations WHERE observation_id IN (SELECT value FROM json_each(?))`, string(raw))
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			more = append(more, personRef(id))
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
		for _, id := range more {
			if id != "" && !seen[id] {
				seen[id] = true
				keys = append(keys, id)
			}
		}
	}
	return keys, nil
}

var errEvidenceDeleted = errors.New("source evidence was deleted or changed; retrieve it again")

func indexCopy(tx *sql.Tx, kind, id string, keys []string, guard bool) error {
	for _, key := range keys {
		if guard {
			blocked, err := store.SourceBlocked(tx, key)
			if err != nil {
				return err
			}
			if blocked {
				return errEvidenceDeleted
			}
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO evidence_refs VALUES(?,?,?)`, key, kind, id); err != nil {
			return err
		}
	}
	return nil
}

// enqueueRedactionTx is shared by consent commits and source changes. Tombstones and intent are one
// transaction; replay also completes source erasure after a crash before the engine call.
func (s *Service) enqueueRedactionTx(tx *sql.Tx, j redactionJob) error {
	if j.At == "" {
		j.At = s.Clock.Now().UTC().Format(time.RFC3339Nano)
	}
	keys := uniqueStrings(append(append(append(append([]string{}, j.Messages...), j.Obs...), j.Leaves...), j.Ref))
	// anything but an edit that leaves words to read takes these sources out of settling for good
	erasing := j.Operation != "edit" || strings.TrimSpace(j.Content) == ""
	for _, key := range keys {
		if err := store.Tombstone(tx, "source", key, j.At); err != nil {
			return err
		}
		if erasing {
			if err := store.Tombstone(tx, "erased", key, j.At); err != nil {
				return err
			}
		}
	}
	for _, id := range j.Messages {
		if err := store.Tombstone(tx, "message", id, j.At); err != nil {
			return err
		}
	}
	if err := store.Tombstone(tx, "account", j.User, j.At); err != nil {
		return err
	}
	raw, err := json.Marshal(j)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO redaction_jobs(payload,created_at) VALUES(?,?)`, string(raw), j.At)
	return err
}

func (s *Service) withdrawalIntent(tx *sql.Tx, user string) error {
	return s.enqueueRedactionTx(tx, redactionJob{User: user, Ref: personRef(user), Operation: "withdraw"})
}

// Source text is copied into child intents in bounded batches before erasing it. The parent intent
// survives until every batch finishes; deleted_at is the source checkpoint, and a committed child
// retains the legacy matching text if the process dies after erasure but before derivative cleanup.
func (s *Service) processWithdrawal(ctx context.Context, db *storage.Store, j redactionJob) (int, error) {
	n := 0
	if err := s.forgetPerson(ctx, j.User); err != nil {
		return n, err
	}
	for ctx.Err() == nil {
		rows, err := db.Reader().QueryContext(ctx, `SELECT observation_id,COALESCE(native_message_id,''),content FROM memory_observations WHERE platform='discord' AND actor_account=? AND deleted_at='' ORDER BY observation_id LIMIT 400`, j.User)
		if err != nil {
			return n, err
		}
		child := redactionJob{Operation: "withdraw_batch", At: j.At}
		for rows.Next() {
			var id, native, text string
			if err := rows.Scan(&id, &native, &text); err != nil {
				rows.Close()
				return n, err
			}
			child.Obs = append(child.Obs, id)
			if native != "" {
				child.Messages = append(child.Messages, native)
			}
			if len([]rune(text)) >= prefixRunes {
				child.Texts = append(child.Texts, clip(text, 80))
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return n, err
		}
		if len(child.Obs) == 0 {
			_, err = db.DB.ExecContext(ctx, `DELETE FROM chat_turns WHERE speaker_id=?`, j.User)
			return n, err
		}
		child.Leaves, err = leavesHolding(db, child.Obs)
		if err != nil {
			return n, err
		}
		var childID int64
		err = db.WithWriterContext(ctx, func(tx *sql.Tx) error {
			if err := s.enqueueRedactionTx(tx, child); err != nil {
				return err
			}
			return tx.QueryRow(`SELECT last_insert_rowid()`).Scan(&childID)
		})
		if err != nil {
			return n, err
		}
		if err := s.processRedactions(ctx, db, []redactionJob{child}); err != nil {
			return n, err
		}
		if _, err := db.DB.ExecContext(ctx, `DELETE FROM redaction_jobs WHERE id=?`, childID); err != nil {
			return n, err
		}
		n += len(child.Obs)
	}
	return n, ctx.Err()
}

// classifyCopies commits the index and per-row checkpoint together, at most 300 rows per transaction.
// Lab rows precede eval rows so review references can expand even when an eval only names a review ID.
func (s *Service) classifyCopies(ctx context.Context, db *storage.Store) (int, error) {
	for _, spec := range []struct{ table, id, raw, kind string }{
		{"lab_reviews", "id", "meta", holderLab}, {"memory_reviews", "leaf_id", "leaf_id", holderReview}, {"eval_runs", "id", "items", holderEval},
		{"chat_turns", "'chat:'||id", `json_object('message_id',native_id,'account_id',speaker_id)`, "chat"},
		{"participation_log", "id", `json_object('message_id',message_id,'speaker_refs',json_array(author_ref))`, "participation"},
		{"traces", "id", "body", "trace"},
		{"channel_summary", "'summary:'||guild_id||':'||channel_id", "anchor_observation", "channel_summary"},
	} {
		rows, err := db.Reader().QueryContext(ctx, `SELECT `+spec.id+`,`+spec.raw+` FROM `+spec.table+` WHERE evidence_state=0 ORDER BY `+spec.id+` LIMIT 300`)
		if err != nil {
			return 0, err
		}
		type row struct{ id, raw string }
		var batch []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.raw); err != nil {
				rows.Close()
				return 0, err
			}
			batch = append(batch, r)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return 0, err
		}
		if len(batch) == 0 {
			continue
		}
		err = db.WithWriterContext(ctx, func(tx *sql.Tx) error {
			for _, r := range batch {
				var v any
				if spec.kind == holderReview || spec.kind == "channel_summary" {
					v = []string{r.raw}
				} else {
					_ = json.Unmarshal([]byte(r.raw), &v)
					if spec.kind == "chat" {
						if body, ok := v.(map[string]any); ok {
							account, _ := body["account_id"].(string)
							body["speaker_refs"] = []string{personRef(account)}
						}
					}
				}
				// Older channel summaries kept only an anchor, and old traces omitted their request ID.
				// Their copied text cannot be attributed completely: retire those ephemeral details.
				if spec.kind == "channel_summary" {
					if _, err := tx.Exec(`DELETE FROM channel_summary WHERE `+spec.id+`=?`, r.id); err != nil {
						return err
					}
					continue
				}
				keys, err := copySources(tx, v)
				if err != nil {
					return err
				}
				if spec.kind == "trace" {
					if _, err := tx.Exec(`UPDATE traces SET body='{}' WHERE id=?`, r.id); err != nil {
						return err
					}
				}
				if err := indexCopy(tx, spec.kind, r.id, keys, false); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `UPDATE `+spec.table+` SET evidence_state=1 WHERE `+spec.id+`=?`, r.id); err != nil {
					return err
				}
			}
			return nil
		})
		return len(batch), err
	}
	return 0, nil
}

func (s *Service) evidenceWriteError(req action.Request, err error) action.Result {
	if errors.Is(err, errEvidenceDeleted) {
		return action.Fail(req, errcode.InvalidInput, "privacy.evidence_changed", s.msg(req, "privacy.evidence_changed"), nil)
	}
	return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
}
