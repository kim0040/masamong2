package discord

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/bwmarrin/discordgo"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/i18n"
)

// Command binds one Discord surface (slash command or legacy prefix alias) to an Application Action.
// Names follow 15장 §6: canonical English here; every other language (command and option names,
// descriptions, choice labels, default prefix words) comes from the message catalogs under
// discord.command.<name>.*, so adding a language never touches this file. Default prefix words must
// stay in sync with contracts/sandbox/tool-parity.json (테스트 샌드박스 !명령).
type Command struct {
	Name      string
	Desc      string
	Action    string
	Aliases   []string // default prefix words: every locale's discord.command.<name>.aliases
	Options   []*discordgo.ApplicationCommandOption
	Build     func(opts map[string]string, ctx InvokeContext) (string, map[string]any) // returns action id + input
	Ephemeral bool
	// DiscordOnly commands are personal or admin surfaces (consent, subscriptions), not agent tools the
	// test sandbox runs, so they are not in contracts/sandbox/tool-parity.json.
	DiscordOnly bool
	// PrefixOnly commands are extra "!" words for another command's subcommand (V1's !이번달운세); the
	// slash command set already covers them.
	PrefixOnly bool
	// ManagersOnly slash commands are shown only to members with Manage Server (Discord hides them from
	// everyone else; the server can widen that in its integration settings). The Action still checks.
	ManagersOnly bool
}

// messages is the catalog commands and renders read. The gateway points it at the service catalog so
// operator locale bundles apply.
var messages = i18n.New()

// SetCatalog replaces the message catalog (the daemon passes the service's).
func SetCatalog(c *i18n.Catalog) {
	if c != nil {
		messages = c
	}
}

// discordLocales maps catalog locales to Discord locale IDs.
var discordLocales = map[string]discordgo.Locale{
	"ko": discordgo.Korean, "ja": discordgo.Japanese, "zh-Hans": discordgo.ChineseCN, "es": discordgo.SpanishES,
}

// localizations is key's text in every non-English loaded locale, keyed by Discord locale.
func localizations(key string) map[discordgo.Locale]string {
	out := map[discordgo.Locale]string{}
	for _, loc := range messages.Locales() {
		dl, ok := discordLocales[loc]
		if !ok {
			continue
		}
		if v, ok := messages.Lookup(loc, key); ok {
			out[dl] = v
		}
	}
	return out
}

// InvokeContext is the minimum Discord context an Action needs. IDs are opaque scope/participant keys;
// they are never joined to a person registry here (COMMON: 채널 연결은 계정 병합이 아니다).
type InvokeContext struct {
	ChannelID string
	GuildID   string
	UserID    string
	Locale    string
	Manager   bool // owns or manages the server (Discord permissions)
	Private   bool // only the invoker sees the answer (a DM, or an ephemeral slash reply)
	UserName  string
}

func option(t discordgo.ApplicationCommandOptionType, name, desc string, required bool) *discordgo.ApplicationCommandOption {
	return &discordgo.ApplicationCommandOption{Type: t, Name: name, Description: desc, Required: required}
}

func hoursChoice(hours int, en string) *discordgo.ApplicationCommandOptionChoice {
	return &discordgo.ApplicationCommandOptionChoice{Name: en, Value: hours}
}

func aliasesFor(name string) []string {
	var out []string
	seen := map[string]bool{}
	locs := messages.Locales()
	sort.Strings(locs)
	for _, loc := range locs {
		v, ok := messages.Lookup(loc, "discord.command."+name+".aliases")
		if !ok {
			continue
		}
		for _, a := range strings.Split(v, ",") {
			if a = strings.TrimSpace(a); a != "" && !seen[a] {
				seen[a] = true
				out = append(out, a)
			}
		}
	}
	return out
}

func aliasesForLocale(name, locale string) []string {
	value, ok := messages.Lookup(locale, "discord.command."+name+".aliases")
	if !ok {
		value, _ = messages.Lookup("en", "discord.command."+name+".aliases")
	}
	var out []string
	for _, alias := range strings.Split(value, ",") {
		if alias = strings.TrimSpace(alias); alias != "" {
			out = append(out, alias)
		}
	}
	return out
}

// commandHints are the commands members can run, for the application's help (manager-only commands and
// prefix-only extra words are left out).
func commandHints() []application.CommandHint {
	var out []application.CommandHint
	for _, c := range Commands() {
		if !c.ManagersOnly && !c.PrefixOnly {
			out = append(out, application.CommandHint{Name: c.Name, Action: c.Action})
		}
	}
	return out
}

