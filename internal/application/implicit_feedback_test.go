package application_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"masamong/internal/application"
	"masamong/internal/chat"
	"masamong/internal/clock"
	"masamong/internal/config"
	"masamong/internal/memory/store"
)

func implicitSvc(t *testing.T) *application.Service {
	t.Helper()
	var plan, resp recordingServer
	planURL := plan.start(t, func(string) string { return `{"memory_requirement":"none","tool_plan":[]}` })
	respURL := resp.start(t, func(string) string { return "답이야" })
	svc := routedSvc(t, planURL, respURL)
	svc.Discord = &postGateway{}
	applyDoc(t, svc, func(d *config.Document) {
		if d.Guilds == nil {
			d.Guilds = map[string]config.GuildSettings{}
		}
		d.Guilds["g"] = config.GuildSettings{Channels: map[string]config.ChannelPolicy{"c": {Respond: "mention"}}}
	})
	return svc
}

func implicitAnswer(t *testing.T, svc *application.Service, user, msgID string) string {
	t.Helper()
	res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: msgID,
		Message: "@마사몽 질문", Speaker: "Chris", SpeakerID: user,
		Deliver: func(_ context.Context, r chat.Result) string { return r.Answer }})
	if err != nil || res.TurnID == "" {
		t.Fatalf("turn: %+v %v", res, err)
	}
	return res.TurnID
}

func implicitSay(svc *application.Service, user, msgID, text, call string, at time.Time) {
	svc.Observe(application.ObservedMessage{GuildID: "g", ChannelID: "c", MessageID: msgID, AuthorID: user, AuthorName: "Chris",
		Content: text, At: at, BotCall: call})
}

// implicitRows lists the source of each row of a turn, waiting up to three seconds for at least min rows
// (the rows are written in the background).
func implicitRows(t *testing.T, svc *application.Service, turn string, min int) map[string]int {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		var n int
		if err := svc.Store.DB.QueryRow(`SELECT COUNT(*) FROM turn_feedback WHERE turn_id=?`, turn).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n >= min || time.Now().After(deadline) {
			break
		}
	}
	got := map[string]int{}
	rows, err := svc.Store.DB.Query(`SELECT source, rating FROM turn_feedback WHERE turn_id=?`, turn)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var src string
		var rating int
		if err := rows.Scan(&src, &rating); err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(src, "implicit:") && rating != 0 {
			t.Fatalf("%s has rating %d: implicit rows carry no direction", src, rating)
		}
		got[src]++
	}
	return got
}

