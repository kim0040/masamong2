package application

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"masamong/internal/conversation"
	"masamong/internal/memory/store"
)

func TestLegacyPostLookupUsesRoomIndex(t *testing.T) {
	t.Parallel()
	s := privacyService(t)
	db, err := s.adminDB()
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.Reader().Query(`EXPLAIN QUERY PLAN SELECT r.message_id FROM reply_messages r
		LEFT JOIN turn_log t ON r.request_id IS NULL AND t.id=r.turn_id
		WHERE r.guild_id=? AND r.channel_id=? AND `+postedRequestSQL(true)+` IN (?)`, "g", "c", "request")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan string
	for rows.Next() {
		var id, parent, aux int
		var detail string
		if err := rows.Scan(&id, &parent, &aux, &detail); err != nil {
			t.Fatal(err)
		}
		plan += detail + "\n"
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "SEARCH r USING INDEX idx_reply_messages_channel") {
		t.Fatalf("legacy lookup is not restricted to the room:\n%s", plan)
	}
}

func TestLegacyAnswerClockWithoutLivePost(t *testing.T) {
	t.Parallel()
	s := privacyService(t)
	db, err := s.adminDB()
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2025, 1, 2, 1, 0, 0, 0, time.UTC)
	flake := fmt.Sprint((at.UnixMilli() - 1420070400000) << 22)
	for _, tc := range []struct {
		id  string
		reg time.Time
	}{{flake, at.Add(time.Hour)}, {"non-snowflake", at}} {
		if _, err := db.DB.Exec(`INSERT INTO reply_messages(message_id,turn_id,guild_id,channel_id,request_id,at) VALUES(?,'turn','g','c',?,?)`,
			tc.id, tc.id+"-request", tc.reg.Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
		turns := []store.ChatTurn{{NativeID: tc.id + "-request", Role: "user", CreatedAt: at.Add(time.Hour).Format(time.RFC3339Nano)},
			{NativeID: tc.id + "-request", Role: "assistant", CreatedAt: at.Add(time.Hour).Format(time.RFC3339Nano)}}
		if err := s.timeChatTurns(ChatInput{GuildID: "g", ChannelID: "c"}, turns); err != nil {
			t.Fatal(err)
		}
		if turns[1].SpokenAt != at.Format(time.RFC3339Nano) {
			t.Fatalf("answer used commit time: %s", turns[1].SpokenAt)
		}
		turns[0].SpokenAt = turns[1].SpokenAt
		if err := s.timeChatTurns(ChatInput{}, turns); err != nil || turns[1].SpokenAt != at.Add(time.Millisecond).Format(time.RFC3339Nano) {
			t.Fatalf("tied answer wasn't ordered after request: %s, %v", turns[1].SpokenAt, err)
		}
	}
}

func TestPostedLookupFailureIsCounted(t *testing.T) {
	t.Parallel()
	s := privacyService(t)
	s.noteLive(ObservedMessage{GuildID: "g", ChannelID: "c", MessageID: "unverified", FromBot: true, Content: "post"})
	eng, err := s.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	db, err := s.adminDB()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`ALTER TABLE reply_messages RENAME TO unavailable_reply_messages`); err != nil {
		t.Fatal(err)
	}
	lines, _, stats := s.channelLines(ChatInput{GuildID: "g", ChannelID: "c"}, eng)
	ledger := map[string]any{}
	stats.note(ledger, conversation.Attribution{})
	if ledger["posted_lookup_errors"] != 1 || len(lines) != 0 {
		t.Fatal("missing body-free lookup failure count or unverified post was replayed")
	}
}

func TestRegistryGapsLeaveWithLivePosts(t *testing.T) {
	t.Parallel()
	s := privacyService(t)
	eng, err := s.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	now := s.Clock.Now()
	for i := 0; i <= liveMaxLines; i++ {
		id := fmt.Sprintf("failed-%d", i)
		m := ObservedMessage{GuildID: "g", ChannelID: "c", MessageID: id, FromBot: true, Content: "unverified", At: now}
		if i%2 == 0 {
			s.noteLive(m)
		}
		s.rejectDelivery("g", "c", "", []string{id})
		s.noteLive(m) // a first observation after REST, or a duplicate gateway event
	}
	s.noteLive(ObservedMessage{GuildID: "g", ChannelID: "c", MessageID: "notice", FromBot: true, Content: "notice", At: now})
	lines, _, _ := s.channelLines(ChatInput{GuildID: "g", ChannelID: "c"}, eng)
	if len(lines) != 1 || lines[0].ID != "notice" {
		t.Fatalf("registry failures hid an unrelated notice or exposed an unverified post: %d lines", len(lines))
	}
	if len(s.live.registryGaps) != 0 {
		t.Fatal("observed gaps outlived their lines")
	}
	s.rejectDelivery("g", "other", "", []string{"never-observed"})
	s.sweepLive(now.Add(liveIdleEvict + time.Hour))
	if len(s.live.registryGaps) != 0 {
		t.Fatal("unobserved gap did not expire")
	}
}

