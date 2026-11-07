// Package chat is the conversational turn: planner (memory need, time, people, tools) → tools →
// channel memory search → a friend-like answer. It ports the test sandbox turn (planner_context.py,
// bot_plan.py, engine._build_chat_messages) that was evaluated before promotion.
//
// Boundaries kept from the sandbox contract: q0 (the user's words) is never rewritten; recent
// conversation is interpretation context, not fact; planner output cannot widen scope (room/channel/
// account/timezone keys are stripped); a damaged plan never turns into "search everything".
//
// No prompt text lives here: every string comes from the language pack (internal/lang) of the
// turn's Voice, and the tool menu comes from the product tool registry.
package chat

import (
	"encoding/json"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"masamong/internal/lang"
	"masamong/internal/memory/search"
)

// Voice is the language and name the bot speaks with. The bot name fills {bot} in pack templates.
type Voice struct {
	Pack    *lang.Pack
	BotName string
}

func (v Voice) pack() *lang.Pack {
	if v.Pack == nil {
		return lang.For(lang.Default)
	}
	return v.Pack
}

func (v Voice) bot() string {
	if strings.TrimSpace(v.BotName) == "" {
		return "masamong"
	}
	return v.BotName
}

// ToolInfo is one tool offered to the planner (product Action ID + planner text).
type ToolInfo struct {
	ID          string   `json:"id"`
	Description string   `json:"description"`
	Route       string   `json:"route,omitempty"`    // routing hint for planner rule 2
	Required    []string `json:"required,omitempty"` // parameters that must be real values
	// The native tool loop's view of the tool (toolloop.go). The memory planner's menu never shows
	// these, so its text stays the one the sandbox evaluated.
	Purpose string      `json:"-"` // one line; empty = Description
	When    string      `json:"-"` // when to use it
	Skip    string      `json:"-"` // when not to (and what instead)
	Params  []ToolParam `json:"-"`
	Writes  bool        `json:"-"` // creates state (a poll, a reminder, a picture)
	Once    bool        `json:"-"` // a call that succeeded closes it for the turn's later rounds (tools.Builtin.Once)
	Final   bool        `json:"-"` // its one result is the answer: see ToolLoop.Final (tools.Builtin.Final)
	Web     bool        `json:"-"` // returns text from the open web, which may carry planted instructions
	// Pays: the tool spends money of its own (a model call, a picture, a paid search), so under a money
	// cap the turn's later steps wait for it to be decided (runSteps).
	Pays bool `json:"-"`
}

// Toolset is what the planner may use in this turn. Enabled is offered in order; Known lists every
// tool the product has, so a plan naming a switched-off tool is reported as disabled, not unknown.
type Toolset struct {
	Enabled []ToolInfo
	Known   map[string]bool
}

func (ts Toolset) find(id string) (ToolInfo, bool) {
	for _, t := range ts.Enabled {
		if t.ID == id {
			return t, true
		}
	}
	return ToolInfo{}, false
}

// without returns the set minus one tool (it stays Known).
func (ts Toolset) without(id string) Toolset {
	out := Toolset{Known: ts.Known}
	for _, t := range ts.Enabled {
		if t.ID != id {
			out.Enabled = append(out.Enabled, t)
		}
	}
	return out
}

// PlannerMaxTokens is the output budget for the planner JSON.
const PlannerMaxTokens = 600

// renderTools returns the tool menu and the routing hint line for planner rule 2.
func renderTools(v Voice, ts Toolset) (string, string) {
	c := v.pack().Chat
	if len(ts.Enabled) == 0 {
		return c.ToolsNone, c.RoutingNone
	}
	lines := []string{c.ToolsHeader}
	hasMemory := false
	var routes []string
	for _, t := range ts.Enabled {
		lines = append(lines, "- "+t.ID+": "+t.Description)
		if t.ID == "memory.search" {
			hasMemory = true
		}
		if t.Route != "" {
			routes = append(routes, t.Route)
		}
	}
	if !hasMemory {
		lines = append(lines, c.MemoryOff)
	}
	routing := c.RoutingNone
	if len(routes) > 0 {
		routing = strings.Join(routes, c.RoutingJoin) + c.RoutingEnd
	}
	return strings.Join(lines, "\n"), routing
}

// Turn is one prior message of the same channel conversation.
type Turn struct {
	Role    string // user | assistant
	Speaker string
	Content string
	At      time.Time // when it was said; zero when unknown
}

var ctrlRE = regexp.MustCompile(`[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]`)

