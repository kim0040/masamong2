package application_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/clock"
	"masamong/internal/config"
	"masamong/internal/tools"
)

// hookGateway is a gateway whose DM send can run code before and after the send (a cancel made while
// the send is going, a crash between the lease and the send, or right after it).
type hookGateway struct {
	*postGateway
	before      func()
	crashBefore bool // the process stops before anything is posted
	crashAfter  bool // the process stops after the message was posted
}

func (g *hookGateway) SendDM(ctx context.Context, user string, c action.Card, img string) error {
	if g.before != nil {
		g.before()
	}
	if g.crashBefore {
		panic("simulated stop before the send")
	}
	err := g.postGateway.SendDM(ctx, user, c, img)
	if g.crashAfter {
		panic("simulated stop after the send")
	}
	return err
}

func deliverCrashing(svc *application.Service) (crashed bool) {
	defer func() { crashed = recover() != nil }()
	application.DeliverReminders(svc, context.Background())
	return false
}

func repSeoulNow() (time.Time, *time.Location) {
	seoul, _ := time.LoadLocation("Asia/Seoul")
	return time.Date(2026, 9, 28, 10, 0, 0, 0, seoul), seoul // a Monday
}

// setSeries asks for a repeating reminder and confirms it to the person's DMs.
func setSeries(t *testing.T, svc *application.Service, user string, in map[string]any) string {
	t.Helper()
	inv := inChannel(user)
	r := as(svc, "reminders.propose", in, inv)
	if !r.OK || r.Output["status"] != "proposed" {
		t.Fatalf("propose %v: %+v", in, r.Output)
	}
	id := reminderID(t, r)
	if c := as(svc, "reminders.update", map[string]any{"op": "confirm", "id": id, "destination": "dm"}, inv); c.Output["status"] != "scheduled" {
		t.Fatalf("confirm: %+v", c.Output)
	}
	return id
}

func remRow(t *testing.T, svc *application.Service, id string) map[string]any {
	t.Helper()
	for _, m := range consoleReminders(t, svc) {
		if m["id"] == id {
			return m
		}
	}
	t.Fatalf("no reminder %s", id)
	return nil
}

func remDue(t *testing.T, svc *application.Service, id string) string {
	t.Helper()
	d, _ := remRow(t, svc, id)["due_at"].(string)
	return d
}

func remStatus(t *testing.T, svc *application.Service, id string) string {
	t.Helper()
	s, _ := remRow(t, svc, id)["status"].(string)
	return s
}

func remAt(svc *application.Service, t time.Time) { svc.Clock = clock.Fixed{T: t} }

func TestRepeatingDailyIsSentOncePerOccurrenceAndStatesTheRepetition(t *testing.T) {
	now, seoul := repSeoulNow()
	svc, gw := remindSvc(t, now)
	inv := inChannel("u1")
	r := as(svc, "reminders.propose", map[string]any{"when": "8am", "what": "take the pill", "repeat": "daily"}, inv)
	if r.Output["repeat_text"] != "Every day at 08:00" {
		t.Fatalf("proposal says: %+v", r.Output)
	}
	c := card(t, r)
	if len(c.Fields) != 2 || c.Fields[1].Value != "Every day at 08:00" {
		t.Fatalf("proposal card does not state the repetition: %+v", c.Fields)
	}
	id := reminderID(t, r)
	set := as(svc, "reminders.update", map[string]any{"op": "confirm", "id": id, "destination": "dm"}, inv)
	if sc := card(t, set); len(sc.Fields) != 3 || sc.Fields[2].Value != "Every day at 08:00" {
		t.Fatalf("set card: %+v", sc.Fields)
	}
	if got := remDue(t, svc, id); got != "2026-09-28T23:00:00Z" { // tomorrow 08:00 in Seoul
		t.Fatalf("first occurrence %s", got)
	}
	remAt(svc, time.Date(2026, 9, 29, 8, 0, 30, 0, seoul))
	application.DeliverReminders(svc, context.Background())
	application.DeliverReminders(svc, context.Background())
	if gw.dmCount() != 1 || remStatus(t, svc, id) != "scheduled" || remDue(t, svc, id) != "2026-09-29T23:00:00Z" {
		t.Fatalf("after day 1: %d sent, %s, due %s", gw.dmCount(), remStatus(t, svc, id), remDue(t, svc, id))
	}
	if f := gw.sent[0].Fields; len(f) != 1 || f[0].Value != "Every day at 08:00" {
		t.Fatalf("the delivery does not say it repeats: %+v", f)
	}
	remAt(svc, time.Date(2026, 9, 30, 8, 0, 10, 0, seoul))
	application.DeliverReminders(svc, context.Background())
	if gw.dmCount() != 2 || remRow(t, svc, id)["occurrences"].(float64) != 2 {
		t.Fatalf("after day 2: %d sent, %+v", gw.dmCount(), remRow(t, svc, id))
	}
}

