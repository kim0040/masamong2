package application

import (
	"database/sql"
	"testing"

	"masamong/internal/features/privacy"
	"masamong/internal/memory/store"
)

// The stored pilot answers of the paid backfill are a copy of what people said: a member's forget-all and a
// withdrawal of memory consent purge that copy in the same transaction, through the real paths.
func TestPeopleBackfillPilotPurgeIsWired(t *testing.T) {
	h := newPBHost(t)
	s := pbService(t, h, "anthropic")
	pbSeed(t, s, 70)
	_, run := pbPiloted(t, s, nil)
	mina, joon := store.PersonRef("101"), store.PersonRef("102")
	has := func(ref string) bool {
		for _, f := range pbStoredFacts(t, s, run.ID) {
			if pbNames(f, ref) {
				return true
			}
		}
		return false
	}
	if !has(joon) || !has(mina) {
		t.Fatal("the stored pilot answers have nothing of Mina and Joon to start with")
	}
	if err := s.Store.WithWriter(func(tx *sql.Tx) error {
		_, err := s.forgetPersonTx(tx, "g1", joon, "self")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if has(joon) {
		t.Fatal("a forget-all left the person's statements in the stored pilot answers")
	}
	if _, err := s.setConsent("101", privacy.Memory, privacy.Withdrawn, "test"); err != nil {
		t.Fatal(err)
	}
	if has(mina) {
		t.Fatal("a memory withdrawal left the person's statements in the stored pilot answers")
	}
}
