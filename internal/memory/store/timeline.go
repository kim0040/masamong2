package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"masamong/internal/memory/corpus"
)

// MessageHit is one exact source message matched by SearchMessages. Actor account IDs are
// deliberately omitted: operator search needs the visible label and provenance, not platform IDs.
type MessageHit struct {
	Message corpus.Message
	Score   int
}

// Rooms lists registered rooms with message counts.
func (s *Store) Rooms() ([]RoomStats, error) {
	// the leaf numbers come from one pass over the leaves grouped by room and recipe, not three
	// correlated counts per room: on a large memory each of those reads every leaf of the room again
	type leafCounts struct{ leaves, embedded, open int }
	byRoom := map[[2]string]leafCounts{}
	// both reads in one read transaction: the counts and the rooms are of the same moment
	tx, err := s.DB.Reader().BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	lrows, err := tx.Query(`SELECT room, recipe, COUNT(*), COALESCE(SUM(CASE WHEN leaf_vec IS NOT NULL THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN closed=0 THEN 1 ELSE 0 END),0) FROM memory_leaves GROUP BY room, recipe`)
	if err != nil {
		return nil, err
	}
	for lrows.Next() {
		var room, recipe string
		var c leafCounts
		if err := lrows.Scan(&room, &recipe, &c.leaves, &c.embedded, &c.open); err != nil {
			lrows.Close()
			return nil, err
		}
		byRoom[[2]string{room, recipe}] = c
	}
	if err := lrows.Err(); err != nil {
		lrows.Close()
		return nil, err
	}
	lrows.Close()
	rows, err := tx.Query(`SELECT r.room, r.collection, r.platform, r.label, r.naive_timezone, r.updated_at, r.revision, r.active_recipe,
		(SELECT COUNT(*) FROM memory_observations o WHERE o.room=r.room AND o.deleted_at='')
		FROM memory_rooms r ORDER BY r.room`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RoomStats
	for rows.Next() {
		var r RoomStats
		if err := rows.Scan(&r.Room.Room, &r.Collection, &r.Platform, &r.Label, &r.NaiveTimezone, &r.UpdatedAt, &r.Revision, &r.ActiveRecipe, &r.Messages); err != nil {
			return nil, err
		}
		c := byRoom[[2]string{r.Room.Room, r.ActiveRecipe}]
		r.Leaves, r.Embedded, r.Open = c.leaves, c.embedded, c.open
		out = append(out, r)
	}
	return out, rows.Err()
}

// RoomStats is a room with counts.
type RoomStats struct {
	Room
	Messages int `json:"messages"`
	Leaves   int `json:"leaves"`
	Embedded int `json:"embedded"`
	// Open counts leaves still open (the tail of a room that was active within the gap). They are not
	// embedded until they close, by design, so they do not count against a room being ready.
	Open int `json:"open"`
	// UpdatedAt changes whenever messages are added to the room (and on label changes)
	UpdatedAt    string `json:"-"`
	Revision     int64  `json:"-"` // durable identity of the room's message snapshot
	ActiveRecipe string `json:"-"`
}

// RoomSpan is when a room's stored messages start and end and how many people wrote them.
type RoomSpan struct {
	First    string `json:"first"`
	Last     string `json:"last"`
	Speakers int    `json:"speakers"`
}

// RoomSpans returns the time span and speaker count of every room with messages.
func (s *Store) RoomSpans() (map[string]RoomSpan, error) {
	rows, err := s.DB.Reader().Query(`SELECT room, MIN(parsed_time), MAX(parsed_time), COUNT(DISTINCT actor_label) FROM memory_observations WHERE deleted_at='' GROUP BY room`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]RoomSpan{}
	for rows.Next() {
		var room string
		var sp RoomSpan
		if err := rows.Scan(&room, &sp.First, &sp.Last, &sp.Speakers); err != nil {
			return nil, err
		}
		out[room] = sp
	}
	return out, rows.Err()
}

// TimelineEntry is one closed leaf placed on a timeline.
type TimelineEntry struct {
	LeafID   string   `json:"doc_id"`
	Room     string   `json:"room"`
	Time     string   `json:"time"` // when the leaf's last message was written
	Summary  string   `json:"summary,omitempty"`
	Excerpt  string   `json:"excerpt"`
	Speakers []string `json:"speakers"`
}

// Timeline lists the closed leaves of rooms between from and to (RFC 3339 prefixes, "" = open),
// newest first. Excerpt is the leaf's first lines without its "[room · date]" prefix.
func (s *Store) Timeline(rooms []string, from, to string, limit int) ([]TimelineEntry, error) {
	if len(rooms) == 0 {
		return nil, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(rooms)), ",")
	args := make([]any, 0, len(rooms)+3)
	for _, r := range rooms {
		args = append(args, r)
	}
	q := `SELECT leaf_id, room, end_time, summary, body, speakers FROM memory_leaves WHERE room IN (` + ph + `) AND closed=1`
	if from != "" {
		q += ` AND end_time >= ?`
		args = append(args, from)
	}
	if to != "" {
		q += ` AND end_time < ?`
		args = append(args, to)
	}
	q += ` ORDER BY end_time DESC LIMIT ?`
	args = append(args, max(1, min(limit, 500)))
	rows, err := s.DB.Reader().Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TimelineEntry
	for rows.Next() {
		var e TimelineEntry
		var body, sp string
		if err := rows.Scan(&e.LeafID, &e.Room, &e.Time, &e.Summary, &body, &sp); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(sp), &e.Speakers)
		lines := strings.Split(body, "\n")
		if len(lines) > 1 && strings.HasPrefix(lines[0], "[") {
			lines = lines[1:]
		}
		e.Excerpt = strings.Join(lines[:min(3, len(lines))], "\n")
		out = append(out, e)
	}
	return out, rows.Err()
}

