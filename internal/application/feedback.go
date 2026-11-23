package application

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"masamong/internal/action"
	"masamong/internal/errcode"
	"masamong/internal/memory/blocks"
	"masamong/internal/memory/store"
)

// Feedback on the bot's replies, gathered without adding anything to the chat. The bot never puts
// reactions on its own messages: it only reads the ones people add themselves (👍 ❤️ 😂 … count as
// good, 👎 ❌ … as bad, anything else is ignored), and the "Rate this reply" item in Discord's Apps
// menu on a reply opens buttons only the person who chose it sees. Each rating is tied to the turn
// that produced the reply, so the console's conversation records and the lab can use them.
//
// Stored: which reply (turn id), a one-way hash of who rated it, good or bad, an optional reason and
// where it came from. No message text, no Discord user id. People who withdrew memory consent and
// blocked people are not recorded. Ratings go with the turn records' retention (retention.turn_days).
//
// The same table also holds implicit signals (source "implicit:*", rating always 0, implicit_feedback.go).
// They are not ratings: every sum, count and chip of ratings leaves them out and shows them apart.
// The console operator's verdicts (source "operator", operator_marks.go) are kept apart in the same way.

var feedbackSQLite = []string{
	`CREATE TABLE IF NOT EXISTS reply_messages (
		message_id TEXT PRIMARY KEY,
		turn_id TEXT NOT NULL,
		guild_id TEXT NOT NULL,
		channel_id TEXT NOT NULL,
		at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS turn_feedback (
		turn_id TEXT NOT NULL,
		user_hash TEXT NOT NULL,
		source TEXT NOT NULL,
		rating INTEGER NOT NULL,
		reason TEXT NOT NULL DEFAULT '',
		at TEXT NOT NULL,
		PRIMARY KEY (turn_id, user_hash, source)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_turn_feedback_at ON turn_feedback(at)`,
}

var feedbackTiDB = []string{
	`CREATE TABLE IF NOT EXISTS reply_messages (
		message_id VARCHAR(64) NOT NULL PRIMARY KEY,
		turn_id VARCHAR(64) NOT NULL,
		guild_id VARCHAR(64) NOT NULL,
		channel_id VARCHAR(64) NOT NULL,
		at VARCHAR(64) NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS turn_feedback (
		turn_id VARCHAR(64) NOT NULL,
		user_hash VARCHAR(32) NOT NULL,
		source VARCHAR(64) NOT NULL,
		rating INT NOT NULL,
		reason VARCHAR(32) NOT NULL DEFAULT '',
		at VARCHAR(64) NOT NULL,
		PRIMARY KEY (turn_id, user_hash, source),
		KEY idx_turn_feedback_at (at)
	)`,
}

func init() {
	adminSQLite = append(adminSQLite, feedbackSQLite...)
	adminTiDB = append(adminTiDB, feedbackTiDB...)
}

// FeedbackReasons are the reasons a person can give for a bad reply (menu only).
var FeedbackReasons = []string{"wrong", "forgot", "tone", "too_long", "other"}

// reactionRating maps a reaction to good (1) or bad (-1); 0 means it says nothing about the reply.
func reactionRating(emoji string) int {
	switch strings.TrimSuffix(emoji, "️") {
	case "👍", "❤", "😂", "🤣", "🔥", "💯", "👏", "😆", "✅", "🥰", "😍", "🙏", "👌":
		return 1
	case "👎", "❌", "😕", "😡", "🙄", "💩", "🤦":
		return -1
	}
	return 0
}

func feedbackUser(userID string) string { return personRef(userID) }

// personRef is the one-way reference to a person kept in logs instead of their account ID: enough to
// find their rows again when they withdraw consent (forgetPerson), not to tell who they are. "" for
// no one.
func personRef(userID string) string { return store.PersonRef(userID) }

// NoteReplyMessages remembers which turn the bot's reply messages came from.
func (s *Service) NoteReplyMessages(turnID, guildID, channelID string, messageIDs []string, ends ...int) {
	if turnID == "" || len(messageIDs) == 0 {
		return
	}
	db, err := s.adminDB()
	now := s.Clock.Now().UTC().Format(time.RFC3339Nano)
	if err == nil {
		err = db.WithWriter(func(tx *sql.Tx) error {
			for i, id := range messageIDs {
				if id == "" {
					return errors.New("reply registration: missing message ID")
				}
				end := 0
				if i < len(ends) {
					end = ends[i]
				}
				if _, err := tx.Exec(insertReplyMessage(db.IsSQLite()), id, turnID, guildID, channelID, now, "", end); err != nil {
					return err
				}
				var owner string
				if err := tx.QueryRow(`SELECT turn_id FROM reply_messages WHERE message_id=?`, id).Scan(&owner); err != nil {
					return err
				}
				if owner != turnID {
					return errors.New("reply registration: message already belongs to another turn")
				}
			}
			return nil
		})
	}
	if err != nil {
		s.rejectDelivery(guildID, channelID, turnID, messageIDs)
		s.Log.Warn("could not register delivered reply; not replayable", "turn", turnID, "parts", len(messageIDs), "err", err)
	}
}

