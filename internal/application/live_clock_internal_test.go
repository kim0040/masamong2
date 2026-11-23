package application

import (
	"testing"
	"time"

	"masamong/internal/clock"
)

// The turn's edit-ordering stamp is on the service clock (ChatTurn writes clockStart; the wall
// started keeps the turn's timings). An edit stamped after the turn began stays unmarked even when
// the service clock runs an hour behind the wall's, and a turn that began after the edit still
// marks its call.
func TestLiveEditOrderingFollowsTheServiceClock(t *testing.T) {
	s := privacyService(t)
	base := time.Now().Add(-time.Hour).UTC()
	s.Clock = clock.Fixed{T: base}
	s.noteLive(ObservedMessage{GuildID: "g", ChannelID: "c", MessageID: "m", AuthorID: "p",
		Content: "마사몽 질문", BotCall: "mention", Mentions: []string{"bot"}, At: base})
	in := ChatInput{GuildID: "g", ChannelID: "c", MessageID: "m", started: time.Now(), clockStart: base}
	s.Clock = clock.Fixed{T: base.Add(time.Minute)}
	s.liveEdit("g", "c", "m", "수정됨") // a service-minute after the turn began
	s.liveToBot(in)
	if s.liveLines("g", "c")[0].ToBot {
		t.Fatal("an edit that came after the turn started kept the call mark")
	}
	in.clockStart = base.Add(2 * time.Minute) // a later turn: the edit is already past
	s.liveToBot(in)
	if !s.liveLines("g", "c")[0].ToBot {
		t.Fatal("a turn that started after the edit lost the call mark")
	}
}
