package chat

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"masamong/internal/lang"
	"masamong/internal/memory/corpus"
	"masamong/internal/memory/search"
)

// ComposeSystem joins persona, channel rules, the global agent prompt and the fixed rule blocks of
// the voice's language. An empty persona uses the pack's default persona. Masamong is a friend, not
// a fact-checker: the pack's memory rules weave memories in casually, shrug off gaps, never narrate
// retrieval, and keep tool results factual.
func ComposeSystem(v Voice, persona, rules, agent string) string {
	c := v.pack().Chat
	if strings.TrimSpace(persona) == "" {
		persona = c.DefaultPersona
	}
	parts := []string{strings.TrimSpace(lang.Fill(persona, "bot", v.bot()))}
	if r := strings.TrimSpace(rules); r != "" {
		parts = append(parts, r)
	}
	if a := strings.TrimSpace(agent); a != "" {
		parts = append(parts, a)
	}
	parts = append(parts, c.ReplyLanguageRule, c.MemoryRules, c.SecurityRules)
	return strings.Join(parts, "\n\n")
}

// ToolResult is one executed tool (Action result subset).
type ToolResult struct {
	Tool   string         `json:"tool"`
	OK     bool           `json:"ok"`
	Status string         `json:"status,omitempty"`
	Data   map[string]any `json:"data,omitempty"`
	Error  string         `json:"error,omitempty"`
}

// toolBlockMax is how much of the answer prompt the tool results may take, in runes.
const toolBlockMax = 3500

// toolJSON renders the tool results for the answer prompt. When they do not fit in toolBlockMax, every
// result keeps its tool, ok, status and error, and the data that does not fit is cut from its end, the
// way a reader stops reading: a list loses its last items first, a text its last words ("…"). The block
// stays valid JSON, the results keep the order the service ranked them in (the first search hits stay
// whole), and one tool's long output no longer hides the outcome of the tools after it. Small results
// keep all their data; the room left goes to the large ones. Results that fit are rendered as before.
func toolJSON(usable, other []ToolResult) string {
	raw, _ := json.Marshal(map[string]any{"usable": usable, "not_success": other})
	if utf8.RuneCount(raw) <= toolBlockMax {
		return string(raw)
	}
	all := append(append([]ToolResult(nil), usable...), other...)
	// the envelopes first (tool, ok, status, error); the data shares what is left
	envs := make([]map[string]any, len(all))
	sizes := make([]int, len(all))
	for i, t := range all {
		m := map[string]any{"tool": t.Tool, "ok": t.OK}
		if t.Status != "" {
			m["status"] = t.Status
		}
		if t.Error != "" {
			e := []rune(t.Error)
			m["error"] = string(e[:min(200, len(e))])
		}
		envs[i] = m
		if len(t.Data) > 0 {
			sizes[i] = runeLen(t.Data)
		}
	}
	block := func(shares []int) string {
		for i, t := range all {
			m := envs[i]
			delete(m, "data")
			delete(m, "truncated")
			if len(t.Data) == 0 {
				continue
			}
			if sizes[i] <= shares[i] {
				m["data"] = t.Data
			} else if shares[i] > 0 {
				m["data"], m["truncated"] = fitTail(t.Data, shares[i]), true
			} else {
				m["truncated"] = true
			}
		}
		out, _ := json.Marshal(map[string]any{"usable": envs[:len(usable)], "not_success": envs[len(usable):]})
		return string(out)
	}
	empty := make([]int, len(all))
	room := toolBlockMax - utf8.RuneCountInString(block(empty)) - 16*len(all) // "data":…,"truncated":true
	for room > 0 {
		shares := fillShares(sizes, room)
		out := block(shares)
		if n := utf8.RuneCountInString(out); n <= toolBlockMax {
			return out
		} else {
			room -= n - toolBlockMax
		}
	}
	out := []rune(block(empty))
	return string(out[:min(toolBlockMax, len(out))])
}

// fillShares splits room between results: those smaller than an even share keep everything, the rest
// split what remains evenly.
func fillShares(sizes []int, room int) []int {
	shares := make([]int, len(sizes))
	left, open := room, 0
	for _, n := range sizes {
		if n > 0 {
			open++
		}
	}
	done := make([]bool, len(sizes))
	for open > 0 {
		even := left / open
		settled := false
		for i, n := range sizes {
			if n > 0 && !done[i] && n <= even {
				shares[i], done[i], left, open, settled = n, true, left-n, open-1, true
			}
		}
		if !settled {
			for i, n := range sizes {
				if n > 0 && !done[i] {
					shares[i] = even
				}
			}
			break
		}
	}
	return shares
}

func runeLen(v any) int {
	raw, _ := json.Marshal(v)
	return utf8.RuneCount(raw)
}

