package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"strings"

	"masamong/internal/memory/corpus"
)

// Room is a registered source room.
type Room struct {
	Room          string `json:"room"`
	Collection    string `json:"collection"`
	Platform      string `json:"platform"`
	Label         string `json:"label"`
	NaiveTimezone string `json:"naive_timezone,omitempty"`
}

// UpsertRoom registers a room (label and naive timezone declaration).
func (s *Store) UpsertRoom(tx *sql.Tx, r Room) error {
	q := `INSERT INTO memory_rooms(room, collection, platform, label, naive_timezone, updated_at) VALUES(?,?,?,?,?,?)
		ON CONFLICT(room) DO UPDATE SET collection=excluded.collection, label=CASE WHEN excluded.label<>'' THEN excluded.label ELSE memory_rooms.label END,
		naive_timezone=CASE WHEN excluded.naive_timezone<>'' THEN excluded.naive_timezone ELSE memory_rooms.naive_timezone END, updated_at=excluded.updated_at`
	if s.tidb() {
		q = `INSERT INTO memory_rooms(room, collection, platform, label, naive_timezone, updated_at) VALUES(?,?,?,?,?,?)
		ON DUPLICATE KEY UPDATE collection=VALUES(collection), label=IF(VALUES(label)<>'', VALUES(label), label),
		naive_timezone=IF(VALUES(naive_timezone)<>'', VALUES(naive_timezone), naive_timezone), updated_at=VALUES(updated_at)`
	}
	if _, err := tx.Exec(q, r.Room, r.Collection, r.Platform, r.Label, r.NaiveTimezone, now()); err != nil {
		return err
	}
	_, err := s.bumpRoomRevision(context.Background(), tx, r.Room, false)
	return err
}

// ErrNoRoom means the room is not registered.
var ErrNoRoom = errors.New("memory: no such room")