func TestRepeatingWeeklyMonthlyAndHoursPickTheirFirstOccurrence(t *testing.T) {
	now, seoul := repSeoulNow()
	svc, gw := remindSvc(t, now)
	// Monday 10:00 asked: "Mon and Wed at 9am" starts at Wednesday (the time given is tomorrow 9am)
	weekly := setSeries(t, svc, "u1", map[string]any{"when": "9am", "what": "standup", "repeat": "weekly", "weekdays": "mon,wed"})
	if got := remDue(t, svc, weekly); got != "2026-09-30T00:00:00Z" {
		t.Fatalf("weekly first: %s", got)
	}
	// day 31 in a 30-day month is its last day
	monthly := setSeries(t, svc, "u2", map[string]any{"when": "10am", "what": "rent", "repeat": "monthly", "day": 31})
	if got := remDue(t, svc, monthly); got != "2026-09-30T01:00:00Z" {
		t.Fatalf("monthly first: %s", got)
	}
	hours := setSeries(t, svc, "u3", map[string]any{"when": "in 1 hour", "what": "water", "repeat": "hours", "every": 6})
	if got := remDue(t, svc, hours); got != "2026-09-28T02:00:00Z" {
		t.Fatalf("hours first: %s", got)
	}
	remAt(svc, time.Date(2026, 9, 28, 11, 0, 20, 0, seoul))
	application.DeliverReminders(svc, context.Background())
	if got := remDue(t, svc, hours); got != "2026-09-28T08:00:00Z" {
		t.Fatalf("hours next: %s", got)
	}
	remAt(svc, time.Date(2026, 9, 30, 9, 0, 20, 0, seoul))
	application.DeliverReminders(svc, context.Background())
	if got := remDue(t, svc, weekly); got != "2026-10-05T00:00:00Z" { // the next Monday
		t.Fatalf("weekly next: %s", got)
	}
	remAt(svc, time.Date(2026, 9, 30, 10, 0, 20, 0, seoul))
	application.DeliverReminders(svc, context.Background())
	if got := remDue(t, svc, monthly); got != "2026-10-31T01:00:00Z" { // October has 31 days
		t.Fatalf("monthly next: %s", got)
	}
	if gw.dmCount() < 3 {
		t.Fatalf("sent %d", gw.dmCount())
	}
	// invalid repetitions are refused with a message, nothing is saved
	for _, bad := range []map[string]any{
		{"when": "9am", "repeat": "yearly"}, {"when": "9am", "repeat": "weekly", "weekdays": "funday"}, {"when": "9am", "repeat": "hours", "every": 0},
		{"when": "9am", "repeat": "daily", "until": "2020-01-01"}, {"when": "9am", "repeat": "monthly", "day": 40},
	} {
		if r := as(svc, "reminders.propose", bad, inChannel("u4")); r.Output["status"] == "proposed" {
			t.Fatalf("accepted %v", bad)
		}
	}
}

func TestRepeatingDSTKeepsTheWallClock(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	now := time.Date(2026, 3, 6, 12, 0, 0, 0, ny)
	svc, gw := remindSvc(t, now)
	if r := exec(svc, "preferences.set", map[string]any{"user_id": "u1", "timezone": "America/New_York"}); !r.OK {
		t.Fatal(r.Error)
	}
	id := setSeries(t, svc, "u1", map[string]any{"when": "8am", "what": "run", "repeat": "daily"})
	if got := remDue(t, svc, id); got != "2026-03-07T13:00:00Z" { // 08:00 EST
		t.Fatalf("before the change: %s", got)
	}
	remAt(svc, time.Date(2026, 3, 7, 8, 0, 30, 0, ny))
	application.DeliverReminders(svc, context.Background())
	if got := remDue(t, svc, id); got != "2026-03-08T12:00:00Z" { // 08:00 EDT, 23 hours later
		t.Fatalf("across spring forward: %s", got)
	}
	remAt(svc, time.Date(2026, 3, 8, 8, 0, 30, 0, ny))
	application.DeliverReminders(svc, context.Background())
	if got := remDue(t, svc, id); got != "2026-03-09T12:00:00Z" || gw.dmCount() != 2 {
		t.Fatalf("after: %s, %d sent", got, gw.dmCount())
	}
}

// A stop between the lease and the send must neither send an occurrence twice nor lose the series.
func TestRepeatingSurvivesAStopBetweenLeaseAndSend(t *testing.T) {
	now, seoul := repSeoulNow()
	for _, after := range []bool{false, true} {
		t.Run(fmt.Sprintf("crash_after_send=%v", after), func(t *testing.T) {
			svc, base := remindSvc(t, now)
			gw := &hookGateway{postGateway: base, crashBefore: !after, crashAfter: after}
			svc.Discord = gw
			id := setSeries(t, svc, "u1", map[string]any{"when": "8am", "what": "pill", "repeat": "daily"})
			remAt(svc, time.Date(2026, 9, 29, 8, 0, 30, 0, seoul))
			if !deliverCrashing(svc) {
				t.Fatal("the simulated stop did not happen")
			}
			// the process died holding the lease: the series already points at tomorrow
			if remStatus(t, svc, id) != "sending" || remDue(t, svc, id) != "2026-09-29T23:00:00Z" {
				t.Fatalf("at the stop: %s, due %s", remStatus(t, svc, id), remDue(t, svc, id))
			}
			application.RecoverReminders(svc) // the next start
			row := remRow(t, svc, id)
			if row["status"] != "scheduled" || !strings.Contains(fmt.Sprint(row["error"]), "interrupted") {
				t.Fatalf("after the restart: %+v", row)
			}
			gw.crashBefore, gw.crashAfter = false, false
			sentThen := gw.dmCount()
			application.DeliverReminders(svc, context.Background()) // same minute again: nothing is due
			if gw.dmCount() != sentThen {
				t.Fatalf("the interrupted occurrence was sent again: %d -> %d", sentThen, gw.dmCount())
			}
			remAt(svc, time.Date(2026, 9, 30, 8, 0, 20, 0, seoul)) // the series was not lost
			application.DeliverReminders(svc, context.Background())
			if gw.dmCount() != sentThen+1 || remStatus(t, svc, id) != "scheduled" {
				t.Fatalf("next occurrence: %d sent, %s", gw.dmCount(), remStatus(t, svc, id))
			}
		})
	}
}

