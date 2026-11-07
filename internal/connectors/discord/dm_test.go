package discord

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/clock"
	"masamong/internal/config"
)

func configureDMTest(t *testing.T, svc *application.Service, dm config.DMSettings, operators bool) {
	t.Helper()
	raw, _ := config.Marshal(svc.Doc())
	doc, err := config.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	doc.DM = dm
	if operators {
		doc.Access.Operators = []string{"11111"}
	} else {
		doc.Access.Operators = nil
	}
	raw, _ = config.Marshal(doc)
	res := svc.Execute(context.Background(), action.Request{Action: "config.apply", Input: mustJSON(map[string]any{"yaml": string(raw), "base_revision": svc.Revision()})})
	if !res.OK {
		t.Fatal("cannot configure DM fixture")
	}
}

func dmGateway(t *testing.T, model bool) (*Gateway, *fakeDiscord) {
	t.Helper()
	svc := svcForTest(t)
	if model {
		svc = gatewayBanterService(t)
	}
	svc.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	configureDMTest(t, svc, config.DMSettings{}, true)
	SetCatalog(svc.I18n)
	sess, err := discordgo.New("Bot synthetic")
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeDiscord{}
	sess.Client = &http.Client{Transport: fake}
	g := &Gateway{svc: svc, sess: sess, ctx: context.Background(), log: svc.Log, ActionTimeout: 10 * time.Second, cfg: config.Discord{MessageContent: true, CommandPrefix: "!"}}
	svc.Discord = g
	g.merge.fire = g.answerHeld
	g.merge.forget = g.forgetFollow
	return g, fake
}

func dmMessage(id, text string) *discordgo.MessageCreate {
	return &discordgo.MessageCreate{Message: &discordgo.Message{ID: id, ChannelID: "dm-fixture", Content: text, Author: &discordgo.User{ID: "11111"}, Timestamp: time.Now()}}
}

func waitDMWork(g *Gateway) { g.inflight.Wait() }

// sendDM delivers a plain DM and releases the merger's hold, so its turn (or notice) runs now.
func sendDM(g *Gateway, m *discordgo.MessageCreate) {
	g.onMessage(g.sess, m)
	flushDM(g, m)
	waitDMWork(g)
}

func flushDM(g *Gateway, m *discordgo.MessageCreate) {
	g.merge.mu.Lock()
	key := mergeKey(m)
	p := g.merge.pending[key]
	var generation uint64
	if p != nil {
		p.timer.Stop()
		generation = p.generation
	}
	g.merge.mu.Unlock()
	if p != nil {
		g.merge.flush(key, p, generation)
	}
	waitDMWork(g)
}

func dmPosts(fake *fakeDiscord) []recordedCall {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	var out []recordedCall
	for _, c := range fake.calls {
		if c.method == http.MethodPost && strings.HasSuffix(c.path, "/channels/dm-fixture/messages") {
			out = append(out, c)
		}
	}
	return out
}

func TestDMGatewayPlainMessageAnsweredAndFragmentsMerged(t *testing.T) {
	g, fake := dmGateway(t, true)
	first, second := dmMessage("dm-first", "Synthetic first fragment"), dmMessage("dm-second", "Synthetic second fragment")
	// Direct messages carry content even without the guild message-content intent.
	g.cfg.MessageContent = false
	g.onMessage(g.sess, first)
	waitDMWork(g)
	g.onMessage(g.sess, second)
	waitDMWork(g)
	flushDM(g, second)
	posts := dmPosts(fake)
	if len(posts) != 1 || !strings.Contains(posts[0].body, `"content":"reply"`) {
		t.Fatal("plain DM was not answered once")
	}
	var turns, captured int
	if err := g.svc.Store.DB.QueryRow(`SELECT COUNT(*) FROM chat_turns WHERE channel_key='discord::dm-fixture'`).Scan(&turns); err != nil || turns != 2 {
		t.Fatal("DM short-term context missing")
	}
	if err := g.svc.Store.DB.QueryRow(`SELECT COUNT(*) FROM memory_observations WHERE room='discord:0:dm-fixture'`).Scan(&captured); err != nil || captured != 0 {
		t.Fatal("DM entered captured memory")
	}
	call := g.heldCall([]*discordgo.MessageCreate{first, second})
	if !call.addressed || call.nameOnly || call.followUp {
		t.Fatal("DM entered name/follow-up gate")
	}
	g.capture(first, config.ChannelPolicy{Capture: true})
	if err := g.svc.Store.DB.QueryRow(`SELECT COUNT(*) FROM memory_observations WHERE room='discord:0:dm-fixture'`).Scan(&captured); err != nil || captured != 0 {
		t.Fatal("direct capture admitted DM")
	}
}

