package application

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"masamong/internal/features/privacy"
	"masamong/internal/memory/corpus"
	"masamong/internal/memory/episodes"
	"masamong/internal/memory/store"
)

// Person memory, write side (contract person-memory-v170 §2.1). Every path that writes a person claim —
// derivation from episode items, live extraction, "remember this" about someone else and the paid
// backfill — goes through savePersonClaims, inside the caller's write transaction. The gate re-reads
// everything a claim rests on under that transaction, so a deletion, an edit, a withdrawal, a forget or
// a hide that committed before it is never undone by a write that read the world earlier.

func init() {
	featureDDL = append(featureDDL, store.PersonClaimDDL...)
}

// savePersonClaims writes the claims that pass the gate and returns how many rows it inserted or
// upgraded. A claim that fails a check is skipped, never the transaction: the caller's other writes
// (an extraction's episodes) stand. The checks, per claim:
//
//   - shape: guild, Discord subject and speaker references (never kakao: or label:), a known kind, the
//     subject's {@ref} in a text of at most 120 runes, a topic of at most 20, at least one evidence id;
//   - evidence: every observation is a live human Discord line of this server, current (no pending
//     deletion or edit: store.ObservationCurrent), written by the speaker, with the text the writer read
//     (Lines) and not edited after it read it (ReadAt); all of it in one room;
//   - people: no tombstone on the subject, the speaker or anyone the text names; the subject has not
//     withdrawn profile, and none of them has withdrawn memory;
//   - person_memory_suppress: no forget or hide marker matches at or after the newest evidence time: all
//     (on the subject, the speaker or anyone the text names), the claim, the statement's text (another
//     writer citing other lines), or for hearsay the topic;
//   - no item a member corrected ("틀려": hidden in an episode never made again) rests on the same lines
//     and names the subject, so a v1.6 correction is not undone by a derivation or the backfill;
//   - an episode_item:<id> source is still an active item;
//   - the evidence index is written with indexCopy(guard=true), after a check that no key is blocked.
//
// The id is recomputed from guild, subject, speaker, kind, topic and evidence, so the same statement
// written by two paths is one row. An existing row keeps its state (a hidden claim stays hidden); a
// derived row (episode_item:) saying the same words on the same lines is taken over by a richer source.
func (s *Service) savePersonClaims(ctx context.Context, tx *sql.Tx, claims []store.PersonClaim) (int, error) {
	if len(claims) == 0 {
		return 0, nil
	}
	if !tableExists(tx, "person_claim") || !tableExists(tx, "memory_observations") {
		return 0, nil
	}
	memoryOut, err := withdrawnRefsTx(tx, privacy.Memory)
	if err != nil {
		return 0, err
	}
	profileOut, err := withdrawnRefsTx(tx, privacy.Profile)
	if err != nil {
		return 0, err
	}
	now := s.Clock.Now().UTC().Format(time.RFC3339)
	naive := s.naiveZone()
	saved := 0
	for _, c := range claims {
		if err := ctx.Err(); err != nil {
			return saved, err
		}
		ok, err := s.personClaimPasses(tx, &c, memoryOut, profileOut, naive)
		if err != nil {
			return saved, err
		}
		if !ok {
			continue
		}
		n, err := writePersonClaim(tx, c, now)
		if err != nil {
			return saved, err
		}
		saved += n
	}
	return saved, nil
}

// withdrawnRefsTx are the person references of the accounts that withdrew scope.
func withdrawnRefsTx(tx *sql.Tx, scope string) (map[string]bool, error) {
	accounts, err := withdrawnTx(tx, scope)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(accounts))
	for a := range accounts {
		out[personRef(a)] = true
	}
	return out, nil
}

