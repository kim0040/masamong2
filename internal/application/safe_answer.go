package application

import (
	"regexp"
	"strings"
	"sync"

	"masamong/internal/chat"
	"masamong/internal/lang"
)

// safeAnswer catches literal prompt recitation before delivery and conversation/history storage. It
// replaces the whole answer, so it only matches what an answer would never contain by itself:
//   - the first sentence of the system prompt that was actually sent (persona, rules, agent prompt),
//   - the exact delimiter tokens that fence the planner's data blocks,
//   - the planner's header sentence ("You are the intent analyzer for <bot>. Output valid JSON only."),
//   - the first two lines of the memory and security rule sections together (a "### Always" heading alone
//     is an ordinary markdown heading),
//   - the answer template's attribute-style tag lines ("<recent_chat for reference>"); bare tags such
//     as <memory> are not markers, a code explanation uses them.
//
// Only the answer's own language pack and English are checked: the model recites the prompt it was given.
func (s *Service) safeAnswer(answer string, voice chat.Voice, system ...string) string {
	locale := "en"
	if voice.Pack != nil {
		locale = voice.Pack.Tag
	}
	for _, prompt := range system {
		header := []rune(strings.SplitN(strings.SplitN(prompt, "\n", 2)[0], ". ", 2)[0])
		if len(header) >= 16 && strings.Contains(answer, string(header[:min(256, len(header))])) {
			return s.Msg(locale, "generic.error")
		}
	}
	text := strings.ReplaceAll(answer, "\r\n", "\n")
	tags := []string{locale}
	if locale != "en" {
		tags = append(tags, "en")
	}
	for _, tag := range tags {
		if m := recitationMarkersFor(tag); m != nil && m.matches(text) {
			return s.Msg(locale, "generic.error")
		}
	}
	return answer
}

type recitationMarkers struct {
	literals []string       // delimiters, multi-line rule openings and the template's attribute-style tags
	header   *regexp.Regexp // the planner header, with any bot name
}

func (m *recitationMarkers) matches(text string) bool {
	for _, l := range m.literals {
		if strings.Contains(text, l) {
			return true
		}
	}
	return m.header != nil && m.header.MatchString(text)
}

var recitationCache sync.Map // locale tag → *recitationMarkers (nil when the pack is unknown)

func recitationMarkersFor(tag string) *recitationMarkers {
	if v, ok := recitationCache.Load(tag); ok {
		return v.(*recitationMarkers)
	}
	m := buildRecitationMarkers(tag)
	recitationCache.Store(tag, m)
	return m
}

func buildRecitationMarkers(tag string) *recitationMarkers {
	p, ok := lang.Get(tag)
	if !ok {
		return nil
	}
	c := p.Chat
	m := &recitationMarkers{}
	for _, d := range c.Delimiters.All() {
		if d != "" {
			m.literals = append(m.literals, d)
		}
	}
	for _, rules := range []string{c.MemoryRules, c.SecurityRules} {
		if opening := firstLines(rules, 2); strings.Count(opening, "\n") == 1 {
			m.literals = append(m.literals, opening)
		}
	}
	if h := strings.TrimSpace(c.PlannerHeader); h != "" {
		// the header names the bot: any name fits the placeholder (the raw template is matched too)
		m.header = regexp.MustCompile(strings.ReplaceAll(regexp.QuoteMeta(h), regexp.QuoteMeta("{bot}"), "[^\n]{1,80}"))
	}
	for _, line := range strings.Split(c.AnswerTemplate, "\n") {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, "<") && strings.HasSuffix(line, ">") && strings.ContainsAny(line, " \t") {
			m.literals = append(m.literals, line)
		}
	}
	return m
}

// firstLines is the first n non-empty lines of text, trimmed and joined with "\n".
func firstLines(text string, n int) string {
	var out []string
	for _, l := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if l = strings.TrimRight(l, " \t"); strings.TrimSpace(l) != "" {
			out = append(out, l)
			if len(out) == n {
				break
			}
		}
	}
	return strings.Join(out, "\n")
}
