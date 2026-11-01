package application_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/clock"
)

type scriptedDailySender struct {
	fakeGateway
	outcomes []error
	sends    int
}

func (s *scriptedDailySender) SendDM(context.Context, string, action.Card, string) error {
	s.sends++ // the test sender records remote acceptance before returning the selected outcome.
	if len(s.outcomes) == 0 {
		return nil
	}
	err := s.outcomes[0]
	s.outcomes = s.outcomes[1:]
	return err
}

func dailyDeliveryService(t *testing.T, now time.Time, zone, date, fortuneTime, noticeTime string, sender *scriptedDailySender) *application.Service {
	t.Helper()
	var calls atomic.Int32
	svc := fortuneSvc(t, &calls)
	serveAnyServer(t, svc)
	inv := person("alice", true)
	for _, scope := range []string{"fortune", "transfer_notice"} {
		if res := as(svc, "privacy.consent", map[string]any{"scope": scope, "decision": "grant", "confirmed": true}, inv); !res.OK {
			t.Fatalf("consent %s: %+v", scope, res)
		}
	}
	stamp := now.UTC().Format(time.RFC3339)
	job, _ := json.Marshal(map[string]any{"date": date, "state": "written", "content": "already written briefing"})
	if _, err := svc.Store.DB.Exec(`INSERT INTO fortune_profiles(user_id,birth_date,sub_time,sub_active,last_sent,job,created_at,updated_at)
		VALUES('alice','1990-01-01',?,1,'',?,?,?)`, fortuneTime, string(job), stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Store.DB.Exec(`INSERT INTO user_prefs(user_id,timezone,quiet_start,quiet_end,updated_at) VALUES('alice',?,'22:00','07:00',?)`, zone, stamp); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(svc.DataRoot, "notices"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(svc.DataRoot, "notices", "catalog.json"), []byte(noticeCatalog("https://example.invalid", "example.invalid")), 0o600); err != nil {
		t.Fatal(err)
	}
	sources, _ := json.Marshal([]string{"transfer_test"})
	if _, err := svc.Store.DB.Exec(`INSERT INTO notice_subs(user_id,kind,sources,delivery_time,active,last_delivery,created_at,updated_at)
		VALUES('alice','transfer',?,?,1,'',?,?)`, string(sources), noticeTime, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Store.DB.Exec(`INSERT INTO notice_items(item_key,source_id,kind,title,url,fingerprint,first_seen,changed_at)
		VALUES('daily-notice','transfer_test','transfer','Transfer admission','https://example.invalid/notice','fingerprint',?,?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	svc.Clock = clock.Fixed{T: now}
	svc.Discord = sender
	return svc
}

func runDailyDeliveries(svc *application.Service) {
	ctx := context.Background()
	application.RunFortuneMorning(svc, ctx)
	application.DeliverNotices(svc, ctx)
}

func TestOvernightQuietHoursDeliverTheOriginalDailySlot(t *testing.T) {
	for _, tc := range []struct {
		zone string
		date time.Time
	}{
		{zone: "America/New_York", date: time.Date(2027, 3, 13, 23, 0, 0, 0, mustLocation(t, "America/New_York"))}, // DST starts overnight.
		{zone: "Asia/Seoul", date: time.Date(2026, 10, 5, 23, 0, 0, 0, mustLocation(t, "Asia/Seoul"))},
	} {
		t.Run(tc.zone, func(t *testing.T) {
			date := tc.date.Format("2006-01-02")
			sender := &scriptedDailySender{}
			svc := dailyDeliveryService(t, tc.date, tc.zone, date, "23:00", "23:00", sender)
			runDailyDeliveries(svc)
			if sender.sends != 0 {
				t.Fatalf("quiet hours did not defer the daily slot: sends=%d", sender.sends)
			}
			loc, _ := time.LoadLocation(tc.zone)
			morning := time.Date(tc.date.Year(), tc.date.Month(), tc.date.Day()+1, 7, 0, 0, 0, loc)
			svc.Clock = clock.Fixed{T: morning}
			runDailyDeliveries(svc)
			if sender.sends != 2 {
				t.Fatalf("daily delivery was skipped or duplicated: sends=%d", sender.sends)
			}
			var job struct {
				Date  string `json:"date"`
				State string `json:"state"`
			}
			var raw, lastSent, lastDelivery, outcome string
			if err := svc.Store.DB.QueryRow(`SELECT job,last_sent FROM fortune_profiles WHERE user_id='alice'`).Scan(&raw, &lastSent); err != nil {
				t.Fatal(err)
			}
			_ = json.Unmarshal([]byte(raw), &job)
			_ = svc.Store.DB.QueryRow(`SELECT last_delivery FROM notice_subs WHERE user_id='alice' AND kind='transfer'`).Scan(&lastDelivery)
			_ = svc.Store.DB.QueryRow(`SELECT state FROM notice_delivery_outcomes WHERE user_id='alice' AND kind='transfer' AND delivery_date=?`, date).Scan(&outcome)
			if job.Date != date || job.State != "sent" || lastSent != date || lastDelivery != date || outcome != "sent" {
				t.Fatalf("slot date changed across midnight: job=%+v fortune=%q notice=%q outcome=%q", job, lastSent, lastDelivery, outcome)
			}
		})
	}
}

func TestNewDailySubscriptionWaitsForItsFirstSlot(t *testing.T) {
	now := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	date := now.Format("2006-01-02")
	sender := &scriptedDailySender{}
	svc := dailyDeliveryService(t, now, "UTC", date, "23:00", "23:00", sender)
	runDailyDeliveries(svc)
	if sender.sends != 0 {
		t.Fatalf("new subscription received a prior-day delivery before its first slot: sends=%d", sender.sends)
	}
	svc.Clock = clock.Fixed{T: time.Date(2026, 10, 5, 23, 0, 0, 0, time.UTC)}
	runDailyDeliveries(svc)
	if sender.sends != 0 {
		t.Fatalf("quiet hours should still defer the first slot: sends=%d", sender.sends)
	}
	svc.Clock = clock.Fixed{T: time.Date(2026, 10, 6, 7, 0, 0, 0, time.UTC)}
	runDailyDeliveries(svc)
	if sender.sends != 2 {
		t.Fatalf("first subscription-day slot was skipped: sends=%d", sender.sends)
	}
	var lastNotice, lastFortune string
	if err := svc.Store.DB.QueryRow(`SELECT last_delivery FROM notice_subs WHERE user_id='alice' AND kind='transfer'`).Scan(&lastNotice); err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.DB.QueryRow(`SELECT last_sent FROM fortune_profiles WHERE user_id='alice'`).Scan(&lastFortune); err != nil {
		t.Fatal(err)
	}
	if lastNotice != date || lastFortune != date {
		t.Fatalf("first delivery used the wrong logical day: notice=%q fortune=%q want=%q", lastNotice, lastFortune, date)
	}
}

func TestUnknownDailyDMOutcomesAreNotRetriedButDefiniteFailuresAre(t *testing.T) {
	now := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	date := now.Format("2006-01-02")
	unknown := errors.New("connection reset after remote acceptance")
	sender := &scriptedDailySender{outcomes: []error{unknown, unknown}}
	svc := dailyDeliveryService(t, now, "UTC", date, "09:00", "09:00", sender)
	runDailyDeliveries(svc)
	svc.Clock = clock.Fixed{T: now.Add(3 * time.Minute)}
	runDailyDeliveries(svc)
	if sender.sends != 2 {
		t.Fatalf("unknown outcomes were sent again: sends=%d", sender.sends)
	}
	var raw, noticeState string
	if err := svc.Store.DB.QueryRow(`SELECT job FROM fortune_profiles WHERE user_id='alice'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var job struct {
		State string `json:"state"`
	}
	_ = json.Unmarshal([]byte(raw), &job)
	_ = svc.Store.DB.QueryRow(`SELECT state FROM notice_delivery_outcomes WHERE user_id='alice' AND kind='transfer' AND delivery_date=?`, date).Scan(&noticeState)
	if job.State != "unknown" || noticeState != "unknown" {
		t.Fatalf("ambiguous outcomes were not recorded: fortune=%q notice=%q", job.State, noticeState)
	}

	definite := &scriptedDailySender{outcomes: []error{application.ErrNotDelivered, application.ErrNotDelivered, nil, nil}}
	svc = dailyDeliveryService(t, now, "UTC", date, "09:00", "09:00", definite)
	runDailyDeliveries(svc)
	svc.Clock = clock.Fixed{T: now.Add(3 * time.Minute)}
	runDailyDeliveries(svc)
	if definite.sends != 4 {
		t.Fatalf("clear failures were not retried exactly once: sends=%d", definite.sends)
	}
	if err := svc.Store.DB.QueryRow(`SELECT job FROM fortune_profiles WHERE user_id='alice'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal([]byte(raw), &job)
	_ = svc.Store.DB.QueryRow(`SELECT state FROM notice_delivery_outcomes WHERE user_id='alice' AND kind='transfer' AND delivery_date=?`, date).Scan(&noticeState)
	if job.State != "sent" || noticeState != "sent" {
		t.Fatalf("definite failures did not settle after retry: fortune=%q notice=%q", job.State, noticeState)
	}
}

func mustLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}