// What a person does right after a reply lands on that reply's turn as implicit rows, apart from the
// ratings people give: the turn shows both, the summary counts both, and neither counts as the other.
func TestImplicitSignalsAreNotedApartFromRatings(t *testing.T) {
	svc := implicitSvc(t)
	t0 := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	svc.Clock = clock.Fixed{T: t0}
	turn := implicitAnswer(t, svc, "u1", "m1")
	svc.NoteReplyMessages(turn, "g", "c", []string{"reply-1"})

	svc.Clock = clock.Fixed{T: t0.Add(90 * time.Second)}
	implicitSay(svc, "u1", "m2", "고마워 ㅋㅋ", "", t0)                // thanks and a laugh
	implicitSay(svc, "u1", "m3", "@마사몽 그게 아니라 다시", "mention", t0) // a recall inside two minutes, and a correction
	implicitSay(svc, "u2", "m4", "고마워", "", t0)                   // someone else: not the person the reply went to
	implicitSay(svc, "u1", "m5", "고마워 고마워", "", t0)               // the same kind again: one row
	svc.Clock = clock.Fixed{T: t0.Add(4 * time.Minute)}
	implicitSay(svc, "u1", "m6", "@마사몽 또", "mention", t0) // after two minutes: not a recall
	svc.Clock = clock.Fixed{T: t0.Add(20 * time.Minute)}
	implicitSay(svc, "u1", "m7", "ㅋㅋㅋ", "", t0) // too late to be about this reply

	want := map[string]int{"implicit:thanks": 1, "implicit:laugh": 1, "implicit:recall": 1, "implicit:correction": 1}
	got := implicitRows(t, svc, turn, 4)
	if len(got) != len(want) {
		t.Fatalf("rows: %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("rows: %v, want %v", got, want)
		}
	}
	// rows hold the one-way reference, never the account
	var raw int
	if err := svc.Store.DB.QueryRow(`SELECT COUNT(*) FROM turn_feedback WHERE user_hash IN ('u1','u2')`).Scan(&raw); err != nil || raw != 0 {
		t.Fatalf("an account id was stored: %d %v", raw, err)
	}
	var mine int
	if err := svc.Store.DB.QueryRow(`SELECT COUNT(*) FROM turn_feedback WHERE user_hash=?`, store.PersonRef("u1")).Scan(&mine); err != nil || mine != 4 {
		t.Fatalf("rows of the person: %d %v", mine, err)
	}

	svc.ReactionFeedback("g", "reply-1", "u3", "👍", true)
	svc.ReactionFeedback("g", "reply-1", "u4", "👎", true)
	view := out(t, svc, "turns.get", map[string]any{"id": turn})["turn"].(map[string]any)
	fb, _ := view["feedback"].(map[string]any)
	imp, _ := view["implicit"].(map[string]any)
	if fb == nil || fb["good"].(float64) != 1 || fb["bad"].(float64) != 1 {
		t.Fatalf("ratings are only what people rated: %v", fb)
	}
	if imp["thanks"].(float64) != 1 || imp["recall"].(float64) != 1 || imp["laugh"].(float64) != 1 || imp["correction"].(float64) != 1 || len(imp) != 4 {
		t.Fatalf("implicit signals on the turn: %v", imp)
	}
	sum := out(t, svc, "feedback.summary", nil)
	if sum["good"].(float64) != 1 || sum["bad"].(float64) != 1 {
		t.Fatalf("the summary counts ratings only: %v", sum)
	}
	if sum["implicit"].(map[string]any)["recall"].(float64) != 1 {
		t.Fatalf("the summary counts implicit signals apart: %v", sum)
	}
	// a turn with only implicit rows shows no rating at all
	svc.Clock = clock.Fixed{T: t0.Add(time.Hour)}
	turn2 := implicitAnswer(t, svc, "u1", "m8")
	svc.Clock = clock.Fixed{T: t0.Add(time.Hour + time.Minute)}
	implicitSay(svc, "u1", "m9", "thanks!", "", t0)
	implicitRows(t, svc, turn2, 1)
	view2 := out(t, svc, "turns.get", map[string]any{"id": turn2})["turn"].(map[string]any)
	if view2["feedback"] != nil || view2["implicit"].(map[string]any)["thanks"].(float64) != 1 {
		t.Fatalf("implicit only: %v", view2)
	}
}