// personClaimPasses applies the gate to one claim and fills its computed fields (id, normalized topic,
// observed_at, room, state). False means skip it.
func (s *Service) personClaimPasses(tx *sql.Tx, c *store.PersonClaim, memoryOut, profileOut map[string]bool, naive *time.Location) (bool, error) {
	c.Text = strings.Join(strings.Fields(c.Text), " ")
	c.Topic = store.PersonTopic(c.Topic)
	c.Evidence = uniqueStrings(c.Evidence)
	slices.Sort(c.Evidence) // stored in order: the same lines are the same JSON
	switch {
	case c.GuildID == "" || c.GuildID == "0" || !store.DiscordPersonRef(c.SubjectRef) || !store.DiscordPersonRef(c.SpeakerRef),
		!slices.Contains(store.PersonKinds, c.Kind), c.Source == "", len(c.Evidence) == 0,
		c.Text == "" || utf8.RuneCountInString(c.Text) > store.PersonTextRunes,
		!strings.Contains(c.Text, "{@"+c.SubjectRef+"}"):
		return false, nil
	}
	switch c.Link {
	case store.LinkSelf, store.LinkMention, store.LinkReply, store.LinkName:
	default:
		return false, nil
	}
	if c.Link == store.LinkSelf && !c.Direct() {
		return false, nil
	}
	people := uniqueStrings(append([]string{c.SubjectRef, c.SpeakerRef}, episodes.TextRefs(c.Text)...))
	for _, ref := range people {
		if !store.DiscordPersonRef(ref) || memoryOut[ref] {
			return false, nil
		}
		if blocked, err := store.SourceBlocked(tx, ref); err != nil || blocked {
			return false, err
		}
	}
	if profileOut[c.SubjectRef] {
		return false, nil
	}
	var readAt time.Time
	if c.ReadAt != "" {
		t, err := time.Parse(time.RFC3339Nano, c.ReadAt)
		if err != nil {
			return false, nil
		}
		readAt = t
	}
	var newest time.Time
	room := ""
	for _, id := range c.Evidence {
		var obsRoom, platform, account, kind, deleted, content, parsed, edited string
		err := tx.QueryRow(`SELECT room,platform,actor_account,record_kind,deleted_at,content,parsed_time,edited_at FROM memory_observations
			WHERE observation_id=? AND source_room=room`, id).Scan(&obsRoom, &platform, &account, &kind, &deleted, &content, &parsed, &edited)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if platform != "discord" || kind != corpus.HumanText || deleted != "" || strings.TrimSpace(content) == "" ||
			!strings.HasPrefix(obsRoom, "discord:"+c.GuildID+":") || account == "" || personRef(account) != c.SpeakerRef {
			return false, nil
		}
		if room != "" && obsRoom != room {
			return false, nil // one statement is made in one room
		}
		room = obsRoom
		if want, ok := c.Lines[id]; ok && strings.TrimSpace(content) != strings.TrimSpace(want) {
			return false, nil
		}
		if edited != "" && !readAt.IsZero() {
			if t, err := time.Parse(time.RFC3339Nano, edited); err != nil || t.After(readAt) {
				return false, nil
			}
		}
		if current, err := store.ObservationCurrent(tx, id); err != nil || !current {
			return false, err
		}
		t, ok := s.obsTime(parsed, naive)
		if !ok {
			return false, nil
		}
		if t.After(newest) {
			newest = t
		}
	}
	c.RoomID = room
	c.ObservedAt = newest.UTC().Format(store.PersonClaimStamp)
	c.ID = store.PersonClaimID(c.GuildID, c.SubjectRef, c.SpeakerRef, c.Kind, c.Topic, c.Evidence)
	if suppressed, err := personSuppressed(tx, *c); err != nil || suppressed {
		return false, err
	}
	if corrected, err := correctedLines(tx, c.SubjectRef, c.Text, c.Evidence); err != nil || corrected {
		return false, err
	}
	if item, ok := strings.CutPrefix(c.Source, "episode_item:"); ok {
		var active bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM episode_item WHERE id=? AND state='active')`, item).Scan(&active); err != nil || !active {
			return false, err
		}
	}
	switch c.State {
	case "", store.PersonActive:
		c.State = store.PersonActive
		if s.guildPrivacy(c.GuildID).NoSensitive && c.Source != store.SourceExplicit {
			c.State = store.PersonPending // read only after the classifier passed it (an explicit fact was classified when it was asked for)
		}
	case store.PersonPending, store.PersonHidden:
	default:
		return false, nil
	}
	return true, nil
}

// personSuppressed reports whether a forget or hide marker covers the claim, set at or after the claim's
// newest evidence (an empty ObservedAt matches every marker): everything about or by anyone the claim
// names (subject, speaker, the people of its text), this claim, what it says (its text key) about the
// subject, or the subject's hearsay topic.
func personSuppressed(tx *sql.Tx, c store.PersonClaim) (bool, error) {
	topic := ""
	if !c.Direct() && c.Topic != "" {
		topic = "topic:" + store.PersonTopicKey(c.Kind, c.Topic)
	}
	text := ""
	if c.Text != "" {
		text = "text:" + store.PersonTextKey(c.Text)
	}
	named, _ := json.Marshal(uniqueStrings(append([]string{c.SubjectRef, c.SpeakerRef}, episodes.TextRefs(c.Text)...)))
	var hit bool
	err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM person_memory_suppress WHERE guild_id=?1 AND ?6<=before_ts AND (
		(scope='all' AND person_ref IN (SELECT value FROM json_each(?7)))
		OR (person_ref=?2 AND role='subject' AND (scope=?3 OR (?4<>'' AND scope=?4) OR (?5<>'' AND scope=?5)))))`,
		c.GuildID, c.SubjectRef, "claim:"+c.ID, topic, text, c.ObservedAt, string(named)).Scan(&hit)
	return hit, err
}

