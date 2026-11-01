package application

import (
	"strings"
	"time"

	"masamong/internal/features/poll"
)

// Polls are posted as Discord's own polls, so people vote in Discord, not through the bot. Without these
// events the bot's record of a poll kept the votes it had at creation (none) and its results never
// matched what people saw (BACKLOG: 투표 상태가 동기화되지 않는다). The gateway links each posted poll
// message to its poll and passes every vote and unvote on; a deleted poll message closes the poll.
// Votes cast while the bot was offline are not replayed (Discord sends no backlog of them).
func init() {
	featureDDL = append(featureDDL, `CREATE TABLE IF NOT EXISTS poll_messages (
		message_id TEXT PRIMARY KEY,
		poll_id TEXT NOT NULL,
		channel_id TEXT NOT NULL,
		created_at TEXT NOT NULL
	)`, `CREATE INDEX IF NOT EXISTS idx_poll_messages_poll ON poll_messages(poll_id)`)
	pruneRules = append(pruneRules, pruneRule{name: "poll_messages", table: "poll_messages", column: "created_at", days: "usage"})
}

// LinkPollMessage records the Discord message a poll was posted as.
func (s *Service) LinkPollMessage(pollID, channelID, messageID string) {
	if pollID == "" || messageID == "" {
		return
	}
	db, err := s.adminDB()
	if err != nil {
		return
	}
	if _, err := db.DB.Exec(`INSERT OR IGNORE INTO poll_messages(message_id,poll_id,channel_id,created_at) VALUES(?,?,?,?)`,
		messageID, pollID, channelID, s.Clock.Now().UTC().Format(time.RFC3339)); err != nil {
		s.Log.Debug("poll link write failed", "err", err.Error())
	}
}

func (s *Service) pollOfMessage(messageID string) string {
	db, err := s.adminDB()
	if err != nil {
		return ""
	}
	var id string
	_ = db.DB.QueryRow(`SELECT poll_id FROM poll_messages WHERE message_id=?`, messageID).Scan(&id)
	return id
}

// PollVote applies a vote (add) or its removal on a Discord poll message. answerID is Discord's
// 1-based answer number, in the order the poll's options were posted.
func (s *Service) PollVote(messageID, userID string, answerID int, add bool) {
	id := s.pollOfMessage(messageID)
	if id == "" || userID == "" || answerID < 1 {
		return
	}
	_, err := s.updatePoll(id, func(p poll.Poll) (poll.Poll, error) {
		if answerID > len(p.Options) {
			return p, nil
		}
		opt := p.Options[answerID-1].ID
		var sel []string
		for _, o := range p.Votes[userID] {
			if o != opt {
				sel = append(sel, o)
			}
		}
		if add {
			sel = append(sel, opt)
			if len(sel) > p.MaxSelect {
				sel = sel[len(sel)-p.MaxSelect:] // Discord enforces the limit; keep the newest
			}
		}
		cmd := poll.VoteCmd{ScopeKey: p.ScopeKey, ExpectedRev: p.Revision, ParticipantKey: userID, OptionIDs: sel, Now: s.Clock.Now()}
		if len(sel) == 0 {
			return poll.Unvote(p, cmd)
		}
		return poll.Vote(p, cmd)
	})
	if err != nil && !strings.Contains(err.Error(), "closed") {
		s.Log.Debug("poll vote not applied", "err", err.Error())
	}
}

// pollMessagesDeleted closes the polls whose Discord message was deleted.
func (s *Service) pollMessagesDeleted(messageIDs []string) {
	for _, m := range messageIDs {
		id := s.pollOfMessage(m)
		if id == "" {
			continue
		}
		_, _ = s.updatePoll(id, func(p poll.Poll) (poll.Poll, error) {
			return poll.Close(p, poll.CloseCmd{ScopeKey: p.ScopeKey, ExpectedRev: p.Revision, Now: s.Clock.Now()})
		})
	}
}
