package application

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"masamong/internal/action"
	"masamong/internal/catalog"
	"masamong/internal/chat"
	"masamong/internal/config"
	"masamong/internal/crash"
	"masamong/internal/errclass"
	"masamong/internal/errcode"
	"masamong/internal/lang"
	"masamong/internal/providers"
	"masamong/internal/tools"
)

// The tool layer sits in front of Execute: every tool Action — whether the planner, a Discord
// command, the CLI or the GUI calls it — passes the same switch (instance, feature flag, channel,
// surface) and the same usage limits. Operator-defined HTTP tools run here too.

var toolUsages sync.Map       // *Service → *tools.Usage
var invalidTimezones sync.Map // timezone string → warned once

func (s *Service) toolUsage() *tools.Usage {
	if s.Store == nil {
		return nil
	}
	if v, ok := toolUsages.Load(s); ok {
		return v.(*tools.Usage)
	}
	u, err := tools.OpenUsage(s.Store)
	if err != nil {
		s.Log.Warn("tools: usage table unavailable", "err", err)
		return nil
	}
	v, _ := toolUsages.LoadOrStore(s, u)
	return v.(*tools.Usage)
}

// toolRegistry is every tool with its effective settings for the current config revision.
func (s *Service) toolRegistry() []tools.Resolved {
	all := tools.Resolve(s.Doc().Tools, s.Doc().Features)
	// Deprecated memory.chat_tools: a whitelist for built-ins whose chat flag is not set explicitly.
	if legacy := s.Doc().Memory.ChatTools; len(legacy) > 0 {
		allowed := map[string]bool{"memory.search": true}
		for _, id := range legacy {
			allowed[id] = true
		}
		for i, r := range all {
			if r.Kind == "builtin" && s.Doc().Tools[r.ID].Chat == nil && !allowed[r.ID] {
				all[i].Chat = false
			}
		}
	}
	return all
}

func (s *Service) instanceZone() *time.Location { return zoneOf(s.Doc()) }

func zoneOf(doc config.Document) *time.Location {
	if doc.I18n.Timezone == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(doc.I18n.Timezone)
	if err == nil {
		return loc
	}
	if _, warned := invalidTimezones.LoadOrStore(doc.I18n.Timezone, true); !warned {
		slog.Default().Error("unknown configured timezone; using UTC until it is corrected", "timezone", doc.I18n.Timezone, "err", err)
	}
	return time.UTC
}

func (s *Service) toolDay() (string, time.Time) {
	now := s.Clock.Now()
	return now.In(s.instanceZone()).Format("2006-01-02"), now
}

func channelKeyOf(inv *action.Invoker) string {
	if inv == nil || inv.ChannelID == "" {
		return ""
	}
	return "discord:" + inv.GuildID + ":" + inv.ChannelID
}

// toolAllowedHere reports whether an enabled tool may run for this invoker (channel switch and
// surface: the planner only uses chat tools, Discord only command tools).
func (s *Service) toolAllowedHere(r tools.Resolved, inv *action.Invoker) (bool, string) {
	if inv == nil {
		return true, ""
	}
	if inv.GuildID == "" && inv.ChannelID != "" && r.ID == "memory.search" {
		return false, "tool.channel_off"
	}
	pol := s.channelPolicy(inv.GuildID, inv.ChannelID)
	override, has := pol.Tools[r.ID]
	if has && !override {
		return false, "tool.channel_off"
	}
	switch inv.Via {
	case "chat":
		if !r.Chat && !(has && override) {
			return false, "tool.not_for_surface"
		}
	case "command":
		if !r.Commands {
			return false, "tool.not_for_surface"
		}
	}
	return true, ""
}

// reserveTool applies r's limits for this caller and counts the call. nil = allowed.
func (s *Service) reserveTool(req action.Request, r tools.Resolved) *action.Result {
	u := s.toolUsage()
	if u == nil {
		return nil
	}
	caller := tools.Caller{Channel: channelKeyOf(req.Invoker)}
	if req.Invoker != nil {
		caller.UserID = req.Invoker.UserID
	}
	day, now := s.toolDay()
	dec, err := u.Reserve(day, now, r.ID, r.Limits, caller)
	if err != nil {
		s.Log.Warn("tools: usage write failed", "tool", r.ID, "err", err)
		return nil // counting is best effort; a DB hiccup must not take tools down
	}
	if dec.OK {
		return nil
	}
	args := map[string]any{"tool": r.ID, "scope": dec.Scope, "used": dec.Used, "limit": dec.Limit}
	key, msg := "tool.limited", s.msg(req, "tool.limited", "tool", r.ID, "used", dec.Used, "limit", dec.Limit)
	if dec.Scope == "cooldown" {
		secs := int(dec.RetryAfter.Seconds())
		args["retry_after_s"] = secs
		key, msg = "tool.cooldown", s.msg(req, "tool.cooldown", "tool", r.ID, "seconds", secs)
	}
	res := action.Fail(req, errcode.ToolLimited, key, msg, args)
	res.Error.Retryable = dec.Scope == "cooldown"
	return &res
}

