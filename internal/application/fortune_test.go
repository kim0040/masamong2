package application_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/clock"
	"masamong/internal/config"
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
	// Expected values by hand. Born 1995-06-15 without a time (noon, Seoul): lunar-go's EightChar gives
	// 을해년 임오월 정축일. 2026-10-15 is lunar-go's 무술월 임술일 (건, 백호 흑도).
	//   ten gods of 정: 임 정관, 술 (main stem 무) 상관; 정 at 술 is 양 (strength 2)
	//   relations of 술: 축 (day) 형, 오 (month) 반합 (인오술 with the cardinal 오), 해 (year) none; 정임 합;
	//   no star
	//   half points (overall, money, love, work, health): 정관×2 {6,0,2,12,0} + 상관 {1,1,4,-3,-1}
	//   + 양 {4,0,0,0,4} + 형 day×2 {-6,0,-2,-2,-4} + 반합 {3,1,1,3,0} + 천간합×2 {6,0,8,2,0}
	//   + 흑도 {-4,-2,-2,-2,-2} = {10,0,11,10,-3} → halved {5,0,5,5,-1} + 60 = 65 60 65 65 59
	//   lucky: natal 목1 화2 토1 금0 수2, plus 임 (수) 술 (토) → 금; JDN 2461329 is odd → number 9
	r := as(svc, "fortune.query", map[string]any{"what": "today"}, dm)
	card := r.Output["card"].(action.Card)
	if !strings.Contains(card.Text, "**65**/100") || !strings.HasPrefix(card.Text, "철수") || !strings.Contains(card.Text, "🧭 ") || card.Tone != "fortune" {
		t.Fatalf("laid out: %+v", card)
	}
	if strings.Contains(card.Text, "**77**") || strings.Contains(fmt.Sprint(card.Fields), "파랑") || strings.Contains(fmt.Sprint(card.Fields), "시계") {
		t.Fatalf("the model's score or lucky points leaked: %+v", card)
	}
	if len(card.Fields) != 1 || !strings.Contains(card.Fields[0].Value, "🧭") || !strings.Contains(card.Fields[0].Value, "🔢 9") {
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
	// 2026-10-16 is 무술월 계해일 (제, 옥당 황도): 계 편관, 해 (main 임) 정관, 정 at 해 태 (0); 해 with 해
	// (year) 형, with 축 and 오 none; 정계 충; 천을귀인 (정 → 해유), 역마 (day 축 → 해).
	//   {-2,0,2,8,-6} + {3,0,1,6,0} + 형 {-3,0,-1,-1,-2} + 천간충×2 {-6,0,-2,-4,-2} + 귀인×2 {10,4,4,6,2}
	//   + 역마×2 {2,2,0,4,-2} + 황도×2 {4,2,2,2,2} = {8,8,6,21,-8} → {4,4,3,10,-4} + 60: overall 64
	if !strings.Contains(m.Title, "16") || strings.Contains(m.Title, "2026-10-16") || len(m.Buttons) != 1 || m.Buttons[0].ID != "fortune:unsub" || !strings.Contains(m.Text, "**64**/100") {
		t.Fatalf("morning card: %+v", m)
	}
}

// A public channel's summary is written without the person's computed facts (scores, lucky points,
// element counts); the private reading gets them.
func TestFortunePublicPromptHasNoComputedFacts(t *testing.T) {
	var mu sync.Mutex
	var prompts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		prompts = append(prompts, string(b))
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "SUMMARY: 좋아요\nWORK: 집중"}, "finish_reason": "stop"}}})
	}))
	t.Cleanup(srv.Close)
	svc := routedSvc(t, srv.URL, srv.URL)
	applyDoc(t, svc, func(d *config.Document) { d.Discord.Enabled = true; d.I18n.Timezone = "Asia/Seoul" })
	serveAnyServer(t, svc)
	seoul, _ := time.LoadLocation("Asia/Seoul")
	svc.Clock = clock.Fixed{T: time.Date(2026, 10, 15, 8, 0, 0, 0, seoul)}
	dm := person("u1", true)
	_ = as(svc, "privacy.consent", map[string]any{"scope": "fortune", "decision": "grant", "confirmed": true}, dm)
	_ = as(svc, "fortune.subscribe", map[string]any{"op": "register", "birth_date": "1995-06-15"}, dm)
	last := func() string {
		mu.Lock()
		defer mu.Unlock()
		return prompts[len(prompts)-1]
	}
	if r := as(svc, "fortune.query", map[string]any{"what": "today"}, person("u1", false)); r.Output["status"] != "ok" {
		t.Fatalf("public: %+v", r)
	}
	if p := last(); strings.Contains(p, "Computed facts") || strings.Contains(p, "Natal elements") {
		t.Fatalf("the public prompt carries the computed facts:\n%s", p)
	}
	if r := as(svc, "fortune.query", map[string]any{"what": "today"}, dm); r.Output["status"] != "ok" {
		t.Fatalf("private: %+v", r)
	}
	if p := last(); !strings.Contains(p, "Computed facts") {
		t.Fatalf("the private prompt misses the computed facts:\n%s", p)
	}
}

