package application_test

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"masamong/internal/application"
	"masamong/internal/config"
	"masamong/internal/memory/corpus"
	"masamong/internal/memory/engine"
	"masamong/internal/memory/store"
)

type personGateway struct{ fakeGateway }

func (*personGateway) Channels(guild string) []application.ChannelInfo {
	return []application.ChannelInfo{{ID: "c", GuildID: guild, Type: "text"}, {ID: "other", GuildID: guild, Type: "text"}}
}

func TestEpisodePersonBlockScope(t *testing.T) {
	s, m, e := memorySvc(t)
	s.Discord = &personGateway{}
	s.NoteThread("g", "thread", "c", false)
	ref := store.PersonRef("101")
	at := time.Now().UTC().Add(-2 * time.Hour)
	var oldEvidence string
	put := func(room, author, label, id, fact string) {
		t.Helper()
		_, err := e.Capture(engine.LiveMessage{GuildID: "g", ChannelID: room, AuthorID: author, AuthorName: label, MessageID: id, Content: id + " evidence", At: at})
		if err != nil {
			t.Fatal(err)
		}
		key := engine.RoomKey("g", room)
		ev := corpus.DiscordLiveObservationID(key, id)
		evs, _ := json.Marshal([]string{ev})
		by, _ := json.Marshal([]string{store.PersonRef(author)})
		if _, err := s.Store.DB.Exec(`INSERT INTO channel_episode(id,guild_id,channel_id,room,kind,title,status,source,first_seen,last_seen,updated_at)
			VALUES(?,'g',?,?,'event',?,'done','discord',?,?,?)`, id, room, key, id, at, at, at); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Store.DB.Exec(`INSERT INTO episode_item(id,episode_id,kind,text,evidence,by_refs,created_at) VALUES(?,?,'fact',?,?,?,?)`,
			id, id, "{@"+ref+"}"+fact, string(evs), string(by), at.Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
		if id == "old" {
			oldEvidence = ev
		}
	}
	put("c", "101", "민수", "old", "는 차를 좋아함")
	put("thread", "202", "영희", "thread-note", "는 바둑을 둠")
	put("private", "101", "민수", "private-note", "는 비밀 모임에 감")
	put("other", "202", "영희", "other-note", "는 기타를 배움")
	for i := range 14 {
		if _, err := e.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", AuthorID: "404", AuthorName: "진우",
			MessageID: "filler" + strconv.Itoa(i), Content: "hello", At: at.Add(time.Hour + time.Duration(i)*time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		message string
		want    bool
	}{{"?", false}, {"<@101>?", true}} {
		res := turn(t, s, "303", "지수", tc.message)
		if (res.Room != nil && len(res.Room.Episodes) > 0) != tc.want {
			t.Fatalf("target bonus for %q: episodes %+v", tc.message, res.Room)
		}
	}
	get := func(room string, proactive bool) string {
		t.Helper()
		turn(t, s, "303", "지수", "민수는 어떤 사람?", func(in *application.ChatInput) { in.ChannelID, in.Proactive = room, proactive })
		return block(m.last("answer"), cardOpen, cardClose)
	}
	check := func(got string, present, absent []string) {
		t.Helper()
		for _, x := range present {
			if !strings.Contains(got, x) {
				t.Errorf("missing %q in %s", x, got)
			}
		}
		for _, x := range absent {
			if strings.Contains(got, x) {
				t.Errorf("unexpected %q in %s", x, got)
			}
		}
	}
	// Cards are off. Only explicit person targets get episode-derived notes,
	// including an unsolicited turn, and hearsay carries its speaker's name.
	b := get("c", true)
	check(b, []string{"민수는 차를 좋아함", "[영희] 민수는 바둑을 둠"}, []string{"기타", "비밀"})
	t.Log("fixture:\n" + b + cardClose)
	check(get("other", false), []string{"차를 좋아함", "기타"}, []string{"바둑", "비밀"})
	check(get("private", false), []string{"비밀", "차를 좋아함"}, []string{"기타", "바둑"})
	strict(t, s, config.GuildPrivacy{NoHearsay: true})
	check(get("c", false), []string{"차를 좋아함"}, []string{"바둑", "기타", "비밀"})
	strict(t, s, config.GuildPrivacy{CardChannelOnly: true})
	check(get("other", false), []string{"기타"}, []string{"차를 좋아함", "바둑", "비밀"})
	strict(t, s, config.GuildPrivacy{})
	// Even before a derived-item invalidation pass runs, an edit after the
	// item was saved makes its old wording unusable.
	if _, err := s.Store.DB.Exec(`UPDATE memory_observations SET edited_at=? WHERE observation_id=?`, time.Now().UTC().Format(time.RFC3339Nano), oldEvidence); err != nil {
		t.Fatal(err)
	}
	check(get("other", false), []string{"기타"}, []string{"차를 좋아함"})
	// Older extracts can identify a speaker but leave the subject as plain
	// text. A statement they made about somebody else is not a self fact.
	put("c", "101", "민수", "unattributed", "")
	if _, err := s.Store.DB.Exec(`UPDATE episode_item SET text='지영이가 춤을 배운대' WHERE id='unattributed'`); err != nil {
		t.Fatal(err)
	}
	check(get("c", false), []string{"지영이가 춤을 배운대"}, nil)
	check(get("other", false), []string{"기타"}, []string{"지영이가"})
}
