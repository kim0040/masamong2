package application

import (
	"database/sql"

	"masamong/internal/memory/store"
)

// personStatementSuppressedTx stands in for the check of the same name that the person-memory core fix
// adds in person_claims_forget.go: true when a forget or hide covers the statement (a forget-all of
// anyone the text names, a hide by claim id, by subject and text with any subset of evidence, a hearsay
// topic, or a v1.6 correction). This stub finds nothing; the file is deleted when the two branches merge.
func (s *Service) personStatementSuppressedTx(tx *sql.Tx, c store.PersonClaim) (bool, error) {
	return false, nil
}
