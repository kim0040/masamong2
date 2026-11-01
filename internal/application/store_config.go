package application

import (
	"database/sql"
	"errors"
	"os"
	"time"

	"masamong/internal/config"
	"masamong/internal/fsutil"
	"masamong/internal/storage"
)

func loadCASState(store *storage.Store, cfgPath string) (config.State, error) {
	st := config.State{}
	row := store.DB.QueryRow(`SELECT revision, status, content_hash, content, base_revision FROM config_revisions WHERE status='committed' ORDER BY revision DESC LIMIT 1`)
	var cur config.Revision
	var content string
	err := row.Scan(&cur.Revision, &cur.Status, &cur.ContentHash, &content, &cur.BaseRevision)
	if err == nil {
		cur.Content = []byte(content)
		st.Current = &cur
	} else if !errors.Is(err, sql.ErrNoRows) {
		return st, err
	}
	prow := store.DB.QueryRow(`SELECT revision, status, content_hash, content, base_revision FROM config_revisions WHERE status='prepared' ORDER BY revision DESC LIMIT 1`)
	var prep config.Revision
	var pcontent string
	err = prow.Scan(&prep.Revision, &prep.Status, &prep.ContentHash, &pcontent, &prep.BaseRevision)
	if err == nil {
		prep.Content = []byte(pcontent)
		st.Prepared = &prep
	} else if !errors.Is(err, sql.ErrNoRows) {
		return st, err
	}
	if b, err := os.ReadFile(cfgPath); err == nil {
		st.FileBytes = b
		st.FileHash = config.Hash(b)
	}
	return st, nil
}

func persistRevision(store *storage.Store, r config.Revision) error {
	return store.WithWriter(func(tx *sql.Tx) error {
		return store.UpsertRevision(tx, r.Revision, string(r.Status), r.ContentHash, string(r.Content), r.BaseRevision, time.Now().UTC().Format(time.RFC3339))
	})
}

func atomicWriteFile(path string, body []byte) error { return fsutil.WriteAtomic(path, body, 0o600) }