// gateTool is the tool check run by Execute. It returns the request with tool defaults filled in,
// or a refusal. Non-tool Actions pass through unchanged.
func (s *Service) gateTool(req action.Request) (action.Request, *action.Result) {
	r, ok := tools.Find(s.toolRegistry(), req.Action)
	if !ok || r.ID == "chat.respond" { // chat turns are gated inside ChatTurn
		return req, nil
	}
	if !r.Enabled {
		res := action.Fail(req, errcode.FeatureDisabled, "tool.disabled", s.msg(req, "tool.disabled", "tool", r.ID), map[string]any{"tool": r.ID, "reason": r.Reason})
		return req, &res
	}
	if inv := req.Invoker; inv != nil && (inv.Via == "chat" || inv.Via == "command") && s.Paused(inv.GuildID, inv.ChannelID) {
		res := action.Fail(req, errcode.FeatureDisabled, "runtime.paused", s.msg(req, "runtime.paused"), map[string]any{"tool": r.ID})
		return req, &res
	}
	if ok, key := s.toolAllowedHere(r, req.Invoker); !ok {
		res := action.Fail(req, errcode.FeatureDisabled, key, s.msg(req, key, "tool", r.ID), map[string]any{"tool": r.ID})
		return req, &res
	}
	if len(r.Defaults) > 0 {
		in := inputMap(req)
		for k, v := range r.Defaults {
			if _, set := in[k]; !set {
				in[k] = v
				if k == "region" {
					in["region_source"] = "default" // the weather says it assumed the place
				}
			}
		}
		req.Input, _ = json.Marshal(in)
	}
	// The console listing polls or reading help is not tool use by the bot; it neither counts nor
	// hits limits. Anything that reaches an external API (tests included) still counts.
	if d, ok := catalog.Lookup(req.Action); ok && d.Effects == "read" && req.Invoker == nil && strings.HasPrefix(req.RequestID, "web") {
		return req, nil
	}
	if r.ID == "images.generate" && s.imageQueueRefuses(req) {
		return req, nil // the queue answers a repeat or a full line without drawing: not a use of the tool
	}
	if refused := s.reserveTool(req, r); refused != nil {
		return req, refused
	}
	return req, nil
}

// ---------------------------------------------------------------------------
// Operator-defined HTTP tools
// ---------------------------------------------------------------------------

func (s *Service) runHTTPTool(ctx context.Context, req action.Request, r tools.Resolved) action.Result {
	h := r.HTTP
	in := inputMap(req)
	var missing []string
	for _, p := range r.Required {
		if strVal(in, p, "") == "" {
			missing = append(missing, p)
		}
	}
	if len(missing) > 0 {
		return action.Fail(req, errcode.InvalidInput, "tool.missing_params", s.msg(req, "tool.missing_params", "tool", r.ID, "params", strings.Join(missing, ", ")), map[string]any{"missing": missing})
	}
	secret := ""
	if h.CredentialRef != "" {
		cred, err := providers.Resolve(h.CredentialRef)
		if err != nil || !cred.Present() {
			return action.OK(req, map[string]any{"status": "unconfigured", "live": false, "tool": r.ID,
				"message": s.msg(req, "provider.unconfigured", "provider", r.ID)})
		}
		secret = cred.Value()
	}
	target, ok := fillURL(h.URL, in, secret)
	if !ok {
		// "." or ".." in a path place would call another path of the operator's host
		return action.Fail(req, errcode.InvalidInput, "generic.error", "a path parameter cannot be . or ..", nil)
	}
	headers := map[string]string{"Accept": "application/json"}
	for k, v := range h.Headers {
		headers[k] = strings.ReplaceAll(v, "{credential}", secret)
	}
	timeout := time.Duration(max(1, h.TimeoutSec)) * time.Second
	if h.TimeoutSec == 0 {
		timeout = 10 * time.Second
	}
	scrub := newScrubber(secret)
	if secret != "" && !scrub.active() {
		s.Log.WarnContext(ctx, "tool credential is too short to scrub from responses; use a longer key", "tool", r.ID, "min_chars", minScrubbedSecret)
	}
	client := providers.NewClient(timeout, s.Log)
	client.MaxBody = 2 << 20
	resp, err := client.Get(ctx, target, headers)
	host := hostOnly(h.URL)
	if err != nil {
		msg := scrub.text(err.Error())
		var pe *providers.Error
		retry := !(errors.As(err, &pe) && !pe.Retryable())
		res := action.Fail(req, errcode.ProviderUnavailable, "generic.error", msg, map[string]any{"tool": r.ID, "host": host})
		res.Error.Retryable = retry
		return res
	}
	limit := h.MaxChars
	if limit == 0 {
		limit = 3000
	}
	out := map[string]any{"status": "ok", "live": true, "source": "http:" + host, "tool": r.ID}
	var data any
	if json.Unmarshal(resp.Body, &data) == nil {
		if len(h.Pick) > 0 {
			data = pickFields(data, h.Pick)
		}
		data = scrub.value(data)
		raw, _ := json.Marshal(data)
		if len([]rune(string(raw))) <= limit {
			out["data"] = data
		} else {
			out["data_text"], out["truncated"] = string([]rune(string(raw))[:limit]), true
		}
	} else {
		text := []rune(scrub.text(strings.TrimSpace(string(resp.Body))))
		out["data_text"] = string(text[:min(limit, len(text))])
		out["truncated"] = len(text) > limit
	}
	return action.OK(req, out)
}

