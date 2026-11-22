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
	in := renderInput{out: out, status: status, live: live, cmd: cmd, locale: locale, t: t}
	e := &discordgo.MessageEmbed{Color: colorOK}
	switch res.Action {
	case "weather.query":
		return renderWeather(in, e)
	case "disasters.query":
		return renderDisasters(in, e)
	case "finance.quote":
		return renderQuote(in, e)
	case "finance.fx":
		return renderFX(in, e)
	case "search.web":
		return renderSearch(in, e)
	case "activity.ranking":
		return renderRanking(in, e)
	case "places.search":
		return renderPlaces(in, e)
	case "polls.create":
		return renderPollCreate(in, e)
	case "polls.vote":
		return renderPollVote(in, e)
	case "polls.close":
		return renderPollClose(in, e)
	case "polls.result":
		return renderPollResult(in, e)
	case "runtime.doctor":
		return renderDoctor(in, e)
	case "images.generate":
		return renderImage(in, e)
	case "help.version":
		return RenderHelp(locale, nil, nil)
	default:
		e.Title = res.Action
		e.Description = truncate(fmt.Sprint(out), 1000)
	}
	return Rendered{Embed: e, Ephemeral: cmd.Ephemeral}
}

// renderInput is what every result renderer reads: the Action's output and the command it answers.
type renderInput struct {
	out    map[string]any
	status string
	live   bool
	cmd    Command
	locale string
	t      func(key string, args ...any) string // a discord.render.* text in locale
}

// embedOnly is a result shown as its embed alone, private when the command is.
func (in renderInput) embedOnly(e *discordgo.MessageEmbed) Rendered {
	return Rendered{Embed: e, Ephemeral: in.cmd.Ephemeral}
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
