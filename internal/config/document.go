package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"masamong/internal/lang"
	"masamong/internal/providers/openai"
	"masamong/internal/providers/websearch"
	"masamong/internal/secrets"
	"masamong/internal/tools"
)

// DefaultEmbedDimensions is the default storage width for known Matryoshka embedding models.
const DefaultEmbedDimensions = 1024

// Document is the declarative instance config (06장). Operational job state is not stored here.
type Document struct {
	SchemaVersion int                 `yaml:"schema_version" json:"schema_version"`
	Instance      Instance            `yaml:"instance" json:"instance"`
	Admin         Admin               `yaml:"admin" json:"admin"`
	Discord       Discord             `yaml:"discord" json:"discord"`
	I18n          I18n                `yaml:"i18n" json:"i18n"`
	Providers     map[string]Provider `yaml:"providers" json:"providers"`
	Models        map[string]Model    `yaml:"models" json:"models"`
	Limits        Limits              `yaml:"limits" json:"limits"`
	Memory        MemoryFlags         `yaml:"memory" json:"memory"`
	Participation Participation       `yaml:"participation" json:"participation"`
	Features      map[string]bool     `yaml:"features" json:"features"`
	Backup        Backup              `yaml:"backup" json:"backup"`
	// Retention bounds the operational records that grow with every reply (not memory itself).
	Retention Retention `yaml:"retention,omitempty" json:"retention,omitempty"`
	// Tools configures the bot's tools: built-in Actions (enable, chat, commands, aliases, limits,
	// defaults, planner text) and operator-defined HTTP tools. Absent entries keep built-in defaults.
	Tools map[string]tools.Settings `yaml:"tools,omitempty" json:"tools,omitempty"`
	// Guilds holds per-Discord-server channel policy. Keys are guild IDs; nested
	// channel keys are Discord channel IDs. Policies never auto-merge rooms or
	// share RAG attachments across channels (RAW_DATA_POLICY).
	Guilds   map[string]GuildSettings `yaml:"guilds,omitempty" json:"guilds,omitempty"`
	Personas map[string]Persona       `yaml:"personas,omitempty" json:"personas,omitempty"`
	// Access is who may do more than talk to the bot from Discord, and who it ignores (19장 A37).
	Access Access `yaml:"access,omitempty" json:"access,omitempty"`
	// Alerts sends the operators a Discord direct message when something needs them.
	Alerts Alerts `yaml:"alerts,omitempty" json:"alerts,omitempty"`
	// Updates says whether the service looks for a newer release once a day.
	Updates Updates `yaml:"updates,omitempty" json:"updates,omitempty"`
	// WebSearch is how search.web picks among the search services that have keys.
	WebSearch WebSearch `yaml:"web_search,omitempty" json:"web_search,omitempty"`
	// DisasterAlerts announces earthquakes and weather warnings in the channels the bot answers in.
	DisasterAlerts DisasterAlerts `yaml:"disaster_alerts,omitempty" json:"disaster_alerts,omitempty"`
	// Chat is how a reply turn works (instance-wide).
	Chat ChatSettings `yaml:"chat,omitempty" json:"chat,omitempty"`
	DM   DMSettings   `yaml:"dm,omitempty" json:"dm,omitempty"`
}

// DMSettings controls conversation in direct messages. Commands keep their own policy.
type DMSettings struct {
	Allow         string `yaml:"allow,omitempty" json:"allow,omitempty"`
	PerUserPerDay int    `yaml:"per_user_per_day,omitempty" json:"per_user_per_day,omitempty"`
	PerDay        int    `yaml:"per_day,omitempty" json:"per_day,omitempty"`
}

var DMAllowModes = []string{"members", "operators", "off"}

func (d DMSettings) AllowMode() string {
	if d.Allow == "" {
		return "members"
	}
	return d.Allow
}

func (d DMSettings) UserDaily() int {
	if d.PerUserPerDay == 0 {
		return 30
	}
	return d.PerUserPerDay
}

func (d DMSettings) Daily() int {
	if d.PerDay == 0 {
		return 100
	}
	return d.PerDay
}

// DisasterAlerts is what the bot announces on its own from the Korea Meteorological Administration
// (it needs a KMA key): earthquakes, checked every minute as the previous bot did, and weather
// warnings, checked every five minutes. A channel turns them off with disaster_alerts: off.
type DisasterAlerts struct {
	// Enabled turns announcing on or off (unset = on; nothing is sent without a KMA key).
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// EarthquakeMinDomestic / EarthquakeMinOverseas are the smallest magnitudes announced (defaults 4.0
	// and 7.0, the previous bot's). Notices KMA marks 국내영향없음 are never announced.
	EarthquakeMinDomestic float64 `yaml:"earthquake_min_domestic,omitempty" json:"earthquake_min_domestic,omitempty"`
	EarthquakeMinOverseas float64 `yaml:"earthquake_min_overseas,omitempty" json:"earthquake_min_overseas,omitempty"`
	// Warnings turns weather warnings on or off (unset = on). 경보 is announced for every land region;
	// 주의보 only for regions whose name contains one of WarningRegions (e.g. 서울, 경기).
	Warnings       *bool    `yaml:"warnings,omitempty" json:"warnings,omitempty"`
	WarningRegions []string `yaml:"warning_regions,omitempty" json:"warning_regions,omitempty"`
	// Marine adds sea-area warnings (풍랑 on 먼바다 …), left out by default.
	Marine bool `yaml:"marine,omitempty" json:"marine,omitempty"`
}

// On reports whether announcing is on.
func (d DisasterAlerts) On() bool { return d.Enabled == nil || *d.Enabled }

// WarningsOn reports whether weather warnings are announced.
func (d DisasterAlerts) WarningsOn() bool { return d.On() && (d.Warnings == nil || *d.Warnings) }

// MinMagnitude is the smallest magnitude announced for a domestic or overseas earthquake.
func (d DisasterAlerts) MinMagnitude(domestic bool) float64 {
	if domestic {
		if d.EarthquakeMinDomestic > 0 {
			return d.EarthquakeMinDomestic
		}
		return 4.0
	}
	if d.EarthquakeMinOverseas > 0 {
		return d.EarthquakeMinOverseas
	}
	return 7.0
}

// WebSearch orders the search services. A search that fails or finds nothing moves to the next one.
type WebSearch struct {
	// Order names search providers (config providers of a search kind) first to last; empty means
	// every one with a key, in the built-in order: Tavily, Linkup, Exa, Serper, Kakao.
	Order []string `yaml:"order,omitempty" json:"order,omitempty"`
	// ReadPages is how many result pages a chat search reads for passages (standard depth; unset = 3,
	// 0 = snippets only). DeepPages is the same for a deep search (unset = 6). Quick reads none.
	ReadPages *int `yaml:"read_pages,omitempty" json:"read_pages,omitempty"`
	DeepPages *int `yaml:"deep_pages,omitempty" json:"deep_pages,omitempty"`
	// Fetch is the service that reads those pages: "tinyfish", "jina" or "off" (unset = tinyfish when a
	// TinyFish key is configured, else jina).
	Fetch string `yaml:"fetch,omitempty" json:"fetch,omitempty"`
	// FetchDailyLimit caps the pages read a day (unset or 0 = 800; TinyFish Fetch is free to 1,000 a day).
	FetchDailyLimit int `yaml:"fetch_daily_limit,omitempty" json:"fetch_daily_limit,omitempty"`
}

// FetchKinds are the values web_search.fetch accepts besides empty (auto).
var FetchKinds = []string{"tinyfish", "jina", "off"}

// Pages is how many result pages a chat search of this depth reads ("" is standard).
func (w WebSearch) Pages(depth string) int {
	switch depth {
	case "quick":
		return 0
	case "deep":
		if w.DeepPages != nil {
			return max(0, min(*w.DeepPages, 10))
		}
		return 6
	}
	if w.ReadPages != nil {
		return max(0, min(*w.ReadPages, 10))
	}
	return 3
}

// FetchKind is the effective page reader: tinyfish, jina or off.
func (w WebSearch) FetchKind(hasTinyFish bool) string {
	switch w.Fetch {
	case "tinyfish", "jina", "off":
		return w.Fetch
	}
	if hasTinyFish {
		return "tinyfish"
	}
	return "jina"
}

// FetchDailyPages is the effective daily cap of pages read.
func (w WebSearch) FetchDailyPages() int {
	if w.FetchDailyLimit > 0 {
		return w.FetchDailyLimit
	}
	return 800
}

// Updates controls the daily release check. It only reports a newer version; it installs nothing.
type Updates struct {
	// Check asks GitHub for the newest release once a day (unset = on). The request carries only the
	// program's name and version.
	Check *bool `yaml:"check,omitempty" json:"check,omitempty"`
}

// CheckOn reports whether the daily release check runs.
func (u Updates) CheckOn() bool { return u.Check == nil || *u.Check }

// Alerts sends new "needs attention" items (a failed backup, a rejected key, the budget running out,
// the service restarting after a crash) to every access.operators member's Discord direct messages.
type Alerts struct {
	// Discord turns the direct messages on or off (unset = on; nothing is sent without operators).
	Discord *bool `yaml:"discord,omitempty" json:"discord,omitempty"`
	// MinSeverity is the least serious item sent: warn (default) or danger.
	MinSeverity string `yaml:"min_severity,omitempty" json:"min_severity,omitempty"`
}

// AlertSeverities are the accepted alerts.min_severity values, least serious first.
var AlertSeverities = []string{"warn", "danger"}

// DiscordOn reports whether alerts go to the operators' direct messages.
func (a Alerts) DiscordOn() bool { return a.Discord == nil || *a.Discord }

// Least is the least serious severity sent.
func (a Alerts) Least() string {
	if a.MinSeverity == "" {
		return "warn"
	}
	return a.MinSeverity
}

// Access is the Discord-side access policy. The console itself is only reachable by the installation's
// owner (loopback + SSH), so this governs Discord only.
type Access struct {
	// Operators are Discord user IDs that may use the admin command (pause, resume, reply mode) in any
	// server the bot is in.
	Operators []string `yaml:"operators,omitempty" json:"operators,omitempty"`
	// GuildAdmins lets a server's owner and members with Manage Server use the admin command in that
	// server (unset = yes).
	GuildAdmins *bool `yaml:"guild_admins,omitempty" json:"guild_admins,omitempty"`
	// BlockedUsers are Discord user IDs the bot ignores: no replies, commands or buttons. Their messages
	// in capturing channels are still stored as part of the conversation unless they withdraw consent.
	BlockedUsers []string `yaml:"blocked_users,omitempty" json:"blocked_users,omitempty"`
	// ServerApproval makes the bot work only in approved servers: those listed in Servers and those
	// with settings under guilds (unset = on). A server someone newly invites the bot to waits for an
	// operator; until then the bot stores nothing there and answers nothing but a short notice.
	ServerApproval *bool `yaml:"server_approval,omitempty" json:"server_approval,omitempty"`
	// Servers are Discord server IDs an operator approved.
	Servers []string `yaml:"servers,omitempty" json:"servers,omitempty"`
}

// GuildAdminsAllowed reports whether server managers may use the admin command.
func (a Access) GuildAdminsAllowed() bool { return a.GuildAdmins == nil || *a.GuildAdmins }

// ApprovalRequired reports whether only approved servers are served.
func (a Access) ApprovalRequired() bool { return a.ServerApproval == nil || *a.ServerApproval }

// GuildSettings is declarative policy for one Discord guild.
type GuildSettings struct {
	Locale   string                   `yaml:"locale,omitempty" json:"locale,omitempty"`
	Channels map[string]ChannelPolicy `yaml:"channels,omitempty" json:"channels,omitempty"`
	// BlockedUsers are ignored in this server only (set by its managers with the admin command, or
	// in the console); access.blocked_users applies everywhere.
	BlockedUsers []string `yaml:"blocked_users,omitempty" json:"blocked_users,omitempty"`
	// Privacy are the stricter memory options of this server, all off by default.
	Privacy GuildPrivacy `yaml:"privacy,omitempty" json:"privacy,omitempty"`
	// Log is this server's log channel: deleted messages and problems in this server (serverlog.go).
	Log GuildLog `yaml:"log,omitempty" json:"log,omitempty"`
}