// minScrubbedSecret is the shortest credential removed from tool responses: a shorter one would also
// mangle ordinary words of the answer, and a key that short is too weak to matter.
const minScrubbedSecret = 8

// scrubber removes one credential, in every form a server may echo it back in, from a tool's response.
// It is built once per tool call: compiling its patterns per string made a large response pay for
// thousands of compiles.
type scrubber struct {
	encoded  *regexp.Regexp // percent-escapes, whose hex digits are case-insensitive
	literals *strings.Replacer
}

func newScrubber(secret string) *scrubber {
	if len(secret) < minScrubbedSecret {
		return &scrubber{}
	}
	// Percent-escape hex digits are case-insensitive. Conservatively scrub mixed-case encodings too.
	var enc []string
	for _, e := range []string{url.QueryEscape(secret), url.PathEscape(secret), strings.ReplaceAll(url.QueryEscape(secret), "+", "%20")} {
		enc = append(enc, regexp.QuoteMeta(e))
	}
	quoted, _ := json.Marshal(secret)
	forms := []string{secret, url.QueryEscape(secret), url.PathEscape(secret),
		base64.StdEncoding.EncodeToString([]byte(secret)), base64.RawStdEncoding.EncodeToString([]byte(secret)),
		base64.URLEncoding.EncodeToString([]byte(secret)), base64.RawURLEncoding.EncodeToString([]byte(secret)),
		string(quoted[1 : len(quoted)-1])}
	forms = append(forms, strings.ReplaceAll(url.QueryEscape(secret), "+", "%20"), strings.ReplaceAll(string(quoted[1:len(quoted)-1]), "/", `\/`))
	var unicodeForm strings.Builder
	for _, code := range utf16.Encode([]rune(secret)) {
		fmt.Fprintf(&unicodeForm, `\u%04x`, code)
	}
	forms = append(forms, unicodeForm.String())
	sort.Slice(forms, func(i, j int) bool { return len(forms[i]) > len(forms[j]) })
	var pairs []string
	for _, f := range forms {
		if f != "" {
			pairs = append(pairs, f, "[redacted]")
		}
	}
	return &scrubber{encoded: regexp.MustCompile("(?i)" + strings.Join(enc, "|")), literals: strings.NewReplacer(pairs...)}
}

// active is false for no credential, or one too short to scrub.
func (sc *scrubber) active() bool { return sc.encoded != nil }

func (sc *scrubber) text(text string) string {
	if !sc.active() {
		return text
	}
	return sc.literals.Replace(sc.encoded.ReplaceAllString(text, "[redacted]"))
}

func (sc *scrubber) value(v any) any {
	if !sc.active() {
		return v
	}
	switch x := v.(type) {
	case string:
		return sc.text(x)
	case []any:
		for i := range x {
			x[i] = sc.value(x[i])
		}
		return x
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, v := range x {
			out[sc.text(k)] = sc.value(v)
		}
		return out
	case float64, bool:
		raw, _ := json.Marshal(v)
		if scrubbed := sc.text(string(raw)); scrubbed != string(raw) {
			return scrubbed
		}
	}
	return v
}

// fillURL substitutes {param} with escaped values (path-escaped before '?', query-escaped after). ok is
// false when a path value is "." or "..": escaping leaves dots alone, so such a value would move the
// request to another path of the same host.
func fillURL(tpl string, in map[string]any, secret string) (string, bool) {
	var b strings.Builder
	inQuery := false
	for i := 0; i < len(tpl); {
		c := tpl[i]
		if c == '?' {
			inQuery = true
		}
		if c == '{' {
			if j := strings.IndexByte(tpl[i:], '}'); j > 0 {
				name := tpl[i+1 : i+j]
				val := secret
				if name != "credential" {
					val = strVal(in, name, "")
				}
				if inQuery {
					b.WriteString(url.QueryEscape(val))
				} else {
					if v := strings.TrimSpace(val); v == "." || v == ".." {
						return "", false
					}
					b.WriteString(url.PathEscape(val))
				}
				i += j + 1
				continue
			}
		}
		b.WriteByte(c)
		i++
	}
	return b.String(), true
}