func TestRepeatingAfterDowntimeSendsOnlyTheNewestOccurrence(t *testing.T) {
	now, seoul := repSeoulNow()
	svc, gw := remindSvc(t, now)
	id := setSeries(t, svc, "u1", map[string]any{"when": "8am", "what": "pill", "repeat": "daily"})
	// the daemon was off for four days and starts again at 08:10: one message, for today's 08:00
	remAt(svc, time.Date(2026, 10, 2, 8, 10, 0, 0, seoul))
	application.DeliverReminders(svc, context.Background())
	application.DeliverReminders(svc, context.Background())
	if gw.dmCount() != 1 || !strings.Contains(gw.sent[0].Footer, "Fri 10/2 08:00") {
		t.Fatalf("catch-up sent %d: %+v", gw.dmCount(), gw.sent)
	}
	if got := remDue(t, svc, id); got != "2026-10-02T23:00:00Z" {
		t.Fatalf("next: %s", got)
	}
	// off again, back well after the grace window: nothing is sent late, the series goes on tomorrow
	remAt(svc, time.Date(2026, 10, 6, 11, 0, 0, 0, seoul))
	application.DeliverReminders(svc, context.Background())
	if gw.dmCount() != 1 || remStatus(t, svc, id) != "scheduled" || remDue(t, svc, id) != "2026-10-06T23:00:00Z" {
		t.Fatalf("after a long stop: %d sent, %s, due %s", gw.dmCount(), remStatus(t, svc, id), remDue(t, svc, id))
	}
	remAt(svc, time.Date(2026, 10, 7, 8, 0, 5, 0, seoul))
	application.DeliverReminders(svc, context.Background())
	if gw.dmCount() != 2 {
		t.Fatalf("the series did not go on: %d", gw.dmCount())
	}
}

func TestRepeatingCancelPauseAndResumeApplyToTheSeries(t *testing.T) {
	now, seoul := repSeoulNow()
	svc, gw := remindSvc(t, now)
	inv := inChannel("u1")
	// pause: nothing is sent while paused; resuming picks the next occurrence from now
	id := setSeries(t, svc, "u1", map[string]any{"when": "8am", "what": "pill", "repeat": "daily"})
	if r := as(svc, "reminders.update", map[string]any{"op": "pause", "id": id}, inChannel("u2")); r.OK {
		t.Fatal("someone else paused it")
	}
	if r := as(svc, "reminders.update", map[string]any{"op": "pause", "id": id}, inv); r.Output["status"] != "paused" {
		t.Fatalf("pause: %+v", r.Output)
	}
	remAt(svc, time.Date(2026, 9, 29, 8, 0, 30, 0, seoul))
	application.DeliverReminders(svc, context.Background())
	if gw.dmCount() != 0 || remStatus(t, svc, id) != "paused" {
		t.Fatalf("sent while paused: %d, %s", gw.dmCount(), remStatus(t, svc, id))
	}
	if r := as(svc, "reminders.update", map[string]any{"op": "resume", "id": id}, inv); r.Output["status"] != "scheduled" {
		t.Fatalf("resume: %+v", r.Output)
	}
	if got := remDue(t, svc, id); got != "2026-09-29T23:00:00Z" {
		t.Fatalf("resumed at %s", got)
	}
	// the console can pause and resume too
	if r := exec(svc, "reminders.update", map[string]any{"op": "pause", "id": id}); r.Output["paused"] != true {
		t.Fatalf("console pause: %+v", r.Output)
	}
	if r := exec(svc, "reminders.update", map[string]any{"op": "resume", "id": id}); r.Output["resumed"] != true {
		t.Fatalf("console resume: %+v", r.Output)
	}
	// cancel ends the series: no later occurrence is sent
	if r := as(svc, "reminders.update", map[string]any{"op": "cancel", "id": id}, inv); r.Output["status"] != "cancelled" {
		t.Fatalf("cancel: %+v", r.Output)
	}
	remAt(svc, time.Date(2026, 9, 30, 8, 0, 30, 0, seoul))
	application.DeliverReminders(svc, context.Background())
	if gw.dmCount() != 0 {
		t.Fatal("a cancelled series was sent")
	}

	// cancelled by the owner while an occurrence is being sent: that one goes, the series stops
	svc2, base := remindSvc(t, now)
	gw2 := &hookGateway{postGateway: base}
	svc2.Discord = gw2
	id2 := setSeries(t, svc2, "u1", map[string]any{"when": "8am", "what": "pill", "repeat": "daily"})
	gw2.before = func() {
		if r := as(svc2, "reminders.update", map[string]any{"op": "cancel", "id": id2}, inv); r.Output["status"] != "cancelled" {
			t.Errorf("cancel while sending: %+v", r.Output)
		}
	}
	remAt(svc2, time.Date(2026, 9, 29, 8, 0, 30, 0, seoul))
	application.DeliverReminders(svc2, context.Background())
	gw2.before = nil
	if remStatus(t, svc2, id2) != "cancelled" || gw2.dmCount() != 1 {
		t.Fatalf("after a cancel during the send: %s, %d sent", remStatus(t, svc2, id2), gw2.dmCount())
	}
	remAt(svc2, time.Date(2026, 9, 30, 8, 0, 30, 0, seoul))
	application.DeliverReminders(svc2, context.Background())
	if gw2.dmCount() != 1 {
		t.Fatal("the series went on after it was cancelled")
	}
}

func TestRepeatingEndsWithCountOrUntil(t *testing.T) {
	now, seoul := repSeoulNow()
	svc, gw := remindSvc(t, now)
	counted := setSeries(t, svc, "u1", map[string]any{"when": "8am", "what": "twice", "repeat": "daily", "count": 2})
	until := setSeries(t, svc, "u2", map[string]any{"when": "8am", "what": "until", "repeat": "daily", "until": "2026-10-01"})
	if r := as(svc, "reminders.propose", map[string]any{"when": "8am", "what": "x", "repeat": "daily", "count": 3, "until": "2026-12-31"}, inChannel("u3")); !strings.Contains(r.Output["repeat_text"].(string), "until 2026-12-31 · 3 times in all") {
		t.Fatalf("the end is not stated: %+v", r.Output)
	}
	for d := 29; d <= 33; d++ { // 29, 30 Sep, 1, 2, 3 Oct
		day := time.Date(2026, 9, d, 8, 0, 20, 0, seoul)
		remAt(svc, day)
		application.DeliverReminders(svc, context.Background())
	}
	byText := map[string]int{}
	for _, c := range gw.sent {
		byText[c.Text]++
	}
	if byText["twice"] != 2 || byText["until"] != 3 {
		t.Fatalf("sent %v", byText)
	}
	if remStatus(t, svc, counted) != "sent" || remStatus(t, svc, until) != "sent" || remDue(t, svc, counted) != "" {
		t.Fatalf("the series did not end: %s %s", remStatus(t, svc, counted), remStatus(t, svc, until))
	}
}

