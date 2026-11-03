package application_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/clock"
	"masamong/internal/config"
	"masamong/internal/tools"
)

// flakyDM fails its first sends as not delivered (a Discord outage), then records like dmGateway.
type flakyDM struct {
	dmGateway
	fail int
}

func (f *flakyDM) SendDM(ctx context.Context, user string, c action.Card, ref string) error {
	f.mu.Lock()
	if f.fail > 0 {
		f.fail--
		f.mu.Unlock()
		return application.ErrNotDelivered
	}
	f.mu.Unlock()
	return f.dmGateway.SendDM(ctx, user, c, ref)
}

// briefingRig is a subscriber in Seoul with a model that records its prompts and always greets with a
// morning line (so a card that must make no time-of-day claim shows whether the greeting was dropped).
type briefingRig struct {
	svc     *application.Service
	mu      sync.Mutex
	prompts []string
	failing bool // the model answers 500 (recorded all the same)
}

func (r *briefingRig) setFailing(v bool) {
	r.mu.Lock()
	r.failing = v
	r.mu.Unlock()
}

func (r *briefingRig) prompt(i int) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if i < 0 {
		i += len(r.prompts)
	}
	if i < 0 || i >= len(r.prompts) {
		return ""
	}
	return r.prompts[i]
}

func (r *briefingRig) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.prompts)
}

func newBriefingRig(t *testing.T, start time.Time, subTime string, quiet []string, defaults map[string]any) *briefingRig {
	t.Helper()
	rig := &briefingRig{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		b, _ := io.ReadAll(r.Body)
		rig.mu.Lock()
		rig.prompts = append(rig.prompts, string(b))
		failing := rig.failing
		rig.mu.Unlock()
		if failing {
			http.Error(w, `{"error":{"message":"down"}}`, http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant",
			"content": "GREETING: 좋은 아침이에요, 철수님!\nSCORE: 99\nSUMMARY: 흐름\nWORK: 집중"}, "finish_reason": "stop"}}})
	}))
	t.Cleanup(srv.Close)
	svc := routedSvc(t, srv.URL, srv.URL)
	applyDoc(t, svc, func(d *config.Document) {
		d.Discord.Enabled = true
		d.I18n.Timezone = "Asia/Seoul"
		d.I18n.DefaultLocale = "ko"
		if defaults != nil {
			if d.Tools == nil {
				d.Tools = map[string]tools.Settings{}
			}
			d.Tools["fortune.subscribe"] = tools.Settings{Defaults: defaults}
		}
	})
	serveAnyServer(t, svc)
	svc.Clock = clock.Fixed{T: start}
	dm := person("u1", true)
	_ = as(svc, "privacy.consent", map[string]any{"scope": "fortune", "decision": "grant", "confirmed": true}, dm)
	_ = as(svc, "fortune.subscribe", map[string]any{"op": "register", "birth_date": "1995-06-15"}, dm)
	if r := as(svc, "fortune.subscribe", map[string]any{"op": "subscribe", "time": subTime}, dm); !r.OK {
		t.Fatalf("subscribe: %+v", r)
	}
	if quiet != nil {
		if r := as(svc, "preferences.set", map[string]any{"quiet_start": quiet[0], "quiet_end": quiet[1]}, dm); !r.OK {
			t.Fatalf("quiet hours: %+v", r)
		}
	}
	rig.svc = svc
	return rig
}

func (r *briefingRig) runAt(t time.Time) {
	r.svc.Clock = clock.Fixed{T: t}
	application.RunFortuneMorning(r.svc, context.Background())
}

func wantAll(t *testing.T, what, text string, words ...string) {
	t.Helper()
	for _, w := range words {
		if !strings.Contains(text, w) {
			t.Errorf("%s misses %q:\n%s", what, w, text)
		}
	}
}

func wantNone(t *testing.T, what, text string, words ...string) {
	t.Helper()
	for _, w := range words {
		if strings.Contains(text, w) {
			t.Errorf("%s has %q:\n%s", what, w, text)
		}
	}
}

// A 23:30 subscription under quiet hours 23:00–07:00 is delivered at 07:00: it is worded and dated from
// that moment (a morning briefing for the date it arrives on), not as an evening briefing for tomorrow.
func TestBriefingHeldByQuietHoursIsWordedForItsDelivery(t *testing.T) {
	seoul, _ := time.LoadLocation("Asia/Seoul")
	rig := newBriefingRig(t, time.Date(2026, 10, 15, 20, 0, 0, 0, seoul), "23:30", []string{"23:00", "07:00"}, nil)
	gw := &dmGateway{}
	rig.svc.Discord = gw
	rig.runAt(time.Date(2026, 10, 15, 23, 21, 0, 0, seoul)) // written within the lead
	if rig.count() != 1 {
		t.Fatalf("written early: %d prompts", rig.count())
	}
	wantAll(t, "prompt", rig.prompt(0), "2026-10-16 아침 운세", "좋은 아침이에요")
	wantNone(t, "prompt", rig.prompt(0), "내일(", "수고 많으셨어요")
	rig.runAt(time.Date(2026, 10, 15, 23, 30, 0, 0, seoul))
	rig.runAt(time.Date(2026, 10, 16, 3, 0, 0, 0, seoul))
	if len(gw.sent) != 0 {
		t.Fatal("sent during quiet hours")
	}
	rig.runAt(time.Date(2026, 10, 16, 7, 0, 0, 0, seoul))
	if len(gw.sent) != 1 || rig.count() != 1 {
		t.Fatalf("sent %d, prompts %d", len(gw.sent), rig.count())
	}
	wantAll(t, "title", gw.sent[0].Title, "☀️", "10월 16일", "아침 운세")
}

