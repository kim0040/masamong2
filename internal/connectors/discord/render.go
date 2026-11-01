package discord

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"

	"masamong/internal/action"
)

// noMentions is applied to every outbound message: the bot never pings users or roles (07장).
var noMentions = &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}

// Card colors. Each keeps about 3:1 against both Discord themes' embed backgrounds (#2B2D31, #F2F3F5),
// so the bar shows in dark and light mode alike (docs/plan-discord-experience.md §3.3).
const (
	colorOK         = 0x2E9E5B
	colorWarn       = 0xD9700A
	colorError      = 0xE5484D
	colorNeutral    = 0x76808F
	colorFortune    = 0x8E63F0
	colorSchool     = 0x3B82F6
	colorTransfer   = 0x0E8F8F
	colorReminder   = 0x5B7CFA
	colorEarthquake = 0xE8590C
)

// toneColors maps action.Card.Tone to a color; an empty or unknown tone is colorOK.
var toneColors = map[string]int{"ok": colorOK, "warn": colorWarn, "error": colorError, "neutral": colorNeutral,
	"fortune": colorFortune, "school": colorSchool, "transfer": colorTransfer, "reminder": colorReminder, "quake": colorEarthquake}

// Rendered is a transport-neutral message: tests assert on it without a Discord session.
type Rendered struct {
	Embed      *discordgo.MessageEmbed
	More       []*discordgo.MessageEmbed // continuation of a long card (same message)
	Components []discordgo.MessageComponent
	Poll       *discordgo.Poll
	Ephemeral  bool
}

