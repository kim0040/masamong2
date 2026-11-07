package tools

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"masamong/internal/storage"
)

func ptr(b bool) *bool { return &b }

func TestResolveDefaultsConfigAndFeatures(t *testing.T) {
	cfg := map[string]Settings{
		"weather.query": {Chat: ptr(false), Aliases: []string{"w"}, Limits: Limits{PerUserPerDay: 3}},
		"finance.fx":    {Enabled: ptr(false)},
		"lotto.latest":  {Description: "로또", HTTP: &HTTP{URL: "https://example.com/lotto?round={round}"}, Params: []Param{{Name: "round"}}},
	}
	all := Resolve(cfg, map[string]bool{"search": false})
	get := func(id string) Resolved { r, _ := Find(all, id); return r }
	if w := get("weather.query"); !w.Enabled || w.Chat || !w.Commands || w.Aliases[0] != "w" || w.Limits.PerUserPerDay != 3 {
		t.Fatalf("%+v", w)
	}
	if fx := get("finance.fx"); fx.Enabled || fx.Chat || fx.Reason != "config" {
		t.Fatalf("%+v", fx)
	}
	if s := get("search.web"); s.Enabled || s.Reason != "feature_flag" {
		t.Fatalf("feature flag must switch the tool off: %+v", s)
	}
	if im := get("images.generate"); !im.Enabled || !im.Chat || !im.CanChat {
		t.Fatalf("pictures are drawn in conversation too: %+v", im)
	}
	if f := get("fortune.query"); f.Chat || f.CanChat {
		t.Fatalf("fortune goes through its consent command, never the planner: %+v", f)
	}
	if l := get("lotto.latest"); l.Kind != "http" || !l.Chat || l.Commands {
		t.Fatalf("%+v", l)
	}
	if all[len(all)-1].ID != "lotto.latest" {
		t.Fatal("operator tools follow built-ins")
	}
}

func TestValidate(t *testing.T) {
	bad := map[string]Settings{
		"weathr.query":   {},
		"x.y":            {Description: "d", HTTP: &HTTP{URL: "http://example.com"}},
		"a.b":            {Description: "d", HTTP: &HTTP{URL: "https://{host}/x"}},
		"c.d":            {Description: "d", HTTP: &HTTP{URL: "https://e.com/?q={q}"}},
		"e.f":            {Description: "d", HTTP: &HTTP{URL: "https://e.com/?k={credential}"}},
		"g.h":            {Description: "d", HTTP: &HTTP{URL: "https://e.com/", CredentialRef: "sk-live-123"}},
		"weather.query2": {HTTP: &HTTP{URL: "https://e.com"}},
		"memory.search":  {Limits: Limits{PerDay: -1}},
		"help.version":   {Aliases: []string{"!help"}},
		"polls.create":   {Description: "d", HTTP: &HTTP{URL: "https://e.com"}},
	}
	for id, s := range bad {
		if err := Validate(id, s); err == nil {
			t.Errorf("%s accepted", id)
		}
	}
	good := Settings{Description: "d", HTTP: &HTTP{URL: "https://e.com/v1/{q}?key={credential}", CredentialRef: "env:K"}, Params: []Param{{Name: "q", Required: true}}}
	if err := Validate("my.tool", good); err != nil {
		t.Fatal(err)
	}
	if err := Validate("weather.query", Settings{Enabled: ptr(false), Limits: Limits{PerDay: 5, CooldownSec: 10}}); err != nil {
		t.Fatal(err)
	}
}

func TestUsageLimitsAndCooldown(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "u.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	u, err := OpenUsage(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenUsage(db); err != nil {
		t.Fatal("idempotent")
	}
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	lim := Limits{PerUserPerDay: 2, PerDay: 3}
	a, b := Caller{UserID: "111", Channel: "discord:1:2"}, Caller{UserID: "222", Channel: "discord:1:2"}
	for i, want := range []bool{true, true, false} {
		d, err := u.Reserve("2026-09-23", now.Add(time.Duration(i)*time.Minute), "weather.query", lim, a)
		if err != nil || d.OK != want {
			t.Fatalf("call %d: %+v %v", i, d, err)
		}
		if !want && d.Scope != "per_user_per_day" {
			t.Fatalf("%+v", d)
		}
	}
	if d, _ := u.Reserve("2026-09-23", now, "weather.query", lim, b); !d.OK {
		t.Fatal("other user has quota")
	}
	if d, _ := u.Reserve("2026-09-23", now, "weather.query", lim, b); d.OK || d.Scope != "per_day" {
		t.Fatalf("instance-wide cap: %+v", d)
	}
	if d, _ := u.Reserve("2026-09-24", now, "weather.query", lim, a); !d.OK {
		t.Fatal("new day resets")
	}
	cd := Limits{CooldownSec: 30}
	if d, _ := u.Reserve("2026-09-23", now, "search.web", cd, a); !d.OK {
		t.Fatal("first call")
	}
	if d, _ := u.Reserve("2026-09-23", now.Add(10*time.Second), "search.web", cd, a); d.OK || d.Scope != "cooldown" || d.RetryAfter != 20*time.Second {
		t.Fatalf("%+v", d)
	}
	today, _ := u.Today("2026-09-23")
	if today["weather.query"] != 3 || today["search.web"] != 1 {
		t.Fatalf("%v", today)
	}
	var raw string
	_ = db.DB.QueryRow(`SELECT group_concat(subject) FROM tool_usage`).Scan(&raw)
	if strings.Contains(raw, "111") || strings.Contains(raw, "222") {
		t.Fatal("user IDs must not be stored")
	}
}

// Final tools end the tool loop on their one result: only tools that read, and a result that needs nothing
// more. search.web is not among them (chat.loop_final decides it, for a search that found enough).
func TestFinalToolsOnlyRead(t *testing.T) {
	var final []string
	for _, b := range Builtins() {
		if b.Final {
			final = append(final, b.ID)
			if b.Writes || b.Once || !b.Chat {
				t.Errorf("%s: final, writes %v, once %v, chat %v", b.ID, b.Writes, b.Once, b.Chat)
			}
		}
	}
	if strings.Join(final, " ") != "weather.query finance.quote finance.fx places.search" {
		t.Fatalf("final tools: %v", final)
	}
	if r, _ := Find(Resolve(nil, nil), "weather.query"); !r.Final {
		t.Fatal("Resolve dropped Final")
	}
}