// Nothing is noted for a person who withdrew memory consent or is blocked, and what was noted goes with
// the person when they withdraw.
func TestImplicitSignalsFollowConsentAndBlocking(t *testing.T) {
	svc := implicitSvc(t)
	t0 := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	svc.Clock = clock.Fixed{T: t0}
	if r := exec(svc, "privacy.consent", map[string]any{"user_id": "gone", "scope": "memory", "decision": "withdraw"}); !r.OK {
		t.Fatal(r)
	}
	applyDoc(t, svc, func(d *config.Document) { d.Access.BlockedUsers = []string{"blocked"} })
	turnGone := implicitAnswer(t, svc, "gone", "g1")
	turnBlocked := implicitAnswer(t, svc, "blocked", "b1")
	turnOK := implicitAnswer(t, svc, "kept", "k1")
	svc.Clock = clock.Fixed{T: t0.Add(30 * time.Second)}
	for i, u := range []string{"gone", "blocked", "kept"} {
		implicitSay(svc, u, "s"+string(rune('a'+i)), "고마워", "", t0)
		implicitSay(svc, u, "t"+string(rune('a'+i)), "@마사몽 다시", "mention", t0)
	}
	// the person with consent is the control: once their rows are there, the others' would be too
	if got := implicitRows(t, svc, turnOK, 2); len(got) != 2 {
		t.Fatalf("a person with consent: %v", got)
	}
	time.Sleep(100 * time.Millisecond)
	if got := implicitRows(t, svc, turnGone, 0); len(got) != 0 {
		t.Fatalf("no consent, yet rows were kept: %v", got)
	}
	if got := implicitRows(t, svc, turnBlocked, 0); len(got) != 0 {
		t.Fatalf("blocked, yet rows were kept: %v", got)
	}
	// withdrawing deletes the rows already kept
	if r := exec(svc, "privacy.consent", map[string]any{"user_id": "kept", "scope": "memory", "decision": "withdraw"}); !r.OK {
		t.Fatal(r)
	}
	drain(t, svc)
	if got := implicitRows(t, svc, turnOK, 0); len(got) != 0 {
		t.Fatalf("withdrawal left implicit rows: %v", got)
	}
}

// The decline of a thread offer is noted on the turn that carried the offer; a person without consent
// leaves nothing.
func TestDecliningAThreadOfferIsNotedOnItsTurn(t *testing.T) {
	var plan, resp recordingServer
	planURL := plan.start(t, func(string) string { return `{"memory_requirement":"none","tool_plan":[]}` })
	respURL := resp.start(t, func(string) string { return strings.Repeat("길게 설명하는 답이야. ", 30) })
	svc := routedSvc(t, planURL, respURL)
	svc.Discord = &postGateway{}
	offerTurn := ""
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
			offerTurn = res.TurnID
		}
	}
	if offerTurn == "" {
		t.Fatal("an offer was made")
	}
	svc.DeclineThread("g", "c", "uc")
	if got := implicitRows(t, svc, offerTurn, 1); got["implicit:thread_declined"] != 1 {
		t.Fatalf("rows: %v", got)
	}
}

// Tagging the bot right after an untagged message of theirs was left alone is noted as "retag" on the
// reply the follow-up window belonged to; a tag with no skipped message before it is only a recall.
func TestRetagAfterASkippedFollowUpIsNoted(t *testing.T) {
	svc := implicitSvc(t)
	t0 := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	svc.Clock = clock.Fixed{T: t0}
	turn := implicitAnswer(t, svc, "u1", "m1")
	skipped := application.ObservedMessage{GuildID: "g", ChannelID: "c", MessageID: "m2", AuthorID: "u1", AuthorName: "Chris", Content: "...", At: t0}
	svc.Observe(skipped)
	if svc.FollowUp(context.Background(), skipped) {
		t.Fatal("an empty message is left alone")
	}
	svc.Clock = clock.Fixed{T: t0.Add(20 * time.Second)}
	implicitSay(svc, "u1", "m3", "@마사몽 아니야 다시 말해봐", "mention", t0)
	svc.NoteDirectCall("g", "c", "u1") // the gateway does this right after Observe
	got := implicitRows(t, svc, turn, 2)
	if got["implicit:retag"] != 1 || got["implicit:recall"] != 1 || len(got) != 2 {
		t.Fatalf("rows: %v", got)
	}
	turn2 := implicitAnswer(t, svc, "u1", "m4")
	svc.Clock = clock.Fixed{T: t0.Add(40 * time.Second)}
	implicitSay(svc, "u1", "m5", "@마사몽 또", "mention", t0)
	if got := implicitRows(t, svc, turn2, 1); got["implicit:retag"] != 0 || got["implicit:recall"] != 1 {
		t.Fatalf("no skipped message, no retag: %v", got)
	}
}
