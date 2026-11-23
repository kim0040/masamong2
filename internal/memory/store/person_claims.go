package store

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Person memory (v1.7.0, contract person-memory-v170 §1): one person_claim row is one statement by one
// speaker about one subject of one server, with the observations it rests on. Nothing is merged: a
// person's card, the trust of a statement and which statement supersedes which are computed when it is
// read, never stored. person_memory_suppress holds no text, only references and times: what a member
// forgot or hid, which every write path checks in the same transaction, so a later pass, a live
// extraction or a backfill cannot bring it back.
//
// SQLite only, like the rest of room memory. Claims are never embedded and never feed the raw-text index.

// HolderPersonClaim is the evidence_refs holder kind of a person claim; invalidation is per row.
const HolderPersonClaim = "person_claim"

// PersonClaimStamp is the fixed-width UTC time of observed_at (the same as activityStamp): ordering is a
// string comparison.
const PersonClaimStamp = "2006-01-02T15:04:05.000000000Z"

// Limits of one claim.
const (
	PersonTopicRunes = 20
	PersonTextRunes  = 120
)

// Claim kinds, states, links and suppress roles and scopes.
const (
	PersonActive  = "active"
	PersonHidden  = "hidden"
	PersonPending = "pending" // waiting for the no_sensitive classifier; never read

	LinkSelf    = "self"
	LinkMention = "mention"
	LinkReply   = "reply"
	LinkName    = "name"

	SuppressSubject = "subject"
	SuppressSpeaker = "speaker"
	SuppressAll     = "all"
	SuppressBySelf  = "self"
	SuppressByOp    = "operator"

	SourceExplicit = "explicit"
)

// PersonKinds are the kinds a claim may have.
var PersonKinds = []string{"nickname", "like", "dislike", "trait", "status", "appearance", "relation", "other"}

// PersonClaim is one statement by one speaker about one subject. Subject and speaker are Discord person
// references (PersonRef), never kakao: or label: references.
type PersonClaim struct {
	ID         string
	GuildID    string
	SubjectRef string
	SpeakerRef string
	Kind       string
	Topic      string
	Text       string // declarative, people written {@ref}
	Evidence   []string
	ObservedAt string // newest evidence time, PersonClaimStamp
	RoomID     string // room of the evidence (scope filter); a private thread is its own room
	Source     string // episode_item:<id> | extract:<segment> | backfill:<run> | explicit
	Link       string // self | mention | reply | name
	State      string // active | hidden | pending
	CreatedAt  string

	// What the writer read, checked by the write gate and not stored. Lines maps an evidence observation
	// to the text the writer saw (the current text must still be the same); ReadAt (RFC 3339) refuses
	// evidence edited after it. A writer sets whichever it has: an extraction its segment's lines, a
	// derivation the time the episode item was saved.
	Lines  map[string]string
	ReadAt string
}

// Direct reports whether the subject said it themselves (self or explicit); otherwise it is hearsay.
func (c PersonClaim) Direct() bool { return c.SubjectRef == c.SpeakerRef }

// PersonSuppress is one forget or hide marker.
type PersonSuppress struct {
	GuildID   string
	PersonRef string
	Role      string // subject | speaker
	Scope     string // all | topic:<kind:topic> | claim:<id>
	BeforeTS  string // PersonClaimStamp: statements observed at or before it are not written
	By        string // self | operator
	CreatedAt string
}

// PersonClaimDDL creates the person memory tables (run with the application's feature tables).
var PersonClaimDDL = []string{
	`CREATE TABLE IF NOT EXISTS person_claim (
		id TEXT PRIMARY KEY,
		guild_id TEXT NOT NULL,
		subject_ref TEXT NOT NULL,
		speaker_ref TEXT NOT NULL,
		kind TEXT NOT NULL,
		topic TEXT NOT NULL DEFAULT '',
		text TEXT NOT NULL,
		evidence TEXT NOT NULL,
		observed_at TEXT NOT NULL,
		room_id TEXT NOT NULL,
		source TEXT NOT NULL,
		link TEXT NOT NULL,
		state TEXT NOT NULL DEFAULT 'active',
		created_at TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_person_claim_subject ON person_claim(guild_id, subject_ref, state)`,
	`CREATE INDEX IF NOT EXISTS idx_person_claim_speaker ON person_claim(guild_id, speaker_ref)`,
	`CREATE INDEX IF NOT EXISTS idx_person_claim_source ON person_claim(source)`,
	`CREATE TABLE IF NOT EXISTS person_memory_suppress (
		guild_id TEXT NOT NULL,
		person_ref TEXT NOT NULL,
		role TEXT NOT NULL,
		scope TEXT NOT NULL,
		before_ts TEXT NOT NULL,
		by TEXT NOT NULL,
		created_at TEXT NOT NULL,
		PRIMARY KEY(guild_id, person_ref, role, scope)
	) WITHOUT ROWID`,
}

// PersonClaimID is the claim's id: hex sha256 of guild \x00 subject \x00 speaker \x00 the sorted evidence
// ids joined by ",". Every path that writes the same statement gets the same id, so writes are idempotent.
func PersonClaimID(guild, subject, speaker string, evidence []string) string {
	ev := slices.Clone(evidence)
	slices.Sort(ev)
	ev = slices.Compact(ev)
	sum := sha256.Sum256([]byte(guild + "\x00" + subject + "\x00" + speaker + "\x00" + strings.Join(ev, ",")))
	return hex.EncodeToString(sum[:])
}

// PersonTopic is the normalized topic key part: NFC, lower case, spaces collapsed, at most
// PersonTopicRunes runes ("" when nothing is left).
func PersonTopic(s string) string {
	s = strings.ToLower(norm.NFC.String(s))
	s = strings.Join(strings.FieldsFunc(s, unicode.IsSpace), " ")
	if utf8.RuneCountInString(s) > PersonTopicRunes {
		s = strings.TrimSpace(string([]rune(s)[:PersonTopicRunes]))
	}
	return s
}

// PersonTopicKey is the suppress and supersession key of a claim: kind:topic.
func PersonTopicKey(kind, topic string) string { return kind + ":" + PersonTopic(topic) }

// DiscordPersonRef reports whether ref is a Discord person reference (not kakao:, label: or empty).
func DiscordPersonRef(ref string) bool {
	return ref != "" && !strings.HasPrefix(ref, "kakao:") && !strings.HasPrefix(ref, "label:") && !strings.ContainsAny(ref, ":{}")
}
