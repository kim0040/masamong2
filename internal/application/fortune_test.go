package application_test

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/clock"
	"masamong/internal/config"
	"masamong/internal/features/fortune"
)

// dmGateway is a connected gateway that records direct messages.
type dmGateway struct {
	fakeGateway
	mu   sync.Mutex
	sent []action.Card
	to   []string
}

func (d *dmGateway) SendDM(_ context.Context, user string, c action.Card, _ string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sent, d.to = append(d.sent, c), append(d.to, user)
	return nil
}

func fortuneSvc(t *testing.T, calls *atomic.Int32) *application.Service {
	url := chatServer(t, "**오늘의 흐름**: 85점", "stop", calls)
	svc := routedSvc(t, url, url)
	applyDoc(t, svc, func(d *config.Document) { d.Discord.Enabled = true })
	return svc
}

func person(id string, private bool) *action.Invoker {
	inv := &action.Invoker{Via: "command", UserID: id, Name: "철수", Private: private}
	if !private {
		inv.GuildID, inv.ChannelID = "g", "c"
	}
	return inv
}

func TestFortuneFlow(t *testing.T) {
	var calls atomic.Int32
	svc := fortuneSvc(t, &calls)
	serveAnyServer(t, svc)
	dm := person("u1", true)
	// without consent: the policy with its agree button, nothing stored, no model call
	r := as(svc, "fortune.subscribe", map[string]any{"op": "register", "text": "1990-01-01 14:30 남 서울"}, dm)
	card := r.Output["card"].(action.Card)
	if r.Output["status"] != "consent_required" || len(card.Buttons) != 1 {
		t.Fatalf("consent first: %+v", r)
	}
	if r := as(svc, "privacy.consent", map[string]any{"scope": "fortune", "decision": "grant", "confirmed": true}, dm); !r.OK {
		t.Fatal(r)
	}
	// personal commands in a public channel are refused without processing anything
	if r := as(svc, "fortune.subscribe", map[string]any{"op": "register", "text": "1990-01-01"}, person("u1", false)); r.Output["status"] != "private_only" {
		t.Fatalf("public register: %+v", r)
	}
	if r := as(svc, "fortune.query", map[string]any{"what": "today"}, dm); r.Output["status"] != "no_profile" {
		t.Fatalf("no profile yet: %+v", r)
	}
	for _, bad := range []string{"1990-13-01", "2999-01-01", "어제"} {
		if r := as(svc, "fortune.subscribe", map[string]any{"op": "register", "text": bad}, dm); r.OK {
			t.Fatalf("bad date %q accepted", bad)
		}
	}
	r = as(svc, "fortune.subscribe", map[string]any{"op": "register", "text": "1990-01-01 14:30 남 서울 강남"}, dm)
	if !r.OK || r.Output["status"] != "registered" {
		t.Fatalf("register: %+v", r)
	}
	fields := r.Output["card"].(action.Card).Fields
	if fields[0].Value != "1990-01-01 14:30" || fields[2].Value != "남성" && fields[2].Value != "male" || fields[3].Value != "서울 강남" {
		t.Fatalf("parsed profile: %+v", fields)
	}
	before := calls.Load()
	r = as(svc, "fortune.query", map[string]any{"what": "detail"}, dm)
	if !r.OK || r.Output["status"] != "ok" || !strings.Contains(r.Output["text"].(string), "85점") || calls.Load() == before {
		t.Fatalf("detail: %+v", r)
	}
	// public: the three-line summary only; detailed readings are refused there without a model call
	before = calls.Load()
	if r := as(svc, "fortune.query", map[string]any{"what": "month"}, person("u1", false)); r.Output["status"] != "private_only" || calls.Load() != before {
		t.Fatalf("public month: %+v", r)
	}
	if r := as(svc, "fortune.query", map[string]any{"what": "today"}, person("u1", false)); r.Output["status"] != "ok" {
		t.Fatalf("public today: %+v", r)
	}
	// daily limit: 3 personal readings (two used)
	if r := as(svc, "fortune.query", map[string]any{"what": "year"}, dm); r.Output["status"] != "ok" || r.Output["left_today"] != 0 {
		t.Fatalf("third: %+v", r)
	}
	if r := as(svc, "fortune.query", map[string]any{"what": "today"}, dm); r.Output["status"] != "limited" {
		t.Fatalf("fourth must be limited: %+v", r)
	}
	// the zodiac is shared and cached for the day
	z := as(svc, "fortune.query", map[string]any{"what": "zodiac", "sign": "물병"}, person("u2", true))
	if !z.OK || z.Output["cached"] != false || z.Output["sign"] == "" {
		t.Fatalf("zodiac: %+v", z)
	}
	z2 := as(svc, "fortune.query", map[string]any{"what": "zodiac", "sign": "물병자리"}, person("u3", true))
	if z2.Output["cached"] != true {
		t.Fatalf("second asker reads the cache: %+v", z2)
	}
	if r := as(svc, "fortune.query", map[string]any{"what": "zodiac", "sign": "물병"}, person("u2", true)); r.OK {
		t.Fatal("cooldown between zodiac requests")
	}
	if r := as(svc, "fortune.query", map[string]any{"what": "zodiac", "sign": "용자리"}, person("u4", true)); r.OK {
		t.Fatal("unknown sign")
	}
	// own sign from the profile (Capricorn for 1 January)
	if r := as(svc, "fortune.query", map[string]any{"what": "zodiac"}, person("u1", true)); !r.OK || !strings.Contains(r.Output["sign"].(string), "염소") && r.Output["sign"] != "Capricorn" {
		t.Fatalf("own sign: %+v", r)
	}
	// the console sees subscriptions, never birth data
	list := exec(svc, "fortune.subscribe", nil)
	if s := asJSON(t, list.Output); strings.Contains(s, "1990") || !strings.Contains(s, "u1") {
		t.Fatalf("console list: %s", s)
	}
	// delete removes everything and withdraws consent
	if r := as(svc, "fortune.subscribe", map[string]any{"op": "delete"}, dm); !r.OK || r.Output["deleted"] != true {
		t.Fatalf("delete: %+v", r)
	}
	if svc.ConsentAllows("u1", "fortune") {
		t.Fatal("deleting withdraws consent")
	}
}

