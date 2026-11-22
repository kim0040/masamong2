package store

import (
	"database/sql"
)

// CacheKey is the embedding cache key for (model, kind, text).
func CacheKey(model, kind, text string) string {
	return hashText(model + "\x1f" + kind + "\x1f" + text)
}

// CachedVector returns a cached float16 vector.
func (s *Store) CachedVector(key string) ([]byte, bool) {
	var v []byte
	if err := s.DB.DB.QueryRow(`SELECT vec FROM memory_embed_cache WHERE cache_key=?`, key).Scan(&v); err != nil {
		return nil, false
	}
	return v, true
}

// PutCachedVectors stores float16 leaf/summary vectors in the cache (existing keys kept).
func (s *Store) PutCachedVectors(items map[string][]byte) error { return s.putCached(items, "") }

// PutQueryVectors caches question embeddings. They grow with every new question, so retention prunes
// them by age (Retention.QueryCacheDays).
func (s *Store) PutQueryVectors(items map[string][]byte) error { return s.putCached(items, "query") }

func (s *Store) putCached(items map[string][]byte, kind string) error {
	if len(items) == 1 { // one question's vector: a single statement, no transaction round trips
		q := `INSERT INTO memory_embed_cache(cache_key, vec, created_at, kind) VALUES(?,?,?,?) ON CONFLICT DO NOTHING`
		if s.tidb() {
			q = `INSERT IGNORE INTO memory_embed_cache(cache_key, vec, created_at, kind) VALUES(?,?,?,?)`
		}
		for k, v := range items {
			_, err := s.DB.DB.Exec(q, k, v, s.now(), kind)
			return err
		}
	}
	return s.DB.WithWriter(func(tx *sql.Tx) error { return s.putCachedTx(tx, items, kind) })
}

func (s *Store) putCachedTx(tx *sql.Tx, items map[string][]byte, kind string) error {
	if len(items) == 0 {
		return nil
	}
	q := `INSERT INTO memory_embed_cache(cache_key, vec, created_at, kind) VALUES(?,?,?,?) ON CONFLICT DO NOTHING`
	if s.tidb() {
		q = `INSERT IGNORE INTO memory_embed_cache(cache_key, vec, created_at, kind) VALUES(?,?,?,?)`
	}
	stmt, err := tx.Prepare(q)
	if err != nil {
		return err
	}
	defer stmt.Close()
	ts := s.now()
	for k, v := range items {
		if _, err := stmt.Exec(k, v, ts, kind); err != nil {
			return err
		}
	}
	return nil
}

// DropCachedDuplicates removes embedding-cache entries that only repeat a vector a leaf already
// stores. Earlier versions cached every leaf and summary vector as well as storing it on the leaf,
// which doubled the vectors on disk; model(space) names the model an entry was keyed by. It works
// through the leaves in batches, calling pause between them, and returns the entries removed.
func (s *Store) DropCachedDuplicates(model func(space string) string, pause func() error) (int64, error) {
	var removed int64
	after := ""
	for {
		rows, err := s.DB.DB.Query(`SELECT leaf_id, embed_space, body, summary, length(leaf_vec), COALESCE(length(summary_vec), 0)
			FROM memory_leaves WHERE leaf_vec IS NOT NULL AND leaf_id > ? ORDER BY leaf_id LIMIT 1000`, after)
		if err != nil {
			return removed, err
		}
		type dup struct {
			key string
			n   int
		}
		var dups []dup
		n := 0
		for rows.Next() {
			var space, body, summary string
			var ln, sn int
			if err := rows.Scan(&after, &space, &body, &summary, &ln, &sn); err != nil {
				rows.Close()
				return removed, err
			}
			n++
			m := model(space)
			dups = append(dups, dup{CacheKey(m, "document", body), ln})
			if summary != "" && sn > 0 {
				dups = append(dups, dup{CacheKey(m, "document", summary), sn})
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return removed, err
		}
		if n == 0 {
			return removed, nil
		}
		err = s.DB.WithWriter(func(tx *sql.Tx) error {
			for _, d := range dups {
				// same length: the entry is this vector, not one kept from another dimensionality
				res, err := tx.Exec(`DELETE FROM memory_embed_cache WHERE cache_key=? AND kind='' AND length(vec)=?`, d.key, d.n)
				if err != nil {
					return err
				}
				k, _ := res.RowsAffected()
				removed += k
			}
			return nil
		})
		if err != nil {
			return removed, err
		}
		if err := pause(); err != nil {
			return removed, err
		}
	}
}
