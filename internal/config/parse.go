package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"masamong/internal/lang"
	"masamong/internal/providers/openai"
	"masamong/internal/providers/websearch"
	"masamong/internal/secrets"
	"masamong/internal/tools"
)

// parse reads and validates a configuration. problems is nil for a candidate someone is writing
// (every rule is an error) and non-nil for a committed document (what an earlier release accepted is
// repaired and reported there instead of failing the start). The sections run in a fixed order and
// the first failure is the error: TestParseErrorOrder and TestParseErrorsPinned pin both the order and
// the text.
func parse(raw []byte, problems *[]Problem) (Document, error) {
	d, raw, err := decodeDocument(raw)
	if err != nil {
		return Document{}, err
	}
	p := &parser{d: &d, problems: problems}
	// keys records which keys the file sets explicitly: memory is on by default, so a switch the
	// file does not name gets its default while a value it sets is never changed.
	_ = yaml.Unmarshal(raw, &p.keys)
	p.defaultMemorySwitches()
	for _, section := range []func() error{
		p.dm, p.identity, p.adminWeb, p.discordPrefix, p.providers, p.webSearchOrder, p.models,
		p.chat, p.webSearchLimits, p.memoryLanguages, p.tools, p.instance, p.backup, p.limits,
		p.memoryNumbers, p.personAliases, p.alerts, p.participation, p.memoryFormats, p.discordToken,
		p.guilds, p.personas,
	} {
		if err := section(); err != nil {
			return Document{}, err
		}
	}
	return d, nil
}

// decodeDocument strictly decodes the file. It returns the text that was decoded (JSON input is
// re-encoded as YAML first) so the caller can read which keys the file names.
func decodeDocument(raw []byte) (Document, []byte, error) {
	// JSON input is a subset of YAML, but the YAML decoder refuses JSON's \uXXXX surrogate-pair
	// escapes. Re-encode real JSON as YAML first; something that only looks like JSON flows on to
	// the YAML decoder for its own error.
	if trimmed := bytes.TrimSpace(raw); len(trimmed) > 0 && trimmed[0] == '{' {
		var v any
		if err := json.Unmarshal(trimmed, &v); err == nil {
			// the map keeps only the last of a repeated object key, which the YAML decoder would
			// refuse as a repeated mapping key: refuse it the same way here
			if err := rejectDuplicateJSONKeys(trimmed); err != nil {
				return Document{}, nil, err
			}
			if y, err := yaml.Marshal(v); err == nil {
				raw = y
			}
		}
	}
	var d Document
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&d); err != nil {
		return Document{}, nil, fmt.Errorf("config parse: %w", err)
	}
	return d, raw, nil
}

// rejectDuplicateJSONKeys walks real JSON as tokens, since decoding into a map silently keeps only
// the last of a repeated object key. A repeated key is an error naming the path it was read at.
func rejectDuplicateJSONKeys(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	return scanJSONValue(dec, "")
}

func scanJSONValue(dec *json.Decoder, path string) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for dec.More() {
			k, err := dec.Token()
			if err != nil {
				return err
			}
			key, _ := k.(string)
			p := key
			if path != "" {
				p = path + "." + key
			}
			if seen[key] {
				return fmt.Errorf("config parse: duplicate object key %q", p)
			}
			seen[key] = true
			if err := scanJSONValue(dec, p); err != nil {
				return err
			}
		}
	case '[':
		for dec.More() {
			if err := scanJSONValue(dec, path); err != nil {
				return err
			}
		}
	}
	_, err = dec.Token() // the closing delimiter
	return err
}

// parseKeys are the keys a file names explicitly, for the defaults that depend on absence.
type parseKeys struct {
	Memory map[string]any `yaml:"memory"`
	Chat   map[string]any `yaml:"chat"`
	Backup map[string]any `yaml:"backup"`
	Guilds map[string]struct {
		Channels map[string]map[string]any `yaml:"channels"`
	} `yaml:"guilds"`
}

