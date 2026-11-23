package application

import (
	"reflect"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/memory/engine"
	"masamong/internal/memory/episodes"
	"masamong/internal/memory/store"
)

// A people fact becomes one claim per speaker, each with that speaker's own evidence lines and the text
// each line had when the model read it; a bot line or a stranger's line is never evidence, and self is
// the link only when the speaker is the subject.
func TestLiveClaimsSplitPerSpeaker(t *testing.T) {
	r := memRoom{guild: "g", channel: "c", room: "discord:g:c", source: "discord"}
	a, b := store.PersonRef("1"), store.PersonRef("2")
	at := time.Now()
	lines := []roomLine{
		{Message: episodes.Message{ID: "o1", Account: "1", Author: "철수", At: at, Text: "민수는 키가 커"}},
		{Message: episodes.Message{ID: "o2", Account: "2", Author: "민수", At: at, Text: "맞아 나 키 커"}},
		{Message: episodes.Message{ID: "bot:o3", Account: "bot", Author: "봇", At: at, Bot: true, Text: "민수는 키가 커요"}},
	}
	fact := episodes.PersonFact{About: b, Kind: "appearance", Topic: "키", Text: "{@" + b + "}는 키가 큼", By: []string{a, b}, Ev: []string{"o1", "o2"}, Link: episodes.LinkReply}
	got, dropped := liveClaims(r, "extract:o1", "2026-01-01T00:00:00Z", []episodes.PersonFact{fact}, lines)
	if dropped != 0 || len(got) != 2 {
		t.Fatalf("claims %+v dropped %d", got, dropped)
	}
	hearsay, own := got[0], got[1]
	if hearsay.SpeakerRef != a || hearsay.Link != store.LinkReply || !reflect.DeepEqual(hearsay.Evidence, []string{"o1"}) ||
		hearsay.Lines["o1"] != "민수는 키가 커" || hearsay.Lines["o2"] != "" {
		t.Errorf("hearsay claim: %+v", hearsay)
	}
	if own.SpeakerRef != b || own.Link != store.LinkSelf || !reflect.DeepEqual(own.Evidence, []string{"o2"}) || own.Lines["o2"] != "맞아 나 키 커" {
		t.Errorf("own claim: %+v", own)
	}
	for _, c := range got {
		if c.GuildID != "g" || c.Source != "extract:o1" || c.ReadAt == "" || c.Kind != "appearance" {
			t.Errorf("claim: %+v", c)
		}
	}
	// evidence only a bot line or a line of someone else carries: the speaker is dropped and counted
	for name, f := range map[string]episodes.PersonFact{
		"bot line":       {About: b, Kind: "trait", Topic: "x", Text: "{@" + b + "}", By: []string{a}, Ev: []string{"bot:o3"}},
		"another author": {About: b, Kind: "trait", Topic: "x", Text: "{@" + b + "}", By: []string{a}, Ev: []string{"o2"}},
	} {
		if c, dropped := liveClaims(r, "extract:o1", "", []episodes.PersonFact{f}, lines); len(c) != 0 || dropped != 1 {
			t.Errorf("%s: %+v dropped %d", name, c, dropped)
		}
	}
	// a hearsay link left as self by the parser can never reach the gate as self
	f := fact
	f.Link = episodes.LinkSelf
	if c, _ := liveClaims(r, "extract:o1", "", []episodes.PersonFact{f}, lines); c[0].Link != store.LinkMention {
		t.Errorf("hearsay link: %q", c[0].Link)
	}
}

