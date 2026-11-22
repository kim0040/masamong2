package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// loopState is what one Do call keeps from round to round: the menu by id and by function name, the
// tools closed for the rest of the turn, the calls already made, and the results so far.
type loopState struct {
	l         ToolLoop
	tr        *LoopTrace
	infos     map[string]ToolInfo
	byName    map[string]string
	ph        *regexp.Regexp
	nonce     string          // the turn's tool result fences (untrusted.go)
	removed   map[string]bool // closed for the turn (Removed in the trace)
	done      map[string]bool // Once tools that succeeded in an earlier round
	failed    map[string]int
	badArgs   map[string]int
	seen      map[string]bool // tool and arguments of the calls already made
	webSeen   bool            // a result held web content: the writing tools are closed
	results   []ToolResult
	needInput []ToolResult
}

// newLoopState is the state of a Do call over the menu and its schemas, before the first round.
func (l ToolLoop) newLoopState(menu Toolset, specs []ToolSpec, tr *LoopTrace) *loopState {
	s := &loopState{l: l, tr: tr, infos: map[string]ToolInfo{}, byName: map[string]string{}}
	for _, t := range menu.Enabled {
		s.infos[t.ID] = t
	}
	for _, sp := range specs {
		s.byName[sp.Name], s.byName[sp.ID] = sp.ID, sp.ID
	}
	s.ph = l.Voice.placeholder()
	s.nonce = newFenceNonce()
	s.removed, s.done = map[string]bool{}, map[string]bool{}
	s.failed, s.badArgs = map[string]int{}, map[string]int{}
	s.seen = map[string]bool{}
	return s
}

// remove closes a tool for the rest of the turn.
func (s *loopState) remove(id string) {
	if !s.removed[id] {
		s.removed[id] = true
		s.tr.Removed = append(s.tr.Removed, id)
	}
}

// offer is the round's tools: the menu less the tools closed for the turn.
func (s *loopState) offer(specs []ToolSpec) []ToolSpec {
	var offer []ToolSpec
	for _, sp := range specs {
		if s.removed[sp.ID] || s.done[sp.ID] || (s.webSeen && s.infos[sp.ID].Writes) {
			continue
		}
		offer = append(offer, sp)
	}
	return offer
}

// prepareCalls checks each call of the reply (prepare) and returns the calls with the assistant message
// that asked for them.
func (s *loopState) prepareCalls(reply LoopReply, round int) (LoopMsg, []*loopCall) {
	assistant := LoopMsg{Role: "assistant", Content: reply.Text, Calls: reply.Calls, Raw: reply.Raw}
	calls := make([]*loopCall, len(reply.Calls))
	for i, c := range reply.Calls {
		if strings.TrimSpace(c.ID) == "" {
			// a host that sends no call IDs gets them made up, and the assistant message is rebuilt
			// with them instead of sent back as it came
			c.ID = fmt.Sprintf("call_%d_%d", round, i+1)
			assistant.Calls[i].ID, assistant.Raw = c.ID, nil
		}
		calls[i] = s.prepare(c, round, i)
	}
	return assistant, calls
}

