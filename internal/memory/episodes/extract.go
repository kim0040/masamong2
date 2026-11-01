package episodes

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"masamong/internal/conversation"
)

// Limits of the derived sentences (ADR-17). A longer text is never cut: the item is rejected.
const (
	MaxText     = 120 // an item, a title, a gist
	MaxLabel    = 40  // a room-state topic label
	MaxWhenText = 60  // the original wording of a date
	MaxPlans    = 3   // open plans offered as link candidates
)

// Episode kinds, item kinds and statuses.
var (
	EpisodeKinds = []string{"plan", "decision", "event", "joke", "fact"}
	ItemKinds    = []string{"decision", "proposal", "open_question", "moment", "fact"}
)

// Person is one speaker of a segment: the token the model sees, the stored reference and the name.
type Person struct {
	Token string // p1, p2, ...
	Ref   string // what is stored (store.PersonRef for Discord accounts, kakao:<room>:<label> for Kakao)
	Label string
}

// OpenPlan is an open or decided plan of the channel offered to the model as a link candidate.
type OpenPlan struct {
	ID       string
	Title    string // stored text: people as {@ref}
	Status   string
	Due      string
	WhenText string
	Revision int64
}

// Input is what an extraction (or a room-state update) prompt is built from.
type Input struct {
	Lines    []Message // the segment, bot lines included, in time order
	BotName  string
	Now      time.Time // the reference time (the segment's last message)
	Location *time.Location
	Plans    []OpenPlan
	// RefOf maps a speaker to its stored reference. Required: the package does not know the store's
	// hashing (Discord) or the room naming (Kakao).
	RefOf func(Message) string
	// LabelOf names a stored reference that is not among the lines (people of earlier plans or state).
	LabelOf  func(ref string) string
	MaxRunes int // per line; 0 = 400
}

// Prompt is a built prompt plus the maps that turn the model's labels back into IDs and references.
type Prompt struct {
	Now   string
	Plans string
	Lines string

	labels  map[string]string // m3 -> message ID
	byID    map[string]Message
	people  []Person
	byToken map[string]Person
	byRef   map[string]Person
	plans   map[string]OpenPlan // e1 -> plan

	refBySpeaker map[string]string
	labelOf      func(string) string
	prev         map[string]carried // t1/q1 -> earlier room-state entry (Previous)
}

// PersonRefs lists the references of the prompt's people in token order.
func (p Prompt) PersonRefs() []Person { return append([]Person(nil), p.people...) }

// Build lays out the lines as "[mN] Name(pN) ↩mK: text" and assigns person tokens in order of first
// appearance. The model never sees platform IDs or stored references.
func Build(in Input) Prompt {
	p := Prompt{labels: map[string]string{}, byID: map[string]Message{}, byToken: map[string]Person{}, byRef: map[string]Person{}, plans: map[string]OpenPlan{}, refBySpeaker: map[string]string{}}
	loc := in.Location
	if loc == nil {
		loc = time.UTC
	}
	maxRunes := in.MaxRunes
	if maxRunes <= 0 {
		maxRunes = 400
	}
	p.labelOf = in.LabelOf
	tokenOf := map[string]string{}
	lines := make([]conversation.Line, 0, len(in.Lines))
	for _, m := range in.Lines {
		p.byID[m.ID] = m
		name := m.Author
		if !m.Bot {
			spk := m.Speaker()
			tok, ok := tokenOf[spk]
			if !ok {
				ref := in.RefOf(m)
				per := p.person(ref, m.Author)
				p.refBySpeaker[spk] = ref
				tok = per.Token
				tokenOf[spk] = tok
			}
			name = m.Author + "(" + tok + ")"
		}
		lines = append(lines, conversation.Line{ID: m.ID, Author: name, Bot: m.Bot, ReplyTo: m.ReplyTo, At: m.At, Text: m.Text})
	}
	text, ids := conversation.Numbered(lines, in.BotName, maxRunes)
	p.Lines, p.labels = text, ids
	plans := in.Plans
	if len(plans) > MaxPlans {
		plans = plans[:MaxPlans]
	}
	var pl []string
	for i, op := range plans {
		label := "e" + strconv.Itoa(i+1)
		p.plans[label] = op
		title := p.tokensFor(op.Title)
		row := "[" + label + "] " + title + " (" + op.Status
		if op.Due != "" {
			row += ", " + op.Due
		}
		if op.WhenText != "" {
			row += ", " + op.WhenText
		}
		pl = append(pl, row+")")
	}
	p.Plans = strings.Join(pl, "\n")
	if p.Plans == "" {
		p.Plans = "-"
	}
	p.Now = in.Now.In(loc).Format("2006-01-02 15:04 (Mon) MST")
	return p
}

