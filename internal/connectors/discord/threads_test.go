package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"masamong/internal/action"
	"masamong/internal/config"
)

func TestThinMessage(t *testing.T) {
	for s, want := range map[string]bool{"ㅋㅋㅋㅋ": true, "ok": true, "ㅇㅋ": true, "👍": true, "www": true, "그럼 내일은?": false, "lol what": false, "https://x.y": false} {
		if got := thinMessage(s); got != want {
			t.Errorf("%q: %v", s, got)
		}
	}
}

func TestThreadName(t *testing.T) {
	if got := threadName("Chris", "<@123> 제주도   여행 일정 짜줘"); got != "Chris · 제주도 여행 일정 짜줘" {
		t.Fatalf("%q", got)
	}
	long := threadName("Chris", "아주 긴 주제를 마흔 글자 넘게 써서 잘리는지 확인하는 문장입니다 정말로 길게 씁니다")
	if r := []rune(long); r[len(r)-1] != '…' {
		t.Fatalf("not clipped: %q", long)
	}
	if got := threadName("", "여행 일정"); got != "여행 일정" {
		t.Fatalf("a thread opened by someone else is named by its topic alone: %q", got)
	}
	if threadName("", "") != "🧵" {
		t.Fatal("empty name")
	}
}

// Pressing one button on a reply keeps the other's row: "more" and the thread offer live side by side.
func TestComponentsWithout(t *testing.T) {
	rows := []discordgo.MessageComponent{
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.Button{CustomID: "more:1"}}},
		&discordgo.ActionsRow{Components: []discordgo.MessageComponent{&discordgo.Button{CustomID: "thr:open:t:u"}, &discordgo.Button{CustomID: "thr:stay:t:u"}}},
	}
	left := componentsWithout(rows, "thr:")
	if len(left) != 1 || left[0].(discordgo.ActionsRow).Components[0].(discordgo.Button).CustomID != "more:1" {
		t.Fatalf("thread buttons gone, more stays: %#v", left)
	}
	left = componentsWithout(rows, "more:")
	if len(left) != 1 || len(left[0].(discordgo.ActionsRow).Components) != 2 {
		t.Fatalf("more gone, thread buttons stay: %#v", left)
	}
	if got := componentsWithout(nil, "more:"); got == nil || len(got) != 0 {
		t.Fatal("an empty list (not nil) clears the message's buttons")
	}
}

// The stop button of a bot thread is the participants' like the stop words: a bystander's press is
// answered with "not yours" and closes nothing.
func TestThreadStopButtonIsTheParticipants(t *testing.T) {
	g, fake := gatewayWithFakeDiscord(t)
	g.svc.ThreadOpened(context.Background(), "g1", "c1", "th1", "owner", "")
	press := func(user string) {
		t.Helper()
		i := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{ID: "i1", Token: "tok", Type: discordgo.InteractionMessageComponent,
			GuildID: "g1", ChannelID: "th1", Member: &discordgo.Member{User: &discordgo.User{ID: user}}, Message: &discordgo.Message{ID: "m1", Content: "bye"},
			Data: discordgo.MessageComponentInteractionData{CustomID: "thr:stop:th1"}}}
		g.handleThreadComponent(g.sess, i, []string{"thr", "stop", "th1"})
	}
	press("bystander")
	time.Sleep(100 * time.Millisecond) // a close would run in the background
	if v, ok := g.svc.ThreadFor("th1", "owner"); !ok || !v.Open {
		t.Fatalf("a bystander closed the thread: %+v", v)
	}
	if _, ok := fake.find(http.MethodPost, "/callback"); !ok {
		t.Fatal("the bystander's press was not answered")
	}
	press("owner")
	if !g.Wait(5 * time.Second) { // Wait drains the gateway: only after the last press
		t.Fatal("thread work did not finish")
	}
	if v, _ := g.svc.ThreadFor("th1", "owner"); v.Open {
		t.Fatal("the owner's press closes the thread")
	}
}

