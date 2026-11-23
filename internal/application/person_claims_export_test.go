package application

import (
	"context"
	"database/sql"
	"encoding/json"

	"masamong/internal/memory/store"
)

// DerivePersonItemForTest derives the person claims of an episode item a test inserted with SQL, as
// insertItems does for the items it saves. It returns how many claims were written.
func (s *Service) DerivePersonItemForTest(ctx context.Context, item string) (int, error) {
	db, err := s.adminDB()
	if err != nil {
		return 0, err
	}
	n := 0
	err = db.WithWriterContext(ctx, func(tx *sql.Tx) error {
		var it deriveItem
		var ev, by string
		if err := tx.QueryRow(`SELECT id,kind,text,evidence,by_refs,created_at FROM episode_item WHERE id=?`, item).Scan(&it.id, &it.kind, &it.text, &ev, &by, &it.saved); err != nil {
			return err
		}
		_ = json.Unmarshal([]byte(ev), &it.evidence)
		_ = json.Unmarshal([]byte(by), &it.by)
		authors, guild, err := evidenceAuthors(tx, it.evidence)
		if err != nil || guild == "" {
			return err
		}
		n, err = s.savePersonClaims(ctx, tx, derivedClaims(guild, it, authors))
		return err
	})
	return n, err
}

// PeoplePassForTest runs room memory's person work: the relation fill, the one-time derivation walk,
// the sweep and the live no_sensitive classification.
func (s *Service) PeoplePassForTest(ctx context.Context) {
	db, err := s.adminDB()
	if err != nil {
		return
	}
	s.peopleClaimsPass(ctx, db)
}

// SavePersonClaimsForTest writes claims through the write gate (savePersonClaims) in one transaction, as
// live extraction or the backfill would, and returns how many rows it wrote.
func (s *Service) SavePersonClaimsForTest(ctx context.Context, claims ...store.PersonClaim) (int, error) {
	db, err := s.adminDB()
	if err != nil {
		return 0, err
	}
	n := 0
	err = db.WithWriterContext(ctx, func(tx *sql.Tx) error {
		var err error
		n, err = s.savePersonClaims(ctx, tx, claims)
		return err
	})
	return n, err
}