// Commands is the registered surface. Every entry maps to a catalog Action; nothing here owns logic.
func Commands() []Command {
	str := discordgo.ApplicationCommandOptionString
	cmds := []Command{
		{
			Name: "weather", Desc: "Current weather from KMA", Action: "weather.query",
			Options: []*discordgo.ApplicationCommandOption{option(str, "region", "City name (e.g. Seoul)", false)},
			Build: func(o map[string]string, c InvokeContext) (string, map[string]any) {
				in := map[string]any{}
				if r := strings.TrimSpace(o["region"]); r != "" {
					in["region"] = r
				}
				return "weather.query", in
			},
		},
		{
			Name: "alerts", Desc: "Earthquake notices and active weather warnings", Action: "disasters.query",
			Build: func(o map[string]string, c InvokeContext) (string, map[string]any) {
				return "disasters.query", map[string]any{}
			},
		},
		{
			Name: "quote", Desc: "Stock quote", Action: "finance.quote",
			Options: []*discordgo.ApplicationCommandOption{option(str, "symbol", "Ticker symbol (e.g. AAPL)", true)},
			Build: func(o map[string]string, c InvokeContext) (string, map[string]any) {
				return "finance.quote", map[string]any{"symbol": o["symbol"], "krw": true}
			},
		},
		{
			Name: "fx", Desc: "Exchange rates", Action: "finance.fx",
			Options: []*discordgo.ApplicationCommandOption{option(str, "base", "Base currency (default USD)", false)},
			Build: func(o map[string]string, c InvokeContext) (string, map[string]any) {
				return "finance.fx", map[string]any{"base": orDefault(o["base"], "USD")}
			},
		},
		{
			Name: "search", Desc: "Web search", Action: "search.web",
			Options: []*discordgo.ApplicationCommandOption{option(str, "query", "What to search for", true)},
			Build: func(o map[string]string, c InvokeContext) (string, map[string]any) {
				return "search.web", map[string]any{"query": o["query"], "size": 3}
			},
		},
		{
			Name: "ranking", Desc: "Who talked the most in this channel", Action: "activity.ranking",
			Options: []*discordgo.ApplicationCommandOption{option(str, "period", "today, week, month or all", false)},
			Build: func(o map[string]string, c InvokeContext) (string, map[string]any) {
				return "activity.ranking", map[string]any{"period": rankingPeriod(o["period"]), "scope": "discord:" + c.ChannelID}
			},
		},
		{
			Name: "places", Desc: "Find places on Kakao Map", Action: "places.search",
			Options: []*discordgo.ApplicationCommandOption{option(str, "query", "Area and kind of place", true)},
			Build: func(o map[string]string, c InvokeContext) (string, map[string]any) {
				return "places.search", map[string]any{"query": o["query"], "size": 5}
			},
		},
		{
			Name: "poll", Desc: "Create a Discord poll in this channel", Action: "polls.create",
			Options: []*discordgo.ApplicationCommandOption{
				option(str, "question", "Poll question (max 300 characters)", true),
				option(str, "answers", "Answers, comma-separated (2–10, 55 chars each). Empty = Yes/No", false),
				func() *discordgo.ApplicationCommandOption {
					o := option(discordgo.ApplicationCommandOptionInteger, "duration", "How long the poll stays open", false)
					o.Choices = []*discordgo.ApplicationCommandOptionChoice{
						hoursChoice(1, "1 hour"), hoursChoice(4, "4 hours"), hoursChoice(8, "8 hours"), hoursChoice(24, "24 hours"),
						hoursChoice(72, "3 days"), hoursChoice(168, "1 week"), hoursChoice(768, "32 days (max)"),
					}
					return o
				}(),
				option(discordgo.ApplicationCommandOptionBoolean, "multiselect", "Allow picking more than one answer", false),
			},
			Build: func(o map[string]string, c InvokeContext) (string, map[string]any) {
				raw := o["answers"]
				if raw == "" {
					raw = o["options"]
				}
				var opts []any
				for i, part := range strings.Split(raw, ",") {
					if part = strings.TrimSpace(part); part != "" {
						opts = append(opts, map[string]any{"id": fmt.Sprintf("o%d", i+1), "label": part})
					}
				}
				hours := 24
				fmt.Sscanf(o["duration"], "%d", &hours)
				multi := strings.EqualFold(strings.TrimSpace(o["multiselect"]), "true")
				in := map[string]any{
					"question":          o["question"],
					"options":           opts,
					"duration_hours":    hours,
					"allow_multiselect": multi,
					"scope":             "discord:" + c.ChannelID,
				}
				return "polls.create", in
			},
		},
		{
			Name: "image", Desc: "Draw a picture from a description", Action: "images.generate",
			Options: []*discordgo.ApplicationCommandOption{option(str, "prompt", "What to draw", true)},
			Build: func(o map[string]string, c InvokeContext) (string, map[string]any) {
				return "images.generate", map[string]any{"prompt": o["prompt"], "scope": "discord:" + c.ChannelID}
			},
		},
		{
			Name: "fortune", Desc: "Today's fortune, detailed, this month or year; your subscription", Action: "fortune.query",
			Ephemeral: true, DiscordOnly: true,
			Options: []*discordgo.ApplicationCommandOption{
				choiceOption("what", "What to see or do", false, "today", "detail", "month", "year", "animals", "status", "subscribe", "unsubscribe", "delete"),
				option(str, "time", "Briefing time HH:MM (with subscribe)", false),
			},
			Build: func(o map[string]string, c InvokeContext) (string, map[string]any) {
				verb, rest := splitVerb(o["what"], o["time"], wordsFor("fortune", "verb",
					"today", "detail", "month", "year", "animals", "status", "subscribe", "unsubscribe", "delete", "register"))
				return fortuneRequest(verb, rest)
			},
		},
		{
			Name: "fortune-register", Desc: "Register your birthday for personal fortunes", Action: "fortune.subscribe",
			Ephemeral: true, DiscordOnly: true,
			Options: []*discordgo.ApplicationCommandOption{
				option(str, "birth_date", "Date of birth, YYYY-MM-DD", true),
				option(str, "birth_time", "Time of birth HH:MM (optional)", false),
				choiceOption("gender", "Gender (optional)", false, "male", "female"),
				option(str, "birth_place", "Birthplace, a city (optional)", false),
			},
			Build: func(o map[string]string, c InvokeContext) (string, map[string]any) {
				in := map[string]any{"op": "register"}
				for _, k := range []string{"birth_date", "birth_time", "gender", "birth_place"} {
					if v := strings.TrimSpace(o[k]); v != "" {
						in[k] = v
					}
				}
				return "fortune.subscribe", in
			},
		},
		{
			Name: "fortune-month", Desc: "This month's fortune", Action: "fortune.query", DiscordOnly: true, PrefixOnly: true,
			Build: func(o map[string]string, c InvokeContext) (string, map[string]any) {
				return "fortune.query", map[string]any{"what": "month"}
			},
		},
		{
			Name: "fortune-year", Desc: "This year's fortune", Action: "fortune.query", DiscordOnly: true, PrefixOnly: true,
			Build: func(o map[string]string, c InvokeContext) (string, map[string]any) {
				return "fortune.query", map[string]any{"what": "year"}
			},
		},
		{
			Name: "subscribe", Desc: "Morning fortune briefing at a time", Action: "fortune.subscribe", DiscordOnly: true, PrefixOnly: true,
			Options: []*discordgo.ApplicationCommandOption{option(str, "time", "HH:MM", false)},
			Build: func(o map[string]string, c InvokeContext) (string, map[string]any) {
				return fortuneRequest("subscribe", o["time"])
			},
		},
		{
			Name: "zodiac", Desc: "Today's fortune of a zodiac sign, or all twelve ranked", Action: "fortune.query", DiscordOnly: true,
			Options: []*discordgo.ApplicationCommandOption{option(str, "sign", "Sign name, or ranking (empty = your own sign)", false)},
			Build: func(o map[string]string, c InvokeContext) (string, map[string]any) {
				sign := strings.TrimSpace(o["sign"])
				if wordsFor("zodiac", "verb", "ranking")[strings.ToLower(sign)] == "ranking" {
					return "fortune.query", map[string]any{"what": "ranking"}
				}
				return "fortune.query", map[string]any{"what": "zodiac", "sign": sign}
			},
		},
		{
			Name: "notices", Desc: "School notices picked for you; your notice settings", Action: "notices.status",
			Ephemeral: true, DiscordOnly: true,
			Options: []*discordgo.ApplicationCommandOption{
				choiceOption("what", "What to see or do", false, "latest", "profile", "status", "time", "pause", "resume", "delete", "mute", "unmute"),
				option(str, "value", "Time HH:MM, or a topic for mute/unmute", false),
			},
			Build: func(o map[string]string, c InvokeContext) (string, map[string]any) {
				verb, rest := splitVerb(o["what"], o["value"], wordsFor("notices", "verb",
					"latest", "profile", "status", "time", "pause", "resume", "delete", "mute", "unmute", "register"))
				return noticeRequest("school", verb, rest)
			},
		},
		{
			Name: "notices-register", Desc: "Register your school for notices picked for you", Action: "notices.subscribe",
			Ephemeral: true, DiscordOnly: true,
			Options: []*discordgo.ApplicationCommandOption{
				option(str, "school", "School name (e.g. 전북대학교)", true),
				choiceOption("degree", "Degree program", false, "undergraduate", "master", "doctorate", "integrated"),
				func() *discordgo.ApplicationCommandOption {
					o := option(discordgo.ApplicationCommandOptionInteger, "grade", "Year, for undergraduates (1-6)", false)
					one, six := 1.0, 6.0
					o.MinValue, o.MaxValue = &one, six
					return o
				}(),
				option(str, "department", "Department or major", false),
				option(str, "campus", "Campus", false),
				option(str, "interests", "Topics you want first, comma-separated", false),
				option(str, "time", "Delivery time HH:MM", false),
			},
			Build: func(o map[string]string, c InvokeContext) (string, map[string]any) {
				in := map[string]any{"kind": "school", "op": "register"}
				for _, k := range []string{"school", "degree", "department", "campus", "interests", "time"} {
					if v := strings.TrimSpace(o[k]); v != "" {
						in[k] = v
					}
				}
				if g, err := strconv.Atoi(strings.TrimSpace(o["grade"])); err == nil {
					in["grade"] = g
				}
				return "notices.subscribe", in
			},
		},
		{
			Name: "transfer", Desc: "Recent transfer admission notices; your subscription to universities", Action: "transfer.recent",
			Ephemeral: true, DiscordOnly: true,
			Options: []*discordgo.ApplicationCommandOption{
				choiceOption("what", "What to see or do", false, "recent", "subscribe", "status", "time", "pause", "resume", "delete"),
				option(str, "value", "Universities (comma-separated) or a time HH:MM", false),
			},
			Build: func(o map[string]string, c InvokeContext) (string, map[string]any) {
				verb, rest := splitVerb(o["what"], o["value"], wordsFor("transfer", "verb",
					"recent", "subscribe", "status", "time", "pause", "resume", "delete"))
				if verb == "" || verb == "recent" {
					in := map[string]any{}
					if rest != "" {
						in["universities"] = rest
					}
					return "transfer.recent", in
				}
				return noticeRequest("transfer", verb, rest)
			},
		},
		{
			Name: "summary", Desc: "Summarize this channel's recent conversation", Action: "summaries.channel", DiscordOnly: true,
			Build: func(o map[string]string, c InvokeContext) (string, map[string]any) {
				return "summaries.channel", map[string]any{}
			},
		},
		{
			Name: "mysettings", Desc: "My settings: weather place, time zone, quiet hours, language", Action: "preferences.set",
			Ephemeral: true, DiscordOnly: true,
			Options: []*discordgo.ApplicationCommandOption{
				option(str, "region", "Default place for weather (e.g. Seoul)", false),
				option(str, "timezone", "Time zone (e.g. Asia/Seoul)", false),
				option(str, "quiet", "Quiet hours for scheduled DMs, HH:MM-HH:MM (\"off\" clears)", false),
				choiceOption("language", "Language of my cards and DMs", false, "ko", "en", "default"),
			},
			Build: func(o map[string]string, c InvokeContext) (string, map[string]any) {
				return "preferences.set", settingsInput(o)
			},
		},
		{
			Name: "admin", Desc: "Run the bot in this channel: pause, replies, memory, chiming in, blocking (server managers)",
			Action: "access.policy", Ephemeral: true, DiscordOnly: true, ManagersOnly: true,
			Options: []*discordgo.ApplicationCommandOption{
				choiceOption("op", "What to do", false, "status", "pause", "resume", "respond", "memory", "chime", "block", "unblock"),
				choiceOption("mode", "Setting (reply mode, memory on/off, chiming in)", false, "always", "mention", "command", "off", "on", "collect", "shadow", "conservative"),
				option(discordgo.ApplicationCommandOptionUser, "user", "Who to block or unblock in this server", false),
			},
			Build: func(o map[string]string, c InvokeContext) (string, map[string]any) {
				verb, rest := splitVerb(o["op"], o["mode"], wordsFor("admin", "verb", "status", "pause", "resume", "respond", "memory", "chime", "block", "unblock"))
				if verb == "" {
					verb = "status"
				}
				in := map[string]any{"op": verb}
				switch verb {
				case "respond", "memory", "chime":
					mode := wordsFor("admin", "mode", "always", "mention", "command", "off", "on", "collect", "shadow", "conservative")[strings.ToLower(rest)]
					if mode == "" {
						mode = rest
					}
					if verb == "chime" && mode == "on" {
						mode = "conservative" // "chiming in on" means it may speak
					}
					in["mode"] = mode
				case "block", "unblock":
					in["user"] = orDefault(o["user"], rest)
				}
				return "access.policy", in
			},
		},
		{
			Name: "privacy", Desc: "See, give or withdraw consent for memory, fortune, notices and chiming in", Action: "privacy.consent",
			Ephemeral: true, DiscordOnly: true,
			Options: []*discordgo.ApplicationCommandOption{
				choiceOption("action", "What to do", false, "status", "grant", "withdraw"),
				choiceOption("scope", "Which purpose", false, "memory", "fortune", "school_notice", "transfer_notice", "participation", "profile"),
			},
			Build: func(o map[string]string, c InvokeContext) (string, map[string]any) {
				verb, scope := splitVerb(o["action"], o["scope"], wordsFor("privacy", "verb", "status", "grant", "withdraw"))
				in := map[string]any{}
				if scope != "" {
					in["scope"] = scope
				}
				if verb == "grant" || verb == "withdraw" {
					in["decision"] = verb
				}
				return "privacy.consent", in
			},
		},
		{
			Name: "remind", Desc: "Remind me at a time (confirm the time and where it goes)", Action: "reminders.propose",
			Ephemeral: true, DiscordOnly: true,
			Options: []*discordgo.ApplicationCommandOption{
				option(str, "when", "When, as you would say it (e.g. tomorrow 3pm, in 30 minutes)", true),
				option(str, "what", "What to remind you of", false),
				choiceOption("repeat", "Repeat it (leave empty for once)", false, "daily", "weekly", "monthly"),
			},
			Build: func(o map[string]string, c InvokeContext) (string, map[string]any) {
				// the weekday of a weekly reminder and the day of a monthly one come from the time given
				in := map[string]any{"text": o["when"]}
				if what := strings.TrimSpace(o["what"]); what != "" {
					in = map[string]any{"when": o["when"], "what": what}
				}
				if rep := o["repeat"]; rep != "" {
					in["repeat"] = rep
				}
				return "reminders.propose", in
			},
		},
		{
			Name: "reminders", Desc: "My reminders still to come, with cancel buttons", Action: "reminders.update",
			Ephemeral: true, DiscordOnly: true,
			Build: func(o map[string]string, c InvokeContext) (string, map[string]any) {
				return "reminders.update", map[string]any{"op": "list"}
			},
		},
		{
			// what the bot keeps that names the member, in this server only (memory.mine.show)
			Name: "mymemory", Desc: "What the bot remembers about you in this server", Action: "memory.mine.show",
			Ephemeral: true, DiscordOnly: true,
			Build: func(o map[string]string, c InvokeContext) (string, map[string]any) {
				return "memory.mine.show", map[string]any{}
			},
		},
		{
			// a preview of the member's own memory with a confirm button (memory.mine.forget, buttons in memory_buttons.go)
			Name: "forgetme", Desc: "See, then delete, what the bot remembers about you in this server", Action: "memory.mine.forget",
			Ephemeral: true, DiscordOnly: true,
			Build: func(o map[string]string, c InvokeContext) (string, map[string]any) {
				return "memory.mine.forget", map[string]any{}
			},
		},
		{
			Name: "status", Desc: "Bot status", Action: "runtime.doctor", Ephemeral: true,
			Build: func(o map[string]string, c InvokeContext) (string, map[string]any) {
				return "runtime.doctor", map[string]any{}
			},
		},
		{
			Name: "help", Desc: "List commands", Action: "help.version", Ephemeral: true,
			Build: func(o map[string]string, c InvokeContext) (string, map[string]any) {
				return "help.version", map[string]any{}
			},
		},
	}
	for i := range cmds {
		cmds[i].Aliases = aliasesFor(cmds[i].Name)
	}
	return cmds
}

