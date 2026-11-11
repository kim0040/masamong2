package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	"masamong/internal/errclass"
	"masamong/internal/lang"
)

// The native tool loop (docs/plan-memory-conversation.md §5.5, option A). A small model sees the
// enabled tools as function schemas and calls them with native tool calling, looks at the results and
// may call again, at most MaxLoopRounds times. It only chooses and runs tools: its own text is an
// internal note that is never the reply and never stored. The memory planner keeps deciding the
// memory search (its prompt stays the evaluated one) and the answer model writes the reply from the
// same tool results the planner path produces.
//
// Every call goes through the turn's ToolRunner (Service.Execute: switches, limits, confirmations),
// and the loop itself fails closed: unknown tools are refused, scope keys never reach a tool, a call
// that repeats is not run again, a tool that failed twice leaves the menu, and once text from the open
// web has been read, tools that create things (polls, reminders, pictures) leave the menu too, so a
// planted instruction in a page cannot make one.

// Tool modes (config chat.tool_mode).
const (
	ToolModePlanner = "planner"
	ToolModeShadow  = "shadow"
	ToolModeLoop    = "loop"
)

const (
	// MaxLoopRounds is how many times the loop model is asked (each answer may call several tools).
	MaxLoopRounds = 3
	// loopMaxCalls bounds the calls one round may make; the rest are refused.
	loopMaxCalls = 5
	// loopResultChars is how much of one tool result the loop model reads back.
	loopResultChars = 1500
	// LoopMaxTokens is the output budget of one loop call (tool calls are short).
	LoopMaxTokens = 600
)

// When the loop stops without asking the model once more (config chat.loop_final).
const (
	LoopFinalTools  = "tools"  // the default ("" too): after a round of tools whose one result is the answer
	LoopFinalSearch = "search" // and after a web search that found enough (searchSettles)
	LoopFinalOff    = "off"    // never: it always asks
)

// ToolParam is one parameter of a tool as the loop's function schema shows it.
type ToolParam struct {
	Name     string
	Type     string   // string | integer | number | boolean | array (of strings) ("" = string)
	Enum     []string // closed values (strings)
	Min, Max *int     // bounds of an integer; Max is the most items of an array
	Required bool
	Hint     string // how to fill it (language pack)
}

// ToolSpec is one function offered to the loop model.
type ToolSpec struct {
	ID          string          // product tool ID
	Name        string          // function name: tool IDs have dots, which function names may not
	Description string          // purpose / when / when not
	Parameters  json.RawMessage // JSON Schema object
}

// FunctionName is the function name of a tool ID (weather.query → weather__query).
func FunctionName(id string) string { return strings.ReplaceAll(id, ".", "__") }

type schemaProp struct {
	Type        string      `json:"type"`
	Description string      `json:"description,omitempty"`
	Enum        []string    `json:"enum,omitempty"`
	Minimum     *int        `json:"minimum,omitempty"`
	Maximum     *int        `json:"maximum,omitempty"`
	Items       *schemaProp `json:"items,omitempty"`
	MaxItems    *int        `json:"maxItems,omitempty"`
}

type schemaObject struct {
	Type                 string                `json:"type"`
	Properties           map[string]schemaProp `json:"properties"`
	Required             []string              `json:"required,omitempty"`
	AdditionalProperties bool                  `json:"additionalProperties"`
}

// allowedParams are the parameters the schema offers: scope, ID, channel, user and clock keys never
// appear (the service injects scope itself; the model cannot widen it).
func allowedParams(t ToolInfo) []ToolParam {
	var out []ToolParam
	seen := map[string]bool{}
	for _, p := range t.Params {
		name := strings.TrimSpace(p.Name)
		if name == "" || seen[name] || forbiddenParams[strings.ToLower(name)] {
			continue
		}
		seen[name] = true
		out = append(out, p)
	}
	return out
}

func requiredOf(t ToolInfo) map[string]bool {
	req := map[string]bool{}
	for _, k := range t.Required {
		req[k] = true
	}
	for _, p := range t.Params {
		if p.Required {
			req[p.Name] = true
		}
	}
	return req
}

func toolDescription(t ToolInfo) string {
	purpose := strings.TrimSpace(t.Purpose)
	if purpose == "" {
		purpose = strings.TrimSpace(t.Description)
	}
	var parts []string
	for _, s := range []string{purpose, strings.TrimSpace(t.When), strings.TrimSpace(t.Skip)} {
		if s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, " ")
}

