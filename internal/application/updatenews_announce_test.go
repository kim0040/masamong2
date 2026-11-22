package application_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/clock"
	"masamong/internal/config"
)

// newsGateway is a connected Discord that records channel posts and DMs, knows who shares a server
// with the bot, and can refuse a DM or run a hook after a send.
type newsGateway struct {
	postingGateway
	mu2       sync.Mutex
	offline   bool
	members   map[string]bool
	closed    map[string]bool // DMs Discord refuses for good
	dms       []string        // user|title|text
	afterSend func()
}

func (g *newsGateway) Connected() bool { return !g.offline }
func (g *newsGateway) SharedGuildMember(_ context.Context, id string) bool {
	return g.members[id]
}
func (g *newsGateway) SendDM(_ context.Context, userID string, card action.Card, _ string) error {
	g.mu2.Lock()
	defer g.mu2.Unlock()
	if g.closed[userID] {
		return application.ErrDMClosed
	}
	g.dms = append(g.dms, userID+"|"+card.Title+"|"+card.Text+"|"+card.Footer)
	if g.afterSend != nil {
		g.afterSend()
	}
	return nil
}
func (g *newsGateway) sent() (rooms []string, dms []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.mu2.Lock()
	defer g.mu2.Unlock()
	return append([]string(nil), g.posts...), append([]string(nil), g.dms...)
}

// newsFixture is an instance with rooms and people for the announcement:
//
//	c1 news on · c2 news off (default) · c3 news on but the bot does not answer there
//	u1 member, subscribed · u2 member, not subscribed · u3 subscribed, shares no server with the bot
//	u4 subscribed, blocked
type newsFixture struct {
	svc *application.Service
	gw  *newsGateway
}

func newNewsFixture(t *testing.T, running string) *newsFixture {
	t.Helper()
	t.Cleanup(application.UseVersionForTest(running))
	t.Cleanup(application.UseNewsLimitsForTest(0, 40))
	svc := setupSvc(t)
	serveAnyServer(t, svc)
	for _, ch := range []map[string]any{
		{"id": "c1", "respond": "mention", "update_news": "on"},
		{"id": "c2", "respond": "mention"},
		{"id": "c3", "respond": "off", "update_news": "on"},
	} {
		ch["guild_id"] = "g1"
		if r := exec(svc, "channels.configure", ch); !r.OK {
			t.Fatal(r.Error)
		}
	}
	applyDoc(t, svc, func(d *config.Document) {
		g := d.Guilds["g1"]
		g.Locale = "en"
		d.Guilds["g1"] = g
		d.Access.BlockedUsers = []string{"u4"}
	})
	subscribe := func(id string) {
		inv := &action.Invoker{Via: "command", GuildID: "g1", ChannelID: "c1", UserID: id}
		if r := as(svc, "updates.subscribe", map[string]any{"op": "on"}, inv); !r.OK {
			// a blocked person cannot subscribe through Discord: the console does it
			if r := exec(svc, "updates.subscribe", map[string]any{"op": "on", "user_id": id}); !r.OK {
				t.Fatal(r.Error)
			}
		}
	}
	for _, id := range []string{"u1", "u3", "u4"} {
		subscribe(id)
	}
	gw := &newsGateway{members: map[string]bool{"u1": true, "u2": true, "u4": true}}
	svc.SetDiscord(gw)
	svc.Clock = clock.Fixed{T: time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)}
	return &newsFixture{svc: svc, gw: gw}
}

func (f *newsFixture) run(t *testing.T) { f.svc.AnnounceUpdatesForTest(context.Background()) }

// The very first start of an installation only records the running version; nothing is said.
func TestUpdateNewsFirstStartSendsNothing(t *testing.T) {
	f := newNewsFixture(t, "v1.5.1")
	f.run(t)
	rooms, dms := f.gw.sent()
	if len(rooms)+len(dms) != 0 {
		t.Fatalf("first install sent %v %v", rooms, dms)
	}
	if f.svc.AnnouncedForTest() != "v1.5.1" {
		t.Fatalf("announced %q", f.svc.AnnouncedForTest())
	}
	f.run(t)
	if rooms, dms := f.gw.sent(); len(rooms)+len(dms) != 0 {
		t.Fatal("and nothing on the next pass")
	}
}

