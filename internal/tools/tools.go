// Package tools is the product tool registry: which Actions the bot can use as tools, how each is
// switched on (instance, chat planner, Discord commands, per channel), how often it may run, and
// operator-defined tools (config `tools:` with an `http:` block) that need no code.
//
// A built-in tool is an existing catalog Action; this package only describes it. Planner text for
// built-ins comes from the language pack (internal/lang); config can override it per tool.
package tools

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"masamong/internal/catalog"
	"masamong/internal/providers/websearch"
	"masamong/internal/secrets"
)

// Limits cap how often a tool runs. Zero means no limit. Days are calendar days in the instance
// time zone (i18n.timezone).
type Limits struct {
	PerDay           int `yaml:"per_day,omitempty" json:"per_day,omitempty"`                   // every caller together
	PerUserPerDay    int `yaml:"per_user_per_day,omitempty" json:"per_user_per_day,omitempty"` // one Discord user
	PerChannelPerDay int `yaml:"per_channel_per_day,omitempty" json:"per_channel_per_day,omitempty"`
	CooldownSec      int `yaml:"cooldown_sec,omitempty" json:"cooldown_sec,omitempty"` // between two calls of one user
}

// Zero reports whether no limit is set.
func (l Limits) Zero() bool { return l == Limits{} }

// Param is one parameter of an operator-defined tool.
type Param struct {
	Name        string `yaml:"name" json:"name"`
	Required    bool   `yaml:"required,omitempty" json:"required,omitempty"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
}

// HTTP defines an operator tool that GETs one https endpoint. {param} placeholders in URL are
// URL-escaped parameter values; {credential} (URL or header values) is the secret behind
// CredentialRef, which never appears in config, logs or tool output.
type HTTP struct {
	URL           string            `yaml:"url" json:"url"`
	Headers       map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
	CredentialRef string            `yaml:"credential_ref,omitempty" json:"credential_ref,omitempty"`
	TimeoutSec    int               `yaml:"timeout_sec,omitempty" json:"timeout_sec,omitempty"` // default 10
	MaxChars      int               `yaml:"max_chars,omitempty" json:"max_chars,omitempty"`     // output cap, default 3000
	Pick          []string          `yaml:"pick,omitempty" json:"pick,omitempty"`               // JSON fields to keep (a.b.c paths)
}

// Settings is config `tools.<id>`. Pointers are tri-state: unset keeps the built-in default.
type Settings struct {
	Enabled     *bool          `yaml:"enabled,omitempty" json:"enabled,omitempty"`   // false: refused everywhere (chat, commands, CLI, GUI)
	Chat        *bool          `yaml:"chat,omitempty" json:"chat,omitempty"`         // the conversation planner may call it
	Commands    *bool          `yaml:"commands,omitempty" json:"commands,omitempty"` // Discord !prefix and slash command
	Aliases     []string       `yaml:"aliases,omitempty" json:"aliases,omitempty"`   // !prefix words (replace the built-in list)
	Description string         `yaml:"description,omitempty" json:"description,omitempty"`
	Route       string         `yaml:"route,omitempty" json:"route,omitempty"` // one "when to use" hint for the planner
	Defaults    map[string]any `yaml:"defaults,omitempty" json:"defaults,omitempty"`
	Params      []Param        `yaml:"params,omitempty" json:"params,omitempty"` // operator tools only
	Limits      Limits         `yaml:"limits,omitempty" json:"limits,omitempty"`
	HTTP        *HTTP          `yaml:"http,omitempty" json:"http,omitempty"` // present = operator-defined tool
}

// Input is one input of a built-in tool. The console builds a tool's test form and its default-value
// form from these, so a tool's inputs are described once, here, next to the Action that reads them.
type Input struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"` // text | number | select | bool | url | provider (a configured provider of the tool)
	Required bool     `json:"required,omitempty"`
	Options  []string `json:"options,omitempty"` // select values; labels are the console's toolp.<name>.<value>
	Min      int      `json:"min,omitempty"`
	Max      int      `json:"max,omitempty"`
	Example  string   `json:"example,omitempty"` // language-neutral placeholder; the console may have a localized one
	// Operator marks a setting of the tool that only the operator sets (a default in the console): the
	// chat model is never offered it, and the console's test form leaves it out.
	Operator bool `json:"operator,omitempty"`
}

