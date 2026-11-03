package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"masamong/internal/memory/search"
	"masamong/internal/storage"
)

// Message vectors are an optional second index over single messages (settings memory.message_vectors).
// A leaf of eight lines averages its lines into one vector, which can bury a one-line fact; scoring the
// messages themselves finds the leaf that holds it. The vector is never shown: search still returns
// whole leaves with their context.
//
// A row holds one message's vector for one embedding space, as int8 components plus one float32 scale
// (search.EncodeQuantizedRow: dims+4 bytes, the quantisation the in-memory matrix uses, so the index loads
// it unchanged and several rows share a database page). It is derived from the message's text, so every
// path that changes or erases the text removes it (invalidateLeaves, DeleteRoom). The content never
// changes under a vector (an edit erases it), but the speaker's label, which is part of the embedded
// text, can be renamed without touching the message; the row keeps the label it was made with so the
// pending query and the loader can tell a renamed message from a current one and embed it again.
//
// The table is its own cache: a row is keyed by the message and carries the hash of the text it was made
// from, so message vectors are not written to the embedding cache (memory_embed_cache) at all.
//
// fmt marks the format of vec. Rows written by the first version of this table are float16 (fmt 0); they
// are read as they are and quantised when the index is loaded (no new embedding is needed), and a message
// saved again is stored in the current format.

// messageVectorDDL and tidbMessageVectorDDL are applied by Open next to the other memory tables.
var messageVectorDDL = []string{
	`CREATE TABLE IF NOT EXISTS memory_message_vectors (
		observation_id TEXT PRIMARY KEY,
		room TEXT NOT NULL,
		embed_space TEXT NOT NULL,
		text_hash TEXT NOT NULL,
		label TEXT NOT NULL DEFAULT '',
		vec BLOB NOT NULL,
		updated_at TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_memory_message_vectors_room ON memory_message_vectors(room, embed_space)`,
}

var tidbMessageVectorDDL = []string{
	`CREATE TABLE IF NOT EXISTS memory_message_vectors (
		observation_id VARCHAR(80) NOT NULL PRIMARY KEY,
		room VARCHAR(160) NOT NULL,
		embed_space VARCHAR(160) NOT NULL,
		text_hash VARCHAR(64) NOT NULL,
		label VARCHAR(255) NOT NULL DEFAULT '',
		vec MEDIUMBLOB NOT NULL,
		updated_at VARCHAR(64) NOT NULL,
		KEY idx_memory_message_vectors_room (room, embed_space)
	)`,
}

// Formats of a stored message vector (the fmt column).
const (
	MessageFormatFloat16 = 0 // little-endian float16 components (the first version of the table)
	MessageFormatInt8    = 1 // search.EncodeQuantizedRow
)

// messageCacheCleanupDDL records that the cache copies of the first version were removed. Adding a column
// and deleting rows are separate statements (a DDL statement commits by itself, on TiDB too), so a column
// that exists says nothing about the delete having run; the marker is written only after it succeeded and
// Open repeats the delete until it exists.
const messageCacheCleanupDDL = `CREATE TABLE IF NOT EXISTS memory_message_vector_migration(id INTEGER PRIMARY KEY, done INTEGER NOT NULL)`

// migrateMessageVectors brings a message vector table made by an earlier version up to date: the label
// column (rows made before it have an empty label and are embedded again once), the format column (rows
// made before it are float16), and the removal of the embedding-cache copies the first version kept of
// every message vector (kind "message"), which nothing reads or writes any more.
func migrateMessageVectors(db *storage.Store) error {
	for _, c := range []struct{ name, sqlite, tidb string }{
		{"label", `TEXT NOT NULL DEFAULT ''`, `VARCHAR(255) NOT NULL DEFAULT ''`},
		{"fmt", `INTEGER NOT NULL DEFAULT 0`, `INT NOT NULL DEFAULT 0`},
	} {
		if db.HasColumn("memory_message_vectors", c.name) {
			continue
		}
		def := c.sqlite
		if db.Backend == storage.BackendTiDB {
			def = c.tidb
		}
		if _, err := db.DB.Exec(`ALTER TABLE memory_message_vectors ADD COLUMN ` + c.name + ` ` + def); err != nil {
			return fmt.Errorf("memory migration: message vector %s: %w", c.name, err)
		}
	}
	return cleanMessageCacheCopies(db)
}

