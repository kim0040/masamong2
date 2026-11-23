package application

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"masamong/internal/action"
	"masamong/internal/errcode"
	"masamong/internal/memory/episodes"
	"masamong/internal/memory/store"
	"masamong/internal/storage"
)

// Person memory, forgetting and hiding (contract person-memory-v170 §2.6). Every one of them leaves a
// marker in person_memory_suppress (no text, only references and a time) that the write gate checks, so
// no later derivation, extraction or backfill brings back what a member forgot or hid.

// suppressStamp is the marker time of now.
func (s *Service) suppressStamp() string { return s.Clock.Now().UTC().Format(store.PersonClaimStamp) }

// writeSuppress records one marker; a marker already there keeps the later time.
func writeSuppress(tx *sql.Tx, m store.PersonSuppress) error {
	if !tableExists(tx, "person_memory_suppress") {
		return nil
	}
	_, err := tx.Exec(`INSERT INTO person_memory_suppress(guild_id,person_ref,role,scope,before_ts,by,created_at) VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(guild_id,person_ref,role,scope) DO UPDATE SET before_ts=MAX(before_ts,excluded.before_ts), by=excluded.by, created_at=excluded.created_at`,
		m.GuildID, m.PersonRef, m.Role, m.Scope, m.BeforeTS, m.By, m.CreatedAt)
	return err
}

// claimMarkers are the markers of one hidden or forgotten statement: the claim itself, what it says (its
// text key, so the same words from other lines or another writer stay hidden), and for what someone else
// said with a topic, that topic about the subject (shown again only once the subject says it themselves).
func claimMarkers(guild, subject, speaker, kind, topic, text, id, by, at, now string) []store.PersonSuppress {
	out := []store.PersonSuppress{{GuildID: guild, PersonRef: subject, Role: store.SuppressSubject, Scope: "claim:" + id, BeforeTS: at, By: by, CreatedAt: now}}
	if text != "" {
		out = append(out, store.PersonSuppress{GuildID: guild, PersonRef: subject, Role: store.SuppressSubject,
			Scope: "text:" + store.PersonTextKey(text), BeforeTS: at, By: by, CreatedAt: now})
	}
	if subject != speaker && store.PersonTopic(topic) != "" {
		out = append(out, store.PersonSuppress{GuildID: guild, PersonRef: subject, Role: store.SuppressSubject,
			Scope: "topic:" + store.PersonTopicKey(kind, topic), BeforeTS: at, By: by, CreatedAt: now})
	}
	return out
}

// hideClaimTx hides one claim and writes its markers. False when there is no such claim.
func (s *Service) hideClaimTx(tx *sql.Tx, id, by string) (bool, error) {
	var guild, subject, speaker, kind, topic, text string
	err := tx.QueryRow(`SELECT guild_id,subject_ref,speaker_ref,kind,topic,text FROM person_claim WHERE id=?`, id).Scan(&guild, &subject, &speaker, &kind, &topic, &text)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(`UPDATE person_claim SET state=? WHERE id=?`, store.PersonHidden, id); err != nil {
		return false, err
	}
	now := s.Clock.Now().UTC().Format(time.RFC3339)
	for _, m := range claimMarkers(guild, subject, speaker, kind, topic, text, id, by, s.suppressStamp(), now) {
		if err := writeSuppress(tx, m); err != nil {
			return false, err
		}
	}
	// the paid backfill's stored pilot answers are another copy: the new markers purge them too
	if err := s.pbRecheckPilotTx(tx, guild); err != nil {
		return false, err
	}
	return true, nil
}