func hostOnly(raw string) string {
	u, err := url.Parse(strings.NewReplacer("{", "", "}", "").Replace(raw))
	if err != nil {
		return ""
	}
	return u.Host
}

// pickFields keeps only the listed dotted paths of a JSON value ("a.b" reads obj["a"]["b"];
// arrays are mapped element-wise).
func pickFields(v any, paths []string) any {
	out := map[string]any{}
	for _, p := range paths {
		if got, ok := dig(v, strings.Split(p, ".")); ok {
			out[p] = got
		}
	}
	return out
}

func dig(v any, parts []string) (any, bool) {
	if len(parts) == 0 {
		return v, true
	}
	switch x := v.(type) {
	case map[string]any:
		next, ok := x[parts[0]]
		if !ok {
			return nil, false
		}
		return dig(next, parts[1:])
	case []any:
		var out []any
		for _, e := range x {
			if got, ok := dig(e, parts); ok {
				out = append(out, got)
			}
		}
		return out, len(out) > 0
	}
	return nil, false
}

// ---------------------------------------------------------------------------
// Chat integration
// ---------------------------------------------------------------------------

// voiceFor picks the reply language: the channel persona's locale, then the guild locale, then the
// instance default. The bot name is the first configured name.
func (s *Service) voiceFor(guildID string, pol config.ChannelPolicy) chat.Voice {
	var tags []string
	if p, ok := s.Doc().Personas[pol.PersonaRef]; ok && p.Locale != "" {
		tags = append(tags, p.Locale)
	} else if p, ok := config.BuiltinPersonas()[pol.PersonaRef]; ok && p.Locale != "" {
		tags = append(tags, p.Locale)
	}
	if g, ok := s.Doc().Guilds[guildID]; ok {
		tags = append(tags, g.Locale)
	}
	tags = append(tags, s.Doc().I18n.DefaultLocale)
	return chat.Voice{Pack: lang.For(tags...), BotName: s.Doc().BotNames()[0]}
}

// memoryUsable reports whether memory search can run for this caller (instance retrieve mode and,
// for a channel, its memory_use policy).
func (s *Service) memoryUsable(inv *action.Invoker) bool {
	if inv != nil && inv.GuildID == "" && inv.ChannelID != "" {
		return false
	}
	if s.Doc().Memory.Retrieve == "off" {
		return false
	}
	if inv == nil || inv.ChannelID == "" {
		return true
	}
	return s.channelPolicy(inv.GuildID, inv.ChannelID).MemoryUse != "off"
}

// toolPays marks the tools that spend money of their own in a chat turn (Guard.Spend): a model call
// (Uses "model": a picture, a summary, a fortune), a paid web search, and a quote that falls back to
// one. Under a money cap the turn's later steps wait for them (chat runSteps).
func toolPays(r tools.Resolved) bool {
	return r.Uses == "model" || r.ID == "search.web" || r.ID == "finance.quote"
}

// toolInfo renders one tool for the planner in the voice's language.
func toolInfo(r tools.Resolved, p *lang.Pack, params []tools.Param) chat.ToolInfo {
	info := chat.ToolInfo{ID: r.ID, Description: r.Description, Route: r.Route, Required: r.Required, Writes: r.Writes, Once: r.Once, Final: r.Final, Web: r.WebContent,
		Pays: toolPays(r)}
	t := p.Tools[r.ID]
	if info.Description == "" {
		info.Description = t.Description
	}
	if info.Route == "" {
		info.Route = t.Route
	}
	// the tool loop's view: an operator's description wins over the pack's purpose line
	info.Purpose = r.Description
	if info.Purpose == "" {
		info.Purpose = t.Purpose
	}
	info.When, info.Skip = t.When, t.Skip
	info.Params = loopParams(r.Inputs, t.Params, params)
	if len(params) > 0 {
		names := make([]string, 0, len(params))
		for _, prm := range params {
			names = append(names, "parameters."+prm.Name)
		}
		info.Description = strings.TrimSpace(info.Description) + " " + strings.Join(names, ", ")
	}
	return info
}

// loopParams describes a tool's inputs for the tool loop's function schema. The type follows the
// console's input type; a choice of numbers becomes an integer range; the search service choice
// (provider) is the operator's, not the model's. The hint is the pack's, else an operator parameter's
// description, else the input's language-neutral example.
func loopParams(inputs []tools.Input, hints map[string]string, operator []tools.Param) []chat.ToolParam {
	desc := map[string]string{}
	for _, p := range operator {
		desc[p.Name] = p.Description
	}
	var out []chat.ToolParam
	for _, in := range inputs {
		if in.Type == "provider" || in.Operator {
			continue
		}
		cp := chat.ToolParam{Name: in.Name, Required: in.Required, Hint: hints[in.Name], Type: "string"}
		if cp.Hint == "" {
			cp.Hint = desc[in.Name]
		}
		if cp.Hint == "" {
			cp.Hint = in.Example
		}
		switch in.Type {
		case "number":
			cp.Type = "integer"
			if in.Max > 0 {
				lo, hi := in.Min, in.Max
				cp.Min, cp.Max = &lo, &hi
			}
		case "bool":
			cp.Type = "boolean"
		case "select":
			if lo, hi, ok := intRange(in.Options); ok {
				cp.Type, cp.Min, cp.Max = "integer", &lo, &hi
			} else {
				cp.Enum = in.Options
			}
		}
		out = append(out, cp)
	}
	return out
}

