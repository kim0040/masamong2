package application

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"sort"
	"strings"
	"sync"
	"time"

	"masamong/internal/action"
	"masamong/internal/errcode"
	"masamong/internal/i18n"
	"masamong/locales"
)

// Operator alerts: the console's "needs attention" list reaches the operators (access.operators) as a
// Discord direct message, so a failed backup, a rejected key, the budget running out or a crash is not
// only visible to someone who happens to open the console.
//
// Every alertEvery the scheduler compares the list with what was already sent (admin_state
// alerts:sent) and sends only new items, in one message. An item that clears and returns within
// alertFlap is not sent again; a danger item still open after alertRepeat is sent once more. Things that
// happen once (the service restarting after a crash) are queued (alerts:queue) and go out with the next
// message, after Discord connects. Nothing is sent in practice mode or while alerts.discord is off.

const (
	alertEvery      = 5 * time.Minute
	alertStartDelay = 2 * time.Minute // let Discord connect and the first index pass settle
	alertFlap       = time.Hour
	alertRepeat     = 24 * time.Hour
	alertForget     = 24 * time.Hour
	alertMaxLines   = 12
	alertMaxQueued  = 20
)

// Queued alert keys (attention.* strings in the console catalog).
const (
	alertCrash        = "crash_restarted"
	alertMemoryScope  = "memory_scope"
	alertDeliveryPerm = "delivery_permission" // a reply could not be posted: the bot lacks a right in a channel
)

// Attention items not worth a message: the operator did them on purpose, or Discord itself is the
// problem (no message could arrive).
var alertSkip = map[string]bool{
	"paused": true, "restart_pending": true, "restore_pending": true, "restore_done": true,
	"discord_down": true, "discord_disabled": true,
}

var severityRank = map[string]int{"info": 0, "warn": 1, "danger": 2}

type alertEvent struct {
	Key  string         `json:"key"`
	Args map[string]any `json:"args,omitempty"`
	At   string         `json:"at"`
}

type alertSent struct {
	At   string `json:"at"`             // last sent
	Gone string `json:"gone,omitempty"` // when it cleared
}

// alertDelivery is the last attempt, for the console.
type alertDelivery struct {
	At     string `json:"at"`
	Items  int    `json:"items"`
	Sent   int    `json:"sent"`   // operators reached
	Failed int    `json:"failed"` // operators not reached
	Reason string `json:"reason,omitempty"`
	Test   bool   `json:"test,omitempty"`
}

var alertMu sync.Mutex

// queueAlert keeps a one-time event for the next message.
func (s *Service) queueAlert(key string, args map[string]any) {
	if s.Store == nil || s.practice {
		return
	}
	alertMu.Lock()
	defer alertMu.Unlock()
	var q []alertEvent
	_, _ = s.kvGet("alerts:queue", &q)
	q = append(q, alertEvent{Key: key, Args: args, At: s.Clock.Now().UTC().Format(time.RFC3339)})
	if len(q) > alertMaxQueued {
		q = q[len(q)-alertMaxQueued:]
	}
	_ = s.kvSet("alerts:queue", q)
}

// uiCatalog holds the console strings, where the attention sentences live.
var uiCatalog = sync.OnceValue(func() *i18n.Catalog {
	c := i18n.New()
	if sub, err := fs.Sub(locales.UIFS, "ui"); err == nil {
		_ = c.LoadFS(sub)
	}
	return c
})

func (s *Service) alertLocale() string {
	if l := s.Doc().I18n.DefaultLocale; l != "" {
		return uiCatalog().Resolve(l)
	}
	return "ko"
}

// alertText is an attention item's sentence, with tool and provider IDs named as the console names them.
func alertText(locale, key string, args map[string]any) string {
	c := uiCatalog()
	if key == "crash_restarted" {
		if reason, _ := args["reason"].(string); strings.TrimSpace(reason) == "" {
			return c.T(locale, "attention.crash_restarted_no_reason")
		}
	}
	var kv []any
	for k, v := range args {
		if str, ok := v.(string); ok {
			switch k {
			case "tool":
				if name, ok := c.Lookup(locale, "tool."+str); ok {
					v = name
				}
			case "provider":
				parts := strings.Split(str, "/")
				for i, p := range parts {
					p = strings.TrimSpace(p)
					if name, ok := c.Lookup(locale, "conn.provider."+p); ok {
						p = name
					}
					parts[i] = p
				}
				v = strings.Join(parts, " / ")
			}
		}
		kv = append(kv, k, v)
	}
	return c.T(locale, "attention."+key, kv...)
}