// selfStatement: exactly one human line, addressed to the bot (a reply of the bot or a gateway-seen
// mention), with at least search.MinMessageRunes meaningful characters.
func TestSelfStatementRules(t *testing.T) {
	s := resolverService(t)
	r := memRoom{guild: "g", channel: "c", room: "discord:g:c", source: "discord"}
	line := func(id, text string, bot bool) roomLine {
		return roomLine{Message: episodes.Message{ID: id, Account: "1", Author: "철수", Text: text, Bot: bot}, native: "n" + id}
	}
	replyTo := func(id string) []episodes.Message { return []episodes.Message{{ID: "bot:x", Bot: true, ReplyTo: id}} }
	long := "나 광양에 살아 요즘"
	for name, tc := range map[string]struct {
		lines []roomLine
		bots  []episodes.Message
		want  bool
	}{
		"answered by the bot":     {[]roomLine{line("a", long, false)}, replyTo("a"), true},
		"answered a human line":   {[]roomLine{line("a", long, false)}, replyTo("zzz"), false},
		"nobody answered":         {[]roomLine{line("a", long, false)}, nil, false},
		"too short":               {[]roomLine{line("a", "응 그래", false)}, replyTo("a"), false},
		"mention is not a length": {[]roomLine{line("a", "<@123> 응 그래", false)}, replyTo("a"), false},
		"two human lines":         {[]roomLine{line("a", long, false), line("b", long, false)}, replyTo("a"), false},
		"no human line":           {[]roomLine{line("a", long, true)}, replyTo("a"), false},
	} {
		if got := s.selfStatement(r, tc.lines, tc.bots, ""); got != tc.want {
			t.Errorf("%s: got %v", name, got)
		}
	}
	// a mention of the bot in the line itself
	if !s.selfStatement(r, []roomLine{line("a", "<@999> 나 광양에 살아 요즘", false)}, nil, personRef("999")) {
		t.Error("a mention of the bot addresses it")
	}
	// the gateway saw the line addressed to the bot
	s.noteLive(ObservedMessage{GuildID: "g", ChannelID: "c", MessageID: "na", AuthorID: "1", Content: long, BotCall: "mention", At: time.Now()})
	if !s.selfStatement(r, []roomLine{line("a", long, false)}, nil, "") {
		t.Error("a line the gateway saw mentioning the bot is addressed")
	}
}

// The people a conversation names without writing in it: <@id> accounts (link mention), exact
// single-account alias matches of this server (link name), never an ambiguous name, the bot, an author of
// the lines or a withdrawn person; the backfill's helper gives the same list.
func TestMentionedPeopleHelper(t *testing.T) {
	s := resolverService(t)
	e, err := s.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range []struct{ id, name string }{{"101", "민수"}, {"202", "철수"}, {"303", "영희"}, {"808", "새봄"}, {"909", "새봄"}, {"404", "마사몽"}, {"505", "떠난이"}} {
		if ok, err := e.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", AuthorID: p.id, AuthorName: p.name, MessageID: p.id + "x",
			At: time.Now().Add(time.Duration(i) * time.Second), Content: "hello"}); err != nil || !ok {
			t.Fatal(ok, err)
		}
	}
	if r := s.Execute(t.Context(), action.Request{Action: "privacy.consent", Input: []byte(`{"scope":"memory","user_id":"505","decision":"withdraw"}`)}); !r.OK {
		t.Fatal(r.Error)
	}
	msgs := []episodes.Message{
		{ID: "m1", Account: "303", Author: "영희", Text: "<@101> 철수는 어때 새봄은 어떻고 <@404> <@505> <@303>"},
		{ID: "m2", Account: "404", Author: "마사몽", Bot: true, Text: "<@202>"},
	}
	bot := personRef("404")
	got, err := s.mentionedPeople(s.Store, "g", msgs, bot)
	if err != nil {
		t.Fatal(err)
	}
	want := []episodes.Person{
		{Ref: personRef("101"), Label: "민수", ID: "101", Link: episodes.LinkMention},
		{Ref: personRef("202"), Label: "철수", Link: episodes.LinkName},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mentioned: %+v, want %+v", got, want)
	}
	old, err := s.pbMentioned(s.Store, "g", msgs, bot)
	if err != nil || !reflect.DeepEqual(old, got) {
		t.Fatalf("the backfill's helper differs: %+v %v", old, err)
	}
}
