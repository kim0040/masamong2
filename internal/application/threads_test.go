package application_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"masamong/internal/application"
	"masamong/internal/chat"
	"masamong/internal/config"
)

// A thread follows its parent channel: the parent's settings, its pause, and its memory.
func TestThreadFollowsItsParent(t *testing.T) {
	svc := setupSvc(t)
	applyDoc(t, svc, func(d *config.Document) {
		if d.Guilds == nil {
			d.Guilds = map[string]config.GuildSettings{}
		}
		d.Guilds["g"] = config.GuildSettings{Channels: map[string]config.ChannelPolicy{"parent": {Respond: "always", Capture: true}}}
	})
	if svc.ChannelPolicyFor("g", "th").Respond == "always" {
		t.Fatal("an unknown channel has the defaults")
	}
	svc.NoteThread("g", "th", "parent", false)
	if p := svc.ChannelPolicyFor("g", "th"); p.Respond != "always" || !p.Capture {
		t.Fatalf("thread did not inherit: %+v", p)
	}
	rooms := svc.ChannelRoomsForTest("g", "th")
	if !containsAll(rooms, "discord:g:th", "discord:g:parent") {
		t.Fatalf("thread rooms: %v", rooms)
	}
	if !containsAll(svc.ChannelRoomsForTest("g", "parent"), "discord:g:parent", "discord:g:th") {
		t.Fatal("the parent searches its threads too")
	}
	if r := exec(svc, "runtime.pause", map[string]any{"scope": "channel", "guild_id": "g", "channel_id": "parent"}); !r.OK {
		t.Fatal(r)
	}
	if !svc.Paused("g", "th") {
		t.Fatal("a paused channel's threads are paused")
	}
}

// A long one-on-one conversation gets one thread offer; "keep it here" holds the next one back; a bot
// thread that settled a plan posts a recap in its parent when it ends.
func TestThreadOfferAndRecap(t *testing.T) {
	var plan, resp recordingServer
	planURL := plan.start(t, func(p string) string {
		if strings.Contains(p, "has_plan") {
			return `{"summary": "제주 5/3–5 · 숙소 협재", "has_plan": true}`
		}
		return `{"memory_requirement":"none","tool_plan":[]}`
	})
	respURL := resp.start(t, func(string) string { return strings.Repeat("길게 설명하는 답이야. ", 30) })
	svc := routedSvc(t, planURL, respURL)
	gw := &postGateway{}
	svc.Discord = gw
	turn := func(id string) (string, bool) {
		svc.Observe(application.ObservedMessage{GuildID: "g", ChannelID: "c", MessageID: id, AuthorID: "uc", AuthorName: "Chris",
			Content: "@마사몽 제주 일정", At: time.Now(), Mentions: []string{"bot"}})
		res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: id,
			Message: "@마사몽 제주 일정", Speaker: "Chris", SpeakerID: "uc",
			Deliver: func(_ context.Context, r chat.Result) string { return r.Answer }})
		if err != nil {
			t.Fatal(err)
		}
		o, ok := svc.TakeThreadOffer(res.TurnID)
		return o.Text, ok
	}
	for i := 1; i <= 5; i++ {
		if _, ok := turn(string(rune('0' + i))); ok {
			t.Fatalf("turn %d: long answers alone are not enough", i)
		}
	}
	text, ok := turn("6")
	if !ok || text == "" {
		t.Fatal("six turns of long answers should be offered a thread")
	}
	if _, ok := turn("7"); ok {
		t.Fatal("one offer per conversation")
	}
	svc.DeclineThread("g", "c", "uc")
	first := svc.ThreadOpened(context.Background(), "g", "c", "th", "uc", "")
	if !strings.Contains(first, "<@uc>") || !strings.Contains(first, "협재") {
		t.Fatalf("first message: %q", first)
	}
	if v, ok := svc.ThreadFor("th", "uc"); !ok || !v.BotOwned || !v.Participant {
		t.Fatalf("thread view: %+v", v)
	}
	if !svc.ThreadStopCue("g", "th", "uc", "그만!") || svc.ThreadStopCue("g", "th", "uc", "그만 좀 웃겨") {
		t.Fatal("stop cue is the whole message only")
	}
	if svc.ThreadStopCue("g", "th", "someone", "그만!") {
		t.Fatal("a bystander's stop word does not close the bot's thread")
	}
	if v, _ := svc.ThreadFor("th", "someone"); v.Alone {
		t.Fatal("the owner counts as a person in the thread before writing there")
	}
	// only what was said in the thread makes a recap: the seeded parent turns alone do not
	if err := svc.CloseThread(context.Background(), "th", "stop"); err != nil {
		t.Fatal(err)
	}
	for _, p := range gw.posts {
		if strings.HasPrefix(p, "c: 🧵") {
			t.Fatalf("a thread with no talk of its own has nothing to recap: %v", gw.posts)
		}
	}
	svc.ThreadOpened(context.Background(), "g", "c", "th2", "uc", "")
	for _, id := range []string{"t1", "t2"} {
		if _, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "th2", MessageID: id,
			Message: "숙소는 협재로 하자", Speaker: "Chris", SpeakerID: "uc",
			Deliver: func(_ context.Context, r chat.Result) string { return r.Answer }}); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.CloseThread(context.Background(), "th2", "stop"); err != nil {
		t.Fatal(err)
	}
	var recap bool
	for _, p := range gw.posts {
		if strings.HasPrefix(p, "c: 🧵") && strings.Contains(p, "협재") {
			recap = true
		}
	}
	if !recap {
		t.Fatalf("no recap in the parent: %v", gw.posts)
	}
	if err := svc.CloseThread(context.Background(), "th", "stop"); err == nil {
		t.Fatal("closing twice")
	}
	rows := svc.ConversationLogForTest("thread")
	if len(rows) < 3 || rows[0] != "offer" {
		t.Fatalf("thread decisions: %v", rows)
	}
}