// SetRoomLabel renames a room; evidence and the console show the new label.
func (s *Store) SetRoomLabel(room, label string) error {
	return s.DB.WithWriter(func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE memory_rooms SET label=?, updated_at=? WHERE room=?`, label, now(), room)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNoRoom
		}
		_, err = s.bumpRoomRevision(context.Background(), tx, room, false)
		return err
	})
}

// DeleteRoom removes a room's stored messages, leaves and registration from the product database.
// The source files it was imported from are not touched; importing them again restores the room.
func (s *Store) DeleteRoom(room string) (int, error) {
	var removed int
	err := s.DB.WithWriter(func(tx *sql.Tx) error {
		if !s.tidb() {
			if _, err := tx.Exec(`DELETE FROM memory_observation_aliases WHERE observation_id IN (SELECT observation_id FROM memory_observations WHERE room=?)`, room); err != nil {
				return err
			}
		}
		if !s.tidb() {
			// cached embeddings of the room's message texts (message vectors) are tied to the messages
			if _, err := tx.Exec(`DELETE FROM memory_embed_cache WHERE cache_key IN (SELECT holder_id FROM evidence_refs WHERE holder_kind='cache'
				AND source_id IN (SELECT observation_id FROM memory_observations WHERE room=?))`, room); err != nil {
				return err
			}
		}
		res, err := tx.Exec(`DELETE FROM memory_observations WHERE room=?`, room)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		removed = int(n)
		if _, err := tx.Exec(`DELETE FROM memory_leaves WHERE room=?`, room); err != nil {
			return err
		}
		if s.leafSyncEnabled {
			// A replacement room must never inherit the old cut's trusted prefix.
			if _, err := tx.Exec(`DELETE FROM memory_leaf_sync WHERE room=?`, room); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`DELETE FROM memory_message_vectors WHERE room=?`, room); err != nil {
			return err
		}
		// an in-flight batch plan names the room's leaves and messages (ids and text hashes)
		if _, err := tx.Exec(`DELETE FROM memory_batch_plans WHERE room=?`, room); err != nil {
			return err
		}
		if !s.tidb() {
			if err := DeleteRoomMemory(tx, room); err != nil {
				return err
			}
		}
		res, err = tx.Exec(`DELETE FROM memory_rooms WHERE room=?`, room)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 && removed == 0 {
			return ErrNoRoom
		}
		return nil
	})
	return removed, err
}

// Revision is one stored message changed by its author after it was captured.
type Revision struct {
	Room          string `json:"room"`
	ObservationID string `json:"observation_id"`
	Deleted       bool   `json:"deleted"`
	RoomRevision  int64  `json:"-"`
}

// EditMessage replaces the text of a stored message (platform + native message ID) with what its author
// changed it to, and the chat turn that quoted it. ok is false when the message is not stored, is
// deleted, or already has this text. An edit to empty text is a deletion.
func (s *Store) EditMessage(platform, nativeID, content, at string) (Revision, bool, error) {
	return s.EditMessageContext(context.Background(), platform, nativeID, content, at)
}

func (s *Store) EditMessageContext(ctx context.Context, platform, nativeID, content, at string) (Revision, bool, error) {
	if strings.TrimSpace(content) == "" {
		revs, err := s.DeleteMessagesContext(ctx, platform, []string{nativeID}, at)
		if err != nil || len(revs) == 0 {
			return Revision{}, false, err
		}
		return revs[0], true, nil
	}
	var rev Revision
	changed := false
	err := s.DB.WithWriterContext(ctx, func(tx *sql.Tx) error {
		if !s.tidb() {
			current, err := currentMessageRevision(tx, nativeID, at)
			if err != nil || !current {
				return err
			}
			// not written again for the same revision: it would undo a finished edit (SettleSources)
			recorded, err := messageRevisionRecorded(tx, nativeID, at)
			if err != nil {
				return err
			}
			if !recorded {
				if err := Tombstone(tx, "message", nativeID, at); err != nil {
					return err
				}
			}
		}
		// the chat turn follows the edit whether or not the channel stores its messages (a channel the
		// bot answers in without capture still keeps the message as conversation context)
		if _, err := tx.Exec(`UPDATE chat_turns SET content=?,call_address=CASE WHEN call_address=? THEN call_address ELSE ? END
			WHERE native_id=? AND role='user'`, content, CallReply, CallEdited, nativeID); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM chat_turns WHERE native_id=? AND role<>'user'`, nativeID); err != nil {
			return err
		}
		if !s.tidb() {
			// a burst answered as one turn, or a turn with a picture's description: the row now holds these words alone,
			// so nothing is left of the other messages to go with them. Its own entries stay, and a withdrawal finds it
			// by them; a thread's copy stays tied to the row it copies.
			if _, err := tx.Exec(`DELETE FROM evidence_refs WHERE holder_kind='chat_from' AND source_id NOT GLOB 'chat:*'
				AND holder_id IN (SELECT 'chat:'||id FROM chat_turns WHERE native_id=? AND role='user')`, nativeID); err != nil {
				return err
			}
		}
		var old string
		err := tx.QueryRow(`SELECT observation_id, room, content FROM memory_observations
			WHERE platform=? AND native_message_id=? AND deleted_at=''`, platform, nativeID).Scan(&rev.ObservationID, &rev.Room, &old)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if old == content {
			// the words stayed (a changed attachment); this revision is applied all the same (ObservationCurrent)
			_, err := tx.Exec(`UPDATE memory_observations SET edited_at=? WHERE observation_id=?`, at, rev.ObservationID)
			return err
		}
		if _, err := tx.Exec(`UPDATE memory_observations SET content=?, edited_at=? WHERE observation_id=?`, content, at, rev.ObservationID); err != nil {
			return err
		}
		changed = true
		if err := invalidateLeaves(tx, rev.ObservationID); err != nil {
			return err
		}
		// the room's signature changes, so the next index build reads its messages again
		rev.RoomRevision, err = s.bumpRoomRevision(ctx, tx, rev.Room, true)
		return err
	})
	return rev, changed, err
}

// DeleteMessages removes the text of stored messages whose authors deleted them. The row stays as a
// tombstone (empty content, deleted_at set) so an archive import of the same channel later cannot bring
// the message back: imports skip native IDs that are already stored. Every read of messages leaves
// tombstones out. Chat turns that quoted the messages are removed.
func (s *Store) DeleteMessages(platform string, nativeIDs []string, at string) ([]Revision, error) {
	return s.DeleteMessagesContext(context.Background(), platform, nativeIDs, at)
}

