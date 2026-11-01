package episodes

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// State is a room's live state (L1, docs/plan-memory-conversation.md §5.4.2): the topics in progress,
// questions nobody answered, the mood and running jokes. Every entry carries the observation IDs it came
// from; people are stored references and texts use {@ref} for people.
type State struct {
	Threads       []Thread   `json:"threads"`
	OpenQuestions []Question `json:"open_questions"`
	Mood          *Note      `json:"mood,omitempty"`
	Banter        *Note      `json:"banter,omitempty"`
}

// Thread is a topic in progress.
type Thread struct {
	Label  string   `json:"label"`
	Gist   string   `json:"gist,omitempty"`
	Who    []string `json:"who,omitempty"`
	Status string   `json:"status"` // active | cooling
	Last   string   `json:"last"`   // the latest evidence message
	LastAt string   `json:"last_at"`
	Ev     []string `json:"ev"`
}

// Question is a question nobody answered yet.
type Question struct {
	Q      string   `json:"q"`
	Who    []string `json:"who,omitempty"`
	LastAt string   `json:"last_at"`
	Ev     []string `json:"ev"`
}

// Note is the mood or a running joke.
type Note struct {
	Text   string   `json:"text"`
	LastAt string   `json:"last_at"`
	Ev     []string `json:"ev"`
}

// MaxThreads is how many topics the state keeps.
const MaxThreads = 4

// Ageing of the state: a topic cools when nobody mentioned it for CoolAfter and leaves the state after
// DropAfter (its content stays in the episodes). Questions and notes leave after DropAfter as well.
const (
	CoolAfter = 30 * time.Minute
	DropAfter = 3 * time.Hour
)