// fitTail returns v cut from its end until its JSON takes at most budget runes: a list drops its last
// items (one is kept and cut instead), an object cuts its largest value first, a text loses its end.
func fitTail(v any, budget int) any {
	if runeLen(v) <= budget {
		return v
	}
	switch x := v.(type) {
	case string:
		r := []rune(x)
		over := runeLen(x) - budget
		keep := len(r) - over - 1 // room for "…"
		if keep <= 0 {
			return "…"
		}
		return string(r[:keep]) + "…"
	case []any:
		items := append([]any(nil), x...)
		for len(items) > 1 && runeLen(items) > budget {
			items = items[:len(items)-1]
		}
		if runeLen(items) > budget {
			items[0] = fitTail(items[0], budget-2)
		} else if len(items) < len(x) {
			// the dropped item's room may hold the start of the next one
			if rest := budget - runeLen(items) - 1; rest > 40 {
				items = append(items, fitTail(x[len(items)], rest))
				if runeLen(items) > budget {
					items = items[:len(items)-1]
				}
			}
		}
		return items
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, val := range x {
			m[k] = val
		}
		for runeLen(m) > budget {
			big, size := "", 0
			for k, val := range m {
				if n := runeLen(val); n > size || (n == size && k < big) {
					big, size = k, n
				}
			}
			if size <= 3 {
				break
			}
			m[big] = fitTail(m[big], max(1, size-(runeLen(m)-budget)))
			if runeLen(m[big]) >= size { // could not shrink further
				delete(m, big)
			}
		}
		return m
	default:
		// typed lists and structs (a tool's []map[string]any) are cut as their JSON shape
		if raw, err := json.Marshal(x); err == nil {
			var generic any
			if json.Unmarshal(raw, &generic) == nil {
				switch generic.(type) {
				case map[string]any, []any:
					return fitTail(generic, budget)
				}
			}
		}
		return x
	}
}

// AnswerInput is everything the answer prompt needs.
type AnswerInput struct {
	System     string
	History    []Turn
	MaxHistory int
	Hits       []search.Hit
	RagState   string
	Tools      []ToolResult
	Speaker    string
	Message    string
	Intent     search.Intent
	RecentRoom []corpus.Message // ambient recent messages of the channel (not addressed to the bot)
	Summary    string
	// RoomMemory is the rendered room memory (roommemory.go); "" leaves the prompt as evaluated.
	RoomMemory string
	// Now is the current time in the channel's zone; zero leaves the prompt as evaluated (no time line,
	// no line ages)
	Now time.Time
}

// BuildAnswerMessages is engine._build_chat_messages with the friend framing.
func BuildAnswerMessages(v Voice, in AnswerInput) []Msg {
	c := v.pack().Chat
	var bits []string
	if in.Intent.Recent {
		bits = append(bits, "recent")
	}
	if in.Intent.Intro {
		bits = append(bits, "intro")
	}
	if in.Intent.Kind != "" && in.Intent.Kind != "other" && in.Intent.Kind != "person_intro" {
		bits = append(bits, in.Intent.Kind)
	}
	hint := "general"
	if len(bits) > 0 {
		hint = strings.Join(bits, ",")
	}
	var evidence []string
	for i, h := range in.Hits {
		speakers := strings.Join(h.Speakers, ", ")
		if speakers == "" {
			speakers = c.Unknown
		}
		date := h.Date
		if date == "" {
			date = c.Unknown
		}
		evidence = append(evidence, lang.Fill(c.EvidenceLine, "i", strconv.Itoa(i+1), "date", date, "speakers", speakers)+"\n"+h.Excerpt)
	}
	memory := strings.Join(evidence, "\n\n")
	if memory != "" && strings.TrimSpace(c.MemoryTail) != "" {
		memory += "\n\n" + c.MemoryTail
	}
	if memory == "" {
		note, ok := c.RagNotes[in.RagState]
		if !ok {
			note = c.RagNotes["empty_in_allowed_scope"]
		}
		memory = note
	}
	var summaries []string
	for i, h := range in.Hits {
		if h.Summary != "" {
			summaries = append(summaries, lang.Fill(c.SummaryLine, "i", strconv.Itoa(i+1), "summary", h.Summary))
		}
	}
	summaryBlock := strings.Join(summaries, "\n")
	if summaryBlock == "" {
		summaryBlock = c.None
	}
	toolBlock := c.None
	if len(in.Tools) > 0 {
		var usable, other []ToolResult
		for _, t := range cleanToolResults(in.Tools) { // tool and web text is untrusted (untrusted.go)
			if t.OK {
				usable = append(usable, t)
			} else {
				other = append(other, t)
			}
		}
		toolBlock = toolJSON(usable, other)
		if assumedValues(in.Tools) && strings.TrimSpace(c.ToolAssumed) != "" {
			// a tool filled a value nobody said (the weather's area): the reply says so (principle 7)
			toolBlock += "\n" + c.ToolAssumed
		}
		if queuedResults(in.Tools) && strings.TrimSpace(c.ToolQueued) != "" {
			// a picture is being drawn in the background: one short line, nothing described as done
			toolBlock += "\n" + c.ToolQueued
		}
		if scheduleResults(in.Tools) && strings.TrimSpace(c.ToolSchedule) != "" {
			// a reminder or task: ask what is still missing, confirm only what the result says was set
			toolBlock += "\n" + c.ToolSchedule
		}
		if searchResults(in.Tools) && strings.TrimSpace(c.ToolConclusion) != "" {
			// a web search answered: the conclusion comes first, sources are not listed one by one
			toolBlock += "\n" + c.ToolConclusion
		}
	}
	var recent []string
	for _, m := range in.RecentRoom {
		c := []rune(m.Content)
		recent = append(recent, m.ActorLabel+": "+string(c[:min(300, len(c))]))
	}
	recentBlock := strings.Join(recent, "\n")
	if recentBlock == "" {
		recentBlock = c.None
	}
	who := strings.TrimSpace(in.Speaker)
	if who == "" {
		who = c.DefaultSpeaker
	}
	msgs := []Msg{{Role: "system", Content: in.System}}
	hist := in.History
	if in.MaxHistory > 0 && len(hist) > in.MaxHistory {
		hist = hist[len(hist)-in.MaxHistory:]
	}
	for _, t := range hist {
		c := strings.TrimSpace(t.Content)
		if (t.Role == "user" || t.Role == "assistant") && c != "" {
			r := []rune(c)
			if t.Role == "user" && t.Speaker != "" {
				c = t.Speaker + lineAge(v, in.Now, t.At) + ": " + string(r[:min(1500, len(r))])
			} else {
				c = string(r[:min(1500, len(r))])
			}
			msgs = append(msgs, Msg{Role: t.Role, Content: c})
		}
	}
	user := lang.Fill(withRoomMemory(c.AnswerTemplate, in.RoomMemory), "room_memory", in.RoomMemory, "who", who, "hint", hint, "recent", recentBlock, "session", nonEmpty(in.Summary, c.None),
		"tools", toolBlock, "summaries", summaryBlock, "memory", memory, "message", in.Message)
	if !in.Now.IsZero() && strings.TrimSpace(c.AnswerNow) != "" {
		user = answerNow(v, in.Now) + "\n" + user
	}
	msgs = append(msgs, Msg{Role: "user", Content: user})
	return msgs
}