// noticeRequest maps a notices or transfer verb (and what follows it) to its Action.
func noticeRequest(kind, verb, rest string) (string, map[string]any) {
	rest = strings.TrimSpace(rest)
	switch verb {
	case "", "latest":
		return "notices.status", map[string]any{"kind": kind, "what": "latest"}
	case "profile", "status":
		if kind == "transfer" && verb == "status" {
			return "notices.status", map[string]any{"kind": kind, "what": "profile"}
		}
		return "notices.status", map[string]any{"kind": kind, "what": verb}
	case "register", "subscribe":
		in := map[string]any{"kind": kind, "op": verb}
		if kind == "transfer" {
			in["universities"] = rest
		} else {
			in["text"] = rest
		}
		return "notices.subscribe", in
	case "time":
		return "notices.subscribe", map[string]any{"kind": kind, "op": "time", "time": rest}
	case "mute", "unmute":
		return "notices.subscribe", map[string]any{"kind": kind, "op": verb, "topic": rest}
	}
	return "notices.subscribe", map[string]any{"kind": kind, "op": verb}
}

// fortuneRequest maps a fortune verb (and what follows it) to its Action.
func fortuneRequest(verb, rest string) (string, map[string]any) {
	rest = strings.TrimSpace(rest)
	switch verb {
	case "", "today", "detail", "month", "year", "animals": // animals: the twelve 띠, the same for everyone
		if verb == "" {
			verb = "today"
		}
		return "fortune.query", map[string]any{"what": verb}
	case "subscribe":
		if rest == "" || wordsFor("fortune", "value", "off")[strings.ToLower(rest)] == "off" {
			if rest != "" {
				return "fortune.subscribe", map[string]any{"op": "unsubscribe"}
			}
			return "fortune.subscribe", map[string]any{"op": "status"}
		}
		return "fortune.subscribe", map[string]any{"op": "subscribe", "time": rest}
	case "register":
		return "fortune.subscribe", map[string]any{"op": "register", "text": rest}
	}
	return "fortune.subscribe", map[string]any{"op": verb}
}

