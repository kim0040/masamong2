package application

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"

	"masamong/internal/action"
	"masamong/internal/chat"
	"masamong/internal/errcode"
	"masamong/internal/lang"
	"masamong/internal/providers/websearch"
	"masamong/internal/tools"
)

// The tool rules that need no I/O: the channel key of an invoker, scrubbing a credential from a response,
// filling and reading tool URLs and results, the fees a call may pay, and the planner's view of a tool and
// of its result.

func channelKeyOf(inv *action.Invoker) string {
	if inv == nil || inv.ChannelID == "" {
		return ""
	}
	return "discord:" + inv.GuildID + ":" + inv.ChannelID
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

// toolPays marks the tools that spend money of their own in a chat turn (Guard.Spend): a model call
// (Uses "model": a picture, a summary, a fortune), a paid web search, and a quote that falls back to
// one. Under a money cap the turn's later steps wait for them (chat runSteps), unless the tool's fee has a
// bound (tools.Builtin.MaxFeeUSD), which the call holds instead.
func toolPays(r tools.Resolved) bool {
	return r.Uses == "model" || r.ID == "search.web" || r.ID == "finance.quote"
}

// searchFees is what bounds the fee of one search.web call: the services it may ask (searchOrder, by id
// and kind), first to last, as the configuration names them when the turn's tools are offered.
type searchFees struct{ ids, kinds []string }

// bound is the most one search with these parameters pays: the sum, over every service it may ask, of
// what one search at the call's depth costs there (websearch.SearchFee). A search asks the next service
// when one finds nothing, and a service may charge for finding nothing, so each service it may reach
// counts. A depth the search does not take is the standard one, as search.web reads it; a provider the
// call names (and the order has) is the only service asked.
func (f searchFees) bound(params map[string]any) float64 {
	depth, _ := params["depth"].(string)
	depth = strings.ToLower(strings.TrimSpace(depth))
	if depth != websearch.DepthQuick && depth != websearch.DepthDeep {
		depth = websearch.DepthStandard
	}
	extra := 0
	for _, q := range strSlice(params, "queries") {
		if strings.TrimSpace(q) != "" {
			extra++
		}
	}
	only, _ := params["provider"].(string)
	if !slices.Contains(f.ids, only) {
		only = ""
	}
	sum := 0.0
	for i, kind := range f.kinds {
		if only == "" || f.ids[i] == only {
			sum += websearch.SearchFee(kind, depth, extra)
		}
	}
	return sum
}

// most is the dearest call's bound: a deep search with every reworded query the services run.
func (f searchFees) most() float64 {
	queries := make([]any, websearch.MaxExtra)
	for i := range queries {
		queries[i] = "q"
	}
	return f.bound(map[string]any{"depth": websearch.DepthDeep, "queries": queries})
}

// quoteBound is the most finance.quote pays: the standard search its fallback makes (quoteBySearch).
func (f searchFees) quoteBound(map[string]any) float64 { return f.bound(nil) }

// withFeeBounds gives a chat turn's search.web and finance.quote the bound of what they may pay with this
// configuration's search order. One that can pay nothing here (no search service, or only free ones) does
// not pay: it neither holds money nor makes the steps after it wait.
func (f searchFees) withFeeBounds(info chat.ToolInfo) chat.ToolInfo {
	if !info.Pays {
		return info
	}
	var bound func(map[string]any) float64
	var most float64
	switch info.ID {
	case "search.web":
		bound, most = f.bound, f.most()
	case "finance.quote":
		bound, most = f.quoteBound, f.quoteBound(nil)
	default:
		return info
	}
	if most <= 0 {
		info.Pays, info.MaxFeeUSD, info.FeeBound = false, 0, nil
		return info
	}
	info.MaxFeeUSD, info.FeeBound = most, bound
	return info
}

// toolInfo renders one tool for the planner in the voice's language.
func toolInfo(r tools.Resolved, p *lang.Pack, params []tools.Param) chat.ToolInfo {
	info := chat.ToolInfo{ID: r.ID, Description: r.Description, Route: r.Route, Required: r.Required, Writes: r.Writes, Once: r.Once, Final: r.Final, Web: r.WebContent,
		Pays: toolPays(r), Parallel: r.Parallel}
	if info.Pays {
		info.MaxFeeUSD = r.MaxFeeUSD // a bound on a tool that pays nothing would only hold money for nothing
	}
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
		case "list":
			cp.Type = "array"
			if in.Max > 0 {
				hi := in.Max
				cp.Max = &hi
			}
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

// withSearchQuery gives a web search's result the query it was asked when the result does not carry it (a
// failure has no output), so a failed search can be matched with a later one for the same words
// (toolFailureToReplay).
func withSearchQuery(out chat.ToolResult, tool string, params map[string]any) chat.ToolResult {
	if tool != "search.web" {
		return out
	}
	q, _ := params["query"].(string)
	if strings.TrimSpace(q) == "" {
		return out
	}
	if have, _ := out.Data["query"].(string); have != "" {
		return out
	}
	data := make(map[string]any, len(out.Data)+1)
	for k, v := range out.Data {
		data[k] = v
	}
	data["query"] = q
	out.Data = data
	return out
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

func isZeroSettings(t tools.Settings) bool {
	return t.Enabled == nil && t.Chat == nil && t.Commands == nil && len(t.Aliases) == 0 && t.Description == "" && t.Route == "" &&
		len(t.Defaults) == 0 && len(t.Params) == 0 && t.Limits.Zero() && t.HTTP == nil
}

// toolSurfaceAllows is toolAllowedHere's decision once the channel's own switch for the tool is read
// (override, has): a channel that turned the tool off refuses it, the planner and tasks use chat tools (or
// one the channel turned on), Discord commands only command tools.
func toolSurfaceAllows(r tools.Resolved, via string, override, has bool) (bool, string) {
	if has && !override {
		return false, "tool.channel_off"
	}
	switch via {
	case viaChat, viaTask:
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

// fillToolDefaults sets each default the input does not set. A defaulted region is marked, so the
// weather says it assumed the place.
func fillToolDefaults(in map[string]any, defaults map[string]any) {
	for k, v := range defaults {
		if _, set := in[k]; !set {
			in[k] = v
			if k == "region" {
				in["region_source"] = "default" // the weather says it assumed the place
			}
		}
	}
}

// missingInputs are the required inputs that are not set (or empty), in the order required lists them.
func missingInputs(in map[string]any, required []string) []string {
	var missing []string
	for _, p := range required {
		if strVal(in, p, "") == "" {
			missing = append(missing, p)
		}
	}
	return missing
}