// What the gateway learned about a channel expires when the channel goes quiet and when too many are
// known, so it does not grow with every thread ever seen; a lookup keeps its channel alive.
func TestChannelKindsExpireAndAreCapped(t *testing.T) {
	g := &Gateway{}
	now := time.Now()
	g.storeChannelKind("quiet", true)
	g.storeChannelKind("used", true)
	g.storeChannelKind("plain", false)
	age := func(id string, by time.Duration) {
		v, _ := g.channelKinds.Load(id)
		v.(*channelKind).seen.Store(now.Add(-by).UnixNano())
	}
	age("quiet", channelKindIdle+time.Minute)
	age("used", channelKindIdle+time.Minute)
	if !g.channelKnown("used") { // a message in it
		t.Fatal("used channel unknown")
	}
	g.sweepChannelKinds(now)
	if g.channelKnown("quiet") || !g.channelKnown("used") || !g.channelKnown("plain") {
		t.Fatal("sweep kept the wrong channels")
	}
	for i := 0; i < channelKindCap+25; i++ {
		g.storeChannelKind(fmt.Sprintf("c%d", i), false)
		age(fmt.Sprintf("c%d", i), time.Duration(i+1)*time.Second)
	}
	g.sweepChannelKinds(now)
	n := 0
	g.channelKinds.Range(func(_, _ any) bool { n++; return true })
	if n != channelKindCap {
		t.Fatalf("%d kinds kept, want %d", n, channelKindCap)
	}
	if g.channelKnown(fmt.Sprintf("c%d", channelKindCap+24)) || !g.channelKnown("c0") {
		t.Fatal("the cap dropped a recent channel instead of the longest unheard")
	}
}

// Archiving or deleting a thread reaches the application, so a human thread is not remembered as open
// forever.
func TestThreadArchiveAndDeleteEventsReachTheApplication(t *testing.T) {
	g, _ := gatewayWithFakeDiscord(t)
	g.svc.NoteThread("g1", "th-human", "c1", false)
	if v, ok := g.svc.ThreadFor("th-human", "u"); !ok || !v.Open {
		t.Fatalf("thread not known open: %+v %v", v, ok)
	}
	thread := func(archived bool) *discordgo.Channel {
		return &discordgo.Channel{ID: "th-human", GuildID: "g1", ParentID: "c1", Type: discordgo.ChannelTypeGuildPublicThread,
			ThreadMetadata: &discordgo.ThreadMetadata{Archived: archived}}
	}
	g.onThreadUpdate(nil, &discordgo.ThreadUpdate{Channel: thread(true)})
	if v, _ := g.svc.ThreadFor("th-human", "u"); v.Open {
		t.Fatal("archived thread is still open")
	}
	g.onThreadUpdate(nil, &discordgo.ThreadUpdate{Channel: thread(false)})
	if v, _ := g.svc.ThreadFor("th-human", "u"); !v.Open {
		t.Fatal("unarchived thread stayed closed")
	}
	g.storeChannelKind("th-human", true)
	g.onThreadDelete(nil, &discordgo.ThreadDelete{Channel: thread(false)})
	if _, ok := g.svc.ThreadFor("th-human", "u"); ok || g.channelKnown("th-human") {
		t.Fatal("deleted thread is still known")
	}
}

// threadRoom sets the thread mode of room c1 in guild g1 ("" leaves the default, offer).
func threadRoom(t *testing.T, g *Gateway, mode string) {
	t.Helper()
	conv := map[string]any{}
	if mode != "" {
		conv["thread"] = mode
	}
	patch, _ := json.Marshal(map[string]any{"ops": []any{map[string]any{"path": []any{"guilds", "g1"}, "value": map[string]any{
		"channels": map[string]any{"c1": map[string]any{"respond": "mention", "conversation": conv}}}}}})
	if res := g.svc.Execute(context.Background(), action.Request{Action: "config.patch", Input: patch}); !res.OK {
		t.Fatalf("room: %+v", res.Error)
	}
}

// pressOpen presses the "open" button of a thread offer under a reply in room c1.
func pressOpen(g *Gateway) {
	offer := discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{CustomID: "thr:open:turn1:owner"}, discordgo.Button{CustomID: "thr:stay:turn1:owner"}}}
	i := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{ID: "i1", AppID: "app", Token: "tok", Type: discordgo.InteractionMessageComponent,
		GuildID: "g1", ChannelID: "c1", Member: &discordgo.Member{User: &discordgo.User{ID: "owner"}},
		Message: &discordgo.Message{ID: "m1", Content: "an answer\n-# let us move to a thread?", Components: []discordgo.MessageComponent{offer}},
		Data:    discordgo.MessageComponentInteractionData{CustomID: "thr:open:turn1:owner"}}}
	g.handleThreadComponent(g.sess, i, []string{"thr", "open", "turn1", "owner"})
}