func TestRepeatingCapsPerPersonAndPerRoom(t *testing.T) {
	now, _ := repSeoulNow()
	svc, _ := remindSvc(t, now)
	applyDoc(t, svc, func(d *config.Document) {
		d.Tools["reminders.propose"] = tools.Settings{Defaults: map[string]any{"max_repeating": 2, "max_repeating_room": 3}}
	})
	ask := func(user string) string {
		r := as(svc, "reminders.propose", map[string]any{"when": "8am", "what": "pill", "repeat": "daily"}, inChannel(user))
		s, _ := r.Output["status"].(string)
		return s
	}
	if ask("u1") != "proposed" || ask("u1") != "proposed" {
		t.Fatal("the first two should be accepted")
	}
	if got := ask("u1"); got != "too_many_repeating" {
		t.Fatalf("third for one person: %s", got)
	}
	if ask("u2") != "proposed" { // the room now holds three
		t.Fatal("another person's first")
	}
	if got := ask("u2"); got != "too_many_repeating_room" {
		t.Fatalf("fourth in the room: %s", got)
	}
	// a one-time reminder is not counted against the caps
	if r := as(svc, "reminders.propose", map[string]any{"when": "in 3 hours", "what": "tea"}, inChannel("u1")); r.Output["status"] != "proposed" {
		t.Fatalf("one-time reminder refused: %+v", r.Output)
	}
}

func TestRescheduleKeepsTheRepetition(t *testing.T) {
	now, seoul := repSeoulNow()
	svc, gw := remindSvc(t, now)
	inv := inChannel("u1")
	id := setSeries(t, svc, "u1", map[string]any{"when": "8am", "what": "pill", "repeat": "daily"})
	r := as(svc, "reminders.update", map[string]any{"op": "reschedule", "id": id, "text": "tomorrow at 7pm"}, inv)
	if r.Output["status"] != "proposed" {
		t.Fatalf("reschedule: %+v", r.Output)
	}
	if c := card(t, r); len(c.Fields) != 2 || c.Fields[1].Value != "Every day at 19:00" {
		t.Fatalf("the new time is not stated with the repetition: %+v", c.Fields)
	}
	if c := as(svc, "reminders.update", map[string]any{"op": "confirm", "id": id, "destination": "dm"}, inv); c.Output["status"] != "scheduled" {
		t.Fatalf("confirm: %+v", c.Output)
	}
	if got := remDue(t, svc, id); got != "2026-09-29T10:00:00Z" {
		t.Fatalf("due after the change: %s", got)
	}
	remAt(svc, time.Date(2026, 9, 29, 19, 0, 10, 0, seoul))
	application.DeliverReminders(svc, context.Background())
	if gw.dmCount() != 1 || remDue(t, svc, id) != "2026-09-30T10:00:00Z" {
		t.Fatalf("%d sent, due %s", gw.dmCount(), remDue(t, svc, id))
	}
}

// ---------------------------------------------------------------------------
// context lines
// ---------------------------------------------------------------------------

func TestContextLinesLimits(t *testing.T) {
	long := strings.Repeat("가", 300)
	in := [][2]string{{"A", "first line"}, {"B", long}, {"C", long}, {"D", long}, {"E", long}}
	out := application.CapContextForTest(in)
	if len(out) > 3 || len(out) == 0 {
		t.Fatalf("lines: %d", len(out))
	}
	total := 0
	for _, l := range out {
		if n := len([]rune(l[1])); n > 220 {
			t.Fatalf("a line of %d characters", n)
		}
		total += len([]rune(l[0])) + len([]rune(l[1])) + 2
	}
	if total > 700 {
		t.Fatalf("%d characters in all", total)
	}
	if out[len(out)-1][0] != "E" { // the newest lines are the ones kept
		t.Fatalf("kept %v", out)
	}
}

func remCtxSvc(t *testing.T, now time.Time) (*application.Service, *postGateway) {
	svc, gw := remindSvc(t, now)
	applyDoc(t, svc, func(d *config.Document) {
		d.Memory.Capture = true
		d.Guilds = map[string]config.GuildSettings{"g": {Channels: map[string]config.ChannelPolicy{"c": {Respond: "mention", Capture: true}}}}
	})
	return svc, gw
}

func remObserve(svc *application.Service, id, author, name, text string, at time.Time, mentions ...string) {
	svc.Observe(application.ObservedMessage{GuildID: "g", ChannelID: "c", MessageID: id, AuthorID: author, AuthorName: name, Content: text, At: at, Mentions: mentions})
}

func remAskVague(t *testing.T, svc *application.Service, channel string, params map[string]any) (string, action.Card) {
	t.Helper()
	run := application.ChatToolForTest(svc, application.ChatInput{GuildID: "g", ChannelID: channel, MessageID: "call", Message: "remind me about that tomorrow 9am", Speaker: "Alex", SpeakerID: "u1"})
	res := run(context.Background(), "reminders.propose", params)
	if !res.OK {
		t.Fatalf("tool: %+v", res)
	}
	c, ok := res.Data["card"].(action.Card)
	if !ok {
		t.Fatalf("no card: %+v", res.Data)
	}
	for _, b := range c.Buttons {
		if id, ok := strings.CutPrefix(b.ID, "remind:cancel:"); ok {
			return id, c
		}
	}
	t.Fatalf("no reminder id: %+v", c)
	return "", c
}

