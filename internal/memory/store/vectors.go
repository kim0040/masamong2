package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// StoredLeaf is one persisted leaf.
type StoredLeaf struct {
	LeafID         string
	Room           string
	ObservationIDs []string
	Body           string
	BodyHash       string
	Speakers       []string
	EndTime        string
	Closed         bool
	Summary        string
	EmbedSpace     string
	LeafVec        []byte
	SummaryVec     []byte
	// LeafLen and SummaryLen are the vectors' sizes in bytes (LoadLeafMeta leaves the vectors out)
	LeafLen, SummaryLen int
}

// PendingLeaf is a closed leaf still missing a summary and/or vectors.
type PendingLeaf struct {
	LeafID     string
	Body       string
	Summary    string
	NeedLeaf   bool
	NeedSumm   bool
	NeedSumVec bool
	// RefreshLeaf bypasses same-space caches while repairing a changed provider width.
	RefreshLeaf bool
}

// Pending lists closed leaves of the rooms that still need work for this embedding space.
func (s *Store) Pending(rooms []string, space string, withSummaries bool, limit int, recipe ...string) ([]PendingLeaf, error) {
	return s.PendingAtWidth(rooms, space, withSummaries, limit, 0, recipe...)
}

// PendingAtWidth also queues vectors unlike the target leaf width. Zero preserves usual pending rules.
func (s *Store) PendingAtWidth(rooms []string, space string, withSummaries bool, limit, dims int, recipe ...string) ([]PendingLeaf, error) {
	return s.pendingAtWidth(rooms, space, withSummaries, limit, dims, nil, recipe...)
}

// PendingAmong is PendingAtWidth restricted to the leaves named in among, without a limit: the pending
// work of a set chosen earlier (a batch preparation's workset). Other leaves are skipped as they are read.
func (s *Store) PendingAmong(rooms []string, space string, withSummaries bool, among map[string]bool, dims int, recipe ...string) ([]PendingLeaf, error) {
	if among == nil {
		among = map[string]bool{}
	}
	return s.pendingAtWidth(rooms, space, withSummaries, 0, dims, among, recipe...)
}

func (s *Store) pendingAtWidth(rooms []string, space string, withSummaries bool, limit, dims int, among map[string]bool, recipe ...string) ([]PendingLeaf, error) {
	if len(rooms) == 0 {
		return nil, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(rooms)), ",")
	needLeaf := `leaf_vec IS NULL OR embed_space<>?`
	args := []any{space}
	refresh := `0`
	if dims > 0 {
		needLeaf += ` OR length(leaf_vec)<>?`
		args = append(args, 2*dims)
		refresh = `leaf_vec IS NOT NULL AND embed_space=? AND length(leaf_vec)<>?`
		args = append(args, space, 2*dims)
	}
	for _, r := range rooms {
		args = append(args, r)
	}
	// a summary vector of another length than the leaf's is from an earlier dimensionality
	q := `SELECT leaf_id, body, summary, ` + needLeaf + `, ` + refresh + `,
		CASE WHEN summary_vec IS NULL OR length(summary_vec)<>length(leaf_vec) THEN 1 ELSE 0 END
		FROM memory_leaves WHERE ` + activeLeaves + ` AND closed=1 AND room IN (` + ph + `) AND (` + needLeaf
	args = append(args, space)
	if dims > 0 {
		args = append(args, 2*dims)
	}
	if withSummaries {
		q += ` OR summary='' OR summary_vec IS NULL OR length(summary_vec)<>length(leaf_vec)`
	}
	q += `)`
	if len(recipe) > 0 {
		q += ` AND recipe=?`
		args = append(args, recipe[0])
	}
	q += ` ORDER BY room, leaf_id`
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := s.DB.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingLeaf
	for rows.Next() {
		var p PendingLeaf
		var needLeaf, needSumVec bool
		if err := rows.Scan(&p.LeafID, &p.Body, &p.Summary, &needLeaf, &p.RefreshLeaf, &needSumVec); err != nil {
			return nil, err
		}
		if among != nil && !among[p.LeafID] {
			continue
		}
		p.NeedLeaf = needLeaf
		p.NeedSumm = withSummaries && p.Summary == ""
		p.NeedSumVec = withSummaries && (needSumVec || p.NeedSumm)
		out = append(out, p)
	}
	return out, rows.Err()
}

