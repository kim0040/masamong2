package application

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/memory/engine"
	"masamong/internal/memory/episodes"
	"masamong/internal/memory/store"
)

// A people fact is one speaker's statement or nothing: every line it cites was written by its one
// speaker. A fact cited from two people's lines is skipped and counted, never split into one claim per
// speaker; a bot line or a stranger's line is never evidence; self is the link only when the speaker is
// the subject, and a self claim names nobody but its subject.
func TestLiveClaimsNeverSplitSpeakers(t *testing.T) {
	r := memRoom{guild: "g", channel: "c", room: "discord:g:c", source: "discord"}
	a, b, c := store.PersonRef("1"), store.PersonRef("2"), store.PersonRef("3")
	at := time.Now()
	lines := []roomLine{
		{Message: episodes.Message{ID: "o1", Account: "1", Author: "철수", At: at, Text: "민수는 키가 커"}},
		{Message: episodes.Message{ID: "o2", Account: "2", Author: "민수", At: at, Text: "맞아 나 키 커"}},
		{Message: episodes.Message{ID: "bot:o3", Account: "bot", Author: "봇", At: at, Bot: true, Text: "민수는 키가 커요"}},
		{Message: episodes.Message{ID: "o4", Account: "2", Author: "민수", At: at, Text: "나 농구도 해"}},
	}
	// the fact both cited: skipped, no half is written
	mixed := episodes.PersonFact{About: b, Kind: "appearance", Topic: "키", Text: "{@" + b + "}는 키가 큼", By: []string{a, b}, Ev: []string{"o1", "o2"}, Link: episodes.LinkSelf}
	if got, skipped := liveClaims(r, "extract:o1", "2026-01-01T00:00:00Z", []episodes.PersonFact{mixed}, lines, nil); len(got) != 0 || skipped != 1 {
		t.Fatalf("a fact cited from two people's lines: claims %+v skipped %d", got, skipped)
	}
	// one speaker's own lines: hearsay of a, and b's own statement on two of b's lines
	hearsay := episodes.PersonFact{About: b, Kind: "appearance", Topic: "키", Text: "{@" + b + "}는 키가 큼", By: []string{a}, Ev: []string{"o1"}, Link: episodes.LinkReply}
	own := episodes.PersonFact{About: b, Kind: "like", Topic: "운동", Text: "{@" + b + "}는 농구를 함", By: []string{b}, Ev: []string{"o2", "o4"}, Link: episodes.LinkSelf}
	got, skipped := liveClaims(r, "extract:o1", "2026-01-01T00:00:00Z", []episodes.PersonFact{hearsay, own}, lines, nil)
	if skipped != 0 || len(got) != 2 {
		t.Fatalf("claims %+v skipped %d", got, skipped)
	}
	if h := got[0]; h.SpeakerRef != a || h.Link != store.LinkReply || !reflect.DeepEqual(h.Evidence, []string{"o1"}) || h.Lines["o1"] != "민수는 키가 커" || len(h.Lines) != 1 {
		t.Errorf("hearsay claim: %+v", h)
	}
	if o := got[1]; o.SpeakerRef != b || o.Link != store.LinkSelf || !reflect.DeepEqual(o.Evidence, []string{"o2", "o4"}) || o.Lines["o4"] != "나 농구도 해" {
		t.Errorf("own claim: %+v", o)
	}
	for _, c := range got {
		if c.GuildID != "g" || c.Source != "extract:o1" || c.ReadAt == "" {
			t.Errorf("claim: %+v", c)
		}
	}
	for name, f := range map[string]episodes.PersonFact{
		"bot line":                 {About: b, Kind: "trait", Topic: "x", Text: "{@" + b + "}", By: []string{a}, Ev: []string{"bot:o3"}},
		"another author":           {About: b, Kind: "trait", Topic: "x", Text: "{@" + b + "}", By: []string{a}, Ev: []string{"o2"}},
		"one line of another":      {About: b, Kind: "trait", Topic: "x", Text: "{@" + b + "}", By: []string{b}, Ev: []string{"o2", "o1"}},
		"no speaker":               {About: b, Kind: "trait", Topic: "x", Text: "{@" + b + "}", Ev: []string{"o2"}},
		"self naming someone else": {About: b, Kind: "relation", Topic: "x", Text: "{@" + b + "}는 {@" + c + "}와 친함", By: []string{b}, Ev: []string{"o2"}},
	} {
		if got, skipped := liveClaims(r, "extract:o1", "", []episodes.PersonFact{f}, lines, nil); len(got) != 0 || skipped != 1 {
			t.Errorf("%s: %+v skipped %d", name, got, skipped)
		}
	}
	// hearsay may name a third person; a hearsay link left as self by the parser never reaches the gate as self
	f := hearsay
	f.Link, f.Text = episodes.LinkSelf, "{@"+b+"}는 {@"+c+"}와 친함"
	if got, _ := liveClaims(r, "extract:o1", "", []episodes.PersonFact{f}, lines, nil); len(got) != 1 || got[0].Link != store.LinkMention {
		t.Errorf("hearsay: %+v", got)
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

// liveNames: a name of exactly one person of the prompt other than the subject, written as a whole word
// or with a particle, becomes that person's token; the subject's own name, a name inside a token or a
// longer word, and a one-letter name stay as written; a name two people share (authors, or people the
// caller listed as mentioned whom the prompt shows without a token) drops the claim, and so does a text
// the tokens make too long.
func TestLiveNamesTieLiteralNames(t *testing.T) {
	s := resolverService(t)
	r := memRoom{guild: "g", channel: "c", room: "discord:g:c", source: "discord"}
	ref := func(id string) string { return personRef(id) }
	lines := []episodes.Message{
		{ID: "o1", Account: "1", Author: "민수", Text: "영희는 찰리랑 연습해"},
		{ID: "o2", Account: "2", Author: "영희", Text: "응"},
		{ID: "o3", Account: "9", Author: "수", Text: "나도"},
	}
	in := episodes.Input{Lines: lines, BotName: "봇", Now: time.Now(), RefOf: func(m episodes.Message) string { return personRefOf(r, m) },
		Mentioned: []episodes.Person{{Ref: ref("5"), Label: "찰리", Link: episodes.LinkName},
			{Ref: ref("6"), Label: "새봄", Link: episodes.LinkName}, {Ref: ref("7"), Label: "새봄", Link: episodes.LinkName}}}
	names := s.liveNames(episodes.Build(in), in)
	tok := func(id string) string { return "{@" + ref(id) + "}" }
	subject := ref("2")
	for name, tc := range map[string]struct{ text, want string }{
		"a name with a particle":     {tok("2") + "는 찰리랑 연습함", tok("2") + "는 " + tok("5") + "랑 연습함"},
		"an author's name":           {tok("2") + "는 민수와 친함", tok("2") + "는 " + tok("1") + "와 친함"},
		"twice":                      {"찰리, " + tok("2") + ", 찰리", tok("5") + ", " + tok("2") + ", " + tok("5")},
		"the subject's own name":     {tok("2") + "의 별명은 영희", tok("2") + "의 별명은 영희"},
		"inside a longer word":       {tok("2") + "는 김민수 팬임", tok("2") + "는 김민수 팬임"},
		"not followed by a particle": {tok("2") + "는 찰리스턴을 춤", tok("2") + "는 찰리스턴을 춤"},
		"a one-letter name":          {tok("2") + "는 수영을 좋아함", tok("2") + "는 수영을 좋아함"},
		"no name":                    {tok("2") + "는 등산을 좋아함", tok("2") + "는 등산을 좋아함"},
		"already a token":            {tok("2") + "는 " + tok("5") + "와 친함", tok("2") + "는 " + tok("5") + "와 친함"},
	} {
		got, ok := names(tc.text, subject)
		if !ok || got != tc.want {
			t.Errorf("%s: %q %v, want %q", name, got, ok, tc.want)
		}
	}
	for name, text := range map[string]string{
		"a name two mentioned people share": tok("2") + "는 새봄과 친함",
		"too long once tied":                tok("2") + "는 " + strings.Repeat("찰리와 ", 8) + "친함",
	} {
		if got, ok := names(text, subject); ok {
			t.Errorf("%s: kept as %q", name, got)
		}
	}
	// two authors with one name
	twins := append(slices.Clone(lines), episodes.Message{ID: "o4", Account: "8", Author: "민수", Text: "나도 민수야"})
	in2 := episodes.Input{Lines: twins, BotName: "봇", Now: time.Now(), RefOf: in.RefOf}
	if got, ok := s.liveNames(episodes.Build(in2), in2)(tok("2")+"는 민수와 친함", subject); ok {
		t.Errorf("a name two authors share: kept as %q", got)
	}
}
