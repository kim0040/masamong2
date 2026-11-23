package episodes

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf8"

	"masamong/internal/lang"
)

// Person memory, extraction side (v1.7.0 phase 2). The model may add a "people" list to the episode
// answer, or answer only that list (the person_extract prompt). Nothing here writes: ParsePeople turns
// the list into PersonFact values whose people are stored references, and drops what does not hold.

// Limits of a person fact.
const (
	MaxPersonTopic = 20      // a topic key
	MaxPersonText  = MaxText // the sentence (120 runes)
)

// PersonKinds is the closed list of kinds of a person fact.
var PersonKinds = []string{"nickname", "like", "dislike", "trait", "status", "appearance", "relation", "other"}

// Links of a person fact: how the subject was tied to the statement.
const (
	LinkSelf    = "self"    // the subject is one of the speakers
	LinkMention = "mention" // the subject was named by a <@id>, or by a token the speaker used
	LinkReply   = "reply"   // the speaker replied to a line of the subject
	LinkName    = "name"    // the subject was found by an exact alias match, not by a mention
)

// PersonFact is one validated statement about one person. About and By are stored references, Ev the
// observation IDs of the evidence lines, Text the stored sentence (people as {@ref}). Topic is trimmed
// with its spaces collapsed; the store normalizes it further.
type PersonFact struct {
	About string
	Kind  string
	Topic string
	Text  string
	By    []string
	Ev    []string
	Link  string
}

// PeopleOptions tune ParsePeople.
type PeopleOptions struct {
	// InstructionLike reports a text that reads as an instruction to the bot. Nil uses the same rule as
	// chat.InstructionLike over every embedded language pack.
	InstructionLike func(text string) bool
}

type rawPerson struct {
	About string          `json:"about"`
	Kind  string          `json:"kind"`
	Topic string          `json:"topic"`
	Text  string          `json:"text"`
	By    json.RawMessage `json:"by"`
	Ev    []string        `json:"ev"`
}

// ParsePeople reads the "people" list of a model answer (the whole answer, code fences and a leading
// sentence allowed) and validates it against the prompt. It returns the valid facts and how many entries
// were dropped (no text is kept). It never fails: a missing list is no facts, a list that is not a list
// counts as one rejected entry. Kakao prompts discard everything.
func (p Prompt) ParsePeople(raw string, opt PeopleOptions) ([]PersonFact, int) {
	if p.kakao {
		return nil, 0
	}
	obj, ok := JSONObject(raw)
	if !ok {
		return nil, 0
	}
	var doc struct {
		People json.RawMessage `json:"people"`
	}
	if err := json.Unmarshal([]byte(obj), &doc); err != nil {
		return nil, 0
	}
	return p.ParsePeopleList(doc.People, opt)
}

// ParsePeopleList is ParsePeople for the "people" value alone.
func (p Prompt) ParsePeopleList(list json.RawMessage, opt PeopleOptions) ([]PersonFact, int) {
	if p.kakao || len(list) == 0 || string(list) == "null" {
		return nil, 0
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(list, &entries); err != nil {
		return nil, 1
	}
	check := opt.InstructionLike
	if check == nil {
		check = anyPackInstructionLike
	}
	var out []PersonFact
	rejected := 0
	seen := map[string]bool{}
	for _, e := range entries {
		var rp rawPerson
		if err := json.Unmarshal(e, &rp); err != nil {
			rejected++
			continue
		}
		f, why := p.validatePerson(rp, check)
		if why != "" {
			rejected++
			continue
		}
		key := f.About + "\x00" + f.Kind + "\x00" + f.Topic + "\x00" + f.Text + "\x00" + strings.Join(f.By, ",") + "\x00" + strings.Join(f.Ev, ",")
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, f)
	}
	return out, rejected
}

var spaces = regexp.MustCompile(`\s+`)