// itemClaimsTx handles the statements about people that episode items give, when a member said the
// items are wrong (hide) or forgot them: each gets its markers, and its row is hidden or deleted. The
// statements are the item's own: the ones it derives, the claims resting on its lines that say the same
// about someone it names (the same text key, from another writer), and for a remembered fact (an item of
// an explicit episode, one per message) the claims it was saved as (a told claim names its person only
// by label). Another statement of the same lines (a second fact of one message) is not the item's and
// stays. It returns how many rows changed.
func (s *Service) itemClaimsTx(tx *sql.Tx, items []string, hide bool, by string) (int, error) {
	if len(items) == 0 || !tableExists(tx, "person_claim") {
		return 0, nil
	}
	ids := map[string]bool{}
	marks := []store.PersonSuppress{}
	guilds := map[string]bool{}
	stamp, now := s.suppressStamp(), s.Clock.Now().UTC().Format(time.RFC3339)
	for _, item := range uniqueStrings(items) {
		var it deriveItem
		var ev, by2, guild, episodeSource string
		err := tx.QueryRow(`SELECT i.id,i.kind,i.text,i.evidence,i.by_refs,i.created_at,e.guild_id,e.source FROM episode_item i JOIN channel_episode e ON e.id=i.episode_id WHERE i.id=?`, item).
			Scan(&it.id, &it.kind, &it.text, &ev, &by2, &it.saved, &guild, &episodeSource)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return 0, err
		}
		guilds[guild] = true
		_ = json.Unmarshal([]byte(ev), &it.evidence)
		_ = json.Unmarshal([]byte(by2), &it.by)
		authors, _, err := evidenceAuthors(tx, it.evidence)
		if err != nil {
			return 0, err
		}
		for _, c := range derivedClaims(guild, it, authors) {
			id := store.PersonClaimID(c.GuildID, c.SubjectRef, c.SpeakerRef, c.Kind, c.Topic, uniqueStrings(c.Evidence))
			ids[id] = true
			marks = append(marks, claimMarkers(c.GuildID, c.SubjectRef, c.SpeakerRef, c.Kind, c.Topic, c.Text, id, by, stamp, now)...)
		}
		lines, _ := json.Marshal(uniqueStrings(it.evidence))
		named, _ := json.Marshal(uniqueStrings(episodes.TextRefs(it.text)))
		explicitItem := episodeSource == explicitSource
		itemKey := store.PersonTextKey(strings.Join(strings.Fields(it.text), " "))
		rows, err := tx.Query(`SELECT id,guild_id,subject_ref,speaker_ref,kind,topic,text,source FROM person_claim WHERE source=?1
			UNION SELECT c.id,c.guild_id,c.subject_ref,c.speaker_ref,c.kind,c.topic,c.text,c.source FROM evidence_refs r JOIN person_claim c ON c.id=r.holder_id
			WHERE r.holder_kind=?2 AND r.source_id IN (SELECT value FROM json_each(?3)) AND c.guild_id=?4
			AND ((?7 AND c.source=?5) OR c.subject_ref IN (SELECT value FROM json_each(?6)))`,
			"episode_item:"+item, store.HolderPersonClaim, string(lines), guild, store.SourceExplicit, string(named), explicitItem)
		if err != nil {
			return 0, err
		}
		for rows.Next() {
			var id, g, subject, speaker, kind, topic, text, source string
			if err := rows.Scan(&id, &g, &subject, &speaker, &kind, &topic, &text, &source); err != nil {
				rows.Close()
				return 0, err
			}
			own := source == "episode_item:"+item || (explicitItem && source == store.SourceExplicit) || store.PersonTextKey(text) == itemKey
			if !own {
				continue // another statement of the same lines
			}
			ids[id] = true
			marks = append(marks, claimMarkers(g, subject, speaker, kind, topic, text, id, by, stamp, now)...)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return 0, err
		}
	}
	for _, m := range marks {
		if err := writeSuppress(tx, m); err != nil {
			return 0, err
		}
	}
	// the paid backfill's stored pilot answers are another copy: the new markers purge them too
	for g := range guilds {
		if err := s.pbRecheckPilotTx(tx, g); err != nil {
			return 0, err
		}
	}
	changed := 0
	for id := range ids {
		var res sql.Result
		var err error
		if hide {
			res, err = tx.Exec(`UPDATE person_claim SET state=? WHERE id=? AND state<>?`, store.PersonHidden, id, store.PersonHidden)
		} else {
			if _, err = tx.Exec(`DELETE FROM evidence_refs WHERE holder_kind=? AND holder_id=?`, store.HolderPersonClaim, id); err == nil {
				res, err = tx.Exec(`DELETE FROM person_claim WHERE id=?`, id)
			}
		}
		if err != nil {
			return changed, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			changed++
		}
	}
	return changed, nil
}

// forgetPersonTx is /잊어줘 (all) for person memory: markers on the person as subject and as speaker up
// to now, then every claim about them, said by them or naming them in its text in the server is deleted.
// The markers are written even when nothing is stored yet: they are the cutoff a later extraction or
// backfill checks.
func (s *Service) forgetPersonTx(tx *sql.Tx, guild, ref, by string) (int, error) {
	if !tableExists(tx, "person_claim") {
		return 0, nil
	}
	stamp, now := s.suppressStamp(), s.Clock.Now().UTC().Format(time.RFC3339)
	for _, role := range []string{store.SuppressSubject, store.SuppressSpeaker} {
		if err := writeSuppress(tx, store.PersonSuppress{GuildID: guild, PersonRef: ref, Role: role, Scope: store.SuppressAll, BeforeTS: stamp, By: by, CreatedAt: now}); err != nil {
			return 0, err
		}
	}
	n, err := deleteClaimsOf(tx, guild, ref, true)
	if err != nil {
		return 0, err
	}
	// the paid backfill's stored pilot answers are another copy: the new markers purge them too
	return n, s.pbRecheckPilotTx(tx, guild)
}

