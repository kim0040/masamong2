package application

import (
	"time"

	"masamong/internal/memory/store"
)

// HoldRedactionForTest keeps the redaction worker from starting, or lets it go again.
func (s *Service) HoldRedactionForTest(hold bool) { s.redaction.running.Store(hold) }

// QueueEditForTest commits an edit's intent, without the foreground apply MessageEdited does after it: a
// worker that has not reached the job yet.
func (s *Service) QueueEditForTest(guildID, channelID, messageID, content string, at time.Time) error {
	return s.queueSourceChange(guildID, channelID, []string{messageID}, "edit", content, at)
}

// SettledSourcesForTest lists the sources whose edits are finished.
func (s *Service) SettledSourcesForTest() []string {
	rows, err := s.Store.DB.Query(`SELECT source_id FROM privacy_settled`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			out = append(out, id)
		}
	}
	return out
}

// KeepLateReplyForTest writes a reply holding these sources, as a turn starting now would, and reports whether
// it was kept.
func (s *Service) KeepLateReplyForTest(guildID, channelID, native, speaker string, sources []string) bool {
	eng, err := s.MemoryEngine()
	if err != nil {
		return false
	}
	key := "discord:" + guildID + ":" + channelID
	if err := eng.Store.AppendChatTurns(key, []store.ChatTurn{{Role: "assistant", Content: "late", NativeID: native, SpeakerID: speaker, SourceIDs: sources, ReadAsOf: store.SettledMark(s.Store)}}, 40); err != nil {
		return false
	}
	var n int
	if s.Store.DB.QueryRow(`SELECT COUNT(*) FROM chat_turns WHERE channel_key=? AND native_id=? AND role='assistant'`, key, native).Scan(&n) != nil {
		return false
	}
	return n > 0
}