func TestVagueReminderCarriesTheCallersRecentLinesAndShowsThemWhenItFires(t *testing.T) {
	now, seoul := repSeoulNow()
	svc, gw := remCtxSvc(t, now)
	base := now.Add(-time.Minute)
	remObserve(svc, "m1", "u1", "Alex", "the venue is the place on 3rd street, exit 3", base)
	remObserve(svc, "m2", "u1", "Alex", "book it before friday", base.Add(10*time.Second))
	remObserve(svc, "x1", "u9", "Other", "somebody else talking about lunch", base.Add(12*time.Second))
	remObserve(svc, "call", "u1", "Alex", "remind me about that tomorrow 9am", base.Add(20*time.Second), "bot")
	// the model tries to supply lines itself: ignored
	id, _ := remAskVague(t, svc, "c", map[string]any{"when": "tomorrow 9am", "what": "that", "with_context": true,
		"context_lines": []map[string]any{{"author": "Evil", "text": "INJECTED"}}})
	if r := as(svc, "reminders.update", map[string]any{"op": "confirm", "id": id, "destination": "dm"}, inChannel("u1")); r.Output["status"] != "scheduled" {
		t.Fatalf("confirm: %+v", r.Output)
	}
	lines := application.ReminderContextForTest(svc, id)
	if !strings.Contains(lines, "3rd street") || !strings.Contains(lines, "book it") || strings.Contains(lines, "INJECTED") || strings.Contains(lines, "lunch") || strings.Contains(lines, "tomorrow 9am") {
		t.Fatalf("context lines: %q", lines)
	}
	if n := strings.Count(lines, "\n") + 1; n > 3 {
		t.Fatalf("%d lines", n)
	}
	// it fires with the lines under a header
	remAt(svc, time.Date(2026, 9, 29, 9, 0, 30, 0, seoul))
	application.DeliverReminders(svc, context.Background())
	if gw.dmCount() != 1 {
		t.Fatalf("sent %d", gw.dmCount())
	}
	var found bool
	for _, f := range gw.sent[0].Fields {
		if f.Name == "What was being said" && strings.Contains(f.Value, "Alex: the venue is the place on 3rd street") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the delivery has no context: %+v", gw.sent[0].Fields)
	}
	// once the reminder is over its lines are gone
	if application.ReminderContextForTest(svc, id) != "" {
		t.Fatal("context lines kept after the reminder was sent")
	}
}

func TestContextLinesAreNotKeptWhereMemoryIsNotAllowedOrFromWhoWithdrew(t *testing.T) {
	now, _ := repSeoulNow()
	svc, _ := remCtxSvc(t, now)
	base := now.Add(-time.Minute)
	// a channel without capture: nothing is kept even when the model asks for context
	svc.Observe(application.ObservedMessage{GuildID: "g", ChannelID: "nocap", MessageID: "n1", AuthorID: "u1", AuthorName: "Alex", Content: "secret plan details", At: base})
	id, _ := remAskVague(t, svc, "nocap", map[string]any{"when": "tomorrow 9am", "what": "that", "with_context": true,
		"context_lines": []map[string]any{{"author": "Evil", "text": "supplied by the model"}}})
	if got := application.ReminderContextForTest(svc, id); got != "" {
		t.Fatalf("a channel without capture kept %q", got)
	}
	// a person who withdrew memory consent: their line is not taken
	as(svc, "privacy.consent", map[string]any{"scope": "memory", "decision": "withdraw"}, inChannel("u2"))
	remObserve(svc, "w1", "u2", "Blair", "withdrawn person's words", base)
	remObserve(svc, "w2", "u1", "Alex", "my own words", base.Add(5*time.Second), "u2")
	remObserve(svc, "call", "u1", "Alex", "remind me about that", base.Add(10*time.Second), "bot")
	id, _ = remAskVague(t, svc, "c", map[string]any{"when": "tomorrow 9am", "what": "that", "with_context": true})
	got := application.ReminderContextForTest(svc, id)
	if strings.Contains(got, "withdrawn person") || !strings.Contains(got, "my own words") {
		t.Fatalf("context: %q", got)
	}
	// a command or the console cannot attach lines
	r := as(svc, "reminders.propose", map[string]any{"when": "tomorrow 9am", "what": "that", "with_context": true,
		"context_lines": []map[string]any{{"author": "X", "text": "forged"}}}, inChannel("u1"))
	if got := application.ReminderContextForTest(svc, reminderID(t, r)); got != "" {
		t.Fatalf("a command attached %q", got)
	}
}