// parser carries what every section needs: the document being completed, the repair list of a
// committed document, and the keys the file set.
type parser struct {
	d        *Document
	problems *[]Problem
	keys     parseKeys
}

func (p *parser) defaultMemorySwitches() {
	d := p.d
	if _, merged := p.keys.Memory["<<"]; !merged {
		if _, set := p.keys.Memory["capture"]; !set {
			d.Memory.Capture = true
		}
		if _, set := p.keys.Memory["embed"]; !set {
			d.Memory.Embed = true
		}
		if _, set := p.keys.Memory["summaries"]; !set {
			d.Memory.Summaries = true
		}
	}
}

func (p *parser) dm() error {
	d := p.d
	if !slices.Contains(DMAllowModes, d.DM.AllowMode()) {
		return fmt.Errorf("dm.allow must be members, operators or off")
	}
	if err := checkBoundInt(p.problems, "dm.per_user_per_day", "-1 (unlimited), 0 (default), or 1..10000", &d.DM.PerUserPerDay); err != nil {
		return err
	}
	return checkBoundInt(p.problems, "dm.per_day", "-1 (unlimited), 0 (default), or 1..100000", &d.DM.PerDay)
}

func (p *parser) identity() error {
	d := p.d
	if d.SchemaVersion != 1 {
		return fmt.Errorf("unsupported schema_version %d", d.SchemaVersion)
	}
	if d.Instance.ID == "" {
		return fmt.Errorf("instance.id required")
	}
	if d.I18n.FallbackLocale == "" {
		d.I18n.FallbackLocale = "en"
	}
	return nil
}

func (p *parser) adminWeb() error {
	d := p.d
	if d.Admin.Web.Bind == "" {
		d.Admin.Web.Bind = "127.0.0.1:8787"
	}
	if len(d.Admin.Web.Hosts) > 8 {
		return fmt.Errorf("admin.web.hosts: at most 8 names")
	}
	for i, h := range d.Admin.Web.Hosts {
		n, err := WebHost(h)
		if err != nil {
			return err
		}
		d.Admin.Web.Hosts[i] = n
	}
	return nil
}

func (p *parser) discordPrefix() error {
	if p.d.Discord.CommandPrefix == "" {
		p.d.Discord.CommandPrefix = "!"
	}
	return nil
}

func (p *parser) providers() error {
	d := p.d
	for name, pr := range d.Providers {
		if err := p.provider(name, &pr); err != nil {
			return err
		}
	}
	return nil
}

// provider checks one provider. pr is a copy: a committed document whose address the policy now
// refuses gets the provider switched off in the map and a Problem.
func (p *parser) provider(name string, pr *Provider) error {
	if !slices.Contains(ProviderKinds, pr.Kind) {
		return fmt.Errorf("providers.%s.kind %q is not one of %v", name, pr.Kind, ProviderKinds)
	}
	if pr.API != "" && (pr.Kind != KindOpenAI || !openai.ValidAPI(pr.API)) {
		return fmt.Errorf("providers.%s.api %q: only openai_compat providers take one of %v", name, pr.API, openai.APIs)
	}
	for family, upstream := range pr.ByokOnly {
		if pr.Kind != KindOpenAI || !routeSlug.MatchString(family) || !routeSlug.MatchString(upstream) {
			return fmt.Errorf("providers.%s.byok_only: %q: %q must map a model family to an OpenRouter provider slug (anthropic: anthropic)", name, family, upstream)
		}
	}
	if pr.CredentialRef != "" && !secrets.ValidRef(pr.CredentialRef) {
		return fmt.Errorf("providers.%s.credential_ref must be %s, never the secret", name, secrets.Hint)
	}
	if pr.BaseURL != "" && !pr.Disabled { // a disabled provider sends nothing, whatever its address
		if err := tools.ValidateHTTPURL(pr.BaseURL); err != nil {
			if p.problems == nil {
				return fmt.Errorf("providers.%s.base_url: %w", name, err)
			}
			pr.Disabled = true
			p.d.Providers[name] = *pr
			*p.problems = append(*p.problems, Problem{Path: "providers." + name + ".base_url", Reason: err.Error(), Off: "providers." + name})
		}
	}
	return nil
}