// A 07:00 briefing that could only go out at 15:00 (the host was down) is written again for the daytime,
// once; with the generation cap spent it goes out with a title and greeting that claim no time of day.
func TestCatchUpBriefingIsRewrittenForItsSlot(t *testing.T) {
	seoul, _ := time.LoadLocation("Asia/Seoul")
	t.Run("rewritten", func(t *testing.T) {
		rig := newBriefingRig(t, time.Date(2026, 10, 15, 5, 0, 0, 0, seoul), "07:00", nil, nil)
		gw := &dmGateway{}
		rig.svc.Discord = gw
		rig.runAt(time.Date(2026, 10, 15, 6, 52, 0, 0, seoul))
		if rig.count() != 1 || len(gw.sent) != 0 {
			t.Fatalf("written early: prompts %d sent %d", rig.count(), len(gw.sent))
		}
		rig.runAt(time.Date(2026, 10, 15, 15, 0, 0, 0, seoul)) // the 07:00 send never ran
		if rig.count() != 2 || len(gw.sent) != 1 {
			t.Fatalf("rewritten once and sent: prompts %d sent %d", rig.count(), len(gw.sent))
		}
		wantAll(t, "second prompt", rig.prompt(1), "2026-10-15 오늘의 운세", "잘 보내고 계신가요")
		wantNone(t, "second prompt", rig.prompt(1), "아침 운세 브리핑")
		wantAll(t, "title", gw.sent[0].Title, "🌤️", "10월 15일", "오늘의 운세")
		rig.runAt(time.Date(2026, 10, 15, 15, 1, 0, 0, seoul))
		if rig.count() != 2 || len(gw.sent) != 1 {
			t.Fatalf("once: prompts %d sent %d", rig.count(), len(gw.sent))
		}
	})
	t.Run("cap spent", func(t *testing.T) {
		rig := newBriefingRig(t, time.Date(2026, 10, 15, 5, 0, 0, 0, seoul), "07:00", nil, map[string]any{"generation_attempts": 1})
		gw := &dmGateway{}
		rig.svc.Discord = gw
		rig.runAt(time.Date(2026, 10, 15, 6, 52, 0, 0, seoul))
		rig.runAt(time.Date(2026, 10, 15, 15, 0, 0, 0, seoul))
		if rig.count() != 1 || len(gw.sent) != 1 {
			t.Fatalf("not rewritten, sent: prompts %d sent %d", rig.count(), len(gw.sent))
		}
		m := gw.sent[0]
		wantAll(t, "title", m.Title, "10월 15일", "운세")
		wantNone(t, "title", m.Title, "아침", "오늘의", "내일", "☀️", "🌙")
		wantNone(t, "card", m.Text, "좋은 아침")
		wantAll(t, "card", m.Text, "철수님") // the neutral head line instead of the model's greeting
	})
}

// An evening briefing due 23:58 whose send failed is retried after midnight: it is now the night slot of
// the same date, so it is written again for that slot instead of being titled "tomorrow".
func TestEveningBriefingRetriedAfterMidnight(t *testing.T) {
	seoul, _ := time.LoadLocation("Asia/Seoul")
	rig := newBriefingRig(t, time.Date(2026, 10, 15, 20, 0, 0, 0, seoul), "23:58", nil, nil)
	gw := &flakyDM{fail: 1}
	rig.svc.Discord = gw
	rig.runAt(time.Date(2026, 10, 15, 23, 48, 0, 0, seoul))
	wantAll(t, "prompt", rig.prompt(0), "내일(2026-10-16)")
	rig.runAt(time.Date(2026, 10, 15, 23, 58, 0, 0, seoul)) // not delivered: retried in two minutes
	if len(gw.sent) != 0 {
		t.Fatal("the first send failed")
	}
	rig.runAt(time.Date(2026, 10, 16, 0, 3, 0, 0, seoul))
	if len(gw.sent) != 1 || rig.count() != 2 {
		t.Fatalf("sent %d, prompts %d", len(gw.sent), rig.count())
	}
	wantAll(t, "second prompt", rig.prompt(1), "2026-10-16 오늘의 운세", "늦은 밤까지")
	wantAll(t, "title", gw.sent[0].Title, "🌙", "10월 16일", "오늘의 운세")
	wantNone(t, "title", gw.sent[0].Title, "내일")
}