// ToolSchemas is the loop's menu: one function per enabled tool except memory.search (the planner
// owns memory), sorted by tool ID so the request prefix stays the same from turn to turn (prompt
// caching). Unknown parameters are refused by the schema (additionalProperties: false).
func ToolSchemas(ts Toolset) []ToolSpec {
	tools := append([]ToolInfo(nil), ts.Enabled...)
	sort.Slice(tools, func(i, j int) bool { return tools[i].ID < tools[j].ID })
	var out []ToolSpec
	for _, t := range tools {
		if t.ID == "" || t.ID == "memory.search" {
			continue
		}
		obj := schemaObject{Type: "object", Properties: map[string]schemaProp{}}
		req := requiredOf(t)
		for _, p := range allowedParams(t) {
			typ := p.Type
			if typ == "" {
				typ = "string"
			}
			prop := schemaProp{Type: typ, Description: functionNames(strings.TrimSpace(p.Hint), ts)}
			if typ == "string" && len(p.Enum) > 0 {
				prop.Enum = append([]string(nil), p.Enum...)
			}
			if typ == "integer" || typ == "number" {
				prop.Minimum, prop.Maximum = p.Min, p.Max
			}
			if typ == "array" {
				prop.Items, prop.MaxItems = &schemaProp{Type: "string"}, p.Max
			}
			obj.Properties[p.Name] = prop
			if req[p.Name] {
				obj.Required = append(obj.Required, p.Name)
			}
		}
		raw, _ := json.Marshal(obj)
		out = append(out, ToolSpec{ID: t.ID, Name: FunctionName(t.ID), Description: functionNames(toolDescription(t), ts), Parameters: raw})
	}
	return out
}

// functionNames writes the tool IDs a description names ("use search.web instead") as the function
// names the model sees.
func functionNames(s string, ts Toolset) string {
	ids := make([]string, 0, len(ts.Known)+len(ts.Enabled))
	for id := range ts.Known {
		ids = append(ids, id)
	}
	for _, t := range ts.Enabled {
		ids = append(ids, t.ID)
	}
	sort.Slice(ids, func(i, j int) bool { return len(ids[i]) > len(ids[j]) || len(ids[i]) == len(ids[j]) && ids[i] < ids[j] })
	for _, id := range ids {
		if strings.Contains(id, ".") {
			s = strings.ReplaceAll(s, id, FunctionName(id))
		}
	}
	return s
}

// LoopCall is one function call the loop model asked for. Arguments is the JSON text as sent.
type LoopCall struct {
	ID        string
	Name      string
	Arguments string
}

// LoopMsg is one message of the loop conversation. Raw, when set, is an assistant message exactly as
// the provider returned it and is sent back unchanged (some providers need their own fields back).
type LoopMsg struct {
	Role    string
	Content string
	Calls   []LoopCall
	CallID  string // a "tool" message answers this call
	Raw     json.RawMessage
}

// LoopReply is one answer of the loop model.
type LoopReply struct {
	Text    string
	Calls   []LoopCall
	Raw     json.RawMessage
	CostUSD float64
	// BindsTools marks an answer whose reasoning, sent back, is valid only with the tool list it was
	// offered (an Anthropic answer's signed thinking): see StableToolsLLM
	BindsTools bool
}

// LoopLLM is the native tool calling port. The application adapts its OpenAI-compatible models.
type LoopLLM interface {
	// ToolsReady is false for a role whose model is known to refuse tools.
	ToolsReady(role string) bool
	ChatTools(ctx context.Context, role string, msgs []LoopMsg, tools []ToolSpec, maxTokens int) (LoopReply, error)
}

// StableToolsLLM is a LoopLLM that may report a role whose model binds the reasoning it sends back to the
// exact tool list it was offered (Anthropic's preserved thinking: a signed thinking block is valid only
// while the system prompt, the tools and the earlier messages are unchanged, and a changed list fails
// the next round with a 400). For such a role, once an answer of that kind is in the loop
// (LoopReply.BindsTools: a fallback of another format may have answered so far), the loop offers the
// list that answer was given on every later round and answers a call to a tool that is not available any
// more with a tool result saying so (prepare), instead of taking it off the menu.
type StableToolsLLM interface {
	StableTools(role string) bool
}

// ErrToolsUnsupported means the model or its route takes no tools; the turn falls back to the planner.
var ErrToolsUnsupported = errors.New("chat: the model takes no tools")

// LoopStep is one call the loop model made, and what became of it.
type LoopStep struct {
	Round  int            `json:"round"`
	Tool   string         `json:"tool"`
	Params map[string]any `json:"params,omitempty"` // the cleaned arguments (turn record only, never the log)
	State  string         `json:"state"`            // ran | failed | not_run | refused
	// Reason says why a call was not run or refused: unknown_tool, bad_arguments, duplicate, removed,
	// writes_after_web, needs_input, limit, too_many, shadow.
	Reason string `json:"reason,omitempty"`
	// Repaired is true when the arguments were not valid JSON as sent and a repair made them so
	// (repairArgs: a code fence, a trailing comma, missing closers, a raw control character).
	Repaired bool `json:"repaired,omitempty"`
}