// GuildLog is where Masamong posts what happened in a server that its managers may want to see, as the
// previous Masamong did in a channel named "logs": a deleted message (who wrote it, where, when and
// what it said) and a problem in this server (a reply or a scheduled post that failed, a tool that
// failed in a reply, a missing permission).
type GuildLog struct {
	// Channel is the log channel's ID, or "auto" for the server's text channel named "logs" when there
	// is one (what the previous Masamong did). Unset (or "off") = no log: the log is opt-in, so a
	// server's existing #logs channel never starts receiving deleted messages unasked.
	Channel string `yaml:"channel,omitempty" json:"channel,omitempty"`
	// Deletes and Problems pick what is posted (unset = on). DeletedText shows a deleted message's
	// text in its entry (unset = on); off posts only who, where and when.
	Deletes     *bool `yaml:"deletes,omitempty" json:"deletes,omitempty"`
	DeletedText *bool `yaml:"deleted_text,omitempty" json:"deleted_text,omitempty"`
	Problems    *bool `yaml:"problems,omitempty" json:"problems,omitempty"`
}

// routeSlug is a model family or an OpenRouter provider slug (anthropic, google-vertex, deepinfra/turbo).
var routeSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*(/[a-z0-9._-]+)?$`)

// GuildLogAuto is the GuildLog.Channel value that looks for the channel named GuildLogName.
const (
	GuildLogAuto = "auto"
	GuildLogName = "logs"
)

// DeletesOn and ProblemsOn report what the log takes (unset = on).
func (l GuildLog) DeletesOn() bool  { return l.Deletes == nil || *l.Deletes }
func (l GuildLog) TextOn() bool     { return l.DeletedText == nil || *l.DeletedText }
func (l GuildLog) ProblemsOn() bool { return l.Problems == nil || *l.Problems }

// isSnowflake reports whether s looks like a Discord ID (digits only).
func isSnowflake(s string) bool {
	if len(s) < 5 || len(s) > 25 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// GuildPrivacy are stricter memory options a server can turn on (docs/plan-memory-conversation.md
// §5.4.8). They are off by default: the bot is for friends' servers, where remembering freely is the
// point. What always holds whatever they say (servers never share people, deletion and withdrawal
// reach derived memory, no bodies or IDs in logs) is not an option.
type GuildPrivacy struct {
	// OwnCardOnly: a reply uses only the card of the person who called the bot, not those of the
	// other people in the conversation.
	OwnCardOnly bool `yaml:"own_card_only,omitempty" json:"own_card_only,omitempty"`
	// NoHearsay: what one person said about another is left out of the room memory a reply sees.
	NoHearsay bool `yaml:"no_hearsay,omitempty" json:"no_hearsay,omitempty"`
	// NoSensitive: facts a classifier marks sensitive (health, romance, money, religion, politics…) are
	// not kept by "remember this" and do not become card items.
	NoSensitive bool `yaml:"no_sensitive,omitempty" json:"no_sensitive,omitempty"`
	// CardChannelOnly: a card item is used only in the channel (and its threads) its evidence came from.
	CardChannelOnly bool `yaml:"card_channel_only,omitempty" json:"card_channel_only,omitempty"`
}

// ChannelPolicy is isolated to one source channel. Capture stores this channel's messages as memory;
// MemoryUse lets replies search memory (the channel's own room plus attached blocks only).
type ChannelPolicy struct {
	Respond    string         `yaml:"respond,omitempty" json:"respond,omitempty"` // command|mention|always|off
	PersonaRef string         `yaml:"persona_ref,omitempty" json:"persona_ref,omitempty"`
	Rules      string         `yaml:"rules,omitempty" json:"rules,omitempty"` // extra reply rules for this channel
	Capture    bool           `yaml:"capture,omitempty" json:"capture,omitempty"`
	Logging    ChannelLogging `yaml:"logging,omitempty" json:"logging,omitempty"`
	MemoryUse  string         `yaml:"memory_use,omitempty" json:"memory_use,omitempty"` // on | off
	Proactive  bool           `yaml:"proactive,omitempty" json:"proactive,omitempty"`   // superseded by Participation; always false
	// Participation is whether the bot may join this channel's conversation on its own (12), an
	// experimental feature: off, collect (records candidate moments and what followed, no model call),
	// shadow (decides and records what it would say, says nothing) or conservative (says it, within the
	// limits under participation). The instance switch participation.enabled/mode caps it.
	Participation string     `yaml:"participation,omitempty" json:"participation,omitempty"`
	RAGView       MemoryView `yaml:"memory_view,omitempty" json:"memory_view,omitempty"`
	// Tools switches tools per channel: false = off here (chat and commands); true = offered to the
	// planner here even when the tool's chat default is off. A tool disabled instance-wide stays off.
	Tools          map[string]bool    `yaml:"tools,omitempty" json:"tools,omitempty"`
	RAGAttachments []RAGAttachmentRef `yaml:"rag_attachments,omitempty" json:"rag_attachments,omitempty"`
	// DisasterAlerts "off" keeps earthquake and weather-warning announcements out of this channel
	// (unset = on wherever the bot answers).
	DisasterAlerts string `yaml:"disaster_alerts,omitempty" json:"disaster_alerts,omitempty"`
	// NameCall is what a message that only says the bot's name (no mention, no reply to it) means:
	// addressed = a small classifier tells "talking to the bot" from "talking about the bot" (default);
	// always = every message with the name is a call (the old behaviour); off = the name alone never calls.
	NameCall string `yaml:"name_call,omitempty" json:"name_call,omitempty"`
	// Attribution decides which recent lines a reply sees: on = only the caller's conversation, plus
	// lines a classifier finds the call pointing at (default); structural = the caller's conversation
	// only, no classifier; off = every recent line, as before (docs/plan-memory-conversation.md §5.1).
	Attribution string `yaml:"attribution,omitempty" json:"attribution,omitempty"`
	// Conversation holds the follow-up window and thread settings.
	Conversation ChannelConversation `yaml:"conversation,omitempty" json:"conversation,omitempty"`
}

// ChannelConversation is how a conversation with the bot may continue in a channel.
type ChannelConversation struct {
	// Follow lets the person the bot just answered carry on without tagging it: off (default, an
	// experimental feature), reply_only (only replies to the bot's message) or window (the scored
	// follow-up window). Decisions are recorded either way, to tune the defaults from real use.
	Follow string `yaml:"follow,omitempty" json:"follow,omitempty"`
	// FollowWindowSec is how long the window stays open after a reply (0 = 90; a question from the bot
	// adds 60).
	FollowWindowSec int `yaml:"follow_window_sec,omitempty" json:"follow_window_sec,omitempty"`
	// FollowMaxUntagged caps untagged replies in a row before the next one needs a tag (0 = 12).
	FollowMaxUntagged int `yaml:"follow_max_untagged,omitempty" json:"follow_max_untagged,omitempty"`
	// Thread: offer (a button under a long one-on-one exchange, default), auto_task (also opens one by
	// itself for a multi-step task) or off.
	Thread string `yaml:"thread,omitempty" json:"thread,omitempty"`
	// ThreadFollow is who the bot answers without a tag inside a thread it opened: participants (the
	// people it was talking with; lines aimed at someone else are left alone, default) or all.
	ThreadFollow string `yaml:"thread_follow,omitempty" json:"thread_follow,omitempty"`
	// ThreadIdleCloseMin ends the bot's part in a quiet thread (0 = 30).
	ThreadIdleCloseMin int `yaml:"thread_idle_close_min,omitempty" json:"thread_idle_close_min,omitempty"`
	// ThreadRecap posts a short recap in the parent channel when a thread ends: plan (only when a plan
	// or decision came out of it, default), always or off.
	ThreadRecap string `yaml:"thread_recap,omitempty" json:"thread_recap,omitempty"`
	// ReplyShape is how a reply is sent: bubbles (two or three short paragraphs go out as separate
	// messages a moment apart, like a person typing, and a reply far longer than a chat line shows its
	// first part with a "more" button; default), trim (only the "more" button) or plain (as written).
	ReplyShape string `yaml:"reply_shape,omitempty" json:"reply_shape,omitempty"`
	// Banter answers someone who talks about the bot in fun ("the bot is broken again lol") with one
	// light line or an emoji: off (default, an experimental feature), shadow (only record what it would
	// have done) or on. Never for a real complaint; a few times an hour at most.
	Banter string `yaml:"banter,omitempty" json:"banter,omitempty"`
	// RoomStyle adds the room's approved style note (how the room talks: length, laughs, slang) after
	// the persona: off (default, an experimental feature) or on. A note is used only after an operator
	// approved it, and stops being used when a message it came from is deleted.
	RoomStyle string `yaml:"room_style,omitempty" json:"room_style,omitempty"`
}

// Choices of the conversation settings (the first is the default).
var (
	NameCallModes     = []string{"addressed", "always", "off"}
	AttributionModes  = []string{"on", "structural", "off"}
	FollowModes       = []string{"off", "reply_only", "window"}
	ThreadModes       = []string{"offer", "auto_task", "off"}
	ThreadFollowModes = []string{"participants", "all"}
	ThreadRecapModes  = []string{"plan", "always", "off"}
	ReplyShapes       = []string{"bubbles", "trim", "plain"}
	BanterModes       = []string{"off", "shadow", "on"}
	RoomStyleModes    = []string{"off", "on"}
)

// FollowWindow is the follow-up window length with its default.
func (c ChannelConversation) FollowWindow() time.Duration {
	if c.FollowWindowSec <= 0 {
		return 90 * time.Second
	}
	return time.Duration(min(c.FollowWindowSec, 600)) * time.Second
}

// MaxUntagged is FollowMaxUntagged with its default.
func (c ChannelConversation) MaxUntagged() int {
	if c.FollowMaxUntagged <= 0 {
		return 12
	}
	return c.FollowMaxUntagged
}

// ThreadIdle is ThreadIdleCloseMin with its default.
func (c ChannelConversation) ThreadIdle() time.Duration {
	if c.ThreadIdleCloseMin <= 0 {
		return 30 * time.Minute
	}
	return time.Duration(c.ThreadIdleCloseMin) * time.Minute
}

// MemoryView is the revisioned set of memory blocks bound to one channel.
// Attach/detach must not copy or rewrite block artifacts.
type MemoryView struct {
	Blocks           []string `yaml:"blocks,omitempty" json:"blocks,omitempty"`
	Revision         int      `yaml:"revision,omitempty" json:"revision,omitempty"`
	ScopeFingerprint string   `yaml:"scope_fingerprint,omitempty" json:"scope_fingerprint,omitempty"`
	UpdatedAt        string   `yaml:"updated_at,omitempty" json:"updated_at,omitempty"`
}

// ChannelLogging records operator-facing ops verbosity. It must not store message bodies or user IDs.
type ChannelLogging struct {
	Level      string `yaml:"level,omitempty" json:"level,omitempty"` // off|errors|ops
	RetainDays int    `yaml:"retain_days,omitempty" json:"retain_days,omitempty"`
}

// RAGAttachmentRef is a channel-scoped staged file for a future retrieval importer.
// Binary content lives under the instance data dir; config never embeds file bodies.
type RAGAttachmentRef struct {
	ID          string `yaml:"id" json:"id"`
	ContentHash string `yaml:"content_hash" json:"content_hash"`
	Filename    string `yaml:"filename" json:"filename"`
	Mime        string `yaml:"mime,omitempty" json:"mime,omitempty"`
	Bytes       int64  `yaml:"bytes,omitempty" json:"bytes,omitempty"`
	Purpose     string `yaml:"purpose,omitempty" json:"purpose,omitempty"` // retrieval_context
	StoredName  string `yaml:"stored_name,omitempty" json:"stored_name,omitempty"`
}

// Persona is a reply style profile. It cannot override memory scope.
type Persona struct {
	Locale  string `yaml:"locale,omitempty" json:"locale,omitempty"`
	Label   string `yaml:"label,omitempty" json:"label,omitempty"`
	Builtin bool   `yaml:"builtin,omitempty" json:"builtin,omitempty"`
	// Hidden keeps a built-in out of the console's lists without deleting it: operators can hide a
	// default they do not want and restore it later.
	Hidden bool   `yaml:"hidden,omitempty" json:"hidden,omitempty"`
	Style  string `yaml:"style,omitempty" json:"style,omitempty"`
}

var (
	RespondModes  = []string{"command", "mention", "always", "off"}
	MemoryUses    = []string{"on", "off"}
	LoggingLevels = []string{"off", "errors", "ops"}
	// ModelRoles is the shared model-role vocabulary (contracts/sandbox/tool-parity.json models.roles).
	// The test sandbox uses the same names in config.json `models`, so a model chosen here means the
	// same thing there: response = answers, planner = tool/intent planning, summary = leaf/session
	// summaries, embed = retrieval vectors, rerank = R2 semantic rerank.
	ModelRoles = []string{"response", "planner", "summary", "embed", "rerank"}
	// ProductModelRoles are roles only the product has (the test sandbox has no image generation):
	// image = the images.generate picture model; vision = the model that describes pictures a member
	// sends (optional: unset keeps the planner-then-response fallback, see internal/application/vision.go).
	ProductModelRoles = []string{"image", "vision"}
)

// OptionalModelRoles are the roles that may be empty: image (image generation answers "not configured")
// and vision (the planner, then the response model read pictures). The console offers to clear them.
var OptionalModelRoles = []string{"image", "vision"}

// AllModelRoles is every role config accepts.
func AllModelRoles() []string { return append(append([]string{}, ModelRoles...), ProductModelRoles...) }

// ValidModelRole reports whether role is one of ModelRoles or ProductModelRoles.
func ValidModelRole(role string) bool {
	return slices.Contains(ModelRoles, role) || slices.Contains(ProductModelRoles, role)
}

// BuiltinPersonas are always available even when Document.Personas is empty. Every language has a
// friendly_<tag> persona, which is the default for rooms of that language unless the language has a
// warm_<tag> one (a polite-but-warm register for languages where a bot talking down to strangers is
// rude: Korean and Japanese); a new language only needs its pack plus a friendly_<tag> built-in here
// to get a default persona. IDs are referenced by config, so a built-in is never renamed.
func BuiltinPersonas() map[string]Persona {
	return map[string]Persona{
		// Style may use {bot} (the first instance.bot_names entry).
		"warm_ko":     {Locale: "ko", Label: "Warm Korean (polite)", Builtin: true, Style: "너는 Discord 봇 {bot}이다. 이 방 사람들과 오래 어울린 친구처럼, 다정하고 편안한 존댓말(해요체)로 짧게 말한다. 반말과 딱딱한 공문체는 쓰지 않는다."},
		"friendly_ko": {Locale: "ko", Label: "Friendly Korean", Builtin: true, Style: "너는 Discord 봇 {bot}이다. 이 방 사람들과 오래 어울린 친구처럼 반말로 짧고 편하게 말한다."},
		// v2 personas describe behaviour, not traits: a list of adjectives does not change how a model
		// writes, a rule about length and form does (docs/plan-memory-conversation.md §5.6). Rooms without
		// a persona use them; rooms that picked a v1 persona keep it.
		"friendly_ko_v2":      {Locale: "ko", Label: "Friendly Korean v2", Builtin: true, Style: "너는 Discord 봇 {bot}이다. 이 방 친구들과 오래 어울린 친구로서 반말로 말한다.\n- 길이: 한 줄로 물으면 한 줄로 답한다. 길어도 세 줄 안에 끝낸다. 정보가 많이 필요할 때만 더 쓴다.\n- 하지 않는 것: 상담원 말투(\"도움이 되셨길 바랍니다\"), 질문 되풀이, 잡담에 제목·굵은 글씨·번호 목록, 매번 이모지.\n- 친구들이 놀리면 같이 가볍게 받아친다. 외모·가족·건강·돈 같은 약점은 건드리지 않는다.\n- 틀리면 쿨하게 인정한다. 모르면 지어내지 말고 \"찾아볼까?\"라고 묻는다.\n- 따로 보내고 싶은 말은 줄을 바꿔 쓴다(최대 세 덩어리)."},
		"warm_ko_v2":          {Locale: "ko", Label: "Warm Korean v2 (polite)", Builtin: true, Style: "너는 Discord 봇 {bot}이다. 이 방 사람들과 오래 어울린 친구처럼 다정한 해요체로 말한다. 반말과 공문체는 쓰지 않는다.\n- 길이: 한 줄로 물으면 한 줄로 답한다. 길어도 세 줄 안에 끝낸다. 정보가 많이 필요할 때만 더 쓴다.\n- 하지 않는 것: 상담원 말투(\"도움이 되셨길 바랍니다\"), 질문 되풀이, 잡담에 제목·굵은 글씨·번호 목록, 매번 이모지.\n- 장난에는 가볍게 맞장구친다. 외모·가족·건강·돈 같은 약점은 건드리지 않는다.\n- 틀리면 솔직하게 인정한다. 모르면 지어내지 말고 \"찾아볼까요?\"라고 묻는다.\n- 따로 보내고 싶은 말은 줄을 바꿔 쓴다(최대 세 덩어리)."},
		"friendly_en_v2":      {Locale: "en", Label: "Friendly English v2", Builtin: true, Style: "You are {bot}, a Discord bot who has hung out with the people in this room for a long time. Talk like a friend.\n- Length: a one-line question gets a one-line answer. Three lines at most unless real information needs more.\n- Don't: sound like customer support (\"I hope this helps!\"), repeat the question, use headings, bold or numbered lists in small talk, or add an emoji every time.\n- If friends tease you, tease back lightly. Never touch looks, family, health or money.\n- When you're wrong, own it casually. When you don't know, don't make it up: ask \"want me to look it up?\"\n- Put things you'd send as separate messages on separate lines (three at most)."},
		"friendly_ja_v2":      {Locale: "ja", Label: "Friendly Japanese v2", Builtin: true, Style: "あなたはDiscordボットの{bot}。この部屋の人たちと長く付き合ってきた友達として、タメ口で話す。\n- 長さ：一行の質問には一行で答える。長くても三行まで。情報が本当に必要なときだけ長くする。\n- しないこと：サポート窓口の口調、質問の繰り返し、雑談での見出し・太字・番号付きリスト、毎回の絵文字。\n- いじられたら軽く返す。見た目・家族・健康・お金には触れない。\n- 間違えたらさらっと認める。知らないことは作らず「調べようか？」と聞く。\n- 別々に送りたいことは改行して書く（三つまで）。"},
		"warm_ja_v2":          {Locale: "ja", Label: "Warm Japanese v2 (polite)", Builtin: true, Style: "あなたはDiscordボットの{bot}です。この部屋の人たちと長く付き合ってきた友達のように、柔らかい丁寧語で話します。\n- 長さ：一行の質問には一行で答えます。長くても三行まで。情報が本当に必要なときだけ長くします。\n- しないこと：サポート窓口の口調、質問の繰り返し、雑談での見出し・太字・番号付きリスト、毎回の絵文字。\n- 冗談には軽く乗ります。見た目・家族・健康・お金には触れません。\n- 間違えたら素直に認めます。知らないことは作らず「調べましょうか？」と聞きます。\n- 別々に送りたいことは改行して書きます（三つまで）。"},
		"friendly_zh_hans_v2": {Locale: "zh-Hans", Label: "Friendly Chinese (Simplified) v2", Builtin: true, Style: "你是 Discord 机器人 {bot}，和这个房间的人相处很久的朋友，用轻松的口语聊天。\n- 长度：一句话的问题就用一句话回答，最多三行；只有真正需要信息时才写更多。\n- 不要：客服腔、重复问题、闲聊时用标题、粗体或编号列表、每次都加表情。\n- 被朋友调侃就轻松回嘴，绝不碰外貌、家人、健康或钱。\n- 说错了就大方承认；不知道就别编，问一句“要我查一下吗？”\n- 想分开发的话就换行写（最多三段）。"},
		"friendly_es_v2":      {Locale: "es", Label: "Friendly Spanish v2", Builtin: true, Style: "Eres {bot}, un bot de Discord que lleva mucho tiempo con la gente de esta sala. Habla como un amigo.\n- Longitud: a una pregunta de una línea, una línea. Tres líneas como máximo salvo que la información lo pida.\n- No: tono de atención al cliente, repetir la pregunta, títulos, negritas o listas numeradas en una charla, ni un emoji cada vez.\n- Si te toman el pelo, responde con humor ligero. Nunca toques el aspecto, la familia, la salud o el dinero.\n- Si te equivocas, admítelo sin drama. Si no sabes, no inventes: pregunta \"¿lo busco?\"\n- Lo que mandarías como mensajes aparte, escríbelo en líneas aparte (tres como máximo)."},
		"friendly_en":         {Locale: "en", Label: "Friendly English", Builtin: true, Style: "You are {bot}, a Discord bot. Talk casually and briefly, like a friend who has hung out with the people in this room for a long time."},
		"neutral_en":          {Locale: "en", Label: "Neutral English", Builtin: true, Style: "plain, calm English replies"},
		"polite_ko":           {Locale: "ko", Label: "Polite Korean", Builtin: true, Style: "너는 Discord 봇 {bot}이다. 항상 존댓말(해요체)로 친절하고 정확하게 답한다. 필요한 경우에만 짧은 목록을 쓴다."},
		"brief_ko":            {Locale: "ko", Label: "Brief Korean", Builtin: true, Style: "너는 Discord 봇 {bot}이다. 인사말이나 군더더기 없이 핵심만 한두 문장으로 답한다."},
		"cheer_ko":            {Locale: "ko", Label: "Cheerful Korean", Builtin: true, Style: "너는 Discord 봇 {bot}이다. 따뜻하고 다정한 말투로, 상대의 기분을 먼저 살피고 응원해 준다. 과장된 칭찬은 하지 않는다."},
		"polite_en":           {Locale: "en", Label: "Polite English", Builtin: true, Style: "You are {bot}, a Discord bot. Always answer politely and accurately. Use a short list only when it helps."},
		"brief_en":            {Locale: "en", Label: "Brief English", Builtin: true, Style: "You are {bot}, a Discord bot. Answer with only the point, in one or two sentences, without greetings or filler."},
		"cheer_en":            {Locale: "en", Label: "Cheerful English", Builtin: true, Style: "You are {bot}, a Discord bot. Speak warmly, notice how people feel and encourage them, without overdoing the praise."},
		"warm_ja":             {Locale: "ja", Label: "Warm Japanese (polite)", Builtin: true, Style: "あなたはDiscordボットの{bot}です。この部屋の人たちと長く付き合ってきた友達のように、親しみやすく柔らかい丁寧語（です・ます）で短く話します。タメ口や堅苦しい敬語は使いません。"},
		"friendly_ja":         {Locale: "ja", Label: "Friendly Japanese", Builtin: true, Style: "あなたはDiscordボットの{bot}です。この部屋の人たちと長く付き合ってきた友達のように、短くて気軽なタメ口で話します。"},
		"polite_ja":           {Locale: "ja", Label: "Polite Japanese", Builtin: true, Style: "あなたはDiscordボットの{bot}です。いつも丁寧語（です・ます）で親切かつ正確に答えます。必要なときだけ短い箇条書きを使います。"},
		"brief_ja":            {Locale: "ja", Label: "Brief Japanese", Builtin: true, Style: "あなたはDiscordボットの{bot}です。挨拶や前置きなしで、要点だけを1〜2文で答えます。"},
		"cheer_ja":            {Locale: "ja", Label: "Cheerful Japanese", Builtin: true, Style: "あなたはDiscordボットの{bot}です。温かく親しみやすい口調で、相手の気持ちをまず汲み取り、励まします。大げさな褒め方はしません。"},
		"friendly_zh_hans":    {Locale: "zh-Hans", Label: "Friendly Chinese (Simplified)", Builtin: true, Style: "你是 Discord 机器人 {bot}。像和这个房间里的人相处很久的朋友一样，用轻松、简短的口语聊天。"},
		"polite_zh_hans":      {Locale: "zh-Hans", Label: "Polite Chinese (Simplified)", Builtin: true, Style: "你是 Discord 机器人 {bot}。始终用礼貌、亲切而准确的方式回答。只在有帮助时使用简短列表。"},
		"brief_zh_hans":       {Locale: "zh-Hans", Label: "Brief Chinese (Simplified)", Builtin: true, Style: "你是 Discord 机器人 {bot}。不要问候或铺垫，只用一两句话回答要点。"},
		"cheer_zh_hans":       {Locale: "zh-Hans", Label: "Cheerful Chinese (Simplified)", Builtin: true, Style: "你是 Discord 机器人 {bot}。用温暖亲切的语气，先体谅对方的心情并给予鼓励，不要过度夸奖。"},
		"friendly_es":         {Locale: "es", Label: "Friendly Spanish", Builtin: true, Style: "Eres {bot}, un bot de Discord. Habla de forma casual y breve, como un amigo que lleva mucho tiempo con la gente de esta sala."},
		"polite_es":           {Locale: "es", Label: "Polite Spanish", Builtin: true, Style: "Eres {bot}, un bot de Discord. Responde siempre con educación y precisión. Usa una lista corta solo cuando ayude."},
		"brief_es":            {Locale: "es", Label: "Brief Spanish", Builtin: true, Style: "Eres {bot}, un bot de Discord. Responde solo lo esencial, en una o dos frases, sin saludos ni relleno."},
		"cheer_es":            {Locale: "es", Label: "Cheerful Spanish", Builtin: true, Style: "Eres {bot}, un bot de Discord. Habla con calidez, fíjate en cómo se sienten las personas y anímalas, sin exagerar los elogios."},
	}
}

// BuiltinPersonaIDs lists built-ins in display order: Korean's two defaults and the historic English
// ones first, then each language's tones (warm, friendly, polite, brief, cheer, neutral) grouped by
// language tag.
func BuiltinPersonaIDs() []string {
	all := BuiltinPersonas()
	out := make([]string, 0, len(all))
	seen := map[string]bool{}
	for _, id := range []string{"warm_ko", "friendly_ko", "friendly_en", "neutral_en"} {
		if _, ok := all[id]; ok {
			seen[id] = true
			out = append(out, id)
		}
	}
	rest := make([]string, 0, len(all))
	for id := range all {
		if !seen[id] {
			rest = append(rest, id)
		}
	}
	slices.SortFunc(rest, func(a, b string) int {
		if all[a].Locale != all[b].Locale {
			return strings.Compare(all[a].Locale, all[b].Locale)
		}
		if ra, rb := personaToneRank(a), personaToneRank(b); ra != rb {
			return ra - rb
		}
		return strings.Compare(a, b)
	})
	return append(out, rest...)
}

// personaToneRank orders the tones inside one language: warm, friendly, polite, brief, cheer, neutral.
func personaToneRank(id string) int {
	base, _, _ := strings.Cut(id, "_")
	switch base {
	case "warm":
		return 0
	case "friendly":
		return 1
	case "polite":
		return 2
	case "brief":
		return 3
	case "cheer":
		return 4
	case "neutral":
		return 5
	}
	return 6
}

// DefaultPersonaCandidates lists the built-ins a room falls back to when it has no persona, in order:
// warm_<locale> (only with polite set, i18n.polite_default, and only for languages that have one),
// friendly_<locale>, friendly_en, friendly_ko (deduplicated). The first existing candidate that is not hidden becomes the room's default, so an
// operator who prefers the casual voice hides warm_<locale> and the room falls to friendly_<locale>.
// Persona IDs use underscores, so zh-Hans becomes friendly_zh_hans. The console mirrors this order
// (pages/Persona.svelte).
func DefaultPersonaCandidates(locale string, polite bool) []string {
	tag := lang.Normalize(locale)
	if tag == "" {
		tag = "en"
	}
	key := strings.ToLower(strings.ReplaceAll(tag, "-", "_"))
	ids := []string{"friendly_" + key, "friendly_en", "friendly_ko"}
	if _, ok := BuiltinPersonas()["warm_"+key]; ok && polite {
		ids = append([]string{"warm_" + key}, ids...)
	}
	// each candidate's v2 (behaviour rules) comes first; an operator who hides it gets the v1
	all := BuiltinPersonas()
	withV2 := make([]string, 0, 2*len(ids))
	for _, id := range ids {
		if _, ok := all[id+"_v2"]; ok {
			withV2 = append(withV2, id+"_v2")
		}
		withV2 = append(withV2, id)
	}
	ids = withV2
	out := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// DefaultChannelPolicy is applied when a live channel has no saved policy yet. PersonaRef stays
// empty: the effective persona follows the server/instance language (application.defaultPersonaFor).
func DefaultChannelPolicy() ChannelPolicy {
	return ChannelPolicy{
		Respond:    "command",
		PersonaRef: "",
		Capture:    false,
		Logging:    ChannelLogging{Level: "ops", RetainDays: 30},
		MemoryUse:  "on",
		Proactive:  false,
	}
}

// NormalizeChannelPolicy fills defaults and clamps reserved RAG fields. PersonaRef is left empty
// when unset so an unrelated edit (a tool switch, a memory view) never pins a room to one language's
// persona; the effective persona is resolved from the language at read time.
func NormalizeChannelPolicy(p ChannelPolicy) ChannelPolicy {
	d := DefaultChannelPolicy()
	if p.Respond == "" {
		p.Respond = d.Respond
	}
	if p.Logging.Level == "" {
		p.Logging.Level = d.Logging.Level
	}
	if p.Logging.RetainDays == 0 && p.Logging.Level != "off" {
		p.Logging.RetainDays = d.Logging.RetainDays
	}
	// "unavailable" was the pre-RAG placeholder; it was never a user choice.
	if p.MemoryUse != "off" {
		p.MemoryUse = "on"
	}
	p.Proactive = false
	p.NameCall = firstOr(p.NameCall, NameCallModes)
	p.Attribution = firstOr(p.Attribution, AttributionModes)
	p.Conversation.Follow = firstOr(p.Conversation.Follow, FollowModes)
	p.Conversation.Thread = firstOr(p.Conversation.Thread, ThreadModes)
	p.Conversation.ThreadFollow = firstOr(p.Conversation.ThreadFollow, ThreadFollowModes)
	p.Conversation.ThreadRecap = firstOr(p.Conversation.ThreadRecap, ThreadRecapModes)
	p.Conversation.ReplyShape = firstOr(p.Conversation.ReplyShape, ReplyShapes)
	p.Conversation.Banter = firstOr(p.Conversation.Banter, BanterModes)
	p.Conversation.RoomStyle = firstOr(p.Conversation.RoomStyle, RoomStyleModes)
	if p.RAGAttachments == nil {
		p.RAGAttachments = []RAGAttachmentRef{}
	}
	if p.RAGView.Blocks == nil {
		p.RAGView.Blocks = []string{}
	}
	return p
}

// firstOr is v, or the default (the first choice) when v is empty.
func firstOr(v string, choices []string) string {
	if v == "" {
		return choices[0]
	}
	return v
}

func ValidRespond(mode string) bool {
	return slices.Contains(RespondModes, mode)
}

func ValidLoggingLevel(level string) bool {
	return slices.Contains(LoggingLevels, level)
}

type Instance struct {
	ID            string `yaml:"id" json:"id"`
	ExpectedBotID string `yaml:"expected_bot_id" json:"expected_bot_id"`
	DataDir       string `yaml:"data_dir" json:"data_dir"`
	Profile       string `yaml:"profile" json:"profile"`
	// BotNames are the names people call the bot by. The first is how prompts refer to it; any of
	// them in a message counts as addressing the bot (respond: mention); none becomes a person target.
	BotNames []string `yaml:"bot_names,omitempty" json:"bot_names,omitempty"`
	// LogLevel is how much the daemon writes to its log: debug, info (the default), warn or error.
	// MASAMONG_LOG_LEVEL in the environment overrides it for one run.
	LogLevel string `yaml:"log_level,omitempty" json:"log_level,omitempty"`
}

// LogLevels are the values instance.log_level accepts besides empty (info).
var LogLevels = []string{"debug", "info", "warn", "error"}

// BotNames returns the configured names, or the product name.
func (d Document) BotNames() []string {
	if len(d.Instance.BotNames) > 0 {
		return d.Instance.BotNames
	}
	return []string{"masamong"}
}

type Admin struct {
	Transport string `yaml:"transport" json:"transport"`
	Socket    string `yaml:"socket" json:"socket"`
	Web       Web    `yaml:"web" json:"web"`
}

type Web struct {
	Enabled bool   `yaml:"enabled" json:"enabled"`
	Bind    string `yaml:"bind" json:"bind"`
	// Hosts are names the console also answers to besides 127.0.0.1/localhost, for a proxy on this
	// computer that forwards another network to it (tailscale serve: the tailnet name). The console
	// still listens on loopback only; any other name is refused (DNS rebinding).
	Hosts []string `yaml:"hosts,omitempty" json:"hosts,omitempty"`
}

// WebHost normalizes one admin.web.hosts entry: a bare host name, lowercase, no trailing dot.
func WebHost(h string) (string, error) {
	h = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
	if h == "" || len(h) > 253 || strings.HasPrefix(h, ".") || strings.Contains(h, "..") {
		return "", fmt.Errorf("admin.web.hosts: %q is not a host name", h)
	}
	for _, r := range h {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '.') {
			return "", fmt.Errorf("admin.web.hosts: %q is not a host name (no scheme, port, path or wildcard)", h)
		}
	}
	return h, nil
}

type I18n struct {
	DefaultLocale  string   `yaml:"default_locale" json:"default_locale"`
	FallbackLocale string   `yaml:"fallback_locale" json:"fallback_locale"`
	Bundled        []string `yaml:"bundled_locales" json:"bundled_locales"`
	Timezone       string   `yaml:"timezone" json:"timezone"`
	// PoliteDefault makes rooms without a persona start in the language's polite-but-warm voice
	// (warm_ko, warm_ja) instead of the casual friend. New instances turn it on; an instance made
	// before it existed keeps the casual default it was running with until the operator turns it on.
	PoliteDefault bool `yaml:"polite_default,omitempty" json:"polite_default,omitempty"`
}

// Provider is an outbound API endpoint. Kind selects the wire adapter (internal/providers/*);
// CredentialRef is env:NAME, file:PATH or keychain:NAME and never the secret itself (06장 §2, 11장 §3).
type Provider struct {
	Kind    string `yaml:"kind" json:"kind"`
	BaseURL string `yaml:"base_url,omitempty" json:"base_url,omitempty"`
	// API names the host family of an openai_compat provider (openrouter | openai | gemini |
	// anthropic | compatible); empty means detect it from base_url's host (internal/providers/openai/api.go).
	API           string `yaml:"api,omitempty" json:"api,omitempty"`
	CredentialRef string `yaml:"credential_ref,omitempty" json:"credential_ref,omitempty"`
	DailyLimit    int    `yaml:"daily_limit,omitempty" json:"daily_limit,omitempty"`
	// Disabled turns the provider off without deleting it (no requests, features report unconfigured).
	Disabled bool `yaml:"disabled,omitempty" json:"disabled,omitempty"`
	// ByokOnly (OpenRouter) maps a model family, the part of a model name before "/", to the one
	// upstream provider its chat requests may go to, with no fallback to others: {anthropic: anthropic}
	// sends anthropic/* only to Anthropic. With your own Anthropic key added to OpenRouter (BYOK, set to
	// never use shared capacity) those models then run only on your key; when the key fails, the request
	// fails and the role's fallback model answers instead of OpenRouter credits paying for it.
	ByokOnly map[string]string `yaml:"byok_only,omitempty" json:"byok_only,omitempty"`
}

// Known provider kinds. "fake" is the offline default used by tests and by features whose provider is not configured.
const (
	KindFake        = "fake"
	KindKMA         = "kma"
	KindFX          = "exchangerate"
	KindFinnhub     = "finnhub"
	KindKakaoSearch = "kakao_search"
	KindTavily      = "tavily"
	KindLinkup      = "linkup"
	KindExa         = "exa"
	KindSerper      = "serper"
	KindBrave       = "brave"
	KindTinyFish    = "tinyfish"
	// KindJinaReader reads a web page as text (web.read); it works without a key.
	KindJinaReader = "jina_reader"
	KindOpenAI     = "openai_compat"
	// KindOpenMeteo is worldwide weather (internal/providers/openmeteo); it works without a key.
	KindOpenMeteo = "open_meteo"
	// KindPractice is the console's offline practice model (internal/providers/practice): no network, no cost.
	KindPractice = "practice"
)

var ProviderKinds = []string{KindFake, KindKMA, KindFX, KindFinnhub, KindKakaoSearch, KindTavily, KindLinkup, KindExa, KindSerper,
	KindBrave, KindTinyFish, KindJinaReader, KindOpenAI, KindPractice, KindOpenMeteo}

// SearchKind reports whether kind is a web search service (search.web).
func SearchKind(kind string) bool { return slices.Contains(websearch.Kinds, kind) }

// Discord is the gateway connector. The bot never starts unless enabled=true and the token ref resolves.
type Discord struct {
	Enabled          bool     `yaml:"enabled" json:"enabled"`
	TokenRef         string   `yaml:"token_ref,omitempty" json:"token_ref,omitempty"`
	GuildIDs         []string `yaml:"guild_ids,omitempty" json:"guild_ids,omitempty"` // command registration scope; empty = global
	CommandPrefix    string   `yaml:"command_prefix,omitempty" json:"command_prefix,omitempty"`
	RegisterCommands bool     `yaml:"register_commands" json:"register_commands"`
	MessageContent   bool     `yaml:"message_content" json:"message_content"` // privileged intent for prefix commands
}

type Model struct {
	Provider   string `yaml:"provider" json:"provider"`
	Model      string `yaml:"model" json:"model"`
	Dimensions int    `yaml:"dimensions,omitempty" json:"dimensions,omitempty"`
	// Prices in USD per million tokens, for hosts whose model list has no prices. They win over the
	// list; a provider-reported cost (OpenRouter usage.cost) wins over both.
	PriceInPerMTok  float64 `yaml:"price_in_per_mtok,omitempty" json:"price_in_per_mtok,omitempty"`
	PriceOutPerMTok float64 `yaml:"price_out_per_mtok,omitempty" json:"price_out_per_mtok,omitempty"`
	// PriceCachedInPerMTok prices input the host served from its prompt cache (optional; empty uses
	// the model list's or the published cache price, else the input price).
	PriceCachedInPerMTok float64 `yaml:"price_cached_in_per_mtok,omitempty" json:"price_cached_in_per_mtok,omitempty"`
	// TimeoutSeconds is how long one chat call to this model may take before the other chat model is
	// asked (planner and response only; 0 = 30 s for the planner, 90 s for the answer). Raise it for a
	// model that thinks before it answers.
	TimeoutSeconds int `yaml:"timeout_seconds,omitempty" json:"timeout_seconds,omitempty"`
	// Effort is how much a reasoning model thinks on this role's calls (the chat roles, FallbackRoles;
	// one of ModelEfforts, empty = low). Precedence:
	//   - the role's calls ask for this level (empty: low);
	//   - calls the code makes thinking-free on purpose (the name-call, reference and follow-up
	//     classifiers, fortunes) ask for none while this is empty; once it is set they ask for it capped
	//     at low (a planner at high or max does not make a 40-token classifier think hard), with output
	//     room for the thinking;
	//   - the summary role's own effort is the level of the job itself and applies as set; a summary on
	//     another role's model (no summary model) asks for none while models.summary.effort is empty;
	//   - a fallback with an effort of its own uses that instead (ModelFallback.Effort); a fallback
	//     without one follows the same rules for the role it answers, whatever model it borrows.
	// The adapter writes the level the way the host takes it and clamps it to what the model's list says
	// it takes (xhigh and max become high where the host has no such level); a model known not to reason
	// gets no field. On Anthropic, low means adaptive thinking at effort low (it may think a little; a
	// model with a fixed thinking budget, such as Claude Haiku 4.5, gets the smallest budget, 1024 tokens).
	Effort string `yaml:"effort,omitempty" json:"effort,omitempty"`
	// Fallbacks are asked in order when this model fails (FallbackRoles only): another key or host for
	// the same model, or another model. See internal/providers/openai/failover.go for what counts as a
	// failure and how a failing model is skipped for a while.
	Fallbacks []ModelFallback `yaml:"fallbacks,omitempty" json:"fallbacks,omitempty"`
}

// ModelFallback is one stand-in for a role's model. Prices and the time limit work as on Model; a
// host that reports no cost and lists no price needs them, or its calls count as free.
type ModelFallback struct {
	Provider             string  `yaml:"provider" json:"provider"`
	Model                string  `yaml:"model" json:"model"`
	PriceInPerMTok       float64 `yaml:"price_in_per_mtok,omitempty" json:"price_in_per_mtok,omitempty"`
	PriceOutPerMTok      float64 `yaml:"price_out_per_mtok,omitempty" json:"price_out_per_mtok,omitempty"`
	PriceCachedInPerMTok float64 `yaml:"price_cached_in_per_mtok,omitempty" json:"price_cached_in_per_mtok,omitempty"`
	TimeoutSeconds       int     `yaml:"timeout_seconds,omitempty" json:"timeout_seconds,omitempty"`
	// Effort replaces the role's effort on this model (one of ModelEfforts; empty = the role's)
	Effort string `yaml:"effort,omitempty" json:"effort,omitempty"`
}

// ModelEfforts are the values of models.<role>.effort and models.<role>.fallbacks[].effort, least
// thinking first. xhigh and max exist on some hosts only (Anthropic, OpenAI and OpenRouter take xhigh):
// elsewhere they are clamped at call time, never refused here.
var ModelEfforts = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// FallbackRoles are the roles whose model may have fallbacks and an effort: the ones that chat. Embeddings are left
// out on purpose (vectors from another model do not match the stored ones), and rerank and image
// generation have no fallback path.
var FallbackRoles = []string{"response", "planner", "summary", "vision"}

// MaxFallbacks caps a role's fallback list: each one can add a full time limit to a call that fails.
const MaxFallbacks = 3

type Limits struct {
	Profile             string `yaml:"profile" json:"profile"`
	MonthlyBudgetUSD    string `yaml:"monthly_budget_usd" json:"monthly_budget_usd"`
	PerRequestBudgetUSD string `yaml:"per_request_budget_usd" json:"per_request_budget_usd"`
	MaxExternalAttempts int    `yaml:"max_external_attempts" json:"max_external_attempts"`
	// BudgetAlertPercent is when the console starts warning about the monthly budget (0 = 80).
	BudgetAlertPercent int `yaml:"budget_alert_percent,omitempty" json:"budget_alert_percent,omitempty"`
	// MaxConcurrentTurns caps chat turns (model calls) running at once across all channels (0 = 8).
	MaxConcurrentTurns int `yaml:"max_concurrent_turns,omitempty" json:"max_concurrent_turns,omitempty"`
}

// ConcurrentTurns is the effective cap on chat turns running at once (1..64).
func (l Limits) ConcurrentTurns() int {
	switch {
	case l.MaxConcurrentTurns <= 0:
		return 8
	case l.MaxConcurrentTurns > 64:
		return 64
	}
	return l.MaxConcurrentTurns
}

// AlertPercent is the effective budget warning threshold.
func (l Limits) AlertPercent() int {
	if l.BudgetAlertPercent <= 0 || l.BudgetAlertPercent > 100 {
		return 80
	}
	return l.BudgetAlertPercent
}

// Backup is the automatic backup schedule. Backups hold the config and the database (never secrets)
// under <data-root>/backups; only automatic ones are pruned to Keep.
// Retention is how long operational records are kept, in days (0 = the default, -1 = forever). Memory
// (captured messages, leaves, summaries) is never pruned by age; it is managed per block.
type Retention struct {
	TurnDays       int `yaml:"turn_days,omitempty" json:"turn_days,omitempty"`               // turn rows for history, stats, reports (400)
	TurnDetailDays int `yaml:"turn_detail_days,omitempty" json:"turn_detail_days,omitempty"` // message, answer and evidence of a turn (30)
	TraceDays      int `yaml:"trace_days,omitempty" json:"trace_days,omitempty"`             // diagnostic traces (30)
	AuditDays      int `yaml:"audit_days,omitempty" json:"audit_days,omitempty"`             // change log and channel ops log (400)
	UsageDays      int `yaml:"usage_days,omitempty" json:"usage_days,omitempty"`             // per-user and per-channel tool counters (14)
	QueryCacheDays int `yaml:"query_cache_days,omitempty" json:"query_cache_days,omitempty"` // question embeddings cache (7)
	ImageDays      int `yaml:"image_days,omitempty" json:"image_days,omitempty"`             // generated pictures (14)
}

func keepDays(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

// Days returns the effective retention for each record kind (-1 = keep forever).
func (r Retention) Days() map[string]int {
	return map[string]int{
		"turns": keepDays(r.TurnDays, 400), "turn_detail": keepDays(r.TurnDetailDays, 30), "traces": keepDays(r.TraceDays, 30),
		"audit": keepDays(r.AuditDays, 400), "usage": keepDays(r.UsageDays, 14), "query_cache": keepDays(r.QueryCacheDays, 7),
		"images": keepDays(r.ImageDays, 14),
	}
}

type Backup struct {
	Schedule string `yaml:"schedule" json:"schedule"` // off | daily | weekly
	Hour     int    `yaml:"hour" json:"hour"`         // local hour in i18n.timezone, 0–23
	Weekday  int    `yaml:"weekday" json:"weekday"`   // weekly: 0 = Sunday … 6 = Saturday
	Keep     int    `yaml:"keep" json:"keep"`         // automatic backups kept
}

// BackupSchedules are the accepted backup.schedule values.
var BackupSchedules = []string{"off", "daily", "weekly"}

// DefaultBackup is weekly at 04:00 on Sunday, keeping four.
func DefaultBackup() Backup { return Backup{Schedule: "weekly", Hour: 4, Weekday: 0, Keep: 4} }

// MemoryFlags configure channel memory (docs/rag/README.md). Capture stores live messages of channels
// whose policy has capture on; Embed runs background indexing; Retrieve is the ranking mode replies
// use (off disables memory search everywhere).
type MemoryFlags struct {
	Capture          bool     `yaml:"capture" json:"capture"`
	Extract          bool     `yaml:"extract" json:"extract"` // claims: not implemented (sandbox B3 unmeasured)
	Embed            bool     `yaml:"embed" json:"embed"`
	Retrieve         string   `yaml:"retrieve" json:"retrieve"` // off | auto | hybrid | rerank
	Summaries        bool     `yaml:"summaries,omitempty" json:"summaries,omitempty"`
	NaiveTimezone    string   `yaml:"naive_timezone,omitempty" json:"naive_timezone,omitempty"` // declared zone of Kakao (naive) times
	IndexBudgetUSD   string   `yaml:"index_budget_usd,omitempty" json:"index_budget_usd,omitempty"`
	TurnBudgetUSD    string   `yaml:"turn_budget_usd,omitempty" json:"turn_budget_usd,omitempty"`
	IndexIntervalMin int      `yaml:"index_interval_min,omitempty" json:"index_interval_min,omitempty"`
	ChatTools        []string `yaml:"chat_tools,omitempty" json:"chat_tools,omitempty"` // deprecated: use tools.<id>.chat
	// IndexWorkers is how many embedding and summary requests an indexing pass sends at once (0 = 8,
	// 1..256). Lower it for an embedding host with a tight rate limit; raise it to finish a large first
	// index sooner on a host without one (paid OpenRouter).
	IndexWorkers int `yaml:"index_workers,omitempty" json:"index_workers,omitempty"`
	// Languages are the corpus languages (language packs, internal/lang): they choose the tokenizer,
	// name particles, pronouns, stop words, evidence markers, summary prompt and query instruction.
	// Several languages are merged (mixed rooms). Empty = the default pack (ko).
	Languages   []string `yaml:"languages,omitempty" json:"languages,omitempty"`
	AgentPrompt string   `yaml:"agent_prompt,omitempty" json:"agent_prompt,omitempty"`
	LeafSplit   string   `yaml:"leaf_split,omitempty" json:"leaf_split,omitempty"` // time_gap (default) | fixed
	// PersonAliases links names that belong to one person (label → other labels), e.g. a KakaoTalk
	// name and a Discord nickname. Only an operator sets them; nothing is linked automatically
	// (RAW_DATA_POLICY). Same shape as the sandbox's person_aliases.
	PersonAliases map[string][]string `yaml:"person_aliases,omitempty" json:"person_aliases,omitempty"`
	// BackfillDays is how far back the bot catches up, when it (re)connects to Discord, on messages
	// posted in capturing channels while it was away (0 = 7 days, -1 = off, at most 30). It only
	// continues channels it has captured before; it never reads a channel's older history.
	BackfillDays int `yaml:"backfill_days,omitempty" json:"backfill_days,omitempty"`
	// BackfillMaxMessages bounds one channel's catch-up (0 = 4000).
	BackfillMaxMessages int `yaml:"backfill_max_messages,omitempty" json:"backfill_max_messages,omitempty"`
	// DerivedBudgetUSD is the monthly budget of the background room memory (ADR-17: episode extraction
	// and the room state, spend kinds episode/state). Empty = 3.00; "0" switches that work off.
	DerivedBudgetUSD string `yaml:"derived_budget_usd,omitempty" json:"derived_budget_usd,omitempty"`
	// DayBoundary (HH:MM, i18n.timezone) is where one day's conversation ends for room memory: talk that
	// runs past midnight stays one conversation. Empty = 05:00.
	DayBoundary string `yaml:"day_boundary,omitempty" json:"day_boundary,omitempty"`
	// Episodes and State switch reading the room memory into replies (docs/plan-memory-conversation.md
	// §5.4.6): related episodes (L2) and the room's live topics (L1). on | off, empty = on. Writing is
	// gated by derived_budget_usd alone.
	Episodes string `yaml:"episodes,omitempty" json:"episodes,omitempty"`
	State    string `yaml:"state,omitempty" json:"state,omitempty"`
	// Cards switches person cards (L3, §5.4.4): promotion of facts that came up on several days into a
	// short card per server and person, and reading them into replies. on | off, empty = off.
	Cards string `yaml:"cards,omitempty" json:"cards,omitempty"`
	// WarmQuery lets a turn embed the calling message before the planner says a memory search is needed,
	// so the search that follows finds the vector ready. on | off, empty = on. Off sends the message to
	// the embedding provider only when a search asks for it (see WarmQueryOn).
	WarmQuery string `yaml:"warm_query,omitempty" json:"warm_query,omitempty"`
	// MessageVectors also embeds every message of a closed leaf on its own (1024 dimensions at most) and
	// lets search use those vectors as one more candidate source, so a one-line fact in a long leaf is
	// found. It costs storage and memory (about 1 KB of RAM per message) and one embedding per message.
	// Unset = on (see MessageVectorsOn); an explicit false is kept and turns it off.
	MessageVectors *bool `yaml:"message_vectors,omitempty" json:"message_vectors,omitempty"`
}

// OnOff are the values of the memory.episodes, memory.state, memory.cards and memory.warm_query switches.
var OnOff = []string{"on", "off"}

// WarmQueryOn reports whether a turn may embed its message ahead of the plan (memory.warm_query, default
// on). The turn's own conditions (practice, a daily_limit, withdrawn consent) still apply on top of it.
func (m MemoryFlags) WarmQueryOn() bool { return m.WarmQuery != "off" }

// EpisodesOn reports whether replies read related room episodes (memory.episodes, default on).
func (m MemoryFlags) EpisodesOn() bool { return m.Episodes != "off" }

// StateOn reports whether replies read the room's live topics (memory.state, default on).
func (m MemoryFlags) StateOn() bool { return m.State != "off" }

// MessageVectorsOn reports whether the message-level index is built and searched (memory.message_vectors,
// default on; only an explicit false turns it off).
func (m MemoryFlags) MessageVectorsOn() bool { return m.MessageVectors == nil || *m.MessageVectors }

// CardsOn reports whether person cards are made and read (memory.cards, default off).
func (m MemoryFlags) CardsOn() bool { return m.Cards == "on" }

// DerivedBudget is the effective monthly budget of the background room memory in USD (0 = off).
func (m MemoryFlags) DerivedBudget() float64 {
	if v, err := strconv.ParseFloat(strings.TrimSpace(m.DerivedBudgetUSD), 64); err == nil && v >= 0 {
		return v
	}
	return 3.00
}

// Boundary is the effective day boundary of room memory (HH:MM).
func (m MemoryFlags) Boundary() string {
	if validHHMM(m.DayBoundary) {
		return m.DayBoundary
	}
	return "05:00"
}

// Backfill is the effective catch-up window (0 = off).
func (m MemoryFlags) Backfill() time.Duration {
	switch {
	case m.BackfillDays < 0:
		return 0
	case m.BackfillDays == 0:
		return 7 * 24 * time.Hour
	}
	return time.Duration(min(m.BackfillDays, 30)) * 24 * time.Hour
}

// MaxIndexWorkers is the most memory.index_workers allows.
const MaxIndexWorkers = 256

// Workers is the effective number of requests an indexing pass sends at once (1..256).
func (m MemoryFlags) Workers() int {
	switch {
	case m.IndexWorkers <= 0:
		return 8
	case m.IndexWorkers > MaxIndexWorkers:
		return MaxIndexWorkers
	}
	return m.IndexWorkers
}

// BackfillMax is the effective per-channel catch-up bound in messages.
func (m MemoryFlags) BackfillMax() int {
	if m.BackfillMaxMessages <= 0 {
		return 4000
	}
	return min(m.BackfillMaxMessages, 50000)
}

// ChatSettings is how a reply turn works, for the whole instance.
type ChatSettings struct {
	// ToolMode is how the turn picks tools (docs/plan-memory-conversation.md §5.5): "loop" (the default:
	// the tool loop picks with native tool calling while the planner only decides the memory search),
	// "planner" (the memory planner's JSON plan names them, the earlier way) or "shadow" (the planner
	// still picks; the tool loop runs beside it and what it would have called is recorded in the turn,
	// nothing more). A model that takes no tools falls back to the planner for that turn.
	ToolMode string `yaml:"tool_mode,omitempty" json:"tool_mode,omitempty"`
	// ToolModel is which model role the tool loop asks: "planner" (the default: the small fast model) or
	// "response" (the precise mode: the answer model, slower and dearer, for weak small models).
	ToolModel string `yaml:"tool_model,omitempty" json:"tool_model,omitempty"`
	// LoopFinal is when the tool loop stops without asking its model once more: "tools" (the default:
	// after a round of tools whose one result is the answer, such as a place search), "search"
	// (also after a web search that found enough) or "off" (it always asks).
	LoopFinal string `yaml:"loop_final,omitempty" json:"loop_final,omitempty"`
	// Vision is whether the bot looks at pictures it is called with: "auto" (the default: the answer
	// model sees them when it takes pictures, otherwise a chat model that does describes them first)
	// or "off". Only pictures attached to the message that calls the bot (or the one it replies to)
	// are read; nothing looks at a room's pictures in the background.
	Vision string `yaml:"vision,omitempty" json:"vision,omitempty"`
	// Progress is when the bot posts a short "still looking" line during a turn that searches the web,
	// deleted before the answer: "deep" (the default: at once for a deep search or a second round of
	// searches), "slow" (those, and also a search turn still unanswered after a few seconds) or "off".
	Progress string `yaml:"progress,omitempty" json:"progress,omitempty"`
}

// ProgressModes are the values chat.progress accepts besides empty (deep).
var ProgressModes = []string{"slow", "deep", "off"}

// ProgressMode is the effective chat.progress.
func (c ChatSettings) ProgressMode() string {
	if c.Progress == "" {
		return "deep"
	}
	return c.Progress
}

// VisionModes are the values chat.vision accepts besides empty (auto).
var VisionModes = []string{"auto", "off"}

// VisionOn reports whether the bot looks at the pictures it is called with.
func (c ChatSettings) VisionOn() bool { return c.Vision != "off" }

// VisionMode is the effective chat.vision.
func (c ChatSettings) VisionMode() string {
	if c.Vision == "" {
		return "auto"
	}
	return c.Vision
}

// ToolModels are the values chat.tool_model accepts besides empty (planner).
var ToolModels = []string{"planner", "response"}

// LoopRole is the model role the tool loop uses.
func (c ChatSettings) LoopRole() string {
	if c.ToolModel == "" {
		return "planner"
	}
	return c.ToolModel
}

// LoopFinals are the values chat.loop_final accepts besides empty (tools).
var LoopFinals = []string{"tools", "search", "off"}

// LoopFinalMode is the effective chat.loop_final.
func (c ChatSettings) LoopFinalMode() string {
	if c.LoopFinal == "" {
		return "tools"
	}
	return c.LoopFinal
}

// ToolModes are the values chat.tool_mode accepts besides empty (loop, the default since the live regression
// sets of 2026-10-11 came out better than the planner: ko 57/59 vs 45/59, en 59/59 vs 49/59, usage 51/52 vs 50/52).
var ToolModes = []string{"loop", "planner", "shadow"}

// Mode is the effective chat.tool_mode.
func (c ChatSettings) Mode() string {
	if c.ToolMode == "" {
		return "loop"
	}
	return c.ToolMode
}

// RetrieveModes are the accepted memory.retrieve values.
var RetrieveModes = []string{"off", "auto", "hybrid", "rerank"}

// Participation is the instance switch and limits of joining conversations unasked (12). A channel
// opts in with guilds.<g>.channels.<c>.participation; the stricter of the two applies. The numbers are
// product hypotheses to tune from the shadow log, not settled values (masamong-v2-design/09 §3).
type Participation struct {
	Enabled bool   `yaml:"enabled" json:"enabled"`
	Mode    string `yaml:"mode" json:"mode"` // the most any channel may do: off | collect | shadow | conservative
	// WaitSec is how long a question may go unanswered by people before the bot considers it (0 = 45).
	WaitSec int `yaml:"wait_sec,omitempty" json:"wait_sec,omitempty"`
	// CooldownSec is the least time between two unasked remarks in a channel (0 = 180, at least 120).
	CooldownSec int `yaml:"cooldown_sec,omitempty" json:"cooldown_sec,omitempty"`
	PerHour     int `yaml:"per_hour,omitempty" json:"per_hour,omitempty"` // per channel (0 = 3)
	PerDay      int `yaml:"per_day,omitempty" json:"per_day,omitempty"`   // per channel (0 = 10)
	// JudgePerHour caps the model judgements of candidates per channel (0 = 12): each is a paid call.
	JudgePerHour int `yaml:"judge_per_hour,omitempty" json:"judge_per_hour,omitempty"`
	// QuietStart/QuietEnd (HH:MM, i18n.timezone) are hours with no unasked remarks.
	QuietStart string `yaml:"quiet_start,omitempty" json:"quiet_start,omitempty"`
	QuietEnd   string `yaml:"quiet_end,omitempty" json:"quiet_end,omitempty"`
	// MinScore is the judge score (a ranking score, not a probability) a candidate needs (0 = 0.7).
	MinScore float64 `yaml:"min_score,omitempty" json:"min_score,omitempty"`
	// ShadowDrafts writes the would-be reply in shadow mode so it can be reviewed (unset = yes; each
	// draft is a model call).
	ShadowDrafts *bool `yaml:"shadow_drafts,omitempty" json:"shadow_drafts,omitempty"`
}

// ParticipationModes in increasing order of what the bot may do. collect records the moments the bot
// could have joined and what people did next, with no model call and nothing said: the data the
// experimental feature is tuned from before any channel lets it speak (plan §5.7).
var ParticipationModes = []string{"off", "collect", "shadow", "conservative"}

// ParticipationRank orders modes (unknown = off).
func ParticipationRank(mode string) int {
	if i := slices.Index(ParticipationModes, mode); i >= 0 {
		return i
	}
	return 0
}

// Wait, Cooldown, Hourly, Daily, Threshold and Drafts are the limits with their defaults.
func (p Participation) Wait() time.Duration {
	if p.WaitSec <= 0 {
		return 45 * time.Second
	}
	return time.Duration(p.WaitSec) * time.Second
}

func (p Participation) Cooldown() time.Duration {
	if p.CooldownSec <= 0 {
		return 180 * time.Second
	}
	return time.Duration(max(120, p.CooldownSec)) * time.Second
}

func (p Participation) Hourly() int {
	if p.PerHour <= 0 {
		return 3
	}
	return p.PerHour
}

func (p Participation) Daily() int {
	if p.PerDay <= 0 {
		return 10
	}
	return p.PerDay
}

func (p Participation) Judges() int {
	if p.JudgePerHour <= 0 {
		return 12
	}
	return p.JudgePerHour
}

func (p Participation) Threshold() float64 {
	if p.MinScore <= 0 || p.MinScore > 1 {
		return 0.7
	}
	return p.MinScore
}

func (p Participation) Drafts() bool { return p.ShadowDrafts == nil || *p.ShadowDrafts }

func Default(instanceID, dataDir string) Document {
	if instanceID == "" {
		instanceID = "masamo"
	}
	return Document{
		SchemaVersion: 1,
		Instance:      Instance{ID: instanceID, DataDir: dataDir, Profile: "lite", BotNames: []string{"마사몽", "masamong"}},
		Admin: Admin{
			Transport: "unix_socket",
			Web:       Web{Enabled: true, Bind: "127.0.0.1:8787"},
		},
		I18n: I18n{
			DefaultLocale: "en", FallbackLocale: "en",
			Bundled:  lang.Available(),
			Timezone: "Asia/Seoul", PoliteDefault: true,
		},
		Discord: Discord{Enabled: false, TokenRef: "env:MASAMONG_DISCORD_TOKEN", CommandPrefix: "!", RegisterCommands: true, MessageContent: false},
		Providers: map[string]Provider{
			"kma":          {Kind: KindKMA, BaseURL: "https://apihub.kma.go.kr", CredentialRef: "env:MASAMONG_KMA_KEY", DailyLimit: 10000},
			"openmeteo":    {Kind: KindOpenMeteo, BaseURL: "https://api.open-meteo.com", DailyLimit: 5000},
			"exchangerate": {Kind: KindFX, CredentialRef: "env:MASAMONG_EXCHANGERATE_KEY", DailyLimit: 40},
			"finnhub":      {Kind: KindFinnhub, BaseURL: "https://finnhub.io/api/v1", CredentialRef: "env:MASAMONG_FINNHUB_KEY", DailyLimit: 1000},
			"kakao":        {Kind: KindKakaoSearch, BaseURL: "https://dapi.kakao.com", CredentialRef: "env:MASAMONG_KAKAO_KEY", DailyLimit: 1000},
			"tavily":       {Kind: KindTavily, BaseURL: "https://api.tavily.com", CredentialRef: "env:MASAMONG_TAVILY_KEY", DailyLimit: 100},
			"linkup":       {Kind: KindLinkup, BaseURL: "https://api.linkup.so", CredentialRef: "env:MASAMONG_LINKUP_KEY", DailyLimit: 100},
			"exa":          {Kind: KindExa, BaseURL: "https://api.exa.ai", CredentialRef: "env:MASAMONG_EXA_KEY", DailyLimit: 100},
			"serper":       {Kind: KindSerper, BaseURL: "https://google.serper.dev", CredentialRef: "env:MASAMONG_SERPER_KEY", DailyLimit: 100},
			// Brave: $5 of free credit a month is about 1,000 searches; TinyFish: 12,000 a day free
			"brave":      {Kind: KindBrave, BaseURL: "https://api.search.brave.com", CredentialRef: "env:MASAMONG_BRAVE_KEY", DailyLimit: 30},
			"tinyfish":   {Kind: KindTinyFish, BaseURL: "https://api.search.tinyfish.ai", CredentialRef: "env:MASAMONG_TINYFISH_KEY", DailyLimit: 1000},
			"openrouter": {Kind: KindOpenAI, BaseURL: "https://openrouter.ai/api/v1", CredentialRef: "env:MASAMONG_OPENROUTER_KEY"},
		},
		Models: map[string]Model{
			"response": {Provider: "openrouter", Model: "deepseek/deepseek-v4.1-flash"},
			"planner":  {Provider: "openrouter", Model: "openai/gpt-5.6-luna"},
			"summary":  {Provider: "openrouter", Model: "deepseek/deepseek-v4.1-flash"},
			"embed":    {Provider: "openrouter", Model: "qwen/qwen3-embedding-8b", Dimensions: DefaultEmbedDimensions},
			"rerank":   {Provider: "openrouter", Model: "voyageai/rerank-2.5-lite"},
		},
		Limits: Limits{
			Profile: "lite", MonthlyBudgetUSD: "10.00", PerRequestBudgetUSD: "0.15", MaxExternalAttempts: 10,
		},
		Memory: MemoryFlags{Capture: false, Extract: false, Embed: true, Retrieve: "auto", Summaries: true,
			NaiveTimezone: "Asia/Seoul", IndexBudgetUSD: "1.00", TurnBudgetUSD: "0.05", IndexIntervalMin: 10,
			Languages: []string{"ko"}},
		Participation: Participation{Enabled: false, Mode: "off"},
		Backup:        DefaultBackup(),
		// V1's image limits: 10 a person, 30 a server channel, 50 in all per day
		Tools: map[string]tools.Settings{"images.generate": {Limits: tools.Limits{PerUserPerDay: 10, PerChannelPerDay: 30, PerDay: 50}}},
		Features: map[string]bool{
			"weather": true, "disasters": true, "fortune": true, "notices": true,
			"polls": true, "images": true, "finance": true, "search": true, "summaries": true, "reminders": true,
		},
	}
}

// Parse reads and validates a configuration someone is writing: every rule applies, including the
// web address policy (tools.ValidateHTTPURL).
func Parse(raw []byte) (Document, error) { return parse(raw, nil) }

// Problem is a setting of a committed configuration that this release no longer accepts. The setting is
// switched off, not the whole bot: an upgrade must not stop a daemon over an address it used to run with.
type Problem struct {
	Path   string `json:"path"`   // providers.<id>.base_url, tools.<id>.http.url
	Reason string `json:"reason"` // what is wrong with it, in words an operator can act on
	Off    string `json:"off"`    // what was switched off: providers.<id>, tools.<id>
}

// ParseCommitted reads a configuration that was accepted earlier (the committed revision at start,
// a backup being verified). A web address the policy now refuses disables its provider or tool and is
// reported in the problems; everything else is validated exactly as Parse does. Writing such a document
// back (config.apply) skips the address check of a disabled entry, and enabling it again fails with the
// reason, so the operator fixes the address at the moment it matters.
func ParseCommitted(raw []byte) (Document, []Problem, error) {
	var problems []Problem
	d, err := parse(raw, &problems)
	return d, problems, err
}

func parse(raw []byte, problems *[]Problem) (Document, error) {
	var d Document
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&d); err != nil {
		return Document{}, fmt.Errorf("config parse: %w", err)
	}
	if !slices.Contains(DMAllowModes, d.DM.AllowMode()) {
		return Document{}, fmt.Errorf("dm.allow must be members, operators or off")
	}
	if d.DM.PerUserPerDay < -1 || d.DM.PerUserPerDay > 10000 {
		return Document{}, fmt.Errorf("dm.per_user_per_day must be -1 (unlimited), 0 (default), or 1..10000")
	}
	if d.DM.PerDay < -1 || d.DM.PerDay > 100000 {
		return Document{}, fmt.Errorf("dm.per_day must be -1 (unlimited), 0 (default), or 1..100000")
	}
	if d.SchemaVersion != 1 {
		return Document{}, fmt.Errorf("unsupported schema_version %d", d.SchemaVersion)
	}
	if d.Instance.ID == "" {
		return Document{}, fmt.Errorf("instance.id required")
	}
	if d.I18n.FallbackLocale == "" {
		d.I18n.FallbackLocale = "en"
	}
	if d.Admin.Web.Bind == "" {
		d.Admin.Web.Bind = "127.0.0.1:8787"
	}
	if len(d.Admin.Web.Hosts) > 8 {
		return Document{}, fmt.Errorf("admin.web.hosts: at most 8 names")
	}
	for i, h := range d.Admin.Web.Hosts {
		n, err := WebHost(h)
		if err != nil {
			return Document{}, err
		}
		d.Admin.Web.Hosts[i] = n
	}
	if d.Discord.CommandPrefix == "" {
		d.Discord.CommandPrefix = "!"
	}
	for name, p := range d.Providers {
		if !slices.Contains(ProviderKinds, p.Kind) {
			return Document{}, fmt.Errorf("providers.%s.kind %q is not one of %v", name, p.Kind, ProviderKinds)
		}
		if p.API != "" && (p.Kind != KindOpenAI || !openai.ValidAPI(p.API)) {
			return Document{}, fmt.Errorf("providers.%s.api %q: only openai_compat providers take one of %v", name, p.API, openai.APIs)
		}
		for family, upstream := range p.ByokOnly {
			if p.Kind != KindOpenAI || !routeSlug.MatchString(family) || !routeSlug.MatchString(upstream) {
				return Document{}, fmt.Errorf("providers.%s.byok_only: %q: %q must map a model family to an OpenRouter provider slug (anthropic: anthropic)", name, family, upstream)
			}
		}
		if p.CredentialRef != "" && !secrets.ValidRef(p.CredentialRef) {
			return Document{}, fmt.Errorf("providers.%s.credential_ref must be %s, never the secret", name, secrets.Hint)
		}
		if p.BaseURL != "" && !p.Disabled { // a disabled provider sends nothing, whatever its address
			if err := tools.ValidateHTTPURL(p.BaseURL); err != nil {
				if problems == nil {
					return Document{}, fmt.Errorf("providers.%s.base_url: %w", name, err)
				}
				p.Disabled = true
				d.Providers[name] = p
				*problems = append(*problems, Problem{Path: "providers." + name + ".base_url", Reason: err.Error(), Off: "providers." + name})
			}
		}
	}
	for i, id := range d.WebSearch.Order {
		if p, ok := d.Providers[id]; !ok || !SearchKind(p.Kind) {
			return Document{}, fmt.Errorf("web_search.order[%d] %q is not a search provider (kinds %v)", i, id, websearch.Kinds)
		}
		if slices.Contains(d.WebSearch.Order[:i], id) {
			return Document{}, fmt.Errorf("web_search.order lists %q twice", id)
		}
	}
	for role, m := range d.Models {
		if !ValidModelRole(role) {
			return Document{}, fmt.Errorf("models.%s is not a known role (use one of %v)", role, AllModelRoles())
		}
		if m.PriceInPerMTok < 0 || m.PriceOutPerMTok < 0 || m.PriceInPerMTok > 1000 || m.PriceOutPerMTok > 1000 || m.PriceCachedInPerMTok < 0 || m.PriceCachedInPerMTok > 1000 {
			return Document{}, fmt.Errorf("models.%s: prices must be 0–1000 USD per million tokens", role)
		}
		if m.TimeoutSeconds != 0 && (m.TimeoutSeconds < 5 || m.TimeoutSeconds > 600) {
			return Document{}, fmt.Errorf("models.%s.timeout_seconds must be 5–600 (or 0 for the default)", role)
		}
		if m.Effort != "" && (!slices.Contains(ModelEfforts, m.Effort) || !slices.Contains(FallbackRoles, role)) {
			return Document{}, fmt.Errorf("models.%s.effort %q: only the chat roles %v take one of %v", role, m.Effort, FallbackRoles, ModelEfforts)
		}
		if m.Provider != "" {
			if _, ok := d.Providers[m.Provider]; !ok {
				return Document{}, fmt.Errorf("models.%s.provider %q is not a configured provider", role, m.Provider)
			}
		}
		if err := validFallbacks(d, role, m); err != nil {
			return Document{}, err
		}
	}
	if d.Memory.Retrieve == "" {
		d.Memory.Retrieve = "off"
	}
	if !slices.Contains(RetrieveModes, d.Memory.Retrieve) {
		return Document{}, fmt.Errorf("memory.retrieve must be one of %v", RetrieveModes)
	}
	if d.Chat.ToolModel != "" && !slices.Contains(ToolModels, d.Chat.ToolModel) {
		return Document{}, fmt.Errorf("chat.tool_model must be one of %v", ToolModels)
	}
	if d.Chat.LoopFinal != "" && !slices.Contains(LoopFinals, d.Chat.LoopFinal) {
		return Document{}, fmt.Errorf("chat.loop_final must be one of %v", LoopFinals)
	}
	if d.Chat.ToolMode != "" && !slices.Contains(ToolModes, d.Chat.ToolMode) {
		return Document{}, fmt.Errorf("chat.tool_mode must be one of %v", ToolModes)
	}
	if d.Chat.Vision != "" && !slices.Contains(VisionModes, d.Chat.Vision) {
		return Document{}, fmt.Errorf("chat.vision must be one of %v", VisionModes)
	}
	if d.Chat.Progress != "" && !slices.Contains(ProgressModes, d.Chat.Progress) {
		return Document{}, fmt.Errorf("chat.progress must be one of %v", ProgressModes)
	}
	// pages a search reads: a request that reads dozens of pages would only slow every search and spend the
	// daily page quota
	if n := d.WebSearch.ReadPages; n != nil && (*n < 0 || *n > 10) {
		return Document{}, fmt.Errorf("web_search.read_pages must be 0–10")
	}
	if n := d.WebSearch.DeepPages; n != nil && (*n < 0 || *n > 10) {
		return Document{}, fmt.Errorf("web_search.deep_pages must be 0–10")
	}
	if d.WebSearch.Fetch != "" && !slices.Contains(FetchKinds, d.WebSearch.Fetch) {
		return Document{}, fmt.Errorf("web_search.fetch must be one of %v", FetchKinds)
	}
	if n := d.WebSearch.FetchDailyLimit; n < 0 || n > 5000 {
		return Document{}, fmt.Errorf("web_search.fetch_daily_limit must be 0–5000 (0 = the default)")
	}
	for _, l := range d.Memory.Languages {
		if _, ok := lang.Get(l); !ok {
			return Document{}, fmt.Errorf("memory.languages: no language pack %q (have %v)", l, lang.Available())
		}
	}
	for id, t := range d.Tools {
		if problems == nil {
			if err := tools.Validate(id, t); err != nil {
				return Document{}, err
			}
			continue
		}
		bad, err := tools.ValidateLenient(id, t)
		if err != nil {
			return Document{}, err
		}
		if bad != nil {
			off := false
			t.Enabled = &off
			d.Tools[id] = t
			*problems = append(*problems, Problem{Path: bad.Path, Reason: bad.Reason.Error(), Off: "tools." + id})
		}
	}
	if len(d.Instance.BotNames) > 8 {
		return Document{}, fmt.Errorf("instance.bot_names: at most 8 names")
	}
	if d.Instance.LogLevel != "" && !slices.Contains(LogLevels, d.Instance.LogLevel) {
		return Document{}, fmt.Errorf("instance.log_level must be one of %v", LogLevels)
	}
	if d.Backup == (Backup{}) {
		d.Backup = DefaultBackup() // documents written before backup existed
	}
	if !slices.Contains(BackupSchedules, d.Backup.Schedule) {
		return Document{}, fmt.Errorf("backup.schedule must be one of %v", BackupSchedules)
	}
	if d.Backup.Hour < 0 || d.Backup.Hour > 23 || d.Backup.Weekday < 0 || d.Backup.Weekday > 6 {
		return Document{}, fmt.Errorf("backup.hour must be 0–23 and backup.weekday 0–6")
	}
	if d.Backup.Keep < 1 || d.Backup.Keep > 100 {
		return Document{}, fmt.Errorf("backup.keep must be 1–100")
	}
	if d.Limits.BudgetAlertPercent < 0 || d.Limits.BudgetAlertPercent > 100 {
		return Document{}, fmt.Errorf("limits.budget_alert_percent must be 0–100")
	}
	if len(d.Memory.PersonAliases) > 500 {
		return Document{}, fmt.Errorf("memory.person_aliases: at most 500 people")
	}
	for k, v := range d.Memory.PersonAliases {
		if strings.TrimSpace(k) == "" || len(v) == 0 || len(v) > 20 {
			return Document{}, fmt.Errorf("memory.person_aliases.%s: a name with 1–20 other names", k)
		}
	}
	if d.Alerts.MinSeverity != "" && !slices.Contains(AlertSeverities, d.Alerts.MinSeverity) {
		return Document{}, fmt.Errorf("alerts.min_severity must be one of %v", AlertSeverities)
	}
	for _, m := range []float64{d.DisasterAlerts.EarthquakeMinDomestic, d.DisasterAlerts.EarthquakeMinOverseas} {
		if m < 0 || m > 10 {
			return Document{}, fmt.Errorf("disaster_alerts earthquake magnitudes must be between 0 and 10")
		}
	}
	for gid, g := range d.Guilds {
		for cid, c := range g.Channels {
			if c.DisasterAlerts != "" && c.DisasterAlerts != "on" && c.DisasterAlerts != "off" {
				return Document{}, fmt.Errorf("guilds.%s.channels.%s.disaster_alerts must be on or off", gid, cid)
			}
		}
	}
	if d.Participation.Mode == "" {
		d.Participation.Mode = "off"
	}
	if !slices.Contains(ParticipationModes, d.Participation.Mode) {
		return Document{}, fmt.Errorf("participation.mode must be one of %v", ParticipationModes)
	}
	for _, v := range []string{d.Participation.QuietStart, d.Participation.QuietEnd} {
		if v != "" && !validHHMM(v) {
			return Document{}, fmt.Errorf("participation.quiet_start and quiet_end are HH:MM")
		}
	}
	if (d.Participation.QuietStart == "") != (d.Participation.QuietEnd == "") {
		return Document{}, fmt.Errorf("participation quiet hours need both quiet_start and quiet_end")
	}
	if d.Memory.DayBoundary != "" && !validHHMM(d.Memory.DayBoundary) {
		return Document{}, fmt.Errorf("memory.day_boundary is HH:MM")
	}
	if v := strings.TrimSpace(d.Memory.DerivedBudgetUSD); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err != nil || f < 0 || f > 1000 {
			return Document{}, fmt.Errorf("memory.derived_budget_usd must be a USD amount from 0 to 1000")
		}
	}
	for key, v := range map[string]string{"memory.episodes": d.Memory.Episodes, "memory.state": d.Memory.State, "memory.cards": d.Memory.Cards, "memory.warm_query": d.Memory.WarmQuery} {
		if v != "" && !slices.Contains(OnOff, v) {
			return Document{}, fmt.Errorf("%s must be on or off", key)
		}
	}
	if d.Memory.LeafSplit != "" && d.Memory.LeafSplit != "time_gap" && d.Memory.LeafSplit != "fixed" {
		return Document{}, fmt.Errorf("memory.leaf_split must be time_gap or fixed")
	}
	if d.Discord.TokenRef != "" && !secrets.ValidRef(d.Discord.TokenRef) {
		return Document{}, fmt.Errorf("discord.token_ref must be %s, never the token", secrets.Hint)
	}
	for guildID, g := range d.Guilds {
		if guildID == "" {
			return Document{}, fmt.Errorf("guilds: empty guild id")
		}
		if c := g.Log.Channel; c != "" && c != "off" && c != GuildLogAuto && !isSnowflake(c) {
			return Document{}, fmt.Errorf("guilds.%s.log.channel must be a channel ID, auto or off", guildID)
		}
		for channelID, ch := range g.Channels {
			if channelID == "" {
				return Document{}, fmt.Errorf("guilds.%s.channels: empty channel id", guildID)
			}
			ch = NormalizeChannelPolicy(ch)
			if !ValidRespond(ch.Respond) {
				return Document{}, fmt.Errorf("guilds.%s.channels.%s.respond must be one of %v", guildID, channelID, RespondModes)
			}
			if !ValidLoggingLevel(ch.Logging.Level) {
				return Document{}, fmt.Errorf("guilds.%s.channels.%s.logging.level must be one of %v", guildID, channelID, LoggingLevels)
			}
			if ch.Participation != "" && !slices.Contains(ParticipationModes, ch.Participation) {
				return Document{}, fmt.Errorf("guilds.%s.channels.%s.participation must be one of %v", guildID, channelID, ParticipationModes)
			}
			for _, c := range []struct {
				key, v  string
				choices []string
			}{{"name_call", ch.NameCall, NameCallModes}, {"attribution", ch.Attribution, AttributionModes},
				{"conversation.follow", ch.Conversation.Follow, FollowModes}, {"conversation.thread", ch.Conversation.Thread, ThreadModes},
				{"conversation.thread_follow", ch.Conversation.ThreadFollow, ThreadFollowModes},
				{"conversation.thread_recap", ch.Conversation.ThreadRecap, ThreadRecapModes},
				{"conversation.reply_shape", ch.Conversation.ReplyShape, ReplyShapes},
				{"conversation.banter", ch.Conversation.Banter, BanterModes},
				{"conversation.room_style", ch.Conversation.RoomStyle, RoomStyleModes}} {
				if !slices.Contains(c.choices, c.v) {
					return Document{}, fmt.Errorf("guilds.%s.channels.%s.%s must be one of %v", guildID, channelID, c.key, c.choices)
				}
			}
			if ch.Conversation.FollowWindowSec < 0 || ch.Conversation.FollowWindowSec > 600 || ch.Conversation.FollowMaxUntagged < 0 ||
				ch.Conversation.FollowMaxUntagged > 100 || ch.Conversation.ThreadIdleCloseMin < 0 || ch.Conversation.ThreadIdleCloseMin > 1440 {
				return Document{}, fmt.Errorf("guilds.%s.channels.%s.conversation: a number is out of range", guildID, channelID)
			}
			if ch.Logging.RetainDays < 0 || ch.Logging.RetainDays > 3650 {
				return Document{}, fmt.Errorf("guilds.%s.channels.%s.logging.retain_days out of range", guildID, channelID)
			}
			for id := range ch.Tools {
				if _, builtin := tools.Lookup(id); !builtin && !d.Tools[id].Custom() {
					return Document{}, fmt.Errorf("guilds.%s.channels.%s.tools: unknown tool %q", guildID, channelID, id)
				}
			}
			seenHash := map[string]bool{}
			for i, att := range ch.RAGAttachments {
				if att.ID == "" || att.ContentHash == "" || att.Filename == "" {
					return Document{}, fmt.Errorf("guilds.%s.channels.%s.rag_attachments[%d] needs id, content_hash, filename", guildID, channelID, i)
				}
				if seenHash[att.ContentHash] {
					return Document{}, fmt.Errorf("guilds.%s.channels.%s.rag_attachments: duplicate content_hash in channel", guildID, channelID)
				}
				seenHash[att.ContentHash] = true
				if att.Purpose == "" {
					att.Purpose = "retrieval_context"
					ch.RAGAttachments[i] = att
				}
			}
			g.Channels[channelID] = ch
		}
		d.Guilds[guildID] = g
	}
	for id, p := range d.Personas {
		if id == "" {
			return Document{}, fmt.Errorf("personas: empty id")
		}
		if p.Label == "" {
			p.Label = id
			d.Personas[id] = p
		}
	}
	return d, nil
}

func Marshal(d Document) ([]byte, error) {
	return yaml.Marshal(d)
}

func Hash(raw []byte) string {
	// Windows editors commonly save YAML with CRLF. Treat line endings as presentation, not as a
	// different configuration revision or recovery hash.
	canonical := bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
	canonical = bytes.ReplaceAll(canonical, []byte("\r"), []byte("\n"))
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

func validHHMM(v string) bool {
	t, err := time.Parse("15:04", v)
	return err == nil && t.Format("15:04") == v
}

func validFallbacks(d Document, role string, m Model) error {
	if len(m.Fallbacks) == 0 {
		return nil
	}
	if !slices.Contains(FallbackRoles, role) {
		return fmt.Errorf("models.%s.fallbacks: only %v can have fallbacks", role, FallbackRoles)
	}
	if len(m.Fallbacks) > MaxFallbacks {
		return fmt.Errorf("models.%s.fallbacks: at most %d", role, MaxFallbacks)
	}
	seen := map[string]bool{m.Provider + "\x00" + m.Model: true}
	for i, f := range m.Fallbacks {
		at := fmt.Sprintf("models.%s.fallbacks[%d]", role, i)
		if strings.TrimSpace(f.Model) == "" {
			return fmt.Errorf("%s.model is required", at)
		}
		p, ok := d.Providers[f.Provider]
		if !ok {
			return fmt.Errorf("%s.provider %q is not a configured provider", at, f.Provider)
		}
		if p.Kind != KindOpenAI {
			return fmt.Errorf("%s.provider %q is not a model provider", at, f.Provider)
		}
		if seen[f.Provider+"\x00"+f.Model] {
			return fmt.Errorf("%s: %s on %s is already in the list", at, f.Model, f.Provider)
		}
		seen[f.Provider+"\x00"+f.Model] = true
		if f.PriceInPerMTok < 0 || f.PriceOutPerMTok < 0 || f.PriceCachedInPerMTok < 0 || f.PriceInPerMTok > 1000 || f.PriceOutPerMTok > 1000 || f.PriceCachedInPerMTok > 1000 {
			return fmt.Errorf("%s: prices must be 0–1000 USD per million tokens", at)
		}
		if f.TimeoutSeconds != 0 && (f.TimeoutSeconds < 5 || f.TimeoutSeconds > 600) {
			return fmt.Errorf("%s.timeout_seconds must be 5–600 (or 0 for the role's default)", at)
		}
		if f.Effort != "" && !slices.Contains(ModelEfforts, f.Effort) {
			return fmt.Errorf("%s.effort %q must be one of %v (or empty for the role's)", at, f.Effort, ModelEfforts)
		}
	}
	return nil
}