func (p *parser) webSearchOrder() error {
	d := p.d
	for i, id := range d.WebSearch.Order {
		if pr, ok := d.Providers[id]; !ok || !SearchKind(pr.Kind) {
			return fmt.Errorf("web_search.order[%d] %q is not a search provider (kinds %v)", i, id, websearch.Kinds)
		}
		if slices.Contains(d.WebSearch.Order[:i], id) {
			return fmt.Errorf("web_search.order lists %q twice", id)
		}
	}
	return nil
}

func (p *parser) models() error {
	d := p.d
	for role, m := range d.Models {
		if !ValidModelRole(role) {
			return fmt.Errorf("models.%s is not a known role (use one of %v)", role, AllModelRoles())
		}
		if m.PriceInPerMTok < 0 || m.PriceOutPerMTok < 0 || m.PriceInPerMTok > 1000 || m.PriceOutPerMTok > 1000 || m.PriceCachedInPerMTok < 0 || m.PriceCachedInPerMTok > 1000 {
			return fmt.Errorf("models.%s: prices must be 0–1000 USD per million tokens", role)
		}
		if m.TimeoutSeconds != 0 && (m.TimeoutSeconds < 5 || m.TimeoutSeconds > 600) {
			return fmt.Errorf("models.%s.timeout_seconds must be 5–600 (or 0 for the default)", role)
		}
		if m.Effort != "" && (!slices.Contains(ModelEfforts, m.Effort) || !slices.Contains(FallbackRoles, role)) {
			return fmt.Errorf("models.%s.effort %q: only the chat roles %v take one of %v", role, m.Effort, FallbackRoles, ModelEfforts)
		}
		if m.Provider != "" {
			if _, ok := d.Providers[m.Provider]; !ok {
				return fmt.Errorf("models.%s.provider %q is not a configured provider", role, m.Provider)
			}
		}
		if err := validFallbacks(*d, role, m); err != nil {
			return err
		}
	}
	return nil
}

// chat checks memory.retrieve and the chat switches; transcript_chars is checked only when the file
// writes it.
func (p *parser) chat() error {
	d := p.d
	if d.Memory.Retrieve == "" {
		d.Memory.Retrieve = "auto"
	}
	if !slices.Contains(RetrieveModes, d.Memory.Retrieve) {
		return fmt.Errorf("memory.retrieve must be one of %v", RetrieveModes)
	}
	if d.Chat.ToolModel != "" && !slices.Contains(ToolModels, d.Chat.ToolModel) {
		return fmt.Errorf("chat.tool_model must be one of %v", ToolModels)
	}
	if d.Chat.LoopFinal != "" && !slices.Contains(LoopFinals, d.Chat.LoopFinal) {
		return fmt.Errorf("chat.loop_final must be one of %v", LoopFinals)
	}
	if d.Chat.ToolMode != "" && !slices.Contains(ToolModes, d.Chat.ToolMode) {
		return fmt.Errorf("chat.tool_mode must be one of %v", ToolModes)
	}
	if d.Chat.Vision != "" && !slices.Contains(VisionModes, d.Chat.Vision) {
		return fmt.Errorf("chat.vision must be one of %v", VisionModes)
	}
	if d.Chat.Progress != "" && !slices.Contains(ProgressModes, d.Chat.Progress) {
		return fmt.Errorf("chat.progress must be one of %v", ProgressModes)
	}
	// a key written at all must be an integer 1000–20000; absence means the default
	// (TranscriptChars.UnmarshalYAML refuses non-integers before this runs)
	if _, set := p.keys.Chat["transcript_chars"]; set {
		return checkBoundInt(p.problems, "chat.transcript_chars", "an integer 1000–20000 (unset means the default 5000)", (*int)(&d.Chat.TranscriptChars))
	}
	return nil
}

