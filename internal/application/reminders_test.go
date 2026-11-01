package application_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/clock"
	"masamong/internal/config"
	"masamong/internal/errcode"
	"masamong/internal/tools"
)

// postGateway is a connected gateway that records DMs and channel posts, and can fail on purpose.
type postGateway struct {
	dmGateway
	pmu    sync.Mutex
	posts  []string // channel: text or card title
	failDM error    // returned by SendDM when set
	failCh error    // returned by PostChannel when set
}

func (g *postGateway) SendDM(ctx context.Context, user string, c action.Card, img string) error {
	if g.failDM != nil {
		return g.failDM
	}
	return g.dmGateway.SendDM(ctx, user, c, img)
}

func (g *postGateway) PostChannel(_ context.Context, channelID, replyTo, text string, card *action.Card) (string, error) {
	if g.failCh != nil {
		return "", g.failCh
	}
	g.pmu.Lock()
	defer g.pmu.Unlock()
	body := text
	if card != nil {
		body = card.Title + "|" + card.Text
	}
	g.posts = append(g.posts, channelID+": "+body)
	return fmt.Sprintf("msg%d", len(g.posts)), nil
}

func (g *postGateway) postCount() int {
	g.pmu.Lock()
	defer g.pmu.Unlock()
	return len(g.posts)
}

func (g *postGateway) dmCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.sent)
}

func remindSvc(t *testing.T, now time.Time) (*application.Service, *postGateway) {
	svc := setupSvc(t)
	serveAnyServer(t, svc)
	applyDoc(t, svc, func(d *config.Document) { d.Discord.Enabled = true; d.I18n.Timezone = "Asia/Seoul" })
	gw := &postGateway{}
	svc.Discord = gw
	svc.Clock = clock.Fixed{T: now}
	return svc, gw
}

func inChannel(user string) *action.Invoker {
	return &action.Invoker{Via: "command", GuildID: "g", ChannelID: "c", UserID: user, Name: "철수"}
}

func card(t *testing.T, r action.Result) action.Card {
	t.Helper()
	c, ok := r.Output["card"].(action.Card)
	if !ok {
		t.Fatalf("no card: %+v", r)
	}
	return c
}

func buttonIDs(c action.Card) []string {
	var out []string
	for _, b := range c.Buttons {
		out = append(out, b.ID)
	}
	return out
}

func reminderID(t *testing.T, r action.Result) string {
	t.Helper()
	for _, id := range buttonIDs(card(t, r)) {
		if strings.HasPrefix(id, "remind:cancel:") {
			return strings.TrimPrefix(id, "remind:cancel:")
		}
	}
	t.Fatalf("no reminder id in %+v", r)
	return ""
}

func TestReminderLifecycle(t *testing.T) {
	seoul, _ := time.LoadLocation("Asia/Seoul")
	now := time.Date(2026, 9, 28, 10, 0, 0, 0, seoul)
	svc, gw := remindSvc(t, now)

	// asking only proposes: the time and where it goes are confirmed by the person who asked
	r := as(svc, "reminders.propose", map[string]any{"text": "tomorrow at 3pm call mom"}, inChannel("u1"))
	if !r.OK || r.Output["status"] != "proposed" {
		t.Fatalf("propose: %+v", r)
	}
	id := reminderID(t, r)
	c := card(t, r)
	if c.Text != "call mom" || len(c.Buttons) != 3 || !strings.Contains(strings.Join(buttonIDs(c), " "), "remind:here:"+id) {
		t.Fatalf("proposal card: %+v", c)
	}
	// someone else cannot confirm it
	if r := as(svc, "reminders.update", map[string]any{"op": "confirm", "id": id, "destination": "dm"}, inChannel("u2")); r.OK || r.Error.Code != errcode.PermissionDenied {
		t.Fatalf("another person confirmed: %+v", r)
	}
	if r := as(svc, "reminders.update", map[string]any{"op": "confirm", "id": id, "destination": "dm"}, inChannel("u1")); !r.OK || r.Output["status"] != "scheduled" {
		t.Fatalf("confirm: %+v", r)
	}
	// nothing is due yet
	application.DeliverReminders(svc, context.Background())
	if gw.dmCount() != 0 {
		t.Fatal("sent early")
	}
	// the next day at 15:00: sent once, never twice
	svc.Clock = clock.Fixed{T: time.Date(2026, 9, 29, 15, 0, 30, 0, seoul)}
	application.DeliverReminders(svc, context.Background())
	application.DeliverReminders(svc, context.Background())
	if gw.dmCount() != 1 || gw.to[0] != "u1" || gw.sent[0].Text != "call mom" {
		t.Fatalf("delivered: %+v", gw.sent)
	}
	list := exec(svc, "reminders.update", map[string]any{"op": "list", "user_id": "u1"})
	if got := list.Output["reminders"]; !strings.Contains(fmt.Sprint(got), "sent") {
		t.Fatalf("console list: %+v", got)
	}
}

