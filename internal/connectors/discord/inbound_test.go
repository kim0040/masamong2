package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/config"
)

func TestBodyOfForwardsAndStickers(t *testing.T) {
	snap := func(content string, stickers ...string) discordgo.MessageSnapshot {
		m := &discordgo.Message{Content: content}
		for _, s := range stickers {
			m.StickerItems = append(m.StickerItems, &discordgo.StickerItem{Name: s})
		}
		return discordgo.MessageSnapshot{Message: m}
	}
	cases := []struct {
		name string
		m    *discordgo.Message
		want string
	}{
		{"plain", &discordgo.Message{Content: "  hi  "}, "hi"},
		{"sticker alone", &discordgo.Message{StickerItems: []*discordgo.StickerItem{{Name: "Wave"}}}, "[sticker: Wave]"},
		{"text and sticker", &discordgo.Message{Content: "hi", StickerItems: []*discordgo.StickerItem{{Name: "Wave"}}}, "hi [sticker: Wave]"},
		{"odd sticker name", &discordgo.Message{StickerItems: []*discordgo.StickerItem{{Name: "a]\n[b"}, {Name: ""}, {Name: strings.Repeat("x", 90)}}},
			"[sticker: a b] [sticker: ?] [sticker: " + strings.Repeat("x", stickerNameMax) + "]"},
		{"forward", &discordgo.Message{MessageSnapshots: []discordgo.MessageSnapshot{snap("original text")}}, "↪ forwarded: original text"},
		{"forwarded sticker", &discordgo.Message{MessageSnapshots: []discordgo.MessageSnapshot{snap("", "Wave")}}, "↪ forwarded: [sticker: Wave]"},
		{"empty forward", &discordgo.Message{MessageSnapshots: []discordgo.MessageSnapshot{snap(""), {}}}, ""},
		{"nothing", &discordgo.Message{}, ""},
	}
	for _, c := range cases {
		if got := bodyOf(c.m); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	var many []discordgo.MessageSnapshot
	for i := 0; i < 6; i++ {
		many = append(many, snap(fmt.Sprint("n", i)))
	}
	if got := bodyOf(&discordgo.Message{MessageSnapshots: many}); strings.Count(got, forwardMark) != forwardMax {
		t.Errorf("forwards are bounded: %q", got)
	}
	if bodyOf(nil) != "" {
		t.Error("nil message")
	}
}

func TestForwardAndStickerAreNotBareCallsNorReplies(t *testing.T) {
	svc := svcForTest(t)
	g := &Gateway{svc: svc, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	id := "bot"
	g.botID.Store(&id)
	call := func(mod func(m *discordgo.Message)) *discordgo.MessageCreate {
		m := &discordgo.Message{ID: "m", Content: "<@bot>", Author: &discordgo.User{ID: "u"}}
		if mod != nil {
			mod(m)
		}
		return &discordgo.MessageCreate{Message: m}
	}
	if !g.isBareCall(call(nil)) {
		t.Fatal("a lone mention is a bare call")
	}
	for name, mod := range map[string]func(m *discordgo.Message){
		"sticker": func(m *discordgo.Message) { m.StickerItems = []*discordgo.StickerItem{{Name: "Wave"}} },
		"forward": func(m *discordgo.Message) {
			m.MessageSnapshots = []discordgo.MessageSnapshot{{Message: &discordgo.Message{Content: "x"}}}
		},
	} {
		if g.isBareCall(call(mod)) {
			t.Errorf("a mention with a %s is a request", name)
		}
	}
	fwd := call(func(m *discordgo.Message) {
		m.MessageReference = &discordgo.MessageReference{Type: discordgo.MessageReferenceTypeForward, MessageID: "orig"}
	})
	var o application.ObservedMessage
	linksOf(fwd, &o)
	if o.ReplyTo != "" {
		t.Fatalf("a forward is not a reply: %q", o.ReplyTo)
	}
	reply := call(func(m *discordgo.Message) { m.MessageReference = &discordgo.MessageReference{MessageID: "orig"} })
	o = application.ObservedMessage{}
	linksOf(reply, &o)
	if o.ReplyTo != "orig" {
		t.Fatalf("a reply keeps its target: %q", o.ReplyTo)
	}
}

func TestMergedTextCarriesForwardsAndStickers(t *testing.T) {
	a := msg("a", "look at this")
	b := msg("a", "")
	b.MessageSnapshots = []discordgo.MessageSnapshot{{Message: &discordgo.Message{Content: "original"}}}
	c := msg("a", "")
	c.StickerItems = []*discordgo.StickerItem{{Name: "Wave"}}
	if got := mergedText([]*discordgo.MessageCreate{a, b, c}); got != "look at this\n↪ forwarded: original\n[sticker: Wave]" {
		t.Fatalf("merged: %q", got)
	}
}

func TestSeenSetDropsReplaysWithinTTLAndIsBounded(t *testing.T) {
	var s seenSet
	t0 := time.Now()
	if !s.first("a", t0) || s.first("a", t0.Add(time.Minute)) {
		t.Fatal("a replay inside the TTL is a duplicate")
	}
	if !s.first("a", t0.Add(seenTTL+time.Second)) {
		t.Fatal("after the TTL the ID is forgotten")
	}
	if !s.first("", t0) || !s.first("", t0) {
		t.Fatal("a message without an ID is never a duplicate")
	}
	var b seenSet
	for i := 0; i < seenCap+500; i++ {
		if !b.first(fmt.Sprint("id", i), t0) {
			t.Fatalf("id%d seen before", i)
		}
	}
	if b.order.Len() > seenCap || len(b.ids) > seenCap {
		t.Fatalf("set grew to %d/%d", b.order.Len(), len(b.ids))
	}
	if !b.first("id0", t0) {
		t.Fatal("the oldest entry is evicted first")
	}
	if b.first(fmt.Sprint("id", seenCap+499), t0) {
		t.Fatal("the newest entry is kept")
	}
}

func TestTurnRegistryStopsOnlyThatPersonInThatChannel(t *testing.T) {
	var r turnRegistry
	mk := func() (context.Context, context.CancelCauseFunc) {
		return context.WithCancelCause(context.Background())
	}
	ctx1, c1 := mk()
	ctx2, c2 := mk() // the same person's second turn queued behind the first
	other, c3 := mk()
	elsewhere, c4 := mk()
	done1 := r.add("c", "u", c1)
	r.add("c", "u", c2)
	r.add("c", "v", c3)
	r.add("d", "u", c4)
	if !r.active("c", "u") || r.active("c", "w") {
		t.Fatal("active")
	}
	if !r.stop("c", "u") {
		t.Fatal("something was in flight")
	}
	for _, ctx := range []context.Context{ctx1, ctx2} {
		if context.Cause(ctx) != errTurnStopped {
			t.Fatalf("cause = %v", context.Cause(ctx))
		}
	}
	if other.Err() != nil || elsewhere.Err() != nil {
		t.Fatal("another person or channel was stopped")
	}
	if r.stop("c", "u") || r.active("c", "u") {
		t.Fatal("nothing left to stop")
	}
	done1() // a finished turn removing itself after a stop is harmless
	if len(r.by) != 2 {
		t.Fatalf("registry keeps %d keys", len(r.by))
	}
}

func inboundGateway(t *testing.T) (*Gateway, *turns) {
	t.Helper()
	shortWindows(t)
	svc := svcForTest(t)
	policy, _ := json.Marshal(map[string]any{"server_approval": false})
	if res := svc.Execute(context.Background(), action.Request{Action: "access.policy", Input: policy}); !res.OK {
		t.Fatalf("disable server approval: %+v", res.Error)
	}
	room, _ := json.Marshal(map[string]any{"ops": []any{map[string]any{"path": []any{"guilds", "g"}, "value": map[string]any{
		"channels": map[string]any{"c": map[string]any{"respond": "mention", "capture": false}}}}}})
	if res := svc.Execute(context.Background(), action.Request{Action: "config.patch", Input: room}); !res.OK {
		t.Fatalf("room: %+v", res.Error)
	}
	g := &Gateway{svc: svc, cfg: config.Discord{MessageContent: true, CommandPrefix: "!"}, log: slog.New(slog.NewTextHandler(io.Discard, nil)), ctx: context.Background()}
	id := "bot"
	g.botID.Store(&id)
	g.storeChannelKind("c", false) // a known ordinary channel: no thread lookup over REST
	tr := &turns{}
	g.merge.fire = tr.fire
	g.merge.forget = g.forgetFollow
	t.Cleanup(func() { g.inflight.Wait() })
	return g, tr
}

func mention(id, user, text string) *discordgo.MessageCreate {
	return &discordgo.MessageCreate{Message: &discordgo.Message{ID: id, GuildID: "g", ChannelID: "c", Content: "<@bot> " + text,
		Author: &discordgo.User{ID: user}, Mentions: []*discordgo.User{{ID: "bot"}}}}
}

func TestReplayedMessageCreateAnswersOnce(t *testing.T) {
	g, tr := inboundGateway(t)
	g.onMessage(nil, mention("m1", "u", "내일 날씨 어때?"))
	g.onMessage(nil, mention("m1", "u", "내일 날씨 어때?")) // Discord's replay after a RESUME
	time.Sleep(300 * time.Millisecond)
	// a replayed message would join the first as a second fragment of the same request
	if got := tr.list(); len(got) != 1 || got[0] != "<@bot> 내일 날씨 어때?" {
		t.Fatalf("turns: %q", got)
	}
	g.onMessage(nil, mention("m2", "u", "내일 날씨 어때?")) // a new message with the same words is a new request
	time.Sleep(300 * time.Millisecond)
	if got := tr.list(); len(got) != 2 {
		t.Fatalf("turns: %q", got)
	}
}

func TestStopWordCancelsTheReplyInFlightAndStartsNoTurn(t *testing.T) {
	g, tr := inboundGateway(t)
	mine, cancelMine := context.WithCancelCause(context.Background())
	theirs, cancelTheirs := context.WithCancelCause(context.Background())
	g.turns.add("c", "u", cancelMine)
	g.turns.add("c", "v", cancelTheirs)
	g.onMessage(nil, mention("s1", "u", "그만!"))
	time.Sleep(300 * time.Millisecond)
	if context.Cause(mine) != errTurnStopped {
		t.Fatalf("the person's reply was not stopped: %v", context.Cause(mine))
	}
	if theirs.Err() != nil {
		t.Fatal("someone else's reply was stopped")
	}
	if got := tr.list(); len(got) != 0 {
		t.Fatalf("the stop word started a turn: %q", got)
	}
	// a stop phrase inside a request is a request, and so is a stop word said to someone else
	g.turns.add("c", "v", cancelTheirs)
	g.onMessage(nil, mention("s2", "v", "그만 좀 웃겨"))
	g.onMessage(nil, &discordgo.MessageCreate{Message: &discordgo.Message{ID: "s3", GuildID: "g", ChannelID: "c", Content: "<@bot> <@other> stop",
		Author: &discordgo.User{ID: "v"}, Mentions: []*discordgo.User{{ID: "bot"}, {ID: "other"}}}})
	time.Sleep(300 * time.Millisecond)
	if theirs.Err() != nil {
		t.Fatal("only the whole message is a stop word, and only when meant for the bot")
	}
}

func TestStopWordWithNothingInFlightIsOrdinaryTalk(t *testing.T) {
	g, tr := inboundGateway(t)
	g.onMessage(nil, mention("n1", "u", "stop"))
	time.Sleep(300 * time.Millisecond)
	if got := tr.list(); len(got) != 1 || !strings.Contains(got[0], "stop") {
		t.Fatalf("with nothing to stop the message is answered as usual: %q", got)
	}
}

func TestPendingMessageEditReplacesItsTextBeforeFlush(t *testing.T) {
	g, tr := inboundGateway(t)
	g.onMessage(nil, mention("edit-me", "u", "old text"))
	at := time.Now().UTC()
	g.onMessageUpdate(nil, &discordgo.MessageUpdate{Message: &discordgo.Message{ID: "edit-me", GuildID: "g", ChannelID: "c", Content: "<@bot> revised text", EditedTimestamp: &at}})
	time.Sleep(150 * time.Millisecond)
	if got := tr.list(); len(got) != 1 || got[0] != "<@bot> revised text" {
		t.Fatalf("edited pending request = %q", got)
	}
}

func TestDeletingTheOnlyPendingMessageDropsTheRequest(t *testing.T) {
	g, tr := inboundGateway(t)
	g.onMessage(nil, mention("delete-only", "u", "answer this"))
	g.onMessageDelete(nil, &discordgo.MessageDelete{Message: &discordgo.Message{ID: "delete-only", GuildID: "g", ChannelID: "c"}})
	time.Sleep(150 * time.Millisecond)
	if got := tr.list(); len(got) != 0 {
		t.Fatalf("deleted pending request was answered: %q", got)
	}
}

func TestDeletingOnePendingMessageKeepsTheOtherPiece(t *testing.T) {
	g, tr := inboundGateway(t)
	g.onMessage(nil, mention("delete-first", "u", "first piece"))
	follow := &discordgo.MessageCreate{Message: &discordgo.Message{ID: "keep-second", GuildID: "g", ChannelID: "c", Content: "second piece", Author: &discordgo.User{ID: "u"}}}
	g.onMessage(nil, follow)
	g.onMessageDelete(nil, &discordgo.MessageDelete{Message: &discordgo.Message{ID: "delete-first", GuildID: "g", ChannelID: "c"}})
	time.Sleep(150 * time.Millisecond)
	if got := tr.list(); len(got) != 1 || got[0] != "second piece" {
		t.Fatalf("remaining pending request = %q", got)
	}
}

func TestStopWordCancelsOnlyThatPersonsPendingRequest(t *testing.T) {
	g, tr := inboundGateway(t)
	acked := make(chan struct{}, 1)
	g.reactForTest = func(_ context.Context, _, _, emoji string) error {
		if emoji != "✋" {
			t.Errorf("stop acknowledgement = %q", emoji)
		}
		acked <- struct{}{}
		return nil
	}
	holdWindows(t)
	g.onMessage(nil, mention("pending-u", "u", "please cancel me"))
	g.onMessage(nil, mention("pending-v", "v", "keep me"))
	g.onMessage(nil, mention("stop-u", "u", "그만"))
	select {
	case <-acked:
	case <-time.After(5 * time.Second):
		t.Fatal("stop acknowledgement was not sent")
	}
	// v's request fires when its window ends; u's was cancelled and the stop word is not a request
	waitFor(t, 8*time.Second, func() bool { return len(tr.list()) > 0 })
	if got := tr.list(); len(got) != 1 || got[0] != "<@bot> keep me" {
		t.Fatalf("pending requests after stop = %q", got)
	}
}

// holdWindows keeps a request pending long enough for a slow run (-race) to deliver the next event
// before the merge window ends.
func holdWindows(t *testing.T) {
	old := [3]time.Duration{mergeSettle, mergeCallWait, mergeMax}
	mergeSettle, mergeCallWait, mergeMax = 2*time.Second, 2*time.Second, 4*time.Second
	t.Cleanup(func() { mergeSettle, mergeCallWait, mergeMax = old[0], old[1], old[2] })
}

func waitFor(t *testing.T, d time.Duration, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatal("condition not reached in time")
}

// The merger must hold nothing for a request that left without firing.
func mergerIsEmpty(g *Gateway) bool {
	g.merge.mu.Lock()
	defer g.merge.mu.Unlock()
	return len(g.merge.pending) == 0 && len(g.merge.byID) == 0
}

func followMark(g *Gateway, id string) bool {
	_, ok := g.followIDs.Load(id)
	return ok
}

func TestBulkDeleteDropsPendingPiecesAndTheirFollowMarks(t *testing.T) {
	g, tr := inboundGateway(t)
	g.messageEventApply = func(messageReconciliation) error { return nil }
	g.cancelPictures = func(string, []string) {}
	g.onMessage(nil, mention("bulk-1", "u", "first piece"))
	second := &discordgo.MessageCreate{Message: &discordgo.Message{ID: "bulk-2", GuildID: "g", ChannelID: "c", Content: "second piece", Author: &discordgo.User{ID: "u"}}}
	g.followIDs.Store("bulk-2", true) // the follow-up window accepted it
	g.merge.add(second, false)
	g.onMessageDeleteBulk(nil, &discordgo.MessageDeleteBulk{Messages: []string{"bulk-1", "bulk-2"}, GuildID: "g", ChannelID: "c"})
	time.Sleep(150 * time.Millisecond)
	if got := tr.list(); len(got) != 0 {
		t.Fatalf("bulk-deleted pending request was answered: %q", got)
	}
	if followMark(g, "bulk-2") || !mergerIsEmpty(g) {
		t.Fatalf("state left behind: follow mark %v, merger empty %v", followMark(g, "bulk-2"), mergerIsEmpty(g))
	}

	// deleting only one of two pieces keeps the other
	g.onMessage(nil, mention("bulk-3", "u", "kept piece"))
	g.merge.add(&discordgo.MessageCreate{Message: &discordgo.Message{ID: "bulk-4", GuildID: "g", ChannelID: "c", Content: "dropped piece", Author: &discordgo.User{ID: "u"}}}, false)
	g.onMessageDeleteBulk(nil, &discordgo.MessageDeleteBulk{Messages: []string{"bulk-4", "unrelated"}, GuildID: "g", ChannelID: "c"})
	time.Sleep(150 * time.Millisecond)
	if got := tr.list(); len(got) != 1 || got[0] != "<@bot> kept piece" {
		t.Fatalf("remaining request after a partial bulk delete = %q", got)
	}
}

func TestDeletingAFollowUpPieceClearsItsFollowMark(t *testing.T) {
	g, tr := inboundGateway(t)
	g.messageEventApply = func(messageReconciliation) error { return nil }
	g.cancelPictures = func(string, []string) {}
	g.followIDs.Store("follow-1", true)
	g.merge.add(&discordgo.MessageCreate{Message: &discordgo.Message{ID: "follow-1", GuildID: "g", ChannelID: "c", Content: "carrying on", Author: &discordgo.User{ID: "u"}}}, false)
	g.onMessageDelete(nil, &discordgo.MessageDelete{Message: &discordgo.Message{ID: "follow-1", GuildID: "g", ChannelID: "c"}})
	time.Sleep(100 * time.Millisecond)
	if followMark(g, "follow-1") || !mergerIsEmpty(g) || len(tr.list()) != 0 {
		t.Fatalf("follow mark %v, merger empty %v, turns %q", followMark(g, "follow-1"), mergerIsEmpty(g), tr.list())
	}
}

func TestStopWordClearsTheFollowMarksOfThePendingRequest(t *testing.T) {
	g, tr := inboundGateway(t)
	holdWindows(t)
	g.reactForTest = func(context.Context, string, string, string) error { return nil }
	g.followIDs.Store("follow-stop", true)
	g.merge.add(&discordgo.MessageCreate{Message: &discordgo.Message{ID: "follow-stop", GuildID: "g", ChannelID: "c", Content: "carrying on", Author: &discordgo.User{ID: "u"}}}, false)
	g.onMessage(nil, mention("stop-follow", "u", "그만"))
	// a stop word that was not recognised would sit in the merger as a request of its own
	waitFor(t, 5*time.Second, func() bool { return mergerIsEmpty(g) })
	if followMark(g, "follow-stop") || !mergerIsEmpty(g) || len(tr.list()) != 0 {
		t.Fatalf("follow mark %v, merger empty %v, turns %q", followMark(g, "follow-stop"), mergerIsEmpty(g), tr.list())
	}
}

// An edit that arrives after the request was already handed to the turn changes nothing in the merger:
// it does not fire the request a second time or leave an index entry behind.
func TestEditAfterFlushDoesNotRefireOrLeaveState(t *testing.T) {
	g, tr := inboundGateway(t)
	g.messageEventApply = func(messageReconciliation) error { return nil }
	g.onMessage(nil, mention("flushed", "u", "original text"))
	time.Sleep(200 * time.Millisecond) // past the settle window: the turn has started
	if got := tr.list(); len(got) != 1 || got[0] != "<@bot> original text" {
		t.Fatalf("turns before the edit: %q", got)
	}
	if g.merge.edit("flushed", "edited text") {
		t.Fatal("an edit of a flushed message was applied to the merger")
	}
	at := time.Now().UTC()
	g.onMessageUpdate(nil, &discordgo.MessageUpdate{Message: &discordgo.Message{ID: "flushed", GuildID: "g", ChannelID: "c", Content: "edited text", EditedTimestamp: &at}})
	time.Sleep(150 * time.Millisecond)
	if got := tr.list(); len(got) != 1 {
		t.Fatalf("the edit started another turn: %q", got)
	}
	if !mergerIsEmpty(g) {
		t.Fatal("the merger kept state for a flushed request")
	}
}

// Edits and deletes racing the flush of a request leave one of two outcomes: it was deleted first and
// no turn starts, or it was handed to the turn (once) with its original or its edited text. Nothing is
// left in the merger either way.
func TestPendingMessageChangesRaceWithFlush(t *testing.T) {
	g, _ := inboundGateway(t)
	g.messageEventApply = func(messageReconciliation) error { return nil }
	g.cancelPictures = func(string, []string) {}
	var mu sync.Mutex
	fired := map[string][]string{} // author → the text of each turn started for them
	g.merge.fire = func(msgs []*discordgo.MessageCreate) {
		mu.Lock()
		defer mu.Unlock()
		fired[msgs[0].Author.ID] = append(fired[msgs[0].Author.ID], mergedText(msgs))
	}
	mergeSettle = time.Millisecond // the flush lands among the edits and deletes
	const rounds = 40
	at := time.Now().UTC()
	for r := 0; r < rounds; r++ {
		id, user := fmt.Sprintf("race-%d", r), fmt.Sprintf("u%d", r)
		g.onMessage(nil, mention(id, user, "original"))
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(2)
			go func() {
				defer wg.Done()
				g.onMessageUpdate(nil, &discordgo.MessageUpdate{Message: &discordgo.Message{ID: id, GuildID: "g", ChannelID: "c", Content: "edited", EditedTimestamp: &at}})
			}()
			go func() {
				defer wg.Done()
				g.onMessageDelete(nil, &discordgo.MessageDelete{Message: &discordgo.Message{ID: id, GuildID: "g", ChannelID: "c"}})
			}()
		}
		wg.Wait()
	}
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	for user, texts := range fired {
		if len(texts) != 1 {
			t.Fatalf("%s: %d turns for one request: %q", user, len(texts), texts)
		}
		if texts[0] != "<@bot> original" && texts[0] != "edited" {
			t.Fatalf("%s: a turn started with text %q, which is neither the original nor the edit", user, texts[0])
		}
	}
	if !mergerIsEmpty(g) {
		t.Fatal("the merger kept state after every request was flushed or deleted")
	}
}