// An upgrade tells the rooms that turned news on and the people who subscribed and may be messaged,
// once. A restart on the same version, and any later pass, say nothing more.
func TestUpdateNewsUpgradeSendsOnceToWhoAsked(t *testing.T) {
	f := newNewsFixture(t, "v1.5.1")
	f.svc.SetAnnouncedForTest("v1.4.0")
	f.run(t)
	rooms, dms := f.gw.sent()
	if len(rooms) != 1 || !strings.HasPrefix(rooms[0], "c1|Updated to v1.5.1|") || !strings.Contains(rooms[0], "People memory (stage 0)") {
		t.Fatalf("rooms: %v", rooms)
	}
	if strings.Contains(rooms[0], "release check stopped") {
		t.Fatalf("operator items must not reach a chat room: %v", rooms[0])
	}
	if len(dms) != 1 || !strings.HasPrefix(dms[0], "u1|Updated to v1.5.1|") || !strings.Contains(dms[0], "/updates") {
		t.Fatalf("dms (u2 did not subscribe, u3 shares no server, u4 is blocked): %v", dms)
	}
	if got := f.svc.AnnouncedForTest(); got != "v1.5.1" {
		t.Fatalf("announced %q", got)
	}
	ledger := f.svc.NewsSentForTest("v1.5.1")
	if len(ledger) != 2 || ledger["room:g1:c1"] != "sent" || ledger["dm:u1"] != "sent" {
		t.Fatalf("ledger: %v", ledger)
	}
	f.run(t) // another pass
	f.svc.SetAnnouncedForTest("v1.4.0")
	f.run(t) // even with the record lost, the ledger holds
	if rooms2, dms2 := f.gw.sent(); len(rooms2) != 1 || len(dms2) != 1 {
		t.Fatalf("repeated: %v %v", rooms2, dms2)
	}
}

// Several versions skipped between two starts arrive as one card, newest first, user-facing items only.
func TestUpdateNewsMergesSkippedVersions(t *testing.T) {
	f := newNewsFixture(t, "v1.5.1")
	f.svc.SetAnnouncedForTest("v1.2.2")
	f.run(t)
	rooms, _ := f.gw.sent()
	if len(rooms) != 1 {
		t.Fatalf("one card, not one per version: %v", rooms)
	}
	text := rooms[0]
	i151, i140, i130 := strings.Index(text, "**v1.5.1**"), strings.Index(text, "**v1.4.0**"), strings.Index(text, "**v1.3.0**")
	if i151 < 0 || i140 < i151 || i130 < i140 {
		t.Fatalf("three versions, newest first: %q", text)
	}
	if strings.Contains(text, "**v1.2.2**") || strings.Contains(text, "**v1.1.1**") {
		t.Fatalf("only versions after the announced one: %q", text)
	}
}

// A release with nothing for chat members, a rebuild of the same version and an older build say
// nothing; the first records the version, the last leaves the high-water mark alone.
func TestUpdateNewsQuietCases(t *testing.T) {
	t.Run("no user-facing items", func(t *testing.T) {
		f := newNewsFixture(t, "v1.2.2")
		f.svc.SetAnnouncedForTest("v1.2.1")
		f.run(t)
		if rooms, dms := f.gw.sent(); len(rooms)+len(dms) != 0 || f.svc.AnnouncedForTest() != "v1.2.2" {
			t.Fatalf("%v %v %q", rooms, dms, f.svc.AnnouncedForTest())
		}
	})
	t.Run("an older build", func(t *testing.T) {
		f := newNewsFixture(t, "v1.4.0")
		f.svc.SetAnnouncedForTest("v1.5.1")
		f.run(t)
		if rooms, dms := f.gw.sent(); len(rooms)+len(dms) != 0 || f.svc.AnnouncedForTest() != "v1.5.1" {
			t.Fatalf("%v %v %q", rooms, dms, f.svc.AnnouncedForTest())
		}
	})
	for _, version := range []string{"1.0.0-dev", "v1.5.1-3-gabc1234", "v1.5.1-dirty", "v1.6.0-rc1"} {
		t.Run(version, func(t *testing.T) {
			f := newNewsFixture(t, version)
			f.svc.SetAnnouncedForTest("v1.4.0")
			f.run(t)
			if rooms, dms := f.gw.sent(); len(rooms)+len(dms) != 0 || f.svc.AnnouncedForTest() != "v1.4.0" {
				t.Fatalf("a build that is not a release announced: %v %v %q", rooms, dms, f.svc.AnnouncedForTest())
			}
		})
	}
	t.Run("the gateway is not ready", func(t *testing.T) {
		f := newNewsFixture(t, "v1.5.1")
		f.svc.SetAnnouncedForTest("v1.4.0")
		f.gw.offline = true
		f.run(t)
		if rooms, dms := f.gw.sent(); len(rooms)+len(dms) != 0 || f.svc.AnnouncedForTest() != "v1.4.0" {
			t.Fatalf("%v %v %q", rooms, dms, f.svc.AnnouncedForTest())
		}
		f.gw.offline = false
		f.run(t)
		if rooms, _ := f.gw.sent(); len(rooms) != 1 {
			t.Fatalf("it comes once the gateway is ready: %v", rooms)
		}
	})
}