// LoopTrace is what the loop did in one turn, without message text.
type LoopTrace struct {
	Mode    string     `json:"mode"` // loop | shadow
	Rounds  int        `json:"rounds"`
	Steps   []LoopStep `json:"calls,omitempty"`
	Refused int        `json:"refused,omitempty"`
	Removed []string   `json:"removed,omitempty"` // tools taken off the menu during the turn
	// Stop is why the loop ended: answered (the model called nothing more), final (a round settled the
	// turn: ToolLoop.Final), max_rounds, error (a later round failed; earlier results are kept), no_tools
	// (nothing to offer), prefilter (a message with no letters or digits), fallback (the planner chose the
	// tools instead, see Fallback).
	Stop     string  `json:"stop"`
	Fallback string  `json:"fallback,omitempty"` // unsupported | no_tool_calling | error:<class>
	CostUSD  float64 `json:"cost_usd"`
	Ms       int     `json:"ms"`
}

// Brief is the trace without arguments, for logs.
func (t *LoopTrace) Brief() *LoopTrace {
	if t == nil {
		return nil
	}
	c := *t
	c.Steps = make([]LoopStep, len(t.Steps))
	for i, s := range t.Steps {
		s.Params = nil
		c.Steps[i] = s
	}
	return &c
}

// hasContent is the loop's prefilter: a message with no letter or digit in any script (emoji, a
// sticker, punctuation) needs no tool. It knows no words of any language.
func hasContent(msg string) bool {
	for _, r := range msg {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// ToolLoop runs the loop for one turn.
type ToolLoop struct {
	LLM   LoopLLM
	Run   ToolRunner // nil, or Record, runs nothing
	Role  string     // model role (the planner's small fast model)
	Voice Voice
	Guard *Guard
	// Record only notes what the model would have called (shadow mode): nothing is executed.
	Record bool
	// Final is config chat.loop_final (LoopFinalTools when empty). A round whose calls all settle the
	// turn ends the loop: asking "anything more?" took 4 of the 7.3 seconds of a two-round loop. The model
	// asks for independent things in one round, so those are all there. What it would have called in a next
	// round because of a result (the weather first, then a place for that weather) is not asked for.
	Final string
	// OnProgress hears a round that starts web searches (Request.OnProgress), each stage once; nil, and
	// never called when Record, for nobody.
	OnProgress func(Progress)
}

// finalRound reports whether the round's calls settle the turn: each ran and found what it looked for,
// and each is of a tool whose one result is the answer. A call refused, held back or left without a value
// means there is more to do, and so does a tool that came back empty (the model tries another source).
func (l ToolLoop) finalRound(calls []*loopCall, infos map[string]ToolInfo, message string) bool {
	if l.Final == LoopFinalOff || len(calls) == 0 || !shortAsk(message) {
		return false
	}
	for _, lc := range calls {
		if !lc.run || !foundIt(lc.result) {
			return false
		}
		if !infos[lc.id].Final && !(l.Final == LoopFinalSearch && lc.id == "search.web" && searchSettles(lc, message)) {
			return false
		}
	}
	return true
}

// shortAskWords is the longest message that can settle a turn in one round. A step that depends on a
// result ("check the weather, then search for a place that suits it") takes more words than a single
// question, and the live comparison (scenarios_multi_ko.json) lost exactly those steps: a model that plans
// in steps calls only the first in round one. Longer messages keep the second round: that costs seconds,
// a dropped step costs the answer.
const shortAskWords = 6

// shortAsk reports whether the message is short enough to be one question. It reads no words of any
// language: it counts what spaces separate, and every two han or kana letters as one word, since those
// scripts write without spaces.
func shortAsk(message string) bool {
	words := 0
	for _, field := range strings.Fields(message) {
		cjk := 0
		for _, r := range field {
			if unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana) {
				cjk++
			}
		}
		words += max(1, cjk/2)
	}
	return words <= shortAskWords
}

// foundIt is a successful result with what was asked: no status but ok (a place not found, a partial quote,
// a stale reading), and from the service itself. A quote answered by a web search (live false) is a
// search's result, which a later round may well improve on.
func foundIt(r *ToolResult) bool {
	if r == nil || !r.OK || (r.Status != "" && r.Status != "ok") {
		return false
	}
	if st, ok := r.Data["status"].(string); ok && st != "ok" {
		return false
	}
	live, says := r.Data["live"].(bool)
	return !says || live
}

// searchEnough is how many results with text a web search needs to settle a turn.
const searchEnough = 3

var linkRE = regexp.MustCompile(`(?i)https?://|\bwww\.`)

// searchSettles is the web search's own test: enough results with text, not a search the model itself
// asked to look wide (depth deep: a digest, a comparison), and not a message with a link in it, which asks
// for that page to be read. Nothing here knows the words of a language, so a question that asks in words
// to read on or search again is not seen, which is why this is off by default (LoopFinalSearch).
func searchSettles(lc *loopCall, message string) bool {
	if depth, _ := lc.params["depth"].(string); strings.EqualFold(strings.TrimSpace(depth), "deep") || linkRE.MatchString(message) {
		return false
	}
	items, _ := lc.result.Data["results"].([]any)
	found := 0
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			if text, _ := m["snippet"].(string); strings.TrimSpace(text) != "" {
				found++
			}
		}
	}
	return found >= searchEnough
}

