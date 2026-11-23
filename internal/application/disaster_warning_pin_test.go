package application_test

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"masamong/internal/application"
	"masamong/internal/clock"
	"masamong/internal/config"
)

// warningAt is a warning row that takes effect at the given time (yyyymmddhhmm).
func warningAt(regID, parent, region, kind, level, cmd, effective string) string {
	return strings.Replace(warning(regID, parent, region, kind, level, cmd), `"TM_EF":"202610011200"`, `"TM_EF":"`+effective+`"`, 1)
}

// TestDisasterWarningCardsArePinned fixes, card by card and in order, what a sequence of checks
// posts and what it records as told: one card per kind, level, change and effective time with its
// regions sorted, lifts grouped without a time, sea areas left out while marine is off, an upgrade
// naming the level it came from, and deliveries recorded only for the level in force.
func TestDisasterWarningCardsArePinned(t *testing.T) {
	svc := setupSvc(t)
	feed := &kmaFeed{}
	pointAt(t, svc, "kma", feed.server(t).URL, "KMA_TEST_KEY", "k")
	rooms := map[string]map[string]int{
		"a": {"L1": 4, "L2": 4, "L3": 4, "S1": 4},
		"b": {"L2": 3},
	}
	for _, id := range []string{"a", "b"} {
		if r := exec(svc, "channels.configure", map[string]any{"guild_id": "g1", "id": id, "respond": "mention", "watch_level": rooms[id]}); !r.OK {
			t.Fatal(r.Error)
		}
	}
	applyDoc(t, svc, func(d *config.Document) {
		g := d.Guilds["g1"]
		g.Locale = "ko"
		d.Guilds["g1"] = g
	})
	gw := &postingGateway{}
	svc.SetDiscord(gw)
	now := time.Date(2026, 10, 1, 3, 0, 0, 0, time.UTC)
	check := func(rows []string, wantPosts []string, wantSent map[string]map[string]string) {
		t.Helper()
		feed.mu.Lock()
		feed.warnings = rows
		feed.mu.Unlock()
		before := len(gw.posts)
		now = now.Add(5 * time.Minute)
		svc.Clock = clock.Fixed{T: now}
		application.DisasterTickForTest(svc, context.Background())
		got := append([]string{}, gw.posts[before:]...)
		if len(got) == 0 {
			got = nil
		}
		if !reflect.DeepEqual(got, wantPosts) {
			t.Fatalf("posts:\n got %q\nwant %q", got, wantPosts)
		}
		if sent := storedDeliveries(t, svc); !reflect.DeepEqual(sent, wantSent) {
			t.Fatalf("told:\n got %v\nwant %v", sent, wantSent)
		}
	}
	rainPohang := warning("L2", "경상북도", "포항시", "호우", "경보", "발표")
	rainGyeongsan := warning("L1", "경상북도", "경산시", "호우", "경보", "발표")
	rainYeongdeok := warningAt("L3", "경상북도", "영덕군", "호우", "경보", "발표", "202610011300")
	snowAdvisory := warning("L1", "경상북도", "경산시", "대설", "주의", "발표")
	snowWarning := warning("L1", "경상북도", "경산시", "대설", "경보", "변경")
	sea := warning("S1", "동해남부전해상", "먼바다", "풍랑", "경보", "발표")

	check(nil, nil, nil) // the first check is the starting point
	check([]string{rainPohang, rainGyeongsan, rainYeongdeok, snowAdvisory, sea}, []string{
		"a|⚠️ 대설주의보 발표|경산시\n발효: 10-01 12:00",
		"a|⚠️ 호우경보 발표|경산시, 포항시\n발효: 10-01 12:00",
		"b|⚠️ 호우경보 발표|포항시\n발효: 10-01 12:00",
		"a|⚠️ 호우경보 발표|영덕군\n발효: 10-01 13:00",
	}, map[string]map[string]string{
		"L1|R": {"a": "3"}, "L1|S": {"a": "2"}, "L2|R": {"a": "3", "b": "3"}, "L3|R": {"a": "3"},
	})
	check([]string{rainPohang, rainYeongdeok, snowWarning, sea}, []string{
		"a|⚠️ 대설 주의보 → 경보 격상|경산시\n발효: 10-01 12:00",
		"a|✅ 호우경보 해제|경산시",
	}, map[string]map[string]string{
		"L1|S": {"a": "3"}, "L2|R": {"a": "3", "b": "3"}, "L3|R": {"a": "3"},
	})
	check([]string{rainPohang, rainYeongdeok, snowWarning, sea}, nil, map[string]map[string]string{
		"L1|S": {"a": "3"}, "L2|R": {"a": "3", "b": "3"}, "L3|R": {"a": "3"},
	})
	check(nil, []string{
		"a|✅ 대설경보 해제|경산시",
		"a|✅ 호우경보 해제|영덕군, 포항시",
		"b|✅ 호우경보 해제|포항시",
	}, nil)
}