// sanitize drops control characters and special tokens (untrusted.go) and neutralizes the pack's block
// delimiters inside user text.
func sanitize(v Voice, s string) string {
	out := cleanUntrusted(s)
	for _, d := range v.pack().Chat.Delimiters.All() {
		out = strings.ReplaceAll(out, d, strings.ReplaceAll(strings.ReplaceAll(d, "[[", "［［"), "]]", "］］"))
	}
	return strings.TrimSpace(out)
}

func clip(s string, limit int, mark string) (string, bool) {
	if limit <= 0 || utf8.RuneCountInString(s) <= limit {
		return s, false
	}
	r := []rune(s)
	keep := max(1, limit-utf8.RuneCountInString(mark))
	return strings.TrimRight(string(r[:keep]), " \t\n") + mark, true
}

// PlannerBudget bounds the planner prompt.
type PlannerBudget struct {
	MaxMessageChars, MaxTurns, MaxTurnChars, MaxContextChars, MaxSummaryChars int
}

// HistoryShown is how many of a conversation's latest rows can reach a prompt: the answer's default (Request.MaxHistory),
// and the planner's and the tool loop's budget. A reply is recorded as having read those rows (the application's
// chatTurn), so no prompt may look further back than this.
const HistoryShown = 8

var defaultBudget = PlannerBudget{1500, HistoryShown, 400, 2600, 600}

func renderTurn(v Voice, t Turn, content string) string {
	c := v.pack().Chat
	label := lang.Fill(c.AssistantLabel, "bot", v.bot())
	if t.Role == "user" {
		label = c.UserLabel
		if sp := sanitize(v, t.Speaker); sp != "" {
			r := []rune(sp)
			label = lang.Fill(c.UserLabelNamed, "name", string(r[:min(40, len(r))]))
		}
	}
	lines := strings.Split(content, "\n")
	for i := 1; i < len(lines); i++ {
		lines[i] = "  " + lines[i]
	}
	return "- " + label + ": " + strings.Join(lines, "\n")
}

// BuildPlannerMessages composes the planner call (planner_context.build_planner_messages).
// timeRef is the rendered reference clock ("" = not provided).
func BuildPlannerMessages(v Voice, message string, history []Turn, summary, timeRef string, ts Toolset) []Msg {
	b := defaultBudget
	c := v.pack().Chat
	d := c.Delimiters
	menu, routing := renderTools(v, ts)
	header := lang.Fill(c.PlannerHeader, "bot", v.bot()) + "\n" + c.PlanOutputSchema + "\n\n" + c.PlanFieldRules
	system := header + "\n\n" + menu + "\n\n" + lang.Fill(c.PlanGeneralRules, "routing", routing) + "\n\n" + lang.Fill(c.ContextRules, "bot", v.bot())
	picked, used := pickTurns(v, history, b)
	sum := sanitize(v, summary)
	if sum != "" {
		limit := min(b.MaxSummaryChars, b.MaxContextChars-used)
		if limit >= 40 {
			sum, _ = clip(sum, limit, c.Truncated)
		} else {
			sum = ""
		}
	}
	shown, _ := clip(sanitize(v, message), b.MaxMessageChars, c.Truncated)
	var parts []string
	if timeRef == "" {
		parts = append(parts, c.TimeRefMissing)
	} else {
		parts = append(parts, lang.Fill(c.TimeRef, "time", timeRef))
	}
	parts = append(parts, d.MessageStart+"\n"+shown+"\n"+d.MessageEnd)
	if len(picked) > 0 {
		parts = append(parts, d.ConversationStart+" "+lang.Fill(c.ConversationHeader, "n", itoa(len(picked)))+"\n"+strings.Join(picked, "\n")+"\n"+d.ConversationEnd)
	} else {
		parts = append(parts, d.ConversationStart+" "+c.ConversationEmpty+"\n"+d.ConversationEnd)
	}
	if sum != "" {
		parts = append(parts, d.SummaryStart+" "+c.SummaryHeader+"\n"+sum+"\n"+d.SummaryEnd)
	}
	return []Msg{{Role: "system", Content: system}, {Role: "user", Content: strings.Join(parts, "\n\n")}}
}