// intRange is the bounds of a choice made only of whole numbers.
func intRange(options []string) (int, int, bool) {
	if len(options) == 0 {
		return 0, 0, false
	}
	lo, hi := 0, 0
	for i, o := range options {
		n, err := strconv.Atoi(o)
		if err != nil {
			return 0, 0, false
		}
		if i == 0 || n < lo {
			lo = n
		}
		if i == 0 || n > hi {
			hi = n
		}
	}
	return lo, hi, true
}

// chatToolset is what the planner may use in one channel.
func (s *Service) chatToolset(v chat.Voice, guildID, channelID string) chat.Toolset {
	ts := chat.Toolset{Known: map[string]bool{}}
	inv := &action.Invoker{Via: "chat", GuildID: guildID, ChannelID: channelID}
	for _, r := range s.toolRegistry() {
		if r.ID == "chat.respond" {
			continue
		}
		ts.Known[r.ID] = true
		if !r.Enabled {
			continue
		}
		if ok, _ := s.toolAllowedHere(r, inv); !ok {
			continue
		}
		ts.Enabled = append(ts.Enabled, toolInfo(r, v.Pack, s.Doc().Tools[r.ID].Params))
	}
	return ts
}

// chatTool runs a planner-chosen tool through Execute, as the chat surface of the calling user.
// Scope-bound tools get the channel's scope injected; a dry-run turn does not create state.
func (s *Service) chatTool(in ChatInput, v chat.Voice) chat.ToolRunner {
	return func(ctx context.Context, tool string, params map[string]any) (out chat.ToolResult) {
		began := time.Now()
		_, spentBefore, _ := chat.GuardFrom(ctx).Totals()
		call := toolCall{Tool: tool}
		var res action.Result
		defer func() {
			if p := recover(); p != nil {
				_ = crash.Error(nil, "chat tool "+tool, p)
				out = chat.ToolResult{Tool: tool, Status: "unavailable"}
				call.Class = errclass.Unknown
				s.logToolProblem(in.GuildID, in.ChannelID, tool, res, true)
			} else if !in.DryRun {
				s.logToolProblem(in.GuildID, in.ChannelID, tool, res, false)
			}
			_, spentAfter, _ := chat.GuardFrom(ctx).Totals()
			call.Status, call.Latency, call.CostUSD = out.Status, time.Since(began), max(spentAfter-spentBefore, 0)
			if call.Status == "" {
				call.Status = "ok"
			}
			if data, err := json.Marshal(out.Data); err == nil && out.Data != nil {
				call.ResultChars = len(data)
			}
			s.noteToolCall(ctx, call)
		}()
		r, _ := tools.Find(s.toolRegistry(), tool)
		p := map[string]any{}
		for k, val := range params {
			p[k] = val
		}
		delete(p, "region_source") // where a place came from is the service's to say, not the model's
		delete(p, "context_lines") // the conversation lines of a reminder are picked here, never by the model
		delete(p, "async")         // how a picture is drawn is the operator's setting, never the model's
		if r.Scoped {
			p["scope"] = "discord:" + in.ChannelID
		}
		// the call ledger keeps the names of what the planner passed, never the values
		keys := declaredParamKeys(r.Inputs, params)
		call.ParamKeys = keys
		if in.DryRun && r.Writes {
			return chat.ToolResult{Tool: tool, OK: true, Status: "dry_run", Data: map[string]any{"status": "dry_run", "note": "not executed in a dry-run turn", "parameters": p}}
		}
		if s.chatToolFault(in.ChannelID, tool) {
			call.Class = errclass.Unknown
			return chat.ToolResult{Tool: tool, OK: false, Status: "unavailable"} // a test's injected outage
		}
		if tool == "reminders.propose" && (boolVal(p, "with_context") || strVal(p, "what", "") == "") {
			if lines := s.reminderContext(in); len(lines) > 0 {
				p["context_lines"] = lines
			}
		}
		raw, _ := json.Marshal(p)
		ctx = withChatOrigin(ctx, chatOrigin{MessageID: in.MessageID}) // where a background picture is posted (imagequeue.go)
		res = s.Execute(ctx, action.Request{Action: tool, Input: raw, Locale: v.Pack.Tag, RequestID: "chat",
			Invoker: &action.Invoker{Via: "chat", GuildID: in.GuildID, ChannelID: in.ChannelID, UserID: in.SpeakerID}})
		out = chatView(res)
		if out.Tool == "" {
			out.Tool = tool
		}
		if res.Error != nil {
			call.Class = toolErrClass(res.Error.Code)
		} else if out.Status == "unconfigured" {
			call.Class = toolErrClass(errcode.ProviderUnconfigured)
		}
		return out
	}
}

