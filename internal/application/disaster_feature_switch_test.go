package application_test

import (
	"context"
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
	svc.SetDiscord(gw)
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
	// the stored switch is on (unset), the effective state is off, and the status names the master
	// feature as the reason — the console shows all three instead of a bare "off"
	st = out(t, svc, "disasters.alerts", nil)
	if st["configured"] != true || st["enabled"] != false || st["warnings"] != true || st["waiting"] != "feature_off" {
		t.Fatalf("the status must show configured-on, effective-off and the master switch: %v", st)
	}
	// the console's enable switch writes only disaster_alerts.enabled: the patch succeeds, and the
	// refreshed status must still say the master feature is what is off
	patch := func(path []any, value any) {
		t.Helper()
		if r := exec(svc, "config.patch", map[string]any{"ops": []any{map[string]any{"path": path, "value": value}}}); !r.OK {
			t.Fatal(r.Error)
		}
	}
	patch([]any{"disaster_alerts", "enabled"}, true)
	st = out(t, svc, "disasters.alerts", nil)
	if st["configured"] != true || st["enabled"] != false || st["waiting"] != "feature_off" {
		t.Fatalf("after the switch's save the status must keep naming the master feature: %v", st)
	}
	// the control: with the alerts switch itself off the reason is the switch, not the master feature
	patch([]any{"disaster_alerts", "enabled"}, false)
	st = out(t, svc, "disasters.alerts", nil)
	if st["configured"] != false || st["enabled"] != false || st["waiting"] != "off" {
		t.Fatalf("with the switch off the reason is the switch itself: %v", st)
	}
	patch([]any{"disaster_alerts", "enabled"}, true)
	// turning features.disasters on is what makes the stored switch take effect
	patch([]any{"features", "disasters"}, true)
	st = out(t, svc, "disasters.alerts", nil)
	if st["configured"] != true || st["enabled"] != true || st["waiting"] != "" {
		t.Fatalf("with the feature on the stored switch takes effect: %v", st)
	}
}