// settingsInput turns /mysettings options (or "!내설정 지역 서울" words) into preferences.set input.
func settingsInput(o map[string]string) map[string]any {
	in := map[string]any{}
	if o["timezone"] == "" && o["quiet"] == "" && o["language"] == "" {
		// prefix form: everything is in the first option
		verb, rest := splitVerb(o["region"], "", wordsFor("mysettings", "verb", "region", "timezone", "quiet", "language", "reset"))
		switch verb {
		case "reset":
			return map[string]any{"reset": true}
		case "":
			if rest != "" {
				o = map[string]string{"region": rest}
			} else {
				return in
			}
		default:
			o = map[string]string{verb: rest}
		}
	}
	if v := strings.TrimSpace(o["region"]); v != "" {
		in["weather_region"] = v
	}
	if v := strings.TrimSpace(o["timezone"]); v != "" {
		in["timezone"] = v
	}
	if v := strings.TrimSpace(o["quiet"]); v != "" {
		if a, b, ok := strings.Cut(strings.ReplaceAll(v, "~", "-"), "-"); ok {
			in["quiet_start"], in["quiet_end"] = strings.TrimSpace(a), strings.TrimSpace(b)
		} else {
			in["quiet_start"], in["quiet_end"] = "", ""
		}
	}
	switch v := strings.TrimSpace(o["language"]); {
	case v == "":
	case wordsFor("mysettings", "value", "default")[strings.ToLower(v)] == "default":
		in["locale"] = ""
	default:
		in["locale"] = v
	}
	return in
}