// SaveLeafVector stores a leaf vector (float16) for the space. A vector from another space is moved to
// the embedding cache first (switching the model back reuses it without paying again), and the
// leaf's summary vector, which belongs to the old space, is dropped so the next pass re-embeds it.
func (s *Store) SaveLeafVector(id, space string, vec []byte, expectedBody ...string) error {
	return s.DB.WithWriter(func(tx *sql.Tx) error {
		_, err := s.saveLeafVectorTx(tx, id, space, vec, expectedBody)
		return err
	})
}

// saveLeafVectorTx is SaveLeafVector in tx; written is false when the leaf is gone or its text changed.
func (s *Store) saveLeafVectorTx(tx *sql.Tx, id, space string, vec []byte, expectedBody []string) (written bool, err error) {
	if len(expectedBody) > 0 {
		ok, err := leafSnapshotCurrent(tx, id, expectedBody)
		if err != nil || !ok {
			return false, err
		}
	}
	var old, body, summary string
	var oldVec, oldSum []byte
	q := `SELECT embed_space, body, summary, leaf_vec, summary_vec FROM memory_leaves WHERE leaf_id=?`
	if s.tidb() {
		q += ` FOR UPDATE`
	}
	err = tx.QueryRow(q, id).
		Scan(&old, &body, &summary, &oldVec, &oldSum)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if old != space {
		archive := map[string][]byte{}
		if old != "" && len(oldVec) > 0 {
			archive[CacheKey(old, "document", body)] = oldVec
		}
		if old != "" && summary != "" && len(oldSum) > 0 {
			archive[CacheKey(old, "document", summary)] = oldSum
		}
		if err := s.putCachedTx(tx, archive, ""); err != nil {
			return false, err
		}
		oldSum = nil
	}
	if _, err = tx.Exec(`UPDATE memory_leaves SET leaf_vec=?, summary_vec=?, embed_space=?, updated_at=? WHERE leaf_id=?`, vec, oldSum, space, s.now(), id); err != nil {
		return false, err
	}
	return true, nil
}

// SharedLeaf is another leaf with the same text that already has vectors in the space.
type SharedLeaf struct {
	LeafVec    []byte
	Summary    string
	SummaryVec []byte
}

// LeafWithBody finds a leaf of the same text embedded in the space (a leaf re-cut by another recipe or
// re-imported): its vectors are reused instead of embedding the text again.
func (s *Store) LeafWithBody(body, space, except string) (SharedLeaf, bool) {
	var l SharedLeaf
	err := s.DB.DB.QueryRow(`SELECT leaf_vec, summary, summary_vec FROM memory_leaves
		WHERE body_hash=? AND embed_space=? AND leaf_vec IS NOT NULL AND leaf_id<>? LIMIT 1`, hashText(body), space, except).
		Scan(&l.LeafVec, &l.Summary, &l.SummaryVec)
	return l, err == nil && len(l.LeafVec) > 0
}