// The twelve animals' day needs no profile and no model call. 2026-10-15 is a 임술 day (lunar-go): by
// the rule in animals.go, 묘 육합 75, 오 반합 72, 술 itself 65, 자 인 사 신 해 60 (인술 has no cardinal),
// 유 해 54, 축 형 52, 미 형·파 47, 진 충 45.
func TestFortuneAnimals(t *testing.T) {
	var calls atomic.Int32
	svc := fortuneSvc(t, &calls)
	applyDoc(t, svc, func(d *config.Document) { d.I18n.Timezone = "Asia/Seoul" })
	serveAnyServer(t, svc)
	seoul, _ := time.LoadLocation("Asia/Seoul")
	svc.Clock = clock.Fixed{T: time.Date(2026, 10, 15, 21, 0, 0, 0, seoul)}
	r := as(svc, "fortune.query", map[string]any{"what": "animals"}, person("nobody", false))
	if !r.OK || r.Output["status"] != "ok" || calls.Load() != 0 {
		t.Fatalf("%+v (model calls %d)", r, calls.Load())
	}
	var order []int
	for _, x := range r.Output["ranking"].([]map[string]any) {
		order = append(order, x["branch"].(int))
	}
	if fmt.Sprint(order) != fmt.Sprint([]int{3, 6, 10, 0, 2, 5, 8, 11, 9, 1, 7, 4}) {
		t.Fatalf("ranking %v", order)
	}
	card := r.Output["card"].(action.Card)
	if strings.Count(card.Text, "\n") < 12 || card.Ephemeral || card.Tone != "fortune" {
		t.Fatalf("card %+v", card)
	}
}

// The briefing's wording follows its local send time; an evening briefing is tomorrow's reading. The
// scores are the hand-derived ones of TestFortuneTagReadingBecomesACard: 2026-10-15 is 65, 10-16 is 64.
func TestBriefingFollowsTheSendTime(t *testing.T) {
	seoul, _ := time.LoadLocation("Asia/Seoul")
	for _, c := range []struct {
		at                string
		due               time.Time
		title             []string
		greeting, task    string
		facts, score      string
		noMorningGreeting bool
	}{
		{"06:30", time.Date(2026, 10, 15, 6, 30, 0, 0, seoul), []string{"☀️", "10월 15일", "아침 운세"}, "좋은 아침이에요", "2026-10-15 아침 운세", "총운 65", "**65**/100", false},
		{"13:00", time.Date(2026, 10, 15, 13, 0, 0, 0, seoul), []string{"🌤️", "10월 15일", "오늘의 운세"}, "잘 보내고 계신가요", "2026-10-15 오늘의 운세", "총운 65", "**65**/100", true},
		{"21:00", time.Date(2026, 10, 15, 21, 0, 0, 0, seoul), []string{"🌙", "10월 16일", "내일 운세"}, "수고 많으셨어요", "내일(2026-10-16)", "총운 64", "**64**/100", true},
		{"00:30", time.Date(2026, 10, 15, 0, 30, 0, 0, seoul), []string{"🌙", "10월 15일", "오늘의 운세"}, "늦은 밤까지", "2026-10-15 오늘의 운세", "총운 65", "**65**/100", true},
	} {
		var mu sync.Mutex
		var prompts []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/chat/completions" {
				http.NotFound(w, r)
				return
			}
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			prompts = append(prompts, string(b))
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "GREETING: 안녕\nSCORE: 99\nSUMMARY: 흐름\nWORK: 집중"}, "finish_reason": "stop"}}})
		}))
		svc := routedSvc(t, srv.URL, srv.URL)
		applyDoc(t, svc, func(d *config.Document) {
			d.Discord.Enabled = true
			d.I18n.Timezone = "Asia/Seoul"
			d.I18n.DefaultLocale = "ko"
		})
		serveAnyServer(t, svc)
		svc.Clock = clock.Fixed{T: c.due.Add(-2 * time.Hour)}
		dm := person("u1", true)
		_ = as(svc, "privacy.consent", map[string]any{"scope": "fortune", "decision": "grant", "confirmed": true}, dm)
		_ = as(svc, "fortune.subscribe", map[string]any{"op": "register", "birth_date": "1995-06-15"}, dm)
		_ = as(svc, "fortune.subscribe", map[string]any{"op": "subscribe", "time": c.at}, dm)
		gw := &dmGateway{}
		svc.Discord = gw
		svc.Clock = clock.Fixed{T: c.due}
		application.RunFortuneMorning(svc, context.Background())
		srv.Close()
		if len(gw.sent) != 1 || len(prompts) != 1 {
			t.Fatalf("%s: sent %d, prompts %d", c.at, len(gw.sent), len(prompts))
		}
		m, prompt := gw.sent[0], prompts[0]
		for _, w := range c.title {
			if !strings.Contains(m.Title, w) {
				t.Errorf("%s: title %q misses %q", c.at, m.Title, w)
			}
		}
		if !strings.Contains(prompt, c.greeting) || !strings.Contains(prompt, c.task) || !strings.Contains(prompt, c.facts) {
			t.Errorf("%s: prompt misses %q / %q / %q:\n%s", c.at, c.greeting, c.task, c.facts, prompt)
		}
		if c.noMorningGreeting && strings.Contains(prompt, "좋은 아침") {
			t.Errorf("%s: a morning greeting outside the morning", c.at)
		}
		if !strings.Contains(m.Text, c.score) {
			t.Errorf("%s: card %q misses %s", c.at, m.Text, c.score)
		}
	}
}
