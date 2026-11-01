package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"masamong/internal/action"
	"masamong/internal/application"
)

// The personal features under load (-features): reminders asked for and delivered, the bot joining
// conversations unasked (participation), consent and personal settings, and the console reading all
// of it — next to the chat traffic. A fake Discord connection takes the DMs and channel posts, so the
// delivery guarantees (each reminder at most once, none left mid-send) are checked at the end.

// featureGateway is a connected Discord stand-in that counts what the bot sends on its own.
type featureGateway struct {
	mu        sync.Mutex
	delivered map[string]int // reminder text -> times delivered
	dms       atomic.Int64
	posts     atomic.Int64
}

func (g *featureGateway) Connected() bool                           { return true }
func (g *featureGateway) BotUser() (string, string)                 { return "bot", "masamong" }
func (g *featureGateway) Guilds() []application.GuildInfo           { return nil }
func (g *featureGateway) Channels(string) []application.ChannelInfo { return nil }
func (g *featureGateway) Permissions(string, string) (int64, bool)  { return 0, false }
func (g *featureGateway) note(c action.Card)                        { g.mu.Lock(); g.delivered[c.Text]++; g.mu.Unlock() }
func (g *featureGateway) delay()                                    { time.Sleep(time.Duration(20+rand.IntN(60)) * time.Millisecond) }
func (g *featureGateway) SendDM(_ context.Context, _ string, c action.Card, _ string) error {
	g.delay()
	g.dms.Add(1)
	g.note(c)
	return nil
}

func (g *featureGateway) PostChannel(_ context.Context, _, _, _ string, c *action.Card) (string, error) {
	g.delay()
	g.posts.Add(1)
	if c != nil {
		g.note(*c)
	}
	return fmt.Sprintf("post%d", g.posts.Load()), nil
}

// participates reports whether channel i joins in unasked (odd channels, alternating shadow and
// conservative); even channels answer every message as before.
func participates(i int) bool { return i%2 == 1 }

func participationMode(i int) string {
	if i%4 == 1 {
		return "shadow"
	}
	return "conservative"
}

var reminderSeq atomic.Int64

// feature is one person using a personal feature.
func (h *harness) feature() {
	ch := h.channel(rand.IntN(h.opt.channels))
	user := fmt.Sprintf("u%03d", rand.IntN(h.opt.users))
	inv := &action.Invoker{Via: "command", GuildID: "g", ChannelID: ch, UserID: user, Name: user}
	switch rand.IntN(6) {
	case 0, 1:
		// ask, then confirm with the button (a DM or this channel); due in a minute or two
		n := reminderSeq.Add(1)
		res := h.exec("reminder_propose", "reminders.propose", map[string]any{"text": fmt.Sprintf("%d분 뒤 물 마시기 #%d", 1+rand.IntN(2), n)}, "dc:remind", inv)
		id := ""
		if c, ok := res.Output["card"].(action.Card); ok {
			for _, b := range c.Buttons {
				if strings.HasPrefix(b.ID, "remind:cancel:") {
					id = strings.TrimPrefix(b.ID, "remind:cancel:")
				}
			}
		}
		if id == "" {
			return
		}
		dest := "dm"
		if rand.IntN(2) == 0 {
			dest = "channel"
		}
		h.exec("reminder_confirm", "reminders.update", map[string]any{"op": "confirm", "id": id, "destination": dest}, "dc:button", inv)
		if rand.IntN(10) == 0 {
			h.exec("reminder_cancel", "reminders.update", map[string]any{"op": "cancel", "id": id}, "dc:button", inv)
		}
	case 2:
		h.exec("reminder_list", "reminders.update", map[string]any{"op": "list"}, "dc:reminders", inv)
	case 3:
		h.exec("privacy_status", "privacy.consent", map[string]any{}, "dc:privacy", inv)
	case 4:
		h.exec("prefs_set", "preferences.set", map[string]any{"timezone": "Asia/Seoul"}, "dc:mysettings", inv)
	case 5:
		if rand.IntN(5) == 0 {
			h.exec("participation_optout", "privacy.consent", map[string]any{"scope": "participation", "decision": "withdraw"}, "dc:privacy", inv)
		} else {
			h.exec("privacy_notice", "privacy.consent", map[string]any{"scope": "fortune"}, "dc:privacy", inv)
		}
	}
}

// featureConsole is the console's reads of the personal features.
func (h *harness) featureConsole() {
	ch := h.channel(rand.IntN(h.opt.channels))
	switch rand.IntN(3) {
	case 0:
		h.exec("console:users.list", "users.list", map[string]any{}, "web", nil)
	case 1:
		h.exec("console:participation.mode", "participation.mode", map[string]any{"guild_id": "g", "channel_id": ch}, "web", nil)
	case 2:
		h.exec("console:reminders.update", "reminders.update", map[string]any{"op": "list", "guild_id": "g", "channel_id": ch}, "web", nil)
	}
}

// featureChecks reads the outcome: what was sent, and whether any guarantee broke.
func (h *harness) featureChecks(g *featureGateway) (map[string]int, []string) {
	stats := map[string]int{"dms": int(g.dms.Load()), "channel_posts": int(g.posts.Load())}
	var failures []string
	res := h.svc.Execute(context.Background(), action.Request{Action: "reminders.update", Input: json.RawMessage(`{"op":"list","limit":500}`), RequestID: "web"})
	raw, _ := json.Marshal(res.Output["reminders"])
	var list []struct {
		Content string `json:"content"`
		Status  string `json:"status"`
	}
	_ = json.Unmarshal(raw, &list)
	sent := 0
	for _, r := range list {
		stats["reminders_"+r.Status]++
		if r.Status == "sent" {
			sent++
		}
		if r.Status == "sending" {
			failures = append(failures, "a reminder was left mid-send after the drain")
		}
	}
	g.mu.Lock()
	for text, n := range g.delivered {
		if n > 1 && strings.Contains(text, "#") {
			failures = append(failures, fmt.Sprintf("reminder %q delivered %d times", clip(text, 40), n))
		}
	}
	g.mu.Unlock()
	if len(list) >= 500 {
		stats["reminders_listed_capped"] = 1
	}
	pres := h.svc.Execute(context.Background(), action.Request{Action: "participation.mode", Input: json.RawMessage(`{"limit":200}`), RequestID: "web"})
	praw, _ := json.Marshal(pres.Output["stats_7d"])
	var pstats map[string]int
	_ = json.Unmarshal(praw, &pstats)
	for k, v := range pstats {
		stats["participation_"+k] = v
	}
	return stats, failures
}
