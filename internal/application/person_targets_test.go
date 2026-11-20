package application

import (
	"slices"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/memory/engine"
)

func TestPersonTargets(t *testing.T) {
	s := privacyService(t)
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
		{"어때?", "", []string{"민수"}, []string{"민수"}},
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