// pickTurns renders those of the last MaxTurns rows that fit the budget, oldest first, and returns how many runes they
// take. The planner and the tool loop show the conversation the same way. A row left out (empty, or over the budget)
// lets no older one in: a reply is recorded as having read rows by their place (HistoryShown), and the words of a row
// further back would be in a prompt without the reply being known to have read them.
func pickTurns(v Voice, history []Turn, b PlannerBudget) ([]string, int) {
	c := v.pack().Chat
	var picked []string
	used := 0
	for i := len(history) - 1; i >= max(0, len(history)-b.MaxTurns); i-- {
		t := history[i]
		content := sanitize(v, t.Content)
		if (t.Role != "user" && t.Role != "assistant") || content == "" {
			continue
		}
		content, _ = clip(content, b.MaxTurnChars, c.Truncated)
		r := renderTurn(v, t, content)
		if len(picked) > 0 && used+utf8.RuneCountInString(r) > b.MaxContextChars {
			continue
		}
		picked = append(picked, r)
		used += utf8.RuneCountInString(r)
	}
	for i, j := 0, len(picked)-1; i < j; i, j = i+1, j-1 {
		picked[i], picked[j] = picked[j], picked[i]
	}
	return picked, used
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// Msg is one chat message.
type Msg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// Images are pictures sent with this message (only the answer's last user message carries them,
	// when the member called the bot with pictures and the answer model takes them). Never logged.
	Images []Image `json:"-"`
}

// Image is a prepared picture: media type and bytes.
type Image struct {
	MIME string
	Data []byte
}

// Step is one planned tool call.
type Step struct {
	Tool       string         `json:"tool_name"`
	Parameters map[string]any `json:"parameters"`
	State      string         `json:"state,omitempty"`
	Missing    []string       `json:"missing,omitempty"`
}

// Plan is the strictly parsed planner output (bot_plan.parse_planner_output).
type Plan struct {
	Parsed            bool             `json:"parsed"`
	ParseState        string           `json:"parse_state"`
	MemoryRequirement string           `json:"memory_requirement,omitempty"`
	TimeScope         string           `json:"time_scope,omitempty"`
	Time              *search.TimeSpec `json:"time,omitempty"`
	QuestionKind      string           `json:"question_kind,omitempty"`
	PersonTargets     []string         `json:"person_targets,omitempty"`
	Sensitive         bool             `json:"sensitive,omitempty"`
	SemanticQuery     string           `json:"semantic_query,omitempty"`
	KeyTerms          []string         `json:"key_terms,omitempty"`
	Unresolved        []string         `json:"unresolved,omitempty"`
	ToolPlan          []Step           `json:"tool_plan"`
	hasIntentFields   bool
}

var (
	memoryReqs    = map[string]bool{"none": true, "optional": true, "required": true}
	questionKinds = map[string]bool{"person_intro": true, "person_fact": true, "event": true, "casual": true, "other": true}
	timeKinds     = map[string]bool{"any": true, "recent": true, "today": true, "yesterday": true, "last_week": true, "this_month": true, "before": true, "after": true, "between": true}
)

func extractJSON(raw string) (map[string]any, string) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return nil, "empty"
	}
	if strings.Contains(text, "```") {
		parts := strings.Split(text, "```")
		if len(parts) > 1 {
			text = strings.TrimSpace(parts[1])
		} else {
			text = strings.TrimSpace(parts[0])
		}
		if strings.HasPrefix(strings.ToLower(text), "json") {
			text = strings.TrimSpace(text[4:])
		}
	}
	start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if start < 0 {
		return nil, "invalid"
	}
	if end < start {
		return nil, "truncated"
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(text[start:end+1]), &obj); err != nil {
		depth := 0
		for _, c := range text[start:] {
			if c == '{' {
				depth++
			} else if c == '}' {
				depth--
			}
		}
		if depth > 0 {
			return nil, "truncated"
		}
		return nil, "invalid"
	}
	if obj == nil {
		return nil, "invalid"
	}
	return obj, "ok"
}

func salvageRequirement(raw string) string {
	i := strings.Index(raw, `"memory_requirement"`)
	if i < 0 {
		return ""
	}
	tail := raw[i+len(`"memory_requirement"`):]
	if len(tail) > 40 {
		tail = tail[:40]
	}
	for _, v := range []string{"none", "optional", "required"} {
		if strings.Contains(tail, `"`+v+`"`) {
			return v
		}
	}
	return ""
}