func (p Prompt) validatePerson(rp rawPerson, instructionLike func(string) bool) (PersonFact, string) {
	f := PersonFact{Kind: strings.TrimSpace(rp.Kind)}
	if !contains(PersonKinds, f.Kind) {
		return f, "unknown kind"
	}
	about, ok := p.byToken[strings.Trim(strings.TrimSpace(rp.About), "{}")]
	if !ok {
		return f, "unknown person in about"
	}
	if !p.personRef(about.Ref) {
		return f, "about is not a person that can be remembered"
	}
	f.About = about.Ref

	f.Topic = strings.TrimSpace(spaces.ReplaceAllString(rp.Topic, " "))
	if f.Topic == "" || utf8.RuneCountInString(f.Topic) > MaxPersonTopic || strings.ContainsAny(f.Topic, "{}") {
		return f, "bad topic"
	}
	text := strings.TrimSpace(rp.Text)
	if text == "" || utf8.RuneCountInString(text) > MaxPersonText || strings.ContainsAny(text, "\n\r") {
		return f, "bad text"
	}
	if instructionLike(text) || instructionLike(f.Topic) {
		return f, "instruction"
	}
	if strings.ContainsAny(promptToken.ReplaceAllString(text, ""), "{}") {
		return f, "template-like text"
	}
	stored, why := p.storeText(text, MaxPersonText)
	if why != "" {
		return f, why
	}
	// the write gate's rules, on the text as it is stored: tokens expanded to references, spaces
	// collapsed, and the subject named in it
	if utf8.RuneCountInString(strings.Join(strings.Fields(stored), " ")) > MaxPersonText {
		return f, "text too long"
	}
	if !strings.Contains(stored, "{@"+f.About+"}") {
		return f, "text does not name its subject"
	}
	f.Text = stored

	if len(rp.Ev) == 0 {
		return f, "no evidence"
	}
	authors := map[string]bool{}
	seen := map[string]bool{}
	for _, label := range rp.Ev {
		id, ok := p.labels[strings.Trim(strings.TrimSpace(label), "[]")]
		if !ok {
			return f, "evidence not in the input"
		}
		if p.byID[id].Bot {
			return f, "evidence is the bot's line"
		}
		if !seen[id] {
			seen[id] = true
			f.Ev = append(f.Ev, id)
		}
		authors[p.refOfLine(id)] = true
	}
	by, ok := tokenList(rp.By)
	if !ok || len(by) == 0 {
		return f, "by is not a list of people"
	}
	bySet := map[string]bool{}
	for _, tok := range by {
		per, known := p.byToken[tok]
		if !known {
			return f, "unknown person in by"
		}
		if !authors[per.Ref] || !p.personRef(per.Ref) {
			return f, "by is not an author of the evidence"
		}
		if !bySet[per.Ref] {
			bySet[per.Ref] = true
			f.By = append(f.By, per.Ref)
		}
	}
	f.Link = p.linkFor(f.About, bySet, f.Ev)
	return f, ""
}

// personRef reports whether a stored reference can be the subject or the speaker of a person fact: not
// the bot, and never a Kakao or label-only identity.
func (p Prompt) personRef(ref string) bool {
	return ref != "" && ref != p.botRef && !strings.HasPrefix(ref, "kakao:") && !strings.HasPrefix(ref, "label:")
}

// linkFor says how the subject was tied to the statement.
func (p Prompt) linkFor(about string, by map[string]bool, ev []string) string {
	if by[about] {
		return LinkSelf
	}
	if l := p.linkOf[about]; l != "" {
		return l
	}
	for _, id := range ev {
		if r := p.byID[id].ReplyTo; r != "" && p.refOfLine(r) == about {
			return LinkReply
		}
	}
	return LinkMention
}

// ---------------------------------------------------------------------------
// mentions in the lines
// ---------------------------------------------------------------------------

var mentionRE = regexp.MustCompile(`<@!?([^<>@&!\s]+)>`)

type mentionSet struct {
	active bool
	byID   map[string]Person
	named  []Person
	dup    map[string]bool // label key shared by two different people
	bot    string
}

func labelKey(label string) string { return strings.ToLower(strings.TrimSpace(label)) }

func linkOrDefault(m Person) string {
	switch m.Link {
	case LinkMention, LinkName:
		return m.Link
	}
	if m.ID != "" {
		return LinkMention
	}
	return LinkName
}

// newMentions prepares the <@id> rendering and the people found by name. It is inactive (and calls
// nothing) for Kakao, or when there is nothing to render.
func newMentions(in Input) *mentionSet {
	ms := &mentionSet{byID: map[string]Person{}, dup: map[string]bool{}, bot: in.BotRef}
	if in.Kakao {
		return ms
	}
	found := len(in.Mentioned) > 0
	for _, m := range in.Lines {
		if !found && strings.Contains(m.Text, "<@") {
			found = true
		}
	}
	if !found {
		return ms
	}
	ms.active = true
	refsOf := map[string]map[string]bool{}
	note := func(label, ref string) {
		k := labelKey(label)
		if refsOf[k] == nil {
			refsOf[k] = map[string]bool{}
		}
		refsOf[k][ref] = true
	}
	seen := map[string]bool{}
	for _, m := range in.Lines {
		if m.Bot || seen[m.Speaker()] {
			continue
		}
		seen[m.Speaker()] = true
		ref := in.RefOf(m)
		note(m.Author, ref)
		if m.Account != "" {
			ms.byID[m.Account] = Person{Ref: ref, Label: m.Author, ID: m.Account, Link: LinkMention}
		}
	}
	for _, m := range in.Mentioned {
		if m.Ref == "" || strings.TrimSpace(m.Label) == "" || m.Ref == in.BotRef {
			continue
		}
		note(m.Label, m.Ref)
		m.Link = linkOrDefault(m)
		if m.ID != "" {
			ms.byID[m.ID] = m
		} else {
			ms.named = append(ms.named, m)
		}
	}
	for k, refs := range refsOf {
		if len(refs) > 1 {
			ms.dup[k] = true
		}
	}
	return ms
}

