package application

import (
	"testing"

	"masamong/internal/config"
	"masamong/internal/lang"
)

// A thread without an entry of its own follows its parent channel's room_style setting (channelPolicy), so
// it uses the parent's approved note too. A thread with its own entry keeps its own.
func TestThreadUsesItsParentRoomStyleNote(t *testing.T) {
	s := jobTestSvc(t)
	doc := s.Doc()
	doc.Guilds = map[string]config.GuildSettings{"g": {Channels: map[string]config.ChannelPolicy{
		"c":     {Respond: "mention", Conversation: config.ChannelConversation{RoomStyle: "on"}},
		"own-t": {Respond: "mention", Conversation: config.ChannelConversation{RoomStyle: "on"}},
	}}}
	s.SetDocForTest(doc)
	db, err := s.adminDB()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`INSERT INTO channel_style(guild_id,channel_id,version,state,note,created_at) VALUES('g','c',1,?,?,?)`,
		styleApproved, "parent-room-note", "2026-11-22T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	s.NoteThread("g", "t", "c", false)
	s.NoteThread("g", "own-t", "c", false)
	pack := lang.For("ko")
	rules := func(channel string) string {
		return s.roomStyleRules("g", channel, s.ChannelPolicyFor("g", channel), pack)
	}
	if got := rules("c"); got == "" {
		t.Fatal("precondition: the parent channel uses its note")
	}
	if got, want := rules("t"), rules("c"); got != want {
		t.Fatalf("a thread of the room must use the room's note: got %q, want %q", got, want)
	}
	if got := rules("own-t"); got != "" {
		t.Fatalf("a thread with its own entry has no note of its own: %q", got)
	}
}
