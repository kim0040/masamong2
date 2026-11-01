package application

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strconv"

	"masamong/internal/config"
	"masamong/internal/features/privacy"
	"masamong/internal/lang"
	"masamong/internal/storage"
)

// Strict privacy (docs/plan-memory-conversation.md §5.4.8, P4-7): guilds.<g>.privacy options a server can
// turn on, all off by default, and the personal "don't analyze me" scope (privacy.Profile).
//
//   - own_card_only: turnCards shows only the caller's card;
//   - no_hearsay: relatedEpisodes leaves out items about a person that only others said, and episode
//     titles and live topics that name someone who said none of what is shown;
//   - no_sensitive: "remember this" refuses a fact the explicit classifier marks sensitive, and card
//     promotion asks a classifier about the facts it would add (dropSensitive, derived budget, kind card).
//     Episode items and topics are not classified;
//   - card_channel_only: a card item is used only where all of its evidence was said (cardIn);
//   - Profile withdrawn: no card is made or used, the cards are deleted (also by a pass that read the
//     consents before: saveCard reads it again), room style versions quoting the person go stale, and the
//     person's lines stay out of the room style analysis. Explicit "remember this" facts are conversation
//     memory and stay (memory_explicit holds no text); agreeing again makes their cards again.

const spendCard = "card"

func (s *Service) guildPrivacy(guildID string) config.GuildPrivacy {
	if guildID == "" {
		return config.GuildPrivacy{}
	}
	return s.Doc().Guilds[guildID].Privacy
}

// profileWithdrawn are the person references of everyone who withdrew the profile scope.
func (s *Service) profileWithdrawn(db *storage.Store) map[string]bool {
	out := map[string]bool{}
	rows, err := db.Reader().Query(`SELECT user_id FROM consent_state WHERE scope=? AND status=?`, privacy.Profile, privacy.Withdrawn)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			out[personRef(id)] = true
		}
	}
	return out
}

// profileWithdrawnTx reads under a writer transaction whether the person behind ref withdrew the
// profile scope (consent rows hold account IDs, so the few withdrawn ones are compared by reference).
func profileWithdrawnTx(tx *sql.Tx, ref string) (bool, error) {
	if ref == "" || !tableExists(tx, "consent_state") {
		return false, nil
	}
	ids, err := withdrawnTx(tx, privacy.Profile)
	if err != nil {
		return false, err
	}
	for id := range ids {
		if personRef(id) == ref {
			return true, nil
		}
	}
	return false, nil
}

// withdrawnTx are the accounts that withdrew any of scopes, read under a writer transaction.
func withdrawnTx(tx *sql.Tx, scopes ...string) (map[string]bool, error) {
	out := map[string]bool{}
	if !tableExists(tx, "consent_state") {
		return out, nil
	}
	raw, _ := json.Marshal(scopes)
	rows, err := tx.Query(`SELECT user_id FROM consent_state WHERE status=? AND scope IN (SELECT value FROM json_each(?))`, privacy.Withdrawn, string(raw))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// dropProfile deletes a person's cards everywhere and makes stale every room style version that quotes
// their lines ("don't analyze me" withdrawn).
func (s *Service) dropProfile(ctx context.Context, userID string) error {
	db, err := s.adminDB()
	if err != nil || !db.IsSQLite() {
		return err
	}
	captured := false
	if err := db.WithWriterContext(ctx, func(tx *sql.Tx) error {
		captured = tableExists(tx, "memory_observations")
		_, err := deleteCardsOf(tx, "", personRef(userID))
		return err
	}); err != nil || !captured {
		return err
	}
	return s.staleStyles(ctx, db, `SELECT DISTINCT r.holder_id FROM evidence_refs r JOIN memory_observations o ON o.observation_id=r.source_id
		WHERE r.holder_kind=? AND o.actor_account=?`, holderRoomStyle, userID)
}

// sensitiveKey is the cache key of one classified fact (the text itself is not stored in the key).
func sensitiveKey(text string) string {
	sum := sha256.Sum256([]byte(text))
	return "cards:sensitive:" + hex.EncodeToString(sum[:12])
}

// dropSensitive leaves out the card items a classifier marks sensitive. Verdicts are cached by text; a
// fact that cannot be classified now (no budget, a model error) waits for a later pass instead of going in.
func (s *Service) dropSensitive(ctx context.Context, want []cardWant) []cardWant {
	verdict := map[string]bool{}
	var ask []string
	for _, w := range want {
		var v bool
		if ok, _ := s.kvGet(sensitiveKey(w.text), &v); ok {
			verdict[w.text] = v
		} else if !slices.Contains(ask, w.text) {
			ask = append(ask, w.text)
		}
	}
	if len(ask) > 0 && s.derivedAllows(spendCard) {
		pack := lang.For(s.Doc().Memory.Languages...)
		lines := ""
		for i, t := range ask {
			lines += "[f" + strconv.Itoa(i+1) + "] " + storedRefPattern.ReplaceAllString(t, "@") + "\n"
		}
		text, _, err := s.featureText(ctx, "summary", spendCard, pack.Memory.SensitiveSystem, lang.Fill(pack.Memory.Sensitive, "facts", lines), 200, 0)
		if err == nil {
			var reply struct {
				Sensitive []string `json:"sensitive"`
			}
			if decodeJSONObject(text, &reply) {
				for i, t := range ask {
					v := slices.Contains(reply.Sensitive, "f"+strconv.Itoa(i+1))
					verdict[t] = v
					_ = s.kvSet(sensitiveKey(t), v)
				}
			}
		}
	}
	out := want[:0]
	for _, w := range want {
		if v, ok := verdict[w.text]; ok && !v {
			out = append(out, w)
		}
	}
	return out
}