func (s *Store) DeleteMessagesContext(ctx context.Context, platform string, nativeIDs []string, at string) ([]Revision, error) {
	var out []Revision
	err := s.DB.WithWriterContext(ctx, func(tx *sql.Tx) error {
		for _, id := range nativeIDs {
			if !s.tidb() {
				if err := Tombstone(tx, "message", id, at); err != nil {
					return err
				}
			}
			// the chat turn goes whether or not the message was stored (see EditMessage)
			if _, err := tx.Exec(`DELETE FROM chat_turns WHERE native_id=?`, id); err != nil {
				return err
			}
			var rev Revision
			err := tx.QueryRow(`SELECT observation_id, room FROM memory_observations
				WHERE platform=? AND native_message_id=? AND deleted_at=''`, platform, id).Scan(&rev.ObservationID, &rev.Room)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE memory_observations SET content='', deleted_at=? WHERE observation_id=?`, at, rev.ObservationID); err != nil {
				return err
			}
			rev.Deleted = true
			out = append(out, rev)
		}
		// one pass over the leaves for the whole batch, not one per message
		ids := make([]string, len(out))
		for i, r := range out {
			ids[i] = r.ObservationID
		}
		if err := invalidateLeaves(tx, ids...); err != nil {
			return err
		}
		return s.reviseRooms(ctx, tx, out)
	})
	return out, err
}

// Remove persisted leaf bodies/summaries immediately; the delayed resync builds fresh leaves.
//
// A few messages (an edit, a deletion) are found by a substring scan of each leaf's ID list, which is
// cheap even for a large history; a bulk change (a person withdrawing) is found with one JSON pass per
// chunk of messages instead of one pass per message.
func invalidateLeaves(tx *sql.Tx, observationIDs ...string) error {
	if len(observationIDs) == 0 {
		return nil
	}
	raw, _ := json.Marshal(observationIDs)
	if _, err := tx.Exec(`INSERT INTO privacy_tombstones(kind,source_id,at) SELECT 'source',value,? FROM json_each(?) WHERE true ON CONFLICT(kind,source_id) DO UPDATE SET at=excluded.at`, now(), string(raw)); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO privacy_tombstones(kind,source_id,at) SELECT DISTINCT 'source',l.leaf_id,? FROM memory_leaves l,json_each(l.observation_ids) j WHERE j.value IN (SELECT value FROM json_each(?)) ON CONFLICT(kind,source_id) DO UPDATE SET at=excluded.at`, now(), string(raw)); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM memory_embed_cache WHERE cache_key IN (SELECT holder_id FROM evidence_refs WHERE holder_kind='cache' AND source_id IN (SELECT value FROM json_each(?)))`, string(raw)); err != nil {
		return err
	}
	if err := deleteMessageVectors(tx, string(raw)); err != nil {
		return err
	}
	const clear = `UPDATE memory_leaves SET body='', body_hash='', speakers='[]', summary='', leaf_vec=NULL, summary_vec=NULL WHERE `
	const few, chunk = 8, 500
	for lo := 0; lo < len(observationIDs); lo += chunk {
		ids := observationIDs[lo:min(len(observationIDs), lo+chunk)]
		ph := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
		if len(ids) > few {
			if _, err := tx.Exec(clear+`leaf_id IN (SELECT l.leaf_id FROM memory_leaves l, json_each(l.observation_ids) j WHERE j.value IN (`+ph+`))`, toAny(ids)...); err != nil {
				return err
			}
			continue
		}
		// the ID list is stored as JSON, so an ID occurs in it as its JSON string; json_each then
		// confirms it is a whole element
		var cond []string
		var args []any
		for _, id := range ids {
			quoted, _ := json.Marshal(id)
			cond = append(cond, `instr(memory_leaves.observation_ids, ?)>0`)
			args = append(args, string(quoted))
		}
		args = append(args, toAny(ids)...)
		if _, err := tx.Exec(clear+`(`+strings.Join(cond, " OR ")+`) AND EXISTS (SELECT 1 FROM json_each(memory_leaves.observation_ids) WHERE value IN (`+ph+`))`, args...); err != nil {
			return err
		}
	}
	return nil
}

// AccountLabels is the name each platform account last wrote under (the console's People page).
func (s *Store) AccountLabels(platform string, accounts []string) (map[string]string, error) {
	out := map[string]string{}
	for _, a := range accounts {
		var label string
		err := s.DB.DB.QueryRow(`SELECT actor_label FROM memory_observations WHERE platform=? AND actor_account=?
			ORDER BY parsed_time DESC LIMIT 1`, platform, a).Scan(&label)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return out, err
		}
		out[a] = label
	}
	return out, nil
}

// DeleteAccountMessages deletes every stored message of one platform account (a person who withdrew
// consent to being remembered), as DeleteMessages does for messages deleted one by one, and the
// person's chat turns. Imported
// messages without a native ID (KakaoTalk) have no account to match and are not touched.
func (s *Store) DeleteAccountMessages(platform, account, at string) ([]Revision, error) {
	if strings.TrimSpace(account) == "" {
		return nil, nil
	}
	// the person's lines in the bot's conversations, wherever they were said (a channel without
	// capture has no stored message to find them by)
	if err := s.DB.WithWriter(func(tx *sql.Tx) error {
		if !s.tidb() {
			if err := Tombstone(tx, "account", account, at); err != nil {
				return err
			}
		}
		_, err := tx.Exec(`DELETE FROM chat_turns WHERE speaker_id=?`, account)
		return err
	}); err != nil {
		return nil, err
	}
	rows, err := s.DB.DB.Query(`SELECT native_message_id FROM memory_observations
		WHERE platform=? AND actor_account=? AND deleted_at='' AND native_message_id IS NOT NULL`, platform, account)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	var out []Revision
	for lo := 0; lo < len(ids); lo += insertBatch {
		revs, err := s.DeleteMessages(platform, ids[lo:min(len(ids), lo+insertBatch)], at)
		out = append(out, revs...)
		if err != nil {
			return out, err
		}
	}
	// A bounded batch covers native-less account rows in one leaf pass. deleted_at is resumable
	// progress: a failed or cancelled withdrawal repeats only rows not yet erased.
	for {
		var revisions []Revision
		err = s.DB.WithWriter(func(tx *sql.Tx) error {
			rows, err := tx.Query(`SELECT observation_id,room FROM memory_observations WHERE platform=? AND actor_account=? AND deleted_at='' AND (native_message_id IS NULL OR native_message_id='') ORDER BY observation_id LIMIT 400`, platform, account)
			if err != nil {
				return err
			}
			for rows.Next() {
				var r Revision
				if err := rows.Scan(&r.ObservationID, &r.Room); err != nil {
					rows.Close()
					return err
				}
				r.Deleted = true
				revisions = append(revisions, r)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			var ids []string
			for _, r := range revisions {
				if _, err := tx.Exec(`UPDATE memory_observations SET content='',deleted_at=? WHERE observation_id=?`, at, r.ObservationID); err != nil {
					return err
				}
				ids = append(ids, r.ObservationID)
			}
			if err := invalidateLeaves(tx, ids...); err != nil {
				return err
			}
			return s.reviseRooms(context.Background(), tx, revisions)
		})
		if err != nil {
			return out, err
		}
		out = append(out, revisions...)
		if len(revisions) < 400 {
			break
		}
	}
	return out, err
}

// InsertResult counts one import batch.
type InsertResult struct {
	Seen      int `json:"seen"`
	Inserted  int `json:"inserted"`
	Duplicate int `json:"duplicate"`
	Skipped   int `json:"skipped_ineligible"`
}

// InsertObservations stores eligible observations (human text with a time). Existing observation IDs
// and already-known native message IDs are skipped, so re-import and live-then-archive are no-ops.
// A large import (a KakaoTalk export of several years) is committed in batches: under TiDB's
// transaction size limit, and without holding the writer long enough to stall live capture. A failed
// import can simply be run again (duplicates are skipped).
func (s *Store) InsertObservations(obs []corpus.Observation, merge map[string]string) (InsertResult, error) {
	return s.InsertObservationsContext(context.Background(), obs, merge)
}

func (s *Store) InsertObservationsContext(ctx context.Context, obs []corpus.Observation, merge map[string]string) (InsertResult, error) {
	var res InsertResult
	counts := corpus.NewKakaoOccurrences()
	for i := range obs {
		if i%256 == 0 {
			if err := ctx.Err(); err != nil {
				return res, err
			}
		}
		o := &obs[i]
		if o.Platform == "kakao" && o.IdentityID == "" {
			n, err := counts.Next(o.SourceRoom, o.ParsedTime, o.ActorLabel, o.Content)
			if err != nil {
				return res, err
			}
			o.IdentityID = corpus.KakaoRowIdentity(o.SourceRoom, o.ParsedTime, o.ActorLabel, o.Content, n)
		}
	}
	for lo := 0; lo < len(obs); lo += insertBatch {
		r, err := s.insertObservations(ctx, obs[lo:min(len(obs), lo+insertBatch)], merge)
		res.Seen += r.Seen
		res.Inserted += r.Inserted
		res.Duplicate += r.Duplicate
		res.Skipped += r.Skipped
		if err != nil {
			return res, err
		}
	}
	return res, nil
}

const insertBatch = 2000

func (s *Store) insertObservations(ctx context.Context, obs []corpus.Observation, merge map[string]string) (InsertResult, error) {
	var res InsertResult
	var nameSources []string
	err := s.DB.WithWriterContext(ctx, func(tx *sql.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		ins := `INSERT INTO memory_observations(observation_id, platform, collection, source_room, room, native_message_id, actor_label,
			actor_account, content, parsed_time, record_kind, imported_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`
		if s.tidb() {
			ins = `INSERT IGNORE INTO memory_observations(observation_id, platform, collection, source_room, room, native_message_id, actor_label,
			actor_account, content, parsed_time, record_kind, imported_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`
		}
		stmt, err := tx.PrepareContext(ctx, ins)
		if err != nil {
			return err
		}
		defer stmt.Close()
		// The gateway's early opt-out check can race a withdrawal. Check again under the same
		// writer transaction as insertion, including archive imports with known Discord accounts.
		var consentTable int
		if !s.tidb() {
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name='consent_state'`).Scan(&consentTable); err != nil {
				return err
			}
		}
		aliases := map[string]string{}
		var pendingAliases []observationAlias
		if !s.tidb() {
			aliases, err = loadObservationAliases(ctx, tx, obs)
			if err != nil {
				return err
			}
		}
		withdrawn := map[string]bool{}
		ts := now()
		rooms := map[string]corpus.Observation{}
		appends := map[string]leafAppend{}
		for _, o := range obs {
			res.Seen++
			if !o.Eligible() {
				res.Skipped++
				continue
			}
			if !s.tidb() && o.Platform == "discord" {
				blocked, err := MessageBlocked(tx, o.NativeMessageID, o.ActorAccount)
				if err != nil {
					return err
				}
				if blocked {
					res.Skipped++
					continue
				}
			}
			if consentTable > 0 && o.Platform == "discord" && o.ActorAccount != "" {
				denied, known := withdrawn[o.ActorAccount]
				if !known {
					var status string
					err := tx.QueryRowContext(ctx, `SELECT status FROM consent_state WHERE user_id=? AND scope='memory'`, o.ActorAccount).Scan(&status)
					if err != nil && !errors.Is(err, sql.ErrNoRows) {
						return err
					}
					denied = status == "withdrawn"
					withdrawn[o.ActorAccount] = denied
				}
				if denied {
					res.Skipped++
					continue
				}
			}
			if !s.tidb() && o.Platform == "kakao" {
				id := ""
				for _, key := range []string{o.ObservationID, o.LegacyID, o.IdentityID} {
					if aliases[key] != "" {
						id = aliases[key]
						break
					}
				}
				if id != "" {
					pendingAliases = append(pendingAliases, observationAliases(o, id, aliases)...)
					res.Duplicate++
					continue
				}
			}
			m := o.Message(merge)
			var native any
			if o.NativeMessageID != "" {
				native = o.NativeMessageID
			}
			r, err := stmt.ExecContext(ctx, o.ObservationID, o.Platform, o.Collection, o.SourceRoom, m.Room, native, m.ActorLabel,
				m.ActorAccount, o.Content, o.ParsedTime, o.RecordKind, ts)
			if err != nil {
				return err
			}
			if n, _ := r.RowsAffected(); n > 0 {
				res.Inserted++
				if !s.tidb() && o.Platform == "discord" && o.RecordKind == corpus.HumanText && m.ActorAccount != "" {
					nameSources = append(nameSources, o.ObservationID)
				}
				a := appends[m.Room]
				key := messageKey(m)
				if a.n == 0 || key.before(a.first) {
					a.first = key
				}
				a.n++
				appends[m.Room] = a
				if !s.tidb() && o.Platform == "kakao" {
					pendingAliases = append(pendingAliases, observationAliases(o, o.ObservationID, aliases)...)
				}
				rooms[m.Room] = o
			} else {
				res.Duplicate++
			}
		}
		if err := saveObservationAliases(ctx, tx, pendingAliases); err != nil {
			return err
		}
		// rooms in a fixed order: two concurrent imports on TiDB never lock them in opposite orders
		names := make([]string, 0, len(rooms))
		for room := range rooms {
			names = append(names, room)
		}
		sort.Strings(names)
		for _, room := range names {
			o := rooms[room]
			if err := s.UpsertRoom(tx, Room{Room: room, Collection: o.Collection, Platform: o.Platform}); err != nil {
				return err
			}
			// Commit the append watermark with the source rows; wall-clock import
			// times can tie or arrive late and cannot prove an unchanged prefix.
			if err := s.recordLeafAppends(ctx, tx, room, appends[room]); err != nil {
				return err
			}
		}
		return ctx.Err()
	})
	if err != nil {
		return InsertResult{}, err
	} // this batch rolled back
	if err := indexCapturedAliases(ctx, s.DB, nameSources); err != nil {
		slog.Warn("could not record display name; captured observation already committed")
	}
	return res, nil
}
