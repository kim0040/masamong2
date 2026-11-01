package store

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"masamong/internal/storage"
)

// Room memory tables (ADR-17, docs/plan-memory-conversation.md §5.4). SQLite only, like every table added
// after the TiDB backend was gated off.
//
//   - channel_episode: one episode of a room (a plan, a decision, something that happened, a running joke,
//     a fact). participants are PersonRef values (Kakao speakers: kakao:<room>:<label>); segment lists the
//     observation IDs the episode was extracted from, so it can be made again from the raw messages.
//     regen=1 queues that regeneration.
//   - episode_item: one short sentence (≤120 characters, people as {@ref}) with its evidence (observation
//     IDs, at least one). state is active, hidden (a cited message was deleted) or dirty (edited).
//   - episode_leaf: the memory leaves that hold an episode's evidence, so search can show an episode and
//     its leaf once and a leaf's invalidation reaches the episode.
//   - channel_state: a room's live state (L1) as one JSON body with its evidence.
//
// Every item and state row is registered in evidence_refs (holder kinds episode_item and channel_state),
// which is how deletions, edits and withdrawn consent reach them (application/episodes.go).
var episodeDDL = []string{
	`CREATE TABLE IF NOT EXISTS channel_episode (
		id TEXT PRIMARY KEY,
		guild_id TEXT NOT NULL,
		channel_id TEXT NOT NULL,
		room TEXT NOT NULL,
		kind TEXT NOT NULL,
		title TEXT NOT NULL,
		status TEXT NOT NULL,
		due_at TEXT NOT NULL DEFAULT '',
		when_text TEXT NOT NULL DEFAULT '',
		importance INTEGER NOT NULL DEFAULT 2,
		source TEXT NOT NULL,
		participants TEXT NOT NULL DEFAULT '[]',
		segment TEXT NOT NULL DEFAULT '[]',
		first_seen TEXT NOT NULL,
		last_seen TEXT NOT NULL,
		expires_at TEXT NOT NULL DEFAULT '',
		supersedes_id TEXT NOT NULL DEFAULT '',
		regen INTEGER NOT NULL DEFAULT 0,
		updated_at TEXT NOT NULL,
		revision INTEGER NOT NULL DEFAULT 1
	)`,
	`CREATE INDEX IF NOT EXISTS idx_channel_episode_channel ON channel_episode(guild_id, channel_id, last_seen)`,
	`CREATE INDEX IF NOT EXISTS idx_channel_episode_room ON channel_episode(room, status)`,
	`CREATE INDEX IF NOT EXISTS idx_channel_episode_expiry ON channel_episode(expires_at) WHERE expires_at<>''`,
	`CREATE INDEX IF NOT EXISTS idx_channel_episode_regen ON channel_episode(regen) WHERE regen<>0`,
	`CREATE TABLE IF NOT EXISTS episode_item (
		id TEXT PRIMARY KEY,
		episode_id TEXT NOT NULL,
		kind TEXT NOT NULL,
		text TEXT NOT NULL,
		evidence TEXT NOT NULL,
		by_refs TEXT NOT NULL DEFAULT '[]',
		state TEXT NOT NULL DEFAULT 'active',
		created_at TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_episode_item_episode ON episode_item(episode_id)`,
	`CREATE TABLE IF NOT EXISTS episode_leaf (
		episode_id TEXT NOT NULL,
		leaf_id TEXT NOT NULL,
		PRIMARY KEY(episode_id, leaf_id)
	) WITHOUT ROWID`,
	`CREATE INDEX IF NOT EXISTS idx_episode_leaf_leaf ON episode_leaf(leaf_id)`,
	`CREATE TABLE IF NOT EXISTS channel_state (
		guild_id TEXT NOT NULL,
		channel_id TEXT NOT NULL,
		room TEXT NOT NULL,
		body TEXT NOT NULL,
		evidence TEXT NOT NULL,
		anchor_observation TEXT NOT NULL DEFAULT '',
		anchor_time TEXT NOT NULL DEFAULT '',
		revision INTEGER NOT NULL DEFAULT 1,
		updated_n INTEGER NOT NULL DEFAULT 0,
		updated_at TEXT NOT NULL,
		PRIMARY KEY(guild_id, channel_id)
	)`,
}

func migrateEpisodes(db *storage.Store) error {
	if !db.IsSQLite() {
		return nil
	}
	for i, q := range episodeDDL {
		if _, err := db.DB.Exec(q); err != nil {
			return fmt.Errorf("memory migration: room memory %d: %w", i, err)
		}
	}
	return nil
}

// Holder kinds of room memory in evidence_refs.
const (
	HolderEpisodeItem  = "episode_item"
	HolderChannelState = "channel_state"
)

// StateHolderID names a room state in evidence_refs.
func StateHolderID(guildID, channelID string) string { return "state:" + guildID + ":" + channelID }

// LeavesOf lists the leaves that hold any of the observations (episode_leaf). It runs inside the
// writer's transaction, so it reads only the leaves of the observations' rooms that end on or after
// two days before the earliest of them: a leaf that ended earlier cannot hold them. Reading every leaf's
// list held the writer for seconds on a large memory. The day is cut from the stored time, which comes
// with or without a zone, and the two days cover any zone; a time that does not read reads the room.
func LeavesOf(tx *sql.Tx, observationIDs []string) ([]string, error) {
	if len(observationIDs) == 0 {
		return nil, nil
	}
	raw, _ := json.Marshal(observationIDs)
	rows, err := tx.Query(`WITH ev(id) AS (SELECT value FROM json_each(?)),
		r AS (SELECT room, COALESCE(date(substr(MIN(parsed_time),1,10),'-2 day'),'') AS since FROM memory_observations
			WHERE observation_id IN (SELECT id FROM ev) GROUP BY room)
		SELECT DISTINCT l.leaf_id FROM (
			SELECT l.leaf_id, l.observation_ids FROM r CROSS JOIN memory_leaves l ON l.room=r.room AND l.end_time>=r.since
			UNION ALL SELECT l.leaf_id, l.observation_ids FROM r CROSS JOIN memory_leaves l ON l.room=r.room AND l.end_time=''
		) l, json_each(l.observation_ids) x WHERE x.value IN (SELECT id FROM ev)`, string(raw))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// DeleteRoomMemory removes a room's episodes, their items, leaf links and evidence index, and its state:
// derived rows go with the messages they came from.
func DeleteRoomMemory(tx *sql.Tx, room string) error {
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='channel_episode'`).Scan(&n); err != nil || n == 0 {
		return err
	}
	for _, q := range []string{
		`DELETE FROM evidence_refs WHERE holder_kind='` + HolderEpisodeItem + `' AND holder_id IN (SELECT i.id FROM episode_item i JOIN channel_episode e ON e.id=i.episode_id WHERE e.room=?)`,
		`DELETE FROM episode_item WHERE episode_id IN (SELECT id FROM channel_episode WHERE room=?)`,
		`DELETE FROM episode_leaf WHERE episode_id IN (SELECT id FROM channel_episode WHERE room=?)`,
		`DELETE FROM channel_episode WHERE room=?`,
		`DELETE FROM evidence_refs WHERE holder_kind='` + HolderChannelState + `' AND holder_id IN (SELECT 'state:'||guild_id||':'||channel_id FROM channel_state WHERE room=?)`,
		`DELETE FROM channel_state WHERE room=?`,
	} {
		if _, err := tx.Exec(q, room); err != nil {
			return err
		}
	}
	return nil
}