func (p *parser) webSearchLimits() error {
	d := p.d
	// pages a search reads: a request that reads dozens of pages would only slow every search and spend the
	// daily page quota
	if n := d.WebSearch.ReadPages; n != nil {
		if err := checkBoundInt(p.problems, "web_search.read_pages", "0–10", n); err != nil {
			return err
		}
	}
	if n := d.WebSearch.DeepPages; n != nil {
		if err := checkBoundInt(p.problems, "web_search.deep_pages", "0–10", n); err != nil {
			return err
		}
	}
	if d.WebSearch.Fetch != "" && !slices.Contains(FetchKinds, d.WebSearch.Fetch) {
		return fmt.Errorf("web_search.fetch must be one of %v", FetchKinds)
	}
	return checkBoundInt(p.problems, "web_search.fetch_daily_limit", "0–5000 (0 = the default)", &d.WebSearch.FetchDailyLimit)
}

func (p *parser) memoryLanguages() error {
	for _, l := range p.d.Memory.Languages {
		if _, ok := lang.Get(l); !ok {
			return fmt.Errorf("memory.languages: no language pack %q (have %v)", l, lang.Available())
		}
	}
	return nil
}

// tools checks every tool entry. A committed document gets an entry whose address the policy now
// refuses switched off and reported instead.
func (p *parser) tools() error {
	d := p.d
	for id, t := range d.Tools {
		if p.problems == nil {
			if err := tools.Validate(id, t); err != nil {
				return err
			}
			continue
		}
		bad, err := tools.ValidateLenient(id, t)
		if err != nil {
			return err
		}
		if bad != nil {
			off := false
			t.Enabled = &off
			d.Tools[id] = t
			*p.problems = append(*p.problems, Problem{Path: bad.Path, Reason: bad.Reason.Error(), Off: "tools." + id})
		}
	}
	return nil
}

func (p *parser) instance() error {
	d := p.d
	if len(d.Instance.BotNames) > 8 {
		return fmt.Errorf("instance.bot_names: at most 8 names")
	}
	if d.Instance.LogLevel != "" && !slices.Contains(LogLevels, d.Instance.LogLevel) {
		return fmt.Errorf("instance.log_level must be one of %v", LogLevels)
	}
	return nil
}

func (p *parser) backup() error {
	d := p.d
	def := DefaultBackup()
	// A key the file does not write gets the default: a map holding only `keep: 0` used to read as
	// an absent section, so the explicit 0 never reached its bound. A merge key supplies values the
	// key scan cannot see, so a merged section is checked as decoded (the all-zero read of it still
	// counts as absent, like before).
	_, merged := p.keys.Backup["<<"]
	onlyMerge := merged && len(p.keys.Backup) == 1
	if merged && d.Backup == (Backup{}) {
		merged = false
	}
	if !merged && (len(p.keys.Backup) == 0 || onlyMerge) && d.Backup == (Backup{}) {
		d.Backup = def // documents written before backup existed
	} else if !merged {
		// a section that names some keys keeps the decoded hour and weekday as before (an omitted hour
		// is midnight, an omitted weekday Sunday): defaulting them now would move existing schedules
		if d.Backup.Schedule == "" {
			d.Backup.Schedule = def.Schedule
		}
		if _, set := p.keys.Backup["keep"]; !set {
			d.Backup.Keep = def.Keep
		}
	}
	if !slices.Contains(BackupSchedules, d.Backup.Schedule) {
		return fmt.Errorf("backup.schedule must be one of %v", BackupSchedules)
	}
	if err := checkBoundInt(p.problems, "backup.hour", "0–23", &d.Backup.Hour); err != nil {
		return err
	}
	if err := checkBoundInt(p.problems, "backup.weekday", "0–6", &d.Backup.Weekday); err != nil {
		return err
	}
	return checkBoundInt(p.problems, "backup.keep", "1–365", &d.Backup.Keep)
}

