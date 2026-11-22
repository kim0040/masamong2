package store

import (
	"slices"
	"strings"

	"masamong/internal/memory/corpus"
)

// ScanLeafVectorBatches is the cold loader's sequential alternative to selective
// primary-key lookups. On SD cards (and Windows) random BLOB page reads dominate
// decoding. A bounded batch keeps the old 500-row memory ceiling while the engine
// restores canonical leaf order after quantization. NOT INDEXED is SQLite-only;
// it avoids walking the room index back into every large table row.
func (s *Store) ScanLeafVectorBatches(rooms []string, space, recipe string, fn func([]StoredLeaf) error) error {
	if len(rooms) == 0 {
		return nil
	}
	args := []any{space, recipe}
	for _, room := range rooms {
		args = append(args, room)
	}
	table := "memory_leaves"
	if !s.tidb() {
		table += " NOT INDEXED"
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(rooms)), ",")
	rows, err := s.DB.Reader().Query(`SELECT leaf_id, leaf_vec, summary_vec FROM `+table+` WHERE leaf_vec IS NOT NULL AND embed_space=? AND recipe=? AND room IN (`+ph+`) AND `+activeLeaves, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	batch := make([]StoredLeaf, 0, 500)
	for rows.Next() {
		var l StoredLeaf
		if err := rows.Scan(&l.LeafID, &l.LeafVec, &l.SummaryVec); err != nil {
			return err
		}
		batch = append(batch, l)
		if len(batch) == 500 {
			if err := fn(batch); err != nil {
				return err
			}
			clear(batch)
			batch = batch[:0]
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(batch) > 0 {
		return fn(batch)
	}
	return nil
}

// Messages returns every stored message of the rooms, room-ordered (corpus.OrderByRoom order).
func (s *Store) Messages(rooms []string) ([]corpus.Message, error) {
	return s.LoadMessages(rooms, 0)
}

// LoadMessages uses the already-read room count only as a capacity hint. Reserving
// once avoids repeatedly copying the whole message history during a cold build;
// concurrent inserts/deletes still determine the actual number of returned rows.
func (s *Store) LoadMessages(rooms []string, capacity int) ([]corpus.Message, error) {
	if len(rooms) == 0 {
		return nil, nil
	}
	return s.loadMessageInputs(rooms, capacity, false)
}

// LoadIdentityInputs reads ordering and native identity fields only. Eligibility is
// selected by the engine using the same metadata/placement path as a full build;
// raw text DISTINCT and message bodies are deliberately absent.
func (s *Store) LoadIdentityInputs(rooms []string) ([]corpus.Message, error) {
	return s.loadMessageInputs(rooms, 0, true)
}

func (s *Store) loadMessageInputs(rooms []string, capacity int, identityOnly bool) ([]corpus.Message, error) {
	if len(rooms) == 0 {
		return nil, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(rooms)), ",")
	args := make([]any, len(rooms))
	for i, r := range rooms {
		args[i] = r
	}
	content := "content"
	if identityOnly {
		content = "''"
	}
	rows, err := s.DB.Reader().Query(`SELECT observation_id, room, source_room, actor_label, actor_account, `+content+`, parsed_time
		FROM memory_observations WHERE room IN (`+ph+`) AND deleted_at=''`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]corpus.Message, 0, max(0, capacity))
	for rows.Next() {
		var m corpus.Message
		if err := rows.Scan(&m.ObservationID, &m.Room, &m.RoomOriginal, &m.ActorLabel, &m.ActorAccount, &m.Content, &m.ParsedTime); err != nil {
			return nil, err
		}
		m.Content = strings.TrimSpace(m.Content)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Store owns this slice: sorting it in place avoids OrderByRoom's additional
	// per-room slices and final history copy. Use exactly its four string keys
	// (not parsed timestamps) so naive times, merges and ties retain parity.
	// WHY unstable sorting is safe: the unique observation_id makes these keys
	// a total order, so stable sorting only adds work on shuffled cold histories.
	slices.SortFunc(out, func(a, b corpus.Message) int {
		if a.Room != b.Room {
			return strings.Compare(a.Room, b.Room)
		}
		if a.ParsedTime != b.ParsedTime {
			return strings.Compare(a.ParsedTime, b.ParsedTime)
		}
		if a.RoomOriginal != b.RoomOriginal {
			return strings.Compare(a.RoomOriginal, b.RoomOriginal)
		}
		return strings.Compare(a.ObservationID, b.ObservationID)
	})
	return out, nil
}