func TestDMGatewayClosedNoticeDailyCommandsStillRunAndBlockedSilent(t *testing.T) {
	g, fake := dmGateway(t, false)
	configureDMTest(t, g.svc, config.DMSettings{Allow: "off"}, false)
	at := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	g.svc.Clock = clock.Fixed{T: at}
	for _, id := range []string{"closed-one", "closed-two"} {
		sendDM(g, dmMessage(id, "Synthetic request"))
	}
	if len(dmPosts(fake)) != 1 {
		t.Fatal("closed DM must have one daily notice")
	}
	g.onMessage(g.sess, dmMessage("command-one", "!help"))
	waitDMWork(g)
	if len(dmPosts(fake)) != 2 {
		t.Fatal("DM command stopped when chat was off")
	}
	g.svc.Clock = clock.Fixed{T: at.Add(24 * time.Hour)}
	sendDM(g, dmMessage("closed-next-day", "Synthetic request"))
	if len(dmPosts(fake)) != 3 {
		t.Fatal("notice did not reset next day")
	}
	raw, _ := config.Marshal(g.svc.Doc())
	doc, _ := config.Parse(raw)
	doc.Access.BlockedUsers = []string{"11111"}
	raw, _ = config.Marshal(doc)
	res := g.svc.Execute(context.Background(), action.Request{Action: "config.apply", Input: mustJSON(map[string]any{"yaml": string(raw), "base_revision": g.svc.Revision()})})
	if !res.OK {
		t.Fatal("cannot block fixture")
	}
	g.onMessage(g.sess, dmMessage("blocked", "!help"))
	waitDMWork(g)
	if len(dmPosts(fake)) != 3 {
		t.Fatal("blocked person received a DM")
	}
	var turns int
	if err := g.svc.Store.DB.QueryRow(`SELECT COUNT(*) FROM turn_log`).Scan(&turns); err != nil || turns != 0 {
		t.Fatal("closed DM created a conversation turn")
	}
}

func TestDMGatewayMembershipStateRESTCacheAndServerApproval(t *testing.T) {
	g, fake := dmGateway(t, false)
	configureDMTest(t, g.svc, config.DMSettings{}, false)
	// An unapproved guild must not authorize a DM, even when state knows the member.
	if err := g.sess.State.GuildAdd(&discordgo.Guild{ID: "55555", Members: []*discordgo.Member{{User: &discordgo.User{ID: "11111"}}}}); err != nil {
		t.Fatal(err)
	}
	if g.SharedGuildMember(context.Background(), "11111") {
		t.Fatal("unapproved guild allowed DM")
	}
	res := g.svc.Execute(context.Background(), action.Request{Action: "access.policy", Input: mustJSON(map[string]any{"servers": []string{"55555"}})})
	if !res.OK {
		t.Fatal("cannot approve fixture guild")
	}
	if !g.SharedGuildMember(context.Background(), "11111") || len(fake.calls) != 0 {
		t.Fatal("state membership not preferred")
	}
	fake.responses = map[string]fakeDiscordResponse{"/members/22222": {status: 200, body: `{"user":{"id":"22222"}}`}, "/members/33333": {status: 404, body: `{"code":10007,"message":"Unknown Member"}`}}
	for i := 0; i < 2; i++ {
		if !g.SharedGuildMember(context.Background(), "22222") {
			t.Fatal("REST member not allowed")
		}
		if g.SharedGuildMember(context.Background(), "33333") {
			t.Fatal("REST non-member allowed")
		}
	}
	fake.mu.Lock()
	calls := len(fake.calls)
	fake.mu.Unlock()
	if calls != 2 {
		t.Fatal("positive/negative membership not cached")
	}
	sendDM(g, &discordgo.MessageCreate{Message: &discordgo.Message{ID: "nonmember", ChannelID: "dm-fixture", Content: "Synthetic request", Author: &discordgo.User{ID: "33333"}}})
	if len(dmPosts(fake)) != 1 {
		t.Fatal("nonmember did not receive notice")
	}
	configureDMTest(t, g.svc, config.DMSettings{Allow: "operators"}, false)
	sendDM(g, dmMessage("nonoperator", "Synthetic request"))
	if len(dmPosts(fake)) != 2 {
		t.Fatal("operators-only did not refuse known member")
	}
}