func (p *parser) limits() error {
	d := p.d
	if err := checkBoundInt(p.problems, "limits.budget_alert_percent", "0–100 (0 = the default)", &d.Limits.BudgetAlertPercent); err != nil {
		return err
	}
	if err := checkBoundInt(p.problems, "limits.max_concurrent_turns", "0–64 (0 = the default)", &d.Limits.MaxConcurrentTurns); err != nil {
		return err
	}
	return checkBoundInt(p.problems, "limits.max_external_attempts", "0–20 (0 = no cap)", &d.Limits.MaxExternalAttempts)
}

func (p *parser) memoryNumbers() error {
	d := p.d
	if err := checkBoundInt(p.problems, "memory.backfill_days", "-1–30 (-1 = off, 0 = the default)", &d.Memory.BackfillDays); err != nil {
		return err
	}
	// index_interval_min: an absent key decodes to 0 (the runtime applies its own floor), so only a
	// written value inside or outside the documented range is checked — a written 0 included
	if _, set := p.keys.Memory["index_interval_min"]; set || d.Memory.IndexIntervalMin != 0 {
		if err := checkBoundInt(p.problems, "memory.index_interval_min", "1–1440", &d.Memory.IndexIntervalMin); err != nil {
			return err
		}
	}
	return checkBoundInt(p.problems, "memory.index_workers", "0–256 (0 = the default)", &d.Memory.IndexWorkers)
}

func (p *parser) personAliases() error {
	d := p.d
	if len(d.Memory.PersonAliases) > 500 {
		return fmt.Errorf("memory.person_aliases: at most 500 people")
	}
	for k, v := range d.Memory.PersonAliases {
		if strings.TrimSpace(k) == "" || len(v) == 0 || len(v) > 20 {
			return fmt.Errorf("memory.person_aliases.%s: a name with 1–20 other names", k)
		}
	}
	return nil
}

// alerts checks the alert switches, including the per-room ones a channel carries.
func (p *parser) alerts() error {
	d := p.d
	if d.Alerts.MinSeverity != "" && !slices.Contains(AlertSeverities, d.Alerts.MinSeverity) {
		return fmt.Errorf("alerts.min_severity must be one of %v", AlertSeverities)
	}
	for _, m := range []float64{d.DisasterAlerts.EarthquakeMinDomestic, d.DisasterAlerts.EarthquakeMinOverseas} {
		if m < 0 || m > 10 {
			return fmt.Errorf("disaster_alerts earthquake magnitudes must be between 0 and 10")
		}
	}
	for gid, g := range d.Guilds {
		for cid, c := range g.Channels {
			if err := channelAlerts(fmt.Sprintf("guilds.%s.channels.%s", gid, cid), c); err != nil {
				return err
			}
		}
	}
	return nil
}

func channelAlerts(at string, c ChannelPolicy) error {
	if c.DisasterAlerts != "" && c.DisasterAlerts != "on" && c.DisasterAlerts != "off" {
		return fmt.Errorf("%s.disaster_alerts must be on or off", at)
	}
	if c.UpdateNews != "" && c.UpdateNews != "on" && c.UpdateNews != "off" {
		return fmt.Errorf("%s.update_news must be on or off", at)
	}
	for region, level := range c.WatchLevel {
		if region == "" || strings.TrimSpace(region) != region || level < 1 || level > 5 {
			return fmt.Errorf("%s.watch_level requires a region code or name and level 1–5, got %q=%d", at, region, level)
		}
	}
	return nil
}