// runCalls runs the round's calls that prepare let through.
func (s *loopState) runCalls(ctx context.Context, calls []*loopCall, round int, progress func(Progress)) {
	l := s.l
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
		info := s.infos[lc.id]
		cancel := func() {
			lc.run = false
			lc.step.State, lc.step.Reason = "not_run", "cancelled"
			lc.result = &ToolResult{Tool: lc.id, Status: "cancelled", Data: searchQueryData(lc.id, lc.params)}
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
				lc.result = &ToolResult{Tool: lc.id, OK: false, Status: "unavailable", Data: searchQueryData(lc.id, lc.params)}
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
}

// settle takes in the round's results in the model's order: the trace's steps, the tools closed by them
// and the tool message of each call, appended to msgs.
func (s *loopState) settle(calls []*loopCall, msgs []LoopMsg) []LoopMsg {
	for _, lc := range calls {
		switch {
		case lc.run:
			r := *lc.result
			s.results = append(s.results, r)
			lc.step.State = "ran"
			if !r.OK {
				lc.step.State = "failed"
				if s.failed[lc.id]++; s.failed[lc.id] >= 2 {
					s.remove(lc.id)
				}
			}
			if readWeb(s.infos[lc.id], r) {
				s.webSeen = true
			}
			if r.OK && s.infos[lc.id].Once {
				s.done[lc.id] = true // closed from the next round on; this round's calls have all run
			}
			lc.reply = loopResultMessage(r, s.nonce)
		case lc.result != nil: // answered without running (limit, needs_input)
			if lc.result.Status == "needs_input" {
				s.needInput = append(s.needInput, *lc.result)
			} else {
				s.results = append(s.results, *lc.result)
			}
			lc.reply = loopResultMessage(*lc.result, s.nonce)
		}
		if lc.step.State == "refused" {
			s.tr.Refused++
		}
		s.tr.Steps = append(s.tr.Steps, lc.step)
		msgs = append(msgs, LoopMsg{Role: "tool", CallID: lc.call.ID, Content: lc.reply})
	}
	return msgs
}

// withNeedInput is the loop's results: a value the loop could not fill reaches the answer (so it asks),
// unless the tool ran anyway later.
func (s *loopState) withNeedInput() []ToolResult {
	results := s.results
	for _, ni := range s.needInput {
		ran := false
		for _, r := range results {
			ran = ran || r.Tool == ni.Tool
		}
		if !ran {
			results = append(results, ni)
		}
	}
	return results
}

// prepare checks one call before anything runs: a known tool still on the menu, valid JSON arguments
// limited to the schema's parameters, real values for the required ones, not a repeat, and the turn's
// limits. Only then is it marked to run.
func (s *loopState) prepare(c LoopCall, round, idx int) *loopCall {
	lc := &loopCall{call: c}
	id, known := s.byName[strings.TrimSpace(c.Name)]
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
	if s.removed[id] {
		return refuse("removed", "this tool is no longer available in this turn")
	}
	if s.done[id] {
		return refuse("done", "this tool already did its work in this turn; its result above stands")
	}
	info := s.infos[id]
	if s.webSeen && info.Writes {
		return refuse("writes_after_web", "this tool is not available after reading web content")
	}
	args, repaired, ok := parseArgs(c.Arguments)
	lc.step.Repaired = repaired
	if !ok {
		if s.badArgs[id]++; s.badArgs[id] >= 2 {
			s.remove(id)
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
	for _, p := range allowedParams(info) {
		// a closed choice is checked before the call runs: the schema offers only its values, but a model
		// may still send another one (reminders.update {op: confirm} would agree for the person)
		if v, ok := params[p.Name]; ok && len(p.Enum) > 0 {
			if s, isText := v.(string); isText && strings.TrimSpace(s) == "" {
				delete(params, p.Name) // left empty: as if not given
				continue
			}
			choice, ok := enumChoice(v, p.Enum)
			if !ok {
				return refuse("bad_arguments", p.Name+" must be one of: "+strings.Join(p.Enum, ", "))
			}
			params[p.Name] = choice
		}
	}
	lc.params = params
	lc.step.Params = params
	var missing []string
	for _, p := range allowedParams(info) {
		if requiredOf(info)[p.Name] && isPlaceholder(s.ph, params[p.Name]) {
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
	if s.seen[key] {
		return refuse("duplicate", "this exact call already ran in this turn; use its result")
	}
	s.seen[key] = true
	if s.l.Record || s.l.Run == nil {
		lc.step.State, lc.step.Reason = "not_run", "shadow"
		raw, _ := json.Marshal(map[string]any{"ok": true, "status": "not_run", "note": "this turn only records the choice"})
		lc.reply = string(raw)
		return lc
	}
	lc.run = true // the turn's limits are checked right before it runs (Guard)
	return lc
}