// LatestNativeID is the newest stored native message ID of a room (deleted ones included: they were
// seen). Discord snowflakes grow with time, so it is where a catch-up read continues from.
func (s *Store) LatestNativeID(room string) (string, error) {
	var id sql.NullString
	// snowflakes are compared as numbers: by length first, then as text
	err := s.DB.DB.QueryRow(`SELECT native_message_id FROM memory_observations WHERE room=? AND native_message_id IS NOT NULL
		ORDER BY LENGTH(native_message_id) DESC, native_message_id DESC LIMIT 1`, room).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id.String, err
}

// RecentMessages returns the last n messages of one room in time order (chat context).
func (s *Store) RecentMessages(room string, n int) ([]corpus.Message, error) {
	rows, err := s.DB.DB.Query(`SELECT observation_id, room, source_room, actor_label, actor_account, content, parsed_time
		FROM memory_observations WHERE room=? AND deleted_at='' ORDER BY parsed_time DESC, observation_id DESC LIMIT ?`, room, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []corpus.Message
	for rows.Next() {
		var m corpus.Message
		if err := rows.Scan(&m.ObservationID, &m.Room, &m.RoomOriginal, &m.ActorLabel, &m.ActorAccount, &m.Content, &m.ParsedTime); err != nil {
			return nil, err
		}
		m.Content = strings.TrimSpace(m.Content)
		out = append(out, m)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

// LiveLine is one stored message of a room with its platform ID, for rebuilding a channel's recent
// conversation after a restart (application/live.go).
type LiveLine struct {
	ObservationID, NativeID, Account, Label, Content, ParsedTime string
}

// RecentLines returns the last n messages of one room in time order, with their platform message IDs.
func (s *Store) RecentLines(room string, n int) ([]LiveLine, error) {
	rows, err := s.DB.DB.Query(`SELECT observation_id, COALESCE(native_message_id,''), actor_account, actor_label, content, parsed_time
		FROM memory_observations WHERE room=? AND deleted_at='' ORDER BY parsed_time DESC, observation_id DESC LIMIT ?`, room, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LiveLine
	for rows.Next() {
		var l LiveLine
		if err := rows.Scan(&l.ObservationID, &l.NativeID, &l.Account, &l.Label, &l.Content, &l.ParsedTime); err != nil {
			return nil, err
		}
		l.Content = strings.TrimSpace(l.Content)
		out = append(out, l)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

// SearchMessages performs bounded lexical search over exact source messages. It is intentionally
// separate from semantic memory search: results are raw observations, use no model, and cost zero.
func (s *Store) SearchMessages(rooms []string, query string, limit int) ([]MessageHit, error) {
	if len(rooms) == 0 || strings.TrimSpace(query) == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 20
	}
	limit = min(limit, 100)
	seen := map[string]bool{}
	var terms []string
	for _, term := range strings.Fields(strings.ToLower(query)) {
		if term != "" && !seen[term] {
			seen[term] = true
			terms = append(terms, term)
			if len(terms) == 8 {
				break
			}
		}
	}
	if len(terms) == 0 {
		terms = []string{strings.ToLower(strings.TrimSpace(query))}
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(rooms)), ",")
	args := make([]any, 0, len(rooms)+len(terms)+1)
	for _, room := range rooms {
		args = append(args, room)
	}
	like := make([]string, 0, len(terms))
	for _, term := range terms {
		like = append(like, "LOWER(content) LIKE ? ESCAPE '!'")
		args = append(args, "%"+escapeLike(term)+"%")
	}
	scanLimit := min(5000, max(200, limit*100))
	args = append(args, scanLimit)
	rows, err := s.DB.DB.Query(`SELECT observation_id, room, source_room, actor_label, actor_account, content, parsed_time
		FROM memory_observations WHERE room IN (`+ph+`) AND deleted_at='' AND (`+strings.Join(like, " OR ")+`)
		ORDER BY parsed_time DESC, observation_id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	phrase := strings.ToLower(strings.TrimSpace(query))
	var out []MessageHit
	for rows.Next() {
		var m corpus.Message
		if err := rows.Scan(&m.ObservationID, &m.Room, &m.RoomOriginal, &m.ActorLabel, &m.ActorAccount, &m.Content, &m.ParsedTime); err != nil {
			return nil, err
		}
		body := strings.ToLower(m.Content)
		score := 0
		for _, term := range terms {
			score += strings.Count(body, term)
		}
		if phrase != "" && strings.Contains(body, phrase) {
			score += 10
		}
		m.ActorAccount = ""
		out = append(out, MessageHit{Message: m, Score: score})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		if out[i].Message.ParsedTime != out[j].Message.ParsedTime {
			return out[i].Message.ParsedTime > out[j].Message.ParsedTime
		}
		return out[i].Message.ObservationID < out[j].Message.ObservationID
	})
	return out[:min(limit, len(out))], nil
}

// A reply's addressee survives edits. For other edited requests, only the new
// words (not the previous revision's acceptance) can address the bot.
const (
	CallAccepted = iota
	CallReply
	CallEdited
)

// ChatTurn is one message of the bot's conversation in a channel (the bot's own replies are not
// memory observations; they are kept here only as conversation context).
type ChatTurn struct {
	Role, Speaker, Content, CreatedAt string
	CallAddress                       int
	// SpokenAt orders the conversation by delivery; CreatedAt remains the retention clock.
	SpokenAt string
	// DeliveryTurn identifies the original assistant row for later delivered parts.
	// A thread's copy is not the delivery being extended.
	DeliveryTurn string
	// DeliverySpans records byte boundaries and delivery times, never a second copy of the words.
	DeliverySpans string
	// NativeID is the Discord request a user or assistant turn came from (empty for lab turns).
	// An edit or deletion of that message changes or removes the turn (EditMessage).
	NativeID string
	// SpeakerID is the account a user turn came from: withdrawing memory consent removes that
	// person's turns (DeleteAccountMessages), in channels that store messages and in those that don't.
	SpeakerID string
	// SourceIDs are the messages and earlier turns read to produce this turn. An earlier turn is a link
	// (ChatLink), not a copy of its sources, so the list stays the size of what was read: a message is here
	// only when its words reached the prompt (ChatReaders), and ChatLineage follows the links for a withdrawal.
	// Rows from earlier releases hold a flat list, and count as having read all of it.
	SourceIDs []string
	// DerivedFrom are other messages part of this turn's text was made from (the description of a
	// picture in the message a user turn replied to). Deleting or editing one of them removes the turn,
	// user turns included (holder kind "chat_from").
	DerivedFrom []string
	// Merged are the other messages of a burst that was answered as one turn: their words are in Content,
	// and the connector read them before any mark could be taken, so nothing excuses them. The turn is tied
	// to them like DerivedFrom (holder kind "chat_from").
	Merged []string
	// ReadAsOf is the SettledMark the turn took before reading its SourceIDs and DerivedFrom (0: none,
	// nothing is excused).
	ReadAsOf int64
	// Copied says the row is a copy of a kept turn (a thread's seed), read from the store after ReadAsOf was
	// taken. So its own message counts as read at that mark too. A live turn's message came from the
	// connector before its mark, and its tombstone refuses the turn whatever the mark says.
	Copied bool
	// Revision says the row holds the words of this revision of its message, an edit made while the turn ran. The
	// tombstone of that edit does not refuse it (revisedTurnRefused); a later change of the message does.
	Revision string
}

// AppendChatTurns commits turns and their source index together, then trims to the last keep turns.
// A trim interrupted by a crash is repeated by the next append.
func (s *Store) AppendChatTurns(channelKey string, turns []ChatTurn, keep int) error {
	var decide func() []ChatTurn
	if len(turns) > 0 {
		decide = func() []ChatTurn { return turns }
	}
	_, err := s.AppendDecidedChatTurns(channelKey, decide, keep)
	return err
}

// AppendDecidedChatTurns is AppendChatTurns for turns that depend on what happens up to the commit: decide is called
// inside the write transaction, when no change of the store can come between it and the rows. It may take a short lock
// of its own, and must not call the store. committed distinguishes a later prune failure from a failed write.
func (s *Store) AppendDecidedChatTurns(channelKey string, decide func() []ChatTurn, keep int) (committed bool, err error) {
	if decide != nil {
		err := s.DB.WithWriter(func(tx *sql.Tx) error {
			for _, t := range decide() {
				if t.Copied {
					t.DeliveryTurn = ""
				}
				if !s.tidb() {
					native, read := t.NativeID, uniqueSources(t.SourceIDs, t.DerivedFrom)
					if t.Copied {
						native, read = "", uniqueSources(read, []string{t.NativeID})
					}
					if t.Revision != "" {
						refused, err := revisedTurnRefused(tx, t.NativeID, t.Revision)
						if err != nil {
							return err
						}
						if refused {
							continue
						}
						native = "" // its own edit's tombstone is what this row answers
					}
					blocked, err := MessageBlocked(tx, native, t.SpeakerID)
					if !blocked && err == nil {
						blocked, err = SourcesBlockedAsOf(tx, read, t.ReadAsOf)
					}
					if !blocked && err == nil {
						blocked, err = SourcesBlocked(tx, uniqueSources(t.Merged))
					}
					if err != nil {
						return err
					}
					if blocked {
						continue
					}
				}
				result, err := tx.Exec(`INSERT INTO chat_turns(channel_key,role,speaker,content,created_at,spoken_at,native_id,speaker_id,delivery_turn,delivery_spans,call_address,evidence_state) VALUES(?,?,?,?,?,?,?,?,?,?,?,1)`, channelKey, t.Role, t.Speaker, t.Content, s.now(), t.SpokenAt, t.NativeID, t.SpeakerID, t.DeliveryTurn, t.DeliverySpans, t.CallAddress)
				if err != nil {
					return err
				}
				if !s.tidb() {
					id, err := result.LastInsertId()
					if err != nil {
						return err
					}
					if err := IndexSourceRefs(tx, "chat", ChatLink(id), uniqueSources(t.SourceIDs, []string{t.NativeID, PersonRef(t.SpeakerID)})); err != nil {
						return err
					}
					from := uniqueSources(t.DerivedFrom, t.Merged)
					if t.Copied && len(t.SourceIDs) > 0 {
						from = uniqueSources(from, t.SourceIDs[:1]) // a copy holds the words of the turn it copies
					}
					if len(from) > 0 {
						if err := IndexSourceRefs(tx, "chat_from", ChatLink(id), from); err != nil {
							return err
						}
					}
				}
			}
			return nil
		})
		if err != nil {
			return false, err
		}
		committed = true
	}
	if keep > 0 {
		// the derived table lets MySQL/TiDB read the table it deletes from
		if _, err := s.DB.DB.Exec(`DELETE FROM chat_turns WHERE channel_key=? AND id <= (SELECT id FROM (SELECT id FROM chat_turns
			WHERE channel_key=? ORDER BY id DESC LIMIT 1 OFFSET ?) AS cutoff)`, channelKey, channelKey, keep); err != nil {
			return committed, err
		}
	}
	return committed, nil
}

// RecentChatTurns returns the last n turns of a channel in order. SourceIDs is what reading the turn
// means for a turn built on it: a link to it and its speaker, and for a user turn the messages its words are: its own
// and those in DerivedFrom. A reply has its request's ID as its message, and reading the reply is not reading that.
func (s *Store) RecentChatTurns(channelKey string, n int) ([]ChatTurn, error) {
	if s.DB.HasPendingEvents("source-change") {
		return nil, nil
	}

	// A turn with a tombstone on its link read something that changed and only has not been removed yet (the job
	// failed after its walk, and waits for its retry): it is not read. A user turn whose edit has settled holds the
	// new words and is read again.
	notGoing := ""
	legacyAddress := "0"
	if !s.tidb() {
		legacyAddress = `CASE WHEN role='user' AND EXISTS (SELECT 1 FROM privacy_tombstones t WHERE t.kind='message' AND t.source_id=chat_turns.native_id) THEN 2 ELSE 0 END`
		notGoing = ` AND NOT EXISTS (SELECT 1 FROM privacy_tombstones t WHERE t.kind IN ('source','going') AND t.source_id='chat:'||chat_turns.id
			AND NOT EXISTS (SELECT 1 FROM privacy_settled d WHERE d.source_id=t.source_id))`
	}
	rows, err := s.DB.DB.Query(`SELECT id,role,speaker,content,created_at,spoken_at,native_id,speaker_id,delivery_turn,COALESCE(delivery_spans,''),COALESCE(call_address,`+legacyAddress+`) FROM chat_turns WHERE channel_key=?`+notGoing+` ORDER BY id DESC LIMIT ?`, channelKey, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChatTurn
	for rows.Next() {
		var t ChatTurn
		var id int64
		if err := rows.Scan(&id, &t.Role, &t.Speaker, &t.Content, &t.CreatedAt, &t.SpokenAt, &t.NativeID, &t.SpeakerID, &t.DeliveryTurn, &t.DeliverySpans, &t.CallAddress); err != nil {
			return nil, err
		}
		t.SourceIDs = []string{ChatLink(id)}
		if t.Role == "user" {
			t.SourceIDs = append(t.SourceIDs, t.NativeID)
		}
		t.SourceIDs = uniqueSources(t.SourceIDs, []string{PersonRef(t.SpeakerID)})
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	if s.tidb() || len(out) == 0 {
		return out, nil
	}
	return s.withDerivedFrom(out)
}

// withDerivedFrom adds what each turn's words were partly made from. Reading a user turn is reading those messages too.
func (s *Store) withDerivedFrom(turns []ChatTurn) ([]ChatTurn, error) {
	links := make([]string, len(turns))
	for i, t := range turns {
		links[i] = t.SourceIDs[0] // the turn's own link comes first
	}
	raw, err := json.Marshal(links)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.DB.Query(`SELECT holder_id,source_id FROM evidence_refs WHERE holder_kind='chat_from' AND holder_id IN (SELECT value FROM json_each(?))`, string(raw))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	from := map[string][]string{}
	for rows.Next() {
		var link, source string
		if err := rows.Scan(&link, &source); err != nil {
			return nil, err
		}
		from[link] = append(from[link], source)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range turns {
		turns[i].DerivedFrom = from[links[i]]
		if turns[i].Role == "user" {
			turns[i].SourceIDs = uniqueSources(turns[i].SourceIDs, from[links[i]])
		}
	}
	return turns, nil
}

// ChatTurnsToCopy is RecentChatTurns for writing the turns to another channel: each one also says what its
// words were partly made from, so the copy stays tied to it (DerivedFrom).
func (s *Store) ChatTurnsToCopy(channelKey string, n int) ([]ChatTurn, error) {
	return s.RecentChatTurns(channelKey, n)
}