// personStatementSuppressedTx reports whether a forget or hide marker, or a member's correction, covers a
// statement that is not stored as a claim (a pilot answer of the paid backfill): the checks the write gate
// makes for markers. c needs GuildID, SubjectRef, SpeakerRef, Kind, Topic, Text and Evidence. ObservedAt
// may be left empty: it is then the newest evidence time, and when an evidence line is gone or has no
// time every marker matches (fail closed).
func (s *Service) personStatementSuppressedTx(tx *sql.Tx, c store.PersonClaim) (bool, error) {
	if !tableExists(tx, "person_memory_suppress") {
		return false, nil
	}
	c.Evidence = uniqueStrings(c.Evidence)
	c.Topic = store.PersonTopic(c.Topic)
	c.Text = strings.Join(strings.Fields(c.Text), " ")
	c.ID = store.PersonClaimID(c.GuildID, c.SubjectRef, c.SpeakerRef, c.Kind, c.Topic, c.Evidence)
	if c.ObservedAt == "" {
		naive := s.naiveZone()
		var newest time.Time
		for _, id := range c.Evidence {
			var parsed string
			if err := tx.QueryRow(`SELECT parsed_time FROM memory_observations WHERE observation_id=?`, id).Scan(&parsed); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return false, err
			}
			t, ok := s.obsTime(parsed, naive)
			if !ok {
				newest = time.Time{}
				break
			}
			if t.After(newest) {
				newest = t
			}
		}
		if !newest.IsZero() {
			c.ObservedAt = newest.UTC().Format(store.PersonClaimStamp)
		}
	}
	if hit, err := personSuppressed(tx, c); err != nil || hit {
		return hit, err
	}
	return correctedLines(tx, c.SubjectRef, c.Text, c.Evidence)
}

// deleteEpisodeClaimsTx deletes the claims resting on an episode an operator deletes: the ones its items
// derive, for a remembered fact ("remember this", an explicit episode) the explicit claims on its message,
// which follow no item, and the claims the same extraction saved live (extract:) on the lines the episode
// was made from (its segment).
func deleteEpisodeClaimsTx(tx *sql.Tx, episode string) error {
	if !tableExists(tx, "person_claim") {
		return nil
	}
	if _, err := deleteClaimsWhere(tx, `SELECT c.id FROM episode_item i JOIN person_claim c ON c.source='episode_item:'||i.id WHERE i.episode_id=?`, episode); err != nil {
		return err
	}
	if _, err := deleteClaimsWhere(tx, `SELECT c.id FROM person_claim c JOIN channel_episode e ON e.id=? WHERE c.source>=? AND c.source<? AND c.guild_id=e.guild_id
		AND EXISTS(SELECT 1 FROM json_each(c.evidence) a JOIN json_each(e.segment) b ON a.value=b.value)`, episode, livePeoplePrefix, liveSourceEnd); err != nil {
		return err
	}
	_, err := deleteClaimsWhere(tx, `SELECT c.id FROM person_claim c JOIN channel_episode e ON e.id=? WHERE e.source=? AND c.source=? AND c.guild_id=e.guild_id
		AND EXISTS(SELECT 1 FROM json_each(c.evidence) a JOIN json_each(e.segment) b ON a.value=b.value)`, episode, explicitSource, store.SourceExplicit)
	return err
}

// liveSourceEnd is the first source after every extract:<id> (':' + 1 is ';'), for a range scan.
const liveSourceEnd = "extract;"

// deleteRoomClaimsTx deletes the claims of a room's memory an operator deletes: the ones derived from its
// items, the ones live extraction saved there and the explicit ones said there.
func deleteRoomClaimsTx(tx *sql.Tx, room string) error {
	if !tableExists(tx, "person_claim") {
		return nil
	}
	_, err := deleteClaimsWhere(tx, `SELECT id FROM person_claim WHERE room_id=? AND (source=? OR (source>='episode_item:' AND source<'episode_item;')
		OR (source>=? AND source<?))`, room, store.SourceExplicit, livePeoplePrefix, liveSourceEnd)
	return err
}

