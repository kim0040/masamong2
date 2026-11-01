package application_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"masamong/internal/action"
	"masamong/internal/application"
)

// serverGateway is a connected gateway in a few servers that can leave one.
type serverGateway struct {
	mu     sync.Mutex
	guilds []application.GuildInfo
	left   []string
	fail   error
}

func (g *serverGateway) Connected() bool                           { return true }
func (g *serverGateway) BotUser() (string, string)                 { return "900000000000000009", "masamong" }
func (g *serverGateway) Channels(string) []application.ChannelInfo { return nil }
func (g *serverGateway) Permissions(string, string) (int64, bool) {
	return 1<<10 | 1<<11 | 1<<14 | 1<<16, true
}
func (g *serverGateway) Guilds() []application.GuildInfo {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]application.GuildInfo(nil), g.guilds...)
}
func (g *serverGateway) LeaveGuild(_ context.Context, id string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.fail != nil {
		return g.fail
	}
	g.left = append(g.left, id)
	for i, x := range g.guilds {
		if x.ID == id {
			g.guilds = append(g.guilds[:i], g.guilds[i+1:]...)
			break
		}
	}
	return nil
}

const (
	oldServer = "100000000000000001" // the bot was here before approval began
	setUp     = "100000000000000002" // has rooms set up
	stranger  = "100000000000000003" // invited later by someone
)

func member(guild, user string) *action.Invoker {
	return &action.Invoker{Via: "command", GuildID: guild, ChannelID: "300000000000000001", UserID: user, Name: "user" + user[len(user)-2:]}
}

func TestNewServersWaitForApproval(t *testing.T) {
	svc := setupSvc(t)
	gw := &serverGateway{guilds: []application.GuildInfo{{ID: oldServer, Name: "old friends"}}}
	svc.Discord = gw
	// the first connection approves the servers the bot is already in, once
	svc.SeedServers([]string{oldServer})
	if !svc.GuildAllowed(oldServer) {
		t.Fatal("a server the bot was in must stay served")
	}
	if r := exec(svc, "channels.configure", map[string]any{"guild_id": setUp, "id": "300000000000000002", "respond": "mention"}); !r.OK {
		t.Fatal(r.Error)
	}
	if !svc.GuildAllowed(setUp) {
		t.Fatal("a server with rooms set up is served")
	}
	// later the bot is invited somewhere new: it waits, and a second Ready does not approve it
	gw.mu.Lock()
	gw.guilds = append(gw.guilds, application.GuildInfo{ID: stranger, Name: "strangers", Members: 4000})
	gw.mu.Unlock()
	svc.SeedServers([]string{oldServer, stranger})
	if svc.GuildAllowed(stranger) {
		t.Fatal("a newly invited server must wait for approval")
	}
	if !svc.GuildAllowed("") {
		t.Fatal("direct messages have no server and are allowed")
	}

	// commands there get the notice; consent can still be withdrawn; operators pass
	someone := member(stranger, "400000000000000001")
	if r := as(svc, "help.version", nil, someone); r.OK || r.Error.MessageKey != "access.server_pending" {
		t.Fatalf("pending server: %+v", r)
	}
	if r := as(svc, "privacy.consent", map[string]any{"scope": "memory", "decision": "withdraw"}, someone); !r.OK {
		t.Fatalf("consent must stay withdrawable: %+v", r.Error)
	}
	if r := exec(svc, "access.policy", map[string]any{"operators": []string{"400000000000000009"}}); !r.OK {
		t.Fatal(r.Error)
	}
	if r := as(svc, "help.version", nil, member(stranger, "400000000000000009")); !r.OK {
		t.Fatalf("an operator is not held back: %+v", r.Error)
	}

	// the console lists it as waiting and the dashboard raises it
	list := out(t, svc, "channels.list", nil)
	if list["server_approval"] != true {
		t.Fatalf("approval on by default: %v", list["server_approval"])
	}
	states := map[string]bool{}
	for _, g := range list["guilds"].([]any) {
		m := g.(map[string]any)
		states[m["id"].(string)] = m["allowed"].(bool)
	}
	if states[stranger] || !states[oldServer] || !states[setUp] {
		t.Fatalf("server states: %v", states)
	}
	found := false
	for _, a := range out(t, svc, "dashboard.overview", nil)["attention"].([]any) {
		if a.(map[string]any)["key"] == "servers_pending" {
			found = true
		}
	}
	if !found {
		t.Fatal("no attention for the waiting server")
	}

	// approving serves it; revoking puts it back
	if r := exec(svc, "access.policy", map[string]any{"approve": stranger}); !r.OK || !svc.GuildAllowed(stranger) {
		t.Fatalf("approve: %+v", r)
	}
	if r := as(svc, "help.version", nil, someone); !r.OK {
		t.Fatalf("approved server: %+v", r.Error)
	}
	if r := exec(svc, "access.policy", map[string]any{"revoke": stranger}); !r.OK || svc.GuildAllowed(stranger) {
		t.Fatalf("revoke: %+v", r)
	}
	// switching approval off serves every server
	if r := exec(svc, "access.policy", map[string]any{"server_approval": false}); !r.OK || !svc.GuildAllowed(stranger) {
		t.Fatalf("approval off: %+v", r)
	}
	if r := exec(svc, "access.policy", map[string]any{"approve": "not-a-server"}); r.OK {
		t.Fatal("ids are validated")
	}
}