// With conversation.thread: off a conversation that would have been offered a thread gets no offer and
// no automatic thread, only the record that it would have: the same six turns offer one with offer.
func TestThreadOffMakesNoOfferButRecordsIt(t *testing.T) {
	run := func(mode string) (offered bool, rows []string) {
		var plan, resp recordingServer
		planURL := plan.start(t, func(string) string { return `{"memory_requirement":"none","tool_plan":[]}` })
		respURL := resp.start(t, func(string) string { return strings.Repeat("길게 설명하는 답이야. ", 30) })
		svc := routedSvc(t, planURL, respURL)
		svc.Discord = &postGateway{}
		applyDoc(t, svc, func(d *config.Document) {
			if d.Guilds == nil {
				d.Guilds = map[string]config.GuildSettings{}
			}
			d.Guilds["g"] = config.GuildSettings{Channels: map[string]config.ChannelPolicy{"c": {Respond: "mention",
				Conversation: config.ChannelConversation{Thread: mode}}}}
		})
		for i := 1; i <= 6; i++ {
			id := string(rune('0' + i))
			svc.Observe(application.ObservedMessage{GuildID: "g", ChannelID: "c", MessageID: id, AuthorID: "uc", AuthorName: "Chris",
				Content: "@마사몽 제주 일정", At: time.Now(), Mentions: []string{"bot"}})
			res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: id,
				Message: "@마사몽 제주 일정", Speaker: "Chris", SpeakerID: "uc",
				Deliver: func(_ context.Context, r chat.Result) string { return r.Answer }})
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := svc.TakeThreadOffer(res.TurnID); ok {
				offered = true
			}
		}
		return offered, svc.ConversationLogForTest("thread")
	}
	if offered, rows := run("offer"); !offered || len(rows) == 0 || rows[0] != "offer" {
		t.Fatalf("offer mode: offered=%v log=%v", offered, rows)
	}
	offered, rows := run("off")
	if offered {
		t.Fatal("thread off: an offer (or an automatic thread) was made")
	}
	if len(rows) == 0 || rows[0] != "would_offer" {
		t.Fatalf("thread off should record what it would have done: %v", rows)
	}
	for _, r := range rows {
		if r == "offer" || r == "auto" {
			t.Fatalf("thread off logged %q", r)
		}
	}
}