// cleanMessageCacheCopies deletes the message rows of memory_embed_cache once. It never runs again after
// the marker is written, so a later row of that kind is left alone; a delete that fails (or a process that
// stops before the marker) leaves no marker and the next Open tries again.
func cleanMessageCacheCopies(db *storage.Store) error {
	if _, err := db.DB.Exec(messageCacheCleanupDDL); err != nil {
		return fmt.Errorf("memory migration: message vector cache marker: %w", err)
	}
	var done int
	if db.DB.QueryRow(`SELECT done FROM memory_message_vector_migration WHERE id=1`).Scan(&done) == nil && done == 1 {
		return nil
	}
	if _, err := db.DB.Exec(`DELETE FROM memory_embed_cache WHERE kind='message'`); err != nil {
		return fmt.Errorf("memory migration: message vector cache copies: %w", err)
	}
	mark := `INSERT INTO memory_message_vector_migration(id,done) VALUES(1,1) ON CONFLICT(id) DO UPDATE SET done=1`
	if db.Backend == storage.BackendTiDB {
		mark = `REPLACE INTO memory_message_vector_migration(id,done) VALUES(1,1)`
	}
	if _, err := db.DB.Exec(mark); err != nil {
		return fmt.Errorf("memory migration: message vector cache marker: %w", err)
	}
	return nil
}

// MessageText is the text a message is embedded as: the speaker's label and the trimmed content. The
// label is part of it because a question often names the speaker ("what did X say about …").
func MessageText(actorLabel, content string) string {
	return actorLabel + ": " + strings.TrimSpace(content)
}

// MessageTextHash identifies the text a message vector was made from.
func MessageTextHash(text string) string { return hashText(text) }

// PendingMessage is a message of a closed leaf that still needs a vector in the space.
type PendingMessage struct {
	ObservationID string
	Room          string
	Text          string // MessageText of the stored message
	TextHash      string
}

// MessageVector is one stored message vector. A save always writes search.EncodeQuantizedRow bytes
// (MessageFormatInt8); a scan reports the format the row has.
type MessageVector struct {
	ObservationID string
	Room          string
	Space         string
	TextHash      string
	Label         string
	UpdatedAt     string
	Bytes         int // byte length without loading the BLOB in a metadata scan
	Format        int
	Vec           []byte
	// Current is set by a scan: the message exists and still carries the speaker label the vector was made
	// with (an edit or erasure removes the vector itself, so the label is the only thing that can drift).
	Current bool
}

// pendingMessageQuery selects the messages of closed leaves of the active recipe that have no vector
// in the space, or whose vector was made with another speaker label, in a stable order. Messages that are
// deleted (content erased) never qualify.
//
// The CROSS JOINs fix the join order (SQLite never reorders them): leaves outside, their ids, then each
// message by primary key, so the cost grows with the number of messages. Left to itself the planner put
// the messages outside and walked every closed leaf's id list for each one, which is messages × leaves:
// 49 s at 10,000 messages, during which the single writer connection was held and the whole bot waited.
func pendingMessageQuery(rooms []string, space string, recipe []string) (string, []any) {
	q := `SELECT o.observation_id, o.room, o.actor_label, o.content
		FROM memory_leaves CROSS JOIN json_each(memory_leaves.observation_ids) j
		CROSS JOIN memory_observations o ON o.observation_id=j.value
		LEFT JOIN memory_message_vectors mv ON mv.observation_id=o.observation_id
		WHERE ` + activeLeaves + ` AND memory_leaves.closed=1 AND o.deleted_at='' AND o.content<>''
		AND (mv.observation_id IS NULL OR mv.embed_space<>? OR mv.label<>o.actor_label)`
	args := []any{space}
	if len(rooms) > 0 {
		q += ` AND memory_leaves.room IN (` + strings.TrimSuffix(strings.Repeat("?,", len(rooms)), ",") + `)`
		args = append(args, toAny(rooms)...)
	}
	if len(recipe) > 0 {
		q += ` AND memory_leaves.recipe=?`
		args = append(args, recipe[0])
	}
	return q + ` GROUP BY o.observation_id ORDER BY o.room, o.observation_id`, args
}

// PendingMessages lists the messages of the rooms' closed leaves that have no vector in the space yet,
// limit at most (0 = all). Messages too short to carry meaning (search.MessageEligible) are left out
// here, so they are never picked again; the leaves of an open tail wait until they close. rooms empty =
// every room. It reads through the read-only pool: the scan covers every closed leaf, and on the writer
// connection (one for the whole process) it would make every answer and capture wait. SaveMessageVectors
// checks each message again before storing, so a snapshot that is a moment old is safe.
func (s *Store) PendingMessages(rooms []string, space string, limit int, recipe ...string) ([]PendingMessage, error) {
	return s.pendingMessages(rooms, space, limit, nil, recipe)
}