// dm.allow decides who may be messaged; a subscriber it excludes gets nothing, and neither does anyone
// when it is off. A person's own language is used for their DM.
func TestUpdateNewsDMsFollowDMAllowAndLanguage(t *testing.T) {
	f := newNewsFixture(t, "v1.5.1")
	f.svc.SetAnnouncedForTest("v1.4.0")
	inv := &action.Invoker{Via: "command", GuildID: "g1", ChannelID: "c1", UserID: "u1"}
	if r := as(f.svc, "preferences.set", map[string]any{"locale": "ko"}, inv); !r.OK {
		t.Fatal(r.Error)
	}
	f.run(t)
	_, dms := f.gw.sent()
	if len(dms) != 1 || !strings.Contains(dms[0], "|v1.5.1 업데이트 소식|") || !strings.Contains(dms[0], "/업데이트 해지") {
		t.Fatalf("the DM speaks the person's language: %v", dms)
	}
	for _, mode := range []string{"off", "operators"} {
		g := newNewsFixture(t, "v1.5.1")
		g.svc.SetAnnouncedForTest("v1.4.0")
		applyDoc(t, g.svc, func(d *config.Document) { d.DM.Allow = mode })
		g.run(t)
		rooms, dms := g.gw.sent()
		if len(dms) != 0 || len(rooms) != 1 {
			t.Fatalf("dm.allow=%s: rooms %v dms %v", mode, rooms, dms)
		}
	}
}

// A person who unsubscribed before the announcement gets nothing; a DM Discord refuses for good is
// recorded and never tried again, and does not hold the announcement open.
func TestUpdateNewsUnsubscribedAndClosedDMs(t *testing.T) {
	f := newNewsFixture(t, "v1.5.1")
	f.svc.SetAnnouncedForTest("v1.4.0")
	inv := &action.Invoker{Via: "command", GuildID: "g1", ChannelID: "c1", UserID: "u1"}
	if r := as(f.svc, "updates.subscribe", map[string]any{"op": "off"}, inv); !r.OK {
		t.Fatal(r.Error)
	}
	f.run(t)
	if _, dms := f.gw.sent(); len(dms) != 0 {
		t.Fatalf("unsubscribed: %v", dms)
	}
	g := newNewsFixture(t, "v1.5.1")
	g.svc.SetAnnouncedForTest("v1.4.0")
	g.gw.closed = map[string]bool{"u1": true}
	g.run(t)
	if g.svc.NewsSentForTest("v1.5.1")["dm:u1"] != "failed" || g.svc.AnnouncedForTest() != "v1.5.1" {
		t.Fatalf("closed DM: %v announced %q", g.svc.NewsSentForTest("v1.5.1"), g.svc.AnnouncedForTest())
	}
	g.gw.closed = nil
	g.svc.SetAnnouncedForTest("v1.4.0")
	g.run(t)
	if _, dms := g.gw.sent(); len(dms) != 0 {
		t.Fatalf("a refused DM is not tried again: %v", dms)
	}
}

// A person in their quiet hours is not messaged then: the announcement waits (and is not recorded as
// done) until the window ends, then goes out once. The rooms already served are not served again.
func TestUpdateNewsWaitsOutQuietHours(t *testing.T) {
	f := newNewsFixture(t, "v1.5.1")
	f.svc.SetAnnouncedForTest("v1.4.0")
	inv := &action.Invoker{Via: "command", GuildID: "g1", ChannelID: "c1", UserID: "u1"}
	if r := as(f.svc, "preferences.set", map[string]any{"timezone": "Asia/Seoul", "quiet_start": "23:00", "quiet_end": "07:00"}, inv); !r.OK {
		t.Fatal(r.Error)
	}
	f.svc.Clock = clock.Fixed{T: time.Date(2026, 11, 22, 14, 30, 0, 0, time.UTC)} // 23:30 in Seoul
	f.run(t)
	rooms, dms := f.gw.sent()
	if len(rooms) != 1 || len(dms) != 0 || f.svc.AnnouncedForTest() != "v1.4.0" {
		t.Fatalf("quiet: rooms %v dms %v announced %q", rooms, dms, f.svc.AnnouncedForTest())
	}
	f.run(t)
	if r2, _ := f.gw.sent(); len(r2) != 1 {
		t.Fatal("the room is not told twice while waiting")
	}
	f.svc.Clock = clock.Fixed{T: time.Date(2026, 11, 22, 22, 5, 0, 0, time.UTC)} // 07:05 the next day
	f.run(t)
	rooms, dms = f.gw.sent()
	if len(rooms) != 1 || len(dms) != 1 || f.svc.AnnouncedForTest() != "v1.5.1" {
		t.Fatalf("after: rooms %v dms %v announced %q", rooms, dms, f.svc.AnnouncedForTest())
	}
}

