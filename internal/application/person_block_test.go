package application_test

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/config"
	"masamong/internal/memory/blocks"
	"masamong/internal/memory/corpus"
	"masamong/internal/memory/engine"
	"masamong/internal/memory/store"
)

type personGateway struct{ fakeGateway }

func (*personGateway) Channels(guild string) []application.ChannelInfo {
	return []application.ChannelInfo{{ID: "c", GuildID: guild, Type: "text"}, {ID: "other", GuildID: guild, Type: "text"}}
}
func (*personGateway) PersonMemoryChannels(string) map[string]bool {
	return map[string]bool{"c": true, "c2": true, "c3": true, "other": true, "private": false}
}

type reviewPerson struct {
	*application.Service
	model  *turnModel
	engine *engine.Engine
	at     time.Time
}

func TestReviewExactAccountMembership(t *testing.T) {
	s, _, _ := memorySvc(t)
	seedServerPerson(t, s, "old-member", "discord:g:c", "101", "Old member", "2026-01-01T00:00:00Z")
	for i := range 200 {
		id := "new-" + strconv.Itoa(i)
		seedServerPerson(t, s, id, "discord:g:c", id, "101 admirer", "2026-06-01T00:00:00Z")
	}
	r := exec(s, "people.servers", map[string]any{"user_id": "101"})
	var out peopleServersTestResponse
	raw, err := json.Marshal(r.Output)
	if err != nil || !r.OK || json.Unmarshal(raw, &out) != nil || len(out.Servers) != 1 || len(out.Servers[0].People) != 1 || out.Servers[0].People[0].UserID != "101" || out.Servers[0].GuildID != "g" {
		t.Fatalf("exact membership: %s, %v", raw, err)
	}
	if r := exec(s, "memory.mine.show", map[string]any{"guild_id": out.Servers[0].GuildID, "user_id": "101"}); !r.OK {
		t.Fatal(r.Error)
	}
}

func (f reviewPerson) source(t *testing.T, guild, room, id, account, label string, at time.Time) string {
	t.Helper()
	ok, err := f.engine.Capture(engine.LiveMessage{GuildID: guild, ChannelID: room, MessageID: id, AuthorID: account, AuthorName: label, At: at, Content: id + " evidence"})
	if !ok || err != nil {
		t.Fatalf("capture: %t %v", ok, err)
	}
	return corpus.DiscordLiveObservationID(engine.RoomKey(guild, room), id)
}

