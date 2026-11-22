package application_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"masamong/internal/application"
	"masamong/internal/clock"
	"masamong/internal/config"
)

// features.disasters is the switch setup turns off outside Korea. It stops the bot's own posts and the
// status Action says so, even though disaster_alerts.enabled is unset (on).
func TestDisasterFeatureSwitchStopsTheWatcher(t *testing.T) {
	feed := &kmaFeed{}
	srv := feed.server(t)
	svc := setupSvc(t)
	pointAt(t, svc, "kma", srv.URL, "KMA_TEST_KEY", "k")
	if r := exec(svc, "channels.configure", map[string]any{"guild_id": "g1", "id": "c1", "respond": "mention"}); !r.OK {
		t.Fatal(r.Error)
	}
	gw := &postingGateway{}
	svc.Discord = gw
	now := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	tick := func(d time.Duration) {
		now = now.Add(d)
		svc.Clock = clock.Fixed{T: now}
		application.DisasterTickForTest(svc, context.Background())
	}
	feed.quakes = []string{quake("202609300900", "경북 경주시 남남서쪽 9km 지역", 35.76, 129.19, 4.2, "")}
	tick(time.Minute) // starting point
	st := out(t, svc, "disasters.alerts", nil)
	if st["enabled"] != true || st["waiting"] != "" {
		t.Fatalf("with the feature on the watcher runs: %v", st)
	}

	applyDoc(t, svc, func(d *config.Document) { d.Features["disasters"] = false })
	feed.quakes = append(feed.quakes, quake("202610011000", "경북 경주시 남서쪽 10km 지역", 35.78, 129.17, 4.5, ""))
	tick(time.Minute)
	if len(gw.posts) != 0 {
		t.Fatalf("features.disasters=false must stop automatic posts: %v", gw.posts)
	}
	st = out(t, svc, "disasters.alerts", nil)
	if st["enabled"] != false || st["warnings"] != false || !strings.EqualFold(st["waiting"].(string), "off") {
		t.Fatalf("the status must show the switch: %v", st)
	}
}