var storedRef = regexp.MustCompile(`\{@([^{}]+)\}`)

// person is the prompt's person for a stored reference, added with the next token when new.
func (p *Prompt) person(ref, label string) Person {
	if per, ok := p.byRef[ref]; ok {
		return per
	}
	per := Person{Token: "p" + strconv.Itoa(len(p.people)+1), Ref: ref, Label: label}
	p.people = append(p.people, per)
	p.byToken[per.Token] = per
	p.byRef[ref] = per
	return per
}

// personOf is person for a reference that is not among the lines (named by Input.LabelOf, else "?").
func (p *Prompt) personOf(ref string) Person {
	label := ""
	if _, ok := p.byRef[ref]; !ok && p.labelOf != nil {
		label = p.labelOf(ref)
	}
	if label == "" {
		label = "?"
	}
	return p.person(ref, label)
}

// tokensFor turns stored {@ref} back into the prompt's {pN} tokens (adding people the lines lack).
func (p *Prompt) tokensFor(text string) string {
	return storedRef.ReplaceAllStringFunc(text, func(m string) string {
		return "{" + p.personOf(storedRef.FindStringSubmatch(m)[1]).Token + "}"
	})
}

// People lists the prompt's people as "pN: name" lines.
func (p Prompt) People() string {
	var people []string
	for _, per := range p.people {
		people = append(people, per.Token+": "+per.Label)
	}
	return strings.Join(people, "\n")
}

// StoredText is a prompt-free rendering of stored text: {@ref} replaced by the name labelOf gives (or
// "someone"). Read paths use it to show items.
func StoredText(text string, labelOf func(string) string, unknown string) string {
	return storedRef.ReplaceAllStringFunc(text, func(m string) string {
		ref := storedRef.FindStringSubmatch(m)[1]
		if labelOf != nil {
			if l := labelOf(ref); l != "" {
				return l
			}
		}
		return unknown
	})
}

// TextRefs lists the references a stored text mentions.
func TextRefs(text string) []string {
	var out []string
	for _, m := range storedRef.FindAllStringSubmatch(text, -1) {
		out = append(out, m[1])
	}
	return out
}

// ---------------------------------------------------------------------------
// the model's answer
// ---------------------------------------------------------------------------

// Op is one validated operation.
type Op struct {
	Op           string // create | update | close
	LinkTo       string // the episode ID an update or close applies to
	LinkRevision int64  // the revision the model saw
	Kind         string
	Title        string // stored text ({@ref}); "" on an update that keeps the title
	Status       string // as asked (before the status machine)
	Due          string // YYYY-MM-DD or YYYY-MM-DDTHH:MM, "" when unknown
	WhenText     string
	Importance   int
	Items        []Item
	Participants []string // references of the human authors of the evidence
	Evidence     []string // all evidence message IDs
}

// Item is one validated item.
type Item struct {
	Kind     string
	Text     string   // stored text: people as {@ref}
	Evidence []string // message IDs, at least one
	By       []string // references of who said or decided it
}

// Result is the validated answer. Problems name what was rejected (no message text).
type Result struct {
	Ops      []Op
	Problems []string
}

// Clean reports whether nothing was rejected.
func (r Result) Clean() bool { return len(r.Problems) == 0 }

type rawItem struct {
	Kind string          `json:"kind"`
	Text string          `json:"text"`
	By   json.RawMessage `json:"by"`
	Ev   []string        `json:"ev"`
}

type rawOp struct {
	Op         string    `json:"op"`
	LinkTo     *string   `json:"link_to"`
	Kind       string    `json:"kind"`
	Title      string    `json:"title"`
	Items      []rawItem `json:"items"`
	Status     string    `json:"status"`
	Due        *string   `json:"due"`
	WhenText   string    `json:"when_text"`
	Importance float64   `json:"importance"`
}

// ErrParse means the answer was not the expected JSON at all.
var ErrParse = errors.New("episodes: the answer is not the expected JSON")

