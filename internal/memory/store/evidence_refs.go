package store

import (
	"database/sql"
	"errors"

	"masamong/internal/storage"
)

// Evidence links contain two integer IDs instead of repeating source/holder strings in every row and
// both indexes. The compatibility view keeps privacy lookups and old callers on the same contract.
func migrateEvidenceRefs(db *storage.Store) error {
	return db.WithWriter(func(tx *sql.Tx) error {
		var kind string
		err := tx.QueryRow(`SELECT type FROM sqlite_master WHERE name='evidence_refs'`).Scan(&kind)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if kind == "view" {
			return nil
		}
		for _, q := range []string{
			`CREATE TABLE IF NOT EXISTS evidence_sources(id INTEGER PRIMARY KEY, source_id TEXT NOT NULL UNIQUE)`,
			`CREATE TABLE IF NOT EXISTS evidence_holders(id INTEGER PRIMARY KEY, holder_kind TEXT NOT NULL, holder_id TEXT NOT NULL, UNIQUE(holder_kind,holder_id))`,
			`CREATE INDEX IF NOT EXISTS idx_evidence_holder_id ON evidence_holders(holder_id)`,
			`CREATE TABLE IF NOT EXISTS evidence_links(source INTEGER NOT NULL, holder INTEGER NOT NULL, PRIMARY KEY(source,holder)) WITHOUT ROWID`,
			`CREATE INDEX IF NOT EXISTS idx_evidence_links_holder ON evidence_links(holder,source)`,
		} {
			if _, err := tx.Exec(q); err != nil {
				return err
			}
		}
		if kind == "table" {
			for _, q := range []string{
				`INSERT INTO evidence_sources(source_id) SELECT DISTINCT source_id FROM evidence_refs`,
				`INSERT INTO evidence_holders(holder_kind,holder_id) SELECT DISTINCT holder_kind,holder_id FROM evidence_refs`,
				`INSERT INTO evidence_links(source,holder) SELECT s.id,h.id FROM evidence_refs r JOIN evidence_sources s ON s.source_id=r.source_id JOIN evidence_holders h ON h.holder_kind=r.holder_kind AND h.holder_id=r.holder_id`,
				`DROP TABLE evidence_refs`,
			} {
				if _, err := tx.Exec(q); err != nil {
					return err
				}
			}
		}
		for _, q := range []string{
			`CREATE VIEW evidence_refs AS SELECT s.source_id,h.holder_kind,h.holder_id FROM evidence_links r JOIN evidence_sources s ON s.id=r.source JOIN evidence_holders h ON h.id=r.holder`,
			`CREATE TRIGGER evidence_refs_insert INSTEAD OF INSERT ON evidence_refs BEGIN
			 INSERT OR IGNORE INTO evidence_sources(source_id) VALUES(NEW.source_id);
			 INSERT OR IGNORE INTO evidence_holders(holder_kind,holder_id) VALUES(NEW.holder_kind,NEW.holder_id);
			 INSERT OR IGNORE INTO evidence_links(source,holder) SELECT s.id,h.id FROM evidence_sources s,evidence_holders h WHERE s.source_id=NEW.source_id AND h.holder_kind=NEW.holder_kind AND h.holder_id=NEW.holder_id;
			 END`,
			`CREATE TRIGGER evidence_refs_delete INSTEAD OF DELETE ON evidence_refs BEGIN
			 DELETE FROM evidence_links WHERE source=(SELECT id FROM evidence_sources WHERE source_id=OLD.source_id) AND holder=(SELECT id FROM evidence_holders WHERE holder_kind=OLD.holder_kind AND holder_id=OLD.holder_id);
			 END`,
			`CREATE TRIGGER evidence_links_cleanup AFTER DELETE ON evidence_links BEGIN
			 DELETE FROM evidence_sources WHERE id=OLD.source AND NOT EXISTS(SELECT 1 FROM evidence_links WHERE source=OLD.source);
			 DELETE FROM evidence_holders WHERE id=OLD.holder AND NOT EXISTS(SELECT 1 FROM evidence_links WHERE holder=OLD.holder);
			 END`,
		} {
			if _, err := tx.Exec(q); err != nil {
				return err
			}
		}
		return nil
	})
}