// choiceOption is a string option limited to fixed values (labels come from the catalogs:
// discord.command.<name>.choice.<option>.<value>).
func choiceOption(name, desc string, required bool, values ...string) *discordgo.ApplicationCommandOption {
	o := option(discordgo.ApplicationCommandOptionString, name, desc, required)
	for _, v := range values {
		o.Choices = append(o.Choices, &discordgo.ApplicationCommandOptionChoice{Name: v, Value: v})
	}
	return o
}

// wordsFor maps the words people may type for a command's fixed values to the values: each value
// itself plus every locale's comma-separated discord.command.<cmd>.<group>.<value> (e.g. the Korean
// "동의" for privacy's grant). Words live in the message catalogs, never in code.
func wordsFor(cmd, group string, values ...string) map[string]string {
	out := map[string]string{}
	for _, v := range values {
		out[v] = v
		for _, loc := range messages.Locales() {
			words, ok := messages.Lookup(loc, "discord.command."+cmd+"."+group+"."+v)
			if !ok {
				continue
			}
			for _, w := range strings.Split(words, ",") {
				if w = strings.ToLower(strings.TrimSpace(w)); w != "" {
					out[w] = v
				}
			}
		}
	}
	return out
}

// splitVerb reads "verb rest" from a prefix command's text (everything lands in the first option) or
// the slash command's separate options, and maps the verb through verbs ("" when absent or unknown:
// then the whole text is the rest).
func splitVerb(first, rest string, verbs map[string]string) (string, string) {
	first = strings.TrimSpace(first)
	if rest == "" {
		head, tail, _ := strings.Cut(first, " ")
		if v, ok := verbs[strings.ToLower(head)]; ok {
			return v, strings.TrimSpace(tail)
		}
		if first != "" {
			return "", first
		}
		return "", ""
	}
	return verbs[strings.ToLower(first)], strings.TrimSpace(rest)
}

