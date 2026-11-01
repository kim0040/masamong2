package application_test

import (
	"testing"

	"masamong/internal/application"
)

type fakeGateway struct{ bits int64 }

func (f *fakeGateway) Connected() bool                           { return true }
func (f *fakeGateway) BotUser() (string, string)                 { return "bot", "masamong" }
func (f *fakeGateway) Guilds() []application.GuildInfo           { return nil }
func (f *fakeGateway) Channels(string) []application.ChannelInfo { return nil }
func (f *fakeGateway) Permissions(string, string) (int64, bool)  { return f.bits, true }

func permAlerts(t *testing.T, svc *application.Service) map[string]string {
	t.Helper()
	got := map[string]string{}
	for _, a := range out(t, svc, "dashboard.overview", nil)["attention"].([]any) {
		m := a.(map[string]any)
		if k, _ := m["key"].(string); k == "discord_perms_missing" || k == "discord_perms_lost" {
			got[k] = m["severity"].(string)
		}
	}
	return got
}

func TestLostDiscordPermissionStaysAnAlertUntilFixed(t *testing.T) {
	svc := setupSvc(t)
	if r := exec(svc, "channels.configure", map[string]any{"guild_id": "g1", "id": "c1", "respond": "mention", "base_revision": 1}); !r.OK {
		t.Fatal(r.Error)
	}
	all := int64(1<<10 | 1<<11 | 1<<14 | 1<<16)
	gw := &fakeGateway{bits: all}
	svc.Discord = gw
	if got := permAlerts(t, svc); len(got) != 0 {
		t.Fatalf("full permissions raised %v", got)
	}
	perm := out(t, svc, "channels.permissions", map[string]any{"guild_id": "g1", "channel_id": "c1"})
	if perm["known"] != true || len(perm["missing"].([]any)) != 0 {
		t.Fatalf("permissions %v", perm)
	}
	gw.bits = all &^ (1 << 11) // someone removed Send Messages
	for i := 0; i < 3; i++ {   // reloading the dashboard must not downgrade the alert
		if got := permAlerts(t, svc); got["discord_perms_lost"] != "danger" {
			t.Fatalf("load %d: %v", i, got)
		}
	}
	gw.bits = 1 << 3 // administrator covers everything
	if got := permAlerts(t, svc); len(got) != 0 {
		t.Fatalf("administrator raised %v", got)
	}
}

func TestThreadPermissionsReportedWhileThreadsAreOn(t *testing.T) {
	svc := setupSvc(t)
	if r := exec(svc, "channels.configure", map[string]any{"guild_id": "g1", "id": "c1", "respond": "mention", "base_revision": 1}); !r.OK {
		t.Fatal(r.Error)
	}
	base := int64(1<<10 | 1<<11 | 1<<14 | 1<<16)
	gw := &fakeGateway{bits: base | 1<<38} // no Create Public Threads
	svc.Discord = gw
	missing := func() []any {
		return out(t, svc, "channels.permissions", map[string]any{"guild_id": "g1", "channel_id": "c1"})["thread_missing"].([]any)
	}
	if m := missing(); len(m) != 1 || m[0] != "create_public_threads" {
		t.Fatalf("default offer: %v", m)
	}
	gw.bits = base
	if m := missing(); len(m) != 2 {
		t.Fatalf("both missing: %v", m)
	}
	gw.bits = 1 << 3 // administrator covers everything
	if m := missing(); len(m) != 0 {
		t.Fatalf("administrator: %v", m)
	}
	gw.bits = base
	r := exec(svc, "config.patch", map[string]any{"base_revision": svc.Revision(), "ops": []any{
		map[string]any{"path": []any{"guilds", "g1", "channels", "c1", "conversation", "thread"}, "value": "off"}}})
	if !r.OK {
		t.Fatal(r.Error)
	}
	if m := missing(); len(m) != 0 {
		t.Fatalf("thread off: %v", m)
	}
}
