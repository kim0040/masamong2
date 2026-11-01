package discord

import (
	"fmt"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"masamong/internal/vision"
)

func lookbackMsg(id, author, content string, pictures ...string) *discordgo.MessageCreate {
	m := &discordgo.Message{ID: id, GuildID: "g", ChannelID: "c", Content: content, Author: &discordgo.User{ID: author}}
	for _, p := range pictures {
		m.Attachments = append(m.Attachments, att(p, "image/png", p+".png"))
	}
	return &discordgo.MessageCreate{Message: m}
}

// A picture sent first and the words about it second: the call takes the caller's own picture even with
// friends talking in between, and anyone's picture only when it is the message right above the call.
// Each picture serves one turn.
func TestCallTakesThePicturePostedJustBeforeIt(t *testing.T) {
	now := time.Now()
	var r recentPictures
	r.note(lookbackMsg("101", "me", "", "a1"), now)
	r.note(lookbackMsg("102", "friend", "ㅋㅋ"), now)
	r.note(lookbackMsg("103", "me", "<@bot> 이거 뭐야"), now)
	refs, total := r.earlier("c", "103", "me", now)
	if len(refs) != 1 || total != 1 || refs[0].MessageID != "101" || refs[0].AuthorID != "me" {
		t.Fatalf("own picture before the call: %+v", refs)
	}
	r.note(lookbackMsg("104", "me", "<@bot> 날씨는?"), now)
	if refs, _ := r.earlier("c", "104", "me", now); len(refs) != 0 {
		t.Fatalf("a picture serves one turn: %+v", refs)
	}

	var o recentPictures
	o.note(lookbackMsg("201", "friend", "", "a2"), now)
	o.note(lookbackMsg("202", "me", "<@bot> 이거 뭐임"), now)
	if refs, _ := o.earlier("c", "202", "me", now); len(refs) != 1 || refs[0].AuthorID != "friend" {
		t.Fatalf("someone's picture right above the call: %+v", refs)
	}

	var far recentPictures
	far.note(lookbackMsg("301", "friend", "", "a3"), now)
	far.note(lookbackMsg("302", "other", "그거 웃기네"), now)
	far.note(lookbackMsg("303", "me", "<@bot> 이거 뭐임"), now)
	if refs, _ := far.earlier("c", "303", "me", now); len(refs) != 0 {
		t.Fatalf("someone else's picture that is not right above the call: %+v", refs)
	}
}

// Handlers may run out of order: "right above" is Discord's order (message IDs), not arrival order.
func TestLookbackFollowsDiscordOrderNotArrivalOrder(t *testing.T) {
	now := time.Now()
	var r recentPictures
	r.note(lookbackMsg("401", "friend", "", "a1"), now)
	r.note(lookbackMsg("403", "me", "<@bot> 이거 뭐임"), now)
	r.note(lookbackMsg("402", "other", "중간에 한 말"), now) // seen late, sent before the call
	if refs, _ := r.earlier("c", "403", "me", now); len(refs) != 0 {
		t.Fatalf("a line between the picture and the call (by ID) hides someone else's picture: %+v", refs)
	}
	r.note(lookbackMsg("402", "other", "중간에 한 말"), now) // a replay is not a second line
	if n := len(r.lines["c"]); n != 3 {
		t.Fatalf("lines: %d", n)
	}
	if !snowflakeLess("99", "100") || snowflakeLess("100", "99") || !snowflakeLess("100", "101") {
		t.Fatal("snowflake order")
	}
}

