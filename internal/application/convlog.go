package application

import (
	"encoding/json"
	"time"
)

// conversation_log records the bot's conversation decisions — a name-only mention judged as said to the
// bot or about it, a follow-up without a tag answered or left alone, a thread offered, accepted or
// declined — with the structural features behind each and, later, what people did next. It holds no
// message text, only IDs and numbers. The follow-up window and the chime-in features are experimental and
// off by default (decision D7): this log is the reference data their defaults are tuned from, so it is
// written in every channel the bot reads, also where those features are off.
func init() {
	featureDDL = append(featureDDL, `CREATE TABLE IF NOT EXISTS conversation_log (
		id TEXT PRIMARY KEY,
		guild_id TEXT NOT NULL,
		channel_id TEXT NOT NULL,
		message_id TEXT NOT NULL DEFAULT '',
		author_ref TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		kind TEXT NOT NULL,
		decision TEXT NOT NULL,
		mode TEXT NOT NULL DEFAULT '',
		score REAL NOT NULL DEFAULT 0,
		features TEXT NOT NULL DEFAULT '{}',
		outcome TEXT NOT NULL DEFAULT '',
		outcome_at TEXT NOT NULL DEFAULT '',
		cost_usd REAL NOT NULL DEFAULT 0
	)`, `CREATE INDEX IF NOT EXISTS idx_conversation_log_channel ON conversation_log(guild_id, channel_id, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_conversation_log_kind ON conversation_log(kind, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_conversation_log_author ON conversation_log(author_ref)`)
}

// Kinds of conversation decisions.
const (
	convNameCall = "name_call" // a message said the bot's name without mentioning or replying to it
	convFollow   = "follow"    // an untagged message from the person the bot just answered
	convThread   = "thread"    // a thread offered, opened, declined, or closed
	convBackoff  = "backoff"   // the bot stepped back from chiming in (a quiet cue, a 👎, a person's opt-out)
	convBanter   = "banter"    // a playful mention of the bot answered with a line or an emoji, or not
)

// convDecision is one row of conversation_log.
type convDecision struct {
	Guild, Channel, Message, Author string
	Kind, Decision, Mode            string
	Score                           float64
	Features                        map[string]any // numbers and enums only: never message text
	CostUSD                         float64
}

// logConversation writes a decision and returns its id (so its outcome can be noted later). Failures are
// logged at debug level and never affect the reply.
func (s *Service) logConversation(d convDecision) string {
	db, err := s.adminDB()
	if err != nil || d.Channel == "" {
		return ""
	}
	id := newID("cv_")
	feats := "{}"
	if len(d.Features) > 0 {
		if raw, err := json.Marshal(d.Features); err == nil {
			feats = string(raw)
		}
	}
	if _, err := db.DB.Exec(`INSERT INTO conversation_log(id,guild_id,channel_id,message_id,author_ref,created_at,kind,decision,mode,score,features,cost_usd)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, id, d.Guild, d.Channel, d.Message, personRef(d.Author), s.Clock.Now().UTC().Format(time.RFC3339Nano),
		d.Kind, d.Decision, d.Mode, d.Score, feats, d.CostUSD); err != nil {
		s.Log.Debug("conversation log write failed", "err", err.Error())
		return ""
	}
	return id
}

// noteConversationOutcome records what followed a decision (the person tagged the bot again, turned to a
// friend, accepted the thread …), once.
func (s *Service) noteConversationOutcome(id, outcome string) {
	if id == "" {
		return
	}
	db, err := s.adminDB()
	if err != nil {
		return
	}
	if _, err := db.DB.Exec(`UPDATE conversation_log SET outcome=?, outcome_at=? WHERE id=? AND outcome=''`, outcome,
		s.Clock.Now().UTC().Format(time.RFC3339Nano), id); err != nil {
		s.Log.Debug("conversation outcome write failed", "err", err.Error())
	}
}
