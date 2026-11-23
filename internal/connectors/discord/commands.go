package discord

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/bwmarrin/discordgo"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/currency"
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
	// Private cards show stored data about the caller: typed as "!" in a server, the card goes to the caller's
	// DMs and the room gets one line (private_cards.go).
	Private bool
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

// shippedCatalog is the catalog built into the program, with no operator bundles. It is read-only and shared:
// tooling that has no service (the code map) and gateways built without one read it. A running gateway reads
// its own service's catalog (Gateway.catalog).
var shippedCatalog = sync.OnceValue(i18n.New)

// discordLocales maps catalog locales to Discord locale IDs.
var discordLocales = map[string]discordgo.Locale{
	"ko": discordgo.Korean, "ja": discordgo.Japanese, "zh-Hans": discordgo.ChineseCN, "es": discordgo.SpanishES,
}

// localizations is key's text in every non-English loaded locale, keyed by Discord locale.
func localizations(cat *i18n.Catalog, key string) map[discordgo.Locale]string {
	out := map[discordgo.Locale]string{}
	for _, loc := range cat.Locales() {
		dl, ok := discordLocales[loc]
		if !ok {
			continue
		}
		if v, ok := cat.Lookup(loc, key); ok {
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
	Typed     bool // the options are words of a message (a prefix command), not values Discord already typed
	// Tag is the locale Discord reports (en-GB, es-419): the interaction's, or for a message the server's
	// preferred one. It keeps the region that Locale (the language the answer is written in, possibly a saved
	// preference) may have lost. Only the fx home currency reads it.
	Tag string
	// words is the catalog the command's typed words (verbs, values) are read from: Commands sets it, so a
	// command always reads the catalog of the surface it came from.
	words *i18n.Catalog
}

func option(t discordgo.ApplicationCommandOptionType, name, desc string, required bool) *discordgo.ApplicationCommandOption {
	return &discordgo.ApplicationCommandOption{Type: t, Name: name, Description: desc, Required: required}
}

func hoursChoice(hours int, en string) *discordgo.ApplicationCommandOptionChoice {
	return &discordgo.ApplicationCommandOptionChoice{Name: en, Value: hours}
}

func aliasesFor(cat *i18n.Catalog, name string) []string {
	var out []string
	seen := map[string]bool{}
	locs := cat.Locales()
	sort.Strings(locs)
	for _, loc := range locs {
		v, ok := cat.Lookup(loc, "discord.command."+name+".aliases")
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

func aliasesForLocale(cat *i18n.Catalog, name, locale string) []string {
	value, ok := cat.Lookup(locale, "discord.command."+name+".aliases")
	if !ok {
		value, _ = cat.Lookup("en", "discord.command."+name+".aliases")
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
func commandHints(cat *i18n.Catalog) []application.CommandHint {
	var out []application.CommandHint
	for _, c := range commandsIn(cat) {
		if !c.ManagersOnly && !c.PrefixOnly {
			out = append(out, application.CommandHint{Name: c.Name, Action: c.Action})
		}
	}
	return out
}

// Commands is the registered surface read through the built-in catalog (tooling and docs). A gateway reads its
// service's catalog through commandsIn.
func Commands() []Command { return commandsIn(shippedCatalog()) }

// commandsIn is the registered surface with its words (aliases, typed values) from cat.
func commandsIn(cat *i18n.Catalog) []Command { return surface{cat}.Commands() }

// surface is the command table read through one message catalog.
type surface struct{ cat *i18n.Catalog }

// Commands is the registered surface. Every entry maps to a catalog Action; nothing here owns logic.
// The docs checker (internal/docnames RegistryNames) reads the command names from the literal in this
// method, and takes the last func or method named Commands in this file: keep the table here, below the
// Commands function.
func (s surface) Commands() []Command {
	cat := s.cat
	str := discordgo.ApplicationCommandOptionString
	cmds := []Command{
		{
			Name: "weather", Desc: "Current weather from KMA", Action: "weather.query",
			Options: []*discordgo.ApplicationCommandOption{option(str, "region", "City name (e.g. Seoul)", false)},
			Build:   buildWeather,
		},
		{
			Name: "alerts", Desc: "Earthquake notices and active weather warnings", Action: "disasters.query",
			Build: buildAlerts,
		},
		{
			Name: "quote", Desc: "Stock quote", Action: "finance.quote",
			Options: []*discordgo.ApplicationCommandOption{option(str, "symbol", "Ticker symbol (e.g. AAPL)", true)},
			Build:   buildQuote,
		},
		{
			Name: "fx", Desc: "Exchange rates", Action: "finance.fx",
			Options: []*discordgo.ApplicationCommandOption{
				option(str, "currency", "Currency code or name (USD, yen): its value in your currency and the main ones", false),
				option(str, "base", "Currency to show values in, e.g. USD or JPY (default: your region's currency)", false),
				option(discordgo.ApplicationCommandOptionNumber, "amount", "Amount to convert", false),
			},
			Build: buildFX,
		},
		{
			Name: "search", Desc: "Web search", Action: "search.web",
			Options: []*discordgo.ApplicationCommandOption{option(str, "query", "What to search for", true)},
			Build:   buildSearch,
		},
		{
			Name: "ranking", Desc: "Who talked the most in this channel", Action: "activity.ranking",
			Options: []*discordgo.ApplicationCommandOption{option(str, "period", "today, week, month or all", false)},
			Build:   buildRanking,
		},
		{
			Name: "places", Desc: "Find places on Kakao Map", Action: "places.search",
			Options: []*discordgo.ApplicationCommandOption{option(str, "query", "Area and kind of place", true)},
			Build:   buildPlaces,
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
			Build: buildPoll,
		},
		{
			Name: "image", Desc: "Draw a picture from a description", Action: "images.generate",
			Options: []*discordgo.ApplicationCommandOption{option(str, "prompt", "What to draw", true)},
			Build:   buildImage,
		},
		{
			Name: "fortune", Desc: "Today's fortune, detailed, this month or year; your subscription", Action: "fortune.query",
			Ephemeral: true, DiscordOnly: true,
			Options: []*discordgo.ApplicationCommandOption{
				choiceOption("what", "What to see or do", false, "today", "detail", "month", "year", "animals", "status", "subscribe", "unsubscribe", "delete"),
				option(str, "time", "Briefing time HH:MM (with subscribe)", false),
			},
			Build: buildFortune,
		},
		{
			Name: "fortune-register", Desc: "Register your birthday for personal fortunes", Action: "fortune.subscribe",
			Ephemeral: true, Private: true, DiscordOnly: true,
			Options: []*discordgo.ApplicationCommandOption{
				option(str, "birth_date", "Date of birth, YYYY-MM-DD", true),
				option(str, "birth_time", "Time of birth HH:MM (optional)", false),
				choiceOption("gender", "Gender (optional)", false, "male", "female"),
				option(str, "birth_place", "Birthplace, a city (optional)", false),
			},
			Build: buildFortuneRegister,
		},
		{
			Name: "fortune-month", Desc: "This month's fortune", Action: "fortune.query", DiscordOnly: true, PrefixOnly: true,
			Build: buildFortuneMonth,
		},
		{
			Name: "fortune-year", Desc: "This year's fortune", Action: "fortune.query", DiscordOnly: true, PrefixOnly: true,
			Build: buildFortuneYear,
		},
		{
			Name: "subscribe", Desc: "Morning fortune briefing at a time", Action: "fortune.subscribe", DiscordOnly: true, PrefixOnly: true,
			Options: []*discordgo.ApplicationCommandOption{option(str, "time", "HH:MM", false)},
			Build:   buildSubscribe,
		},
		{
			Name: "zodiac", Desc: "Today's fortune of a zodiac sign, or all twelve ranked", Action: "fortune.query", DiscordOnly: true,
			Options: []*discordgo.ApplicationCommandOption{option(str, "sign", "Sign name, or ranking (empty = your own sign)", false)},
			Build:   buildZodiac,
		},
		{
			Name: "notices", Desc: "School notices picked for you; your notice settings", Action: "notices.status",
			Ephemeral: true, DiscordOnly: true,
			Options: []*discordgo.ApplicationCommandOption{
				choiceOption("what", "What to see or do", false, "latest", "profile", "status", "time", "pause", "resume", "delete", "mute", "unmute"),
				option(str, "value", "Time HH:MM, or a topic for mute/unmute", false),
			},
			Build: buildNotices,
		},
		{
			Name: "notices-register", Desc: "Register your school for notices picked for you", Action: "notices.subscribe",
			Ephemeral: true, Private: true, DiscordOnly: true,
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
			Build: buildNoticesRegister,
		},
		{
			Name: "transfer", Desc: "Recent transfer admission notices; your subscription to universities", Action: "transfer.recent",
			Ephemeral: true, DiscordOnly: true,
			Options: []*discordgo.ApplicationCommandOption{
				choiceOption("what", "What to see or do", false, "recent", "subscribe", "status", "time", "pause", "resume", "delete"),
				option(str, "value", "Universities (comma-separated) or a time HH:MM", false),
			},
			Build: buildTransfer,
		},
		{
			Name: "summary", Desc: "Summarize this channel's recent conversation", Action: "summaries.channel", DiscordOnly: true,
			Build: buildSummary,
		},
		{
			Name: "mysettings", Desc: "My settings: weather place, time zone, quiet hours, language", Action: "preferences.set",
			Ephemeral: true, Private: true, DiscordOnly: true,
			Options: []*discordgo.ApplicationCommandOption{
				option(str, "region", "Default place for weather (e.g. Seoul)", false),
				option(str, "timezone", "Time zone (e.g. Asia/Seoul)", false),
				option(str, "quiet", "Quiet hours for scheduled DMs, HH:MM-HH:MM (\"off\" clears)", false),
				choiceOption("language", "Language of my cards and DMs", false, "ko", "en", "default"),
			},
			Build: buildMysettings,
		},
		{
			Name: "updates", Desc: "What's new in this version; your update news by DM", Action: "updates.notes",
			Ephemeral: true, DiscordOnly: true,
			Options: []*discordgo.ApplicationCommandOption{
				choiceOption("what", "What to see or do", false, "now", "previous", "subscribe", "unsubscribe", "status"),
				option(str, "value", "A version, e.g. v1.4.0", false),
			},
			Build: buildUpdates,
		},
		{
			Name: "admin", Desc: "Run the bot in this channel: pause, replies, memory, chiming in, blocking (server managers)",
			Action: "access.policy", Ephemeral: true, DiscordOnly: true, ManagersOnly: true,
			Options: []*discordgo.ApplicationCommandOption{
				choiceOption("op", "What to do", false, "status", "pause", "resume", "respond", "memory", "chime", "news", "block", "unblock"),
				choiceOption("mode", "Setting (reply mode, memory on/off, chiming in)", false, "always", "mention", "command", "off", "on", "collect", "shadow", "conservative"),
				option(discordgo.ApplicationCommandOptionUser, "user", "Who to block or unblock in this server", false),
			},
			Build: buildAdmin,
		},
		{
			Name: "privacy", Desc: "See, give or withdraw consent for memory, fortune, notices and chiming in", Action: "privacy.consent",
			Ephemeral: true, Private: true, DiscordOnly: true,
			Options: []*discordgo.ApplicationCommandOption{
				choiceOption("action", "What to do", false, "status", "grant", "withdraw"),
				choiceOption("scope", "Which purpose", false, "memory", "fortune", "school_notice", "transfer_notice", "participation", "profile"),
			},
			Build: buildPrivacy,
		},
		{
			Name: "remind", Desc: "Remind me at a time (confirm the time and where it goes)", Action: "reminders.propose",
			Ephemeral: true, DiscordOnly: true,
			Options: []*discordgo.ApplicationCommandOption{
				option(str, "when", "When, as you would say it (e.g. tomorrow 3pm, in 30 minutes)", true),
				option(str, "what", "What to remind you of", false),
				choiceOption("repeat", "Repeat it (leave empty for once)", false, "daily", "weekly", "monthly"),
			},
			Build: buildRemind,
		},
		{
			Name: "reminders", Desc: "My reminders still to come, with cancel buttons", Action: "reminders.update",
			Ephemeral: true, Private: true, DiscordOnly: true,
			Build: buildReminders,
		},
		{
			// what the bot keeps that names the member, in this server only (memory.mine.show)
			Name: "mymemory", Desc: "What the bot remembers about you in this server", Action: "memory.mine.show",
			Ephemeral: true, Private: true, DiscordOnly: true,
			Build: buildMymemory,
		},
		{
			// a preview of the member's own memory with a confirm button (memory.mine.forget, buttons in memory_buttons.go)
			Name: "forgetme", Desc: "See, then delete, what the bot remembers about you in this server", Action: "memory.mine.forget",
			Ephemeral: true, Private: true, DiscordOnly: true,
			Build: buildForgetme,
		},
		{
			Name: "status", Desc: "Bot status", Action: "runtime.doctor", Ephemeral: true,
			Build: buildStatus,
		},
		{
			Name: "help", Desc: "List commands", Action: "help.version", Ephemeral: true,
			Build: buildHelp,
		},
	}
	for i := range cmds {
		cmds[i].Aliases = aliasesFor(cat, cmds[i].Name)
		build := cmds[i].Build
		cmds[i].Build = func(opts map[string]string, ctx InvokeContext) (string, map[string]any) {
			ctx.words = cat
			return build(opts, ctx)
		}
	}
	return cmds
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
func wordsFor(cat *i18n.Catalog, cmd, group string, values ...string) map[string]string {
	out := map[string]string{}
	for _, v := range values {
		out[v] = v
		for _, loc := range cat.Locales() {
			words, ok := cat.Lookup(loc, "discord.command."+cmd+"."+group+"."+v)
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
func localized(cat *i18n.Catalog, c Command) *discordgo.ApplicationCommand {
	base := "discord.command." + c.Name
	ac := &discordgo.ApplicationCommand{Name: c.Name, Description: c.Desc}
	if c.ManagersOnly {
		perms := int64(discordgo.PermissionManageGuild)
		ac.DefaultMemberPermissions = &perms
	}
	if l := localizations(cat, base+".name"); len(l) > 0 {
		ac.NameLocalizations = &l
	}
	if l := localizations(cat, base+".description"); len(l) > 0 {
		ac.DescriptionLocalizations = &l
	}
	for _, o := range c.Options {
		cp := *o
		ob := base + ".option." + o.Name
		if l := localizations(cat, ob+".name"); len(l) > 0 {
			cp.NameLocalizations = l
		}
		if l := localizations(cat, ob+".description"); len(l) > 0 {
			cp.DescriptionLocalizations = l
		}
		if len(o.Choices) > 0 {
			cp.Choices = nil
			for _, ch := range o.Choices {
				cc := *ch
				if l := localizations(cat, fmt.Sprintf("%s.choice.%s.%v", base, o.Name, ch.Value)); len(l) > 0 {
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
	return applicationCommandsIn(shippedCatalog(), nil)
}

// applicationCommandsIn renders only the commands whose Action allowed accepts (nil = all), so a tool
// switched off for commands is not offered as a slash command.
func applicationCommandsIn(cat *i18n.Catalog, allowed func(action string) bool) []*discordgo.ApplicationCommand {
	var out []*discordgo.ApplicationCommand
	for _, c := range commandsIn(cat) {
		if c.PrefixOnly || (allowed != nil && !allowed(c.Action)) {
			continue
		}
		out = append(out, localized(cat, c))
	}
	return append(out, rateCommand(cat))
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
func LocalName(cat *i18n.Catalog, c Command, locale string) string {
	if v, ok := cat.Lookup(locale, "discord.command."+c.Name+".name"); ok {
		return v
	}
	return c.Name
}

// LocalDesc is the command's description in locale.
func LocalDesc(cat *i18n.Catalog, c Command, locale string) string {
	if v, ok := cat.Lookup(locale, "discord.command."+c.Name+".description"); ok {
		return v
	}
	return c.Desc
}

// pendingServerAllows names the Actions a member's "!" command may run in a server waiting for approval: the
// same ones the application lets through there (application gateDiscord).
func pendingServerAllows(action string) bool {
	return action == "privacy.consent" || action == "memory.mine.show" || action == "memory.mine.forget"
}

// ByName / ByAlias resolve a Discord surface to a Command.
func ByName(cat *i18n.Catalog, name string) (Command, bool) {
	for _, c := range commandsIn(cat) {
		if c.Name == name {
			return c, true
		}
	}
	return Command{}, false
}

func ByAlias(cat *i18n.Catalog, alias string) (Command, bool) {
	return PrefixTable{cat: cat}.byAlias(alias)
}

// ToolCommand is the connector view of application.ToolCommand (prefix words per tool from config).
type ToolCommand struct {
	Tool       string
	Aliases    []string
	Custom     bool
	FirstParam string
}

// PrefixTable resolves "!word" with config overrides: configured aliases replace the default words of the
// first built-in command on that Action, and operator tools with aliases become commands whose text fills their
// first parameter. cat is the catalog the default words come from (nil: the built-in one).
type PrefixTable struct {
	cat       *i18n.Catalog
	overrides map[string][]string // action → aliases
	custom    []ToolCommand
}

// NewPrefixTable builds the table from the service's tool commands.
func NewPrefixTable(cat *i18n.Catalog, tc []ToolCommand) PrefixTable {
	t := PrefixTable{cat: cat, overrides: map[string][]string{}}
	for _, c := range tc {
		if c.Custom {
			t.custom = append(t.custom, c)
		} else if len(c.Aliases) > 0 {
			t.overrides[c.Tool] = c.Aliases
		}
	}
	return t
}

// catalog is the table's catalog; the zero value reads the built-in one.
func (t PrefixTable) catalog() *i18n.Catalog {
	if t.cat != nil {
		return t.cat
	}
	return shippedCatalog()
}

func (t PrefixTable) byAlias(alias string) (Command, bool) {
	alias = strings.ToLower(strings.TrimSpace(alias))
	// configured words are the tool's own: they replace the words of the first command on that Action only, so
	// the other commands on it (!이번달운세 and !별자리 on fortune.query) keep theirs
	overridden := map[string]bool{}
	for _, c := range commandsIn(t.catalog()) {
		words := c.Aliases
		if o, ok := t.overrides[c.Action]; ok && !overridden[c.Action] {
			words = o
		}
		overridden[c.Action] = true
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
func ParsePrefix(cat *i18n.Catalog, prefix, content string) (Command, map[string]string, bool) {
	return PrefixTable{cat: cat}.Parse(prefix, content)
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
		if cmd.Name == "fx" {
			fxPrefixOptions(t.catalog(), tail, opts)
		} else if cmd.Name == "poll" {
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

// fxPrefixOptions reads "!환율 50000 엔화", "!환율 엔화 50000" and "!환율 달러 유로": the first number is the
// amount; the rest is a currency (a code or a name that may hold spaces), or two of them, the currency and
// the base to value it in. Words are split into two names only when both resolve, so "미국 달러" stays one
// name; when nothing resolves they stay one currency, which the action refuses as it would any other.
func fxPrefixOptions(cat *i18n.Catalog, tail string, opts map[string]string) {
	var words []string
	for _, w := range strings.Fields(tail) {
		if _, taken := opts["amount"]; !taken && currency.IsNumber(w) {
			opts["amount"] = w
			continue
		}
		words = append(words, w)
	}
	if len(words) == 0 {
		return
	}
	opts["currency"] = strings.Join(words, " ")
	lex := currency.New(cat)
	if _, ok := lex.Resolve(opts["currency"]); ok {
		return
	}
	// a side longer than the longest admissible name cannot resolve, so only the splits inside it are
	// tried (a long unknown input stays one currency without scanning them all)
	bound := lex.MaxNameWords()
	lo, hi := len(words)-bound, bound
	if lo < 1 {
		lo = 1
	}
	if hi > len(words)-1 {
		hi = len(words) - 1
	}
	for i := lo; i <= hi; i++ {
		first, second := strings.Join(words[:i], " "), strings.Join(words[i:], " ")
		_, ok1 := lex.Resolve(first)
		_, ok2 := lex.Resolve(second)
		if ok1 && ok2 {
			opts["currency"], opts["base"] = first, second
			return
		}
	}
}

// requestFor builds the Action request for a command invocation. The invoker lets the tool layer
// apply per-channel switches and per-user limits.
func requestFor(cmd Command, opts map[string]string, ctx InvokeContext, requestID string) action.Request {
	act, input := cmd.Build(opts, ctx)
	raw, _ := json.Marshal(input)
	return action.Request{RequestID: requestID, Action: act, Input: raw, Locale: ctx.Locale,
		Invoker: &action.Invoker{Via: "command", GuildID: ctx.GuildID, ChannelID: ctx.ChannelID, UserID: ctx.UserID, Manager: ctx.Manager, Private: ctx.Private, Name: ctx.UserName}}
}