// Builtin describes a product Action used as a tool.
type Builtin struct {
	ID          string
	Feature     string   // features.<name> switch that also gates it ("" = none)
	Chat        bool     // offered to the planner unless config says otherwise
	Command     bool     // has a Discord command
	Implemented bool     // false: fixture output only; never offered to the planner by default
	Required    []string // parameters that must be real values
	Scoped      bool     // acts on the calling channel (scope is injected, never taken from the planner)
	Writes      bool     // creates state; not executed by a dry-run chat turn
	// Once: in a chat turn, a call that succeeded closes the tool for the turn's later rounds. Several
	// calls in one round still make several (two reminders asked at once); a second call in a later
	// round would be the same request made twice (a second confirmation card for one reminder).
	Once   bool
	Sample map[string]any
	Inputs []Input
	// Providers are the provider kinds the tool calls; any one of them with a key is enough (weather:
	// keyless Open-Meteo or KMA; search: whichever search services have keys). Empty: no provider.
	Providers []string
	// Uses says where a tool without a keyed provider gets its answer: "model" (a model role, its
	// cost counts toward the budget) or "web" (public pages read directly). Empty: neither.
	Uses string
	// Role is the model role the tool cannot work without ("response", "summary", "image", or "embed"
	// for the memory index). The console and the dashboard read a tool's readiness from it.
	Role string
	// OptIn tools stay off until the operator turns them on: they hand what people post (a link) to
	// an outside service, which nobody agreed to by installing the bot.
	OptIn bool
	// WebContent tools return text written by anyone on the open web (search results, a read page),
	// which may carry planted instructions: once one has answered, the chat tool loop stops offering
	// tools that write (Writes) for the rest of the turn.
	WebContent bool
}

var (
	days14    = []string{"0", "1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11", "12", "13"}
	fresh     = []string{"day", "week", "month"}
	ratios    = []string{"1:1", "16:9", "9:16", "4:3", "3:4"}
	subscribe = []string{"status", "subscribe", "unsubscribe"}
)