func (l ToolLoop) messages(history []Turn, message, timeRef string) []LoopMsg {
	v := l.Voice
	c := v.pack().Chat
	d := c.Delimiters
	picked, _ := pickTurns(v, history, defaultBudget)
	shown, _ := clip(sanitize(v, message), defaultBudget.MaxMessageChars, c.Truncated)
	var parts []string
	if timeRef == "" {
		parts = append(parts, c.TimeRefMissing)
	} else {
		parts = append(parts, lang.Fill(c.TimeRef, "time", timeRef))
	}
	if len(picked) > 0 {
		parts = append(parts, d.ConversationStart+" "+lang.Fill(c.ConversationHeader, "n", itoa(len(picked)))+"\n"+strings.Join(picked, "\n")+"\n"+d.ConversationEnd)
	}
	parts = append(parts, d.MessageStart+"\n"+shown+"\n"+d.MessageEnd)
	return []LoopMsg{{Role: "system", Content: lang.Fill(c.ToolPolicy, "bot", v.bot())}, {Role: "user", Content: strings.Join(parts, "\n\n")}}
}

// loopCall is one call being handled in a round.
type loopCall struct {
	call   LoopCall
	id     string
	params map[string]any
	reply  string // the tool message for the model, when the call is answered without running
	step   LoopStep
	result *ToolResult
	run    bool
}

