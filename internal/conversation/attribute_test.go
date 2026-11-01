package conversation

import (
	"slices"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 11, 21, 0, 0, 0, time.UTC)

func at(sec int) time.Time { return t0.Add(time.Duration(sec) * time.Second) }

const bot = "B0"

func ln(id, author string, sec int, text string, opts ...func(*Line)) Line {
	l := Line{ID: id, AuthorID: author, Author: map[string]string{"A": "Alex", "C": "Chris", "D": "Dana", "E": "Eli", bot: "Masamong"}[author], At: at(sec), Text: text, Bot: author == bot}
	for _, o := range opts {
		o(&l)
	}
	return l
}

func reply(id string) func(*Line) { return func(l *Line) { l.ReplyTo = id } }
func mention(ids ...string) func(*Line) {
	return func(l *Line) { l.Mentions = append(l.Mentions, ids...) }
}

func ids(a Attribution) string { return strings.Join(a.Included, ",") }

// The case the user named: one friend talks to the bot while two others decide which game to play. The
// game lines carry no link to the bot conversation and must stay out, in Korean and in English alike
// (attribution never reads the words).
func TestGameTalkStaysOut(t *testing.T) {
	for _, words := range [][]string{
		{"@마사몽 내일 서울 비 와?", "오늘 롤 할 사람", "ㄴㄴ 발로란트 ㄱ", "내일 오후부터 비 소식 있어", "그럼 우산 챙겨야겠네", "발로 몇 시?"},
		{"@masamong will it rain in Seoul tomorrow?", "anyone up for LoL tonight", "nah valorant", "rain from tomorrow afternoon", "guess I need an umbrella", "valo at what time?"},
	} {
		lines := []Line{
			ln("1", "C", 0, words[0], mention(bot)),
			ln("2", "A", 5, words[1]),
			ln("3", "D", 9, words[2]),
			ln("4", bot, 12, words[3], reply("1")),
			ln("5", "A", 20, words[5]),
			ln("6", "C", 25, words[4], mention(bot)),
		}
		a := Attribute(lines, []string{"6"}, "C", Options{BotID: bot, Names: NamesFrom(lines)})
		if got := ids(a); got != "1,4,6" {
			t.Fatalf("included %s (reasons %v), want 1,4,6", got, a.Reasons)
		}
		if a.Excluded != 3 {
			t.Fatalf("excluded %d", a.Excluded)
		}
		// the game lines remain candidates the classifier may pick when the call points at them
		if !slices.Equal(a.Candidates, []string{"2", "3", "5"}) {
			t.Fatalf("candidates %v", a.Candidates)
		}
	}
}

// Replying to a friend's line while calling the bot brings that line in: it is what the call is about.
func TestReplyBringsTheTarget(t *testing.T) {
	lines := []Line{
		ln("1", "A", 0, "부산 국밥집 추천 좀"),
		ln("2", "D", 10, "아무거나"),
		ln("3", "C", 30, "@마사몽 여기 어디가 좋아?", reply("1"), mention(bot)),
	}
	a := Attribute(lines, []string{"3"}, "C", Options{BotID: bot})
	if ids(a) != "1,3" || a.Reasons["1"] != "replied_to" {
		t.Fatalf("got %s %v", ids(a), a.Reasons)
	}
}

// Two people ask the bot different things; answering one never mixes in the other's exchange.
func TestTwoCallersStaySeparate(t *testing.T) {
	lines := []Line{
		ln("1", "C", 0, "@bot what's the weather", mention(bot)),
		ln("2", "D", 3, "@bot recommend a game", mention(bot)),
		ln("3", bot, 6, "sunny, 21°", reply("1")),
		ln("4", bot, 8, "try Hades", reply("2")),
		ln("5", "D", 20, "@bot something co-op?", mention(bot)),
	}
	a := Attribute(lines, []string{"5"}, "D", Options{BotID: bot})
	if ids(a) != "2,4,5" {
		t.Fatalf("got %s %v", ids(a), a.Reasons)
	}
}