func (p *parser) participation() error {
	d := p.d
	if d.Participation.Mode == "" {
		d.Participation.Mode = "off"
	}
	if !slices.Contains(ParticipationModes, d.Participation.Mode) {
		return fmt.Errorf("participation.mode must be one of %v", ParticipationModes)
	}
	if err := checkBound(p.problems, "participation.min_score", "0–1 (0 = the default)", d.Participation.MinScore, func(v float64) { d.Participation.MinScore = v }); err != nil {
		return err
	}
	for _, v := range []string{d.Participation.QuietStart, d.Participation.QuietEnd} {
		if v != "" && !validHHMM(v) {
			return fmt.Errorf("participation.quiet_start and quiet_end are HH:MM")
		}
	}
	if (d.Participation.QuietStart == "") != (d.Participation.QuietEnd == "") {
		return fmt.Errorf("participation quiet hours need both quiet_start and quiet_end")
	}
	return nil
}

func (p *parser) memoryFormats() error {
	d := p.d
	if d.Memory.DayBoundary != "" && !validHHMM(d.Memory.DayBoundary) {
		return fmt.Errorf("memory.day_boundary is HH:MM")
	}
	if v := strings.TrimSpace(d.Memory.DerivedBudgetUSD); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err != nil || f < 0 || f > 1000 {
			return fmt.Errorf("memory.derived_budget_usd must be a USD amount from 0 to 1000")
		}
	}
	for key, v := range map[string]string{"memory.episodes": d.Memory.Episodes, "memory.state": d.Memory.State, "memory.cards": d.Memory.Cards, "memory.people": d.Memory.People, "memory.warm_query": d.Memory.WarmQuery} {
		if v != "" && !slices.Contains(OnOff, v) {
			return fmt.Errorf("%s must be on or off", key)
		}
	}
	if d.Memory.LeafSplit != "" && !slices.Contains(LeafSplitModes, d.Memory.LeafSplit) {
		return fmt.Errorf("memory.leaf_split must be time_gap or fixed")
	}
	return nil
}

func (p *parser) discordToken() error {
	if t := p.d.Discord.TokenRef; t != "" && !secrets.ValidRef(t) {
		return fmt.Errorf("discord.token_ref must be %s, never the token", secrets.Hint)
	}
	return nil
}

func (p *parser) guilds() error {
	d := p.d
	for guildID, g := range d.Guilds {
		if guildID == "" {
			return fmt.Errorf("guilds: empty guild id")
		}
		if c := g.Log.Channel; c != "" && c != "off" && c != GuildLogAuto && !isSnowflake(c) {
			return fmt.Errorf("guilds.%s.log.channel must be a channel ID, auto or off", guildID)
		}
		for channelID, ch := range g.Channels {
			if channelID == "" {
				return fmt.Errorf("guilds.%s.channels: empty channel id", guildID)
			}
			ch, err := p.channel(guildID, channelID, ch)
			if err != nil {
				return err
			}
			g.Channels[channelID] = ch
		}
		d.Guilds[guildID] = g
	}
	return nil
}

// channel validates and normalizes one room's policy.
func (p *parser) channel(guildID, channelID string, ch ChannelPolicy) (ChannelPolicy, error) {
	at := "guilds." + guildID + ".channels." + channelID
	// an explicit capture (or a YAML merge, whose decoded value is kept) is never defaulted
	if chMap, ok := p.keys.Guilds[guildID].Channels[channelID]; ok {
		_, set := chMap["capture"]
		_, merged := chMap["<<"]
		ch.captureSet = set || merged
	}
	ch = NormalizeChannelPolicy(ch)
	if err := p.channelChoices(at, &ch); err != nil {
		return ch, err
	}
	if err := p.channelRanges(at, ch); err != nil {
		return ch, err
	}
	atts, err := p.channelAttachments(at, ch)
	if err != nil {
		return ch, err
	}
	ch.RAGAttachments = atts
	return ch, nil
}

