package application

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"masamong/internal/action"
	"masamong/internal/features/fortune"
)

// Fortune cards (docs/plan-discord-experience.md §3.4). A personal fortune is asked for as "KEY: value"
// lines (the language packs' fortune_* prompts) and laid out here: the score and the summary on top, the
// areas, advice and lucky points in their own places. Labels come from the catalogs (fortune.card.*) and
// the emoji from here, so every language gets the same layout. A text that is not in that shape (an older
// cached text, a model that ignored the format) is shown as written, as before.

// fortuneCompact kinds keep the four areas in the description; the longer readings give each its field.
var fortuneCompact = map[string]bool{"today": true, "morning": true, "sign": true}

var fortuneAreas = []struct{ key, emoji string }{{"WORK", "💼"}, {"MONEY", "💰"}, {"LOVE", "💞"}, {"HEALTH", "🌿"}}

// fortuneFlows are the time-of-period lines of the longer readings, one field each kind.
var fortuneFlows = []struct {
	field, emoji string
	keys         []string
}{
	{"flow_day", "⏱️", []string{"MORNING", "AFTERNOON", "EVENING"}},
	{"flow_month", "🗓️", []string{"EARLY", "MID", "LATE"}},
	{"flow_year", "🗓️", []string{"Q1", "Q2", "Q3", "Q4"}},
}

var fortuneLucky = []struct{ key, emoji string }{{"LUCKY_COLOR", "🎨"}, {"LUCKY_ITEM", "🎁"}, {"LUCKY_NUMBER", "🔢"}, {"LUCKY_TIME", "⏰"}}

// luckyInlineMax is the longest lucky value (runes) that still sits in a column of its own.
const luckyInlineMax = 14

// fortuneCard lays the text of base out as a reading of kind; head is the line under the title (who and
// when) and footer replaces base's footer. When the text is not a reading, base is returned unchanged.
func (s *Service) fortuneCard(loc, kind string, base action.Card, head, footer string) action.Card {
	r := fortune.ParseReading(base.Text)
	if !r.Structured() {
		return base
	}
	label := func(key string) string { return s.I18n.T(loc, "fortune.card."+strings.ToLower(key)) }
	v := r.Values
	var desc []string
	add := func(lines ...string) {
		for _, l := range lines {
			if l != "" {
				desc = append(desc, l)
			}
		}
	}
	gap := func() {
		if len(desc) > 0 && desc[len(desc)-1] != "" {
			desc = append(desc, "")
		}
	}
	if g := v["GREETING"]; g != "" {
		add(g)
	} else {
		add(head)
	}
	gap()
	if r.Score >= 0 {
		add(fmt.Sprintf("%s  **%d**/100", fortune.ScoreBar(r.Score), r.Score))
	}
	add(v["SUMMARY"])
	var fields []action.CardField
	if fortuneCompact[kind] {
		gap()
		for _, a := range fortuneAreas {
			if v[a.key] != "" {
				add(a.emoji + " **" + label(a.key) + "** " + v[a.key])
			}
		}
	}
	for _, f := range fortuneFlows {
		var lines []string
		for _, k := range f.keys {
			if v[k] != "" {
				lines = append(lines, "**"+label(k)+"** "+v[k])
			}
		}
		if len(lines) > 0 {
			fields = append(fields, action.CardField{Name: f.emoji + " " + label(f.field), Value: strings.Join(lines, "\n")})
		}
	}
	if !fortuneCompact[kind] {
		for _, a := range fortuneAreas {
			if v[a.key] != "" {
				fields = append(fields, action.CardField{Name: a.emoji + " " + label(a.key), Value: v[a.key]})
			}
		}
	}
	for _, f := range []struct{ key, emoji string }{{"DATES", "📌"}, {"BEST_TIME", "🌈"}, {"CAUTION_TIME", "🌧️"}, {"DO", "✅"}, {"AVOID", "⚠️"}, {"CHECKLIST", "☑️"}} {
		if v[f.key] != "" {
			fields = append(fields, action.CardField{Name: f.emoji + " " + label(f.key), Value: bullets(fortune.Items(v[f.key]))})
		}
	}
	fields = append(fields, s.luckyFields(loc, v)...)
	if one := v["ONELINE"]; one != "" {
		gap()
		add("> " + one)
	}
	return action.Card{Title: base.Title, Text: strings.Join(desc, "\n"), Tone: "fortune", Fields: fields, Footer: footer,
		Buttons: base.Buttons, Ephemeral: base.Ephemeral}
}

