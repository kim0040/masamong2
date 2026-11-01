package store

import "strings"

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