type pauseReminderClock struct {
	at      time.Time
	mu      sync.Mutex
	calls   int
	reached chan struct{}
	resume  chan struct{}
}

func (c *pauseReminderClock) Now() time.Time {
	c.mu.Lock()
	c.calls++
	block := c.calls == 2 // Execute reserves the tool on its first clock read; the handler reads the snapshot next.
	c.mu.Unlock()
	if block {
		close(c.reached)
		<-c.resume
	}
	return c.at
}

func TestStaleReminderConfirmationCannotUndoCancellation(t *testing.T) {
	now := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	svc, _ := remindSvc(t, now)
	inv := person("u1", true)
	proposal := as(svc, "reminders.propose", map[string]any{"text": "in 30 minutes call mom"}, inv)
	if !proposal.OK {
		t.Fatalf("propose: %+v", proposal)
	}
	id := reminderID(t, proposal)
	c := &pauseReminderClock{at: now, reached: make(chan struct{}), resume: make(chan struct{})}
	svc.Clock = c
	confirmed := make(chan action.Result, 1)
	go func() {
		confirmed <- as(svc, "reminders.update", map[string]any{"op": "confirm", "id": id, "destination": "dm"}, inv)
	}()
	<-c.reached // the confirmation read the proposed snapshot and is waiting before its conditional write.
	cancelled := as(svc, "reminders.update", map[string]any{"op": "cancel", "id": id}, inv)
	if !cancelled.OK || cancelled.Output["status"] != "cancelled" {
		close(c.resume)
		t.Fatalf("cancel: %+v", cancelled)
	}
	close(c.resume)
	result := <-confirmed
	if result.Output["status"] != "cancelled" {
		t.Fatalf("stale confirmation did not return current state: %+v", result.Output)
	}
}

func TestReminderParserRejectsExplicitInvalidDateWithLocalizedMessage(t *testing.T) {
	now := time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC)
	svc, _ := remindSvc(t, now)
	res := as(svc, "reminders.propose", map[string]any{"text": "2026-02-30 at 15:30 call"}, person("u1", true))
	if !res.OK || res.Output["status"] != "invalid_date" {
		t.Fatalf("invalid explicit date was not rejected: %+v", res)
	}
	if got := card(t, res).Text; !strings.Contains(got, "That date does not exist. Check the day and month.") {
		t.Fatalf("localized invalid-date message: %q", got)
	}
}