// PendingMessagesAmong is PendingMessages restricted to the observations named in among, without a limit
// (a batch preparation's workset).
func (s *Store) PendingMessagesAmong(rooms []string, space string, among map[string]bool, recipe ...string) ([]PendingMessage, error) {
	if among == nil {
		among = map[string]bool{}
	}
	return s.pendingMessages(rooms, space, 0, among, recipe)
}

func (s *Store) pendingMessages(rooms []string, space string, limit int, among map[string]bool, recipe []string) ([]PendingMessage, error) {
	q, args := pendingMessageQuery(rooms, space, recipe)
	rows, err := s.DB.Reader().Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingMessage
	for rows.Next() {
		var p PendingMessage
		var label, content string
		if err := rows.Scan(&p.ObservationID, &p.Room, &label, &content); err != nil {
			return nil, err
		}
		if among != nil && !among[p.ObservationID] {
			continue
		}
		if !search.MessageEligible(strings.TrimSpace(content)) {
			continue
		}
		p.Text = MessageText(label, content)
		p.TextHash = MessageTextHash(p.Text)
		out = append(out, p)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, rows.Err()
}

// CountPendingMessages is how many messages PendingMessages would list (the console's and doctor's
// "waiting" number), read like PendingMessages through the read-only pool.
func (s *Store) CountPendingMessages(rooms []string, space string, recipe ...string) (int, error) {
	return s.CountPendingMessagesContext(context.Background(), rooms, space, recipe...)
}

// CountPendingMessagesContext is CountPendingMessages that stops when ctx ends (a count of a long history
// takes seconds on a small machine; shutdown should not wait for it).
func (s *Store) CountPendingMessagesContext(ctx context.Context, rooms []string, space string, recipe ...string) (int, error) {
	// IN deduplicates observation ids before fetching pending text. Unlike the list query it
	// needs neither a GROUP BY of four text columns nor an ORDER BY; current vectors return no text.
	leafQ := `SELECT j.value FROM memory_leaves CROSS JOIN json_each(memory_leaves.observation_ids) j
		WHERE ` + activeLeaves + ` AND memory_leaves.closed=1`
	args := []any{space}
	if len(rooms) > 0 {
		leafQ += ` AND memory_leaves.room IN (` + strings.TrimSuffix(strings.Repeat("?,", len(rooms)), ",") + `)`
		args = append(args, toAny(rooms)...)
	}
	if len(recipe) > 0 {
		leafQ += ` AND memory_leaves.recipe=?`
		args = append(args, recipe[0])
	}
	q := `SELECT o.content FROM memory_observations o
		LEFT JOIN memory_message_vectors mv ON mv.observation_id=o.observation_id
		WHERE o.deleted_at='' AND o.content<>''
		AND (mv.observation_id IS NULL OR mv.embed_space<>? OR mv.label<>o.actor_label)
		AND o.observation_id IN (` + leafQ + `)`
	rows, err := s.DB.Reader().QueryContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			return 0, err
		}
		if search.MessageEligible(strings.TrimSpace(content)) {
			n++
		}
	}
	return n, rows.Err()
}

// MessageVectorCounts returns how many message vectors are stored in total and how many of them are in
// the space search reads.
func (s *Store) MessageVectorCounts(space string) (total, inSpace int, err error) {
	err = s.DB.Reader().QueryRow(`SELECT COUNT(*), COALESCE(SUM(CASE WHEN embed_space=? THEN 1 ELSE 0 END),0) FROM memory_message_vectors`, space).
		Scan(&total, &inSpace)
	return total, inSpace, err
}

// MessageVectorSpaces lists the embedding spaces message vectors are stored in, with how many each holds.
func (s *Store) MessageVectorSpaces() (map[string]int, error) {
	rows, err := s.DB.Reader().Query(`SELECT embed_space, COUNT(*) FROM memory_message_vectors GROUP BY embed_space`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var space string
		var n int
		if err := rows.Scan(&space, &n); err != nil {
			return nil, err
		}
		out[space] = n
	}
	return out, rows.Err()
}

// SaveMessageVectors stores message vectors (search.EncodeQuantizedRow bytes), replacing a message's
// earlier one. They go only to this table, never to the embedding cache. A vector is kept only
// if its message still exists and still reads as the text it was made from: one edited, erased or
// blocked while the embedding was in flight is dropped, so a slow embedding can never bring erased text
// back. It returns how many were stored.
func (s *Store) SaveMessageVectors(items []MessageVector) (int, error) {
	saved := 0
	err := s.DB.WithWriter(func(tx *sql.Tx) error {
		var err error
		saved, err = s.saveMessageVectorsTx(tx, items)
		return err
	})
	return saved, err
}