func (f reviewPerson) fact(t *testing.T, guild, channel, id, subject, speaker, ev string) {
	t.Helper()
	room := engine.RoomKey(guild, channel)
	raw, _ := json.Marshal([]string{ev})
	by, _ := json.Marshal([]string{store.PersonRef(speaker)})
	if _, err := f.Store.DB.Exec(`INSERT INTO channel_episode(id,guild_id,channel_id,room,kind,title,status,source,first_seen,last_seen,updated_at)
		VALUES(?,?,?,?,'event',?,'done','discord',?,?,?)`, id, guild, channel, room, id, f.at, f.at, f.at); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Store.DB.Exec(`INSERT INTO episode_item(id,episode_id,kind,text,evidence,by_refs,created_at) VALUES(?,?,'fact',?,?,?,?)`,
		id, id, "{@"+store.PersonRef(subject)+"} "+id, string(raw), string(by), f.at.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
}

func (f reviewPerson) ask(t *testing.T, text string) string {
	t.Helper()
	turn(t, f.Service, "303", "지수", text, func(in *application.ChatInput) { in.Locale = "ko" })
	return f.model.last("answer")
}
func (f reviewPerson) attach(t *testing.T, guild, channel string) {
	id := blocks.BlockID("discord", engine.RoomKey(guild, channel), f.engine.Settings.Recipe.Version(), f.engine.EmbedSpace())
	applyDoc(t, f.Service, func(d *config.Document) {
		c := d.Guilds["g"].Channels["c"]
		c.RAGView.Blocks = []string{id}
		d.Guilds["g"].Channels["c"] = c
	})
}

func TestReviewPersonPrivacy(t *testing.T) {
	tests := map[string]func(*testing.T, reviewPerson){
		"ForgetAll": func(t *testing.T, f reviewPerson) {
			by, _ := json.Marshal([]string{store.PersonRef("101")})
			if _, err := f.Store.DB.Exec(`WITH RECURSIVE nums(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM nums WHERE i<1000)
				INSERT INTO episode_item(id,episode_id,kind,text,evidence,by_refs,created_at)
				SELECT 'new-'||i,'public','fact',text,evidence,?,? FROM nums,episode_item WHERE id='public'`, string(by), f.at.Add(time.Hour).Format(time.RFC3339Nano)); err != nil {
				t.Fatal(err)
			}
			req := action.Request{Action: "memory.mine.forget"}
			do := func(token any) action.Result {
				req.Input, _ = json.Marshal(map[string]any{"guild_id": "g", "user_id": "101", "scope": "all", "confirm": token})
				return f.Execute(t.Context(), req)
			}
			p := do("")
			if !p.OK || p.Output["items"] != 1001 {
				t.Fatalf("preview: %+v", p)
			}
			if _, err := f.Store.DB.Exec(`UPDATE episode_item SET text=? WHERE id='public'`, "{@"+store.PersonRef("101")+"} changed oldest"); err != nil {
				t.Fatal(err)
			}
			stale := do(p.Output["token"])
			if !stale.OK || stale.Output["stale"] != true || stale.Output["applied"] != false {
				t.Fatalf("oldest item not fingerprinted: %+v", stale)
			}
			if r := do(stale.Output["token"]); !r.OK || r.Output["applied"] != true || r.Output["deleted"] != 1001 {
				t.Fatalf("apply: %+v", r)
			}
			if got := block(f.ask(t, "<@101> 어떤 사람이야?"), cardOpen, cardClose); got != "" {
				t.Fatal(got)
			}
			var n int
			if err := f.Store.Reader().QueryRow(`SELECT count(*) FROM episode_item,json_each(by_refs) WHERE value=?`, store.PersonRef("101")).Scan(&n); err != nil || n != 0 {
				t.Fatalf("surviving facts %d: %v", n, err)
			}
		},
		"DeletedAlias": func(t *testing.T, f reviewPerson) {
			for i := range 3 {
				f.source(t, "g", "c", "rename"+strconv.Itoa(i), "101", "삭제된별명", f.at.Add(time.Duration(i+1)*time.Minute))
			}
			check := func(want string, n int) {
				t.Helper()
				got := block(f.ask(t, "<@101> 어떤 사람이야?"), cardOpen, cardClose)
				if !strings.Contains(got, want+" public") {
					t.Fatal(got)
				}
				var rows int
				if err := f.Store.Reader().QueryRow(`SELECT count(*) FROM person_alias_source WHERE person_ref=? AND label='삭제된별명'`, store.PersonRef("101")).Scan(&rows); err != nil || rows != n {
					t.Fatalf("sources=%d want %d: %v", rows, n, err)
				}
			}
			if n, err := f.engine.Delete([]string{"rename2"}, time.Now()); err != nil || n != 1 {
				t.Fatalf("delete %d: %v", n, err)
			}
			check("삭제된별명", 2)
			if ok, err := f.engine.Edit("rename1", "edited words", time.Now()); err != nil || !ok {
				t.Fatalf("edit %t: %v", ok, err)
			}
			check("삭제된별명", 1)
			if n, err := f.engine.Delete([]string{"rename0"}, time.Now()); err != nil || n != 1 {
				t.Fatalf("delete %d: %v", n, err)
			}
			check("민수", 0)
			if got := block(f.ask(t, "삭제된별명 어떤 사람이야?"), cardOpen, cardClose); got != "" {
				t.Fatal(got)
			}
		},
		"BlockedTarget": func(t *testing.T, f reviewPerson) {
			applyDoc(t, f.Service, func(d *config.Document) { d.Access.BlockedUsers = []string{"101"} })
			if got := block(f.ask(t, "<@101> 어떤 사람이야?"), cardOpen, cardClose); got != "" {
				t.Fatal(got)
			}
		},
		"BlockedHearsayAuthor": func(t *testing.T, f reviewPerson) {
			ev := f.source(t, "g", "c", "hidden-source", "202", "영희", f.at)
			f.fact(t, "g", "c", "hidden-gossip", "101", "202", ev)
			// A malformed by_refs that omits the blocked evidence author must not leak either.
			f.fact(t, "g", "c", "misattributed", "101", "101", ev)
			applyDoc(t, f.Service, func(d *config.Document) { g := d.Guilds["g"]; g.BlockedUsers = []string{"202"}; d.Guilds["g"] = g })
			got := f.ask(t, "<@101> 어떤 사람이야?")
			if b := block(got, cardOpen, cardClose); !strings.Contains(b, "민수 public") || strings.Contains(b, "hidden") || strings.Contains(b, "misattributed") {
				t.Fatal(b)
			}
			if b := block(got, episodesOpen, episodesClose); strings.Contains(b, "hidden") || strings.Contains(b, "misattributed") {
				t.Fatal(b)
			}
		},
		"PrivateAlias": func(t *testing.T, f reviewPerson) {
			f.source(t, "g", "unclassified", "rename", "101", "비공개별명", f.at.Add(time.Hour))
			if got := block(f.ask(t, "비공개별명?"), cardOpen, cardClose); got != "" {
				t.Fatalf("resolved private alias: %s", got)
			}
			got := block(f.ask(t, "<@101> 어떤 사람이야?"), cardOpen, cardClose)
			if !strings.Contains(got, "민수 public") || strings.Contains(got, "비공개별명") {
				t.Fatal(f.model.last("answer"))
			}
		},
		"AttachedPrivateRoom": func(t *testing.T, f reviewPerson) {
			ev := f.source(t, "g", "unclassified", "restricted-marker", "101", "민수", f.at)
			f.fact(t, "g", "unclassified", "restricted", "101", "101", ev)
			f.attach(t, "g", "unclassified")
			got := block(f.ask(t, "<@101> 어떤 사람이야?"), cardOpen, cardClose)
			if !strings.Contains(got, "민수 public") || strings.Contains(got, "restricted") {
				t.Fatal(got)
			}
			if strings.Contains(f.model.last("answer"), "restricted-marker") {
				t.Fatal("person bonus used attached-only room")
			}
		},
		"CrossGuildBonus": func(t *testing.T, f reviewPerson) {
			ev := f.source(t, "g2", "c2", "restricted-marker", "101", "민수", f.at)
			f.fact(t, "g2", "c2", "butterfly", "101", "101", ev)
			f.attach(t, "g2", "c2")
			for _, q := range []string{"날씨?", "<@101> 어떤 사람이야?"} {
				if strings.Contains(f.ask(t, q), "restricted-marker") {
					t.Fatalf("cross-guild bonus for %q", q)
				}
			}
		},
		"LegacyAttachmentRanking": func(t *testing.T, f reviewPerson) {
			ev := f.source(t, "g2", "c2", "linked-marker", "101", "민수", f.at)
			f.fact(t, "g2", "c2", "butterfly", "101", "101", ev)
			f.attach(t, "g2", "c2")
			if !strings.Contains(f.ask(t, "butterfly 관련된 거 있어?"), "linked-marker") {
				t.Fatal("legacy lexical match no longer available")
			}
		},
	}
	for name, run := range tests {
		t.Run(name, func(t *testing.T) {
			s, m, e := memorySvc(t)
			f := reviewPerson{s, m, e, time.Now().UTC().Add(-3 * time.Hour)}
			ev := f.source(t, "g", "c", "public", "101", "민수", f.at)
			f.fact(t, "g", "c", "public", "101", "101", ev)
			run(t, f)
		})
	}
}

func TestEpisodePersonBlockScope(t *testing.T) {
	s, m, e := memorySvc(t)
	s.Discord = &personGateway{}
	applyDoc(t, s, func(d *config.Document) { d.Guilds["g"].Channels["other"] = config.ChannelPolicy{Capture: true} })
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
	if _, err := e.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "other", AuthorID: "101", AuthorName: "민수", MessageID: "other-label", Content: "hello", At: at}); err != nil {
		t.Fatal(err)
	}
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
	check(get("private", false), []string{"비밀"}, []string{"차를 좋아함", "기타", "바둑"})
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