func TestReminderAmbiguousTimeAndChannel(t *testing.T) {
	seoul, _ := time.LoadLocation("Asia/Seoul")
	now := time.Date(2026, 9, 28, 10, 0, 0, 0, seoul)
	svc, gw := remindSvc(t, now)
	inv := inChannel("u1")
	r := asKo(svc, "reminders.propose", map[string]any{"text": "3시에 회의"}, inv)
	c := card(t, r)
	var picks []string
	for _, id := range buttonIDs(c) {
		if strings.HasPrefix(id, "remind:pick:") {
			picks = append(picks, id)
		}
	}
	if len(picks) != 2 || !strings.Contains(c.Buttons[0].Label, "15:00") {
		t.Fatalf("two times to pick, soonest first: %+v", c)
	}
	id := reminderID(t, r)
	// before picking there is nothing to confirm
	if r := asKo(svc, "reminders.update", map[string]any{"op": "confirm", "id": id, "destination": "channel"}, inv); r.Output["status"] != "proposed" {
		t.Fatalf("confirmed before picking a time: %+v", r)
	}
	if r := asKo(svc, "reminders.update", map[string]any{"op": "pick", "id": id, "n": 0}, inv); !r.OK || len(card(t, r).Fields) != 1 {
		t.Fatalf("pick: %+v", r)
	}
	if r := asKo(svc, "reminders.update", map[string]any{"op": "confirm", "id": id, "destination": "channel"}, inv); r.Output["status"] != "scheduled" {
		t.Fatalf("confirm: %+v", r)
	}
	svc.Clock = clock.Fixed{T: now.Add(5*time.Hour + 10*time.Second)}
	application.DeliverReminders(svc, context.Background())
	if gw.postCount() != 1 || !strings.Contains(gw.posts[0], "c: ") || !strings.Contains(gw.posts[0], "회의") {
		t.Fatalf("channel post: %v", gw.posts)
	}
}

func asKo(svc *application.Service, act string, in map[string]any, inv *action.Invoker) action.Result {
	raw, _ := json.Marshal(in)
	return svc.Execute(context.Background(), action.Request{Action: act, Input: raw, Locale: "ko", Invoker: inv})
}

