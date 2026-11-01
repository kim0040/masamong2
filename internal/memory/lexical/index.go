package lexical

import (
	"context"
	"database/sql"
	"fmt"
	"sync"

	_ "modernc.org/sqlite"
)

// Hit is one ranked document. Score is FTS5's bm25(): negative, smaller is better.
type Hit struct {
	DocID string
	Score float64
}

// Index is an in-memory SQLite FTS5 table (unicode61 tokenizer) over n-gram-expanded text — the
// same engine and SQLite version the test sandbox uses, so tokenization (including SQLite's Unicode
// 6.1 category table, which treats later-added emoji as token characters), bm25 and tie order match
// the sandbox exactly. This is a derived, rebuildable search structure, not product storage.
type Index struct {
	N   int // n-gram size (2 = the sandbox "bigram" variant)
	Tok Tokenizer

	mu  sync.Mutex
	db  *sql.DB
	ids []string // doc IDs by rowid-1: the table is contentless and keeps no text or IDs
}

// NewIndex opens an empty in-memory index with the default (sandbox) tokenizer.
func NewIndex(n int) (*Index, error) {
	t := DefaultTokenizer()
	if n > 0 {
		t.N = n
	}
	return NewIndexWith(t)
}

// NewIndexWith opens an empty in-memory index that tokenizes documents and queries with tok.
func NewIndexWith(tok Tokenizer) (*Index, error) {
	if tok.N <= 0 {
		tok.N = 2
	}
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, err
	}
	// Every connection to :memory: is a separate database: pin exactly one.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	// contentless: bm25 and phrase matching need only the inverted index, so the n-gram-expanded text
	// (larger than the messages themselves) is not stored a second time. Scores and tie order are the
	// sandbox's: an UNINDEXED doc_id column never contributed tokens, and rowids keep insertion order.
	if _, err := db.Exec(`CREATE VIRTUAL TABLE docs USING fts5(body, content='', tokenize='unicode61')`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("lexical: fts5 unavailable: %w", err)
	}
	return &Index{N: tok.N, Tok: tok, db: db}, nil
}

// Close releases the in-memory database.
func (ix *Index) Close() error { return ix.db.Close() }

// Doc is one (id, raw text) pair; n-gram expansion happens on insert.
type Doc struct {
	ID   string
	Text string
}

// AddMany indexes documents in one transaction, in order (insertion order = FTS5 rowid order).
func (ix *Index) AddMany(docs []Doc) error {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	tx, err := ix.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO docs(rowid, body) VALUES (?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for i, d := range docs {
		if _, err := stmt.Exec(len(ix.ids)+i+1, ix.Tok.ToNgrams(d.Text)); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, d := range docs {
		ix.ids = append(ix.ids, d.ID)
	}
	return nil
}

// Add indexes one document.
func (ix *Index) Add(docID, text string) error { return ix.AddMany([]Doc{{ID: docID, Text: text}}) }

// Len is the number of indexed documents.
func (ix *Index) Len() int {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	return len(ix.ids)
}

// StorageSource names where StorageBytes got its number, so a report never passes the page count off
// as SQLite's whole allocation or the process's memory.
const StorageSource = "sqlite_page_count_x_page_size"

// StorageStats is the size of the database pages an in-memory FTS5 index occupies.
type StorageStats struct {
	Bytes     int64 // PageCount x PageSize
	PageCount int64
	PageSize  int64
	Source    string // StorageSource
}

// StorageBytes reads the database's page count and page size. The pages are the index's own data (the
// inverted index of a contentless table) and not SQLite's whole allocation: the page cache, prepared
// statements and the library's own state are outside it, and none of it is counted in the Go heap.
// It takes no part in the index's search lock: the only connection is shared through the pool, so a
// running search or build makes it wait there, and ctx bounds that wait. Reading a database that was
// closed meanwhile returns an error and has no other effect.
func (ix *Index) StorageBytes(ctx context.Context) (StorageStats, error) {
	var out StorageStats
	if err := ix.db.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&out.PageCount); err != nil {
		return StorageStats{}, err
	}
	if err := ix.db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&out.PageSize); err != nil {
		return StorageStats{}, err
	}
	out.Bytes, out.Source = out.PageCount*out.PageSize, StorageSource
	return out, nil
}

// Search runs the sandbox query expression (OR of per-word n-gram phrases) ranked by bm25.
// limit <= 0 returns every match. A query with no usable words, or one FTS5 rejects, returns nil
// (the sandbox treats sqlite3.OperationalError as no hits).
func (ix *Index) Search(q string, limit int) []Hit {
	expr := ix.Tok.MatchExpression(q)
	if expr == "" {
		return nil
	}
	if limit <= 0 {
		limit = -1
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	rows, err := ix.db.Query(`SELECT rowid, bm25(docs) FROM docs WHERE docs MATCH ? ORDER BY bm25(docs) LIMIT ?`, expr, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Hit
	for rows.Next() {
		var h Hit
		var rowid int
		if err := rows.Scan(&rowid, &h.Score); err != nil || rowid < 1 || rowid > len(ix.ids) {
			return nil
		}
		h.DocID = ix.ids[rowid-1]
		out = append(out, h)
	}
	if rows.Err() != nil {
		return nil
	}
	return out
}