// Render turns an Action result into an embed. Every branch says where data came from and whether it is
// live; fixture or unconfigured outputs are never dressed up as real observations.
// locale picks the catalog language (discord.render.* keys); unknown locales fall back to English.
func Render(res action.Result, cmd Command, locale string) Rendered {
	t := func(key string, args ...any) string { return messages.T(locale, "discord.render."+key, args...) }
	if !res.OK {
		e := &discordgo.MessageEmbed{Color: colorError, Title: t("failed_title", "command", LocalName(cmd, locale))}
		msg := messages.T(locale, "generic.error")
		if res.Error != nil {
			if messages.Has(res.Error.MessageKey) {
				args, dropped := errorMessageArgs(res.Error.Args)
				// a sentence whose subject was withheld ("no such pair {pair}") would read as broken
				// text: the generic line says less but is true
				if !needsAny(locale, res.Error.MessageKey, dropped) {
					msg = messages.T(locale, res.Error.MessageKey, args...)
				}
			}
			if res.Error.Retryable {
				msg += "\n" + t("retry")
			}
		}
		e.Description = truncate(msg, 1000)
		return Rendered{Embed: e, Ephemeral: true}
	}
	out := res.Output
	if c, ok := out["card"].(action.Card); ok {
		return renderCard(c, cmd.Ephemeral)
	}
	status, _ := out["status"].(string)
	live, _ := out["live"].(bool)
	e := &discordgo.MessageEmbed{Color: colorOK}
	switch res.Action {
	case "weather.query":
		region := fmt.Sprint(out["region"])
		e.Title = t("weather.title", "region", region)
		switch status {
		case "unconfigured":
			e.Color, e.Description = colorWarn, t("weather.unconfigured")
		case "empty":
			e.Color, e.Description = colorWarn, t("weather.empty")
		default:
			var lines []string
			if c, _ := out["condition"].(string); c != "" {
				lines = append(lines, "🌤 "+t("weather.cond."+c))
			}
			if v, ok := out["temp_c"]; ok && v != nil {
				line := fmt.Sprintf("🌡 %s %.1f°C", t("weather.temperature"), toF(v))
				if f, ok := out["feels_like_c"]; ok && f != nil {
					line += fmt.Sprintf(" (%s %.1f°C)", t("weather.feels"), toF(f))
				}
				lines = append(lines, line)
			}
			if v, ok := out["humidity_pct"]; ok && v != nil {
				lines = append(lines, fmt.Sprintf("💧 %s %.0f%%", t("weather.humidity"), toF(v)))
			}
			if v, ok := out["wind_ms"]; ok && v != nil {
				lines = append(lines, fmt.Sprintf("🍃 %s %.1f m/s", t("weather.wind"), toF(v)))
			}
			if p, _ := out["precip"].(string); p != "" && p != "none" && p != "unknown" {
				lines = append(lines, "☔ "+t("weather.precipitation", "value", precipText(p, locale)))
			}
			if fc, ok := out["forecast"].(map[string]any); ok {
				var f []string
				if fc["max_c"] != nil && fc["min_c"] != nil {
					f = append(f, t("weather.range", "max", fmt.Sprintf("%.0f", toF(fc["max_c"])), "min", fmt.Sprintf("%.0f", toF(fc["min_c"]))))
				}
				if fc["precip_prob_pct"] != nil {
					f = append(f, t("weather.rain_chance", "value", fmt.Sprintf("%.0f", toF(fc["precip_prob_pct"]))))
				}
				if c, _ := fc["condition"].(string); c != "" {
					f = append([]string{t("weather.cond." + c)}, f...)
				}
				if len(f) > 0 {
					title := "📅 " + strings.Join(f, " · ")
					if day, _ := out["day_offset"].(int); day > 0 {
						title = "📅 " + t("weather.forecast_title", "date", fmt.Sprint(fc["date"])) + " — " + strings.Join(f, " · ")
					}
					lines = append(lines, title)
				}
			}
			e.Description = strings.Join(lines, "\n")
			if s, _ := out["source"].(string); s == "fixture" {
				e.Color = colorNeutral
				e.Description += "\n\n" + t("weather.sample")
			}
		}
		if at, _ := out["observed_at"].(string); at != "" && live {
			// Timestamp markdown is parsed in description/fields, not footers.
			e.Fields = append(e.Fields, &discordgo.MessageEmbedField{
				Name: t("weather.observed"), Value: discordTime(at), Inline: true,
			})
			footer := "KMA"
			if src, _ := out["source"].(string); src == "open_meteo" {
				footer = "Open-Meteo.com · CC BY 4.0" // the free API asks for credit under CC BY 4.0
			}
			e.Footer = &discordgo.MessageEmbedFooter{Text: footer}
		}
	case "disasters.query":
		e.Title = t("alerts.title")
		switch status {
		case "unconfigured":
			e.Color, e.Description = colorWarn, t("alerts.unconfigured")
		case "unconfirmed":
			e.Color, e.Description = colorWarn, t("alerts.unconfirmed")
		case "none":
			e.Description = t("alerts.none")
		default:
			e.Color = colorWarn
			var lines []string
			if ws, ok := out["warnings"].([]any); ok {
				for i, w := range ws {
					if i >= 8 {
						lines = append(lines, t("alerts.more", "n", len(ws)-8))
						break
					}
					m, _ := w.(map[string]any)
					lines = append(lines, fmt.Sprintf("⚠️ %v: %v %v", m["region"], m["kind"], m["level"]))
				}
			}
			if qs, ok := out["earthquakes"].([]any); ok {
				for i, q := range qs {
					if i >= 5 {
						break
					}
					m, _ := q.(map[string]any)
					mag := "?"
					if v, ok := m["magnitude"]; ok && v != nil {
						mag = fmt.Sprintf("%.1f", toF(v))
					}
					when := discordTime(fmt.Sprint(m["time"]))
					lines = append(lines, fmt.Sprintf("🌐 M%s · %v · %s", mag, m["location"], when))
				}
			}
			e.Description = strings.Join(lines, "\n")
		}
	case "finance.quote":
		sym := fmt.Sprint(out["symbol"])
		e.Title = sym
		if status == "unconfigured" {
			e.Color, e.Description = colorWarn, t("quote.unconfigured")
			break
		}
		e.Description = fmt.Sprintf("%v %v (%v%%)", out["price"], out["currency"], out["change_pct"])
		if krw, ok := out["price_krw"]; ok {
			e.Description += fmt.Sprintf("\n≈ ₩%v", krw)
		}
		if status == "partial" {
			e.Color = colorWarn
			e.Description += "\n" + t("quote.fx_missing")
		}
		if at, _ := out["at"].(string); at != "" {
			e.Fields = []*discordgo.MessageEmbedField{{Name: t("quote.as_of"), Value: discordTime(at), Inline: true}}
			e.Footer = &discordgo.MessageEmbedFooter{Text: "Finnhub"}
		}
	case "finance.fx":
		e.Title = t("fx.title", "base", fmt.Sprint(out["base"]))
		if status == "unconfigured" {
			e.Color, e.Description = colorWarn, t("fx.unconfigured")
			break
		}
		if rates, ok := out["rates"].(map[string]any); ok {
			keys := make([]string, 0, len(rates))
			for k := range rates {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			var lines []string
			for _, k := range keys {
				lines = append(lines, fmt.Sprintf("%s %.4g", k, toF(rates[k])))
			}
			e.Description = strings.Join(lines, "\n")
		}
		if at, _ := out["updated_at"].(string); at != "" {
			e.Fields = []*discordgo.MessageEmbedField{{Name: t("fx.updated"), Value: discordTime(at), Inline: true}}
			e.Footer = &discordgo.MessageEmbedFooter{Text: fmt.Sprint(out["source"])}
		}
	case "search.web":
		e.Title = t("search.title", "query", fmt.Sprint(out["query"]))
		if status == "unconfigured" {
			e.Color, e.Description = colorWarn, t("search.unconfigured")
			break
		}
		results, _ := out["results"].([]any)
		if len(results) == 0 {
			e.Description = t("search.none")
			break
		}
		var row discordgo.ActionsRow
		for i, r := range results {
			if i >= 5 {
				break
			}
			m, _ := r.(map[string]any)
			title := truncate(fmt.Sprint(m["title"]), 256)
			url := strings.TrimSpace(fmt.Sprint(m["url"]))
			e.Fields = append(e.Fields, &discordgo.MessageEmbedField{
				Name:  title,
				Value: truncate(fmt.Sprint(m["snippet"]), 1024),
			})
			if url != "" && strings.HasPrefix(url, "http") && len(row.Components) < 5 {
				row.Components = append(row.Components, discordgo.Button{
					Label: truncate(title, 80),
					Style: discordgo.LinkButton,
					URL:   url,
				})
			}
		}
		if len(row.Components) > 0 {
			return Rendered{Embed: e, Components: []discordgo.MessageComponent{row}, Ephemeral: cmd.Ephemeral}
		}
	case "activity.ranking":
		e.Title = t("ranking.title." + fmt.Sprint(out["period"]))
		top, _ := out["top"].([]any)
		if len(top) == 0 {
			if l, ok := out["top"].([]map[string]any); ok {
				for _, m := range l {
					top = append(top, m)
				}
			}
		}
		if len(top) == 0 {
			e.Description = t("ranking.none")
			break
		}
		medals := []string{"🥇", "🥈", "🥉"}
		var lines []string
		for i, r := range top {
			m, _ := r.(map[string]any)
			mark := fmt.Sprintf("%d.", i+1)
			if i < len(medals) {
				mark = medals[i]
			}
			lines = append(lines, t("ranking.line", "mark", mark, "name", fmt.Sprint(m["name"]), "count", fmt.Sprint(m["count"]), "share", fmt.Sprint(m["share"])))
		}
		e.Description = strings.Join(lines, "\n")
		e.Footer = &discordgo.MessageEmbedFooter{Text: t("ranking.footer", "total", fmt.Sprint(out["total_messages"]), "people", fmt.Sprint(out["people"]))}
	case "places.search":
		e.Title = t("places.title", "query", fmt.Sprint(out["query"]))
		if status == "unconfigured" {
			e.Color, e.Description = colorWarn, t("places.unconfigured")
			break
		}
		places, _ := out["places"].([]map[string]any)
		if places == nil {
			for _, p := range func() []any { l, _ := out["places"].([]any); return l }() {
				if m, ok := p.(map[string]any); ok {
					places = append(places, m)
				}
			}
		}
		if len(places) == 0 {
			e.Description = t("places.none")
			break
		}
		var row discordgo.ActionsRow
		for i, m := range places {
			if i >= 5 {
				break
			}
			name := truncate(fmt.Sprint(m["name"]), 256)
			lines := []string{fmt.Sprint(m["address"])}
			if c, _ := m["category"].(string); c != "" {
				lines = append(lines, c)
			}
			if ph, _ := m["phone"].(string); ph != "" {
				lines = append(lines, ph)
			}
			e.Fields = append(e.Fields, &discordgo.MessageEmbedField{Name: name, Value: truncate(strings.Join(lines, "\n"), 1024)})
			if u, _ := m["url"].(string); strings.HasPrefix(u, "http") && len(row.Components) < 5 {
				row.Components = append(row.Components, discordgo.Button{Label: truncate(name, 80), Style: discordgo.LinkButton, URL: u})
			}
		}
		if len(row.Components) > 0 {
			return Rendered{Embed: e, Components: []discordgo.MessageComponent{row}, Ephemeral: cmd.Ephemeral}
		}
	case "polls.create":
		p := nativeDiscordPoll(out, locale)
		e.Title = t("poll.posted")
		e.Description = t("poll.how")
		if closes, _ := out["closes_at"].(string); closes != "" {
			e.Fields = []*discordgo.MessageEmbedField{{Name: t("poll.closes"), Value: discordTime(closes), Inline: true}}
		}
		e.Footer = &discordgo.MessageEmbedFooter{Text: t("poll.label", "id", out["id"])}
		return Rendered{Embed: e, Poll: p}
	case "polls.vote":
		e.Title = t("poll.voted")
		e.Description = t("poll.revision", "n", out["revision"])
		return Rendered{Embed: e, Ephemeral: true}
	case "polls.close":
		e.Title = t("poll.closed")
		e.Color = colorNeutral
	case "polls.result":
		e.Title = "📊 " + fmt.Sprint(out["question"])
		by, _ := out["by_option"].([]any)
		var lines []string
		for _, row := range by {
			m, _ := row.(map[string]any)
			label := fmt.Sprint(m["label"])
			if label == "" || label == "<nil>" {
				label = fmt.Sprint(m["option_id"])
			}
			lines = append(lines, fmt.Sprintf("%s — %v", label, toInt(m["votes"])))
		}
		lines = append(lines, "", t("poll.voters", "n", out["unique_voters"], "status", pollStatus(fmt.Sprint(out["status"]), locale)))
		if closes, _ := out["closes_at"].(string); closes != "" {
			lines = append(lines, t("poll.closes_at", "when", discordTime(closes)))
		}
		e.Description = strings.Join(lines, "\n")
	case "runtime.doctor":
		e.Title = t("status.title")
		feats, _ := out["features"].(map[string]any)
		keys := make([]string, 0, len(feats))
		for k := range feats {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var lines []string
		for _, k := range keys {
			m, _ := feats[k].(map[string]any)
			name := k
			if messages.Has("feature.name." + k) {
				name = messages.T(locale, "feature.name."+k)
			}
			line := fmt.Sprintf("%s %s", statusIcon(fmt.Sprint(m["status"])), name)
			if why, _ := m["reason_text"].(string); why != "" && m["status"] != "ready" {
				line += " — " + why
			}
			lines = append(lines, line)
		}
		e.Description = strings.Join(lines, "\n")
		e.Footer = &discordgo.MessageEmbedFooter{Text: fmt.Sprintf("v%v · rev %v", out["version"], out["revision"])}
		return Rendered{Embed: e, Ephemeral: true}
	case "images.generate":
		e.Title = t("image.title")
		if status == "not_implemented" || status == "unconfigured" {
			e.Color, e.Description = colorWarn, t("image.unconfigured")
			break
		}
		e.Description = truncate(fmt.Sprint(out["url"]), 1000)
	case "help.version":
		return RenderHelp(locale, nil, nil)
	default:
		e.Title = res.Action
		e.Description = truncate(fmt.Sprint(out), 1000)
	}
	return Rendered{Embed: e, Ephemeral: cmd.Ephemeral}
}

// RenderHelp renders only the command surfaces supplied by the live gateway plus the message menu
// action that is always registered.
func RenderHelp(locale string, slash, prefix []string) Rendered {
	t := func(key string) string { return messages.T(locale, "discord.render.help."+key) }
	var lines []string
	if len(slash) > 0 {
		lines = append(lines, "**"+t("slash")+"**")
		lines = append(lines, slash...)
	}
	if len(prefix) > 0 {
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, "**"+t("prefixes")+"**")
		lines = append(lines, prefix...)
	}
	if len(lines) > 0 {
		lines = append(lines, "")
	}
	lines = append(lines, "**"+t("message")+"**", messages.T(locale, "discord.feedback.command"))
	return Rendered{Embed: &discordgo.MessageEmbed{Title: t("title"), Color: colorNeutral, Description: strings.Join(lines, "\n")}, Ephemeral: true}
}

// errorMessageArgs picks the values of an error that a member may be shown. It keeps ordinary values
// ("USD/KRW", a city name) and withholds the ones that look like a filesystem path or a secret, or whose
// name says they carry error text; it returns the names it withheld.
func errorMessageArgs(args map[string]any) (kept []any, dropped []string) {
	keys := make([]string, 0, len(args))
	for key := range args {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if sensitiveErrorArg(strings.ToLower(key)) || unsafeErrorArgValue(args[key]) {
			dropped = append(dropped, key)
			continue
		}
		kept = append(kept, key, args[key])
	}
	return kept, dropped
}

// needsAny reports whether the message template uses a placeholder for one of the names.
func needsAny(locale, key string, names []string) bool {
	if len(names) == 0 {
		return false
	}
	text, ok := messages.Lookup(locale, key)
	if !ok {
		text, _ = messages.Lookup("en", key)
	}
	for _, name := range names {
		if strings.Contains(text, "{"+name+"}") {
			return true
		}
	}
	return false
}

func sensitiveErrorArg(name string) bool {
	return strings.Contains(name, "error") || strings.Contains(name, "path") || strings.Contains(name, "trace") ||
		strings.Contains(name, "token") || strings.Contains(name, "secret") || strings.Contains(name, "dsn") ||
		strings.Contains(name, "password")
}

var (
	// a drive path (C:\dir, C:/dir), anything with a backslash (\\server\share), /etc/x or ~/x; a lone
	// "/word" is a command name, and "USD/KRW" is a currency pair, so neither matches
	pathLike = regexp.MustCompile(`^[A-Za-z]:[\\/]|\\|^~?/[^\s/]+/`)
	// a long unbroken run of letters and digits with both kinds in it: keys and tokens, not words
	secretLike = regexp.MustCompile(`[A-Za-z0-9_\-.]{32,}`)
)

func unsafeErrorArgValue(value any) bool {
	switch v := value.(type) {
	case string:
		lower := strings.ToLower(v)
		if pathLike.MatchString(v) || strings.Contains(lower, "token") || strings.Contains(lower, "secret") ||
			strings.Contains(lower, "password") || strings.Contains(lower, "bearer ") {
			return true
		}
		for _, run := range secretLike.FindAllString(v, -1) {
			if strings.IndexFunc(run, unicode.IsDigit) >= 0 && strings.IndexFunc(run, unicode.IsLetter) >= 0 {
				return true
			}
		}
	case []string:
		for _, item := range v {
			if unsafeErrorArgValue(item) {
				return true
			}
		}
	case []any:
		for _, item := range v {
			if unsafeErrorArgValue(item) {
				return true
			}
		}
	case map[string]any:
		for key, item := range v {
			if sensitiveErrorArg(strings.ToLower(key)) || unsafeErrorArgValue(item) {
				return true
			}
		}
	case nil, bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return false
	default:
		return true
	}
	return false
}

func statusIcon(s string) string {
	switch s {
	case "ready":
		return "🟢"
	case "disabled":
		return "⚪"
	case "unconfigured", "pending":
		return "🟡"
	case "error":
		return "🔴"
	}
	return "⚫"
}

func pollStatus(s, locale string) string {
	if v, ok := messages.Lookup(locale, "discord.render.poll.status."+s); ok {
		return v
	}
	return s
}

func precipText(p, locale string) string {
	if v, ok := messages.Lookup(locale, "discord.render.precip."+p); ok {
		return v
	}
	return p
}

func toF(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case string:
		var f float64
		fmt.Sscanf(n, "%g", &f)
		return f
	}
	return 0
}