func insertReplyMessage(sqlite bool) string {
	insert := "INSERT IGNORE"
	if sqlite {
		insert = "INSERT OR IGNORE"
	}
	return insert + ` INTO reply_messages(message_id,turn_id,guild_id,channel_id,at,request_id,part_end) VALUES(?,?,?,?,?,?,?)`
}

// DeliverReplyContinuation keeps a reply to a newly posted part behind its
// registration, even when the gateway event arrives before the REST acknowledgement.
func (s *Service) DeliverReplyContinuation(ctx context.Context, guildID, channelID, headID string, send func() (string, []string, []int)) error {
	release, err := s.turns.enter(ctx, blocks.ChannelKey(guildID, channelID), 0)
	if err != nil {
		return err
	}
	defer release()
	said, ids, ends := send()
	return s.NoteReplyContinuation(guildID, channelID, headID, said, ids, ends...)
}

// NoteReplyContinuation attaches only the parts a later "more" press actually posted.
// The original row owns their provenance: never insert a new answer if it was removed.
func (s *Service) NoteReplyContinuation(guildID, channelID, headID, said string, ids []string, ends ...int) error {
	if len(ids) == 0 {
		return nil
	}
	db, err := s.adminDB()
	if err != nil {
		s.rejectDelivery(guildID, channelID, "", ids)
		return err
	}
	now := s.Clock.Now().UTC()
	observed := map[string]time.Time{}
	for _, l := range s.liveLines(guildID, channelID) {
		if l.Bot {
			observed[l.ID] = l.At
		}
	}
	key := blocks.ChannelKey(guildID, channelID)
	byteLen, appendText := "LENGTH(content)", "CONCAT(content,?)"
	if db.IsSQLite() {
		byteLen, appendText = "LENGTH(CAST(content AS BLOB))", "content || ?"
	}
	err = db.WithWriter(func(tx *sql.Tx) error {
		var turn, request string
		err := tx.QueryRow(`SELECT turn_id,COALESCE(request_id,'') FROM reply_messages WHERE message_id=? AND guild_id=? AND channel_id=?`,
			headID, guildID, channelID).Scan(&turn, &request)
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("continued reply: no registered head")
		}
		if err != nil {
			return err
		}
		var base int
		var spoken, encoded string
		err = tx.QueryRow(`SELECT `+byteLen+`,spoken_at,COALESCE(delivery_spans,'') FROM chat_turns
			WHERE channel_key=? AND delivery_turn=? AND native_id=? AND role='assistant'`, key, turn, request).Scan(&base, &spoken, &encoded)
		if errors.Is(err, sql.ErrNoRows) {
			request = "" // registration for feedback only; never re-create a removed answer
		} else if err != nil {
			return err
		}
		var spans []deliverySpan
		if encoded != "" {
			if err := json.Unmarshal([]byte(encoded), &spans); err != nil {
				return err
			}
		} else if base != 0 {
			at, _ := time.Parse(time.RFC3339Nano, spoken)
			spans = append(spans, deliverySpan{End: base, At: at})
		}
		var addition strings.Builder
		grouped := len(ends) == 0 // older connectors supplied a single text for the group
		if !grouped && (len(ids) != len(ends) || ends[len(ends)-1] != len(said)) {
			return errors.New("continued reply: invalid part boundaries")
		}
		from, fresh := 0, 0
		var last time.Time
		for i, id := range ids {
			if id == "" {
				return errors.New("continued reply: missing post ID")
			}
			to := len(said)
			if !grouped {
				to = ends[i]
			}
			if to < from || to > len(said) {
				return errors.New("continued reply: invalid part boundary")
			}
			part := said[from:to]
			if !grouped {
				from = to
			}
			res, err := tx.Exec(insertReplyMessage(db.IsSQLite()), id, turn, guildID, channelID, now.Format(time.RFC3339Nano), request, 0)
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			if n == 0 || request == "" || said == "" {
				continue
			}
			fresh++
			at := snowflakeTime(id)
			if at.IsZero() {
				at = observed[id]
			}
			if at.IsZero() {
				at = now
			}
			start := base + 2
			if fresh == 1 {
				addition.WriteString("\n\n")
			}
			if !grouped || fresh == 1 {
				start = base + addition.Len()
				addition.WriteString(part)
			}
			end := base + addition.Len()
			if _, err := tx.Exec(`UPDATE reply_messages SET rest_start=?,part_end=? WHERE message_id=?`, start, end, id); err != nil {
				return err
			}
			if !grouped {
				spans = append(spans, deliverySpan{End: end, At: at})
			}
			if at.After(last) {
				last = at
			}
		}
		if fresh == 0 {
			return nil
		}
		if grouped {
			if fresh != len(ids) {
				return errors.New("continued reply: partially overlapping group without boundaries")
			}
			spans = append(spans, deliverySpan{End: base + addition.Len(), At: last})
		}
		_, err = tx.Exec(`UPDATE chat_turns SET content=`+appendText+`,delivery_spans=? WHERE channel_key=? AND delivery_turn=? AND native_id=? AND role='assistant'`,
			addition.String(), encodeSpans(spans), key, turn, request)
		return err
	})
	if err != nil {
		s.rejectDelivery(guildID, channelID, "", ids)
	}
	return err
}