// A closed bot thread the in-memory list let go of stays closed when it is seen again: the gateway's
// NoteThread reads its row back instead of taking it for a new open thread.
func TestForgottenClosedThreadStaysClosed(t *testing.T) {
	svc := setupSvc(t)
	svc.ThreadOpened(context.Background(), "g", "c", "th", "uc", "")
	if err := svc.CloseThread(context.Background(), "th", "idle"); err != nil {
		t.Fatal(err)
	}
	application.ForgetThreadsForTest(svc)
	svc.NoteThread("g", "th", "c", true)
	if v, ok := svc.ThreadFor("th", "uc"); !ok || !v.BotOwned || v.Open {
		t.Fatalf("thread view: %+v %v", v, ok)
	}
	svc.NoteThread("g", "th-new", "c", true)
	if v, ok := svc.ThreadFor("th-new", "uc"); !ok || !v.Open {
		t.Fatalf("a new thread is open: %+v %v", v, ok)
	}
}

func TestBotThreadParticipantsRestoreAfterRestart(t *testing.T) {
	svc := setupSvc(t)
	svc.ThreadOpened(context.Background(), "g", "parent", "thread", "owner", "")
	svc.NoteThreadParticipant("thread", "member")
	svc = restartServiceForTest(t, svc)
	view, ok := svc.ThreadFor("thread", "member")
	if !ok || !view.Participant || !svc.ThreadStopCue("g", "thread", "member", "stop") {
		t.Fatal("a participant who spoke before restart could not stop the bot thread")
	}
}

func TestWithdrawnThreadParticipantIsNotRestored(t *testing.T) {
	svc := setupSvc(t)
	svc.ThreadOpened(context.Background(), "g", "parent", "thread", "owner", "")
	svc.NoteThreadParticipant("thread", "member")
	var before int
	if err := svc.Store.DB.QueryRow(`SELECT COUNT(*) FROM thread_participants WHERE thread_id=? AND user_id=?`, "thread", "member").Scan(&before); err != nil || before != 1 {
		t.Fatal("the participant row should exist before withdrawal")
	}
	if r := exec(svc, "privacy.consent", map[string]any{"user_id": "member", "scope": "memory", "decision": "withdraw"}); !r.OK {
		t.Fatal("memory withdrawal failed")
	}
	drain(t, svc)
	var after int
	if err := svc.Store.DB.QueryRow(`SELECT COUNT(*) FROM thread_participants WHERE thread_id=? AND user_id=?`, "thread", "member").Scan(&after); err != nil || after != 0 {
		t.Fatal("withdrawal did not directly delete the participant row")
	}
	svc = restartServiceForTest(t, svc)
	view, ok := svc.ThreadFor("thread", "member")
	if !ok || view.Participant {
		t.Fatal("a withdrawn participant was restored after restart")
	}
}

func TestThreadParticipantAfterWithdrawalStaysInMemoryOnly(t *testing.T) {
	svc := setupSvc(t)
	svc.ThreadOpened(context.Background(), "g", "parent", "thread", "owner", "")
	if r := exec(svc, "privacy.consent", map[string]any{"user_id": "member", "scope": "memory", "decision": "withdraw"}); !r.OK {
		t.Fatal("memory withdrawal failed")
	}
	drain(t, svc)
	svc.NoteThreadParticipant("thread", "member")
	var persisted int
	if err := svc.Store.DB.QueryRow(`SELECT COUNT(*) FROM thread_participants WHERE thread_id=? AND user_id=?`, "thread", "member").Scan(&persisted); err != nil {
		t.Fatal("could not inspect participant persistence")
	}
	view, ok := svc.ThreadFor("thread", "member")
	if persisted != 0 || !ok || !view.Participant || !svc.ThreadStopCue("g", "thread", "member", "stop") {
		t.Fatal("a withdrawn participant should work in memory without being persisted")
	}
	svc = restartServiceForTest(t, svc)
	view, ok = svc.ThreadFor("thread", "member")
	if !ok || view.Participant {
		t.Fatal("an in-memory-only participant was restored after restart")
	}
}