// Builtins lists the built-in tools in planner-menu order.
func Builtins() []Builtin {
	return []Builtin{
		{ID: "memory.search", Chat: true, Implemented: true, Role: "embed", Inputs: []Input{{Name: "query", Type: "text", Required: true}}},
		{ID: "weather.query", Feature: "weather", Chat: true, Command: true, Implemented: true, Sample: map[string]any{}, Providers: []string{"open_meteo", "kma"},
			Inputs: []Input{{Name: "region", Type: "text", Example: "Seoul · Tokyo · Paris"}, {Name: "day_offset", Type: "select", Options: days14}}},
		{ID: "disasters.query", Feature: "disasters", Chat: true, Command: true, Implemented: true, Sample: map[string]any{}, Providers: []string{"kma"},
			Inputs: []Input{{Name: "days", Type: "number", Min: 1, Max: 7, Example: "3"}}},
		{ID: "finance.quote", Feature: "finance", Chat: true, Command: true, Implemented: true, Required: []string{"symbol"}, Sample: map[string]any{"symbol": "AAPL"}, Providers: []string{"finnhub"},
			Inputs: []Input{{Name: "symbol", Type: "text", Required: true, Example: "AAPL · 005930.KS"}, {Name: "krw", Type: "bool"}}},
		{ID: "finance.fx", Feature: "finance", Chat: true, Command: true, Implemented: true, Required: []string{"base"}, Sample: map[string]any{"base": "USD"}, Providers: []string{"exchangerate"},
			Inputs: []Input{{Name: "base", Type: "text", Required: true, Example: "USD"}, {Name: "symbols", Type: "text", Example: "KRW, JPY, EUR"}}},
		{ID: "search.web", Feature: "search", Chat: true, Command: true, Implemented: true, WebContent: true, Required: []string{"query"}, Sample: map[string]any{"query": "masamong", "size": 1},
			Providers: websearch.Kinds,
			Inputs: []Input{{Name: "query", Type: "text", Required: true}, {Name: "size", Type: "number", Min: 1, Max: 10, Example: "5"},
				{Name: "fresh", Type: "select", Options: fresh},
				// quick: one fact; deep: a wider, slower look (a digest, a comparison); news: news articles
				{Name: "depth", Type: "select", Options: []string{"quick", "standard", "deep"}},
				{Name: "topic", Type: "select", Options: []string{"general", "news"}},
				{Name: "provider", Type: "provider"}}},
		{ID: "places.search", Feature: "search", Chat: true, Command: true, Implemented: true, Required: []string{"query"}, Sample: map[string]any{"query": "강남역 맛집", "size": 3},
			Providers: []string{"kakao_search"},
			Inputs:    []Input{{Name: "query", Type: "text", Required: true, Example: "강남역 맛집 · Hongdae cafe"}, {Name: "size", Type: "number", Min: 1, Max: 15, Example: "5"}}},
		{ID: "web.read", Feature: "search", Chat: true, Implemented: true, OptIn: true, WebContent: true, Required: []string{"url"}, Sample: map[string]any{"url": "https://example.com", "max_chars": 600},
			Providers: []string{"jina_reader"},
			Inputs:    []Input{{Name: "url", Type: "url", Required: true, Example: "https://…"}, {Name: "max_chars", Type: "number", Min: 500, Max: 20000, Example: "2800"}}},
		{ID: "images.generate", Feature: "images", Chat: true, Command: true, Uses: "model", Role: "image", Implemented: true, Required: []string{"prompt"}, Writes: true, Once: true,
			Inputs: []Input{{Name: "prompt", Type: "text", Required: true}, {Name: "aspect_ratio", Type: "select", Options: ratios},
				// async: a picture asked for in chat is drawn in the background (default on; imagequeue.go)
				{Name: "async", Type: "bool", Operator: true}}},
		{ID: "polls.create", Feature: "polls", Chat: true, Command: true, Implemented: true, Required: []string{"question"}, Scoped: true, Writes: true, Once: true,
			Inputs: []Input{{Name: "question", Type: "text", Required: true}, {Name: "options_text", Type: "text"},
				{Name: "duration_hours", Type: "number", Min: 1, Max: 768, Example: "24"}, {Name: "allow_multiselect", Type: "bool"}}},
		{ID: "help.version", Chat: true, Command: true, Implemented: true, Sample: map[string]any{}},
		{ID: "activity.ranking", Chat: true, Command: true, Implemented: true, Scoped: true, Sample: map[string]any{"period": "week"},
			Inputs: []Input{{Name: "period", Type: "select", Options: []string{"today", "week", "month", "all"}}}},
		{ID: "summaries.channel", Feature: "summaries", Chat: true, Uses: "model", Role: "summary", Command: true, Implemented: true, Scoped: true,
			Inputs: []Input{{Name: "refresh", Type: "bool"}}},
		{ID: "polls.vote", Feature: "polls", Implemented: true, Scoped: true, Writes: true,
			Inputs: []Input{{Name: "id", Type: "text", Required: true}, {Name: "option_ids_text", Type: "text", Required: true}}},
		{ID: "polls.close", Feature: "polls", Implemented: true, Scoped: true, Writes: true, Inputs: []Input{{Name: "id", Type: "text", Required: true}}},
		{ID: "polls.result", Feature: "polls", Implemented: true, Inputs: []Input{{Name: "id", Type: "text", Required: true}}},
		{ID: "fortune.query", Feature: "fortune", Command: true, Uses: "model", Role: "response", Implemented: true,
			Inputs: []Input{{Name: "what", Type: "select", Options: []string{"today", "detail", "month", "year", "zodiac", "ranking"}}}},
		{ID: "fortune.subscribe", Feature: "fortune", Command: true, Uses: "model", Role: "response", Implemented: true, Writes: true,
			Inputs: []Input{{Name: "op", Type: "select", Options: subscribe}, {Name: "time", Type: "text", Example: "08:00"}}},
		{ID: "notices.status", Feature: "notices", Command: true, Uses: "web", Implemented: true,
			Inputs: []Input{{Name: "kind", Type: "select", Options: []string{"school", "transfer"}}}},
		{ID: "notices.subscribe", Feature: "notices", Command: true, Uses: "web", Implemented: true, Writes: true,
			Inputs: []Input{{Name: "op", Type: "select", Options: subscribe}, {Name: "kind", Type: "select", Options: []string{"school", "transfer"}},
				{Name: "universities", Type: "text"}, {Name: "time", Type: "text", Example: "08:00"}}},
		{ID: "transfer.recent", Feature: "notices", Command: true, Uses: "web", Implemented: true,
			Inputs: []Input{{Name: "universities", Type: "text"}, {Name: "days", Type: "number", Min: 1, Max: 365, Example: "30"}}},
		{ID: "reminders.propose", Feature: "reminders", Chat: true, Command: true, Implemented: true, Writes: true, Once: true,
			Inputs: []Input{{Name: "when", Type: "text", Required: true}, {Name: "what", Type: "text"},
				// repeating: the rule, and an optional end
				{Name: "repeat", Type: "select", Options: []string{"daily", "weekly", "monthly", "hours"}},
				{Name: "weekdays", Type: "text", Example: "mon,wed"},
				{Name: "day", Type: "number", Min: 1, Max: 31},
				{Name: "every", Type: "number", Min: 1, Max: 48},
				{Name: "until", Type: "text", Example: "2026-12-31"},
				{Name: "count", Type: "number", Min: 1, Max: 366},
				// the reminder points at what was just said: keep the last lines of the conversation with it
				{Name: "with_context", Type: "bool"},
				// a scheduled task (reminders_task.go): the bot does task when due and posts the answer;
				// ask is the one question to put first when the request misses something the run needs
				{Name: "kind", Type: "select", Options: []string{"reminder", "task"}},
				{Name: "task", Type: "text"}, {Name: "ask", Type: "text"}}},
		{ID: "reminders.update", Feature: "reminders", Chat: true, Command: true, Implemented: true, Writes: true,
			Inputs: []Input{{Name: "op", Type: "select", Options: []string{"list", "cancel", "pause", "resume"}}, {Name: "id", Type: "text"},
				// the words that name the reminder when no id is known ("뉴스")
				{Name: "match", Type: "text"}}},
		{ID: "chat.respond", Implemented: true, Role: "response"}, // the conversation itself: limits apply to replies
	}
}