// JSONObject cuts the outermost JSON object out of a model answer (code fences, a leading sentence).
func JSONObject(raw string) (string, bool) {
	i, j := strings.IndexByte(raw, '{'), strings.LastIndexByte(raw, '}')
	if i < 0 || j <= i {
		return "", false
	}
	return raw[i : j+1], true
}

var promptToken = regexp.MustCompile(`\{(p\d+)\}`)

// Parse validates an extraction answer against the prompt. It never trusts the model: every item needs
// evidence that was in the input and is not the bot's, a person said to have said or decided something
// must be an author of its evidence, a decision needs two authors or the person confirming their own,
// texts must fit, people must be known. Rejected items and operations are listed in Problems.
func (p Prompt) Parse(raw string) (Result, error) {
	obj, ok := JSONObject(raw)
	if !ok {
		return Result{}, ErrParse
	}
	var doc struct {
		Episodes *[]rawOp `json:"episodes"`
	}
	if err := json.Unmarshal([]byte(obj), &doc); err != nil || doc.Episodes == nil {
		return Result{}, ErrParse
	}
	var res Result
	usedLinks := map[string]bool{}
	for i, ro := range *doc.Episodes {
		op, problems := p.validateOp(ro, usedLinks)
		for _, pr := range problems {
			res.Problems = append(res.Problems, fmt.Sprintf("episode %d: %s", i+1, pr))
		}
		if op != nil {
			res.Ops = append(res.Ops, *op)
		}
	}
	return res, nil
}

func (p Prompt) validateOp(ro rawOp, usedLinks map[string]bool) (*Op, []string) {
	var problems []string
	op := &Op{Op: strings.TrimSpace(ro.Op), Kind: strings.TrimSpace(ro.Kind), Status: strings.TrimSpace(ro.Status), Importance: int(ro.Importance)}
	switch op.Op {
	case "create", "update", "close":
	default:
		return nil, []string{"unknown op"}
	}
	link := ""
	if ro.LinkTo != nil {
		link = strings.TrimSpace(*ro.LinkTo)
	}
	switch op.Op {
	case "create":
		if link != "" {
			return nil, []string{"create with link_to"}
		}
		if !contains(EpisodeKinds, op.Kind) {
			return nil, []string{"unknown kind"}
		}
	default:
		plan, ok := p.plans[link]
		if !ok {
			return nil, []string{"link_to is not an offered plan"}
		}
		if usedLinks[link] {
			return nil, []string{"plan linked twice"}
		}
		usedLinks[link] = true
		op.LinkTo, op.LinkRevision = plan.ID, plan.Revision
		op.Kind = ""
	}
	if t := strings.TrimSpace(ro.Title); t != "" {
		title, err := p.storeText(t, MaxText)
		if err != "" {
			if op.Op == "create" {
				return nil, []string{"title: " + err}
			}
			problems = append(problems, "title: "+err)
		} else {
			op.Title = title
		}
	} else if op.Op == "create" {
		return nil, []string{"title missing"}
	}
	if op.Importance < 1 {
		op.Importance = 2
	}
	op.Importance = min(op.Importance, 5)
	if ro.Due != nil {
		op.Due = NormalizeDue(*ro.Due)
	}
	if w := strings.TrimSpace(ro.WhenText); w != "" {
		if utf8.RuneCountInString(w) <= MaxWhenText && !strings.ContainsAny(w, "{}") {
			op.WhenText = w
		} else {
			problems = append(problems, "when_text too long")
		}
	}
	authors := map[string]bool{}
	evidence := map[string]bool{}
	for j, ri := range ro.Items {
		it, err := p.validateItem(ri)
		if err != "" {
			problems = append(problems, fmt.Sprintf("item %d: %s", j+1, err))
			continue
		}
		op.Items = append(op.Items, it)
		for _, id := range it.Evidence {
			if !evidence[id] {
				evidence[id] = true
				op.Evidence = append(op.Evidence, id)
			}
			ref := p.refOfLine(id)
			if ref != "" && !authors[ref] {
				authors[ref] = true
				op.Participants = append(op.Participants, ref)
			}
		}
	}
	switch op.Op {
	case "create":
		if len(op.Items) == 0 {
			return nil, append(problems, "no valid item")
		}
		switch op.Status {
		case "", "open":
			op.Status = Open
		case Decided, Done:
		default:
			problems = append(problems, "status not allowed on create")
			op.Status = Open
		}
	case "update":
		if len(op.Items) == 0 && op.Status == "" && op.Due == "" && op.Title == "" {
			return nil, append(problems, "empty update")
		}
		if op.Status == Dropped || op.Status == Expired {
			problems = append(problems, "update cannot drop or expire")
			op.Status = ""
		}
	case "close":
		if op.Status != Done && op.Status != Dropped {
			return nil, append(problems, "close needs status done or dropped")
		}
		if len(op.Items) == 0 {
			return nil, append(problems, "close without evidence")
		}
	}
	return op, problems
}