func TestCallDoesNotTakeAPictureTheCallerMovedOnFrom(t *testing.T) {
	now := time.Now()
	var r recentPictures
	r.note(lookbackMsg("101", "me", "", "a1"), now)
	r.note(lookbackMsg("102", "me", "아 배고프다"), now)
	r.note(lookbackMsg("103", "me", "<@bot> 날씨 어때"), now)
	if refs, _ := r.earlier("c", "103", "me", now); len(refs) != 0 {
		t.Fatalf("the caller said something else after the picture: %+v", refs)
	}

	var old recentPictures
	old.note(lookbackMsg("101", "me", "", "a1"), now.Add(-pictureLookback-time.Second))
	old.note(lookbackMsg("102", "me", "<@bot> 이거 뭐야"), now)
	if refs, _ := old.earlier("c", "102", "me", now); len(refs) != 0 {
		t.Fatalf("a picture older than the lookback: %+v", refs)
	}

	var held recentPictures
	held.note(lookbackMsg("101", "me", "<@bot> 이거 뭐야", "a1"), now)
	held.used("c", []string{"101"})
	held.note(lookbackMsg("102", "friend", "<@bot> 그럼 이건?"), now)
	if refs, _ := held.earlier("c", "102", "friend", now); len(refs) != 0 {
		t.Fatalf("a picture another call holds: %+v", refs)
	}
}

// A deleted picture leaves at once; an edit that removes it leaves nothing to read.
func TestDeletedOrEditedPictureIsNotLookedBackAt(t *testing.T) {
	now := time.Now()
	var r recentPictures
	r.note(lookbackMsg("101", "me", "", "a1"), now)
	r.forget("c", []string{"101"})
	r.note(lookbackMsg("102", "me", "<@bot> 이거 뭐야"), now)
	if refs, _ := r.earlier("c", "102", "me", now); len(refs) != 0 {
		t.Fatalf("deleted picture: %+v", refs)
	}

	var e recentPictures
	e.note(lookbackMsg("201", "me", "", "a1"), now)
	e.edited("c", "201", nil)
	e.note(lookbackMsg("202", "me", "<@bot> 이거 뭐야"), now)
	if refs, _ := e.earlier("c", "202", "me", now); len(refs) != 0 {
		t.Fatalf("picture removed by an edit: %+v", refs)
	}
}

func TestLookbackCountsPicturesOverTheLimit(t *testing.T) {
	now := time.Now()
	var r recentPictures
	r.note(lookbackMsg("101", "me", "", "a1", "a2", "a3", "a4"), now)
	r.note(lookbackMsg("102", "me", "<@bot> 이거 다 뭐야"), now)
	refs, total := r.earlier("c", "102", "me", now)
	if len(refs) != vision.MaxImages || total != 4 {
		t.Fatalf("refs %d total %d", len(refs), total)
	}
}

// The words first and the picture a few seconds later: the picture continues that call once, within
// pictureFollow, carrying its words. Saying something else first, or deleting the call, drops it.
func TestPictureRightAfterACallWithoutOneFollowsIt(t *testing.T) {
	now := time.Now()
	var r recentPictures
	r.called("c", "me", "<@bot> 이거 번역해줘", []string{"101"}, now)
	if _, ok := r.follows("c", "friend", now); ok {
		t.Fatal("someone else's picture does not continue my call")
	}
	r.note(lookbackMsg("102", "me", "", "a1"), now)
	c, ok := r.follows("c", "me", now.Add(10*time.Second))
	if !ok || c.text != "<@bot> 이거 번역해줘" {
		t.Fatalf("my picture right after my call continues it: %+v %v", c, ok)
	}
	if _, ok := r.follows("c", "me", now.Add(11*time.Second)); ok {
		t.Fatal("only one picture message continues a call")
	}
	r.called("c", "me", "q", []string{"103"}, now)
	if _, ok := r.follows("c", "me", now.Add(pictureFollow+time.Second)); ok {
		t.Fatal("a picture long after the call is not its rest")
	}
	r.called("c", "me", "q", []string{"104"}, now)
	r.note(lookbackMsg("105", "me", "아니다 됐어"), now)
	if _, ok := r.follows("c", "me", now); ok {
		t.Fatal("the person moved on")
	}
	r.called("c", "me", "q", []string{"106"}, now)
	r.forget("c", []string{"106"})
	if _, ok := r.follows("c", "me", now); ok {
		t.Fatal("a deleted call is not continued")
	}

	if !pictureOnly(lookbackMsg("p", "me", "", "a").Message) || pictureOnly(lookbackMsg("p", "me", "봐봐", "a").Message) ||
		pictureOnly(lookbackMsg("p", "me", "").Message) {
		t.Fatal("pictureOnly: pictures and nothing else")
	}
}