// ShrinkSpace moves leaves embedded in a larger space of the same model to the target space by
// keeping each vector's first components (Matryoshka/MRL, the same truncation fitDims applies to fresh
// embeddings), so lowering models.embed.dimensions needs no paid re-embedding. It returns the leaves
// moved.
func (s *Store) ShrinkSpace(from, to string, shrink func([]byte) []byte) (int, error) {
	if from == to {
		return 0, nil
	}
	moved := 0
	for {
		rows, err := s.DB.DB.Query(`SELECT leaf_id, leaf_vec, summary_vec FROM memory_leaves
			WHERE embed_space=? AND leaf_vec IS NOT NULL LIMIT 500`, from)
		if err != nil {
			return moved, err
		}
		type upd struct {
			id       string
			leaf, sm []byte
		}
		var batch []upd
		for rows.Next() {
			var u upd
			if err := rows.Scan(&u.id, &u.leaf, &u.sm); err != nil {
				rows.Close()
				return moved, err
			}
			u.leaf = shrink(u.leaf)
			if len(u.sm) > 0 {
				u.sm = shrink(u.sm)
			}
			batch = append(batch, u)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return moved, err
		}
		if len(batch) == 0 {
			return moved, nil
		}
		err = s.DB.WithWriter(func(tx *sql.Tx) error {
			for _, u := range batch {
				if _, err := tx.Exec(`UPDATE memory_leaves SET leaf_vec=?, summary_vec=?, embed_space=?, updated_at=? WHERE leaf_id=? AND embed_space=?`,
					u.leaf, u.sm, to, s.now(), u.id, from); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return moved, err
		}
		moved += len(batch)
	}
}

// Spaces lists the embedding spaces leaves are stored in.
func (s *Store) Spaces() ([]string, error) {
	rows, err := s.DB.Reader().Query(`SELECT DISTINCT embed_space FROM memory_leaves WHERE embed_space<>''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var sp string
		if err := rows.Scan(&sp); err != nil {
			return nil, err
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}

// SaveSummary stores a summary and (optionally) its vector.
func (s *Store) SaveSummary(id, summary string, vec []byte, expectedBody ...string) error {
	return s.DB.WithWriter(func(tx *sql.Tx) error {
		_, err := s.saveSummaryTx(tx, id, summary, vec, expectedBody)
		return err
	})
}

// saveSummaryTx is SaveSummary in tx; written is false when the leaf is gone or its text changed.
func (s *Store) saveSummaryTx(tx *sql.Tx, id, summary string, vec []byte, expectedBody []string) (written bool, err error) {
	ok, err := leafSnapshotCurrent(tx, id, expectedBody)
	if err != nil || !ok {
		return false, err
	}
	if _, err = tx.Exec(`UPDATE memory_leaves SET summary=?, summary_vec=?, updated_at=? WHERE leaf_id=?`, summary, vec, s.now(), id); err != nil {
		return false, err
	}
	return true, nil
}

// LoadLeaves returns every leaf of the rooms that has a vector in the space (the searchable set).
func (s *Store) LoadLeaves(rooms []string, space string, recipe ...string) ([]StoredLeaf, error) {
	if len(rooms) == 0 {
		return nil, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(rooms)), ",")
	args := []any{space}
	for _, r := range rooms {
		args = append(args, r)
	}
	filter := activeLeaves
	if len(recipe) > 0 {
		filter += ` AND recipe=?`
		args = append(args, recipe[0])
	}
	rows, err := s.DB.Reader().Query(`SELECT leaf_id, room, observation_ids, body, body_hash, speakers, end_time, closed, summary, embed_space, leaf_vec, summary_vec
		FROM memory_leaves WHERE leaf_vec IS NOT NULL AND embed_space=? AND room IN (`+ph+`) AND `+filter, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StoredLeaf
	for rows.Next() {
		var l StoredLeaf
		var ids, sp string
		var closed int
		if err := rows.Scan(&l.LeafID, &l.Room, &ids, &l.Body, &l.BodyHash, &sp, &l.EndTime, &closed, &l.Summary, &l.EmbedSpace, &l.LeafVec, &l.SummaryVec); err != nil {
			return nil, err
		}
		l.Closed = closed == 1
		_ = json.Unmarshal([]byte(ids), &l.ObservationIDs)
		_ = json.Unmarshal([]byte(sp), &l.Speakers)
		out = append(out, l)
	}
	return out, rows.Err()
}

// LoadLeafMeta is LoadLeaves without the vectors: rebuilding the index fetches only the vectors it
// does not already hold (LeafVectors), instead of reading every vector of the history again, which
// over a network (TiDB) is hundreds of MB per rebuild for a long history.
func (s *Store) LoadLeafMeta(rooms []string, space string, recipe ...string) ([]StoredLeaf, error) {
	if len(rooms) == 0 {
		return nil, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(rooms)), ",")
	args := []any{space}
	for _, r := range rooms {
		args = append(args, r)
	}
	filter := activeLeaves
	if len(recipe) > 0 {
		filter += ` AND recipe=?`
		args = append(args, recipe[0])
	}
	rows, err := s.DB.Reader().Query(`SELECT leaf_id, room, observation_ids, body, body_hash, speakers, end_time, closed, summary, embed_space,
		length(leaf_vec), COALESCE(length(summary_vec), 0)
		FROM memory_leaves WHERE leaf_vec IS NOT NULL AND embed_space=? AND room IN (`+ph+`) AND `+filter, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StoredLeaf
	for rows.Next() {
		var l StoredLeaf
		var ids, sp string
		var closed int
		if err := rows.Scan(&l.LeafID, &l.Room, &ids, &l.Body, &l.BodyHash, &sp, &l.EndTime, &closed, &l.Summary, &l.EmbedSpace,
			&l.LeafLen, &l.SummaryLen); err != nil {
			return nil, err
		}
		l.Closed = closed == 1
		_ = json.Unmarshal([]byte(ids), &l.ObservationIDs)
		_ = json.Unmarshal([]byte(sp), &l.Speakers)
		out = append(out, l)
	}
	return out, rows.Err()
}

// LeafVectors returns the leaf and summary vectors of the given leaves, keyed by leaf ID.
func (s *Store) LeafVectors(ids []string) (map[string][2][]byte, error) {
	out := make(map[string][2][]byte, len(ids))
	for lo := 0; lo < len(ids); lo += 500 {
		chunk := ids[lo:min(len(ids), lo+500)]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		ph := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
		rows, err := s.DB.Reader().Query(`SELECT leaf_id, leaf_vec, summary_vec FROM memory_leaves WHERE leaf_id IN (`+ph+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			var lv, sv []byte
			if err := rows.Scan(&id, &lv, &sv); err != nil {
				rows.Close()
				return nil, err
			}
			out[id] = [2][]byte{lv, sv}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// SpaceCounts returns how many leaves have a vector in total and how many of them are in space (the
// embedding space search reads). After the embedding model changes, the rest wait for re-indexing.
func (s *Store) SpaceCounts(space string) (total, inSpace int, err error) {
	err = s.DB.Reader().QueryRow(`SELECT COUNT(*), COALESCE(SUM(CASE WHEN embed_space=? THEN 1 ELSE 0 END),0) FROM memory_leaves WHERE leaf_vec IS NOT NULL AND `+activeLeaves, space).
		Scan(&total, &inSpace)
	return total, inSpace, err
}

// RoomText is how much embedded text a room holds.
type RoomText struct {
	Leaves int
	Chars  int64
}

// EmbeddedRooms returns, per room, the leaves that have a vector and the characters of their bodies
// and summaries: what another embedding model would have to embed again.
func (s *Store) EmbeddedRooms() (map[string]RoomText, error) {
	rows, err := s.DB.Reader().Query(`SELECT room, COUNT(*), COALESCE(SUM(LENGTH(body)+LENGTH(summary)),0) FROM memory_leaves WHERE leaf_vec IS NOT NULL AND ` + activeLeaves + ` GROUP BY room`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]RoomText{}
	for rows.Next() {
		var room string
		var t RoomText
		if err := rows.Scan(&room, &t.Leaves, &t.Chars); err != nil {
			return nil, err
		}
		out[room] = t
	}
	return out, rows.Err()
}

// LeafVectorWidths reads the range of active float16 widths without fetching vector BLOBs or
// allocating the history's metadata. Both are zero before the first embedded leaf.
func (s *Store) LeafVectorWidths(space, recipe string) (lo, hi int, err error) {
	err = s.DB.Reader().QueryRow(`SELECT COALESCE(MIN(length(leaf_vec)),0), COALESCE(MAX(length(leaf_vec)),0)
		FROM memory_leaves WHERE embed_space=? AND recipe=? AND `+activeLeaves+`
		AND leaf_vec IS NOT NULL AND length(leaf_vec)>0 AND length(leaf_vec)%2=0`, space, recipe).Scan(&lo, &hi)
	return lo / 2, hi / 2, err
}

// LeafVectorMode chooses the most common active width; ties choose the smaller width consistently.
func (s *Store) LeafVectorMode(space, recipe string) (int, error) {
	var bytes int
	err := s.DB.Reader().QueryRow(`SELECT length(leaf_vec) FROM memory_leaves
		WHERE embed_space=? AND recipe=? AND `+activeLeaves+`
		AND leaf_vec IS NOT NULL AND length(leaf_vec)>0 AND length(leaf_vec)%2=0
		GROUP BY length(leaf_vec) ORDER BY COUNT(*) DESC, length(leaf_vec) LIMIT 1`, space, recipe).Scan(&bytes)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return bytes / 2, err
}

// LeafWidthRepairRooms reads only room names, so background passes can repair quiet rooms too.
func (s *Store) LeafWidthRepairRooms(space, recipe string, dims int) ([]string, error) {
	rows, err := s.DB.Reader().Query(`SELECT DISTINCT room FROM memory_leaves
		WHERE embed_space=? AND recipe=? AND `+activeLeaves+`
		AND closed=1 AND leaf_vec IS NOT NULL AND length(leaf_vec)<>? ORDER BY room`, space, recipe, 2*dims)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rooms []string
	for rows.Next() {
		var room string
		if err := rows.Scan(&room); err != nil {
			return nil, err
		}
		rooms = append(rooms, room)
	}
	return rooms, rows.Err()
}

// LeafVectorDims is the uniform active leaf width. With mixed widths no message space is comparable
// to all leaves, so it returns zero rather than choosing a differently ordered first row.
func (s *Store) LeafVectorDims(space, recipe string) (int, error) {
	lo, hi, err := s.LeafVectorWidths(space, recipe)
	if lo != hi {
		return 0, err
	}
	return lo, err
}
