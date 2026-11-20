package application

import (
	"database/sql"
	"fmt"
	"slices"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/config"
	"masamong/internal/memory/engine"
	"masamong/internal/memory/store"
)

type resolverGateway struct{ DiscordInfo }

func (resolverGateway) BotUser() (string, string) { return "bot", "bot" }
func (resolverGateway) PersonMemoryChannels(string) map[string]bool {
	return map[string]bool{"c": true}
}

func resolverService(t testing.TB) *Service {
	t.Helper()
	s, err := Open(t.TempDir(), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if r := s.Execute(t.Context(), action.Request{Action: "setup.resume", Input: []byte(`{"instance":"resolver"}`)}); !r.OK {
		t.Fatal(r.Error)
	}
	d := s.Doc()
	d.Guilds = map[string]config.GuildSettings{"g": {Channels: map[string]config.ChannelPolicy{"c": {Capture: true}}}}
	s.setDoc(d, s.Revision())
	s.Discord = resolverGateway{}
	return s
}

func loadCostGuild(t testing.TB, n int) *Service {
	t.Helper()
	s := resolverService(t)
	d := s.Doc()
	c := d.Guilds["g"].Channels["c"]
	c.RAGView.Blocks = []string{"attached-block"}
	d.Guilds["g"].Channels["c"] = c
	s.setDoc(d, s.Revision())
	e, err := s.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	for lo := 0; lo < n; lo += 256 {
		if err := s.Store.WithWriter(func(tx *sql.Tx) error {
			_, err := tx.Exec(`WITH RECURSIVE ids(i) AS (VALUES(?) UNION ALL SELECT i+1 FROM ids WHERE i<?)
				INSERT INTO memory_observations(observation_id,platform,collection,source_room,room,native_message_id,actor_label,actor_account,content,parsed_time,record_kind,imported_at)
				SELECT 'o'||i,'discord','discord','discord:g:c','discord:g:c','m'||i,'user-'||i,'u'||i,'fixture','2026-10-01T00:00:00Z','human_text','2026-10-01T00:00:00Z' FROM ids`, lo, min(lo+255, n-1))
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Store.DB.Exec(`UPDATE person_alias_fill SET cursor=0`); err != nil {
		t.Fatal(err)
	}
	if err := store.FillPersonAliases(t.Context(), s.Store); err != nil {
		t.Fatal(err)
	}
	if ok, err := e.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", MessageID: "target", AuthorID: "101", AuthorName: "민수", Content: "hello", At: time.Now()}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	return s
}

func TestReviewResolverCost(t *testing.T) {
	s := loadCostGuild(t, 10000)
	input := ChatInput{GuildID: "g", ChannelID: "c", SpeakerID: "303", Message: "오늘 날씨 알려줘"}
	start := time.Now()
	for range 100 {
		if got := s.targetPeople(s.Store, input, nil); len(got) != 0 {
			t.Fatal(got)
		}
	}
	t.Logf("10,000 aliases, mean of 100 no-target resolutions: %v", time.Since(start)/100)
	input.Message = "user-9876는?"
	if got := s.targetPeople(s.Store, input, nil); len(got) != 1 || got[0].ref != personRef("u9876") {
		t.Fatal(got)
	}
}

func TestAliasFillRunsAfterStartup(t *testing.T) {
	s := resolverService(t)
	e, err := s.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := e.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", MessageID: "legacy", AuthorID: "101", AuthorName: "민수", Content: "fixture", At: time.Now()}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if _, err := s.Store.DB.Exec(`DELETE FROM person_alias_source; UPDATE person_alias_fill SET cursor=0;
		CREATE TRIGGER fill_fault BEFORE INSERT ON person_alias BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	root := s.DataRoot
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	next := func() *Service {
		t.Helper()
		n, err := Open(root, nil, false)
		if err != nil {
			t.Fatalf("optional fill blocked startup: %v", err)
		}
		return n
	}
	s = next()
	if _, err := s.Store.DB.Exec(`DROP TRIGGER fill_fault`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = next()
	defer s.Close()
	timeout := time.After(5 * time.Second)
	for {
		var ready bool
		if err := s.Store.Reader().QueryRow(`SELECT (SELECT cursor=-1 FROM person_alias_fill) AND EXISTS(SELECT 1 FROM person_alias WHERE person_ref=?)`, personRef("101")).Scan(&ready); err != nil {
			t.Fatal(err)
		}
		if ready {
			return
		}
		select {
		case <-timeout:
			t.Fatal("startup worker did not fill persisted alias")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func BenchmarkPersonResolver(b *testing.B) {
	s := loadCostGuild(b, 10000)
	for _, busy := range []bool{false, true} {
		for _, named := range []bool{false, true} {
			b.Run(fmt.Sprintf("named=%t/busy=%t", named, busy), func(b *testing.B) {
				input := ChatInput{GuildID: "g", ChannelID: "c", SpeakerID: "303", Message: "오늘 날씨 알려줘"}
				if named {
					input.Message = "민수는 어떤 사람이야?"
				}
				if busy {
					stop, done := make(chan struct{}), make(chan struct{})
					go func() {
						defer close(done)
						for {
							select {
							case <-stop:
								return
							default:
							}
							if _, err := s.Store.DB.Exec(`UPDATE person_alias_fill SET cursor=-1`); err != nil {
								return
							}
							var n int
							_ = s.Store.Reader().QueryRow(`SELECT count(*) FROM person_alias`).Scan(&n)
						}
					}()
					defer func() { close(stop); <-done }()
				}
				b.ReportAllocs()
				for b.Loop() {
					got := s.targetPeople(s.Store, input, nil)
					if named && (len(got) != 1 || got[0].ref != personRef("101")) || !named && len(got) != 0 {
						b.Fatal(got)
					}
				}
			})
		}
	}
}

func TestReviewResolverIntent(t *testing.T) {
	s := resolverService(t)
	e, err := s.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range []struct{ id, name string }{{"101", "사과"}, {"202", "민수"}, {"303", "영희"}, {"404", "쇼핑"}, {"505", " 김영수 "}, {"606", "김영수"}, {"606", "뉴이름"}, {"707", "수"}, {"808", "새봄"}, {"909", "새봄"}} {
		ok, err := e.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", AuthorID: p.id, AuthorName: p.name, MessageID: p.id + p.name, At: time.Now().Add(time.Duration(i) * time.Second), Content: "hello"})
		if err != nil || !ok {
			t.Fatalf("seed: %v %v", ok, err)
		}
	}
	d := s.Doc()
	d.Access.BlockedUsers = []string{"606"}
	s.setDoc(d, s.Revision())
	for _, tc := range []struct {
		msg, speaker string
		want, plan   []string
	}{
		{"사과를 먹을까?", "303", []string{"101"}, nil}, // bounded common nouns are indistinguishable
		{"사과를 먹을까?", "101", nil, nil},
		{"풋사과가 제철이야?", "303", nil, nil},
		{"민수123은? A민수는? 민수Z?", "303", nil, nil},
		{"김영수는? 수는?", "303", nil, nil},
		{"나는 뭘 좋아해?", "303", []string{"303"}, nil},
		{"날 기억해?", "303", []string{"303"}, nil},
		{"what do I like?", "303", []string{"303"}, nil},
		{"영희야, 민수는 어떤 사람이야?", "303", []string{"202"}, nil},
		{"민수는 어떤 사람이야?", "101", []string{"202"}, []string{"영희", "쇼핑"}},
		{"<@909> <@808> 민수", "101", []string{"909", "808"}, nil},
	} {
		got := s.targetPeople(s.Store, ChatInput{GuildID: "g", ChannelID: "c", Message: tc.msg, SpeakerID: tc.speaker}, tc.plan)
		var refs []string
		for _, p := range got {
			refs = append(refs, p.ref)
		}
		var want []string
		for _, id := range tc.want {
			want = append(want, personRef(id))
		}
		if !slices.Equal(refs, want) {
			t.Errorf("%q: refs %v, want %v", tc.msg, refs, want)
		}
		if len(got) == 2 {
			labels := map[string]string{got[0].ref: got[0].label, got[1].ref: got[1].label}
			distinguishNames(got, labels)
			if labels[got[0].ref] == labels[got[1].ref] {
				t.Fatal("indistinguishable headers")
			}
		}
	}
}

func TestPersonTargets(t *testing.T) {
	s := resolverService(t)
	e, err := s.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []struct{ id, label string }{{"1", "민수"}, {"2", "영희"}, {"3", "지수"}, {"4", "철수"}, {"5", "수빈"}, {"6", "수빈"}, {"7", "로봇"}, {"8", "떠난이"}, {"9", "비공개"}, {"10", "미라"}} {
		_, err := e.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", AuthorID: p.id, AuthorName: p.label, MessageID: p.id, At: time.Now(), Content: "hello", Bot: p.id == "7"})
		if err != nil {
			t.Fatal(err)
		}
	}
	if r := s.Execute(t.Context(), action.Request{Action: "privacy.consent", Input: []byte(`{"scope":"profile","user_id":"9","decision":"withdraw"}`)}); !r.OK {
		t.Fatal(r.Error)
	}
	if r := s.Execute(t.Context(), action.Request{Action: "privacy.consent", Input: []byte(`{"scope":"memory","user_id":"10","decision":"withdraw"}`)}); !r.OK {
		t.Fatal(r.Error)
	}
	for _, id := range []string{"3", "4"} {
		s.noteLive(ObservedMessage{GuildID: "g", ChannelID: "c", MessageID: id, AuthorID: id, At: time.Now(), Content: "recent speaker"})
	}
	s.noteLive(ObservedMessage{GuildID: "g", ChannelID: "c", MessageID: "reply", ReplyTo: "2", AuthorID: "4", At: time.Now()})
	uncaptured := ObservedMessage{GuildID: "g", ChannelID: "c", MessageID: "reply-old", ReplyTo: "uncaptured", ReplyAuthorID: "1", AuthorID: "4", At: time.Now()}
	s.noteLive(uncaptured)
	uncaptured.ReplyAuthorID, uncaptured.ReplyTo = "", "" // a partial update has no reference
	s.noteLive(uncaptured)
	for _, tc := range []struct {
		text, messageID string
		planner, want   []string
	}{
		{"민수는 어떤 사람?", "", nil, []string{"민수"}},
		{"민수형은? 영희누나는?", "", nil, []string{"민수", "영희"}},
		{"민수님은? 영희씨는?", "", nil, []string{"민수", "영희"}},
		{"<@2> <@!1>", "", nil, []string{"영희", "민수"}},
		{"수빈이는 누구야?", "", nil, nil},
		{"<@7> 로봇 <@9> 비공개 <@10> 미라", "", nil, nil},
		{"떠난이는?", "", nil, []string{"떠난이"}},
		{"어때?", "reply", nil, []string{"영희"}},
		{"어때?", "reply-old", nil, []string{"민수"}},
		{"어때?", "", []string{"민수"}, nil},
		{"내가 민수가 궁금해", "", nil, []string{"민수"}},
		{"나에 대해 뭐 알아?", "", nil, []string{"철수"}},
		{"날씨 어때?", "", nil, nil},
	} {
		in := ChatInput{GuildID: "g", ChannelID: "c", SpeakerID: "4", Message: tc.text, MessageID: tc.messageID}
		var got []string
		for _, p := range s.targetPeople(s.Store, in, tc.planner) {
			got = append(got, p.label)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%q (%v): got %v, want %v", tc.text, tc.planner, got, tc.want)
		}
	}
}

func TestSelfIntent(t *testing.T) {
	for _, msg := range []string{
		"날 기억해?", "나를 기억하니", "저를 기억하세요?", "<@1> 날 기억하고 있어?", "나 기억해?", "저 기억하시나요?",
		"나에 대해 뭐 알아?", "저에 대해서 알려줘", "내 정보 보여줘", "제 정보",
		"내가 누구야?", "난 누구게?", "내가 누구인지 알아?", "나는 어떤 사람이야?", "저는 어떤 사람인가요?",
		"나는 뭘 좋아해?", "내가 뭘 좋아하지?", "나 뭐 좋아해?", "내 취향 알아?", "내 별명이 뭐야?", "나한테 어울리는 거 추천해줘",
		"나 소개해줘", "날 그려줘!", "날 기억해 ㅋㅋ",
		"what do I like?", "Do you remember me?", "who am I", "what do you know about me", "Tell me about myself", "describe me",
		"what are my hobbies?", "¿Quién soy?", "qué me gusta", "私のこと覚えてる？", "私はどんな人？", "我是谁", "你还记得我吗",
	} {
		if !selfIntent(msg) {
			t.Errorf("%q: no self intent", msg)
		}
	}
	for _, msg := range []string{
		"나는 기억력이 좋아. 오늘 서울 날씨 알려줘.", "제가 기억하기로는 민수가 먼저 왔어", "내가 민수가 궁금해", "내가 좋아하는 노래 틀어줘",
		"내가 누구랑 갔는지 알아?", "나는 기억해, 그날 일", "그 날 기억해?", "그 절 기억해?", "날씨 어때?", "내 취미는 등산이야",
		"나 소개해 줄 사람 있어?", "나한테 어울리는 옷이 없어", "don't worry about me", "remember me to your mom", "lo que me gusta es el té",
		"私について来て", "你知道我昨天去哪了吗", "오늘날 기억할 만한 일",
	} {
		if selfIntent(msg) {
			t.Errorf("%q: incidental first person counted as self intent", msg)
		}
	}
}