func TestMorningBriefingIsWrittenEarlyAndSentOnce(t *testing.T) {
	var calls atomic.Int32
	svc := fortuneSvc(t, &calls)
	dm := person("u1", true)
	_ = as(svc, "privacy.consent", map[string]any{"scope": "fortune", "decision": "grant", "confirmed": true}, dm)
	_ = as(svc, "fortune.subscribe", map[string]any{"op": "register", "birth_date": "1995-06-15"}, dm)
	if r := as(svc, "fortune.subscribe", map[string]any{"op": "subscribe", "time": "7:30"}, dm); !r.OK || r.Output["time"] != "07:30" {
		t.Fatalf("subscribe: %+v", r)
	}
	gw := &dmGateway{}
	svc.Discord = gw
	seoul, _ := time.LoadLocation("Asia/Seoul")
	at := func(h, m int) { svc.Clock = clock.Fixed{T: time.Date(2026, 9, 29, h, m, 0, 0, seoul)} }
	ctx := context.Background()
	at(7, 0) // too early
	application.RunFortuneMorning(svc, ctx)
	before := calls.Load()
	at(7, 22) // within the lead: written, not yet sent
	application.RunFortuneMorning(svc, ctx)
	if calls.Load() != before+1 || len(gw.sent) != 0 {
		t.Fatalf("written early: calls %d sent %d", calls.Load()-before, len(gw.sent))
	}
	at(7, 30)
	application.RunFortuneMorning(svc, ctx)
	application.RunFortuneMorning(svc, ctx)
	if len(gw.sent) != 1 || gw.to[0] != "u1" || calls.Load() != before+1 {
		t.Fatalf("sent once without writing again: sent %d calls %d", len(gw.sent), calls.Load()-before)
	}
	// quiet hours hold the next day's briefing until they end
	_ = as(svc, "preferences.set", map[string]any{"quiet_start": "06:00", "quiet_end": "09:00"}, dm)
	svc.Clock = clock.Fixed{T: time.Date(2026, 9, 30, 7, 30, 0, 0, seoul)}
	application.RunFortuneMorning(svc, ctx)
	if len(gw.sent) != 1 {
		t.Fatal("held by quiet hours")
	}
	svc.Clock = clock.Fixed{T: time.Date(2026, 9, 30, 9, 1, 0, 0, seoul)}
	application.RunFortuneMorning(svc, ctx)
	if len(gw.sent) != 2 {
		t.Fatalf("sent after quiet hours: %d", len(gw.sent))
	}
	// withdrawn consent stops delivery
	_ = as(svc, "privacy.consent", map[string]any{"scope": "fortune", "decision": "withdraw"}, dm)
	svc.Clock = clock.Fixed{T: time.Date(2026, 10, 1, 7, 31, 0, 0, seoul)}
	application.RunFortuneMorning(svc, ctx)
	if len(gw.sent) != 2 {
		t.Fatal("no briefing without consent")
	}
}