func TestContextLinesGoWhenTheirMessageIsDeletedOrTheirAuthorWithdraws(t *testing.T) {
	now, _ := repSeoulNow()
	svc, _ := remCtxSvc(t, now)
	base := now.Add(-time.Minute)
	setup := func() string {
		remObserve(svc, "m1", "u1", "Alex", "first thing said", base)
		remObserve(svc, "m2", "u1", "Alex", "second thing said", base.Add(5*time.Second))
		remObserve(svc, "call", "u1", "Alex", "remind me about that", base.Add(10*time.Second), "bot")
		id, _ := remAskVague(t, svc, "c", map[string]any{"when": "tomorrow 9am", "what": "that", "with_context": true})
		return id
	}
	id := setup()
	if got := application.ReminderContextForTest(svc, id); !strings.Contains(got, "first thing") || !strings.Contains(got, "second thing") {
		t.Fatalf("before: %q", got)
	}
	// the author deletes one message in Discord: that line goes, the other stays
	if err := svc.MessagesDeleted("g", "c", []string{"m1"}); err != nil {
		t.Fatal(err)
	}
	if got := application.ReminderContextForTest(svc, id); strings.Contains(got, "first thing") || !strings.Contains(got, "second thing") {
		t.Fatalf("after deleting m1: %q", got)
	}
	// the erasure job (a deletion the gateway reported in bulk) does the same
	if err := svc.DrainRedactions(context.Background()); err != nil {
		t.Fatal(err)
	}
	// editing a message drops its copy too: a reminder must not keep words the author changed
	if err := svc.MessageEdited("g", "c", "m2", "changed words", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if got := application.ReminderContextForTest(svc, id); got != "" {
		t.Fatalf("after editing m2: %q", got)
	}
	// withdrawing memory consent removes the person's lines from every reminder
	id2 := remSetupAgain(t, svc, base)
	if got := application.ReminderContextForTest(svc, id2); got == "" {
		t.Fatal("no lines before the withdrawal")
	}
	if r := as(svc, "privacy.consent", map[string]any{"scope": "memory", "decision": "withdraw"}, inChannel("u1")); !r.OK {
		t.Fatal(r.Error)
	}
	if err := svc.DrainRedactions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := application.ReminderContextForTest(svc, id2); got != "" {
		t.Fatalf("after the withdrawal: %q", got)
	}
}

func remSetupAgain(t *testing.T, svc *application.Service, base time.Time) string {
	remObserve(svc, "n1", "u1", "Alex", "third thing said", base.Add(30*time.Second))
	remObserve(svc, "call2", "u1", "Alex", "remind me about that too", base.Add(35*time.Second), "bot")
	run := application.ChatToolForTest(svc, application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "call2", Message: "remind me about that too", Speaker: "Alex", SpeakerID: "u1"})
	res := run(context.Background(), "reminders.propose", map[string]any{"when": "tomorrow 9am", "what": "that", "with_context": true})
	c, _ := res.Data["card"].(action.Card)
	for _, b := range c.Buttons {
		if id, ok := strings.CutPrefix(b.ID, "remind:cancel:"); ok {
			return id
		}
	}
	t.Fatalf("no reminder id: %+v", res)
	return ""
}

// The durable erasure job (not only the immediate hooks) removes the lines of a deleted or edited
// message and of a person who withdrew.
func TestErasureJobRemovesReminderContextLines(t *testing.T) {
	now, _ := repSeoulNow()
	svc, _ := remCtxSvc(t, now)
	base := now.Add(-time.Minute)
	ask := func(call string) string {
		remObserve(svc, "a-"+call, "u1", "Alex", "alpha words "+call, base)
		remObserve(svc, "b-"+call, "u1", "Alex", "beta words "+call, base.Add(5*time.Second))
		remObserve(svc, call, "u1", "Alex", "remind me about that", base.Add(10*time.Second), "bot")
		run := application.ChatToolForTest(svc, application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: call, Message: "remind me about that", Speaker: "Alex", SpeakerID: "u1"})
		res := run(context.Background(), "reminders.propose", map[string]any{"when": "tomorrow 9am", "what": "that", "with_context": true})
		c, _ := res.Data["card"].(action.Card)
		for _, b := range c.Buttons {
			if id, ok := strings.CutPrefix(b.ID, "remind:cancel:"); ok {
				return id
			}
		}
		t.Fatalf("no id: %+v", res)
		return ""
	}
	del := ask("c1")
	if got := application.ReminderContextForTest(svc, del); !strings.Contains(got, "alpha words c1") {
		t.Fatalf("before: %q", got)
	}
	if err := application.RedactionJobForTest(svc, context.Background(), "delete", "g", "c", "", "a-c1"); err != nil {
		t.Fatal(err)
	}
	if got := application.ReminderContextForTest(svc, del); strings.Contains(got, "alpha words") || !strings.Contains(got, "beta words c1") {
		t.Fatalf("after the deletion job: %q", got)
	}
	if err := application.RedactionJobForTest(svc, context.Background(), "edit", "g", "c", "", "b-c1"); err != nil {
		t.Fatal(err)
	}
	if got := application.ReminderContextForTest(svc, del); strings.Contains(got, "beta words c1") {
		t.Fatalf("after the edit job: %q", got)
	}
	other := ask("c2")
	if got := application.ReminderContextForTest(svc, other); got == "" {
		t.Fatal("no lines to withdraw")
	}
	if err := application.RedactionJobForTest(svc, context.Background(), "withdraw", "", "", "u1"); err != nil {
		t.Fatal(err)
	}
	if got := application.ReminderContextForTest(svc, other); got != "" {
		t.Fatalf("after the withdrawal job: %q", got)
	}
}

// ---------------------------------------------------------------------------
// review fixes
// ---------------------------------------------------------------------------

// A second process that read the due list before the first one leased cannot send the occurrence again,
// not even after the series is back to scheduled with its next due_at.
func TestRepeatingStaleSecondDelivererCannotSendTheSameOccurrenceAgain(t *testing.T) {
	now, seoul := repSeoulNow()
	svc, gw := remindSvc(t, now)
	id := setSeries(t, svc, "u1", map[string]any{"when": "8am", "what": "pill", "repeat": "daily"})
	remAt(svc, time.Date(2026, 9, 29, 8, 0, 30, 0, seoul))
	stale := application.SnapshotDueForTest(svc) // what the other process read
	application.DeliverReminders(svc, context.Background())
	application.DeliverSnapshotForTest(svc, context.Background(), stale)
	application.DeliverSnapshotForTest(svc, context.Background(), stale)
	if gw.dmCount() != 1 || remStatus(t, svc, id) != "scheduled" || remDue(t, svc, id) != "2026-09-29T23:00:00Z" {
		t.Fatalf("%d sent, %s, due %s", gw.dmCount(), remStatus(t, svc, id), remDue(t, svc, id))
	}
}

func remConfirmDM(t *testing.T, svc *application.Service, id string) {
	t.Helper()
	if r := as(svc, "reminders.update", map[string]any{"op": "confirm", "id": id, "destination": "dm"}, inChannel("u1")); r.Output["status"] != "scheduled" {
		t.Fatalf("confirm: %+v", r.Output)
	}
}