// correctedLines reports whether a statement is one a member said is wrong: an item hidden in an episode
// that is never made again from its lines (regen=3, explicit.go), resting on any of the lines, naming the
// subject and saying the same (the text key, as a hide marker). v1.6 recorded corrections only this way,
// without person markers; another statement of the same lines (a second fact of one message) is not the
// corrected one and passes.
func correctedLines(tx *sql.Tx, subject, text string, evidence []string) (bool, error) {
	if len(evidence) == 0 || !tableExists(tx, "episode_item") {
		return false, nil
	}
	raw, _ := json.Marshal(evidence)
	rows, err := tx.Query(`SELECT DISTINCT i.text FROM evidence_refs r JOIN episode_item i ON i.id=r.holder_id JOIN channel_episode e ON e.id=i.episode_id
		WHERE r.holder_kind=? AND r.source_id IN (SELECT value FROM json_each(?)) AND i.state='hidden' AND e.regen=3 AND instr(i.text,?)>0`,
		store.HolderEpisodeItem, string(raw), "{@"+subject+"}")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	key := store.PersonTextKey(strings.Join(strings.Fields(text), " "))
	hit := false
	for rows.Next() {
		var itemText string
		if err := rows.Scan(&itemText); err != nil {
			return false, err
		}
		hit = hit || store.PersonTextKey(strings.Join(strings.Fields(itemText), " ")) == key
	}
	return hit, rows.Err()
}

// writePersonClaim inserts a claim that passed the gate, or hands a derived row with the same id to a
// richer source, and indexes its evidence. It returns 1 when a row was written.
//
// A derived row and a richer one saying the same words on the same lines (an explicit fact and the item
// it was saved as) are one statement: the richer one replaces the derived twin (a hidden twin stays
// hidden), and a derived write is skipped while a richer twin exists.
func writePersonClaim(tx *sql.Tx, c store.PersonClaim, now string) (int, error) {
	refs := uniqueStrings(append([]string{c.SubjectRef, c.SpeakerRef}, episodes.TextRefs(c.Text)...))
	keys, err := copySources(tx, c.Evidence, refs)
	if err != nil {
		return 0, err
	}
	if blocked, err := store.SourcesBlocked(tx, keys); err != nil || blocked {
		return 0, err
	}
	derived := strings.HasPrefix(c.Source, "episode_item:")
	ev, _ := json.Marshal(c.Evidence)
	var source, state string
	err = tx.QueryRow(`SELECT source,state FROM person_claim WHERE id=?`, c.ID).Scan(&source, &state)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		twins, err := claimTwins(tx, c, string(ev))
		if err != nil {
			return 0, err
		}
		for _, t := range twins {
			if derived && !t.derived {
				return 0, nil // a richer source already holds the statement
			}
		}
		for _, t := range twins {
			if derived || !t.derived {
				continue
			}
			if t.state == store.PersonHidden {
				c.State = store.PersonHidden
			}
			if err := deletePersonClaimTx(tx, t.id); err != nil {
				return 0, err
			}
		}
		if _, err := tx.Exec(`INSERT INTO person_claim(id,guild_id,subject_ref,speaker_ref,kind,topic,text,evidence,observed_at,room_id,source,link,state,created_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, c.ID, c.GuildID, c.SubjectRef, c.SpeakerRef, c.Kind, c.Topic, c.Text, string(ev), c.ObservedAt, c.RoomID,
			c.Source, c.Link, c.State, now); err != nil {
			return 0, err
		}
	case err != nil:
		return 0, err
	case strings.HasPrefix(source, "episode_item:") && !derived:
		// the same statement, now from a richer source: no longer derived. A row still waiting for the
		// classifier takes the new source's state (an explicit fact was classified when it was asked for);
		// a hidden row stays hidden.
		if state == store.PersonPending {
			state = c.State
		}
		if _, err := tx.Exec(`UPDATE person_claim SET source=?,link=?,state=? WHERE id=?`, c.Source, c.Link, state, c.ID); err != nil {
			return 0, err
		}
	case strings.HasPrefix(source, "episode_item:") && strings.HasPrefix(c.Source, "episode_item:") && source != c.Source:
		// an item made again (a regenerated episode): the statement now rests on the new item once the
		// old one is gone; the row keeps its state
		var active bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM episode_item WHERE id=? AND state='active')`, strings.TrimPrefix(source, "episode_item:")).Scan(&active); err != nil || active {
			return 0, err
		}
		if _, err := tx.Exec(`UPDATE person_claim SET text=?,source=? WHERE id=?`, c.Text, c.Source, c.ID); err != nil {
			return 0, err
		}
	default:
		return 0, nil // already written: idempotent
	}
	if err := indexCopy(tx, store.HolderPersonClaim, c.ID, keys, true); err != nil {
		return 0, err
	}
	return 1, nil
}