func TestDecliningAServerTakesTheBotOut(t *testing.T) {
	svc := setupSvc(t)
	gw := &serverGateway{guilds: []application.GuildInfo{{ID: stranger, Name: "strangers"}}}
	svc.Discord = gw
	// without confirm it only says what would happen
	r := exec(svc, "channels.leave", map[string]any{"guild_id": stranger})
	if !r.OK || r.Output["confirm_required"] != true || r.Output["name"] != "strangers" || len(gw.left) != 0 {
		t.Fatalf("plan: %+v %v", r, gw.left)
	}
	if r := exec(svc, "access.policy", map[string]any{"approve": stranger}); !r.OK {
		t.Fatal(r.Error)
	}
	if r := exec(svc, "channels.leave", map[string]any{"guild_id": stranger, "confirm": true}); !r.OK || r.Output["left"] != true {
		t.Fatalf("leave: %+v", r)
	}
	if len(gw.left) != 1 || svc.GuildAllowed(stranger) {
		t.Fatalf("left %v, still approved %v", gw.left, svc.GuildAllowed(stranger))
	}
	// not there any more, or Discord refuses: an error, nothing changed
	if r := exec(svc, "channels.leave", map[string]any{"guild_id": stranger, "confirm": true}); r.OK {
		t.Fatal("left a server the bot is not in")
	}
	gw.guilds = []application.GuildInfo{{ID: oldServer, Name: "old"}}
	gw.fail = errors.New("discord said no")
	if r := exec(svc, "channels.leave", map[string]any{"guild_id": oldServer, "confirm": true}); r.OK {
		t.Fatal("a refused leave must fail")
	}
	// the plan is not a change log row; the leave and the two refused attempts are
	rows := out(t, svc, "audit.list", map[string]any{"action": "channels.leave"})["entries"].([]any)
	ok := 0
	for _, x := range rows {
		if x.(map[string]any)["ok"] == true {
			ok++
		}
	}
	if len(rows) != 3 || ok != 1 {
		t.Fatalf("change log rows for leave: %d (%d ok)", len(rows), ok)
	}
}