// consoleReminders is every reminder as the console sees it.
func consoleReminders(t *testing.T, svc *application.Service) []map[string]any {
	t.Helper()
	var out []map[string]any
	raw, _ := json.Marshal(exec(svc, "reminders.update", map[string]any{"op": "list"}).Output["reminders"])
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestReminderDeliveryOutcomes(t *testing.T) {
	seoul, _ := time.LoadLocation("Asia/Seoul")
	now := time.Date(2026, 9, 28, 10, 0, 0, 0, seoul)
	set := func(svc *application.Service, user, text string) string {
		inv := inChannel(user)
		r := as(svc, "reminders.propose", map[string]any{"text": text}, inv)
		id := reminderID(t, r)
		if r := as(svc, "reminders.update", map[string]any{"op": "confirm", "id": id, "destination": "dm"}, inv); r.Output["status"] != "scheduled" {
			t.Fatalf("confirm %s: %+v", text, r)
		}
		return id
	}
	status := func(svc *application.Service, id string) string {
		for _, m := range consoleReminders(t, svc) {
			if m["id"] == id {
				return m["status"].(string)
			}
		}
		return ""
	}

	// a connection that broke mid-send: unknown, never sent again
	svc, gw := remindSvc(t, now)
	id := set(svc, "u1", "in 5 minutes stretch")
	gw.failDM = errors.New("connection reset")
	svc.Clock = clock.Fixed{T: now.Add(6 * time.Minute)}
	application.DeliverReminders(svc, context.Background())
	gw.failDM = nil
	application.DeliverReminders(svc, context.Background())
	if st := status(svc, id); st != "unknown" || gw.dmCount() != 0 {
		t.Fatalf("unknown outcome: %s, %d sent", st, gw.dmCount())
	}

	// Discord refused: nothing was posted, so it tries again, then gives up after three attempts
	svc, gw = remindSvc(t, now)
	id = set(svc, "u1", "in 5 minutes stretch")
	gw.failDM = fmt.Errorf("%w: 500", application.ErrNotDelivered)
	for i := 0; i < 4; i++ {
		svc.Clock = clock.Fixed{T: now.Add(time.Duration(6+i*10) * time.Minute)}
		application.DeliverReminders(svc, context.Background())
	}
	if st := status(svc, id); st != "failed" {
		t.Fatalf("refused three times: %s", st)
	}

	// DMs closed for good: failed at once
	svc, gw = remindSvc(t, now)
	id = set(svc, "u1", "in 5 minutes stretch")
	gw.failDM = application.ErrDMClosed
	svc.Clock = clock.Fixed{T: now.Add(6 * time.Minute)}
	application.DeliverReminders(svc, context.Background())
	if st := status(svc, id); st != "failed" {
		t.Fatalf("dm closed: %s", st)
	}

	// the daemon was off past the grace window: missed, not sent late
	svc, gw = remindSvc(t, now)
	id = set(svc, "u1", "in 5 minutes stretch")
	svc.Clock = clock.Fixed{T: now.Add(3 * time.Hour)}
	application.DeliverReminders(svc, context.Background())
	if st := status(svc, id); st != "missed" || gw.dmCount() != 0 {
		t.Fatalf("missed: %s", st)
	}

	// quiet hours hold a DM until they end
	svc, gw = remindSvc(t, now)
	if r := exec(svc, "preferences.set", map[string]any{"user_id": "u1", "quiet_start": "10:00", "quiet_end": "12:00", "timezone": "Asia/Seoul"}); !r.OK {
		t.Fatal(r.Error)
	}
	id = set(svc, "u1", "in 5 minutes stretch")
	svc.Clock = clock.Fixed{T: now.Add(6 * time.Minute)}
	application.DeliverReminders(svc, context.Background())
	if gw.dmCount() != 0 || status(svc, id) != "scheduled" {
		t.Fatal("sent in quiet hours")
	}
	svc.Clock = clock.Fixed{T: time.Date(2026, 9, 28, 12, 0, 30, 0, seoul)}
	application.DeliverReminders(svc, context.Background())
	if gw.dmCount() != 1 {
		t.Fatal("not sent when quiet hours ended")
	}

	// a send cut short by a stop is unknown after a restart
	svc, _ = remindSvc(t, now)
	id = set(svc, "u1", "in 5 minutes stretch")
	if r := exec(svc, "reminders.update", map[string]any{"op": "list"}); !r.OK {
		t.Fatal(r.Error)
	}
	application.MarkReminderSendingForTest(svc, id)
	application.RecoverReminders(svc)
	if st := status(svc, id); st != "unknown" {
		t.Fatalf("interrupted: %s", st)
	}
}

func TestReminderCancelRescheduleAndLimits(t *testing.T) {
	seoul, _ := time.LoadLocation("Asia/Seoul")
	now := time.Date(2026, 9, 28, 10, 0, 0, 0, seoul)
	svc, gw := remindSvc(t, now)
	inv := inChannel("u1")
	r := as(svc, "reminders.propose", map[string]any{"text": "tomorrow at 9am gym"}, inv)
	id := reminderID(t, r)
	_ = as(svc, "reminders.update", map[string]any{"op": "confirm", "id": id, "destination": "dm"}, inv)
	// a new time is a new agreement
	r = as(svc, "reminders.update", map[string]any{"op": "reschedule", "id": id, "text": "tomorrow at 7pm"}, inv)
	if r.Output["status"] != "proposed" {
		t.Fatalf("reschedule: %+v", r)
	}
	if r := as(svc, "reminders.update", map[string]any{"op": "cancel", "id": id}, inv); r.Output["status"] != "cancelled" {
		t.Fatalf("cancel: %+v", r)
	}
	svc.Clock = clock.Fixed{T: now.Add(34 * time.Hour)}
	application.DeliverReminders(svc, context.Background())
	if gw.dmCount() != 0 {
		t.Fatal("a cancelled reminder was sent")
	}
	svc.Clock = clock.Fixed{T: now}
	// errors a person sees: no time, a time that passed, too far ahead, too many
	for text, want := range map[string]string{"call mom": "no_time", "today at 8am call": "past", "2028-01-01 12:00 party": "too_far"} {
		if r := as(svc, "reminders.propose", map[string]any{"text": text}, inv); r.Output["status"] != want {
			t.Fatalf("%q: %+v", text, r.Output)
		}
	}
	applyDoc(t, svc, func(d *config.Document) {
		d.Tools["reminders.propose"] = tools.Settings{Defaults: map[string]any{"max_active": 2}}
	})
	for i := 0; i < 2; i++ {
		_ = as(svc, "reminders.propose", map[string]any{"text": "in 3 hours tea"}, inv)
	}
	if r := as(svc, "reminders.propose", map[string]any{"text": "in 3 hours tea"}, inv); r.Output["status"] != "too_many" {
		t.Fatalf("limit: %+v", r.Output)
	}
	// the console cannot create one: a reminder is the person's own request
	if r := exec(svc, "reminders.propose", map[string]any{"text": "in 3 hours tea"}); r.OK {
		t.Fatal("the console created a reminder")
	}
}

func TestReminderOfferIsPerPerson(t *testing.T) {
	seoul, _ := time.LoadLocation("Asia/Seoul")
	now := time.Date(2026, 9, 28, 10, 0, 0, 0, seoul)
	svc, _ := remindSvc(t, now)
	offer, err := application.SaveOfferForTest(svc, "g", "c", "토요일 저녁 7시 강남 모임")
	if err != nil {
		t.Fatal(err)
	}
	a := as(svc, "reminders.propose", map[string]any{"offer_id": offer}, inChannel("u1"))
	b := as(svc, "reminders.propose", map[string]any{"offer_id": offer}, inChannel("u2"))
	ida, idb := reminderID(t, a), reminderID(t, b)
	if ida == idb {
		t.Fatal("each person gets a reminder of their own")
	}
	// pressing for yourself sets nothing for the other person
	_ = as(svc, "reminders.update", map[string]any{"op": "confirm", "id": ida, "destination": "dm"}, inChannel("u1"))
	st := map[string]string{}
	for _, m := range consoleReminders(t, svc) {
		st[m["id"].(string)] = m["status"].(string)
	}
	if st[ida] != "scheduled" || st[idb] != "proposed" {
		t.Fatalf("offer states: %v", st)
	}
}

// Many people asking, confirming and cancelling at once while delivery passes overlap: every
// reminder goes out at most once, and none is left mid-send.
func TestRemindersUnderConcurrency(t *testing.T) {
	seoul, _ := time.LoadLocation("Asia/Seoul")
	now := time.Date(2026, 9, 28, 10, 0, 0, 0, seoul)
	svc, gw := remindSvc(t, now)
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			inv := &action.Invoker{Via: "command", GuildID: "g", ChannelID: "c", UserID: fmt.Sprintf("u%02d", i%10), Name: "p"}
			r := as(svc, "reminders.propose", map[string]any{"text": fmt.Sprintf("in 5 minutes task %d", i)}, inv)
			c, ok := r.Output["card"].(action.Card)
			if !ok {
				t.Errorf("no card: %+v", r)
				return
			}
			id := ""
			for _, b := range c.Buttons {
				if strings.HasPrefix(b.ID, "remind:cancel:") {
					id = strings.TrimPrefix(b.ID, "remind:cancel:")
				}
			}
			dest := []string{"dm", "channel"}[i%2]
			_ = as(svc, "reminders.update", map[string]any{"op": "confirm", "id": id, "destination": dest}, inv)
			if i%7 == 0 {
				_ = as(svc, "reminders.update", map[string]any{"op": "cancel", "id": id}, inv)
			}
		}(i)
	}
	wg.Wait()
	svc.Clock = clock.Fixed{T: now.Add(6 * time.Minute)}
	var passes sync.WaitGroup
	for i := 0; i < 4; i++ {
		passes.Add(1)
		go func() { defer passes.Done(); application.DeliverReminders(svc, context.Background()) }()
	}
	passes.Wait()
	application.DeliverReminders(svc, context.Background())
	counts := map[string]int{}
	for _, c := range gw.sent {
		counts[c.Text]++
	}
	for _, p := range gw.posts {
		counts[p[strings.Index(p, "|")+1:]]++
	}
	sent, cancelled := 0, 0
	for _, m := range consoleReminders(t, svc) {
		switch m["status"] {
		case "sent":
			sent++
		case "cancelled":
			cancelled++
		default:
			t.Fatalf("left %s: %+v", m["status"], m)
		}
	}
	for text, n := range counts {
		if n > 1 {
			t.Fatalf("%q delivered %d times", text, n)
		}
	}
	if sent+cancelled != 40 || cancelled != 6 || len(counts) != sent {
		t.Fatalf("sent %d, cancelled %d, delivered %d", sent, cancelled, len(counts))
	}
}