// personAction is memory.person.hide {guild_id, user_id, claim_id}: one claim about the person is hidden
// and never written again; for something someone else said with a topic, that topic about the person is
// not shown again until the person says it themselves. A member hides only what is about them; an
// operator (the console) names the person.
func (s *Service) personAction(ctx context.Context, req action.Request) action.Result {
	db, err := s.adminDB()
	if err != nil || !db.IsSQLite() || s.Store == nil || !tableReady(db, "person_claim") {
		return action.Fail(req, errcode.NotSetup, "generic.error", "person memory needs the SQLite memory store", nil)
	}
	in := inputMap(req)
	guild, user, id := strVal(in, "guild_id", ""), strVal(in, "user_id", ""), strVal(in, "claim_id", "")
	by := store.SuppressByOp
	if inv := req.Invoker; inv != nil && inv.UserID != "" && (inv.Via == "command" || inv.Via == "chat" || !s.IsOperator(inv.UserID)) {
		guild, user, by = inv.GuildID, inv.UserID, store.SuppressBySelf
	}
	if guild == "" || user == "" || id == "" {
		return action.Fail(req, errcode.InvalidInput, "memory.mine.server_only", s.msg(req, "memory.mine.server_only"), nil)
	}
	ref := personRef(user)
	hidden := false
	err = db.WithWriterContext(ctx, func(tx *sql.Tx) error {
		var ok bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM person_claim WHERE id=? AND guild_id=? AND subject_ref=?)`, id, guild, ref).Scan(&ok); err != nil || !ok {
			return err
		}
		var err error
		hidden, err = s.hideClaimTx(tx, id, by)
		return err
	})
	if err != nil {
		return s.failErr(req, errcode.IO, "error.storage", err, nil)
	}
	if !hidden {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "no such person memory item", nil)
	}
	s.emit("memory", "people", nil)
	return action.OK(req, map[string]any{"hidden": true, "claim_id": id})
}

// personClaimsView is the person field of memory.mine.show: the statements about the person the gate
// lets through (claimsAbout: a member sees the channel's scope, the console every active and pending
// one), with who said each one when it was someone else.
func (s *Service) personClaimsView(ctx context.Context, db *storage.Store, guild, ref, channel string, operator bool, labelIn func(room string) func(string) string) ([]map[string]any, []claimRow) {
	claims, err := s.claimsAbout(ctx, db, guild, ref, channel, operator)
	if err != nil {
		return []map[string]any{}, nil
	}
	out := make([]map[string]any, 0, len(claims))
	for _, c := range claims {
		label := labelIn(c.room)
		saidBy := []string{}
		if !c.direct() {
			if name := label(c.speaker); name != "" {
				saidBy = append(saidBy, clip(name, 24))
			}
		}
		out = append(out, map[string]any{"id": c.id, "kind": c.kind, "text": episodes.StoredText(c.text, label, "?"), "origin": c.origin(),
			"said_by": saidBy, "observed_at": c.observed, "evidence_n": len(c.evidence), "state": c.state})
	}
	return out, claims
}

// personKindFor is the claim kind of an explicit fact's topic key: a single-valued status topic, or other.
func personKindFor(key string) string {
	if singleTopics[store.PersonTopic(key)] {
		return "status"
	}
	return "other"
}

// saveExplicitClaim writes what a member asked to have remembered as a person claim (source explicit),
// through the write gate: about themselves it replaces the statement the item gave (its derived twin, the
// same words on the same line; writePersonClaim); about one other person the turn named (about), it is a
// told claim about that person.
func (s *Service) saveExplicitClaim(tx *sql.Tx, guild, ref string, self bool, about turnPerson, key, text, obs string) error {
	if !s.Doc().Memory.PeopleOn() || !tableExists(tx, "person_claim") {
		return nil
	}
	c := store.PersonClaim{GuildID: guild, SpeakerRef: ref, Kind: personKindFor(key), Topic: key, Evidence: []string{obs}, Source: store.SourceExplicit}
	switch {
	case self:
		c.SubjectRef, c.Link, c.Text = ref, store.LinkSelf, text
		if !strings.Contains(text, "{@"+ref+"}") {
			c.Text = "{@" + ref + "} " + text
		}
	case about.ref != "":
		token := "{@" + about.ref + "}"
		c.SubjectRef, c.Link, c.Text = about.ref, about.link, text
		if !strings.Contains(text, token) {
			if l := strings.TrimSpace(about.label); utf8.RuneCountInString(l) >= 2 && strings.Contains(text, l) {
				c.Text = strings.Replace(text, l, token, 1)
			} else {
				c.Text = token + " " + text
			}
		}
	default:
		return nil
	}
	_, err := s.savePersonClaims(context.Background(), tx, []store.PersonClaim{c})
	return err
}

// deleteExplicitClaims deletes the explicit claims resting on the evidence of an explicit episode that a
// newer fact on the same topic replaces.
func deleteExplicitClaims(tx *sql.Tx, episode string) error {
	if !tableExists(tx, "person_claim") {
		return nil
	}
	_, err := deleteClaimsWhere(tx, `SELECT c.id FROM person_claim c JOIN channel_episode e ON e.id=? WHERE c.source=? AND c.guild_id=e.guild_id
		AND EXISTS(SELECT 1 FROM json_each(c.evidence) a JOIN json_each(e.segment) b ON a.value=b.value)`, episode, store.SourceExplicit)
	return err
}