// A reading in "KEY: value" lines becomes a laid-out card; the public summary carries no personal count;
// the morning DM gets its stop button. A day's reading shows the computed score, lucky points and basis,
// not the ones the model wrote (77, 파랑, 시계).
func TestFortuneTagReadingBecomesACard(t *testing.T) {
	var calls atomic.Int32
	url := chatServer(t, "SCORE: 77\nSUMMARY: 좋은 흐름이에요.\nWORK: 집중\nLUCKY_COLOR: 파랑\nLUCKY_ITEM: 시계", "stop", &calls)
	svc := routedSvc(t, url, url)
	applyDoc(t, svc, func(d *config.Document) { d.Discord.Enabled = true; d.I18n.Timezone = "Asia/Seoul" })
	serveAnyServer(t, svc)
	seoul, _ := time.LoadLocation("Asia/Seoul")
	at := time.Date(2026, 10, 15, 8, 0, 0, 0, seoul)
	svc.Clock = clock.Fixed{T: at}
	dm := person("u1", true)
	_ = as(svc, "privacy.consent", map[string]any{"scope": "fortune", "decision": "grant", "confirmed": true}, dm)
	_ = as(svc, "fortune.subscribe", map[string]any{"op": "register", "birth_date": "1995-06-15"}, dm)
	// the expected day, from the rule engine itself: born 1995-06-15 (no time, so noon), in Seoul
	y, mo, d, _, _ := fortune.PillarsAt(time.Date(1995, 6, 15, 12, 0, 0, 0, seoul), false)
	natal := fortune.Natal{Year: y, Month: mo, Day: d}
	want := fortune.Analyze(natal, at)
	r := as(svc, "fortune.query", map[string]any{"what": "today"}, dm)
	card := r.Output["card"].(action.Card)
	score := fmt.Sprintf("**%d**/100", want.Scores.Overall)
	if !strings.Contains(card.Text, score) || !strings.HasPrefix(card.Text, "철수") || !strings.Contains(card.Text, "🧭 ") || card.Tone != "fortune" {
		t.Fatalf("laid out: %+v", card)
	}
	if want.Scores.Overall != 77 && strings.Contains(card.Text, "**77**") || strings.Contains(fmt.Sprint(card.Fields), "파랑") || strings.Contains(fmt.Sprint(card.Fields), "시계") {
		t.Fatalf("the model's score or lucky points leaked: %+v", card)
	}
	if len(card.Fields) != 1 || !strings.Contains(card.Fields[0].Value, "🧭") || !strings.Contains(card.Fields[0].Value, "🔢 "+strconv.Itoa(want.Lucky.Number)) {
		t.Fatalf("computed lucky points in one line: %+v", card.Fields)
	}
	// the day pillar is today's (it changes daily); the requests here are in English
	if !regexp.MustCompile(`Day pillar [A-Z][a-z]+-[A-Z][a-z]+`).MatchString(card.Footer) || !strings.Contains(card.Footer, "Gemini") {
		t.Fatalf("footer names the day pillar and the sign: %q", card.Footer)
	}
	pub := as(svc, "fortune.query", map[string]any{"what": "today"}, person("u1", false)).Output["card"].(action.Card)
	if strings.Contains(pub.Footer, "left today") || pub.Ephemeral {
		t.Fatalf("a public answer shows no personal count: %+v", pub)
	}

	_ = as(svc, "fortune.subscribe", map[string]any{"op": "subscribe", "time": "07:30"}, dm)
	gw := &dmGateway{}
	svc.Discord = gw
	due := time.Date(2026, 10, 16, 7, 30, 0, 0, seoul)
	svc.Clock = clock.Fixed{T: due}
	application.RunFortuneMorning(svc, context.Background())
	if len(gw.sent) != 1 {
		t.Fatalf("sent %d", len(gw.sent))
	}
	m := gw.sent[0]
	morning := fmt.Sprintf("**%d**/100", fortune.Analyze(natal, due).Scores.Overall)
	if !strings.Contains(m.Title, "16") || strings.Contains(m.Title, "2026-10-16") || len(m.Buttons) != 1 || m.Buttons[0].ID != "fortune:unsub" || !strings.Contains(m.Text, morning) {
		t.Fatalf("morning card: %+v", m)
	}
}
