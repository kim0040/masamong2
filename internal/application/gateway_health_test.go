package application_test

import (
	"strings"
	"testing"

	"masamong/internal/application"
	"masamong/internal/config"
)

// healthGateway is a connected gateway that reports a fixed health snapshot.
type healthGateway struct{ health application.GatewayHealth }

func (g *healthGateway) Connected() bool                           { return g.health.Connected }
func (g *healthGateway) BotUser() (string, string)                 { return "", "" }
func (g *healthGateway) Guilds() []application.GuildInfo           { return nil }
func (g *healthGateway) Channels(string) []application.ChannelInfo { return nil }
func (g *healthGateway) Permissions(string, string) (int64, bool)  { return 0, false }
func (g *healthGateway) GatewayHealth() application.GatewayHealth  { return g.health }

func TestChannelsListCarriesGatewayHealth(t *testing.T) {
	svc := setupSvc(t)
	// a gateway that does not report health leaves the field out, so the console shows nothing extra
	svc.Discord = &startupDoctorGateway{}
	if _, has := exec(svc, "channels.list", nil).Output["gateway"]; has {
		t.Fatal("a gateway without health reporting must not add a gateway field")
	}
	want := application.GatewayHealth{State: application.GatewayOK, Connected: true, LatencyMs: 42, AckAgeSec: 7, EventAgeSec: 300, Ready: 1, Resumed: 2, Disconnects: 2}
	svc.Discord = &healthGateway{health: want}
	res := exec(svc, "channels.list", nil)
	if !res.OK {
		t.Fatalf("channels.list: %+v", res.Error)
	}
	got, ok := res.Output["gateway"].(application.GatewayHealth)
	if !ok || got != want {
		t.Fatalf("gateway field = %#v, want %#v", res.Output["gateway"], want)
	}
}

func TestDoctorWarnsWhenTheGatewayLooksStuck(t *testing.T) {
	svc := setupSvc(t)
	applyDoc(t, svc, func(d *config.Document) { d.Discord.Enabled = true })
	discordOf := func(h application.GatewayHealth) (map[string]any, []map[string]any) {
		svc.Discord = &healthGateway{health: h}
		res := exec(svc, "runtime.doctor", nil)
		if !res.OK {
			t.Fatalf("doctor: %+v", res.Error)
		}
		return res.Output["features"].(map[string]any)["discord"].(map[string]any), res.Output["attention"].([]map[string]any)
	}
	hasAttention := func(items []map[string]any) bool {
		for _, a := range items {
			if a["item"] == "discord" {
				return true
			}
		}
		return false
	}

	stuck, attention := discordOf(application.GatewayHealth{State: application.GatewayCheck, Connected: true, AckAgeSec: 400})
	if stuck["status"] != "degraded" || stuck["reason"] != "gateway_stale" || stuck["reason_text"] == nil || !hasAttention(attention) {
		t.Fatalf("a connected gateway with a stale ACK must warn: %+v attention=%+v", stuck, attention)
	}
	for _, state := range []string{application.GatewayOK, application.GatewayUnknown} {
		fine, attention := discordOf(application.GatewayHealth{State: state, Connected: true})
		if fine["status"] != "ready" || hasAttention(attention) {
			t.Fatalf("state %q must not warn: %+v attention=%+v", state, fine, attention)
		}
	}
}

// reconcilingGateway is a connected gateway that also reports quarantined recovery files.
type reconcilingGateway struct {
	healthGateway
	blocked int
}

func (g *reconcilingGateway) ReconciliationBlocked() int { return g.blocked }

// A quarantined Discord edit/delete file used to show only in the log. The doctor and the console's
// notifications must show it too, with the count and no path, id or content.
func TestDoctorAndAttentionShowBlockedReconciliation(t *testing.T) {
	svc := setupSvc(t)
	applyDoc(t, svc, func(d *config.Document) { d.Discord.Enabled = true })
	gw := &reconcilingGateway{healthGateway: healthGateway{health: application.GatewayHealth{State: application.GatewayOK, Connected: true}}}
	svc.Discord = gw

	doctorItem := func() map[string]any {
		res := exec(svc, "runtime.doctor", nil)
		if !res.OK {
			t.Fatalf("doctor: %+v", res.Error)
		}
		for _, a := range res.Output["attention"].([]map[string]any) {
			if a["item"] == "discord_reconciliation" {
				return a
			}
		}
		return nil
	}
	bellItem := func() map[string]any {
		for _, a := range out(t, svc, "dashboard.overview", nil)["attention"].([]any) {
			if m := a.(map[string]any); m["key"] == "discord_reconciliation" {
				return m
			}
		}
		return nil
	}

	if doctorItem() != nil || bellItem() != nil {
		t.Fatal("nothing is blocked, so nothing must be reported")
	}
	gw.blocked = 3
	d := doctorItem()
	if d == nil || d["reason"] != "reconciliation_blocked:3" {
		t.Fatalf("doctor attention = %+v", d)
	}
	if text, _ := d["reason_text"].(string); text == "" || text == "features.reason.reconciliation_blocked" || !strings.Contains(text, "3") {
		t.Fatalf("reason_text must be the localized sentence with the count, got %q", text)
	}
	// the console's Doctor tab lists features, so the same state must be a feature row there
	if f, _ := exec(svc, "runtime.doctor", nil).Output["features"].(map[string]any)["discord_reconciliation"].(map[string]any); f == nil || f["status"] != "degraded" {
		t.Fatalf("doctor feature row = %+v", f)
	}
	b := bellItem()
	if b == nil || b["severity"] != "warn" || b["args"].(map[string]any)["n"] != float64(3) {
		t.Fatalf("bell attention = %+v", b)
	}

	// a gateway that cannot report (older or test gateways) adds nothing
	svc.Discord = &healthGateway{health: gw.health}
	if doctorItem() != nil || bellItem() != nil {
		t.Fatal("a gateway without a reconciliation reporter must not add an item")
	}
}