// Do asks the loop model and runs what it calls. It returns the executed results (in the shape the
// planner path produces), the trace, and an error only when the very first call failed: the caller
// then lets the planner choose the tools for this turn.
func (l ToolLoop) Do(ctx context.Context, menu Toolset, history []Turn, message, timeRef string) ([]ToolResult, *LoopTrace, error) {
	started := time.Now()
	mode := ToolModeLoop
	if l.Record {
		mode = ToolModeShadow
	}
	tr := &LoopTrace{Mode: mode}
	defer func() { tr.Ms = ms(started) }()
	specs := ToolSchemas(menu)
	if len(specs) == 0 {
		tr.Stop = "no_tools"
		return nil, tr, nil
	}
	infos := map[string]ToolInfo{}
	byName := map[string]string{}
	for _, t := range menu.Enabled {
		infos[t.ID] = t
	}
	for _, s := range specs {
		byName[s.Name], byName[s.ID] = s.ID, s.ID
	}
	ph := l.Voice.placeholder()
	nonce := newFenceNonce() // the turn's tool result fences (untrusted.go)
	msgs := l.messages(history, message, timeRef)
	removed := map[string]bool{}
	done := map[string]bool{} // Once tools that succeeded in an earlier round
	failed, badArgs := map[string]int{}, map[string]int{}
	seen := map[string]bool{}
	webSeen := false
	var results, needInput []ToolResult
	remove := func(id string) {
		if !removed[id] {
			removed[id] = true
			tr.Removed = append(tr.Removed, id)
		}
	}
	stable := false
	if s, ok := l.LLM.(StableToolsLLM); ok {
		stable = s.StableTools(l.Role)
	}
	progress := progressOnce(l.OnProgress)
	if l.Record {
		progress = nil // a shadow loop runs nothing
	}
	var frozen []ToolSpec // the list a binding answer was given (stable roles)
	for round := 1; round <= MaxLoopRounds; round++ {
		var offer []ToolSpec
		for _, s := range specs {
			if removed[s.ID] || done[s.ID] || (webSeen && infos[s.ID].Writes) {
				continue
			}
			offer = append(offer, s)
		}
		if len(offer) == 0 {
			tr.Stop = "no_tools"
			break
		}
		if frozen != nil {
			// the wire list stays the one the binding answer saw; prepare refuses what is closed now
			offer = frozen
		}
		reply, err := l.LLM.ChatTools(ctx, l.Role, msgs, offer, LoopMaxTokens)
		tr.CostUSD += reply.CostUSD
		tr.Rounds = round
		if err != nil {
			if round == 1 {
				tr.Stop, tr.Fallback = "fallback", fallbackReason(err)
				return nil, tr, err
			}
			tr.Stop = "error"
			break
		}
		if len(reply.Calls) == 0 {
			tr.Stop = "answered"
			break
		}
		if stable && reply.BindsTools && frozen == nil {
			frozen = offer
		}
		assistant := LoopMsg{Role: "assistant", Content: reply.Text, Calls: reply.Calls, Raw: reply.Raw}
		calls := make([]*loopCall, len(reply.Calls))
		for i, c := range reply.Calls {
			if strings.TrimSpace(c.ID) == "" {
				// a host that sends no call IDs gets them made up, and the assistant message is rebuilt
				// with them instead of sent back as it came
				c.ID = fmt.Sprintf("call_%d_%d", round, i+1)
				assistant.Calls[i].ID, assistant.Raw = c.ID, nil
			}
			calls[i] = l.prepare(c, round, i, byName, infos, removed, done, webSeen, seen, badArgs, ph, remove)
		}
		msgs = append(msgs, assistant)
		// the round's calls run at once (runSteps): each is checked against the turn's limits (Guard) in
		// the model's order, and runs with its own child guard, so the call ledger gives
		// each tool what it spent; the results stay in the model's order
		var steps []step
		var searches []map[string]any
		told := &roundProgress{report: progress, round: round} // the stage is told as searches are let in
		for _, lc := range calls {
			if !lc.run {
				continue
			}
			if lc.id == webSearchTool {
				searches = append(searches, lc.params)
			}
			info := infos[lc.id]
			cancel := func() {
				lc.run = false
				lc.step.State, lc.step.Reason = "not_run", "cancelled"
				lc.result = &ToolResult{Tool: lc.id, Status: "cancelled"}
			}
			hold := &paidHold{}
			steps = append(steps, step{key: stepKey(info, lc.id), reserved: true, cancel: cancel, pays: info.Pays, maxFee: feeBound(info, lc.params), hold: hold, decide: func() bool {
				if ctx.Err() != nil {
					cancel()
					return false
				}
				allowed, endPaid := admitTool(ctx, l.Guard, lc.id, info, lc.params)
				hold.end = endPaid
				if !allowed {
					if ctx.Err() != nil {
						cancel()
						return false
					}
					// the turn spent its share, the calls before this one included: the answer says it could
					// not check, as for a tool that is down
					lc.run = false
					lc.step.State, lc.step.Reason = "not_run", "limit"
					lc.result = &ToolResult{Tool: lc.id, OK: false, Status: "unavailable"}
					return false
				}
				l.Guard.Note(0) // what the tool pays for is added by the tool (Guard.Spend)
				if lc.id == webSearchTool {
					told.admit(lc.params)
				}
				return true
			}, run: func() {
				r := runTool(ctx, l.Guard, l.Run, lc.id, lc.params)
				if r.Tool == "" {
					r.Tool = lc.id
				}
				lc.result = &r
			}})
		}
		told.asked = len(searches)
		runSteps(ctx, l.Guard, steps)
		for _, lc := range calls {
			switch {
			case lc.run:
				r := *lc.result
				results = append(results, r)
				lc.step.State = "ran"
				if !r.OK {
					lc.step.State = "failed"
					if failed[lc.id]++; failed[lc.id] >= 2 {
						remove(lc.id)
					}
				}
				if infos[lc.id].Web {
					webSeen = true
				}
				if r.OK && infos[lc.id].Once {
					done[lc.id] = true // closed from the next round on; this round's calls have all run
				}
				lc.reply = loopResultMessage(r, nonce)
			case lc.result != nil: // answered without running (limit, needs_input)
				if lc.result.Status == "needs_input" {
					needInput = append(needInput, *lc.result)
				} else {
					results = append(results, *lc.result)
				}
				lc.reply = loopResultMessage(*lc.result, nonce)
			}
			if lc.step.State == "refused" {
				tr.Refused++
			}
			tr.Steps = append(tr.Steps, lc.step)
			msgs = append(msgs, LoopMsg{Role: "tool", CallID: lc.call.ID, Content: lc.reply})
		}
		if l.finalRound(calls, infos, message) {
			tr.Stop = "final"
			break
		}
		if round == MaxLoopRounds {
			tr.Stop = "max_rounds"
		}
	}
	// a value the loop could not fill reaches the answer (so it asks), unless the tool ran anyway later
	for _, ni := range needInput {
		ran := false
		for _, r := range results {
			ran = ran || r.Tool == ni.Tool
		}
		if !ran {
			results = append(results, ni)
		}
	}
	if l.Record {
		return nil, tr, nil
	}
	return results, tr, nil
}