func (s *Store) saveMessageVectorsTx(tx *sql.Tx, items []MessageVector) (int, error) {
	saved := 0
	for _, it := range items {
		var room, label, content, deleted string
		err := tx.QueryRow(`SELECT room, actor_label, content, deleted_at FROM memory_observations WHERE observation_id=?`, it.ObservationID).
			Scan(&room, &label, &content, &deleted)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if deleted != "" || MessageTextHash(MessageText(label, content)) != it.TextHash {
			continue
		}
		if !s.tidb() {
			current, err := ObservationCurrent(tx, it.ObservationID)
			if err != nil {
				return 0, err
			}
			if !current {
				continue
			}
		}
		q := `INSERT INTO memory_message_vectors(observation_id, room, embed_space, text_hash, label, fmt, vec, updated_at) VALUES(?,?,?,?,?,?,?,?)
			ON CONFLICT(observation_id) DO UPDATE SET room=excluded.room, embed_space=excluded.embed_space, text_hash=excluded.text_hash, label=excluded.label, fmt=excluded.fmt, vec=excluded.vec, updated_at=excluded.updated_at`
		if s.tidb() {
			q = `INSERT INTO memory_message_vectors(observation_id, room, embed_space, text_hash, label, fmt, vec, updated_at) VALUES(?,?,?,?,?,?,?,?)
				ON DUPLICATE KEY UPDATE room=VALUES(room), embed_space=VALUES(embed_space), text_hash=VALUES(text_hash), label=VALUES(label), fmt=VALUES(fmt), vec=VALUES(vec), updated_at=VALUES(updated_at)`
		}
		if _, err := tx.Exec(q, it.ObservationID, room, it.Space, it.TextHash, label, MessageFormatInt8, it.Vec, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return 0, err
		}
		saved++
	}
	return saved, nil
}

// ScanMessageVectors calls fn for every stored message vector of the rooms in the space, ordered by
// observation ID (a stable row order). Current tells the caller whether the vector still describes the
// message as it is stored now: the join reads the live row, so a speaker label renamed behind a running
// engine's message snapshot is noticed here, not in that snapshot.
func (s *Store) ScanMessageVectors(rooms []string, space string, fn func(MessageVector) error) error {
	return scanMessageVectors(s.DB.Reader(), rooms, nil, space, false, fn)
}

// ScanColdMessageVectors reads each BLOB and its live identity together. A first
// load cannot reuse rows, so a metadata scan followed by 500-ID lookups only
// doubles joins and random page reads. Row order is deliberately unspecified:
// the engine projects rows in canonical leaf/message order before publication.
// Vec is valid only during fn: the loader converts it immediately into owned int8
// storage, avoiding database/sql's second full BLOB copy for every message.
func (s *Store) ScanColdMessageVectors(rooms []string, space string, fn func(MessageVector) error) error {
	return scanMessageVectorRows(s.DB.Reader(), rooms, nil, space, false, true, fn)
}

// ScanMessageVectorMeta checks every live message and its stored vector identity without copying
// vector bytes into the Go heap. The live join is necessary even if the engine reuses room messages:
// a speaker rename can invalidate a vector without changing the room's message snapshot.
func (s *Store) ScanMessageVectorMeta(rooms []string, space string, fn func(MessageVector) error) error {
	return scanMessageVectors(s.DB.Reader(), rooms, nil, space, true, fn)
}

// MessageVectorReader keeps metadata and selective BLOB reads in the same read-only snapshot.
// Without it, a re-embedding between the two queries could remove a valid row from the new index.
type MessageVectorReader struct{ db messageVectorQuerier }

type messageVectorQuerier interface {
	Query(string, ...any) (*sql.Rows, error)
}

// ReadMessageVectors holds a reader connection, never the process's single writer. A WAL writer may
// continue while the snapshot is read; the loader observes the same coherent version as a full scan.
func (s *Store) ReadMessageVectors(fn func(*MessageVectorReader) error) error {
	tx, err := s.DB.Reader().Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return fn(&MessageVectorReader{db: tx})
}

func (r *MessageVectorReader) ScanMessageVectorMeta(rooms []string, space string, fn func(MessageVector) error) error {
	return scanMessageVectors(r.db, rooms, nil, space, true, fn)
}