func TestServerManagersRunTheirServerFromDiscord(t *testing.T) {
	svc := setupSvc(t)
	serveAnyServer(t, svc)
	if r := exec(svc, "access.policy", map[string]any{"operators": []string{"400000000000000009"}}); !r.OK {
		t.Fatal(r.Error)
	}
	manager := member(oldServer, "400000000000000001")
	manager.Manager = true
	troll := "400000000000000002"

	// block someone in this server only, by mention
	r := as(svc, "access.policy", map[string]any{"op": "block", "user": "<@!" + troll + ">"}, manager)
	if !r.OK {
		t.Fatalf("block: %+v", r.Error)
	}
	if !svc.BlockedIn(oldServer, troll) || svc.BlockedIn(setUp, troll) || svc.Blocked(troll) {
		t.Fatal("the block must hold in this server only")
	}
	if r := as(svc, "help.version", nil, member(oldServer, troll)); r.OK || r.Error.MessageKey != "access.blocked" {
		t.Fatalf("blocked here: %+v", r)
	}
	if r := as(svc, "help.version", nil, member(setUp, troll)); !r.OK {
		t.Fatalf("not blocked elsewhere: %+v", r.Error)
	}
	// the bot, oneself and operators cannot be blocked; a missing target is explained
	for _, who := range []string{"900000000000000009", "400000000000000001", "400000000000000009"} {
		svc.Discord = &serverGateway{}
		if r := as(svc, "access.policy", map[string]any{"op": "block", "user": who}, manager); r.OK || r.Error.MessageKey != "access.cannot_block" {
			t.Fatalf("blocking %s: %+v", who, r)
		}
	}
	if r := as(svc, "access.policy", map[string]any{"op": "block", "user": "someone"}, manager); r.OK || r.Error.MessageKey != "access.need_user" {
		t.Fatalf("no target: %+v", r)
	}
	if r := as(svc, "access.policy", map[string]any{"op": "unblock", "user": troll}, manager); !r.OK || svc.BlockedIn(oldServer, troll) {
		t.Fatalf("unblock: %+v", r)
	}

	// memory and chiming in for this channel
	if r := as(svc, "access.policy", map[string]any{"op": "memory", "mode": "on"}, manager); !r.OK || r.Output["capture"] != true {
		t.Fatalf("memory on: %+v", r)
	}
	if r := as(svc, "access.policy", map[string]any{"op": "chime", "mode": "shadow"}, manager); !r.OK || r.Output["participation"] != "shadow" {
		t.Fatalf("chime: %+v", r)
	}
	if r := as(svc, "access.policy", map[string]any{"op": "chime", "mode": "loud"}, manager); r.OK || r.Error.MessageKey != "access.bad_mode" {
		t.Fatalf("bad mode: %+v", r)
	}
	// a plain member cannot
	if r := as(svc, "access.policy", map[string]any{"op": "memory", "mode": "off"}, member(oldServer, "400000000000000003")); r.OK {
		t.Fatal("a member changed memory")
	}

	// every change made from Discord says who, where and how in the change log
	rows := out(t, svc, "audit.list", map[string]any{"via": "discord", "limit": 50})["entries"].([]any)
	if len(rows) < 4 {
		t.Fatalf("discord changes logged: %d", len(rows))
	}
	for _, x := range rows {
		e := x.(map[string]any)
		if e["actor_id"] != "discord:400000000000000001" || e["actor"] != "user01" || e["guild_id"] != oldServer {
			t.Fatalf("change without who/where: %v", e)
		}
	}
	// the status view writes nothing
	before := len(out(t, svc, "audit.list", map[string]any{"limit": 500})["entries"].([]any))
	if r := as(svc, "access.policy", map[string]any{"op": "status"}, manager); !r.OK {
		t.Fatal(r.Error)
	}
	if after := len(out(t, svc, "audit.list", map[string]any{"limit": 500})["entries"].([]any)); after != before {
		t.Fatalf("status wrote %d change log rows", after-before)
	}
	// the console can see and lift a server's blocks
	if r := as(svc, "access.policy", map[string]any{"op": "block", "user": troll}, manager); !r.OK {
		t.Fatal(r.Error)
	}
	pol := out(t, svc, "access.policy", nil)
	if gb := pol["guild_blocked"].(map[string]any)[oldServer].([]any); len(gb) != 1 || gb[0] != troll {
		t.Fatalf("guild blocks: %v", pol["guild_blocked"])
	}
	if r := exec(svc, "access.policy", map[string]any{"guild_id": oldServer, "unblock": troll}); !r.OK || svc.BlockedIn(oldServer, troll) {
		t.Fatalf("console unblock: %+v", r)
	}
}

func TestChangeLogFiltersAndPages(t *testing.T) {
	svc := setupSvc(t)
	for i := 0; i < 7; i++ {
		if r := exec(svc, "access.policy", map[string]any{"guild_admins": i%2 == 0}); !r.OK {
			t.Fatal(r.Error)
		}
	}
	if r := exec(svc, "access.policy", map[string]any{"operators": []string{"bad"}}); r.OK {
		t.Fatal("expected a refusal")
	}
	first := out(t, svc, "audit.list", map[string]any{"action": "access.policy", "limit": 3})
	if len(first["entries"].([]any)) != 3 || first["next_before_id"] == nil {
		t.Fatalf("first page: %v", first)
	}
	seen := 3
	next := first["next_before_id"]
	for next != nil {
		page := out(t, svc, "audit.list", map[string]any{"action": "access.policy", "limit": 3, "before_id": next})
		seen += len(page["entries"].([]any))
		next = page["next_before_id"]
	}
	if seen != 8 {
		t.Fatalf("paged through %d rows, want 8", seen)
	}
	failed := out(t, svc, "audit.list", map[string]any{"action": "access.policy", "result": "failed"})["entries"].([]any)
	if len(failed) != 1 || failed[0].(map[string]any)["ok"] != false {
		t.Fatalf("failed filter: %v", failed)
	}
	if n := len(out(t, svc, "audit.list", map[string]any{"via": "discord"})["entries"].([]any)); n != 0 {
		t.Fatalf("no Discord changes here, got %d", n)
	}
}

func TestInviteAsksOnlyForWhatTheBotUses(t *testing.T) {
	svc := setupSvc(t)
	svc.Discord = &serverGateway{}
	r := exec(svc, "channels.invite", map[string]any{"guild_id": oldServer})
	if !r.OK {
		t.Fatal(r.Error)
	}
	u := r.Output["url"].(string)
	// view, send, embed, attach, history, create threads, threads, polls — never Administrator (8)
	if !strings.Contains(u, "permissions=563259191184384") || !strings.Contains(u, "client_id=900000000000000009") {
		t.Fatalf("invite: %s", u)
	}
	if !strings.Contains(u, "guild_id="+oldServer+"&disable_guild_select=true") {
		t.Fatalf("no preselected server: %s", u)
	}
	if u2 := out(t, svc, "channels.invite", nil)["url"].(string); strings.Contains(u2, "guild_id=") {
		t.Fatalf("a plain invite picks no server: %s", u2)
	}
}