// render replaces every <@id> it can resolve by "label(pN)"; a label shared by two people is written
// without a token. Unknown ids are left as they were.
func (ms *mentionSet) render(p *Prompt, text string) string {
	if !ms.active || !strings.Contains(text, "<@") {
		return text
	}
	return mentionRE.ReplaceAllStringFunc(text, func(m string) string {
		per, ok := ms.byID[mentionRE.FindStringSubmatch(m)[1]]
		if !ok || per.Ref == ms.bot {
			return m
		}
		if ms.dup[labelKey(per.Label)] {
			return strings.TrimSpace(per.Label)
		}
		got := p.person(per.Ref, per.Label)
		if p.linkOf[per.Ref] == "" {
			p.linkOf[per.Ref] = per.Link
		}
		return got.Label + "(" + got.Token + ")"
	})
}

// addNamed lists the people found by name after the people of the lines.
func (ms *mentionSet) addNamed(p *Prompt) {
	for _, m := range ms.named {
		if ms.dup[labelKey(m.Label)] {
			continue
		}
		if _, known := p.byRef[m.Ref]; known {
			continue
		}
		p.person(m.Ref, m.Label)
		p.linkOf[m.Ref] = LinkName
	}
}

// ---------------------------------------------------------------------------
// prompts
// ---------------------------------------------------------------------------

// The pack's person section is fenced by two placeholder lines; FillExtract removes the fences (and, for
// Kakao, what is between them) before anything is substituted, so message text can never forge them.
const (
	peopleBegin = "{people_begin}"
	peopleEnd   = "{people_end}"
)

// stripPeopleSection removes the fence lines of tpl, and the lines between them when drop is set.
func stripPeopleSection(tpl string, drop bool) string {
	if !strings.Contains(tpl, peopleBegin) {
		return tpl
	}
	var out []string
	in := false
	for _, line := range strings.Split(tpl, "\n") {
		switch strings.TrimSpace(line) {
		case peopleBegin:
			in = true
			continue
		case peopleEnd:
			in = false
			continue
		}
		if in && drop {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// FillExtract fills the pack's memory.episode_extract template: the usual placeholders, and the people
// section (rules and output field) kept, or dropped for a Kakao prompt. Use it instead of lang.Fill.
func (p Prompt) FillExtract(tpl string) string {
	return lang.Fill(stripPeopleSection(tpl, p.kakao), "now", p.Now, "people", p.People(), "plans", p.Plans, "lines", p.Lines)
}

// BuildPersonExtract builds the input of the people-only prompt (memory.person_extract): a one-line self
// statement, or a backfill window. The people, mentions, line labels and limits are exactly Build's; open
// plans are not offered. ok is false when there is nothing to ask: a Kakao segment (no person memory)
// or no human line.
func BuildPersonExtract(in Input) (Prompt, bool) {
	in.Plans = nil
	p := Build(in)
	human := false
	for _, m := range in.Lines {
		if !m.Bot {
			human = true
			break
		}
	}
	return p, human && !in.Kakao
}

// FillPersonExtract fills the pack's memory.person_extract template.
func (p Prompt) FillPersonExtract(tpl string) string {
	return lang.Fill(tpl, "now", p.Now, "people", p.People(), "lines", p.Lines)
}

// ---------------------------------------------------------------------------
// the instruction check
// ---------------------------------------------------------------------------

// anyPackInstructionLike is chat.InstructionLike over every embedded pack: a text with one of the
// packs' instruction cues or a block delimiter of the prompt reads as an instruction to the bot.
// (episodes cannot import chat; a test keeps the two in step.)
func anyPackInstructionLike(text string) bool {
	t := strings.ToLower(text)
	for _, tag := range lang.Available() {
		pk, ok := lang.Get(tag)
		if !ok || pk == nil {
			continue
		}
		for _, c := range pk.Memory.InstructionCues {
			if c = strings.ToLower(strings.TrimSpace(c)); c != "" && strings.Contains(t, c) {
				return true
			}
		}
		for _, d := range pk.Chat.Delimiters.All() {
			if d != "" && strings.Contains(text, d) {
				return true
			}
		}
	}
	return false
}