// One person's burst before the call joins it; a friend's line in between breaks the burst.
func TestBurst(t *testing.T) {
	lines := []Line{
		ln("1", "C", 0, "내일 부산 가는데"),
		ln("2", "C", 6, "@마사몽 비 와?", mention(bot)),
	}
	if a := Attribute(lines, []string{"2"}, "C", Options{BotID: bot}); ids(a) != "1,2" {
		t.Fatalf("burst: %s", ids(a))
	}
	lines = []Line{
		ln("1", "C", 0, "내일 부산 가는데"),
		ln("x", "A", 3, "ㅋㅋ"),
		ln("2", "C", 6, "@마사몽 비 와?", mention(bot)),
	}
	if a := Attribute(lines, []string{"2"}, "C", Options{BotID: bot}); ids(a) != "2" {
		t.Fatalf("broken burst: %s", ids(a))
	}
}

// After the bot answers, the caller's untagged follow-up belongs to the exchange; turning to a friend by
// name or by reply does not.
func TestFollowAndTurningAway(t *testing.T) {
	lines := []Line{
		ln("1", "C", 0, "@bot is the new Zelda out?", mention(bot)),
		ln("2", bot, 5, "yes, since May", reply("1")),
		ln("3", "C", 30, "nice, how long is it"),
		ln("4", "C", 50, "Alex you should get it"),
		ln("5", "A", 55, "maybe"),
		ln("6", "C", 70, "@bot price?", mention(bot)),
	}
	opt := Options{BotID: bot}
	opt.Names = NamesFrom(lines)
	a := Attribute(lines, []string{"6"}, "C", opt)
	if ids(a) != "1,2,3,6" {
		t.Fatalf("got %s %v", ids(a), a.Reasons)
	}
}

// Someone answering the caller (by mention) is part of the caller's conversation.
func TestToCaller(t *testing.T) {
	lines := []Line{
		ln("1", "A", 0, "@Chris it's on sale btw", mention("C")),
		ln("2", "C", 10, "@bot how much is it?", mention(bot)),
	}
	if a := Attribute(lines, []string{"2"}, "C", Options{BotID: bot}); ids(a) != "1,2" || a.Reasons["1"] != "to_caller" {
		t.Fatalf("got %s %v", ids(a), a.Reasons)
	}
}

// Lines older than the window and lines after the call are not looked at.
func TestWindowEdges(t *testing.T) {
	lines := []Line{
		ln("old", "C", -3600, "@bot old question", mention(bot)),
		ln("1", "C", 0, "@bot new question", mention(bot)),
		ln("later", "C", 60, "@bot after", mention(bot)),
	}
	if a := Attribute(lines, []string{"1"}, "C", Options{BotID: bot}); ids(a) != "1" {
		t.Fatalf("got %s", ids(a))
	}
}

func TestCallsName(t *testing.T) {
	cases := []struct {
		text, name string
		want       bool
	}{
		{"민수야 뭐해", "민수", true},
		{"김민수 왔어?", "민수", false},
		{"오늘 민수랑 롤함", "민수", true},
		{"hey sam!", "sam", true},
		{"samsung phone", "sam", false},
		{"ask sam.", "sam", true},
		{"みんすさん来た", "みんす", true},
		{"", "sam", false},
	}
	for _, c := range cases {
		if got := CallsName(c.text, c.name); got != c.want {
			t.Errorf("CallsName(%q,%q)=%v", c.text, c.name, got)
		}
	}
}

func TestSelectAndNumbered(t *testing.T) {
	var lines []Line
	for i := range 20 {
		lines = append(lines, ln(string(rune('a'+i)), "C", i*10, strings.Repeat("x", 100)))
	}
	kept, omitted := Select(lines, 500, 8)
	if len(kept) != 8 || omitted != 12 {
		t.Fatalf("kept %d omitted %d", len(kept), omitted)
	}
	kept, omitted = Select(lines[:3], 500, 8)
	if len(kept) != 3 || omitted != 0 {
		t.Fatalf("small: %d %d", len(kept), omitted)
	}
	text, labels := Numbered([]Line{ln("1", "A", 0, "hi"), ln("2", bot, 400, "yo", reply("1"))}, "Masamong", 50)
	if text != "[m1] Alex: hi\n[m2] (+6m) Masamong ↩m1: yo" || labels["m2"] != "2" {
		t.Fatalf("%q %v", text, labels)
	}
}