// chatView is what the answer model sees of a tool's result. Why a tool cannot answer is the operator's
// business: a missing key reaches the model as plain "unavailable", without the note for the operator
// ("the key is not set, add it under Settings") or a provider's error text. Shown those, the model told
// people "설정이 안 돼 있어서 / 키가 없어서", which nobody in the chat can do anything about. The command
// card and the console keep the full result.
func chatView(res action.Result) chat.ToolResult {
	out := chat.ToolResult{Tool: res.Action, OK: res.OK, Data: res.Output}
	if st, _ := res.Output["status"].(string); st == "unconfigured" {
		out.OK, out.Status = false, "unconfigured"
		data := make(map[string]any, len(res.Output))
		for k, v := range res.Output {
			if k != "message" {
				data[k] = v
			}
		}
		data["status"] = "unconfigured"
		out.Data = data
	}
	if st, _ := res.Output["status"].(string); st == "queued" || st == "already_queued" {
		out.Status = st // the answer says it is on its way, not that it is done
	}
	if !res.OK && res.Error != nil {
		out.Status, out.Error = res.Error.Code, res.Error.Message
		if res.Error.Code == errcode.ProviderUnconfigured {
			out.Status, out.Error = "unconfigured", ""
		} else if strings.HasPrefix(res.Error.Code, "PROVIDER_") {
			out.Status, out.Error = "unavailable", ""
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// tools.* Actions
// ---------------------------------------------------------------------------

func (s *Service) toolsList(req action.Request) action.Result {
	day, _ := s.toolDay()
	used := map[string]int{}
	if u := s.toolUsage(); u != nil {
		used, _ = u.Today(day)
	}
	p := lang.For(req.Locale, s.Doc().I18n.DefaultLocale)
	var list []any
	memo := map[string]string{}
	for _, r := range s.toolRegistry() {
		info := toolInfo(r, p, s.Doc().Tools[r.ID].Params)
		item := map[string]any{"id": r.ID, "kind": r.Kind, "enabled": r.Enabled, "chat": r.Chat, "commands": r.Commands,
			"can_chat": r.CanChat, "can_command": r.CanCommand, "implemented": r.Implemented, "limits": r.Limits, "used_today": used[r.ID], "description": info.Description}
		if r.Reason != "" {
			item["reason"] = r.Reason
		}
		if len(r.Aliases) > 0 {
			item["aliases"] = r.Aliases
		}
		if len(r.Defaults) > 0 {
			item["defaults"] = r.Defaults
		}
		if r.ID == "images.generate" { // the pictures asked for in chat that are waiting or being drawn
			waiting, running := s.imageQueueState()
			item["queue"] = map[string]any{"waiting": waiting, "running": running}
		}
		if r.HTTP != nil {
			item["host"] = hostOnly(r.HTTP.URL)
		}
		if len(r.Inputs) > 0 {
			item["inputs"] = r.Inputs
		}
		if r.Uses != "" {
			item["uses"] = r.Uses
		}
		if len(r.Sample) > 0 {
			item["sample"] = r.Sample
		}
		if r.Role != "" {
			item["role"] = r.Role
		}
		if r.OptIn {
			item["opt_in"] = true
		}
		item["ready"], item["ready_reason"] = s.toolReady(r, memo)
		if len(r.Providers) > 0 {
			// the configured providers behind the tool, so the console shows them without knowing hosts
			var ps []string
			for _, st := range s.Registry().Statuses() {
				if slices.Contains(r.Providers, st.Kind) {
					ps = append(ps, st.ID)
				}
			}
			item["provider_kinds"], item["providers"], item["provider_ready"] = r.Providers, ps, s.toolProviderReady(r.Providers)
			if r.ID == "search.web" {
				item["provider_order"] = s.searchOrder()
			}
		}
		list = append(list, item)
	}
	var overrides []any
	for gid, g := range s.Doc().Guilds {
		for cid, ch := range g.Channels {
			if len(ch.Tools) > 0 {
				overrides = append(overrides, map[string]any{"guild_id": gid, "channel_id": cid, "tools": ch.Tools})
			}
		}
	}
	dm := s.Doc().DM
	return action.OK(req, map[string]any{"tools": list, "day": day, "channel_overrides": overrides,
		"dm":   map[string]any{"allow": dm.AllowMode(), "per_user_per_day": dm.UserDaily(), "per_day": dm.Daily(), "used_today": used["dm.chat"]},
		"note": "configure with 'tools configure' or config.yaml tools:; backup, config and credential Actions are never tools"})
}

// toolReady says whether a tool can answer now and, when it cannot, why: turned off (config,
// feature_flag, opt_in), not built, a provider key missing (no_key), or the model role it needs
// (needs_model, model_key_missing, or the memory index's own reason for memory.search). The tools
// pages show this one answer. The planner still sees a tool without keys: its "unconfigured" result
// lets the answer say so instead of making the weather up. memo keeps a role's answer for one listing.
func (s *Service) toolReady(r tools.Resolved, memo map[string]string) (bool, string) {
	switch {
	case !r.Enabled:
		if r.Reason == "" {
			return false, "config"
		}
		return false, r.Reason
	case !r.Implemented:
		return false, "not_implemented"
	case len(r.Providers) > 0 && s.practice && r.Defaults["fixture"] != nil:
		return true, "" // practice answers it from an offline sample (weather, disasters)
	case len(r.Providers) > 0 && !s.toolProviderReady(r.Providers):
		return false, s.practiceReason("no_key")
	case r.Role == "":
		return true, ""
	}
	why, seen := memo[r.Role]
	if !seen {
		if r.Role == "embed" {
			if st, detail := s.memoryDoctor(); st != "ready" {
				why = detail
			}
		} else if a, err := s.llmAdapter(r.Role); err != nil {
			why = "needs_model"
		} else if !a.Configured() {
			why = "model_key_missing"
		}
		if memo != nil {
			memo[r.Role] = why
		}
	}
	if why == "needs_model" || why == "model_key_missing" {
		why = s.practiceReason(why)
	}
	return why == "", why
}

// practiceReason: practice mode cannot store keys or pick models, so a missing one is not something
// the operator can fix there.
func (s *Service) practiceReason(why string) string {
	if s.practice {
		return "practice_unavailable"
	}
	return why
}

// toolsTest runs a tool for real (counts toward its limits) with --input JSON or its sample input.
func (s *Service) toolsTest(ctx context.Context, req action.Request) action.Result {
	in := inputMap(req)
	id := strVal(in, "id", "")
	r, ok := tools.Find(s.toolRegistry(), id)
	if !ok || id == "chat.respond" {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "unknown tool id (see tools list)", map[string]any{"id": id})
	}
	input := map[string]any{}
	if raw := strVal(in, "input", ""); raw != "" {
		if err := json.Unmarshal([]byte(raw), &input); err != nil {
			return action.Fail(req, errcode.InvalidInput, "cli.input.powershell", s.msg(req, "cli.input.powershell"), nil)
		}
	} else if b, ok := tools.Lookup(id); ok && b.Sample != nil {
		input = b.Sample
	} else if b.Writes || id == "memory.search" {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "this tool needs --input (it has no safe sample)", map[string]any{"id": id})
	}
	raw, _ := json.Marshal(input)
	start := time.Now()
	res := s.Execute(ctx, action.Request{Action: id, Input: raw, Locale: req.Locale, RequestID: req.RequestID})
	out := map[string]any{"id": id, "ok": res.OK, "latency_ms": time.Since(start).Milliseconds(), "input": input, "enabled": r.Enabled}
	if res.OK {
		out["output"] = res.Output
		if st, _ := res.Output["status"].(string); st != "" {
			out["status"] = st
		}
	} else if res.Error != nil {
		out["error"] = res.Error
	}
	return action.OK(req, out)
}

// toolsConfigure edits config `tools.<id>` (instance-wide) or, with --guild_id and --channel_id,
// switches the tool in one channel. Fields not given are left as they are; "default" clears one.
func (s *Service) toolsConfigure(req action.Request) action.Result {
	in := inputMap(req)
	allowed := map[string]bool{"id": true, "guild_id": true, "channel_id": true, "enabled": true, "chat": true, "commands": true,
		"aliases": true, "description": true, "route": true, "defaults": true, "per_day": true, "per_user_per_day": true,
		"per_channel_per_day": true, "cooldown_sec": true}
	for key := range in {
		if !allowed[key] {
			return action.Fail(req, errcode.InvalidInput, "generic.error", fmt.Sprintf("unknown tools.configure key %q", key), map[string]any{"key": key})
		}
	}
	id := strVal(in, "id", "")
	if id == "" {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "--id required (see tools list)", nil)
	}
	if _, known := tools.Find(s.toolRegistry(), id); !known {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "unknown tool id; define operator tools in config.yaml tools:", map[string]any{"id": id})
	}
	triState := func(key string) (*bool, bool, error) {
		v, set := in[key]
		if !set {
			return nil, false, nil
		}
		switch strings.ToLower(strings.TrimSpace(fmt.Sprint(v))) {
		case "true", "on", "1", "yes":
			t := true
			return &t, true, nil
		case "false", "off", "0", "no":
			f := false
			return &f, true, nil
		case "default", "":
			return nil, true, nil
		}
		return nil, false, fmt.Errorf("%s must be on, off or default", key)
	}
	guildID, channelID := strVal(in, "guild_id", ""), strVal(in, "channel_id", "")
	res := s.mutateConfig(req, func(d *config.Document) error {
		if channelID != "" {
			if guildID == "" {
				return fmt.Errorf("--guild_id required with --channel_id")
			}
			v, set, err := triState("enabled")
			if err != nil || !set {
				return fmt.Errorf("channel switch needs --enabled on|off|default")
			}
			ensureGuildChannelMaps(d, guildID)
			p := config.NormalizeChannelPolicy(d.Guilds[guildID].Channels[channelID])
			if p.Tools == nil {
				p.Tools = map[string]bool{}
			}
			if v == nil {
				delete(p.Tools, id)
			} else {
				p.Tools[id] = *v
			}
			if len(p.Tools) == 0 {
				p.Tools = nil
			}
			d.Guilds[guildID].Channels[channelID] = p
			return nil
		}
		if d.Tools == nil {
			d.Tools = map[string]tools.Settings{}
		}
		t := d.Tools[id]
		for key, dst := range map[string]**bool{"enabled": &t.Enabled, "chat": &t.Chat, "commands": &t.Commands} {
			v, set, err := triState(key)
			if err != nil {
				return err
			}
			if set {
				*dst = v
			}
		}
		if r, ok := tools.Find(s.toolRegistry(), id); ok {
			if t.Chat != nil && *t.Chat && !r.CanChat {
				return fmt.Errorf("%s is not offered in conversation; it runs as a command only", id)
			}
			if t.Commands != nil && *t.Commands && !r.CanCommand {
				return fmt.Errorf("%s has no Discord command", id)
			}
		}
		if _, set := in["aliases"]; set {
			t.Aliases = strSlice(in, "aliases")
		}
		if _, set := in["description"]; set {
			t.Description = strVal(in, "description", "")
		}
		if _, set := in["route"]; set {
			t.Route = strVal(in, "route", "")
		}
		if raw := strVal(in, "defaults", ""); raw != "" {
			var m map[string]any
			if err := json.Unmarshal([]byte(raw), &m); err != nil {
				return fmt.Errorf("defaults must be a JSON object")
			}
			if len(m) == 0 {
				m = nil
			}
			t.Defaults = m
		}
		for key, dst := range map[string]*int{"per_day": &t.Limits.PerDay, "per_user_per_day": &t.Limits.PerUserPerDay,
			"per_channel_per_day": &t.Limits.PerChannelPerDay, "cooldown_sec": &t.Limits.CooldownSec} {
			if _, set := in[key]; set {
				*dst = intVal(in, key, 0)
			}
		}
		if err := tools.Validate(id, t); err != nil {
			return err
		}
		if isZeroSettings(t) {
			delete(d.Tools, id)
		} else {
			d.Tools[id] = t
		}
		if len(d.Tools) == 0 {
			d.Tools = nil
		}
		return nil
	})
	if !res.OK {
		return res
	}
	r, _ := tools.Find(s.toolRegistry(), id)
	res.Output["tool"] = map[string]any{"id": r.ID, "enabled": r.Enabled, "chat": r.Chat, "commands": r.Commands,
		"can_chat": r.CanChat, "can_command": r.CanCommand, "aliases": r.Aliases, "limits": r.Limits, "defaults": r.Defaults}
	return res
}

