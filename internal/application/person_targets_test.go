package application

import (
	"slices"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/config"
	"masamong/internal/memory/engine"
)

type resolverGateway struct{ DiscordInfo }

func (resolverGateway) BotUser() (string, string) { return "bot", "bot" }
func (resolverGateway) PersonMemoryChannels(string) map[string]bool {
	return map[string]bool{"c": true}
}

func resolverService(t *testing.T) *Service {
	s := privacyService(t)
	d := s.Doc()
	d.Guilds = map[string]config.GuildSettings{"g": {Channels: map[string]config.ChannelPolicy{"c": {Capture: true}}}}
	s.setDoc(d, s.Revision())
	s.Discord = resolverGateway{}
	return s
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