func TestDMGatewayLimitsNoticeOncePerKindAndUnansweredPrefixConversation(t *testing.T) {
	g, fake := dmGateway(t, true)
	configureDMTest(t, g.svc, config.DMSettings{PerUserPerDay: 1, PerDay: 2}, true)
	// The unconfigured weather tool keeps its result and becomes one DM conversation.
	cmd, ok := ByName("weather")
	if !ok {
		t.Fatal("weather command missing")
	}
	m := dmMessage("weather-dm", "!weather Synthetic")
	g.runCommand(g.sess, m, cmd, map[string]string{"region": "Synthetic"})
	waitDMWork(g)
	posts := dmPosts(fake)
	if len(posts) != 1 || !strings.Contains(posts[0].body, `"content":"reply"`) {
		t.Fatal("unanswered DM prefix command did not continue")
	}
	for _, id := range []string{"limit-one", "limit-two"} {
		g.onMessage(g.sess, dmMessage(id, "Synthetic request"))
		waitDMWork(g)
		flushDM(g, dmMessage(id, "Synthetic request"))
	}
	if len(dmPosts(fake)) != 2 {
		t.Fatal("user quota notices repeated")
	}
	configureDMTest(t, g.svc, config.DMSettings{PerUserPerDay: -1, PerDay: 1}, true)
	g.onMessage(g.sess, dmMessage("global-limit", "Synthetic request"))
	waitDMWork(g)
	flushDM(g, dmMessage("global-limit", "Synthetic request"))
	if len(dmPosts(fake)) != 3 {
		t.Fatal("global quota did not get a separate notice")
	}
	var n int
	if err := g.svc.Store.DB.QueryRow(`SELECT n FROM tool_usage WHERE tool='dm.chat' AND scope='all'`).Scan(&n); err != nil || n != 1 {
		t.Fatal("refused DM turns counted")
	}
}

// A DM turn whose model call fails used to leave the person without any answer (the error was only
// logged). It now gets the short failure notice, once.
func TestDMTurnThatFailsSaysSo(t *testing.T) {
	g, fake := dmGateway(t, true)
	doc := g.svc.Doc()
	raw, _ := config.Marshal(doc)
	broken, err := config.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	p := broken.Providers["gateway-test"]
	p.BaseURL = "http://127.0.0.1:1" // nothing listens there
	broken.Providers["gateway-test"] = p
	raw, _ = config.Marshal(broken)
	if res := g.svc.Execute(context.Background(), action.Request{Action: "config.apply", Input: mustJSON(map[string]any{"yaml": string(raw), "base_revision": g.svc.Revision()})}); !res.OK {
		t.Fatal("cannot break the fixture's model")
	}
	m := dmMessage("dm-fail", "Synthetic request")
	sendDM(g, m)
	posts := dmPosts(fake)
	want, _ := json.Marshal(messages.T(g.noticeLocale(m), "discord.chat.failed"))
	if len(posts) != 1 || !strings.Contains(posts[0].body, strings.Trim(string(want), `"`)) {
		t.Fatalf("a failed turn must say so once: %d posts", len(posts))
	}
}

// A DM reply of two short paragraphs goes out as two bubbles, like a reply in a server channel (DMs
// had no reply shape and were always sent whole).
func TestDMReplySplitsIntoBubbles(t *testing.T) {
	g, fake := dmGateway(t, false)
	m := dmMessage("dm-bubbles", "Synthetic request")
	sent, ids, rep := g.sendShaped(context.Background(), g.sess, "", "dm-fixture", m.Reference(), "first line\nsecond line", "", g.svc.ChannelPolicyFor("", "dm-fixture").Conversation.ReplyShape)
	if rep.Parts != 2 || rep.Sent != 2 || len(ids) != 2 || sent == "" {
		t.Fatalf("a DM reply must go out as two bubbles: parts %d sent %d", rep.Parts, rep.Sent)
	}
	if posts := dmPosts(fake); len(posts) != 2 {
		t.Fatalf("posts = %d, want 2", len(posts))
	}
}