func remHasContext(c action.Card) bool {
	for _, f := range c.Fields {
		if f.Name == "What was being said" {
			return true
		}
	}
	return false
}

// Conversation lines are shown with the first message of a series only, and the rows are gone after it.
func TestRepeatingShowsContextLinesOnlyWithTheFirstOccurrence(t *testing.T) {
	now, seoul := repSeoulNow()
	svc, gw := remCtxSvc(t, now)
	base := now.Add(-time.Minute)
	remObserve(svc, "m1", "u1", "Alex", "the venue is on 3rd street", base)
	remObserve(svc, "call", "u1", "Alex", "remind me about that every day", base.Add(10*time.Second), "bot")
	id, _ := remAskVague(t, svc, "c", map[string]any{"when": "8am", "what": "that", "repeat": "daily", "with_context": true})
	remConfirmDM(t, svc, id)
	for d := 29; d <= 30; d++ {
		remAt(svc, time.Date(2026, 9, d, 8, 0, 20, 0, seoul))
		application.DeliverReminders(svc, context.Background())
	}
	if gw.dmCount() != 2 || !remHasContext(gw.sent[0]) || remHasContext(gw.sent[1]) {
		t.Fatalf("context on the occurrences: %d sent, first %v, second %v", gw.dmCount(), remHasContext(gw.sent[0]), remHasContext(gw.sent[1]))
	}
	if n := application.ReminderContextRowsForTest(svc, id); n != 0 {
		t.Fatalf("%d context rows kept", n)
	}
}

// Context is read under the channel's rules at delivery: turning capture off drops the lines unsent.
func TestContextLinesAreNotShownAfterCaptureIsTurnedOff(t *testing.T) {
	now, seoul := repSeoulNow()
	svc, gw := remCtxSvc(t, now)
	base := now.Add(-time.Minute)
	remObserve(svc, "m1", "u1", "Alex", "the venue is on 3rd street", base)
	remObserve(svc, "call", "u1", "Alex", "remind me about that", base.Add(10*time.Second), "bot")
	id, _ := remAskVague(t, svc, "c", map[string]any{"when": "tomorrow 9am", "what": "that", "with_context": true})
	remConfirmDM(t, svc, id)
	if application.ReminderContextRowsForTest(svc, id) == 0 {
		t.Fatal("no lines were taken")
	}
	applyDoc(t, svc, func(d *config.Document) {
		d.Guilds = map[string]config.GuildSettings{"g": {Channels: map[string]config.ChannelPolicy{"c": {Respond: "mention", Capture: false}}}}
	})
	remAt(svc, time.Date(2026, 9, 29, 9, 0, 20, 0, seoul))
	application.DeliverReminders(svc, context.Background())
	if gw.dmCount() != 1 || remHasContext(gw.sent[0]) || application.ReminderContextRowsForTest(svc, id) != 0 {
		t.Fatalf("context after capture was turned off: %d sent, %d rows", gw.dmCount(), application.ReminderContextRowsForTest(svc, id))
	}
}

// When grace is longer than the interval, the newest missed occurrence is sent, not the oldest.
func TestRepeatingWithLongGraceSendsTheNewestOccurrence(t *testing.T) {
	now, seoul := repSeoulNow()
	svc, gw := remindSvc(t, now)
	applyDoc(t, svc, func(d *config.Document) {
		d.Tools["reminders.propose"] = tools.Settings{Defaults: map[string]any{"grace_min": 180}}
	})
	id := setSeries(t, svc, "u1", map[string]any{"when": "in 1 hour", "what": "water", "repeat": "hours", "every": 1})
	remAt(svc, time.Date(2026, 9, 28, 13, 20, 0, 0, seoul)) // 11:00, 12:00 and 13:00 are all inside grace
	application.DeliverReminders(svc, context.Background())
	application.DeliverReminders(svc, context.Background())
	if gw.dmCount() != 1 || !strings.Contains(gw.sent[0].Footer, "Mon 9/28 13:00") || remDue(t, svc, id) != "2026-09-28T05:00:00Z" {
		t.Fatalf("%d sent: %+v, due %s", gw.dmCount(), gw.sent, remDue(t, svc, id))
	}
}

// A weekly rule that did not name its weekday follows the option that was picked, not the first one.
func TestRepeatingDerivedWeekdayFollowsThePickedOption(t *testing.T) {
	now, seoul := repSeoulNow() // Monday 10:00
	inv := inChannel("u1")
	seen := map[string]bool{}
	for n := 0; n < 2; n++ {
		svc, _ := remindSvc(t, now)
		r := asKo(svc, "reminders.propose", map[string]any{"when": "6시", "what": "walk", "repeat": "weekly"}, inv)
		if r.Output["status"] != "proposed" {
			t.Fatalf("propose: %+v", r.Output)
		}
		id := reminderID(t, r)
		if p := as(svc, "reminders.update", map[string]any{"op": "pick", "id": id, "n": n}, inv); p.Output["status"] != "proposed" {
			t.Fatalf("pick %d: %+v", n, p.Output)
		}
		remConfirmDM(t, svc, id)
		first := remDue(t, svc, id)
		seen[first] = true
		at, _ := time.Parse(time.RFC3339, first)
		remAt(svc, at.Add(20*time.Second))
		application.DeliverReminders(svc, context.Background())
		if got, want := remDue(t, svc, id), at.AddDate(0, 0, 7).Format(time.RFC3339); got != want {
			t.Fatalf("option %d: first %s, then %s (want %s)", n, first, got, want)
		}
		_ = seoul
	}
	if len(seen) != 2 {
		t.Fatalf("the two options gave the same first occurrence: %v", seen)
	}
	// and rescheduling takes the weekday from the new time
	svc, _ := remindSvc(t, now)
	id := setSeries(t, svc, "u1", map[string]any{"when": "8am", "what": "pill", "repeat": "weekly"})
	if r := as(svc, "reminders.update", map[string]any{"op": "reschedule", "id": id, "text": "wednesday at 7pm"}, inv); r.Output["status"] != "proposed" {
		t.Fatalf("reschedule: %+v", r.Output)
	}
	remConfirmDM(t, svc, id)
	if got := remDue(t, svc, id); got != "2026-09-30T10:00:00Z" {
		t.Fatalf("rescheduled first occurrence %s", got)
	}
}