// When the one rewrite for a late send fails, the text written for the planned slot goes out at the next
// try, as it is, under the neutral title (not left unsent until the generation cap fails the job).
func TestFailedRewriteSendsTheWrittenTextNeutrally(t *testing.T) {
	seoul, _ := time.LoadLocation("Asia/Seoul")
	rig := newBriefingRig(t, time.Date(2026, 10, 15, 5, 0, 0, 0, seoul), "07:00", nil, nil)
	gw := &dmGateway{}
	rig.svc.Discord = gw
	rig.runAt(time.Date(2026, 10, 15, 6, 52, 0, 0, seoul))
	rig.setFailing(true)
	rig.runAt(time.Date(2026, 10, 15, 15, 0, 0, 0, seoul)) // the rewrite fails
	if len(gw.sent) != 0 {
		t.Fatalf("sent while rewriting: %d", len(gw.sent))
	}
	n := rig.count()
	rig.setFailing(false)
	rig.runAt(time.Date(2026, 10, 15, 15, 30, 0, 0, seoul)) // after the retry wait
	if len(gw.sent) != 1 || rig.count() != n {
		t.Fatalf("sent %d, prompts %d (was %d)", len(gw.sent), rig.count(), n)
	}
	m := gw.sent[0]
	wantAll(t, "title", m.Title, "🔮", "10월 15일", "운세")
	wantNone(t, "title", m.Title, "아침", "오늘의")
	wantNone(t, "card", m.Text, "좋은 아침")
}

// A briefing that could not go out on its day (the host was down past the next morning) is not
// rewritten for the date the next day's own briefing covers: that one goes, once.
func TestDayOldBriefingGivesWayToTodays(t *testing.T) {
	seoul, _ := time.LoadLocation("Asia/Seoul")
	rig := newBriefingRig(t, time.Date(2026, 10, 15, 5, 0, 0, 0, seoul), "07:00", nil, nil)
	gw := &dmGateway{}
	rig.svc.Discord = gw
	rig.runAt(time.Date(2026, 10, 15, 6, 52, 0, 0, seoul)) // written, then the host stops
	for _, at := range []time.Time{time.Date(2026, 10, 16, 8, 0, 0, 0, seoul), time.Date(2026, 10, 16, 8, 1, 0, 0, seoul), time.Date(2026, 10, 16, 8, 2, 0, 0, seoul)} {
		rig.runAt(at)
	}
	if len(gw.sent) != 1 {
		t.Fatalf("sent %d briefings", len(gw.sent))
	}
	wantAll(t, "title", gw.sent[0].Title, "☀️", "10월 16일", "아침 운세")
	wantAll(t, "last prompt", rig.prompt(-1), "2026-10-16 아침 운세")
}

// An evening briefing sent at 23:59 on the last day of a month and of a year is the next month's and
// the next year's first day.
func TestEveningBriefingAtMonthAndYearEnd(t *testing.T) {
	seoul, _ := time.LoadLocation("Asia/Seoul")
	for _, c := range []struct {
		day   time.Time
		task  string
		title string
	}{
		{time.Date(2026, 10, 31, 0, 0, 0, 0, seoul), "내일(2026-11-01)", "11월 1일"},
		{time.Date(2026, 12, 31, 0, 0, 0, 0, seoul), "내일(2027-01-01)", "1월 1일"},
	} {
		rig := newBriefingRig(t, c.day.Add(20*time.Hour), "23:59", nil, nil)
		gw := &dmGateway{}
		rig.svc.Discord = gw
		rig.runAt(c.day.Add(23*time.Hour + 59*time.Minute))
		if len(gw.sent) != 1 {
			t.Fatalf("%s: sent %d", c.task, len(gw.sent))
		}
		wantAll(t, "prompt", rig.prompt(0), c.task, "기준 시각: "+c.task[len("내일("):len(c.task)-1])
		wantAll(t, "title", gw.sent[0].Title, "🌙", c.title, "내일 운세")
	}
}

// An evening briefing is tomorrow's reading: its prompt does not call the day "today" or the sky "now"
// (the data labels name the reading's date and time, the SUMMARY line the day).
func TestEveningPromptDoesNotSayToday(t *testing.T) {
	seoul, _ := time.LoadLocation("Asia/Seoul")
	rig := newBriefingRig(t, time.Date(2026, 10, 15, 18, 0, 0, 0, seoul), "21:00", nil, nil)
	rig.svc.Discord = &dmGateway{}
	rig.runAt(time.Date(2026, 10, 15, 21, 0, 0, 0, seoul))
	p := rig.prompt(0)
	wantAll(t, "prompt", p, "내일(2026-10-16)", "기준 시각: 2026-10-16 21:00", "기준일의 간지", "기준 시각의 천체 배치", "SUMMARY: 하루의")
	wantNone(t, "prompt", p, "SUMMARY: 오늘", "현재 천체", "오늘의 간지", "오늘 일간", "오늘 채울")
}
