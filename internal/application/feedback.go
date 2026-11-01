package application

import (
	"strings"
	"time"

	"masamong/internal/action"
	"masamong/internal/errcode"
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
func (s *Service) NoteReplyMessages(turnID, guildID, channelID string, messageIDs []string) {
	db, err := s.adminDB()
	if err != nil || turnID == "" {
		return
	}
	now := s.Clock.Now().UTC().Format(time.RFC3339)
	for _, id := range messageIDs {
		if id == "" {
			continue
		}
		q := `INSERT OR IGNORE INTO reply_messages(message_id, turn_id, guild_id, channel_id, at) VALUES(?,?,?,?,?)`
		if !db.IsSQLite() {
			q = `INSERT IGNORE INTO reply_messages(message_id, turn_id, guild_id, channel_id, at) VALUES(?,?,?,?,?)`
		}
		_, _ = db.DB.Exec(q, id, turnID, guildID, channelID, now)
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

// turnFeedback sums the ratings of the given turns: good and bad counts and the reasons given.
func (s *Service) turnFeedback(ids []string) map[string]map[string]any {
	out := map[string]map[string]any{}
	db, err := s.adminDB()
	if err != nil || len(ids) == 0 {
		return out
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := db.Reader().Query(`SELECT turn_id, rating, reason FROM turn_feedback WHERE turn_id IN (?`+strings.Repeat(",?", len(ids)-1)+`)`, args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id, reason string
		var rating int
		if rows.Scan(&id, &rating, &reason) != nil {
			continue
		}
		f := out[id]
		if f == nil {
			f = map[string]any{"good": 0, "bad": 0}
			out[id] = f
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
	return out
}

// feedbackAction: feedback.summary (counts for the console: last 30 days, bad replies first).
func (s *Service) feedbackAction(req action.Request) action.Result {
	db, err := s.adminDB()
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	since := s.Clock.Now().AddDate(0, 0, -max(intVal(inputMap(req), "days", 30), 1)).UTC().Format(time.RFC3339)
	var good, bad int
	_ = db.Reader().QueryRow(`SELECT COALESCE(SUM(CASE WHEN rating>0 THEN 1 ELSE 0 END),0), COALESCE(SUM(CASE WHEN rating<0 THEN 1 ELSE 0 END),0) FROM turn_feedback WHERE at>=?`, since).Scan(&good, &bad)
	reasons := map[string]int{}
	if rows, err := db.Reader().Query(`SELECT reason, COUNT(*) FROM turn_feedback WHERE at>=? AND reason<>'' GROUP BY reason`, since); err == nil {
		defer rows.Close()
		for rows.Next() {
			var r string
			var n int
			if rows.Scan(&r, &n) == nil {
				reasons[r] = n
			}
		}
	}
	return action.OK(req, map[string]any{"good": good, "bad": bad, "reasons": reasons, "since": since})
}