type claimTwin struct {
	id, state string
	derived   bool
}

// claimTwins are the other rows of the same speaker about the same subject with the same words on the
// same lines.
func claimTwins(tx *sql.Tx, c store.PersonClaim, evidence string) ([]claimTwin, error) {
	rows, err := tx.Query(`SELECT id,state,source FROM person_claim WHERE guild_id=? AND subject_ref=? AND speaker_ref=? AND text=? AND evidence=? AND id<>?`,
		c.GuildID, c.SubjectRef, c.SpeakerRef, c.Text, evidence, c.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []claimTwin
	for rows.Next() {
		var t claimTwin
		var source string
		if err := rows.Scan(&t.id, &t.state, &source); err != nil {
			return nil, err
		}
		t.derived = strings.HasPrefix(source, "episode_item:")
		out = append(out, t)
	}
	return out, rows.Err()
}

// deletePersonClaimTx removes one claim and its evidence index.
func deletePersonClaimTx(tx *sql.Tx, id string) error {
	if _, err := tx.Exec(`DELETE FROM evidence_refs WHERE holder_kind=? AND holder_id=?`, store.HolderPersonClaim, id); err != nil {
		return err
	}
	_, err := tx.Exec(`DELETE FROM person_claim WHERE id=?`, id)
	return err
}

// deleteClaimsCiting deletes the claims whose evidence index holds any of keys (observation ids, native
// message ids or person references) and returns how many went.
func deleteClaimsCiting(tx *sql.Tx, keys []string) (int, error) {
	if len(keys) == 0 {
		return 0, nil
	}
	raw, _ := json.Marshal(uniqueStrings(keys))
	return deleteClaimsWhere(tx, `SELECT DISTINCT holder_id FROM evidence_refs WHERE holder_kind=? AND source_id IN (SELECT value FROM json_each(?))`,
		store.HolderPersonClaim, string(raw))
}

// deleteClaimsOf deletes a person's claims in one server (every server when guild is ""): the ones
// about them, and with speaker also the ones they said about others and the ones naming them in the text.
func deleteClaimsOf(tx *sql.Tx, guild, ref string, speaker bool) (int, error) {
	if ref == "" || !tableExists(tx, "person_claim") {
		return 0, nil
	}
	q := `SELECT id FROM person_claim WHERE (subject_ref=?1 OR (?2 AND (speaker_ref=?1 OR instr(text,'{@'||?1||'}')>0)))`
	args := []any{ref, speaker}
	if guild != "" {
		q += ` AND guild_id=?3`
		args = append(args, guild)
	}
	return deleteClaimsWhere(tx, q, args...)
}

// deleteClaimsWhere deletes the claims whose ids the query returns.
func deleteClaimsWhere(tx *sql.Tx, query string, args ...any) (int, error) {
	rows, err := tx.Query(query, args...)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if err := deletePersonClaimTx(tx, id); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}