func TestLiveEditRecomputesToBot(t *testing.T) {
	t.Parallel()
	s := privacyService(t)
	m := ObservedMessage{GuildID: "g", ChannelID: "c", MessageID: "request", AuthorID: "person",
		Content: "addressed", BotCall: "mention", Mentions: []string{"bot"}, At: time.Now()}
	s.noteLive(m)
	m.Content, m.BotCall, m.Mentions = "ordinary", "", nil
	s.noteLive(m)
	if s.liveLines("g", "c")[0].ToBot {
		t.Fatal("replacement inherited the old mention")
	}
	m.Content, m.BotCall, m.Mentions = "addressed again", "mention", []string{"bot"}
	s.noteLive(m)
	if err := s.MessageEdited("g", "c", "request", "ordinary again", time.Now()); err != nil {
		t.Fatal(err)
	}
	if l := s.liveLines("g", "c")[0]; l.ToBot || len(l.Mentions) != 0 {
		t.Fatal("Discord edit kept the old mention")
	}
	call := ChatInput{GuildID: "g", ChannelID: "c", MessageID: "request", clockStart: m.At}
	s.liveToBot(call)
	if s.liveLines("g", "c")[0].ToBot {
		t.Fatal("completion of the pre-edit turn re-marked the edited line")
	}
	call.clockStart = time.Now()
	s.liveToBot(call)
	if !s.liveLines("g", "c")[0].ToBot {
		t.Fatal("a turn accepting the edited revision could not mark its own call")
	}
	m.MessageID, m.ReplyTo, m.ReplyAuthorID = "reply", "prior-post", "bot"
	s.noteLive(m)
	s.liveEdit("g", "c", "reply", "edited reply")
	if !s.liveLines("g", "c")[1].ToBot {
		t.Fatal("editing the words removed the reply target")
	}
}

func TestTurnSetupFailureDoesNotMarkLiveCall(t *testing.T) {
	t.Parallel()
	s, err := Open(t.TempDir(), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.noteLive(ObservedMessage{GuildID: "g", ChannelID: "c", MessageID: "request", Content: "ordinary"})
	in := ChatInput{GuildID: "g", ChannelID: "c", MessageID: "request", Message: "ordinary"}
	if _, err := s.chatTurn(t.Context(), in); err == nil || err.Error() != "run setup first" {
		t.Fatalf("expected memory initialization failure, got %v", err)
	}
	if s.liveLines("g", "c")[0].ToBot {
		t.Fatal("failed setup marked the message as an accepted call")
	}
}

func TestEditedAddressUsesOnlyCurrentRevision(t *testing.T) {
	t.Parallel()
	s := privacyService(t)
	m := ObservedMessage{GuildID: "g", ChannelID: "c", MessageID: "request", AuthorID: "person", Content: "DIRECT", BotCall: "mention"}
	s.noteLive(m)
	m.Content, m.BotCall = "REFERENCE "+s.Doc().BotNames()[0], "name"
	s.noteLive(m)
	if s.liveLines("g", "c")[0].ToBot {
		t.Fatal("replacement inherited the earlier call's decision")
	}
	for _, tc := range []struct {
		text     string
		others   bool
		mentions int
	}{{"<@&42> <@!7>", true, 1}, {"REFERENCE", false, 0}, {"@everyone", true, 0}, {"<@8>", false, 1}, {"@here", true, 0}} {
		s.liveEdit("g", "c", "request", tc.text)
		line := s.liveLines("g", "c")[0]
		if line.MentionsOthers != tc.others || len(line.Mentions) != tc.mentions || line.ToBot {
			t.Fatalf("incorrect edited address for %q", tc.text)
		}
	}
}