func TestRepeatingFinalOccurrenceRetriesLikeAOneTimeReminder(t *testing.T) {
	now, seoul := repSeoulNow()
	svc, base := remindSvc(t, now)
	base.failDM = fmt.Errorf("%w: 500", application.ErrNotDelivered)
	id := setSeries(t, svc, "u1", map[string]any{"when": "8am", "what": "once more", "repeat": "daily", "count": 1})
	remAt(svc, time.Date(2026, 9, 29, 8, 0, 30, 0, seoul))
	application.DeliverReminders(svc, context.Background())
	if remStatus(t, svc, id) != "scheduled" {
		t.Fatalf("the last occurrence was not retried: %s", remStatus(t, svc, id))
	}
	base.failDM = nil
	remAt(svc, time.Date(2026, 9, 29, 8, 5, 0, 0, seoul))
	application.DeliverReminders(svc, context.Background())
	if base.dmCount() != 1 || remStatus(t, svc, id) != "sent" {
		t.Fatalf("after the retry: %d sent, %s", base.dmCount(), remStatus(t, svc, id))
	}
	// refused every time: three attempts, then failed
	svc2, base2 := remindSvc(t, now)
	base2.failDM = fmt.Errorf("%w: 500", application.ErrNotDelivered)
	id2 := setSeries(t, svc2, "u1", map[string]any{"when": "8am", "what": "never", "repeat": "daily", "count": 1})
	for i := 0; i < 4; i++ {
		remAt(svc2, time.Date(2026, 9, 29, 8, 0, 30, 0, seoul).Add(time.Duration(i*10)*time.Minute))
		application.DeliverReminders(svc2, context.Background())
	}
	if remStatus(t, svc2, id2) != "failed" {
		t.Fatalf("refused three times: %s", remStatus(t, svc2, id2))
	}
}

func TestRepeatingMissedSeriesDropsContextAndPauseCannotStrandTheLastOccurrence(t *testing.T) {
	now, seoul := repSeoulNow()
	// the series ended (until passed) while the daemon was off: missed, and its lines are gone
	svc, _ := remCtxSvc(t, now)
	remObserve(svc, "m1", "u1", "Alex", "the venue is on 3rd street", now.Add(-time.Minute))
	remObserve(svc, "call", "u1", "Alex", "remind me about that", now.Add(-50*time.Second), "bot")
	id, _ := remAskVague(t, svc, "c", map[string]any{"when": "8am", "what": "that", "repeat": "daily", "until": "2026-09-29", "with_context": true})
	remConfirmDM(t, svc, id)
	if application.ReminderContextRowsForTest(svc, id) == 0 {
		t.Fatal("no lines to drop")
	}
	remAt(svc, time.Date(2026, 10, 5, 12, 0, 0, 0, seoul))
	application.DeliverReminders(svc, context.Background())
	if remStatus(t, svc, id) != "missed" || application.ReminderContextRowsForTest(svc, id) != 0 {
		t.Fatalf("%s, %d rows", remStatus(t, svc, id), application.ReminderContextRowsForTest(svc, id))
	}
	// pausing while the last occurrence is going out does not leave a paused series with nothing to resume
	svc2, base := remindSvc(t, now)
	gw := &hookGateway{postGateway: base}
	svc2.Discord = gw
	id2 := setSeries(t, svc2, "u1", map[string]any{"when": "8am", "what": "last", "repeat": "daily", "count": 1})
	gw.before = func() {
		as(svc2, "reminders.update", map[string]any{"op": "pause", "id": id2}, inChannel("u1"))
	}
	remAt(svc2, time.Date(2026, 9, 29, 8, 0, 30, 0, seoul))
	application.DeliverReminders(svc2, context.Background())
	if remStatus(t, svc2, id2) != "sent" || gw.dmCount() != 1 {
		t.Fatalf("after pausing the last occurrence: %s, %d sent", remStatus(t, svc2, id2), gw.dmCount())
	}
}

func TestRepeatingCapsAreCheckedAgainAtConfirmation(t *testing.T) {
	now, _ := repSeoulNow()
	svc, _ := remindSvc(t, now)
	applyDoc(t, svc, func(d *config.Document) {
		d.Tools["reminders.propose"] = tools.Settings{Defaults: map[string]any{"max_repeating": 2}}
	})
	inv := inChannel("u1")
	var ids []string
	for i := 0; i < 2; i++ {
		r := as(svc, "reminders.propose", map[string]any{"when": "8am", "what": "pill", "repeat": "daily"}, inv)
		ids = append(ids, reminderID(t, r))
	}
	// at the cap exactly, confirming is fine
	remConfirmDM(t, svc, ids[0])
	// the cap is lowered before the other one is confirmed: refused, and it stays a proposal
	applyDoc(t, svc, func(d *config.Document) {
		d.Tools["reminders.propose"] = tools.Settings{Defaults: map[string]any{"max_repeating": 1}}
	})
	if c := as(svc, "reminders.update", map[string]any{"op": "confirm", "id": ids[1], "destination": "dm"}, inv); c.Output["status"] != "too_many_repeating" || remStatus(t, svc, ids[1]) != "proposed" {
		t.Fatalf("confirm over the cap: %+v, %s", c.Output, remStatus(t, svc, ids[1]))
	}
}