func isZeroSettings(t tools.Settings) bool {
	return t.Enabled == nil && t.Chat == nil && t.Commands == nil && len(t.Aliases) == 0 && t.Description == "" && t.Route == "" &&
		len(t.Defaults) == 0 && len(t.Params) == 0 && t.Limits.Zero() && t.HTTP == nil
}

// ToolCommands are the Discord prefix words per tool for the connector: configured aliases replace
// a built-in command's defaults; operator tools with aliases become commands whose text fills their
// first parameter. Tools whose commands are off are absent.
type ToolCommand struct {
	Tool       string
	Aliases    []string
	Custom     bool
	FirstParam string
}

func (s *Service) ToolCommands() []ToolCommand {
	var out []ToolCommand
	for _, r := range s.toolRegistry() {
		if !r.Commands {
			continue
		}
		c := ToolCommand{Tool: r.ID, Aliases: r.Aliases, Custom: r.Kind == "http"}
		if c.Custom {
			if ps := s.Doc().Tools[r.ID].Params; len(ps) > 0 {
				c.FirstParam = ps[0].Name
			}
			if len(c.Aliases) == 0 {
				continue
			}
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Tool < out[j].Tool })
	return out
}

// ToolCommandEnabled reports whether a Discord command for tool may be registered/used.
func (s *Service) ToolCommandEnabled(tool string) bool {
	r, ok := tools.Find(s.toolRegistry(), tool)
	return !ok || r.Commands
}
