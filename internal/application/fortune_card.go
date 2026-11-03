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

var fortuneLucky = []struct{ key, emoji string }{{"LUCKY_COLOR", "🎨"}, {"LUCKY_ITEM", "🎁"}, {"LUCKY_NUMBER", "🔢"}, {"LUCKY_DIRECTION", "🧭"}, {"LUCKY_TIME", "⏰"}}

// fortuneComputed kinds are a day's readings: their score and lucky points are computed (fortune.Analyze),
// and the card shows those whatever the model wrote.
var fortuneComputed = map[string]bool{"today": true, "detail": true, "morning": true}

// luckyInlineMax is the longest lucky value (runes) that still sits in a column of its own.
const luckyInlineMax = 14

// fortuneCard lays the text of base out as a reading of kind; head is the line under the title (who and
// when) and footer replaces base's footer. When the text is not a reading, base is returned unchanged.
func (s *Service) fortuneCard(loc, kind string, base action.Card, head, footer string) action.Card {
	return s.fortuneCardFor(loc, kind, base, head, footer, nil)
}

// fortuneCardFor is fortuneCard with the computed day of a day's reading: its score, area scores, lucky
// points and basis replace whatever the model wrote. A text that is not a reading keeps its words (minus
// SCORE and LUCKY_* key lines) and gets the computed points as fields and footer as its footer.
func (s *Service) fortuneCardFor(loc, kind string, base action.Card, head, footer string, day *fortune.Day) action.Card {
	if !fortuneComputed[kind] {
		day = nil
	}
	r := fortune.ParseReading(base.Text)
	if !r.Structured() {
		if day == nil {
			return base
		}
		// the words stay, without the model's own score and lucky lines when it wrote them as keys (a key
		// in another language, "총점: 85", stays: it cannot be told from prose safely); the computed points
		// and the basis footer are added as on a reading
		base.Text = fortune.WithoutKeys(base.Text, "SCORE", "LUCKY_COLOR", "LUCKY_ITEM", "LUCKY_NUMBER", "LUCKY_DIRECTION", "LUCKY_TIME")
		base.Footer = footer
		lines := []string{s.scoreLine(day.Scores.Overall), s.areaScores(loc, day.Scores), "🧭 " + s.sajuBasis(loc, *day)}
		base.Fields = append(append(base.Fields, action.CardField{Name: "📊 " + s.I18n.T(loc, "fortune.card.scores"), Value: strings.Join(lines, "\n")}),
			s.luckyFields(loc, s.computedLucky(loc, *day, map[string]string{}))...)
		return base
	}
	label := func(key string) string { return s.I18n.T(loc, "fortune.card."+strings.ToLower(key)) }
	v := r.Values
	if day != nil {
		r.Score = day.Scores.Overall
		v = s.computedLucky(loc, *day, v)
	}
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
		add(s.scoreLine(r.Score))
	}
	if day != nil {
		add(s.areaScores(loc, day.Scores), "🧭 "+s.sajuBasis(loc, *day))
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

func (s *Service) scoreLine(score int) string {
	return fmt.Sprintf("%s  **%d**/100", fortune.ScoreBar(score), score)
}

// areaScores is the computed score of each area in one line ("💼 일·학업 65 · 💰 재물 70 …").
func (s *Service) areaScores(loc string, sc fortune.Scores) string {
	scores := map[string]int{"WORK": sc.Work, "MONEY": sc.Money, "LOVE": sc.Love, "HEALTH": sc.Health}
	parts := make([]string, 0, len(fortuneAreas))
	for _, a := range fortuneAreas {
		parts = append(parts, fmt.Sprintf("%s %s %d", a.emoji, s.I18n.T(loc, "fortune.card."+strings.ToLower(a.key)), scores[a.key]))
	}
	return strings.Join(parts, " · ")
}

// computedLucky puts the day's computed lucky points into a reading's values (a copy), over the model's.
func (s *Service) computedLucky(loc string, d fortune.Day, values map[string]string) map[string]string {
	v := make(map[string]string, len(values)+5)
	for k, x := range values {
		v[k] = x
	}
	v["LUCKY_COLOR"] = s.I18n.T(loc, fmt.Sprintf("fortune.lucky.color.%d", d.Lucky.Color))
	v["LUCKY_ITEM"] = s.I18n.T(loc, fmt.Sprintf("fortune.lucky.item.%d", d.Lucky.Item))
	v["LUCKY_NUMBER"] = strconv.Itoa(d.Lucky.Number)
	v["LUCKY_DIRECTION"] = s.I18n.T(loc, fmt.Sprintf("fortune.lucky.direction.%d", d.Lucky.Direction))
	delete(v, "LUCKY_TIME")
	if len(d.BestHours) > 0 {
		v["LUCKY_TIME"] = s.hourLabel(loc, d.BestHours[0])
	}
	return v
}

// hourLabel is a two-hour 시진 with its clock times ("사시(09~11시)").
func (s *Service) hourLabel(loc string, branch int) string {
	return s.I18n.T(loc, "fortune.saju.hour", "branch", s.I18n.T(loc, fmt.Sprintf("fortune.branch.%d", branch)),
		"from", fmt.Sprintf("%02d", (branch*2+23)%24), "to", fmt.Sprintf("%02d", branch*2+1))
}

// sajuRelations are the day's relations with the natal chart, in loc's words.
func (s *Service) sajuRelations(loc string, d fortune.Day) []string {
	rel := func(with, r string) string {
		return s.I18n.T(loc, "fortune.saju.rel_with", "with", s.I18n.T(loc, "fortune.saju.with."+with), "rel", s.I18n.T(loc, "fortune.saju.rel."+r))
	}
	var out []string
	for _, h := range d.Hits {
		out = append(out, rel(strconv.Itoa(h.Pos), strconv.Itoa(h.Rel)))
	}
	if d.StemHap {
		out = append(out, rel("day_stem", "stem_hap"))
	}
	if d.StemClash {
		out = append(out, rel("day_stem", "stem_clash"))
	}
	return out
}

// sajuBasis is the one-line why of a day's reading: its ten god, one relation and its first star
// ("정재일 · 일지와 육합 · 천을귀인"). A harmony (육합, 삼합, 천간합) is named before a clash, so the card
// does not lead with a warning on a day that has both.
func (s *Service) sajuBasis(loc string, d fortune.Day) string {
	parts := []string{s.I18n.T(loc, "fortune.saju.day_of", "god", s.I18n.T(loc, fmt.Sprintf("fortune.saju.god.%d", d.StemGod)))}
	harmony := fortune.Day{StemHap: d.StemHap}
	for _, h := range d.Hits {
		if h.Rel == fortune.RelSixHarmony || h.Rel == fortune.RelHalfTriad {
			harmony.Hits = append(harmony.Hits, h)
		}
	}
	if rels := s.sajuRelations(loc, harmony); len(rels) > 0 {
		parts = append(parts, rels[0])
	} else if rels := s.sajuRelations(loc, d); len(rels) > 0 {
		parts = append(parts, rels[0])
	}
	if len(d.Stars) > 0 {
		parts = append(parts, s.I18n.T(loc, fmt.Sprintf("fortune.saju.star.%d", d.Stars[0])))
	}
	return strings.Join(parts, " · ")
}

// factsText is the computed-facts block of a day's fortune data (about a hundred tokens).
func (s *Service) factsText(loc string, d fortune.Day) string {
	none := s.I18n.T(loc, "fortune.saju.none")
	list := func(items []string) string {
		if len(items) == 0 {
			return none
		}
		return strings.Join(items, ", ")
	}
	name := func(prefix string, ids []int) []string {
		out := make([]string, 0, len(ids))
		for _, id := range ids {
			out = append(out, s.I18n.T(loc, fmt.Sprintf("%s.%d", prefix, id)))
		}
		return out
	}
	acts := func(ids []string) []string {
		out := make([]string, 0, len(ids))
		for _, id := range ids {
			out = append(out, s.I18n.T(loc, "fortune.saju.act."+id))
		}
		return out
	}
	var hours, elements []string
	for _, b := range d.BestHours {
		hours = append(hours, s.hourLabel(loc, b))
	}
	for e, n := range d.Elements {
		elements = append(elements, s.I18n.T(loc, "fortune.saju.element_count", "element", s.I18n.T(loc, fmt.Sprintf("fortune.saju.element.%d", e)), "n", n))
	}
	spirit := "black"
	if fortune.Yellow(d.Spirit) {
		spirit = "yellow"
	}
	noHour := ""
	if !d.HasHour {
		noHour = s.I18n.T(loc, "fortune.data.no_hour")
	}
	lucky := s.computedLucky(loc, d, nil)
	return s.I18n.T(loc, "fortune.data.facts",
		"stem_god", s.I18n.T(loc, fmt.Sprintf("fortune.saju.god.%d", d.StemGod)),
		"branch_god", s.I18n.T(loc, fmt.Sprintf("fortune.saju.god.%d", d.BranchGod)),
		"stage", s.I18n.T(loc, fmt.Sprintf("fortune.saju.stage.%d", d.Stage)),
		"relations", list(s.sajuRelations(loc, d)), "stars", list(name("fortune.saju.star", d.Stars)),
		"officer", s.I18n.T(loc, fmt.Sprintf("fortune.saju.officer.%d", d.Officer)), "spirit", s.I18n.T(loc, "fortune.saju.spirit."+spirit),
		"yi", list(acts(d.Yi)), "ji", list(acts(d.Ji)), "hours", list(hours),
		"overall", d.Scores.Overall, "money", d.Scores.Money, "love", d.Scores.Love, "work", d.Scores.Work, "health", d.Scores.Health,
		"color", lucky["LUCKY_COLOR"], "item", lucky["LUCKY_ITEM"], "number", lucky["LUCKY_NUMBER"], "direction", lucky["LUCKY_DIRECTION"],
		"lucky_element", s.I18n.T(loc, fmt.Sprintf("fortune.saju.element.%d", d.LuckyElem)),
		"elements", strings.Join(elements, " "), "weakest", s.I18n.T(loc, fmt.Sprintf("fortune.saju.element.%d", d.Weakest)),
		"hour_note", noHour)
}