func (p Prompt) validateItem(ri rawItem) (Item, string) {
	it := Item{Kind: strings.TrimSpace(ri.Kind)}
	if !contains(ItemKinds, it.Kind) {
		return it, "unknown kind"
	}
	text, err := p.storeText(strings.TrimSpace(ri.Text), MaxText)
	if err != "" {
		return it, err
	}
	it.Text = text
	if len(ri.Ev) == 0 {
		return it, "no evidence"
	}
	authors := map[string]bool{}
	seen := map[string]bool{}
	for _, label := range ri.Ev {
		label = strings.Trim(strings.TrimSpace(label), "[]")
		id, ok := p.labels[label]
		if !ok {
			return it, "evidence not in the input"
		}
		if p.byID[id].Bot {
			return it, "evidence is the bot's line"
		}
		if !seen[id] {
			seen[id] = true
			it.Evidence = append(it.Evidence, id)
		}
		authors[p.refOfLine(id)] = true
	}
	by, ok := tokenList(ri.By)
	if !ok {
		return it, "by is not a list of people"
	}
	for _, tok := range by {
		per, known := p.byToken[tok]
		if !known {
			return it, "unknown person in by"
		}
		if !authors[per.Ref] {
			return it, "by is not an author of the evidence"
		}
		it.By = append(it.By, per.Ref)
	}
	if it.Kind == "decision" {
		single := len(authors) == 1 && len(it.By) == 1 && authors[it.By[0]]
		if len(authors) < 2 && !single {
			return it, "decision without two people or the person's own confirmation"
		}
	}
	return it, ""
}

// storeText checks a model text and maps its {pN} tokens to stored {@ref}.
func (p Prompt) storeText(t string, limit int) (string, string) {
	if t == "" {
		return "", "empty text"
	}
	if utf8.RuneCountInString(t) > limit {
		return "", "text too long"
	}
	if strings.Contains(t, "{@") {
		return "", "stored reference in text"
	}
	bad := ""
	out := promptToken.ReplaceAllStringFunc(t, func(m string) string {
		per, ok := p.byToken[promptToken.FindStringSubmatch(m)[1]]
		if !ok {
			bad = "unknown person token"
			return m
		}
		return "{@" + per.Ref + "}"
	})
	if bad != "" {
		return "", bad
	}
	return out, ""
}

func (p Prompt) refOfLine(id string) string {
	m, ok := p.byID[id]
	if !ok || m.Bot {
		return ""
	}
	return p.refBySpeaker[m.Speaker()]
}

// tokenList reads "by"/"who": a list of "p1" or "{p1}" (null and a single string are accepted too).
func tokenList(raw json.RawMessage) ([]string, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, true
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil {
		var one string
		if err := json.Unmarshal(raw, &one); err != nil {
			return nil, false
		}
		list = []string{one}
	}
	var out []string
	for _, t := range list {
		t = strings.Trim(strings.TrimSpace(t), "{}")
		if t != "" {
			out = append(out, t)
		}
	}
	return out, true
}

// NormalizeDue keeps a date the model computed when it is well formed: YYYY-MM-DD or YYYY-MM-DDTHH:MM.
func NormalizeDue(s string) string {
	s = strings.TrimSpace(s)
	for _, layout := range []string{"2006-01-02", "2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			if layout == "2006-01-02" {
				return t.Format("2006-01-02")
			}
			return t.Format("2006-01-02T15:04")
		}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Format("2006-01-02T15:04")
	}
	return ""
}

// DueTime reads a stored due in loc; a date alone is the end of that day.
func DueTime(due string, loc *time.Location) (time.Time, bool) {
	if loc == nil {
		loc = time.UTC
	}
	if t, err := time.ParseInLocation("2006-01-02T15:04", due, loc); err == nil {
		return t, true
	}
	if t, err := time.ParseInLocation("2006-01-02", due, loc); err == nil {
		return t.Add(24*time.Hour - time.Second), true
	}
	return time.Time{}, false
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