// A crash between the claim and the send leaves a row: that send is not repeated. A pass stopped by
// shutdown carries on where it was, without sending twice, and records the version only when done.
func TestUpdateNewsNeverDoubleSendsAfterACrashOrStop(t *testing.T) {
	f := newNewsFixture(t, "v1.5.1")
	f.svc.SetAnnouncedForTest("v1.4.0")
	f.svc.ClaimNewsForTest("v1.5.1", "room:g1:c1") // the previous run died after this claim
	f.run(t)
	rooms, dms := f.gw.sent()
	if len(rooms) != 0 || len(dms) != 1 {
		t.Fatalf("the claimed room is not sent again: %v %v", rooms, dms)
	}

	g := newNewsFixture(t, "v1.5.1")
	g.svc.SetAnnouncedForTest("v1.4.0")
	for _, id := range []string{"u5", "u6"} {
		g.gw.members[id] = true
		if r := exec(g.svc, "updates.subscribe", map[string]any{"op": "on", "user_id": id}); !r.OK {
			t.Fatal(r.Error)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	g.gw.afterSend = func() { cancel() } // the service stops after the first DM
	g.svc.AnnounceUpdatesForTest(ctx)
	if g.svc.AnnouncedForTest() != "v1.4.0" {
		t.Fatalf("a stopped pass must not record the version: %q", g.svc.AnnouncedForTest())
	}
	g.gw.afterSend = nil
	g.run(t)
	rooms, dms = g.gw.sent()
	seen := map[string]int{}
	for _, d := range dms {
		seen[strings.SplitN(d, "|", 2)[0]]++
	}
	if len(rooms) != 1 || len(seen) != 3 || seen["u1"] != 1 || seen["u5"] != 1 || seen["u6"] != 1 || g.svc.AnnouncedForTest() != "v1.5.1" {
		t.Fatalf("resumed: rooms %v dms %v announced %q", rooms, dms, g.svc.AnnouncedForTest())
	}
}

// One pass sends at most its share; the next carries on with the rest and no one gets two.
func TestUpdateNewsSpreadsSendsOverPasses(t *testing.T) {
	f := newNewsFixture(t, "v1.5.1")
	f.svc.SetAnnouncedForTest("v1.4.0")
	for i := 0; i < 5; i++ {
		id := string(rune('a'+i)) + "x"
		f.gw.members[id] = true
		if r := exec(f.svc, "updates.subscribe", map[string]any{"op": "on", "user_id": id}); !r.OK {
			t.Fatal(r.Error)
		}
	}
	defer application.UseNewsLimitsForTest(0, 3)()
	f.run(t)
	rooms, dms := f.gw.sent()
	if len(rooms)+len(dms) != 3 || f.svc.AnnouncedForTest() != "v1.4.0" {
		t.Fatalf("first pass: %v %v %q", rooms, dms, f.svc.AnnouncedForTest())
	}
	f.run(t)
	f.run(t)
	rooms, dms = f.gw.sent()
	if len(rooms) != 1 || len(dms) != 6 || f.svc.AnnouncedForTest() != "v1.5.1" {
		t.Fatalf("later passes: rooms %v dms %v announced %q", rooms, dms, f.svc.AnnouncedForTest())
	}
}

// A paused room gets no card.
func TestUpdateNewsSkipsPausedRooms(t *testing.T) {
	f := newNewsFixture(t, "v1.5.1")
	f.svc.SetAnnouncedForTest("v1.4.0")
	if r := exec(f.svc, "runtime.pause", map[string]any{"scope": "channel", "guild_id": "g1", "channel_id": "c1", "paused": true}); !r.OK {
		t.Fatal(r.Error)
	}
	f.run(t)
	if rooms, _ := f.gw.sent(); len(rooms) != 0 {
		t.Fatalf("paused room: %v", rooms)
	}
}