// Many people in one busy channel, or many channels, do not grow the memory past the lookback.
func TestRecentPicturesForgetsOldCallsAndLines(t *testing.T) {
	now := time.Now()
	var r recentPictures
	for i := 0; i < 1000; i++ {
		r.called("c", fmt.Sprint("p", i), "q", []string{fmt.Sprint(i)}, now)
		r.note(lookbackMsg(fmt.Sprint(1000+i), "x", "hi"), now)
		r.lines[fmt.Sprint("ch", i)] = []recentLine{{id: "1", at: now}}
	}
	later := now.Add(pictureLookback + time.Second)
	r.note(lookbackMsg("5000", "x", "hi"), later)
	if len(r.calls) != 0 || len(r.lines) != 1 || len(r.lines["c"]) != 1 {
		t.Fatalf("calls %d channels %d", len(r.calls), len(r.lines))
	}
}

func TestHeldCallLooksBackForAPictureAndMarksTheTurn(t *testing.T) {
	svc := svcForTest(t)
	g := &Gateway{svc: svc}
	bot := "bot"
	g.botID.Store(&bot)
	now := time.Now()
	g.pictures.note(lookbackMsg("101", "me", "", "a1"), now)
	call := lookbackMsg("102", "me", "<@bot> 이거 뭐야")
	call.Mentions = []*discordgo.User{{ID: bot}}
	g.pictures.note(call, now)
	got := g.heldCall([]*discordgo.MessageCreate{call})
	if len(got.images) != 1 || !got.imageEarlier || got.imageOnly || got.images[0].MessageID != "101" {
		t.Fatalf("looked-back picture: %+v", got)
	}

	// a bare call after the picture: the picture is the request
	g.pictures.note(lookbackMsg("103", "me", "", "a2"), now)
	bare := lookbackMsg("104", "me", "<@bot>")
	bare.Mentions = []*discordgo.User{{ID: bot}}
	g.pictures.note(bare, now)
	if got := g.heldCall([]*discordgo.MessageCreate{bare}); !got.imageEarlier || !got.imageOnly {
		t.Fatalf("bare call after a picture: %+v", got)
	}

	// a call that found no picture waits for the picture sent right after it, which is answered as
	// addressed, with the call's words, even without a mention
	ask := lookbackMsg("105", "me", "<@bot> 이 사진 번역해줘")
	ask.Mentions = []*discordgo.User{{ID: bot}}
	g.pictures.note(ask, now)
	if got := g.heldCall([]*discordgo.MessageCreate{ask}); len(got.images) != 0 {
		t.Fatalf("no picture yet: %+v", got)
	}
	pic := lookbackMsg("106", "me", "", "a3")
	g.pictures.note(pic, now)
	if !g.notePictureFollow(pic) {
		t.Fatal("the picture should continue the call")
	}
	got = g.heldCall([]*discordgo.MessageCreate{pic})
	if !got.addressed || len(got.images) != 1 || got.imageEarlier || got.imageOnly || got.continues != "<@bot> 이 사진 번역해줘" {
		t.Fatalf("picture following the call: %+v", got)
	}
}

// A call by name alone may turn out not to be one: it leaves nothing waiting for a picture.
func TestNameOnlyCallDoesNotWaitForAPicture(t *testing.T) {
	g := &Gateway{}
	msg := lookbackMsg("101", "me", "마사몽 또 고장났네")
	g.pictures.note(msg, time.Now())
	g.callPictures([]*discordgo.MessageCreate{msg}, &turnCall{nameOnly: true})
	if _, ok := g.pictures.follows("c", "me", time.Now()); ok {
		t.Fatal("a name-only call left a call waiting for a picture")
	}
	g.callPictures([]*discordgo.MessageCreate{msg}, &turnCall{})
	if _, ok := g.pictures.follows("c", "me", time.Now()); !ok {
		t.Fatal("an ordinary call without a picture should wait for one")
	}
}