func orDefault(v, d string) string {
	if strings.TrimSpace(v) == "" {
		return d
	}
	return v
}

// localized fills Discord name/description localizations for a command and its options.
func localized(c Command) *discordgo.ApplicationCommand {
	base := "discord.command." + c.Name
	ac := &discordgo.ApplicationCommand{Name: c.Name, Description: c.Desc}
	if c.ManagersOnly {
		perms := int64(discordgo.PermissionManageGuild)
		ac.DefaultMemberPermissions = &perms
	}
	if l := localizations(base + ".name"); len(l) > 0 {
		ac.NameLocalizations = &l
	}
	if l := localizations(base + ".description"); len(l) > 0 {
		ac.DescriptionLocalizations = &l
	}
	for _, o := range c.Options {
		cp := *o
		ob := base + ".option." + o.Name
		if l := localizations(ob + ".name"); len(l) > 0 {
			cp.NameLocalizations = l
		}
		if l := localizations(ob + ".description"); len(l) > 0 {
			cp.DescriptionLocalizations = l
		}
		if len(o.Choices) > 0 {
			cp.Choices = nil
			for _, ch := range o.Choices {
				cc := *ch
				if l := localizations(fmt.Sprintf("%s.choice.%s.%v", base, o.Name, ch.Value)); len(l) > 0 {
					cc.NameLocalizations = l
				}
				cp.Choices = append(cp.Choices, &cc)
			}
		}
		ac.Options = append(ac.Options, &cp)
	}
	return ac
}