// answerNow is the time line that heads the answer's user message.
func answerNow(v Voice, now time.Time) string {
	weekday := now.Weekday().String()
	if w := v.pack().Chat.Weekdays; len(w) == 7 {
		weekday = w[now.Weekday()]
	}
	return lang.Fill(v.pack().Chat.AnswerNow, "time", now.Format("2006-01-02 15:04"), "weekday", weekday, "tz", now.Location().String())
}

// lineAgeAfter is how old a line must be before its age is shown: the last few minutes read as now.
const lineAgeAfter = 5 * time.Minute

// lineAge is " (25분 전)" for a line said a while before now, "" for a recent one or an unknown time.
func lineAge(v Voice, now, at time.Time) string {
	if now.IsZero() || at.IsZero() {
		return ""
	}
	a, age := v.pack().Chat.Ages, now.Sub(at)
	form, n := a.Minutes, int(age/time.Minute)
	switch {
	case age < lineAgeAfter:
		return ""
	case age >= 24*time.Hour:
		form, n = a.Days, int(age/(24*time.Hour))
	case age >= time.Hour:
		form, n = a.Hours, int(age/time.Hour)
	}
	if strings.TrimSpace(form) == "" {
		return ""
	}
	return " (" + lang.Fill(form, "n", strconv.Itoa(n)) + ")"
}

// assumedValues reports whether a tool result carries a value the tool assumed (an assumed_* key).
func scheduleResults(results []ToolResult) bool {
	for _, r := range results {
		if strings.HasPrefix(r.Tool, "reminders.") {
			return true
		}
	}
	return false
}

// searchPrefixes are the tools that look something up on the web (a search, a page, a place): the
// replies people read most, and the ones that turn into a list of sources when nothing says otherwise.
var searchPrefixes = []string{"search.", "web.", "places."}

// searchResults reports whether a search tool returned a usable result.
func searchResults(results []ToolResult) bool {
	for _, r := range results {
		if !r.OK {
			continue
		}
		for _, p := range searchPrefixes {
			if strings.HasPrefix(r.Tool, p) {
				return true
			}
		}
	}
	return false
}

func queuedResults(results []ToolResult) bool {
	for _, r := range results {
		if r.OK && (r.Status == "queued" || r.Status == "already_queued") {
			return true
		}
	}
	return false
}

func assumedValues(results []ToolResult) bool {
	for _, r := range results {
		for k := range r.Data {
			if strings.HasPrefix(k, "assumed_") {
				return true
			}
		}
	}
	return false
}

func nonEmpty(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