// prepare checks one call before anything runs: a known tool still on the menu, valid JSON arguments
// limited to the schema's parameters, real values for the required ones, not a repeat, and the turn's
// limits. Only then is it marked to run.
func (l ToolLoop) prepare(c LoopCall, round, idx int, byName map[string]string, infos map[string]ToolInfo, removed, done map[string]bool,
	webSeen bool, seen map[string]bool, badArgs map[string]int, ph *regexp.Regexp, remove func(string)) *loopCall {
	lc := &loopCall{call: c}
	id, known := byName[strings.TrimSpace(c.Name)]
	lc.id = id
	lc.step = LoopStep{Round: round, Tool: id, State: "refused"}
	refuse := func(reason, note string) *loopCall {
		lc.step.Reason = reason
		raw, _ := json.Marshal(map[string]any{"ok": false, "status": "refused", "error": note})
		lc.reply = string(raw)
		return lc
	}
	if !known {
		lc.step.Tool = clipName(c.Name)
		return refuse("unknown_tool", "no such tool; use only the tools offered")
	}
	if idx >= loopMaxCalls {
		return refuse("too_many", "too many calls at once")
	}
	if removed[id] {
		return refuse("removed", "this tool is no longer available in this turn")
	}
	if done[id] {
		return refuse("done", "this tool already did its work in this turn; its result above stands")
	}
	info := infos[id]
	if webSeen && info.Writes {
		return refuse("writes_after_web", "this tool is not available after reading web content")
	}
	args, repaired, ok := parseArgs(c.Arguments)
	lc.step.Repaired = repaired
	if !ok {
		if badArgs[id]++; badArgs[id] >= 2 {
			remove(id)
			return refuse("bad_arguments", "arguments were not valid JSON; this tool is no longer available")
		}
		return refuse("bad_arguments", "arguments were not a valid JSON object; call again with a JSON object")
	}
	allowed := map[string]bool{}
	for _, p := range allowedParams(info) {
		allowed[p.Name] = true
	}
	params := map[string]any{}
	for k, v := range args {
		if !allowed[k] || v == nil {
			continue
		}
		params[k] = v
	}
	for _, p := range allowedParams(info) {
		if v, ok := params[p.Name]; ok && p.Type == "array" {
			if list := stringList(v, p.Max); len(list) > 0 {
				params[p.Name] = list
			} else {
				delete(params, p.Name)
			}
		}
	}
	lc.params = params
	lc.step.Params = params
	var missing []string
	for _, p := range allowedParams(info) {
		if requiredOf(info)[p.Name] && isPlaceholder(ph, params[p.Name]) {
			missing = append(missing, p.Name)
			delete(params, p.Name)
		}
	}
	if len(missing) > 0 {
		lc.step.State, lc.step.Reason = "not_run", "needs_input"
		lc.result = &ToolResult{Tool: id, OK: false, Status: "needs_input", Error: "missing: " + strings.Join(missing, ",")}
		return lc
	}
	key := id + " " + normalizedArgs(params)
	if seen[key] {
		return refuse("duplicate", "this exact call already ran in this turn; use its result")
	}
	seen[key] = true
	if l.Record || l.Run == nil {
		lc.step.State, lc.step.Reason = "not_run", "shadow"
		raw, _ := json.Marshal(map[string]any{"ok": true, "status": "not_run", "note": "this turn only records the choice"})
		lc.reply = string(raw)
		return lc
	}
	lc.run = true // the turn's limits are checked right before it runs (Guard)
	return lc
}