func alertFingerprint(a attention) string {
	var id []string
	if m, ok := a.Args.(map[string]any); ok {
		for _, k := range []string{"channel_id", "guild_id", "name", "provider", "safety", "tool"} {
			if v, ok := m[k]; ok {
				id = append(id, k+"="+strings.TrimSpace(anyString(v)))
			}
		}
	}
	return a.Severity + "|" + a.Key + "|" + strings.Join(id, ",")
}

func anyString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// alertsReady says whether a message can go out now, and why not.
func (s *Service) alertsReady() (DiscordSender, string) {
	doc := s.Doc()
	switch {
	case s.practice:
		return nil, "practice"
	case !doc.Alerts.DiscordOn():
		return nil, "off"
	case len(doc.Access.Operators) == 0:
		return nil, "no_operators"
	}
	dm, ok := s.Discord.(DiscordSender)
	if !ok || s.Discord == nil || !s.Discord.Connected() {
		return nil, "discord_offline"
	}
	return dm, ""
}

// alertTick sends what is new. The scheduler calls it every alertEvery.
func (s *Service) alertTick(ctx context.Context) {
	if s.Store == nil {
		return
	}
	alertMu.Lock()
	defer alertMu.Unlock()
	dm, why := s.alertsReady()
	if why == "off" || why == "no_operators" || why == "practice" {
		_ = s.kvSet("alerts:queue", []alertEvent{}) // nobody to tell; do not send a stale backlog later
		return
	}
	if dm == nil {
		return // Discord is not connected yet: try again next time
	}
	now := s.Clock.Now()
	locale := s.alertLocale()
	least := severityRank[s.Doc().Alerts.Least()]
	var items []attention
	if res := s.dashboardOverview(ctx, action.Request{Action: "dashboard.overview", Locale: locale}); res.OK {
		items, _ = res.Output["attention"].([]attention)
	}
	sent := map[string]alertSent{}
	_, _ = s.kvGet("alerts:sent", &sent)
	var queue []alertEvent
	_, _ = s.kvGet("alerts:queue", &queue)

	var lines []string
	danger := false
	current := map[string]bool{}
	for _, a := range items {
		if alertSkip[a.Key] || severityRank[a.Severity] < least {
			continue
		}
		fp := alertFingerprint(a)
		current[fp] = true
		prev, seen := sent[fp]
		if seen {
			if prev.Gone != "" {
				gone, _ := time.Parse(time.RFC3339, prev.Gone)
				if now.Sub(gone) < alertFlap {
					sent[fp] = alertSent{At: prev.At} // back after a moment: already told
					continue
				}
			} else {
				at, _ := time.Parse(time.RFC3339, prev.At)
				if a.Severity != "danger" || now.Sub(at) < alertRepeat {
					continue
				}
			}
		}
		args, _ := a.Args.(map[string]any)
		lines = append(lines, alertText(locale, a.Key, args))
		danger = danger || a.Severity == "danger"
		sent[fp] = alertSent{At: now.UTC().Format(time.RFC3339)}
	}
	for _, e := range queue {
		key := e.Key
		if key == alertCrash && e.Args["gave_up"] == true {
			key = "crash_gave_up"
		}
		lines = append(lines, alertText(locale, key, e.Args))
		danger = true
	}
	for fp, v := range sent {
		if current[fp] {
			continue
		}
		if v.Gone == "" {
			v.Gone = now.UTC().Format(time.RFC3339)
			sent[fp] = v
		} else if gone, err := time.Parse(time.RFC3339, v.Gone); err != nil || now.Sub(gone) > alertForget {
			delete(sent, fp)
		}
	}
	if len(lines) == 0 {
		_ = s.kvSet("alerts:sent", sent)
		return
	}
	d := s.sendAlert(ctx, dm, locale, lines, danger, false)
	if d.Sent == 0 && d.Reason != "dm_closed" {
		return // nobody reached for a passing reason: the same items go out next time
	}
	_ = s.kvSet("alerts:sent", sent)
	_ = s.kvSet("alerts:queue", []alertEvent{})
}