// channelChoices checks the room's closed value sets. A committed document's retired follow mode
// reads as off.
func (p *parser) channelChoices(at string, ch *ChannelPolicy) error {
	if !ValidRespond(ch.Respond) {
		return fmt.Errorf("%s.respond must be one of %v", at, RespondModes)
	}
	if !ValidLoggingLevel(ch.Logging.Level) {
		return fmt.Errorf("%s.logging.level must be one of %v", at, LoggingLevels)
	}
	if ch.Participation != "" && !slices.Contains(ParticipationModes, ch.Participation) {
		return fmt.Errorf("%s.participation must be one of %v", at, ParticipationModes)
	}
	if ch.Conversation.Follow == "reply_only" {
		// reply_only was retired: a stored value is read as off, a new write is rejected
		if p.problems == nil {
			return fmt.Errorf("%s.conversation.follow must be one of %v", at, FollowModes)
		}
		ch.Conversation.Follow = "off"
		*p.problems = append(*p.problems, Problem{
			Path:   at + ".conversation.follow",
			Reason: "reply_only is retired; the stored value is read as off",
			Off:    at + ".conversation.follow"})
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
			return fmt.Errorf("%s.%s must be one of %v", at, c.key, c.choices)
		}
	}
	return nil
}

// channelRanges checks the room's numbers and the tools it names.
func (p *parser) channelRanges(at string, ch ChannelPolicy) error {
	if ch.Conversation.FollowWindowSec < 0 || ch.Conversation.FollowWindowSec > 600 || ch.Conversation.FollowMaxUntagged < 0 ||
		ch.Conversation.FollowMaxUntagged > 100 || ch.Conversation.ThreadIdleCloseMin < 0 || ch.Conversation.ThreadIdleCloseMin > 1440 {
		return fmt.Errorf("%s.conversation: a number is out of range", at)
	}
	if ch.Logging.RetainDays < 0 || ch.Logging.RetainDays > 3650 {
		return fmt.Errorf("%s.logging.retain_days out of range", at)
	}
	for id := range ch.Tools {
		if _, builtin := tools.Lookup(id); !builtin && !p.d.Tools[id].Custom() {
			return fmt.Errorf("%s.tools: unknown tool %q", at, id)
		}
	}
	return nil
}

// channelAttachments checks the room's attached files and returns the list to keep. A committed
// document drops an attachment whose stored name is not a generated one and reports it.
func (p *parser) channelAttachments(at string, ch ChannelPolicy) ([]RAGAttachmentRef, error) {
	seenHash := map[string]bool{}
	atts := ch.RAGAttachments[:0]
	for i, att := range ch.RAGAttachments {
		if att.ID == "" || att.ContentHash == "" || att.Filename == "" {
			return nil, fmt.Errorf("%s.rag_attachments[%d] needs id, content_hash, filename", at, i)
		}
		// stored_name names the staged file the attachment delete path joins to the channel
		// RAG dir; only the generated form (<sha256 hex><extension>) is safe to delete. A
		// candidate with anything else is rejected; a committed document drops the
		// attachment and reports it instead of failing to load.
		if att.StoredName != "" && !storedNameForm.MatchString(att.StoredName) {
			if p.problems == nil {
				return nil, fmt.Errorf("%s.rag_attachments[%d].stored_name is not a generated name", at, i)
			}
			*p.problems = append(*p.problems, Problem{
				Path:   fmt.Sprintf("%s.rag_attachments[%d].stored_name", at, i),
				Reason: "is not a generated name; the attachment was dropped",
				Off:    fmt.Sprintf("%s.rag_attachments[%d]", at, i)})
			continue
		}
		if seenHash[att.ContentHash] {
			return nil, fmt.Errorf("%s.rag_attachments: duplicate content_hash in channel", at)
		}
		seenHash[att.ContentHash] = true
		if att.Purpose == "" {
			att.Purpose = "retrieval_context"
			ch.RAGAttachments[i] = att
		}
		atts = append(atts, att)
	}
	return atts, nil
}

func (p *parser) personas() error {
	d := p.d
	for id, pe := range d.Personas {
		if id == "" {
			return fmt.Errorf("personas: empty id")
		}
		if pe.Label == "" {
			pe.Label = id
			d.Personas[id] = pe
		}
	}
	return nil
}