// stringList is an array parameter as the tool takes it: its texts trimmed, empty ones and anything but
// text dropped, at most max of them (nil: no bound). A lone text, which small models send for a list of
// one, is a list of one.
func stringList(v any, max *int) []any {
	var in []any
	switch x := v.(type) {
	case []any:
		in = x
	case string:
		in = []any{x}
	}
	var out []any
	for _, it := range in {
		if s, ok := it.(string); ok && strings.TrimSpace(s) != "" {
			if max != nil && len(out) >= *max {
				break
			}
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}

func clipName(s string) string {
	r := []rune(strings.TrimSpace(s))
	return string(r[:min(64, len(r))])
}

// parseArgs reads a call's arguments as a JSON object. Arguments that are not valid JSON get one repair
// (repairArgs) before they are refused; repaired reports that the repair made them valid.
func parseArgs(raw string) (args map[string]any, repaired, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return map[string]any{}, false, true
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err == nil && m != nil {
		return m, false, true
	}
	fixed, ok := repairArgs(raw)
	if !ok {
		return nil, false, false
	}
	m = nil
	if err := json.Unmarshal([]byte(fixed), &m); err != nil || m == nil {
		return nil, false, false
	}
	return m, true, true
}

// repairArgs fixes the small damage small models do to JSON arguments: a code fence around them, a
// trailing comma before } or ], raw control characters inside strings (newline, tab and carriage return
// are escaped, the rest dropped), closers left out at the end, and surplus closers after the value. It
// guesses nothing else: a string still open at the end, a closer that does not match, or anything but
// closers after the value is not repaired. The caller still parses the result and takes only an object.
func repairArgs(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```")
		if i := strings.IndexByte(s, '\n'); i >= 0 && !strings.ContainsAny(s[:i], "{[") {
			s = s[i+1:] // the fence's language tag
		}
		s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "```"))
	}
	space := func(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
	out := make([]byte, 0, len(s)+4)
	dropComma := func() {
		end := len(out)
		for end > 0 && space(out[end-1]) {
			end--
		}
		if end > 0 && out[end-1] == ',' {
			out = append(out[:end-1], out[end:]...)
		}
	}
	var stack []byte
	inStr, esc, done := false, false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case done: // after the value only surplus closers and space may follow
			if c != '}' && c != ']' && !space(c) {
				return "", false
			}
			continue
		case inStr:
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			case c == '\n':
				out = append(out, `\n`...)
				continue
			case c == '\r':
				out = append(out, `\r`...)
				continue
			case c == '\t':
				out = append(out, `\t`...)
				continue
			case c < 0x20:
				continue
			}
		case c == '"':
			inStr = true
		case c == '{' || c == '[':
			stack = append(stack, c)
		case c == '}' || c == ']':
			open := byte('{')
			if c == ']' {
				open = '['
			}
			if len(stack) == 0 || stack[len(stack)-1] != open {
				return "", false
			}
			dropComma()
			stack = stack[:len(stack)-1]
			done = len(stack) == 0
		}
		out = append(out, c)
	}
	if inStr {
		return "", false
	}
	if len(stack) > 0 {
		// a number cut off at the end ("limit": 1 of 10) would parse as another value: like an open
		// string, it is not guessed
		end := len(out) - 1
		for end >= 0 && (out[end] == ' ' || out[end] == '\t' || out[end] == '\n' || out[end] == '\r') {
			end--
		}
		if end >= 0 && strings.IndexByte("0123456789.-+eE", out[end]) >= 0 {
			return "", false
		}
	}
	for i := len(stack) - 1; i >= 0; i-- {
		dropComma()
		if stack[i] == '{' {
			out = append(out, '}')
		} else {
			out = append(out, ']')
		}
	}
	return string(out), true
}

// normalizedArgs is the arguments as compared for repeats: keys sorted, text trimmed and lower-cased.
func normalizedArgs(params map[string]any) string {
	norm := make(map[string]any, len(params))
	for k, v := range params {
		if s, ok := v.(string); ok {
			v = strings.ToLower(strings.TrimSpace(s))
		}
		norm[k] = v
	}
	raw, _ := json.Marshal(norm)
	return string(raw)
}

// loopResultMessage is what the loop model reads of one result: the answer model's view of it without
// the page text of search results (resultsWithoutPages), cut to loopResultChars, marked as untrusted
// data, its texts cleaned and the whole fenced with the turn's nonce (untrusted.go).
func loopResultMessage(r ToolResult, nonce string) string {
	env := map[string]any{"tool": r.Tool, "untrusted_data": true, "ok": r.OK}
	if r.Status != "" {
		env["status"] = r.Status
	}
	if r.Error != "" {
		e := []rune(cleanToolText(r.Error))
		env["error"] = string(e[:min(200, len(e))])
	}
	if data := resultsWithoutPages(cleanToolData(r.Data)); len(data) > 0 {
		env["data"] = data
	}
	raw, _ := json.Marshal(fitTail(env, loopResultChars))
	return fenceToolResult(nonce, string(raw))
}

// pageKeys are the keys of a search result that carry text read from its page (a passage, a service's
// page text). The answer reads them; the loop model does not: it only judges whether more is needed, which
// titles, snippets and the read flag tell, and in loopResultChars a page's text would push the other
// results out of its view (fitTail drops a list's last items first).
var pageKeys = []string{"passage", "text"}