// A thread offer that was already on screen when the room's threads were switched off makes no thread
// when pressed: the one who pressed is told privately, and the buttons are gone.
func TestThreadOpenButtonRefusedWhenThreadsAreOff(t *testing.T) {
	g, fake := gatewayWithFakeDiscord(t)
	g.ActionTimeout = 5 * time.Second // as below: a thread would be made if the check failed
	threadRoom(t, g, "off")
	pressOpen(g)
	if !g.Wait(5 * time.Second) {
		t.Fatal("thread work did not finish")
	}
	if _, ok := fake.find(http.MethodPost, "/messages/m1/threads"); ok {
		t.Fatal("a thread was made in a room whose threads are off")
	}
	cb, ok := fake.find(http.MethodPost, "/callback")
	if !ok || !strings.Contains(cb.body, `"type":7`) || !strings.Contains(cb.body, `"components":[]`) {
		t.Fatalf("the offer's buttons were not removed: %+v", cb)
	}
	notice, ok := fake.find(http.MethodPost, "/webhooks/app/tok")
	if !ok {
		t.Fatalf("the one who pressed was told nothing: %+v", fake.calls)
	}
	var sent struct {
		Content string `json:"content"`
		Flags   int    `json:"flags"`
	}
	if err := json.Unmarshal([]byte(notice.body), &sent); err != nil {
		t.Fatal(err)
	}
	want := g.svc.ThreadOffNotice("g1", "c1")
	if want == "" || sent.Content != want || sent.Flags&int(discordgo.MessageFlagsEphemeral) == 0 {
		t.Fatalf("notice %+v, want ephemeral %q", sent, want)
	}
	if _, ok := g.svc.ThreadFor("1", "owner"); ok {
		t.Fatal("a thread was recorded")
	}
}

// Regression: with threads on (the default), the same button still opens one.
func TestThreadOpenButtonOpensWhenThreadsAreOffered(t *testing.T) {
	g, fake := gatewayWithFakeDiscord(t)
	g.ActionTimeout = 5 * time.Second // the helper's spent timeout would refuse every call of the work
	threadRoom(t, g, "")
	pressOpen(g)
	if !g.Wait(5 * time.Second) {
		t.Fatal("thread work did not finish")
	}
	if _, ok := fake.find(http.MethodPost, "/messages/m1/threads"); !ok {
		t.Fatalf("no thread was made: %+v", fake.calls)
	}
	if _, ok := fake.find(http.MethodPost, "/webhooks/app/tok"); ok {
		t.Fatal("the refusal was sent although threads are on")
	}
	if v, ok := g.svc.ThreadFor("1", "owner"); !ok || !v.BotOwned {
		t.Fatalf("thread view %+v %v", v, ok)
	}
}

// With threads off the bot does not answer untagged in a thread it opened earlier (the room's ordinary
// respond rule applies); with them on it does. A thread a person made is not the bot's in either case.
func TestThreadAnswerIsNotGivenWhenThreadsAreOff(t *testing.T) {
	g, _ := gatewayWithFakeDiscord(t)
	g.svc.ThreadOpened(context.Background(), "g1", "c1", "th1", "owner", "")
	g.svc.NoteThread("g1", "th2", "c1", false)
	msg := func(channel string) *discordgo.MessageCreate {
		return &discordgo.MessageCreate{Message: &discordgo.Message{ID: "m1", GuildID: "g1", ChannelID: channel, Content: "what about the hotel then",
			Author: &discordgo.User{ID: "owner"}}}
	}
	pol := func(mode string) config.ChannelPolicy {
		return config.ChannelPolicy{Respond: "mention", Conversation: config.ChannelConversation{Thread: mode}}
	}
	if answer, ok := g.threadAnswer(msg("th1"), pol("offer"), true); !answer || !ok {
		t.Fatalf("threads on: want an untagged answer in the bot's thread, got %v %v", answer, ok)
	}
	if answer, ok := g.threadAnswer(msg("th1"), pol("off"), true); answer || ok {
		t.Fatalf("threads off: no thread rule applies, got %v %v", answer, ok)
	}
	if answer, ok := g.threadAnswer(msg("th2"), pol("offer"), true); answer || ok {
		t.Fatalf("a person's thread is not the bot's: %v %v", answer, ok)
	}
}