// luckyFields are the lucky points: two or three short ones in columns, otherwise one line.
func (s *Service) luckyFields(loc string, v map[string]string) []action.CardField {
	type item struct{ name, value string }
	var items []item
	short := true
	for _, l := range fortuneLucky {
		if v[l.key] == "" {
			continue
		}
		items = append(items, item{l.emoji + " " + s.I18n.T(loc, "fortune.card."+strings.ToLower(l.key)), v[l.key]})
		short = short && utf8.RuneCountInString(v[l.key]) <= luckyInlineMax
	}
	switch {
	case len(items) == 0:
		return nil
	case short && len(items) >= 2 && len(items) <= 3:
		out := make([]action.CardField, len(items))
		for i, it := range items {
			out[i] = action.CardField{Name: it.name, Value: it.value, Inline: true}
		}
		return out
	}
	var parts []string
	for _, l := range fortuneLucky {
		if v[l.key] != "" {
			parts = append(parts, l.emoji+" "+v[l.key])
		}
	}
	return []action.CardField{{Name: "🍀 " + s.I18n.T(loc, "fortune.card.lucky"), Value: strings.Join(parts, " · ")}}
}

// rankingCard lays the twelve-sign ranking out in three groups; sign names are the catalog's, not the
// model's spelling. When the text is not a ranking, base is returned unchanged.
func (s *Service) rankingCard(loc string, base action.Card) action.Card {
	r := fortune.ParseReading(base.Text)
	if !r.Ranked() {
		return base
	}
	medals := []string{"🥇", "🥈", "🥉"}
	groups := []struct {
		key, emoji string
		from, to   int
	}{{"rank_top", "🌟", 1, 3}, {"rank_mid", "🌤️", 4, 9}, {"rank_low", "☁️", 10, 12}}
	var desc []string
	for _, g := range groups {
		var lines []string
		for _, rk := range r.Ranks {
			if rk.Pos < g.from || rk.Pos > g.to {
				continue
			}
			mark := strconv.Itoa(rk.Pos) + "."
			if rk.Pos <= len(medals) {
				mark = medals[rk.Pos-1]
			}
			name := rk.Sign
			if n, ok := s.signOf(rk.Sign); ok {
				name = s.I18n.T(loc, fmt.Sprintf("fortune.sign.%d", n))
			}
			line := mark + " **" + name + "** — " + rk.Comment
			if rk.Color != "" {
				line += " · 🎨 " + rk.Color
			}
			if rk.Number != "" {
				line += " · 🔢 " + rk.Number
			}
			lines = append(lines, line)
		}
		if len(lines) == 0 {
			continue
		}
		if len(desc) > 0 {
			desc = append(desc, "")
		}
		desc = append(desc, "**"+g.emoji+" "+s.I18n.T(loc, "fortune.card."+g.key)+"**")
		desc = append(desc, lines...)
	}
	return action.Card{Title: base.Title, Text: strings.Join(desc, "\n"), Tone: "fortune", Footer: base.Footer, Ephemeral: base.Ephemeral}
}

// bullets is one item as it is, more as a bullet list.
func bullets(items []string) string {
	if len(items) == 1 {
		return items[0]
	}
	return "• " + strings.Join(items, "\n• ")
}

// dayLabel is a date as people say it ("10월 15일 (수)"), in loc's words.
func (s *Service) dayLabel(loc string, t time.Time) string {
	return s.I18n.T(loc, "date.day", "month", int(t.Month()), "day", t.Day(),
		"weekday", s.I18n.T(loc, "reminders.weekday."+strconv.Itoa(int(t.Weekday()))))
}

// shortDate is a date in a list ("10/20"), in loc's order of month and day.
func (s *Service) shortDate(loc string, t time.Time) string {
	return s.I18n.T(loc, "date.short", "month", int(t.Month()), "day", t.Day())
}

// fortuneBasis is the footer of a personal fortune: today's day pillar and the person's sign, the facts the
// reading was written from.
func (s *Service) fortuneBasis(loc string, p fortuneProfile, at time.Time) string {
	birth, _ := time.Parse("2006-01-02", p.BirthDate)
	_, _, day, _, _ := fortune.Pillars(at, -1)
	return s.I18n.T(loc, "fortune.card.basis", "day", s.pillarText(loc, day),
		"sign", s.I18n.T(loc, fmt.Sprintf("fortune.sign.%d", fortune.SunSign(int(birth.Month()), birth.Day()))))
}