// Lookup finds a built-in tool.
func Lookup(id string) (Builtin, bool) {
	for _, b := range Builtins() {
		if b.ID == id {
			return b, true
		}
	}
	return Builtin{}, false
}

var (
	customIDRE = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z0-9_]+)+$`)
	aliasRE    = regexp.MustCompile(`^[^\s!/]{1,32}$`)
	paramRE    = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	placeRE    = regexp.MustCompile(`\{([a-z][a-z0-9_]*)\}`)
)

// Validate checks one `tools.<id>` entry.
func Validate(id string, s Settings) error { return validate(id, s, false) }

// validate checks an entry; skipURL leaves the web address policy to ValidateLenient.
func validate(id string, s Settings, skipURL bool) error {
	skipURL = skipURL || s.Enabled != nil && !*s.Enabled // a tool that is switched off calls nothing
	if s.HTTP == nil {
		if _, ok := Lookup(id); !ok {
			return fmt.Errorf("tools.%s: not a built-in tool (an operator tool needs an http: block)", id)
		}
		if len(s.Params) > 0 {
			return fmt.Errorf("tools.%s.params: only operator tools declare params", id)
		}
	} else {
		if !customIDRE.MatchString(id) {
			return fmt.Errorf("tools.%s: operator tool IDs look like group.name (lowercase, at least one dot)", id)
		}
		if _, ok := catalog.Lookup(id); ok {
			return fmt.Errorf("tools.%s: collides with a product Action", id)
		}
		if strings.TrimSpace(s.Description) == "" {
			return fmt.Errorf("tools.%s.description: required (the planner decides from it)", id)
		}
		if err := validateHTTP(id, s, skipURL); err != nil {
			return err
		}
	}
	l := s.Limits
	if l.PerDay < 0 || l.PerUserPerDay < 0 || l.PerChannelPerDay < 0 || l.CooldownSec < 0 || l.CooldownSec > 86400 {
		return fmt.Errorf("tools.%s.limits: values must be 0 (no limit) or positive; cooldown_sec at most 86400", id)
	}
	for _, a := range s.Aliases {
		if !aliasRE.MatchString(a) {
			return fmt.Errorf("tools.%s.aliases: %q must be one word without ! or /", id, a)
		}
	}
	if len([]rune(s.Description)) > 600 || len([]rune(s.Route)) > 200 {
		return fmt.Errorf("tools.%s: description at most 600 and route at most 200 characters", id)
	}
	return nil
}

// checkToolURL applies the web address policy to an operator tool's URL (placeholders removed).
func checkToolURL(id string, s Settings) error {
	u, err := url.Parse(strings.NewReplacer("{", "", "}", "").Replace(s.HTTP.URL))
	if err != nil || u.Host == "" {
		return fmt.Errorf("tools.%s.http.url: not a URL", id)
	}
	if err := ValidateHTTPURL(u.String()); err != nil {
		return &URLPolicyError{Path: "tools." + id + ".http.url", Reason: err}
	}
	return nil
}

func validateHTTP(id string, s Settings, skipURL bool) error {
	h := s.HTTP
	if u, err := url.Parse(strings.NewReplacer("{", "", "}", "").Replace(h.URL)); err != nil || u.Host == "" {
		return fmt.Errorf("tools.%s.http.url: not a URL", id)
	}
	if !skipURL {
		if err := checkToolURL(id, s); err != nil {
			return err
		}
	}
	_, hostPart, _ := strings.Cut(h.URL, "://")
	if i := strings.IndexAny(hostPart, "/?#"); i >= 0 {
		hostPart = hostPart[:i]
	}
	if strings.ContainsAny(hostPart, "{}") {
		return fmt.Errorf("tools.%s.http.url: the host cannot contain placeholders", id)
	}
	if h.CredentialRef != "" && !secrets.ValidRef(h.CredentialRef) {
		return fmt.Errorf("tools.%s.http.credential_ref must be %s, never the secret", id, secrets.Hint)
	}
	uses := strings.Contains(h.URL, "{credential}")
	for _, v := range h.Headers {
		uses = uses || strings.Contains(v, "{credential}")
	}
	if uses && h.CredentialRef == "" {
		return fmt.Errorf("tools.%s.http: {credential} used without credential_ref", id)
	}
	if h.TimeoutSec < 0 || h.TimeoutSec > 60 || h.MaxChars < 0 || h.MaxChars > 20000 {
		return fmt.Errorf("tools.%s.http: timeout_sec 0–60, max_chars 0–20000", id)
	}
	names := map[string]bool{}
	for _, p := range s.Params {
		if !paramRE.MatchString(p.Name) || p.Name == "credential" || names[p.Name] {
			return fmt.Errorf("tools.%s.params: bad or duplicate name %q", id, p.Name)
		}
		names[p.Name] = true
	}
	for _, m := range placeRE.FindAllStringSubmatch(h.URL, -1) {
		if m[1] != "credential" && !names[m[1]] {
			return fmt.Errorf("tools.%s.http.url: {%s} is not a declared param", id, m[1])
		}
	}
	return nil
}

// ValidateHTTPURL allows trusted HTTPS endpoints and explicit local HTTP endpoints only.
//
// The error names the rule that failed, because the address may be one an earlier release accepted:
// a message that says "must use https" about an https address (one with a user name in it) sends the
// operator looking in the wrong place.
func ValidateHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	switch {
	case err != nil || u.Host == "" || u.Hostname() == "" || u.Opaque != "":
		return fmt.Errorf("not a valid web address")
	case u.User != nil:
		return fmt.Errorf("the address contains a user name or password (user:pass@host); keep the key in credential_ref and remove it from the address")
	case u.Scheme != "http" && u.Scheme != "https":
		return fmt.Errorf("the address scheme %q is not allowed; use https", u.Scheme)
	case u.Scheme == "https":
		return nil
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); strings.EqualFold(host, "localhost") || ip != nil && ip.IsLoopback() {
		return nil
	}
	if strings.Contains(strings.ToLower(host), "localhost") || strings.HasPrefix(host, "127.") {
		return fmt.Errorf("plain http is only allowed for localhost, 127.x.x.x and [::1]; %q only looks like one (a look-alike name resolves elsewhere); use https", host)
	}
	return fmt.Errorf("plain http is only allowed for localhost, 127.x.x.x and [::1], not for %q; use https", host)
}

// URLPolicyError is a web address the URL policy refuses. Configurations an earlier release accepted
// can hold one, so loading a committed configuration treats it differently from a new change
// (config.ParseCommitted).
type URLPolicyError struct {
	Path   string // providers.<id>.base_url, tools.<id>.http.url
	Reason error
}

func (e *URLPolicyError) Error() string { return e.Path + ": " + e.Reason.Error() }
func (e *URLPolicyError) Unwrap() error { return e.Reason }

// ValidateLenient is Validate that reports a refused web address separately instead of failing on it:
// problem is non-nil when the only thing wrong is the address; err when anything else is.
func ValidateLenient(id string, s Settings) (problem *URLPolicyError, err error) {
	if err := validate(id, s, true); err != nil {
		return nil, err
	}
	if s.HTTP != nil && (s.Enabled == nil || *s.Enabled) {
		if err := checkToolURL(id, s); err != nil {
			var pe *URLPolicyError
			if errors.As(err, &pe) {
				return pe, nil
			}
			return nil, err
		}
	}
	return nil, nil
}

// RequiredParams lists required parameters of an operator tool.
func (s Settings) RequiredParams() []string {
	var out []string
	for _, p := range s.Params {
		if p.Required {
			out = append(out, p.Name)
		}
	}
	return out
}

// Custom reports whether the entry defines an operator tool.
func (s Settings) Custom() bool { return s.HTTP != nil }

// Resolved is a tool's effective state after merging built-in defaults, config and feature flags.
type Resolved struct {
	ID          string         `json:"id"`
	Kind        string         `json:"kind"` // builtin | http
	Enabled     bool           `json:"enabled"`
	Reason      string         `json:"reason,omitempty"` // why disabled: config | feature_flag
	Chat        bool           `json:"chat"`
	Commands    bool           `json:"commands"`
	CanChat     bool           `json:"can_chat"`    // the model may be offered it (a built-in made for conversation)
	CanCommand  bool           `json:"can_command"` // it has a Discord command
	Implemented bool           `json:"implemented"`
	Aliases     []string       `json:"aliases,omitempty"`
	Description string         `json:"description,omitempty"` // config override ("" = language pack)
	Route       string         `json:"route,omitempty"`
	Required    []string       `json:"required,omitempty"`
	Defaults    map[string]any `json:"defaults,omitempty"`
	Limits      Limits         `json:"limits"`
	Scoped      bool           `json:"scoped,omitempty"`
	Writes      bool           `json:"writes,omitempty"`
	Once        bool           `json:"-"` // Builtin.Once
	Inputs      []Input        `json:"inputs,omitempty"`
	Providers   []string       `json:"providers,omitempty"` // provider kinds, any one with a key is enough
	Uses        string         `json:"uses,omitempty"`
	Role        string         `json:"role,omitempty"`
	OptIn       bool           `json:"opt_in,omitempty"`
	WebContent  bool           `json:"web_content,omitempty"`
	Sample      map[string]any `json:"sample,omitempty"`
	HTTP        *HTTP          `json:"-"`
}

func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

// Resolve merges built-ins with config. features is config `features` (nil = all on). Operator
// tools follow built-ins in ID order.
func Resolve(cfg map[string]Settings, features map[string]bool) []Resolved {
	var out []Resolved
	for _, b := range Builtins() {
		s := cfg[b.ID]
		r := Resolved{ID: b.ID, Kind: "builtin", Implemented: b.Implemented, Required: b.Required, Scoped: b.Scoped, Writes: b.Writes, Once: b.Once,
			Description: s.Description, Route: s.Route, Defaults: s.Defaults, Limits: s.Limits, Inputs: b.Inputs, Providers: b.Providers, Uses: b.Uses, Role: b.Role, OptIn: b.OptIn, Sample: b.Sample,
			WebContent: b.WebContent}
		r.Enabled = boolOr(s.Enabled, !b.OptIn)
		if !r.Enabled && s.Enabled == nil {
			r.Reason = "opt_in"
		} else if !r.Enabled {
			r.Reason = "config"
		} else if b.Feature != "" && features != nil {
			if on, ok := features[b.Feature]; ok && !on {
				r.Enabled, r.Reason = false, "feature_flag"
			}
		}
		// a built-in is offered in conversation or as a command only where it was made for it (fortune
		// and notices go through consent); config can only turn it off. Pictures are drawn in
		// conversation too: the previous bot's logs show people ask for them there ("고양이 그려줘"),
		// and each is held by the image tool's own daily limits and the monthly budget.
		r.CanChat, r.CanCommand = b.Chat && b.Implemented, b.Command
		r.Chat = r.Enabled && r.CanChat && boolOr(s.Chat, true)
		r.Commands = r.Enabled && r.CanCommand && boolOr(s.Commands, true)
		if len(s.Aliases) > 0 {
			r.Aliases = s.Aliases
		}
		out = append(out, r)
	}
	var custom []string
	for id, s := range cfg {
		if s.Custom() {
			custom = append(custom, id)
		}
	}
	slices.Sort(custom)
	for _, id := range custom {
		s := cfg[id]
		r := Resolved{ID: id, Kind: "http", Implemented: true, Description: s.Description, Route: s.Route, Required: s.RequiredParams(),
			Defaults: s.Defaults, Limits: s.Limits, Aliases: s.Aliases, HTTP: s.HTTP, WebContent: true} // whatever the endpoint returns
		for _, p := range s.Params {
			r.Inputs = append(r.Inputs, Input{Name: p.Name, Type: "text", Required: p.Required})
		}
		r.Enabled = boolOr(s.Enabled, true)
		if !r.Enabled {
			r.Reason = "config"
		}
		r.CanChat, r.CanCommand = true, true
		r.Chat = r.Enabled && boolOr(s.Chat, true)
		r.Commands = r.Enabled && boolOr(s.Commands, len(s.Aliases) > 0)
		out = append(out, r)
	}
	return out
}

// Find returns one resolved tool.
func Find(all []Resolved, id string) (Resolved, bool) {
	for _, r := range all {
		if r.ID == id {
			return r, true
		}
	}
	return Resolved{}, false
}