// ApplicationCommands renders the registration payload. Its JSON hash is compared before syncing (15장 §6).
func ApplicationCommands() []*discordgo.ApplicationCommand {
	return ApplicationCommandsFor(nil)
}

// ApplicationCommandsFor renders only the commands whose Action allowed accepts (nil = all), so a tool
// switched off for commands is not offered as a slash command.
func ApplicationCommandsFor(allowed func(action string) bool) []*discordgo.ApplicationCommand {
	var out []*discordgo.ApplicationCommand
	for _, c := range Commands() {
		if c.PrefixOnly || (allowed != nil && !allowed(c.Action)) {
			continue
		}
		out = append(out, localized(c))
	}
	return append(out, rateCommand())
}

// CommandsHash identifies the registration payload so startup only re-syncs when it changed.
func CommandsHash() string {
	return hashCommands(ApplicationCommands())
}

func hashCommands(cmds []*discordgo.ApplicationCommand) string {
	b, _ := json.Marshal(cmds)
	return fmt.Sprintf("%x", fnv64(b))
}

func fnv64(b []byte) uint64 {
	h := uint64(14695981039346656037)
	for _, c := range b {
		h ^= uint64(c)
		h *= 1099511628211
	}
	return h
}

// LocalName is the command's name in locale (falls back to the canonical name).
func LocalName(c Command, locale string) string {
	if v, ok := messages.Lookup(locale, "discord.command."+c.Name+".name"); ok {
		return v
	}
	return c.Name
}

// LocalDesc is the command's description in locale.
func LocalDesc(c Command, locale string) string {
	if v, ok := messages.Lookup(locale, "discord.command."+c.Name+".description"); ok {
		return v
	}
	return c.Desc
}

// ByName / ByAlias resolve a Discord surface to a Command.
func ByName(name string) (Command, bool) {
	for _, c := range Commands() {
		if c.Name == name {
			return c, true
		}
	}
	return Command{}, false
}

func ByAlias(alias string) (Command, bool) {
	return PrefixTable{}.byAlias(alias)
}