func discordTime(raw string) string {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(raw))
	if err != nil {
		t, err = time.Parse(time.RFC3339Nano, strings.TrimSpace(raw))
	}
	if err != nil {
		return raw
	}
	return fmt.Sprintf("<t:%d:f> (<t:%d:R>)", t.Unix(), t.Unix())
}

func nativeDiscordPoll(out map[string]any, locale string) *discordgo.Poll {
	q := truncate(fmt.Sprint(out["question"]), 300)
	hours := toInt(out["duration_hours"])
	if hours < 1 {
		hours = 24
	}
	if hours > 768 {
		hours = 768
	}
	multi, _ := out["allow_multiselect"].(bool)
	if !multi && toInt(out["max_select"]) > 1 {
		multi = true
	}
	emojis := []string{"1️⃣", "2️⃣", "3️⃣", "4️⃣", "5️⃣", "6️⃣", "7️⃣", "8️⃣", "9️⃣", "🔟"}
	var answers []discordgo.PollAnswer
	if opts, ok := out["options"].([]any); ok {
		for i, o := range opts {
			if i >= 10 {
				break
			}
			m, _ := o.(map[string]any)
			label := truncate(fmt.Sprint(m["label"]), 55)
			ans := discordgo.PollAnswer{Media: &discordgo.PollMedia{Text: label}}
			if i < len(emojis) {
				ans.Media.Emoji = &discordgo.ComponentEmoji{Name: emojis[i]}
			}
			answers = append(answers, ans)
		}
	}
	if len(answers) < 2 {
		yes, no := messages.T(locale, "poll.yes"), messages.T(locale, "poll.no")
		answers = []discordgo.PollAnswer{
			{Media: &discordgo.PollMedia{Text: yes, Emoji: &discordgo.ComponentEmoji{Name: "👍"}}},
			{Media: &discordgo.PollMedia{Text: no, Emoji: &discordgo.ComponentEmoji{Name: "👎"}}},
		}
	}
	return &discordgo.Poll{
		Question:         discordgo.PollMedia{Text: q},
		Answers:          answers,
		Duration:         hours,
		AllowMultiselect: multi,
		LayoutType:       discordgo.PollLayoutTypeDefault,
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// Discord limits (docs: Embed Limits, Message Components).
const (
	embedTitleMax  = 256
	embedDescMax   = 4096
	embedFieldsMax = 25
	fieldNameMax   = 256
	fieldValueMax  = 1024
	footerMax      = 2048
	embedsPerMsg   = 10
	embedTotalMax  = 6000 // across every embed of one message
	buttonLabelMax = 80
	customIDMax    = 100
	rowsMax        = 5
	buttonsPerRow  = 5
)

var (
	mdHeader   = regexp.MustCompile(`(?m)^#{1,6}\s+(.+?)\s*#*\s*$`)
	mdTableSep = regexp.MustCompile(`^\s*\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)*\|?\s*$`)
)

// discordMarkdown turns Markdown a model may write into what an embed shows: headers become bold lines
// (embeds show "## x" literally) and tables become " · "-joined rows without the separator line.
func discordMarkdown(text string) string {
	text = mdHeader.ReplaceAllString(text, "**$1**")
	lines := strings.Split(text, "\n")
	out := lines[:0]
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "|") && strings.HasSuffix(t, "|") && len(t) > 1 {
			if mdTableSep.MatchString(t) {
				continue
			}
			cells := strings.Split(strings.Trim(t, "|"), "|")
			for i := range cells {
				cells[i] = strings.TrimSpace(cells[i])
			}
			l = strings.Join(cells, " · ")
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// chunkText splits text into parts of at most max characters: at a blank line when there is one in the
// second half (a group's heading stays with its items), else at a line break.
func chunkText(text string, limit int) []string {
	var parts []string
	r := []rune(text)
	for len(r) > limit {
		cut := limit
		for i := limit; i > limit/2; i-- {
			if r[i] == '\n' && r[i-1] == '\n' {
				cut = i
				break
			}
		}
		if cut == limit {
			for i := limit; i > limit/2; i-- {
				if r[i] == '\n' {
					cut = i
					break
				}
			}
		}
		parts = append(parts, strings.TrimRight(string(r[:cut]), "\n"))
		r = []rune(strings.TrimLeft(string(r[cut:]), "\n"))
	}
	if len(r) > 0 {
		parts = append(parts, string(r))
	}
	return parts
}

// renderCard shows an Application-built card (action.Card) as embeds with buttons, inside every
// Discord limit: a long text continues in more embeds of the same message, and when the 6000-character
// budget of a message runs out the text is cut with an ellipsis.
func renderCard(c action.Card, ephemeral bool) Rendered {
	color := colorOK
	if v, ok := toneColors[c.Tone]; ok {
		color = v
	}
	budget := embedTotalMax
	take := func(s string, limit int) string {
		s = truncate(s, min(limit, max(budget, 1)))
		if budget <= 0 {
			return ""
		}
		budget -= utf8.RuneCountInString(s)
		return s
	}
	e := &discordgo.MessageEmbed{Color: color, URL: c.URL}
	e.Title = take(c.Title, embedTitleMax)
	footer := ""
	if c.Footer != "" {
		footer = truncate(c.Footer, min(footerMax, 300))
		budget -= utf8.RuneCountInString(footer)
	}
	// fields may use half of what is left when there is text too, so neither starves the other
	var fields []*discordgo.MessageEmbedField
	textBudget := 0
	if strings.TrimSpace(c.Text) != "" {
		textBudget = budget / 2
	}
	for i, f := range c.Fields {
		if i == embedFieldsMax || budget-textBudget < 50 {
			break
		}
		name, value := take(f.Name, fieldNameMax), take(f.Value, fieldValueMax)
		if name == "" {
			name = "\u200b"
		}
		if value == "" {
			value = "\u200b"
		}
		fields = append(fields, &discordgo.MessageEmbedField{Name: name, Value: value, Inline: f.Inline})
	}
	parts := chunkText(discordMarkdown(c.Text), embedDescMax)
	r := Rendered{Embed: e, Ephemeral: ephemeral || c.Ephemeral}
	for i, part := range parts {
		if budget <= 0 || i == embedsPerMsg {
			break
		}
		cut := budget < utf8.RuneCountInString(part)
		part = take(part, embedDescMax)
		if cut || (i == embedsPerMsg-1 && i < len(parts)-1) {
			part = truncate(part, max(1, utf8.RuneCountInString(part)-1)) + "…"
		}
		if i == 0 {
			e.Description = part
		} else {
			r.More = append(r.More, &discordgo.MessageEmbed{Color: color, Description: part})
		}
	}
	last := e
	if n := len(r.More); n > 0 {
		last = r.More[n-1]
	}
	last.Fields = fields
	if footer != "" {
		last.Footer = &discordgo.MessageEmbedFooter{Text: footer}
	}
	if c.Timestamp != "" {
		last.Timestamp = c.Timestamp // Discord shows it next to the footer, in the reader's time zone
	}
	if c.ImageURL != "" {
		last.Image = &discordgo.MessageEmbedImage{URL: c.ImageURL}
	}
	var row []discordgo.MessageComponent
	for _, b := range c.Buttons {
		if len(b.ID) > customIDMax || (b.ID == "" && b.URL == "") {
			continue // a cut custom ID would route the press to the wrong place
		}
		if len(row) == buttonsPerRow {
			r.Components = append(r.Components, discordgo.ActionsRow{Components: row})
			row = nil
			if len(r.Components) == rowsMax {
				break
			}
		}
		btn := discordgo.Button{Label: truncate(b.Label, buttonLabelMax), CustomID: b.ID, Style: discordgo.PrimaryButton}
		switch b.Style {
		case "secondary":
			btn.Style = discordgo.SecondaryButton
		case "success":
			btn.Style = discordgo.SuccessButton
		case "danger":
			btn.Style = discordgo.DangerButton
		case "link":
			btn.Style, btn.URL, btn.CustomID = discordgo.LinkButton, b.URL, ""
		}
		row = append(row, btn)
	}
	if len(row) > 0 && len(r.Components) < rowsMax {
		r.Components = append(r.Components, discordgo.ActionsRow{Components: row})
	}
	return r
}

// Embeds are all of a rendered message's embeds.
func (r Rendered) Embeds() []*discordgo.MessageEmbed {
	return append([]*discordgo.MessageEmbed{r.Embed}, r.More...)
}