// MessageVectors reads only the requested new or changed vectors, at most 500 per query.
func (s *Store) MessageVectors(ids []string, space string) (map[string]MessageVector, error) {
	return (&MessageVectorReader{db: s.DB.Reader()}).MessageVectors(ids, space)
}

func (r *MessageVectorReader) MessageVectors(ids []string, space string) (map[string]MessageVector, error) {
	out := make(map[string]MessageVector, len(ids))
	for lo := 0; lo < len(ids); lo += 500 {
		if err := scanMessageVectors(r.db, nil, ids[lo:min(len(ids), lo+500)], space, false, func(v MessageVector) error {
			out[v.ObservationID] = v
			return nil
		}); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func scanMessageVectors(db messageVectorQuerier, rooms, ids []string, space string, meta bool, fn func(MessageVector) error) error {
	return scanMessageVectorRows(db, rooms, ids, space, meta, false, fn)
}

func scanMessageVectorRows(db messageVectorQuerier, rooms, ids []string, space string, meta, cold bool, fn func(MessageVector) error) error {
	if len(rooms) == 0 && len(ids) == 0 {
		return nil
	}
	filter, values := "mv.room", rooms
	if len(ids) > 0 {
		filter, values = "mv.observation_id", ids
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(values)), ",")
	args := append([]any{space}, toAny(values)...)
	blob := "mv.vec"
	if meta {
		blob = "length(mv.vec)"
	}
	order := " ORDER BY mv.observation_id"
	if cold {
		order = ""
	}
	rows, err := db.Query(`SELECT mv.observation_id, mv.room, mv.text_hash, mv.label, mv.updated_at, mv.fmt, `+blob+`,
		CASE WHEN o.actor_label=mv.label AND o.deleted_at='' THEN 1 ELSE 0 END
		FROM memory_message_vectors mv JOIN memory_observations o ON o.observation_id=mv.observation_id
		WHERE mv.embed_space=? AND `+filter+` IN (`+ph+`)`+order, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		v := MessageVector{Space: space}
		var raw sql.RawBytes
		var cur int
		var value any = &v.Vec
		if meta {
			value = &v.Bytes
		} else if cold {
			value = &raw
		}
		if err := rows.Scan(&v.ObservationID, &v.Room, &v.TextHash, &v.Label, &v.UpdatedAt, &v.Format, value, &cur); err != nil {
			return err
		}
		if !meta {
			if cold {
				v.Vec = raw
			}
			v.Bytes = len(v.Vec)
		}
		v.Current = cur == 1
		if err := fn(v); err != nil {
			return err
		}
	}
	return rows.Err()
}

// CountUnleafedMessages counts, for the console's estimate, the messages of the rooms that would be
// embedded if they had a place in a closed leaf of the active recipe but have none yet: the room was never
// split into leaves (text just imported), its recipe changed, or they sit in the open tail. Only messages
// with at least search.MinMessageRunes meaningful characters count, so it is an upper bound of what a
// preparation adds. rooms empty = every room. It reads every message, so it goes through the read-only pool.
func (s *Store) CountUnleafedMessages(rooms []string, recipe ...string) (int, error) {
	leafQ := `SELECT DISTINCT j.value AS oid FROM memory_leaves, json_each(memory_leaves.observation_ids) j WHERE ` + activeLeaves + ` AND memory_leaves.closed=1`
	var args []any
	if len(recipe) > 0 {
		leafQ += ` AND memory_leaves.recipe=?`
		args = append(args, recipe[0])
	}
	q := `SELECT o.content FROM memory_observations o LEFT JOIN (` + leafQ + `) c ON c.oid=o.observation_id
		WHERE c.oid IS NULL AND o.deleted_at='' AND o.content<>'' AND o.room IN (SELECT room FROM memory_rooms)`
	if len(rooms) > 0 {
		q += ` AND o.room IN (` + strings.TrimSuffix(strings.Repeat("?,", len(rooms)), ",") + `)`
		args = append(args, toAny(rooms)...)
	}
	rows, err := s.DB.Reader().Query(q, args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			return 0, err
		}
		if search.MessageEligible(strings.TrimSpace(content)) {
			n++
		}
	}
	return n, rows.Err()
}

// deleteMessageVectors removes the vectors of messages whose text changed or was erased, and the cached
// copies tied to those messages. ids is the JSON array invalidateLeaves already built.
func deleteMessageVectors(tx *sql.Tx, rawIDs string) error {
	if _, err := tx.Exec(`DELETE FROM memory_message_vectors WHERE observation_id IN (SELECT value FROM json_each(?))`, rawIDs); err != nil {
		return fmt.Errorf("message vectors: %w", err)
	}
	return nil
}