func containsAll(list []string, want ...string) bool {
	seen := map[string]bool{}
	for _, v := range list {
		seen[v] = true
	}
	for _, w := range want {
		if !seen[w] {
			return false
		}
	}
	return true
}

// A thread the in-memory table let go of (idle for a day, or over the cap) behaves as before: it still
// follows its parent's policy and pause, the parent still searches its room, a thread it still hears
// from is read back, and an archive or delete event reaches its row.
func TestEvictedThreadKeepsItsParentRelation(t *testing.T) {
	svc := setupSvc(t)
	applyDoc(t, svc, func(d *config.Document) {
		if d.Guilds == nil {
			d.Guilds = map[string]config.GuildSettings{}
		}
		d.Guilds["g"] = config.GuildSettings{Channels: map[string]config.ChannelPolicy{"parent": {Respond: "always", Capture: true}}}
	})
	for _, id := range []string{"th", "th-archived", "th-deleted"} {
		svc.NoteThread("g", id, "parent", false)
	}
	application.ForgetThreadsForTest(svc)

	if p := svc.ChannelPolicyFor("g", "th"); p.Respond != "always" || !p.Capture {
		t.Fatalf("a thread that left memory lost its parent's policy: %+v", p)
	}
	application.ForgetThreadsForTest(svc)
	if !containsAll(svc.ChannelRoomsForTest("g", "parent"), "discord:g:parent", "discord:g:th", "discord:g:th-archived") {
		t.Fatalf("the parent stopped searching a thread that left memory: %v", svc.ChannelRoomsForTest("g", "parent"))
	}
	application.ForgetThreadsForTest(svc)
	if !containsAll(svc.ChannelRoomsForTest("g", "th"), "discord:g:th", "discord:g:parent") {
		t.Fatal("a thread that left memory lost its parent's room")
	}
	if r := exec(svc, "runtime.pause", map[string]any{"scope": "channel", "guild_id": "g", "channel_id": "parent"}); !r.OK {
		t.Fatal(r)
	}
	application.ForgetThreadsForTest(svc)
	if !svc.Paused("g", "th") {
		t.Fatal("a paused channel's thread that left memory is not paused")
	}
	application.ForgetThreadsForTest(svc)
	if _, ok := svc.ThreadFor("th", "someone"); !ok {
		t.Fatal("a thread that left memory was not read back when it was looked up")
	}

	state := func(id string) (string, string) {
		var st, why string
		if err := svc.Store.DB.QueryRow(`SELECT state, close_reason FROM channel_threads WHERE thread_id=?`, id).Scan(&st, &why); err != nil {
			t.Fatal(err)
		}
		return st, why
	}
	application.ForgetThreadsForTest(svc)
	svc.ThreadArchived("th-archived", true)
	if st, why := state("th-archived"); st != "closed" || why != "archived" {
		t.Fatalf("archive event for a thread that left memory: row is %q/%q", st, why)
	}
	application.ForgetThreadsForTest(svc)
	svc.ThreadDeleted("th-deleted")
	if st, why := state("th-deleted"); st != "closed" || why != "deleted" {
		t.Fatalf("delete event for a thread that left memory: row is %q/%q", st, why)
	}
	if _, ok := svc.ThreadFor("th-deleted", "someone"); ok {
		t.Fatal("a deleted thread was read back")
	}
	if rooms := svc.ChannelRoomsForTest("g", "parent"); containsAll(rooms, "discord:g:th-deleted") {
		t.Fatalf("a deleted thread is still searched: %v", rooms)
	}
}