// linkPostedReply publishes the request for all parts together, before the channel gate opens.
// A post not cleared for replay stays available for feedback, but not conversation history.
func (s *Service) linkPostedReply(turnID, requestID, channelKey string) {
	if db, err := s.adminDB(); err == nil {
		if _, err := db.DB.Exec(`UPDATE reply_messages SET request_id=? WHERE turn_id=? AND EXISTS
			(SELECT 1 FROM chat_turns WHERE channel_key=? AND delivery_turn=? AND native_id=? AND role='assistant')`, requestID, turnID, channelKey, turnID, requestID); err != nil {
			s.Log.Warn("could not link posted reply", "err", err)
		}
	} else {
		s.Log.Warn("could not open posted reply registry", "err", err)
	}
}

// ReplyTurn is the turn a bot message answered, when it is one of the bot's replies.
func (s *Service) ReplyTurn(messageID string) (string, bool) {
	db, err := s.adminDB()
	if err != nil {
		return "", false
	}
	var turn string
	if db.Reader().QueryRow(`SELECT turn_id FROM reply_messages WHERE message_id=?`, messageID).Scan(&turn) != nil {
		return "", false
	}
	return turn, true
}

func (s *Service) feedbackAllowed(guildID, userID string) bool {
	return userID != "" && s.MemoryAllowed(userID) && !s.BlockedIn(guildID, userID)
}

// ReactionFeedback records (added) or withdraws a reaction someone put on a bot reply. Reactions that
// say nothing about the reply, and messages that are not replies, are ignored.
func (s *Service) ReactionFeedback(guildID, messageID, userID, emoji string, added bool) {
	rating := reactionRating(emoji)
	if rating == 0 || !s.feedbackAllowed(guildID, userID) {
		return
	}
	turn, ok := s.ReplyTurn(messageID)
	if !ok {
		return
	}
	db, err := s.adminDB()
	if err != nil {
		return
	}
	source := "reaction:" + strings.TrimSuffix(emoji, "️")
	if !added {
		_, _ = db.DB.Exec(`DELETE FROM turn_feedback WHERE turn_id=? AND user_hash=? AND source=?`, turn, feedbackUser(userID), source)
		return
	}
	s.putFeedback(turn, userID, source, rating, "")
	s.partFeedback(turn, rating > 0)
	if rating < 0 {
		s.banterDisliked(turn)
	}
}

// RateReply records a rating from the "Rate this reply" menu (good or bad, and for bad an optional
// reason). It reports false when the message is not one of the bot's replies.
func (s *Service) RateReply(guildID, messageID, userID string, good bool, reason string) bool {
	turn, ok := s.ReplyTurn(messageID)
	if !ok {
		return false
	}
	if !s.feedbackAllowed(guildID, userID) {
		return true // nothing kept, but the person is thanked all the same
	}
	rating := -1
	if good {
		rating, reason = 1, ""
	}
	known := reason == ""
	for _, r := range FeedbackReasons {
		known = known || r == reason
	}
	if !known {
		reason = "other"
	}
	s.putFeedback(turn, userID, "menu", rating, reason)
	s.partFeedback(turn, rating > 0)
	if rating < 0 {
		s.banterDisliked(turn)
	}
	return true
}