// resultsWithoutPages is data whose results list has no page text. Data without any is returned as it
// came; otherwise data and the results with page text are copied, never changed.
func resultsWithoutPages(data map[string]any) map[string]any {
	items := resultItems(data)
	found := false
	for _, it := range items {
		for _, k := range pageKeys {
			_, has := it[k]
			found = found || has
		}
	}
	if !found {
		return data
	}
	list := make([]any, len(items))
	for i, it := range items {
		c := make(map[string]any, len(it))
		for k, v := range it {
			if !slices.Contains(pageKeys, k) {
				c[k] = v
			}
		}
		list[i] = c
	}
	out := make(map[string]any, len(data))
	for k, v := range data {
		out[k] = v
	}
	out["results"] = list
	return out
}

// resultItems is the results list of a tool's data (a web search's), each item as an object; nil when
// the data has no list of objects.
func resultItems(data map[string]any) []map[string]any {
	var items []map[string]any
	switch list := data["results"].(type) {
	case []any:
		for _, it := range list {
			m, ok := it.(map[string]any)
			if !ok {
				return nil
			}
			items = append(items, m)
		}
	case []map[string]any:
		items = list
	}
	return items
}

func fallbackReason(err error) string {
	if errors.Is(err, ErrToolsUnsupported) || errclass.Of(err) == errclass.Unsupported {
		return "unsupported"
	}
	c := errclass.Of(err)
	if c == "" {
		c = errclass.Unknown
	}
	return "error:" + string(c)
}

// loopRun is a tool loop running beside the planner.
type loopRun struct {
	mode    string
	done    chan struct{} // nil when the loop did not start
	results []ToolResult
	trace   *LoopTrace
	err     error
}

// wait returns the loop's results and whether they are the turn's tools (loop mode, no fallback).
func (lr *loopRun) wait() ([]ToolResult, bool) {
	if lr == nil {
		return nil, false
	}
	if lr.done != nil {
		<-lr.done
	}
	return lr.results, lr.mode == ToolModeLoop && lr.err == nil && lr.trace.Fallback == ""
}

// running reports whether the loop is still at work.
func (lr *loopRun) running() bool {
	if lr == nil || lr.done == nil {
		return false
	}
	select {
	case <-lr.done:
		return false
	default:
		return true
	}
}

// costAhead is what the loop counts for in the money left to a step that starts beside it: what it cost
// when it has ended, and while it runs what all its rounds can cost, each at the price of roundUSD (the
// loop asks the planner's small model unless chat.tool_model says otherwise).
func (lr *loopRun) costAhead(roundUSD float64) float64 {
	switch {
	case lr == nil:
		return 0
	case lr.running():
		return MaxLoopRounds * roundUSD
	}
	return lr.trace.CostUSD
}

// startLoop starts the tool loop for the loop and shadow modes and returns the tools the planner may
// still choose: all of them in planner and shadow mode, none in loop mode (memory.search is added by
// the caller), all again when the model cannot take tools.
func (rt *Runtime) startLoop(ctx context.Context, req Request, msg, timeRef string, others Toolset) (*loopRun, Toolset) {
	mode := req.ToolMode
	if mode != ToolModeLoop && mode != ToolModeShadow {
		return nil, others
	}
	lr := &loopRun{mode: mode, trace: &LoopTrace{Mode: mode}}
	role := req.LoopModel
	if role == "" {
		role = req.PlannerModel
	}
	llm, ok := rt.LLM.(LoopLLM)
	switch {
	case !ok:
		lr.trace.Stop, lr.trace.Fallback = "fallback", "no_tool_calling"
		return lr, others
	case !llm.ToolsReady(role):
		lr.trace.Stop, lr.trace.Fallback = "fallback", "unsupported"
		return lr, others
	case !hasContent(msg):
		lr.trace.Stop = "prefilter"
	case len(ToolSchemas(others)) == 0:
		lr.trace.Stop = "no_tools"
	default:
		lr.done = make(chan struct{})
		l := ToolLoop{LLM: llm, Run: rt.Tools, Role: role, Voice: req.Voice, Guard: req.Guard, Record: mode == ToolModeShadow, Final: req.LoopFinal}
		if mode == ToolModeLoop {
			l.OnProgress = req.OnProgress
		}
		go func() {
			defer close(lr.done)
			lr.results, lr.trace, lr.err = l.Do(ctx, others, req.History, msg, timeRef)
		}()
	}
	if mode == ToolModeShadow {
		return lr, others
	}
	return lr, Toolset{Known: others.Known}
}