// Evidence lists every observation ID the state cites.
func (s State) Evidence() []string {
	seen := map[string]bool{}
	var out []string
	add := func(ids []string) {
		for _, id := range ids {
			if id != "" && !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	for _, t := range s.Threads {
		add(t.Ev)
	}
	for _, q := range s.OpenQuestions {
		add(q.Ev)
	}
	if s.Mood != nil {
		add(s.Mood.Ev)
	}
	if s.Banter != nil {
		add(s.Banter.Ev)
	}
	return out
}

// Refs lists every person reference the state names.
func (s State) Refs() []string {
	seen := map[string]bool{}
	var out []string
	add := func(refs []string) {
		for _, r := range refs {
			if r != "" && !seen[r] {
				seen[r] = true
				out = append(out, r)
			}
		}
	}
	for _, t := range s.Threads {
		add(t.Who)
		add(TextRefs(t.Label + " " + t.Gist))
	}
	for _, q := range s.OpenQuestions {
		add(q.Who)
		add(TextRefs(q.Q))
	}
	for _, n := range []*Note{s.Mood, s.Banter} {
		if n != nil {
			add(TextRefs(n.Text))
		}
	}
	return out
}

// Empty reports whether nothing is in progress.
func (s State) Empty() bool {
	return len(s.Threads) == 0 && len(s.OpenQuestions) == 0 && s.Mood == nil && s.Banter == nil
}

// Age cools and drops what nobody mentioned for a while.
func (s *State) Age(now time.Time) {
	old := func(at string, d time.Duration) bool {
		t, err := time.Parse(time.RFC3339, at)
		return err != nil || now.Sub(t) >= d
	}
	threads := s.Threads[:0:0]
	for _, t := range s.Threads {
		if old(t.LastAt, DropAfter) {
			continue
		}
		if old(t.LastAt, CoolAfter) {
			t.Status = "cooling"
		}
		threads = append(threads, t)
	}
	s.Threads = threads
	questions := s.OpenQuestions[:0:0]
	for _, q := range s.OpenQuestions {
		if !old(q.LastAt, DropAfter) {
			questions = append(questions, q)
		}
	}
	s.OpenQuestions = questions
	if s.Mood != nil && old(s.Mood.LastAt, DropAfter) {
		s.Mood = nil
	}
	if s.Banter != nil && old(s.Banter.LastAt, DropAfter) {
		s.Banter = nil
	}
}

// carried is an earlier state entry the model may continue ("from": "t1").
type carried struct {
	who    []string
	ev     []string
	last   string
	lastAt string
}

// Previous renders an earlier state for an incremental update: topics as [tN], questions as [qN], people
// as the prompt's tokens. It returns "-" when there is nothing.
func (p *Prompt) Previous(prev State) string {
	p.prev = map[string]carried{}
	var rows []string
	toks := func(refs []string) string {
		var t []string
		for _, r := range refs {
			t = append(t, p.personOf(r).Token)
		}
		return strings.Join(t, ", ")
	}
	for i, t := range prev.Threads {
		label := "t" + strconv.Itoa(i+1)
		p.prev[label] = carried{who: t.Who, ev: t.Ev, last: t.Last, lastAt: t.LastAt}
		row := "[" + label + "] " + p.tokensFor(t.Label)
		if t.Gist != "" {
			row += " — " + p.tokensFor(t.Gist)
		}
		rows = append(rows, row+" ("+toks(t.Who)+"; "+t.Status+")")
	}
	for i, q := range prev.OpenQuestions {
		label := "q" + strconv.Itoa(i+1)
		p.prev[label] = carried{who: q.Who, ev: q.Ev, lastAt: q.LastAt}
		rows = append(rows, "["+label+"] ? "+p.tokensFor(q.Q)+" ("+toks(q.Who)+")")
	}
	if prev.Mood != nil {
		rows = append(rows, "mood: "+p.tokensFor(prev.Mood.Text))
	}
	if prev.Banter != nil {
		rows = append(rows, "banter: "+p.tokensFor(prev.Banter.Text))
	}
	if len(rows) == 0 {
		return "-"
	}
	return strings.Join(rows, "\n")
}

type rawThread struct {
	From   *string         `json:"from"`
	Label  string          `json:"label"`
	Gist   string          `json:"gist"`
	Who    json.RawMessage `json:"who"`
	Status string          `json:"status"`
	Ev     []string        `json:"ev"`
}

type rawQuestion struct {
	From *string         `json:"from"`
	Q    string          `json:"q"`
	Who  json.RawMessage `json:"who"`
	Ev   []string        `json:"ev"`
}

type rawNote struct {
	Text string   `json:"text"`
	Ev   []string `json:"ev"`
}

// ParseState validates a room-state answer like Parse does for episodes: every entry needs evidence from
// the input that is not the bot's (or continues an earlier entry, keeping that entry's evidence), people
// must be known, texts must fit, at most MaxThreads topics.
func (p Prompt) ParseState(raw string) (State, []string, error) {
	obj, ok := JSONObject(raw)
	if !ok {
		return State{}, nil, ErrParse
	}
	var doc struct {
		Threads   *[]rawThread  `json:"threads"`
		Questions []rawQuestion `json:"open_questions"`
		Mood      *rawNote      `json:"mood"`
		Banter    *rawNote      `json:"banter"`
	}
	if err := json.Unmarshal([]byte(obj), &doc); err != nil || doc.Threads == nil {
		return State{}, nil, ErrParse
	}
	var st State
	var problems []string
	for i, rt := range *doc.Threads {
		if len(st.Threads) == MaxThreads {
			problems = append(problems, fmt.Sprintf("thread %d: more than %d topics", i+1, MaxThreads))
			continue
		}
		label, err := p.storeText(strings.TrimSpace(rt.Label), MaxLabel)
		if err != "" {
			problems = append(problems, fmt.Sprintf("thread %d: label: %s", i+1, err))
			continue
		}
		t := Thread{Label: label, Status: "active"}
		if rt.Status == "cooling" {
			t.Status = "cooling"
		}
		if g := strings.TrimSpace(rt.Gist); g != "" {
			if t.Gist, err = p.storeText(g, MaxText); err != "" {
				problems = append(problems, fmt.Sprintf("thread %d: gist: %s", i+1, err))
				continue
			}
		}
		ev, last, lastAt, who, err := p.stateEvidence(rt.Ev, rt.From, rt.Who)
		if err != "" {
			problems = append(problems, fmt.Sprintf("thread %d: %s", i+1, err))
			continue
		}
		t.Ev, t.Last, t.LastAt, t.Who = ev, last, lastAt, who
		st.Threads = append(st.Threads, t)
	}
	for i, rq := range doc.Questions {
		q, err := p.storeText(strings.TrimSpace(rq.Q), MaxText)
		if err != "" {
			problems = append(problems, fmt.Sprintf("question %d: %s", i+1, err))
			continue
		}
		ev, _, lastAt, who, err := p.stateEvidence(rq.Ev, rq.From, rq.Who)
		if err != "" {
			problems = append(problems, fmt.Sprintf("question %d: %s", i+1, err))
			continue
		}
		st.OpenQuestions = append(st.OpenQuestions, Question{Q: q, Who: who, LastAt: lastAt, Ev: ev})
	}
	note := func(name string, rn *rawNote) *Note {
		if rn == nil || strings.TrimSpace(rn.Text) == "" {
			return nil
		}
		text, err := p.storeText(strings.TrimSpace(rn.Text), MaxText)
		if err != "" {
			problems = append(problems, name+": "+err)
			return nil
		}
		ev, _, lastAt, _, err := p.stateEvidence(rn.Ev, nil, nil)
		if err != "" {
			problems = append(problems, name+": "+err)
			return nil
		}
		return &Note{Text: text, LastAt: lastAt, Ev: ev}
	}
	st.Mood, st.Banter = note("mood", doc.Mood), note("banter", doc.Banter)
	return st, problems, nil
}

// stateEvidence validates the labels of one state entry, adding what an earlier entry ("from") had.
func (p Prompt) stateEvidence(labels []string, from *string, whoRaw json.RawMessage) (ev []string, last, lastAt string, who []string, problem string) {
	seen := map[string]bool{}
	var latest time.Time
	for _, label := range labels {
		id, ok := p.labels[strings.Trim(strings.TrimSpace(label), "[]")]
		if !ok {
			return nil, "", "", nil, "evidence not in the input"
		}
		m := p.byID[id]
		if m.Bot {
			return nil, "", "", nil, "evidence is the bot's line"
		}
		if !seen[id] {
			seen[id] = true
			ev = append(ev, id)
		}
		if !m.At.Before(latest) {
			latest, last = m.At, id
		}
	}
	if !latest.IsZero() {
		lastAt = latest.UTC().Format(time.RFC3339)
	}
	whoSeen := map[string]bool{}
	if from != nil && *from != "" {
		c, ok := p.prev[strings.TrimSpace(*from)]
		if !ok {
			return nil, "", "", nil, "from is not an earlier entry"
		}
		for _, id := range c.ev {
			if !seen[id] {
				seen[id] = true
				ev = append(ev, id)
			}
		}
		if last == "" {
			last, lastAt = c.last, c.lastAt
		}
		for _, r := range c.who {
			if !whoSeen[r] {
				whoSeen[r] = true
				who = append(who, r)
			}
		}
	}
	if len(ev) == 0 {
		return nil, "", "", nil, "no evidence"
	}
	toks, ok := tokenList(whoRaw)
	if !ok {
		return nil, "", "", nil, "who is not a list of people"
	}
	for _, tok := range toks {
		per, known := p.byToken[tok]
		if !known {
			return nil, "", "", nil, "unknown person in who"
		}
		if !whoSeen[per.Ref] {
			whoSeen[per.Ref] = true
			who = append(who, per.Ref)
		}
	}
	return ev, last, lastAt, who, ""
}

// Schedule decides when a room's state is updated (debounce) and when it is made again from the raw
// messages instead of updated (drift cut).
type Schedule struct {
	Quiet     time.Duration // the room is this quiet ... (3m)
	MinNew    int           // ... and has at least this many new messages (5)
	BusyEvery int           // or this many new messages arrived, quiet or not (40)
	FullEvery int           // every this many updates the state is made from the raw messages (6)
}

func (s Schedule) withDefaults() Schedule {
	if s.Quiet <= 0 {
		s.Quiet = 3 * time.Minute
	}
	if s.MinNew <= 0 {
		s.MinNew = 5
	}
	if s.BusyEvery <= 0 {
		s.BusyEvery = 40
	}
	if s.BusyEvery < s.MinNew {
		s.BusyEvery = s.MinNew
	}
	if s.FullEvery <= 0 {
		s.FullEvery = 6
	}
	return s
}

// Due reports whether a room with newMsgs messages since the last update, the last at last, is updated now.
func (s Schedule) Due(newMsgs int, last, now time.Time) bool {
	s = s.withDefaults()
	return newMsgs >= s.BusyEvery || (newMsgs >= s.MinNew && now.Sub(last) >= s.Quiet)
}

// Full reports whether the update after updates earlier ones regenerates the state from the raw messages.
func (s Schedule) Full(updates int) bool {
	s = s.withDefaults()
	return updates%s.FullEvery == 0
}