// ToolCommand is the connector view of application.ToolCommand (prefix words per tool from config).
type ToolCommand struct {
	Tool       string
	Aliases    []string
	Custom     bool
	FirstParam string
}

// PrefixTable resolves "!word" with config overrides: configured aliases replace a built-in
// command's default words, and operator tools with aliases become commands whose text fills their
// first parameter. The zero value uses the defaults only.
type PrefixTable struct {
	overrides map[string][]string // action → aliases
	custom    []ToolCommand
}

// NewPrefixTable builds the table from the service's tool commands.
func NewPrefixTable(tc []ToolCommand) PrefixTable {
	t := PrefixTable{overrides: map[string][]string{}}
	for _, c := range tc {
		if c.Custom {
			t.custom = append(t.custom, c)
		} else if len(c.Aliases) > 0 {
			t.overrides[c.Tool] = c.Aliases
		}
	}
	return t
}

func (t PrefixTable) byAlias(alias string) (Command, bool) {
	alias = strings.ToLower(strings.TrimSpace(alias))
	for _, c := range Commands() {
		words := c.Aliases
		if o, ok := t.overrides[c.Action]; ok {
			words = o
		}
		for _, a := range words {
			if strings.ToLower(a) == alias {
				return c, true
			}
		}
	}
	for _, c := range t.custom {
		for _, a := range c.Aliases {
			if strings.ToLower(a) == alias {
				return customCommand(c), true
			}
		}
	}
	return Command{}, false
}

func customCommand(c ToolCommand) Command {
	param := c.FirstParam
	cmd := Command{Name: c.Tool, Desc: c.Tool, Action: c.Tool, Aliases: c.Aliases}
	if param != "" {
		cmd.Options = []*discordgo.ApplicationCommandOption{option(discordgo.ApplicationCommandOptionString, param, param, false)}
	}
	cmd.Build = func(o map[string]string, _ InvokeContext) (string, map[string]any) {
		in := map[string]any{}
		if param != "" && strings.TrimSpace(o[param]) != "" {
			in[param] = strings.TrimSpace(o[param])
		}
		return c.Tool, in
	}
	return cmd
}

// ParsePrefix splits "!날씨 서울" into the command and its positional text with default aliases.
// Returns ok=false when the message does not start with the prefix or the alias is unknown.
func ParsePrefix(prefix, content string) (Command, map[string]string, bool) {
	return PrefixTable{}.Parse(prefix, content)
}

// Parse is ParsePrefix with the table's aliases.
func (t PrefixTable) Parse(prefix, content string) (Command, map[string]string, bool) {
	if prefix == "" || !strings.HasPrefix(content, prefix) {
		return Command{}, nil, false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(content, prefix))
	if rest == "" {
		return Command{}, nil, false
	}
	head, tail, _ := strings.Cut(rest, " ")
	cmd, ok := t.byAlias(head)
	if !ok {
		return Command{}, nil, false
	}
	opts := map[string]string{}
	tail = strings.TrimSpace(tail)
	if len(cmd.Options) > 0 && tail != "" {
		if cmd.Name == "poll" {
			q, o, cut := strings.Cut(tail, "|")
			opts["question"] = strings.TrimSpace(q)
			if cut {
				opts["answers"] = strings.TrimSpace(o)
			}
		} else {
			opts[cmd.Options[0].Name] = tail
		}
	}
	return cmd, opts, true
}

// requestFor builds the Action request for a command invocation. The invoker lets the tool layer
// apply per-channel switches and per-user limits.
func requestFor(cmd Command, opts map[string]string, ctx InvokeContext, requestID string) action.Request {
	act, input := cmd.Build(opts, ctx)
	raw, _ := json.Marshal(input)
	return action.Request{RequestID: requestID, Action: act, Input: raw, Locale: ctx.Locale,
		Invoker: &action.Invoker{Via: "command", GuildID: ctx.GuildID, ChannelID: ctx.ChannelID, UserID: ctx.UserID, Manager: ctx.Manager, Private: ctx.Private, Name: ctx.UserName}}
}

// rankingPeriod reads the period people type after !랭킹 (오늘, 이번주, 이번달, 전체 …).
func rankingPeriod(v string) string {
	switch strings.ReplaceAll(strings.ToLower(strings.TrimSpace(v)), " ", "") {
	case "오늘", "today", "day":
		return "today"
	case "이번달", "한달", "month", "이달":
		return "month"
	case "전체", "all", "누적":
		return "all"
	}
	return "week"
}