func str(v any) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case nil:
		return ""
	case float64:
		b, _ := json.Marshal(x)
		return string(b)
	case bool:
		if x {
			return "true"
		}
		return "false"
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func strList(v any, maxItems, maxChars int) ([]string, bool) {
	arr, ok := v.([]any)
	if !ok {
		return nil, false
	}
	var out []string
	seen := map[string]bool{}
	for _, it := range arr {
		t := str(it)
		if n := utf8.RuneCountInString(t); n >= 1 && n <= maxChars && !seen[t] && len(out) < maxItems {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out, true
}

// validPlanSchema checks the fields that define a plan before any optional field is salvaged. The
// prompt permits optional intent details, but a memory decision and a tool list are always required.
func validPlanSchema(obj map[string]any) bool {
	req, ok := obj["memory_requirement"].(string)
	if !ok || !memoryReqs[strings.ToLower(strings.TrimSpace(req))] {
		return false
	}
	tools, ok := obj["tool_plan"].([]any)
	if !ok {
		return false
	}
	for _, raw := range tools {
		step, ok := raw.(map[string]any)
		if !ok {
			return false
		}
		name, ok := step["tool_name"].(string)
		if !ok {
			name, ok = step["name"].(string) // accepted legacy spelling
		}
		if !ok || strings.TrimSpace(name) == "" {
			return false
		}
		if params, exists := step["parameters"]; exists {
			if params == nil {
				continue
			}
			if _, ok := params.(map[string]any); !ok {
				return false
			}
		}
	}
	if v, exists := obj["time_scope"]; exists && v != nil {
		scope, ok := v.(string)
		if !ok || (strings.ToLower(strings.TrimSpace(scope)) != "recent" && strings.ToLower(strings.TrimSpace(scope)) != "any") {
			return false
		}
	}
	if v, exists := obj["question_kind"]; exists && v != nil {
		kind, ok := v.(string)
		if !ok || !questionKinds[strings.ToLower(strings.TrimSpace(kind))] {
			return false
		}
	}
	if v, exists := obj["sensitive"]; exists && v != nil {
		if _, ok := v.(bool); !ok {
			return false
		}
	}
	for _, key := range []string{"semantic_query"} {
		if v, exists := obj[key]; exists && v != nil {
			if _, ok := v.(string); !ok {
				return false
			}
		}
	}
	for _, key := range []string{"person_targets", "key_terms", "unresolved"} {
		v, exists := obj[key]
		if !exists || v == nil {
			continue
		}
		items, ok := v.([]any)
		if !ok {
			return false
		}
		for _, item := range items {
			if _, ok := item.(string); !ok {
				return false
			}
		}
	}
	if v, exists := obj["time"]; exists && v != nil {
		tm, ok := v.(map[string]any)
		if !ok {
			return false
		}
		if k, exists := tm["kind"]; exists && k != nil {
			kind, ok := k.(string)
			if !ok || !timeKinds[strings.ToLower(strings.TrimSpace(kind))] {
				return false
			}
		}
		for _, key := range []string{"from", "to"} {
			if x, exists := tm[key]; exists && x != nil {
				if _, ok := x.(string); !ok {
					return false
				}
			}
		}
	}
	return true
}

// ParsePlan is the strict parse: a damaged response never becomes an implicit memory search unless it
// literally said memory_requirement=required before it was cut.
func ParsePlan(raw string) Plan {
	obj, state := extractJSON(raw)
	if obj == nil {
		p := Plan{Parsed: false, ParseState: state}
		if state == "truncated" {
			p.MemoryRequirement = salvageRequirement(raw)
		}
		if p.MemoryRequirement == "required" {
			p.ToolPlan = []Step{{Tool: "memory.search", Parameters: map[string]any{}}}
		}
		return p
	}
	if !validPlanSchema(obj) {
		return Plan{Parsed: false, ParseState: "schema_invalid"}
	}
	p := Plan{Parsed: true, ParseState: "ok"}
	if r := strings.ToLower(str(obj["memory_requirement"])); memoryReqs[r] {
		p.MemoryRequirement = r
	}
	scope := strings.ToLower(str(obj["time_scope"]))
	if scope != "recent" && scope != "any" {
		scope = ""
	}
	if k := strings.ToLower(str(obj["question_kind"])); questionKinds[k] {
		p.QuestionKind = k
	}
	if t, ok := strList(obj["person_targets"], 64, 40); ok {
		p.PersonTargets = t
		p.hasIntentFields = true
	}
	if arr, ok := obj["tool_plan"].([]any); ok {
		for _, it := range arr {
			m, ok := it.(map[string]any)
			if !ok {
				continue
			}
			name := str(m["tool_name"])
			if name == "" {
				name = str(m["name"])
			}
			if name == "" {
				continue
			}
			params, _ := m["parameters"].(map[string]any)
			if params == nil {
				params = map[string]any{}
			}
			p.ToolPlan = append(p.ToolPlan, Step{Tool: canonicalTool(name), Parameters: params})
		}
	}
	if p.MemoryRequirement == "required" && !p.hasTool("memory.search") {
		p.ToolPlan = append(p.ToolPlan, Step{Tool: "memory.search", Parameters: map[string]any{}})
	}
	// time: v2 object, or derived from time_scope
	var tspec *search.TimeSpec
	if tm, ok := obj["time"].(map[string]any); ok {
		kind := strings.ToLower(str(tm["kind"]))
		if !timeKinds[kind] {
			kind = ""
		}
		frm, to := str(tm["from"]), str(tm["to"])
		if n := utf8.RuneCountInString(frm); n < 4 || n > 40 {
			frm = ""
		}
		if n := utf8.RuneCountInString(to); n < 4 || n > 40 {
			to = ""
		}
		if kind == "" {
			kind = "any"
			if scope == "recent" {
				kind = "recent"
			}
		}
		if kind != "before" && kind != "after" && kind != "between" {
			frm, to = "", ""
		}
		tspec = &search.TimeSpec{Kind: kind, From: frm, To: to}
	} else if scope != "" {
		kind := "any"
		if scope == "recent" {
			kind = "recent"
		}
		tspec = &search.TimeSpec{Kind: kind}
	}
	if tspec != nil && tspec.Kind == "recent" {
		scope = "recent"
	}
	p.TimeScope, p.Time = scope, tspec
	if s, ok := obj["semantic_query"].(string); ok {
		r := []rune(strings.TrimSpace(s))
		p.SemanticQuery = string(r[:min(300, len(r))])
		p.hasIntentFields = true
	}
	if t, ok := strList(obj["key_terms"], 12, 40); ok {
		p.KeyTerms = t
		p.hasIntentFields = true
	}
	if u, ok := strList(obj["unresolved"], 6, 80); ok {
		p.Unresolved = u
		p.hasIntentFields = true
	}
	if b, ok := obj["sensitive"].(bool); ok {
		p.Sensitive = b
	}
	if scope != "" || p.QuestionKind != "" || tspec != nil {
		p.hasIntentFields = true
	}
	return p
}

func (p Plan) hasTool(id string) bool {
	for _, s := range p.ToolPlan {
		if s.Tool == id {
			return true
		}
	}
	return false
}

// legacyToolAliases maps old tool names to Action IDs (contracts/sandbox/tool-parity.json).
var legacyToolAliases = map[string]string{
	"local_rag": "memory.search", "rag": "memory.search", "memory_search": "memory.search",
	"weather": "weather.query", "get_weather": "weather.query", "web_search": "search.web", "search": "search.web",
	"stock": "finance.quote", "exchange_rate": "finance.fx", "poll": "polls.create", "help": "help.version",
	"read_url": "web.read", "fetch_url": "web.read", "browse": "web.read",
	"search_for_place": "places.search", "places": "places.search", "place_search": "places.search",
}

func canonicalTool(name string) string {
	n := strings.TrimSpace(name)
	if a, ok := legacyToolAliases[strings.ToLower(n)]; ok {
		return a
	}
	return n
}

// memoryQuery is the planner's refined memory.search query ("" when absent).
func (p Plan) memoryQuery() string {
	for _, s := range p.ToolPlan {
		if s.Tool != "memory.search" {
			continue
		}
		q := str(s.Parameters["query"])
		if q == "" {
			q = str(s.Parameters["q"])
		}
		if n := utf8.RuneCountInString(q); n >= 2 && n <= 200 {
			return q
		}
	}
	return ""
}

// Intent converts the plan into the retrieval intent (bot_plan.intent_from_plan). ok=false when the
// planner emitted no intent fields (the retriever then uses its neutral intent).
func (p Plan) Intent() (search.Intent, bool) {
	if !p.Parsed || !p.hasIntentFields {
		return search.Intent{Kind: "other", Source: "none"}, false
	}
	t := p.Time
	if t == nil {
		k := "any"
		if p.TimeScope == "recent" {
			k = "recent"
		}
		t = &search.TimeSpec{Kind: k}
	}
	kind := p.QuestionKind
	if kind == "" {
		kind = "other"
	}
	return search.Intent{
		Recent: p.TimeScope == "recent" || t.Kind == "recent", Intro: p.QuestionKind == "person_intro", Sensitive: p.Sensitive,
		Kind: kind, Targets: append([]string{}, p.PersonTargets...), Time: t, SemanticQuery: p.SemanticQuery,
		KeyTerms: p.KeyTerms, MemorySearchQuery: p.memoryQuery(), Unresolved: p.Unresolved, Source: "planner",
	}, true
}

var forbiddenParams = map[string]bool{
	"channel_id": true, "channel": true, "room": true, "room_id": true, "source_room_id": true, "guild_id": true,
	"block_ids": true, "allowed_rooms": true, "allowed_ids": true, "scope": true, "timezone": true, "tz": true,
	"now": true, "reference_time": true, "user_id": true, "speaker_id": true, "person_id": true, "account_id": true,
}

var (
	placeholderMu sync.Mutex
	placeholderRE = map[string]*regexp.Regexp{}
)

// placeholder matches values the planner invented instead of leaving a parameter out ("user's city",
// "{symbol}", "unknown"). The words that start such phrases come from the language pack.
func (v Voice) placeholder() *regexp.Regexp {
	p := v.pack()
	placeholderMu.Lock()
	defer placeholderMu.Unlock()
	if re, ok := placeholderRE[p.Tag]; ok {
		return re
	}
	var words []string
	for _, w := range p.Chat.PlaceholderWords {
		words = append(words, regexp.QuoteMeta(w))
	}
	pat := `\bunknown\b|\bnull\b|\?\?\?|\{|\}|<[^>]*>|^\s*$`
	if len(words) > 0 {
		pat = `(` + strings.Join(words, "|") + `)\s*\S+|` + pat
	}
	re := regexp.MustCompile(`(?i)` + pat)
	placeholderRE[p.Tag] = re
	return re
}

func isPlaceholder(re *regexp.Regexp, v any) bool {
	switch v.(type) {
	case nil:
		return true
	case float64, int:
		return false
	}
	t := str(v)
	return t == "" || re.MatchString(t)
}

// Assessment is what the caller may do with a plan (planner_context.assess_plan).
type Assessment struct {
	MemoryState string           `json:"memory_state"` // search | skip | optional_unplanned | blocked_disabled | unparsed
	Steps       []Step           `json:"steps"`
	Time        *search.TimeSpec `json:"time,omitempty"`
	Stripped    []string         `json:"stripped_params,omitempty"`
}

// Assess maps a plan onto runnable states without executing anything. ts is the turn's tool set;
// memory.search is governed by memoryOn, not by ts.
func Assess(v Voice, p Plan, ts Toolset, memoryOn, hasTimeRef bool) Assessment {
	ph := v.placeholder()
	planned := p.hasTool("memory.search")
	a := Assessment{}
	switch {
	case !p.Parsed && p.MemoryRequirement == "":
		a.MemoryState = "unparsed"
	case p.MemoryRequirement == "none":
		a.MemoryState = "skip"
	case p.MemoryRequirement == "required" || (p.MemoryRequirement == "optional" && planned):
		a.MemoryState = "search"
	case p.MemoryRequirement == "optional":
		a.MemoryState = "optional_unplanned"
	case planned:
		a.MemoryState = "search"
	default:
		a.MemoryState = "skip"
	}
	if a.MemoryState == "search" && !memoryOn {
		a.MemoryState = "blocked_disabled"
	}
	for _, s := range p.ToolPlan {
		if s.Tool == "" || s.Tool == "memory.search" {
			continue
		}
		params := map[string]any{}
		for k, v := range s.Parameters {
			if forbiddenParams[strings.ToLower(k)] {
				a.Stripped = append(a.Stripped, s.Tool+"."+k)
				continue
			}
			params[k] = v
		}
		st := Step{Tool: s.Tool, Parameters: params}
		info, enabled := ts.find(s.Tool)
		switch {
		case !enabled && !ts.Known[s.Tool]:
			st.State = "unknown_tool"
		case !enabled:
			st.State = "disabled"
		default:
			for _, k := range info.Required {
				if isPlaceholder(ph, params[k]) {
					st.Missing = append(st.Missing, k)
					delete(params, k)
				}
			}
			st.State = "runnable"
			if len(st.Missing) > 0 {
				st.State = "needs_input"
			}
		}
		a.Steps = append(a.Steps, st)
	}
	if p.Time != nil {
		t := *p.Time
		if !hasTimeRef && (t.From != "" || t.To != "" || t.Kind == "today" || t.Kind == "yesterday" || t.Kind == "last_week" || t.Kind == "this_month") {
			t.From, t.To = "", ""
		}
		a.Time = &t
	}
	return a
}