func (s *Service) putFeedback(turn, userID, source string, rating int, reason string) {
	db, err := s.adminDB()
	if err != nil {
		return
	}
	now := s.Clock.Now().UTC().Format(time.RFC3339)
	q := `INSERT INTO turn_feedback(turn_id, user_hash, source, rating, reason, at) VALUES(?,?,?,?,?,?)
		ON CONFLICT(turn_id, user_hash, source) DO UPDATE SET rating=excluded.rating, reason=excluded.reason, at=excluded.at`
	if !db.IsSQLite() {
		q = `INSERT INTO turn_feedback(turn_id, user_hash, source, rating, reason, at) VALUES(?,?,?,?,?,?)
			ON DUPLICATE KEY UPDATE rating=VALUES(rating), reason=VALUES(reason), at=VALUES(at)`
	}
	if _, err := db.DB.Exec(q, turn, feedbackUser(userID), source, rating, reason, now); err == nil {
		s.emit("turn", turn, map[string]any{"feedback": rating})
	}
}

// turnFeedback sums, per turn, the ratings people gave (good and bad counts and the reasons) and,
// separately, the implicit signals noted on it (count per kind, implicit_feedback.go). The two never mix:
// an implicit row is never counted as good or bad.
func (s *Service) turnFeedback(ids []string) (rated map[string]map[string]any, implicit map[string]map[string]int) {
	rated, implicit = map[string]map[string]any{}, map[string]map[string]int{}
	db, err := s.adminDB()
	if err != nil || len(ids) == 0 {
		return rated, implicit
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := db.Reader().Query(`SELECT turn_id, source, rating, reason FROM turn_feedback WHERE turn_id IN (?`+strings.Repeat(",?", len(ids)-1)+`)`, args...)
	if err != nil {
		return rated, implicit
	}
	defer rows.Close()
	for rows.Next() {
		var id, source, reason string
		var rating int
		if rows.Scan(&id, &source, &rating, &reason) != nil {
			continue
		}
		if source == operatorSource { // the operator's verdict is shown apart (operator_marks.go)
			continue
		}
		if kind, ok := strings.CutPrefix(source, implicitSource); ok {
			if implicit[id] == nil {
				implicit[id] = map[string]int{}
			}
			implicit[id][kind]++
			continue
		}
		f := rated[id]
		if f == nil {
			f = map[string]any{"good": 0, "bad": 0}
			rated[id] = f
		}
		if rating > 0 {
			f["good"] = f["good"].(int) + 1
		} else {
			f["bad"] = f["bad"].(int) + 1
		}
		if reason != "" {
			rs, _ := f["reasons"].([]string)
			f["reasons"] = append(rs, reason)
		}
	}
	return rated, implicit
}

// feedbackAction: feedback.summary (counts for the console: last 30 days; the implicit signals apart from the ratings).
func (s *Service) feedbackAction(req action.Request) action.Result {
	db, err := s.adminDB()
	if err != nil {
		return s.failErr(req, errcode.IO, "error.storage", err, nil)
	}
	since := s.Clock.Now().AddDate(0, 0, -max(intVal(inputMap(req), "days", 30), 1)).UTC().Format(time.RFC3339)
	// Only what people rated: the implicit signals (below) and the operator's marks (operator_marks.go) are counted apart and never as good or bad.
	var good, bad int
	_ = db.Reader().QueryRow(`SELECT COALESCE(SUM(CASE WHEN rating>0 THEN 1 ELSE 0 END),0), COALESCE(SUM(CASE WHEN rating<0 THEN 1 ELSE 0 END),0) FROM turn_feedback WHERE at>=? AND source NOT LIKE 'implicit:%' AND source<>'operator'`, since).Scan(&good, &bad)
	reasons := map[string]int{}
	if rows, err := db.Reader().Query(`SELECT reason, COUNT(*) FROM turn_feedback WHERE at>=? AND reason<>'' AND source NOT LIKE 'implicit:%' AND source<>'operator' GROUP BY reason`, since); err == nil {
		defer rows.Close()
		for rows.Next() {
			var r string
			var n int
			if rows.Scan(&r, &n) == nil {
				reasons[r] = n
			}
		}
	}
	implicit := map[string]int{}
	if rows, err := db.Reader().Query(`SELECT source, COUNT(*) FROM turn_feedback WHERE at>=? AND source LIKE 'implicit:%' GROUP BY source`, since); err == nil {
		defer rows.Close()
		for rows.Next() {
			var src string
			var n int
			if rows.Scan(&src, &n) == nil {
				implicit[strings.TrimPrefix(src, implicitSource)] = n
			}
		}
	}
	return action.OK(req, map[string]any{"good": good, "bad": bad, "reasons": reasons, "implicit": implicit, "since": since})
}
