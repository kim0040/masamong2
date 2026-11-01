package application_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"masamong/internal/application"
	"masamong/internal/config"
)

// Collect mode keeps no text, follows the privacy rules, and records what happened next.

type plRow struct {
	ID, Mode, State, Reason, MessageID, Message, Draft, Features string
}

// plRows reads participation_log straight from the database, oldest first.
func plRows(t *testing.T, svc *application.Service) []plRow {
	t.Helper()
	rows, err := svc.Store.DB.Query(`SELECT id, mode, state, reason, message_id, message, draft, features FROM participation_log ORDER BY created_at, rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []plRow
	for rows.Next() {
		var r plRow
		if err := rows.Scan(&r.ID, &r.Mode, &r.State, &r.Reason, &r.MessageID, &r.Message, &r.Draft, &r.Features); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func featuresOf(t *testing.T, r plRow) map[string]any {
	t.Helper()
	var f map[string]any
	if err := json.Unmarshal([]byte(r.Features), &f); err != nil {
		t.Fatalf("features %q: %v", r.Features, err)
	}
	return f
}

// waitRows waits until the log has n rows (the write is asynchronous after a cancellation).
func waitRows(t *testing.T, svc *application.Service, n int) []plRow {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if rows := plRows(t, svc); len(rows) >= n {
			return rows
		}
		if time.Now().After(deadline) {
			t.Fatalf("waited for %d rows, have %+v", n, plRows(t, svc))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func nextNow(t *testing.T, svc *application.Service, id string) string {
	t.Helper()
	var next string
	if err := svc.Store.DB.QueryRow(`SELECT COALESCE(json_extract(features,'$.next'),'') FROM participation_log WHERE id=?`, id).Scan(&next); err != nil {
		t.Fatal(err)
	}
	return next
}

// waitNext waits until a row's features.next is set and returns it.
func waitNext(t *testing.T, svc *application.Service, id string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if next := nextNow(t, svc, id); next != "" {
			return next
		}
		if time.Now().After(deadline) {
			t.Fatalf("features.next of %s never set", id)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitWatching waits for the number of collected rows that still wait for what happens next.
func waitWatching(t *testing.T, svc *application.Service, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for application.CollectWatching(svc) != n {
		if time.Now().After(deadline) {
			t.Fatalf("watches: %d, want %d", application.CollectWatching(svc), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func observe(svc *application.Service, m application.ObservedMessage) {
	m.GuildID, m.ChannelID = "g", "c"
	if m.At.IsZero() {
		m.At = time.Now()
	}
	if m.AuthorName == "" {
		m.AuthorName = m.AuthorID
	}
	svc.Observe(m)
}

func nameCallRoom(t *testing.T, svc *application.Service, js *judgeServer) {
	t.Helper()
	js.answer = `{"to":"about","tone":"neutral"}`
	applyDoc(t, svc, func(d *config.Document) {
		p := d.Guilds["g"].Channels["c"]
		p.NameCall = "addressed"
		d.Guilds["g"].Channels["c"] = p
	})
}

func sayName(svc *application.Service, id, user string) {
	_, _ = svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: id, Message: "마사몽 요즘 똑똑하더라",
		Speaker: user, SpeakerID: user, NameOnly: true})
}

func switchMode(t *testing.T, svc *application.Service, mode string) {
	t.Helper()
	applyDoc(t, svc, func(d *config.Document) {
		d.Participation.Mode = mode
		p := d.Guilds["g"].Channels["c"]
		p.Participation = mode
		d.Guilds["g"].Channels["c"] = p
	})
}

// A collected row holds the source message by ID alone: no text, no draft, and no word of the message in its
// features; a moment people handled meanwhile is the same, and says what they did.
func TestCollectedRowsHoldNoMessageText(t *testing.T) {
	svc, _, js := partSvc(t, "collect")
	say(svc, "m1", "u1", "누가 텐트 챙기기로 했지? 비밀 메모")
	waitRows(t, svc, 1)
	application.SetParticipationWait(svc, 300*time.Millisecond)
	say(svc, "m2", "u1", "누가 보드게임 가져오기로 했지? 둘째 비밀")
	say(svc, "m3", "u2", "내가 가져갈게")
	rows := waitRows(t, svc, 2)
	byID := map[string]plRow{}
	for _, r := range rows {
		byID[r.MessageID] = r
	}
	collected, cancelled := byID["m1"], byID["m2"]
	if collected.State != "collected" || cancelled.State != "cancelled" || cancelled.Reason != "people_answered" {
		t.Fatalf("rows: %+v", rows)
	}
	for _, r := range rows {
		if r.Mode != "collect" || r.Message != "" || r.Draft != "" {
			t.Fatalf("row keeps text: %+v", r)
		}
		if strings.Contains(r.Features, "비밀") || strings.Contains(r.Features, "텐트") || strings.Contains(r.Features, "보드게임") {
			t.Fatalf("features carry the text: %s", r.Features)
		}
	}
	f := featuresOf(t, collected)
	// u1 alone had spoken, in Korean, and the bot never had in this room
	if f["participants"] != float64(1) || f["lang"] != "ko" || f["kind"] != "question" {
		t.Fatalf("features: %s", collected.Features)
	}
	if h, ok := f["hour"].(float64); !ok || h < 0 || h > 23 {
		t.Fatalf("hour: %s", collected.Features)
	}
	if _, ok := f["msgs_per_min"].(float64); !ok {
		t.Fatalf("msgs_per_min: %s", collected.Features)
	}
	if _, ok := f["since_bot_min"]; ok {
		t.Fatalf("the bot has not spoken here: %s", collected.Features)
	}
	// the people who answered while it waited are what happened next
	if got := featuresOf(t, cancelled)["next"]; got != "answered" {
		t.Fatalf("cancelled next: %v", got)
	}
	// the console log carries no text either, and names what happened next
	r := exec(svc, "participation.mode", map[string]any{"guild_id": "g", "channel_id": "c"})
	for _, e := range r.Output["log"].([]map[string]any) {
		if e["message"] != "" || e["draft"] != "" {
			t.Fatalf("log entry keeps text: %+v", e)
		}
		if e["message_id"] == "m2" && e["next"] != "answered" {
			t.Fatalf("log next: %+v", e)
		}
	}
	if js.judged.Load() != 0 {
		t.Fatal("collect called the judge")
	}
}

// Collect records only where capture is on and for people who agreed, and says why it did not.
func TestCollectSkipsWhatIsNotAllowed(t *testing.T) {
	cases := []struct {
		name, reason string
		setup        func(t *testing.T, svc *application.Service)
	}{
		{"channel capture off", "capture_off", func(t *testing.T, svc *application.Service) {
			applyDoc(t, svc, func(d *config.Document) {
				p := d.Guilds["g"].Channels["c"]
				p.Capture = false
				d.Guilds["g"].Channels["c"] = p
			})
		}},
		{"instance capture off", "capture_off", func(t *testing.T, svc *application.Service) {
			applyDoc(t, svc, func(d *config.Document) { d.Memory.Capture = false })
		}},
		{"participation consent withdrawn", "opted_out", func(t *testing.T, svc *application.Service) {
			if r := as(svc, "privacy.consent", map[string]any{"scope": "participation", "decision": "withdraw"}, inChannel("u1")); !r.OK {
				t.Fatal(r.Error)
			}
		}},
		{"memory consent withdrawn", "opted_out", func(t *testing.T, svc *application.Service) {
			if r := as(svc, "privacy.consent", map[string]any{"scope": "memory", "decision": "withdraw"}, inChannel("u1")); !r.OK {
				t.Fatal(r.Error)
			}
		}},
		{"blocked", "blocked", func(t *testing.T, svc *application.Service) {
			applyDoc(t, svc, func(d *config.Document) {
				g := d.Guilds["g"]
				g.BlockedUsers = []string{"u1"}
				d.Guilds["g"] = g
			})
		}},
		{"paused", "paused", func(t *testing.T, svc *application.Service) {
			if r := exec(svc, "runtime.pause", map[string]any{"scope": "channel", "guild_id": "g", "channel_id": "c", "paused": true}); !r.OK {
				t.Fatal(r.Error)
			}
		}},
	}
	filteredOf := func(svc *application.Service) map[string]int {
		r := exec(svc, "participation.mode", map[string]any{"guild_id": "g", "channel_id": "c"})
		out, _ := r.Output["filtered"].(map[string]int)
		return out
	}
	for _, tc := range cases {
		t.Run(tc.name+"/question", func(t *testing.T) {
			svc, _, _ := partSvc(t, "collect")
			tc.setup(t, svc)
			say(svc, "m1", "u1", "누가 텐트 챙기기로 했지?")
			time.Sleep(200 * time.Millisecond)
			if rows := plRows(t, svc); len(rows) != 0 {
				t.Fatalf("recorded: %+v", rows)
			}
			if got := filteredOf(svc)[tc.reason]; got != 1 {
				t.Fatalf("filtered %s: %v", tc.reason, filteredOf(svc))
			}
		})
		t.Run(tc.name+"/name mention", func(t *testing.T) {
			svc, _, js := partSvc(t, "collect")
			nameCallRoom(t, svc, js)
			tc.setup(t, svc)
			sayName(svc, "m9", "u1")
			time.Sleep(100 * time.Millisecond)
			if rows := plRows(t, svc); len(rows) != 0 {
				t.Fatalf("recorded: %+v", rows)
			}
			// a paused channel never reaches the name-call check, so nothing is counted there
			if got := filteredOf(svc)[tc.reason]; got != 1 && tc.reason != "paused" {
				t.Fatalf("filtered %s: %v", tc.reason, filteredOf(svc))
			}
		})
	}
}

// Quiet hours stop the bot speaking; collect says nothing, so it keeps recording through them, in both paths.
func TestCollectIgnoresQuietHours(t *testing.T) {
	quiet := func(d *config.Document) {
		loc := time.UTC
		if l, err := time.LoadLocation(d.I18n.Timezone); err == nil {
			loc = l
		}
		now := time.Now().In(loc) // quiet hours are read in the instance's zone
		d.Participation.QuietStart = now.Add(-time.Hour).Format("15:04")
		d.Participation.QuietEnd = now.Add(time.Hour).Format("15:04")
	}
	// the control: shadow is held back by the same window
	svc, _, js := partSvc(t, "shadow")
	applyDoc(t, svc, quiet)
	say(svc, "m1", "u1", "누가 텐트 챙기기로 했지?")
	time.Sleep(200 * time.Millisecond)
	if len(plRows(t, svc)) != 0 || js.judged.Load() != 0 {
		t.Fatal("shadow must stay quiet in quiet hours")
	}

	svc, _, js = partSvc(t, "collect")
	nameCallRoom(t, svc, js)
	applyDoc(t, svc, quiet)
	say(svc, "m1", "u1", "누가 텐트 챙기기로 했지?")
	rows := waitRows(t, svc, 1)
	if rows[0].State != "collected" || rows[0].MessageID != "m1" {
		t.Fatalf("question in quiet hours: %+v", rows)
	}
	sayName(svc, "m2", "u1")
	rows = waitRows(t, svc, 2)
	if rows[1].Reason != "bot_mentioned" || rows[1].MessageID != "m2" {
		t.Fatalf("mention in quiet hours: %+v", rows)
	}
}

// Where no name-call check runs (name_call off, or a room that answers everything), a message that says the
// bot's name without calling it is recorded by the observer; with name_call addressed only the check does.
func TestCollectRecordsNamedBotWhereNoCheckRuns(t *testing.T) {
	for _, tc := range []struct {
		nameCall, respond string
		want              int
	}{{"off", "mention", 1}, {"always", "mention", 1}, {"addressed", "always", 1}, {"addressed", "mention", 0}} {
		svc, _, js := partSvc(t, "collect")
		applyDoc(t, svc, func(d *config.Document) {
			p := d.Guilds["g"].Channels["c"]
			p.NameCall, p.Respond = tc.nameCall, tc.respond
			d.Guilds["g"].Channels["c"] = p
		})
		observe(svc, application.ObservedMessage{MessageID: "m1", AuthorID: "u1", Content: "마사몽 오늘 좀 웃기네 ㅋㅋ", BotCall: "name", Addressed: tc.respond == "always"})
		time.Sleep(200 * time.Millisecond)
		rows := plRows(t, svc)
		if len(rows) != tc.want {
			t.Fatalf("name_call %s, respond %s: %+v", tc.nameCall, tc.respond, rows)
		}
		if tc.want == 1 && (rows[0].Reason != "bot_mentioned" || rows[0].Message != "" || rows[0].MessageID != "m1") {
			t.Fatalf("name_call %s: %+v", tc.nameCall, rows[0])
		}
		if js.judged.Load() != 0 {
			t.Fatal("the judge was called")
		}
	}
	// a mention or a reply to the bot is a call, not a name said
	svc, _, _ := partSvc(t, "collect")
	observe(svc, application.ObservedMessage{MessageID: "m1", AuthorID: "u1", Content: "안녕", BotCall: "mention"})
	time.Sleep(150 * time.Millisecond)
	if rows := plRows(t, svc); len(rows) != 0 {
		t.Fatalf("a call was recorded as a name: %+v", rows)
	}
}

// features.next is set once, by the first of: the bot called, the source replied to by someone else, the same
// author speaking again; and a quiet window ends as silent.
func TestCollectRecordsWhatHappenedNext(t *testing.T) {
	newCollect := func(t *testing.T) (*application.Service, *stepClock, func(), func()) {
		t.Helper()
		svc, _, _ := partSvc(t, "collect")
		finishWait, drain := application.ManualParticipationForTest(svc)
		clk := &stepClock{t: time.Now()}
		svc.Clock = clk
		return svc, clk, func() {
			t.Helper()
			if !finishWait("g", "c") {
				t.Fatal("candidate missing")
			}
		}, drain
	}
	cases := []struct {
		name           string
		then           []application.ObservedMessage
		want           string
		collectBetween bool // finish the second question's quiet wait before the third message
	}{
		{"reply by someone else", []application.ObservedMessage{{MessageID: "m2", AuthorID: "u2", Content: "나야", ReplyTo: "m1"}}, "answered", false},
		{"same author again", []application.ObservedMessage{{MessageID: "m2", AuthorID: "u1", Content: "아무도 없나"}}, "continued", false},
		{"bot called", []application.ObservedMessage{{MessageID: "m2", AuthorID: "u2", Content: "마사몽 알아?", BotCall: "mention", Addressed: true}}, "called_bot", false}, // a mention is addressed, as the gateway marks it
		{"the bot's name said is not a call", []application.ObservedMessage{{MessageID: "m2", AuthorID: "u1", Content: "마사몽 또 고장났네", BotCall: "name"}}, "continued", false},
		{"first event wins", []application.ObservedMessage{{MessageID: "m2", AuthorID: "u1", Content: "음"}, {MessageID: "m3", AuthorID: "u2", Content: "나야", ReplyTo: "m1"}}, "continued", false},
		{"another person talking, no reply", []application.ObservedMessage{{MessageID: "m2", AuthorID: "u2", Content: "점심 뭐 먹지"}, {MessageID: "m3", AuthorID: "u1", Content: "음"}}, "continued", false},
		{"another person talking, no reply after quiet wait", []application.ObservedMessage{{MessageID: "m2", AuthorID: "u2", Content: "점심 뭐 먹지"}, {MessageID: "m3", AuthorID: "u1", Content: "음"}}, "continued", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, clk, finishWait, drain := newCollect(t)
			say(svc, "m1", "u1", "누가 텐트 챙기기로 했지?")
			finishWait()
			row := plRows(t, svc)[0]
			if !application.CollectWatchingRowForTest(svc, row.ID) {
				t.Fatal("source row is not being followed")
			}
			for i, m := range tc.then {
				observe(svc, m)
				if tc.collectBetween && i == 0 {
					finishWait()
				}
			}
			drain()
			if got := nextNow(t, svc, row.ID); got != tc.want {
				t.Fatalf("next = %q, want %q", got, tc.want)
			}
			if application.CollectWatchingRowForTest(svc, row.ID) {
				t.Fatal("completed source row is still being followed")
			}
			// A second question can finish its quiet wait on a slow machine before the third message.
			// The third message ends the first author's watch, but is no answer to the second question.
			wantWatches := 0
			if tc.collectBetween {
				wantWatches = 1
			}
			if got := application.CollectWatching(svc); got != wantWatches {
				t.Fatalf("watches: %d, want %d", got, wantWatches)
			}
			if tc.collectBetween {
				second := plRows(t, svc)[1]
				if second.MessageID != "m2" || second.State != "collected" || nextNow(t, svc, second.ID) != "" || !application.CollectWatchingRowForTest(svc, second.ID) {
					t.Fatal("the second question should still be followed")
				}
				clk.Advance(10 * time.Minute)
				application.SweepCollectNext(svc)
				if got := nextNow(t, svc, second.ID); got != "silent" || application.CollectWatchingRowForTest(svc, second.ID) {
					t.Fatalf("second question after expiry: next = %q", got)
				}
				if got := application.CollectWatching(svc); got != 0 {
					t.Fatalf("watches after expiry: %d", got)
				}
			}
			observe(svc, application.ObservedMessage{MessageID: "m9", AuthorID: "u2", Content: "마사몽", BotCall: "name"})
			drain()
			if got := nextNow(t, svc, row.ID); got != tc.want {
				t.Fatalf("next changed to %q", got)
			}
			if r := plRows(t, svc)[0]; r.Message != "" || strings.Contains(r.Features, "점심") || strings.Contains(r.Features, "아무도") {
				t.Fatalf("text in row: %+v", r)
			}
		})
	}

	t.Run("nobody does anything", func(t *testing.T) {
		svc, clk, finishWait, _ := newCollect(t)
		application.SetCollectNextWindow(svc, 200*time.Millisecond)
		say(svc, "m1", "u1", "누가 텐트 챙기기로 했지?")
		finishWait()
		row := plRows(t, svc)[0]
		application.SweepCollectNext(svc) // inside the window: nothing yet
		if got := nextNow(t, svc, row.ID); got != "" || application.CollectWatching(svc) != 1 {
			t.Fatalf("swept inside the window: next %q", got)
		}
		clk.Advance(200 * time.Millisecond)
		application.SweepCollectNext(svc)
		if got := nextNow(t, svc, row.ID); got != "silent" {
			t.Fatalf("next = %q", got)
		}
		if got := application.CollectWatching(svc); got != 0 {
			t.Fatalf("watches after expiry: %d", got)
		}
	})

	t.Run("a message after the window is not an answer", func(t *testing.T) {
		svc, clk, finishWait, drain := newCollect(t)
		application.SetCollectNextWindow(svc, 30*time.Millisecond)
		say(svc, "m1", "u1", "누가 텐트 챙기기로 했지?")
		finishWait()
		row := plRows(t, svc)[0]
		clk.Advance(80 * time.Millisecond)
		observe(svc, application.ObservedMessage{MessageID: "m2", AuthorID: "u2", Content: "나야", ReplyTo: "m1"})
		drain()
		if got := nextNow(t, svc, row.ID); got != "silent" {
			t.Fatalf("next = %q", got)
		}
	})

	t.Run("people who withdrew are not counted", func(t *testing.T) {
		svc, _, finishWait, drain := newCollect(t)
		if r := as(svc, "privacy.consent", map[string]any{"scope": "participation", "decision": "withdraw"}, inChannel("u2")); !r.OK {
			t.Fatal(r.Error)
		}
		say(svc, "m1", "u1", "누가 텐트 챙기기로 했지?")
		finishWait()
		row := plRows(t, svc)[0]
		observe(svc, application.ObservedMessage{MessageID: "m2", AuthorID: "u2", Content: "나야", ReplyTo: "m1"})
		drain()
		if got := nextNow(t, svc, row.ID); got != "" {
			t.Fatalf("a withdrawn person's reply was recorded: %q", got)
		}
		observe(svc, application.ObservedMessage{MessageID: "m3", AuthorID: "u1", Content: "음"})
		drain()
		if got := nextNow(t, svc, row.ID); got != "continued" {
			t.Fatalf("next = %q", got)
		}
	})

	t.Run("a mention row is followed too", func(t *testing.T) {
		svc, _, js := partSvc(t, "collect")
		_, drain := application.ManualParticipationForTest(svc)
		nameCallRoom(t, svc, js)
		sayName(svc, "m1", "u1")
		drain()
		row := plRows(t, svc)[0]
		observe(svc, application.ObservedMessage{MessageID: "m2", AuthorID: "u2", Content: "맞아", ReplyTo: "m1"})
		drain()
		if got := nextNow(t, svc, row.ID); got != "answered" {
			t.Fatalf("next = %q", got)
		}
	})

	t.Run("the same person going on while it waits", func(t *testing.T) {
		svc, _, finishWait, _ := newCollect(t)
		say(svc, "m1", "u1", "누가 텐트 챙기기로 했지?")
		observe(svc, application.ObservedMessage{MessageID: "m2", AuthorID: "u1", Content: "음"})
		finishWait()
		row := plRows(t, svc)[0]
		if row.State != "collected" || featuresOf(t, row)["next"] != "continued" {
			t.Fatalf("row: %+v", row)
		}
	})
}

// The features say how long ago the bot spoke, when it did.
func TestCollectFeaturesSayWhenTheBotLastSpoke(t *testing.T) {
	svc, _, _ := partSvc(t, "collect")
	observe(svc, application.ObservedMessage{MessageID: "b1", AuthorID: "bot", Content: "안녕하세요", FromBot: true, At: time.Now().Add(-3*time.Minute - 10*time.Second)})
	say(svc, "m1", "u1", "누가 텐트 챙기기로 했지?")
	rows := waitRows(t, svc, 1)
	f := featuresOf(t, rows[0])
	if f["since_bot_min"] != float64(3) || f["participants"] != float64(1) {
		t.Fatalf("features: %s", rows[0].Features)
	}
	if rate, _ := f["msgs_per_min"].(float64); rate != 0.1 { // one person's message when it was asked: 1 in 10 minutes
		t.Fatalf("msgs_per_min: %s", rows[0].Features)
	}
}

// Collected rows go with their source message, when the person is forgotten and when they withdraw
// participation consent; rows of other modes keep their rows and lose only the text.
func TestCollectedRowsAreDeleted(t *testing.T) {
	twoRows := func(t *testing.T) *application.Service {
		svc, _, _ := partSvc(t, "collect")
		say(svc, "m1", "u1", "누가 텐트 챙기기로 했지?")
		waitRows(t, svc, 1)
		say(svc, "m2", "u2", "누가 보드게임 가져오기로 했지?")
		waitRows(t, svc, 2)
		return svc
	}
	remaining := func(svc *application.Service) []string {
		var ids []string
		for _, r := range plRows(t, svc) {
			ids = append(ids, r.MessageID)
		}
		return ids
	}
	onlySecond := func(t *testing.T, svc *application.Service) {
		t.Helper()
		if got := remaining(svc); len(got) != 1 || got[0] != "m2" {
			t.Fatalf("rows left: %v", got)
		}
	}
	t.Run("source deleted", func(t *testing.T) {
		svc := twoRows(t)
		svc.MessagesDeleted("g", "c", []string{"m1"})
		svc.DrainRedactions(context.Background())
		onlySecond(t, svc)
	})
	t.Run("person forgotten", func(t *testing.T) {
		svc := twoRows(t)
		if r := as(svc, "privacy.consent", map[string]any{"scope": "memory", "decision": "withdraw"}, inChannel("u1")); !r.OK {
			t.Fatal(r.Error)
		}
		svc.DrainRedactions(context.Background())
		onlySecond(t, svc)
	})
	t.Run("participation consent withdrawn", func(t *testing.T) {
		svc := twoRows(t)
		if r := as(svc, "privacy.consent", map[string]any{"scope": "participation", "decision": "withdraw"}, inChannel("u1")); !r.OK {
			t.Fatal(r.Error)
		}
		onlySecond(t, svc)
		// and nothing new is recorded about them
		say(svc, "m3", "u1", "누가 간식 사 오기로 했지?")
		time.Sleep(150 * time.Millisecond)
		onlySecond(t, svc)
	})
	t.Run("other modes keep the row and lose the text", func(t *testing.T) {
		svc, _, _ := partSvc(t, "shadow")
		say(svc, "m1", "u1", "누가 텐트 챙기기로 했지? 비밀 메모")
		before := waitRows(t, svc, 1)
		if before[0].Message == "" || before[0].Mode != "shadow" {
			t.Fatalf("shadow row: %+v", before[0])
		}
		svc.MessagesDeleted("g", "c", []string{"m1"})
		svc.DrainRedactions(context.Background())
		after := plRows(t, svc)
		if len(after) != 1 || after[0].Message != "" || after[0].Draft != "" || after[0].State != "shadow" {
			t.Fatalf("shadow row after deletion: %+v", after)
		}
	})
}

// Only the labels on what the bot decided to say count toward the move to conservative; the labels on
// collected moments are reported apart.
func TestCollectedLabelsStayOutOfTheGate(t *testing.T) {
	svc, _, _ := partSvc(t, "collect")
	say(svc, "m1", "u1", "누가 텐트 챙기기로 했지?")
	collected := waitRows(t, svc, 1)[0]
	if r := exec(svc, "participation.mode", map[string]any{"review": collected.ID, "verdict": "good"}); !r.OK {
		t.Fatal(r.Error)
	}
	reviewed := func() map[string]int {
		r := exec(svc, "participation.mode", map[string]any{"guild_id": "g", "channel_id": "c"})
		return r.Output["reviewed"].(map[string]int)
	}
	if got := reviewed(); got["good"] != 0 || got["bad"] != 0 || got["collected_good"] != 1 {
		t.Fatalf("reviewed after a collected label: %v", got)
	}
	switchMode(t, svc, "shadow")
	say(svc, "m2", "u1", "누가 보드게임 가져오기로 했지?")
	rows := waitRows(t, svc, 2)
	if rows[1].State != "shadow" {
		t.Fatalf("rows: %+v", rows)
	}
	if r := exec(svc, "participation.mode", map[string]any{"review": rows[1].ID, "verdict": "bad"}); !r.OK {
		t.Fatal(r.Error)
	}
	if got := reviewed(); got["good"] != 0 || got["bad"] != 1 || got["collected_good"] != 1 || got["collected_bad"] != 0 {
		t.Fatalf("reviewed: %v", got)
	}
}

// A mode change while a moment waits, or while a collected row waits for what comes next.
func TestCollectModeSwitchWhilePending(t *testing.T) {
	// collect -> shadow: the moment is judged like any shadow candidate and logged as one
	svc, _, js := partSvc(t, "collect")
	application.SetParticipationWait(svc, 250*time.Millisecond)
	say(svc, "m1", "u1", "누가 텐트 챙기기로 했지?")
	switchMode(t, svc, "shadow")
	rows := waitRows(t, svc, 1)
	if rows[0].State != "shadow" || rows[0].Mode != "shadow" || js.judged.Load() != 1 {
		t.Fatalf("after collect -> shadow: %+v judged=%d", rows, js.judged.Load())
	}

	// collect -> off: nothing is recorded
	svc, _, js = partSvc(t, "collect")
	application.SetParticipationWait(svc, 150*time.Millisecond)
	say(svc, "m1", "u1", "누가 텐트 챙기기로 했지?")
	applyDoc(t, svc, func(d *config.Document) { d.Participation.Enabled = false })
	time.Sleep(350 * time.Millisecond)
	if rows := plRows(t, svc); len(rows) != 0 || js.judged.Load() != 0 {
		t.Fatalf("after collect -> off: %+v", rows)
	}

	// collect -> conservative right after the bot spoke: the moment, which no cooldown was checked for,
	// is held to the cooldown now and not said
	svc, gw, js := partSvc(t, "conservative")
	say(svc, "m1", "u1", "누가 텐트 챙기기로 했지?")
	partLog(t, svc, 1)
	if gw.postCount() != 1 {
		t.Fatalf("posts: %d", gw.postCount())
	}
	switchMode(t, svc, "collect")
	application.SetParticipationWait(svc, 250*time.Millisecond)
	say(svc, "m2", "u2", "누가 보드게임 가져오기로 했지?")
	switchMode(t, svc, "conservative")
	rows = waitRows(t, svc, 2)
	last := rows[len(rows)-1]
	if last.MessageID != "m2" || last.State != "cancelled" || last.Reason != "cooldown" || last.Mode != "conservative" {
		t.Fatalf("after collect -> conservative within the cooldown: %+v", rows)
	}
	if gw.postCount() != 1 || js.judged.Load() != 1 {
		t.Fatalf("posts %d, judged %d", gw.postCount(), js.judged.Load())
	}

	// a collected row waiting for what comes next stops being followed when collect ends
	svc, _, _ = partSvc(t, "collect")
	say(svc, "m1", "u1", "누가 텐트 챙기기로 했지?")
	row := waitRows(t, svc, 1)[0]
	waitWatching(t, svc, 1)
	switchMode(t, svc, "shadow")
	observe(svc, application.ObservedMessage{MessageID: "m2", AuthorID: "u1", Content: "음"})
	time.Sleep(150 * time.Millisecond)
	if got := nextNow(t, svc, row.ID); got != "" {
		t.Fatalf("next recorded after the mode left collect: %q", got)
	}
}

// A message deleted while its moment waits leaves no collected row, whether the deletion came through the
// service (the candidate is dropped) or only as a tombstone, and a late mention of a deleted message is not
// recorded either.
func TestCollectDropsMomentsOfDeletedMessages(t *testing.T) {
	t.Run("deleted while it waits", func(t *testing.T) {
		svc, _, _ := partSvc(t, "collect")
		application.SetParticipationWait(svc, 250*time.Millisecond)
		say(svc, "m1", "u1", "누가 텐트 챙기기로 했지?")
		if err := svc.MessagesDeleted("g", "c", []string{"m1"}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(450 * time.Millisecond)
		if rows := plRows(t, svc); len(rows) != 0 {
			t.Fatalf("a deleted message was collected: %+v", rows)
		}
		if n := application.CollectWatching(svc); n != 0 {
			t.Fatalf("watches: %d", n)
		}
	})
	t.Run("only the tombstone exists when the wait ends", func(t *testing.T) {
		svc, _, _ := partSvc(t, "collect")
		application.SetParticipationWait(svc, 250*time.Millisecond)
		say(svc, "m1", "u1", "누가 텐트 챙기기로 했지?")
		if _, err := svc.Store.DB.Exec(`INSERT INTO privacy_tombstones(kind,source_id,at) VALUES('message','m1',?)`, time.Now().UTC().Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(450 * time.Millisecond)
		if rows := plRows(t, svc); len(rows) != 0 {
			t.Fatalf("a deleted message was collected: %+v", rows)
		}
	})
	t.Run("a moment still waiting for the follow-up is dropped", func(t *testing.T) {
		svc, _, _ := partSvc(t, "collect")
		say(svc, "m1", "u1", "누가 텐트 챙기기로 했지?")
		waitRows(t, svc, 1)
		waitWatching(t, svc, 1)
		if err := svc.MessagesDeleted("g", "c", []string{"m1"}); err != nil {
			t.Fatal(err)
		}
		waitWatching(t, svc, 0)
	})
	t.Run("a name said in a message that is already gone", func(t *testing.T) {
		svc, _, _ := partSvc(t, "collect")
		if err := svc.MessagesDeleted("g", "c", []string{"m1"}); err != nil {
			t.Fatal(err)
		}
		observe(svc, application.ObservedMessage{MessageID: "m1", AuthorID: "u1", Content: "마사몽 오늘 좀 웃기네 ㅋㅋ", BotCall: "name"})
		time.Sleep(200 * time.Millisecond)
		if rows := plRows(t, svc); len(rows) != 0 {
			t.Fatalf("recorded: %+v", rows)
		}
	})
}

// Withdrawing participation consent while a moment waits leaves nothing about the person: no collected row
// and no cancelled one either.
func TestCollectWithdrawnWhileWaitingLeavesNoRow(t *testing.T) {
	svc, _, _ := partSvc(t, "collect")
	application.SetParticipationWait(svc, 300*time.Millisecond)
	say(svc, "m1", "u1", "누가 텐트 챙기기로 했지?")
	if r := as(svc, "privacy.consent", map[string]any{"scope": "participation", "decision": "withdraw"}, inChannel("u1")); !r.OK {
		t.Fatal(r.Error)
	}
	say(svc, "m2", "u2", "내가 가져갈게") // would cancel the moment as handled
	time.Sleep(500 * time.Millisecond)
	if rows := plRows(t, svc); len(rows) != 0 {
		t.Fatalf("rows about a person who withdrew: %+v", rows)
	}
}

// Talk about the bot (only its name) is not a call: features.next says called_bot for a mention or a reply to it.
func TestCollectNameSaidIsNotACallInPendingMoments(t *testing.T) {
	for _, tc := range []struct{ call, want string }{{"name", "answered"}, {"mention", "called_bot"}} {
		svc, _, _ := partSvc(t, "collect")
		application.SetParticipationWait(svc, 300*time.Millisecond)
		say(svc, "m1", "u1", "누가 텐트 챙기기로 했지?")
		observe(svc, application.ObservedMessage{MessageID: "m2", AuthorID: "u2", Content: "마사몽 알아?", BotCall: tc.call})
		rows := waitRows(t, svc, 1)
		if got := featuresOf(t, rows[0])["next"]; got != tc.want {
			t.Fatalf("%s: next = %v, want %s", tc.call, got, tc.want)
		}
	}
	for _, tc := range []struct{ call, want string }{{"name", "continued"}, {"mention", "called_bot"}} {
		svc, _, _ := partSvc(t, "collect")
		application.SetParticipationWait(svc, 300*time.Millisecond)
		say(svc, "m1", "u1", "누가 텐트 챙기기로 했지?")
		observe(svc, application.ObservedMessage{MessageID: "m2", AuthorID: "u1", Content: "마사몽 또 고장났네", BotCall: tc.call})
		rows := waitRows(t, svc, 1)
		if got := featuresOf(t, rows[0])["next"]; got != tc.want {
			t.Fatalf("same person, %s: next = %v, want %s", tc.call, got, tc.want)
		}
	}
}

// An edit changes no collected row (it holds no text); a deletion removes it.
func TestCollectedRowsSurviveAnEdit(t *testing.T) {
	svc, _, _ := partSvc(t, "collect")
	say(svc, "m1", "u1", "누가 텐트 챙기기로 했지?")
	waitRows(t, svc, 1)
	say(svc, "m2", "u2", "누가 보드게임 가져오기로 했지?")
	waitRows(t, svc, 2)
	if err := svc.MessageEdited("g", "c", "m1", "다른 말로 고침", time.Now()); err != nil {
		t.Fatal(err)
	}
	svc.DrainRedactions(context.Background())
	if rows := plRows(t, svc); len(rows) != 2 {
		t.Fatalf("an edit removed a collected row: %+v", rows)
	}
	if err := svc.MessagesDeleted("g", "c", []string{"m1"}); err != nil {
		t.Fatal(err)
	}
	svc.DrainRedactions(context.Background())
	rows := plRows(t, svc)
	if len(rows) != 1 || rows[0].MessageID != "m2" {
		t.Fatalf("rows after the deletion: %+v", rows)
	}
}