// sendAlert sends one message with lines to every operator and records the attempt.
func (s *Service) sendAlert(ctx context.Context, dm DiscordSender, locale string, lines []string, danger, test bool) alertDelivery {
	c := uiCatalog()
	shown := lines
	more := 0
	if len(shown) > alertMaxLines {
		shown, more = lines[:alertMaxLines], len(lines)-alertMaxLines
	}
	var b strings.Builder
	for _, l := range shown {
		b.WriteString("• " + l + "\n")
	}
	if more > 0 {
		b.WriteString(c.T(locale, "alerts.more", "n", more) + "\n")
	}
	card := action.Card{Title: c.T(locale, "alerts.title"), Text: strings.TrimSpace(b.String()), Tone: "warn",
		Footer: c.T(locale, "alerts.footer", "instance", s.Doc().Instance.ID)}
	if danger {
		card.Tone = "error"
	}
	if test {
		card.Title = c.T(locale, "alerts.test_title")
	}
	d := alertDelivery{At: s.Clock.Now().UTC().Format(time.RFC3339), Items: len(lines), Test: test}
	ops := append([]string(nil), s.Doc().Access.Operators...)
	sort.Strings(ops)
	closed := 0
	for _, id := range ops {
		sendCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := dm.SendDM(sendCtx, id, card, "")
		cancel()
		switch {
		case err == nil:
			d.Sent++
		case errors.Is(err, ErrDMClosed):
			d.Failed++
			closed++
		default:
			d.Failed++
			d.Reason = "send_failed"
			if s.Log != nil {
				s.Log.Warn("alert not delivered", "err", err) // no user ID in logs
			}
		}
	}
	if d.Sent == 0 && closed == len(ops) && len(ops) > 0 {
		d.Reason = "dm_closed"
	} else if closed > 0 && d.Reason == "" {
		d.Reason = "dm_closed_some"
	}
	_ = s.kvSet("alerts:last", d)
	return d
}

// alertsAction: alerts.status (settings, readiness, last delivery) and alerts.test (send one now).
func (s *Service) alertsAction(ctx context.Context, req action.Request) action.Result {
	doc := s.Doc()
	var last *alertDelivery
	if s.Store != nil {
		var d alertDelivery
		if ok, _ := s.kvGet("alerts:last", &d); ok {
			last = &d
		}
	}
	_, why := s.alertsReady()
	status := map[string]any{"discord": doc.Alerts.DiscordOn(), "min_severity": doc.Alerts.Least(), "operators": len(doc.Access.Operators),
		"ready": why == "", "reason": why, "last": last, "every_min": int(alertEvery.Minutes())}
	if req.Action == "alerts.status" {
		return action.OK(req, status)
	}
	alertMu.Lock()
	defer alertMu.Unlock()
	// a test works with alerts off: it checks that the operators can be reached
	var dm DiscordSender
	switch d, ok := s.Discord.(DiscordSender); {
	case s.practice:
		why = "practice"
	case len(doc.Access.Operators) == 0:
		why = "no_operators"
	case !ok || !s.Discord.Connected():
		why = "discord_offline"
	default:
		dm, why = d, ""
	}
	if dm == nil || why != "" {
		code := errcode.InvalidInput
		if why == "discord_offline" {
			code = errcode.DiscordNotConnected
		}
		return action.Fail(req, code, "generic.error", "alerts cannot be sent: "+why, map[string]any{"reason": why})
	}
	locale := s.alertLocale()
	d := s.sendAlert(ctx, dm, locale, []string{uiCatalog().T(locale, "alerts.test_line")}, false, true)
	status["last"] = d
	if d.Sent == 0 {
		return action.Fail(req, errcode.ProviderUnavailable, "generic.error", "no operator could be reached: "+d.Reason, map[string]any{"reason": d.Reason, "last": d})
	}
	return action.OK(req, status)
}
