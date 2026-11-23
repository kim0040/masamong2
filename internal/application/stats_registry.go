package application

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"masamong/internal/catalog"
	"masamong/internal/config"
	"masamong/internal/errcode"
	"masamong/internal/features/participation"
	"masamong/internal/jobs"
	"masamong/internal/providers/openai"
	"masamong/internal/storage"
	"masamong/internal/tools"
)

// Usage statistics (v1): daily totals of what this install did, kept in its own database and never sent
// anywhere. An operator may export a file of weekly totals (stats.export) and share it by hand.
//
// This file is the contract every statistics part writes through:
//   - the registry: every metric, whether it is a counter or derived, its group, its dimensions in their
//     canonical order with the closed vocabulary each one exports, and its export policy;
//   - the tables: stats_daily (one row per day, metric and dimension values; additive numbers only) and
//     stats_days (which (group, day) the rollup has finished; a finished day is never rewritten unless
//     forced, so aggregates whose source rows were pruned since are not replaced by nothing);
//   - the two writes: statsAdd for counters (additive upsert, never recomputed) and statsReplaceDay for
//     derived groups (the rollup replaces one group's day and marks it finished in the same transaction).
//
// Nothing here may hold message text, names or Discord IDs. A dimension value is either one of a closed
// list (Values) or, for an Open dimension, a name that comes from code or the operator's config (a model
// id, an operator's HTTP tool id, a role label), which stays on this install: the export folds it into
// the closed list. Free text from members, models or providers (a judge's reason, an error message)
// never becomes a dimension value.

// statsKind says how a metric is written.
type statsKind string

const (
	// statsCounter is incremented as events happen (statsAdd: n = n + ?). It has no source to recompute
	// from, so nothing ever replaces or deletes its rows.
	statsCounter statsKind = "counter"
	// statsDerived is computed by the daily rollup from rows that already exist, one group and day at a
	// time (statsReplaceDay).
	statsDerived statsKind = "derived"
)

// statsBucket is how a metric's weekly totals are coarsened in the export (review H5 rule 3).
type statsBucket string

const (
	statsBucketNone  statsBucket = ""      // the count as it is
	statsBucketSig1  statsBucket = "sig1"  // rounded to one significant digit (statsRoundSig1)
	statsBucketRange statsBucket = "range" // 1, 2-5, 6-20, 21-100, 100+ (statsRangeLabel)
)

// statsCombine is how a metric's days add up into a longer period.
type statsCombine string

const (
	statsSum statsCombine = ""    // n and vals are summed (every counter and most derived metrics)
	statsMax statsCombine = "max" // n is a distinct count per day (active rooms, people): the period takes the largest day
)

// statsExportPolicy is what stats.export does with a metric (review H4, H5).
type statsExportPolicy struct {
	// Exported puts the metric in the export; false keeps it on this install (the console only).
	Exported bool
	// MemberFacing marks what members do (actions from Discord, tools, feedback, reminders, chiming in):
	// a week with fewer than statsExportMinEvents events of such a row is folded into "other" (H5 rule 5).
	// Operational metrics (model errors, latency, cost, jobs) are exported as counted.
	MemberFacing bool
	// MemberWhen narrows MemberFacing to rows whose dimension holds one of the listed values (actions:
	// only those from Discord are a member's act).
	MemberWhen map[string][]string
	// SkipWhen leaves rows with one of the listed dimension values out of the export (actions: the
	// model's own tool calls are counted in the tool metrics only; coordinator decision 2026-11-22).
	SkipWhen map[string][]string
	// Bucket coarsens the weekly n.
	Bucket statsBucket
}

// statsExportMinEvents is the H5 rule 5 threshold: member-facing rows with fewer events in an ISO week
// fold into "other" (coordinator decision 2026-11-22).
const statsExportMinEvents = 5

// statsOther is the value unknown or rare values fold into.
const statsOther = "other"

// statsDim is one dimension of a metric.
type statsDim struct {
	Key string
	// Values is the closed vocabulary: every exported value of this dimension is one of these. It
	// always holds statsOther.
	Values []string
	// Open stores the source's own name (cleaned by statsOpenValue) so the console can show it; the
	// export folds it. Only for names from code or config, never from member, model or provider text.
	Open bool
	// Fold maps a source value (closed dimensions, when written) or a stored value (open ones, when
	// exported) into Values. It never returns anything outside Values.
	Fold func(string) string
}

// store is the value written to stats_daily for a source value.
func (d statsDim) store(raw string) string {
	if d.Open {
		return statsOpenValue(raw)
	}
	return d.Fold(raw)
}

// export is the value a stored value has in the export: always one of Values.
func (d statsDim) export(stored string) string {
	if v := d.Fold(stored); slices.Contains(d.Values, v) {
		return v
	}
	return statsOther
}

// statsMetric declares one metric. An undeclared metric, dimension or value key cannot be written.
type statsMetric struct {
	Name  string // "<group>.<what>"
	Group string
	Kind  statsKind
	Doc   string     // what n counts
	Dims  []statsDim // canonical order: the dims column spells them in this order
	// Vals are the additive numbers a row may carry beside n (summed like n across days and installs).
	Vals []string
	// Hist says rows may carry "h", a latency histogram over statsHistBounds (review M1).
	Hist    bool
	Combine statsCombine
	Export  statsExportPolicy
}

// dim returns the metric's dimension key.
func (m *statsMetric) dim(key string) (statsDim, bool) {
	for _, d := range m.Dims {
		if d.Key == key {
			return d, true
		}
	}
	return statsDim{}, false
}

// statsHistBounds are the fixed upper bounds (milliseconds) of the latency histogram buckets; one more
// bucket past the last holds everything slower (+Inf). Fixed bounds make histograms of any day and any
// install add up bucket by bucket, which percentile columns cannot (review M1).
var statsHistBounds = []int64{100, 250, 500, 1000, 2000, 4000, 8000, 15000, 30000, 60000, 120000}

// statsHistLen is the length of a histogram: one bucket per bound plus +Inf.
var statsHistLen = len(statsHistBounds) + 1

// statsHistIndex is the bucket a latency falls in.
func statsHistIndex(ms int64) int {
	for i, b := range statsHistBounds {
		if ms <= b {
			return i
		}
	}
	return len(statsHistBounds)
}

// statsHistAdd counts one latency into h (allocated when nil).
func statsHistAdd(h []int64, ms int64) []int64 {
	if len(h) != statsHistLen {
		h = make([]int64, statsHistLen)
	}
	h[statsHistIndex(ms)]++
	return h
}

// statsHistMerge adds b into a bucket by bucket (a nil or wrong-length side counts as empty).
func statsHistMerge(a, b []int64) []int64 {
	if len(b) != statsHistLen {
		return a
	}
	if len(a) != statsHistLen {
		a = make([]int64, statsHistLen)
	}
	for i := range b {
		a[i] += b[i]
	}
	return a
}

// statsHistQuantile is the upper bound of the bucket holding quantile q (0..1), in milliseconds:
// an over-estimate by at most one bucket. -1 for an empty histogram; math.MaxInt64 when it falls past
// the last bound.
func statsHistQuantile(h []int64, q float64) int64 {
	var total int64
	for _, c := range h {
		total += c
	}
	if total == 0 || len(h) != statsHistLen {
		return -1
	}
	want := int64(math.Ceil(q * float64(total)))
	want = max(want, 1)
	var seen int64
	for i, c := range h {
		seen += c
		if seen >= want {
			if i < len(statsHistBounds) {
				return statsHistBounds[i]
			}
			return math.MaxInt64
		}
	}
	return math.MaxInt64
}

// statsRoundSig1 rounds n to one significant digit (1234 → 1000, 56 → 60).
func statsRoundSig1(n int64) int64 {
	if n <= 0 {
		return 0
	}
	p := int64(1)
	for n/p >= 10 {
		p *= 10
	}
	return (n + p/2) / p * p
}

// statsRangeLabel is the H5 rule 3 range of a count ("" for none).
func statsRangeLabel(n int64) string {
	switch {
	case n <= 0:
		return ""
	case n == 1:
		return "1"
	case n <= 5:
		return "2-5"
	case n <= 20:
		return "6-20"
	case n <= 100:
		return "21-100"
	}
	return "100+"
}

// ---------------------------------------------------------------------------
// dimension vocabularies

// closedFold folds a value into values (with statsOther), after an optional alias table.
func closedFold(values []string, alias map[string]string) func(string) string {
	return func(v string) string {
		v = strings.TrimSpace(v)
		if a, ok := alias[v]; ok {
			v = a
		}
		if slices.Contains(values, v) {
			return v
		}
		return statsOther
	}
}

// closedDim is a dimension whose stored and exported values are the same closed list.
func closedDim(key string, values []string, alias map[string]string) statsDim {
	vs := withOther(values)
	return statsDim{Key: key, Values: vs, Fold: closedFold(vs, alias)}
}

// openDim stores the source's name and exports it folded by fold (its outputs are added to Values).
func openDim(key string, values []string, fold func(string) string) statsDim {
	vs := withOther(values)
	return statsDim{Key: key, Values: vs, Open: true, Fold: func(v string) string {
		if f := fold(v); slices.Contains(vs, f) {
			return f
		}
		return statsOther
	}}
}

func withOther(values []string) []string {
	out := slices.Clone(values)
	if !slices.Contains(out, statsOther) {
		out = append(out, statsOther)
	}
	return out
}

// statsOpenValue cleans a name for an open dimension: trimmed, at most 64 characters, no control
// characters and none of the separators of the dims column ("" becomes "none").
func statsOpenValue(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "none"
	}
	var b strings.Builder
	n := 0
	for _, r := range raw {
		if n == 64 {
			break
		}
		if r == '|' || r == '=' || unicode.IsControl(r) || unicode.IsSpace(r) {
			r = '_'
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

// statsErrClasses are the errclass values, plus "none" for a call without an error.
var statsErrClasses = []string{"none", "timeout", "network", "rate_limited", "overloaded", "auth", "permission", "quota",
	"bad_request", "unsupported", "context_length", "not_found", "format", "empty", "canceled", "unknown"}

func classDim() statsDim { return closedDim("class", statsErrClasses, map[string]string{"": "none"}) }

// statsErrCodes are the errcode values, plus "ok" (an Action that succeeded) and "none" (no code).
var statsErrCodes = []string{"ok", "none", errcode.ConfigConflict, errcode.ConfigInvalid, errcode.ActionUnavailable, errcode.ActionUnknown,
	errcode.StaleRevision, errcode.ScopeMismatch, errcode.LeaseMismatch, errcode.JobNotFound, errcode.JobIdempotency, errcode.InvalidInput,
	errcode.NotSetup, errcode.ProviderFakeOnly, errcode.NoRecommendation, errcode.BackupVerifyFailed, errcode.PlanRejected,
	errcode.BudgetExceeded, errcode.PermissionDenied, errcode.FeatureDisabled, errcode.ToolLimited, errcode.IO,
	errcode.ProviderUnconfigured, errcode.ProviderUnavailable, errcode.ProviderRateLimited, errcode.ProviderRejected,
	errcode.ProviderFormat, errcode.ProviderError, errcode.DiscordNotConnected, errcode.Unauthorized}

func codeDim() statsDim { return closedDim("code", statsErrCodes, map[string]string{"": "none"}) }

// statsRoles are the model-call roles of the ledger: the config roles, the tool loop, the memory index's
// summary and the small planner-route classifiers (withLedgerRole), and the feature kinds of featureText.
var statsRoles = append(config.AllModelRoles(), ledgerRoleToolLoop, "chat", "memory_summary", "memory_explicit", "reference",
	"name_call", "follow", "thread_summary", "fortune", "participation", "style", "card")

// statsProviderFamilies are what a provider is exported as: the host family of an OpenAI-compatible
// provider, else its kind; an unknown provider is "custom" (review H4: the provider key is a name the
// operator typed).
var statsProviderFamilies = func() []string {
	out := []string{"none", "custom"}
	for _, a := range openai.APIs {
		out = append(out, string(a))
	}
	for _, k := range config.ProviderKinds {
		if k != config.KindOpenAI && !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	return out
}()

// statsProviderFamily is the family the rollup stores for the provider key a ledger row names.
func statsProviderFamily(doc config.Document, key string) string {
	if strings.TrimSpace(key) == "" {
		return "none"
	}
	p, ok := doc.Providers[key]
	if !ok {
		return "custom"
	}
	if p.Kind == config.KindOpenAI {
		return string(openai.ResolveAPI(p.API, p.BaseURL))
	}
	if slices.Contains(statsProviderFamilies, p.Kind) {
		return p.Kind
	}
	return "custom"
}

// statsModelVendors are the model vendors an unknown model id is exported under ("<vendor>/other").
var statsModelVendors = []string{"openai", "anthropic", "google", "deepseek", "qwen", "meta-llama", "mistralai", "x-ai",
	"voyageai", "cohere", "moonshotai", "z-ai", "minimax", "nvidia", "microsoft", "amazon", "practice"}

// statsModelPrefixes name the vendor of a model id written without one.
var statsModelPrefixes = [][2]string{{"gpt-", "openai"}, {"chatgpt-", "openai"}, {"o1", "openai"}, {"o3", "openai"},
	{"o4", "openai"}, {"text-embedding-3", "openai"}, {"dall-e", "openai"}, {"claude-", "anthropic"}, {"gemini-", "google"},
	{"gemma-", "google"}, {"imagen-", "google"}, {"text-embedding-004", "google"}, {"deepseek-", "deepseek"}, {"qwen", "qwen"},
	{"mistral-", "mistralai"}, {"codestral", "mistralai"}, {"grok-", "x-ai"}, {"voyage-", "voyageai"}, {"rerank-", "voyageai"}}

// statsKnownModels are exported by their own id: the models the default config uses and the practice
// models. Every other id is exported as its vendor's "other" or "custom" (a fine-tune or proxy id can
// carry an organization's name, review H4).
var statsKnownModels = func() []string {
	var out []string
	for _, m := range config.Default("", "").Models {
		if m.Model != "" && !slices.Contains(out, strings.ToLower(m.Model)) {
			out = append(out, strings.ToLower(m.Model))
		}
	}
	return append(out, "practice/chat", "practice/embed", "practice/rerank")
}()

func foldModel(id string) string {
	id = strings.ToLower(strings.TrimSpace(id))
	if id == "" || id == "none" {
		return "none"
	}
	if id == statsOther || slices.Contains(statsKnownModels, id) {
		return id
	}
	vendor := ""
	if v, _, ok := strings.Cut(id, "/"); ok {
		if slices.Contains(statsModelVendors, v) {
			vendor = v
		}
	} else {
		for _, p := range statsModelPrefixes {
			if strings.HasPrefix(id, p[0]) {
				vendor = p[1]
				break
			}
		}
	}
	if vendor == "" || strings.HasPrefix(id, "ft:") || strings.Contains(id, ":ft") {
		return "custom"
	}
	return vendor + "/other"
}

func modelValues() []string {
	out := append([]string{"none", "custom"}, statsKnownModels...)
	for _, v := range statsModelVendors {
		out = append(out, v+"/other")
	}
	return out
}

// statsBuiltinTools are the built-in tool ids; an operator's HTTP tool is exported as custom_http.
var statsBuiltinTools = func() []string {
	var out []string
	for _, b := range tools.Builtins() {
		out = append(out, b.ID)
	}
	return out
}()

func foldTool(id string) string {
	if slices.Contains(statsBuiltinTools, id) {
		return id
	}
	if strings.TrimSpace(id) == "" || id == "none" || id == statsOther {
		return statsOther
	}
	return "custom_http"
}

// statsActions are what the action counter may store: catalog ids, custom_http (an operator's HTTP tool)
// and unknown (any other id a caller sent; review M7).
var statsActions = func() []string {
	var out []string
	for _, d := range catalog.All() {
		out = append(out, d.ID)
	}
	return append(out, "custom_http", "unknown")
}()

// statsActionID is the action dimension value for a request's Action id (the counter calls it).
func statsActionID(id string, operatorTool bool) string {
	if _, ok := catalog.Lookup(id); ok {
		return id
	}
	if operatorTool {
		return "custom_http"
	}
	return "unknown"
}

// statsVias are where an Action came from: the audit's values (admin.go audit), plus the console's polled
// reads (gui_poll), the model's tool calls (chat), the terminal UI, the simulator and practice mode.
var statsVias = []string{"discord", "gui", "gui_poll", "cli", "api", "system", "tray", "tui", "chat", "sim", "practice"}

// statsFinish buckets a provider's finish_reason (stored as received: review H4).
func statsFinish(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "none":
		return "none"
	case "stop", "end_turn", "stop_sequence", "eos":
		return "stop"
	case "length", "max_tokens", "max_output_tokens":
		return "length"
	case "tool_calls", "tool_use", "function_call":
		return "tool_calls"
	case "content_filter", "safety", "refusal", "recitation", "blocklist", "prohibited_content":
		return "content_filter"
	}
	return statsOther
}

// statsToolStatuses are the statuses tool results report (chat.ToolResult.Status and the ledger's own).
var statsToolStatuses = []string{"ok", "error", "failed", "no_match", "ready", "unavailable", "unconfigured", "cancelled",
	"dry_run", "needs_input", "none", "queued", "refused", "scheduled", "skipped"}

// statsReminderStatuses are the reminder rows' statuses.
var statsReminderStatuses = []string{"proposed", "scheduled", "sending", "sent", "paused", "cancelled", "missed", "failed", "unknown"}

// statsLateness buckets how late a reminder went out (sent time minus due_at); none = not sent.
var statsLateness = []string{"none", "on_time", "late_5m", "late_1h", "late_day", "later"}

// statsLatenessOf is the lateness bucket of a reminder sent late by d (negative = early, on time).
func statsLatenessOf(d time.Duration) string {
	switch {
	case d <= time.Minute:
		return "on_time"
	case d <= 5*time.Minute:
		return "late_5m"
	case d <= time.Hour:
		return "late_1h"
	case d <= 24*time.Hour:
		return "late_day"
	}
	return "later"
}

// statsJobStatuses are the job machine's statuses.
var statsJobStatuses = []string{string(jobs.Pending), string(jobs.Leased), string(jobs.Running), string(jobs.Paused),
	string(jobs.RetryWait), string(jobs.Succeeded), string(jobs.Failed), string(jobs.Cancelled)}

func foldJobType(t string) string {
	if t == jobPrepare {
		return t
	}
	if _, ok := catalog.Lookup(t); ok {
		return t
	}
	return statsOther
}

// statsConvDecisions are conversation_log decisions; skip_* and would_* fold to skip and would.
var statsConvDecisions = []string{"answer", "auto", "classify", "declined", "disliked", "offer", "opened", "quiet", "unparsed", "skip", "would"}

func foldConvDecision(v string) string {
	switch {
	case strings.HasPrefix(v, "skip_"):
		return "skip"
	case strings.HasPrefix(v, "would_"):
		return "would"
	case slices.Contains(statsConvDecisions, v):
		return v
	}
	return statsOther
}

func hourValues() []string {
	out := make([]string, 24)
	for h := range out {
		out[h] = fmt.Sprintf("%02d", h)
	}
	return out
}

func foldHour(v string) string {
	h, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || h < 0 || h > 23 {
		return statsOther
	}
	return fmt.Sprintf("%02d", h)
}

// ---------------------------------------------------------------------------
// the registry

// Groups. A derived group is replaced and finished per day as a whole by its own rollup; the counter
// group is only ever added to.
const (
	statsGroupTurns         = "turns"         // turn_log (lab rows excluded, review L3)
	statsGroupLLM           = "llm"           // llm_calls (job aggregate rows: review M3)
	statsGroupTools         = "tools"         // tool_calls
	statsGroupDeliveries    = "deliveries"    // delivery_ledger
	statsGroupFeedback      = "feedback"      // turn_feedback
	statsGroupParticipation = "participation" // participation_log and conversation_log
	statsGroupReminders     = "reminders"     // reminders
	statsGroupJobs          = "jobs"          // jobs
	statsGroupActivity      = "activity"      // activity_counts (never memory_observations, review M9)
	statsGroupActions       = "actions"       // the in-memory action counter (counter)
)

var statsRegistry = func() []statsMetric {
	role := openDim("role", statsRoles, func(v string) string { return v })
	model := openDim("model", modelValues(), foldModel)
	provider := closedDim("provider", statsProviderFamilies, map[string]string{"": "none"})
	tool := openDim("tool", append(slices.Clone(statsBuiltinTools), "custom_http"), foldTool)
	operational := statsExportPolicy{Exported: true}
	member := statsExportPolicy{Exported: true, MemberFacing: true}
	return []statsMetric{
		{Name: "turns.count", Group: statsGroupTurns, Kind: statsDerived, Doc: "turns by outcome; the histogram and latency_ms cover answered turns",
			Dims: []statsDim{closedDim("outcome", []string{"answered", "paused", "disabled", "limited", "budget", "not_addressed",
				"no_vision", "reacted", "canceled", "error", "silent"}, nil)},
			Vals: []string{"cost_usd", "hits", "latency_ms"}, Hist: true, Export: operational},
		{Name: "turns.hour", Group: statsGroupTurns, Kind: statsDerived, Doc: "turns by local hour (console only, review H5 rule 2)",
			Dims: []statsDim{{Key: "hour", Values: withOther(hourValues()), Fold: foldHour}}},

		{Name: "llm.calls", Group: statsGroupLLM, Kind: statsDerived, Doc: "model calls (SUM(call_count)); the histogram only from rows with call_count = 1",
			Dims: []statsDim{role, model, provider},
			Vals: []string{"prompt_tokens", "completion_tokens", "cached_tokens", "cost_usd", "errors", "fallback", "zero_cost", "job_rows"},
			Hist: true, Export: operational},
		{Name: "llm.errors", Group: statsGroupLLM, Kind: statsDerived, Doc: "failed model calls by class (a job aggregate row counts once: at least one failed)",
			Dims: []statsDim{role, classDim()}, Export: operational},
		{Name: "llm.finish", Group: statsGroupLLM, Kind: statsDerived, Doc: "model calls by finish reason bucket",
			Dims:   []statsDim{role, {Key: "finish", Values: []string{"none", "stop", "length", "tool_calls", "content_filter", statsOther}, Fold: statsFinish}},
			Export: operational},

		{Name: "tools.calls", Group: statsGroupTools, Kind: statsDerived, Doc: "tool executions",
			Dims: []statsDim{tool, closedDim("status", statsToolStatuses, map[string]string{"": "none"}), classDim(), provider},
			Vals: []string{"cost_usd", "result_chars", "latency_ms"}, Hist: true, Export: member},
		{Name: "tools.guard", Group: statsGroupTools, Kind: statsDerived, Doc: "tool steps by guard verdict",
			Dims: []statsDim{tool, closedDim("guard", []string{"allowed", "skipped"}, map[string]string{"": "allowed"})}, Export: member},

		{Name: "deliveries.count", Group: statsGroupDeliveries, Kind: statsDerived, Doc: "reply deliveries by failure class",
			Dims: []statsDim{classDim()}, Vals: []string{"parts", "sent", "retries", "reference_dropped", "incomplete"}, Export: operational},

		{Name: "feedback.ratings", Group: statsGroupFeedback, Kind: statsDerived, Doc: "explicit ratings and operator marks",
			Dims: []statsDim{
				{Key: "source", Values: []string{"menu", "reaction", "operator", statsOther}, Fold: func(v string) string {
					switch {
					case v == "menu", v == "operator", v == "reaction":
						return v
					case strings.HasPrefix(v, "reaction:"):
						return "reaction"
					}
					return statsOther
				}},
				closedDim("rating", []string{"good", "bad", "stale", "neutral"}, map[string]string{"1": "good", "-1": "bad", "-2": "stale", "0": "neutral"}),
				closedDim("reason", []string{"none", "wrong", "forgot", "tone", "too_long"}, map[string]string{"": "none"}),
			}, Export: member},
		{Name: "feedback.implicit", Group: statsGroupFeedback, Kind: statsDerived, Doc: "implicit reactions by kind",
			Dims: []statsDim{closedDim("kind", ImplicitKinds, nil)}, Export: member},

		{Name: "participation.decisions", Group: statsGroupParticipation, Kind: statsDerived, Doc: "chime-in candidates by mode, kind and state",
			Dims: []statsDim{
				closedDim("mode", config.ParticipationModes, nil),
				closedDim("kind", []string{participation.Plan, participation.Question, "bot_mentioned"}, nil),
				closedDim("state", []string{partSilent, partShadow, partSpoken, partCancelled, partCollected, partError}, nil),
			}, Vals: []string{"cost_usd"}, Export: member},
		{Name: "participation.conversation", Group: statsGroupParticipation, Kind: statsDerived, Doc: "conversation decisions (name calls, follow-ups, threads, banter) and what followed",
			Dims: []statsDim{
				closedDim("kind", []string{convNameCall, convFollow, convThread, convBackoff, convBanter}, nil),
				openDim("decision", statsConvDecisions, foldConvDecision),
				closedDim("outcome", []string{"none", "retagged", "retagged_after_answer", threadOutcomeNotShown}, map[string]string{"": "none"}),
			}, Vals: []string{"cost_usd"}, Export: member},

		{Name: "reminders.status", Group: statsGroupReminders, Kind: statsDerived, Doc: "reminders and scheduled tasks by status and lateness",
			Dims: []statsDim{
				closedDim("kind", []string{"reminder", "task"}, map[string]string{"": "reminder"}),
				closedDim("status", statsReminderStatuses, nil),
				closedDim("lateness", statsLateness, map[string]string{"": "none"}),
			}, Vals: []string{"attempts"}, Export: member},

		{Name: "jobs.runs", Group: statsGroupJobs, Kind: statsDerived, Doc: "background jobs by type, status and error code",
			Dims:   []statsDim{openDim("type", []string{jobPrepare}, foldJobType), closedDim("status", statsJobStatuses, nil), codeDim()},
			Export: operational},

		{Name: "activity.messages", Group: statsGroupActivity, Kind: statsDerived, Doc: "messages captured from Discord (consenting members)",
			Export: statsExportPolicy{Exported: true, Bucket: statsBucketSig1}},
		{Name: "activity.rooms", Group: statsGroupActivity, Kind: statsDerived, Doc: "rooms with at least one captured message that day",
			Combine: statsMax, Export: statsExportPolicy{Exported: true, Bucket: statsBucketRange}},
		{Name: "activity.people", Group: statsGroupActivity, Kind: statsDerived, Doc: "people with at least one captured message that day",
			Combine: statsMax, Export: statsExportPolicy{Exported: true, Bucket: statsBucketRange}},

		{Name: "actions.calls", Group: statsGroupActions, Kind: statsCounter, Doc: "Actions run through Service.Execute, by where they came from and their result code",
			Dims: []statsDim{
				closedDim("action", statsActions, nil),
				closedDim("via", statsVias, nil),
				codeDim(),
			},
			Export: statsExportPolicy{Exported: true, MemberFacing: true, MemberWhen: map[string][]string{"via": {"discord"}},
				SkipWhen: map[string][]string{"via": {"chat"}}}},
	}
}()

var statsByName = func() map[string]*statsMetric {
	m := make(map[string]*statsMetric, len(statsRegistry))
	for i := range statsRegistry {
		m[statsRegistry[i].Name] = &statsRegistry[i]
	}
	return m
}()

// statsLookup finds a declared metric.
func statsLookup(name string) (*statsMetric, bool) {
	m, ok := statsByName[name]
	return m, ok
}

// statsLookup finds a declared metric: one a test declared for this service, else the registry's.
func (s *Service) statsLookup(name string) (*statsMetric, bool) {
	if m, ok := s.hooks.statsMetrics.Load(name); ok {
		return m.(*statsMetric), true
	}
	return statsLookup(name)
}

// statsGroupMetrics lists the metrics of a group in registry order.
func statsGroupMetrics(group string) []*statsMetric {
	var out []*statsMetric
	for i := range statsRegistry {
		if statsRegistry[i].Group == group {
			out = append(out, &statsRegistry[i])
		}
	}
	return out
}

// statsDerivedGroups lists the derived groups in registry order (the rollup's work list).
func statsDerivedGroups() []string {
	var out []string
	for _, m := range statsRegistry {
		if m.Kind == statsDerived && !slices.Contains(out, m.Group) {
			out = append(out, m.Group)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// rows and encoding

// statsRow is one row to write. Dims holds source values by key (every declared key, no other);
// they are stored through the registry, so callers pass what the source table says.
type statsRow struct {
	Day    string // YYYY-MM-DD, the local day (statsReplaceDay fills it when empty)
	Metric string
	Dims   map[string]string
	N      int64
	Vals   map[string]float64
	Hist   []int64 // statsHistLen buckets, or nil
}

var (
	errStatsOff        = errors.New("statistics are off (stats.enabled: false, or practice mode)")
	errStatsUndeclared = errors.New("statistics: undeclared metric, dimension or value")
	errStatsKind       = errors.New("statistics: wrong write for this metric kind")
	errStatsFinalized  = errors.New("statistics: day already finished for this group")
)

// statsEncodeDims is the canonical dims column: "k=v|k=v" in the metric's declared order, with each
// value stored through its dimension. Every declared key must be present and no other.
func statsEncodeDims(m *statsMetric, dims map[string]string) (string, error) {
	for k := range dims {
		if _, ok := m.dim(k); !ok {
			return "", fmt.Errorf("%w: %s has no dimension %q", errStatsUndeclared, m.Name, k)
		}
	}
	parts := make([]string, 0, len(m.Dims))
	for _, d := range m.Dims {
		raw, ok := dims[d.Key]
		if !ok {
			return "", fmt.Errorf("%w: %s needs dimension %q", errStatsUndeclared, m.Name, d.Key)
		}
		parts = append(parts, d.Key+"="+d.store(raw))
	}
	return strings.Join(parts, "|"), nil
}

// statsDecodeDims reads a dims column back into key → stored value.
func statsDecodeDims(s string) map[string]string {
	out := map[string]string{}
	if s == "" {
		return out
	}
	for _, p := range strings.Split(s, "|") {
		if k, v, ok := strings.Cut(p, "="); ok {
			out[k] = v
		}
	}
	return out
}

// statsExportDims maps stored dims to their export vocabulary (always members of each Values).
func statsExportDims(m *statsMetric, stored map[string]string) map[string]string {
	out := make(map[string]string, len(m.Dims))
	for _, d := range m.Dims {
		out[d.Key] = d.export(stored[d.Key])
	}
	return out
}

// statsEncodeVals is the vals column: a JSON object of the non-zero declared numbers plus "h".
func statsEncodeVals(m *statsMetric, vals map[string]float64, hist []int64) (string, error) {
	obj := map[string]any{}
	for k, v := range vals {
		if !slices.Contains(m.Vals, k) {
			return "", fmt.Errorf("%w: %s has no value %q", errStatsUndeclared, m.Name, k)
		}
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return "", fmt.Errorf("statistics: %s.%s is not a number", m.Name, k)
		}
		if v != 0 {
			obj[k] = v
		}
	}
	if hist != nil {
		if !m.Hist || len(hist) != statsHistLen {
			return "", fmt.Errorf("%w: %s takes no histogram of %d buckets", errStatsUndeclared, m.Name, len(hist))
		}
		var total int64
		for _, c := range hist {
			total += c
		}
		if total > 0 {
			obj["h"] = hist
		}
	}
	raw, err := json.Marshal(obj) // map keys come out sorted
	return string(raw), err
}

// statsDecodeVals reads a vals column.
func statsDecodeVals(raw string) (map[string]float64, []int64, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(cmpOr(raw, "{}")), &obj); err != nil {
		return nil, nil, err
	}
	vals := map[string]float64{}
	var hist []int64
	for k, v := range obj {
		if k == "h" {
			if err := json.Unmarshal(v, &hist); err != nil {
				return nil, nil, err
			}
			continue
		}
		var f float64
		if err := json.Unmarshal(v, &f); err != nil {
			return nil, nil, err
		}
		vals[k] = f
	}
	return vals, hist, nil
}

func cmpOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// statsKey is a row's primary key after encoding.
type statsKey struct{ day, metric, dims string }

type statsEncoded struct {
	key  statsKey
	n    int64
	vals map[string]float64
	hist []int64
}

// statsPrepare checks rows against the registry and merges rows with the same key (additively).
func (s *Service) statsPrepare(rows []statsRow, kind statsKind) ([]statsEncoded, error) {
	idx := map[statsKey]int{}
	var out []statsEncoded
	for _, r := range rows {
		m, ok := s.statsLookup(r.Metric)
		if !ok {
			return nil, fmt.Errorf("%w: metric %q", errStatsUndeclared, r.Metric)
		}
		if m.Kind != kind {
			return nil, fmt.Errorf("%w: %s is a %s", errStatsKind, m.Name, m.Kind)
		}
		if _, err := time.Parse("2006-01-02", r.Day); err != nil {
			return nil, fmt.Errorf("statistics: day %q: %w", r.Day, err)
		}
		if r.N < 0 {
			return nil, fmt.Errorf("statistics: %s: negative n", m.Name)
		}
		dims, err := statsEncodeDims(m, r.Dims)
		if err != nil {
			return nil, err
		}
		if _, err := statsEncodeVals(m, r.Vals, r.Hist); err != nil {
			return nil, err
		}
		k := statsKey{r.Day, m.Name, dims}
		if i, ok := idx[k]; ok {
			out[i].n += r.N
			out[i].vals = statsMergeVals(out[i].vals, r.Vals)
			out[i].hist = statsHistMerge(out[i].hist, r.Hist)
			continue
		}
		idx[k] = len(out)
		out = append(out, statsEncoded{key: k, n: r.N, vals: statsMergeVals(nil, r.Vals), hist: statsHistMerge(nil, r.Hist)})
	}
	return out, nil
}

// statsMergeVals adds b into a.
func statsMergeVals(a, b map[string]float64) map[string]float64 {
	if a == nil {
		a = map[string]float64{}
	}
	for k, v := range b {
		a[k] += v
	}
	return a
}

// ---------------------------------------------------------------------------
// tables

var statsSQLite = []string{
	`CREATE TABLE IF NOT EXISTS stats_daily (
		day TEXT NOT NULL,
		metric TEXT NOT NULL,
		dims TEXT NOT NULL DEFAULT '',
		n INTEGER NOT NULL DEFAULT 0,
		vals TEXT NOT NULL DEFAULT '{}',
		PRIMARY KEY(day, metric, dims)
	) WITHOUT ROWID`,
	`CREATE INDEX IF NOT EXISTS idx_stats_daily_metric ON stats_daily(metric, day)`,
	// which (group, day) the rollup finished: zone it used, where the rows came from (local, or a backup
	// file later) and when. A finished day is not rewritten unless forced (review H1).
	`CREATE TABLE IF NOT EXISTS stats_days (
		grp TEXT NOT NULL,
		day TEXT NOT NULL,
		zone TEXT NOT NULL DEFAULT '',
		source TEXT NOT NULL DEFAULT 'local',
		at TEXT NOT NULL,
		PRIMARY KEY(grp, day)
	) WITHOUT ROWID`,
}

var statsTiDB = []string{
	`CREATE TABLE IF NOT EXISTS stats_daily (
		day VARCHAR(10) NOT NULL,
		metric VARCHAR(64) NOT NULL,
		dims VARCHAR(512) NOT NULL DEFAULT '',
		n BIGINT NOT NULL DEFAULT 0,
		vals TEXT NOT NULL,
		PRIMARY KEY(day, metric, dims),
		KEY idx_stats_daily_metric (metric, day)
	)`,
	`CREATE TABLE IF NOT EXISTS stats_days (
		grp VARCHAR(32) NOT NULL,
		day VARCHAR(10) NOT NULL,
		zone VARCHAR(64) NOT NULL DEFAULT '',
		source VARCHAR(16) NOT NULL DEFAULT 'local',
		at VARCHAR(64) NOT NULL,
		PRIMARY KEY(grp, day)
	)`,
}

func init() {
	adminSQLite = append(adminSQLite, statsSQLite...)
	adminTiDB = append(adminTiDB, statsTiDB...)
}

// ---------------------------------------------------------------------------
// writes

// statsOn reports whether statistics are collected here (stats.enabled, not the practice instance).
// Collection and the rollup check it; the export does not (review L2).
func (s *Service) statsOn() bool { return !s.practice && s.Doc().Stats.On() }

// statsAdd adds counter rows: n and every value are added to what the row holds (created at zero when
// missing), in one write transaction. Derived metrics are refused (errStatsKind). Two processes adding
// to the same row (the CLI runs the service in its own process) both count: SQLite's writer lock
// (_txlock=immediate) and TiDB's SELECT ... FOR UPDATE serialize the read and the write of a row.
func (s *Service) statsAdd(ctx context.Context, rows []statsRow) error {
	if !s.statsOn() {
		return errStatsOff
	}
	enc, err := s.statsPrepare(rows, statsCounter)
	if err != nil || len(enc) == 0 {
		return err
	}
	db, err := s.adminDB()
	if err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	tidb := db.Backend == storage.BackendTiDB
	insert := `INSERT INTO stats_daily(day, metric, dims, n, vals) VALUES(?,?,?,0,'{}') ON CONFLICT(day, metric, dims) DO NOTHING`
	lock := ``
	if tidb {
		insert = `INSERT IGNORE INTO stats_daily(day, metric, dims, n, vals) VALUES(?,?,?,0,'{}')`
		lock = ` FOR UPDATE`
	}
	return db.WithWriterContext(ctx, func(tx *sql.Tx) error {
		for _, e := range enc {
			m, _ := s.statsLookup(e.key.metric)
			if _, err := tx.ExecContext(ctx, insert, e.key.day, e.key.metric, e.key.dims); err != nil {
				return err
			}
			var raw string
			if err := tx.QueryRowContext(ctx, `SELECT vals FROM stats_daily WHERE day=? AND metric=? AND dims=?`+lock,
				e.key.day, e.key.metric, e.key.dims).Scan(&raw); err != nil {
				return err
			}
			vals, hist, err := statsDecodeVals(raw)
			if err != nil {
				vals, hist = nil, nil // a damaged cell restarts from this delta rather than blocking every flush
			}
			merged, err := statsEncodeVals(m, statsMergeVals(vals, e.vals), statsHistMerge(statsHistMerge(nil, hist), e.hist))
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE stats_daily SET n = n + ?, vals = ? WHERE day=? AND metric=? AND dims=?`,
				e.n, merged, e.key.day, e.key.metric, e.key.dims); err != nil {
				return err
			}
		}
		return nil
	})
}

// statsDayWrite is one derived group's finished day.
type statsDayWrite struct {
	Group  string
	Day    string // YYYY-MM-DD local day
	Zone   string // the IANA zone the day was cut in (review M6)
	Source string // "local" (default) or "backup" (a later stats.backfill --source)
	// Force rewrites a day already finished. Only an operator's explicit request, and only for days
	// still inside every source's retention window (the rollup checks that; review H1).
	Force bool
	Rows  []statsRow // every metric of the group, Day empty or equal to Day; an empty list finishes an empty day
}

// statsReplaceDay replaces one derived group's rows of one day and marks the day finished, in one
// transaction, so a crash leaves either the old day or the new one (review M4). It refuses counter
// metrics and metrics of another group (errStatsKind) and a finished day unless forced
// (errStatsFinalized).
func (s *Service) statsReplaceDay(ctx context.Context, w statsDayWrite) error {
	if !s.statsOn() {
		return errStatsOff
	}
	metrics := statsGroupMetrics(w.Group)
	if len(metrics) == 0 {
		return fmt.Errorf("%w: group %q", errStatsUndeclared, w.Group)
	}
	if metrics[0].Kind != statsDerived {
		return fmt.Errorf("%w: group %s holds counters, which are never replaced", errStatsKind, w.Group)
	}
	if _, err := time.Parse("2006-01-02", w.Day); err != nil {
		return fmt.Errorf("statistics: day %q: %w", w.Day, err)
	}
	rows := make([]statsRow, len(w.Rows))
	for i, r := range w.Rows {
		if r.Day == "" {
			r.Day = w.Day
		}
		if r.Day != w.Day {
			return fmt.Errorf("statistics: row of %s for %s in the write of %s", r.Metric, r.Day, w.Day)
		}
		if m, ok := s.statsLookup(r.Metric); ok && m.Group != w.Group {
			return fmt.Errorf("%w: %s belongs to group %s, not %s", errStatsKind, m.Name, m.Group, w.Group)
		}
		rows[i] = r
	}
	enc, err := s.statsPrepare(rows, statsDerived)
	if err != nil {
		return err
	}
	db, err := s.adminDB()
	if err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	names := make([]any, 0, len(metrics)+1)
	names = append(names, w.Day)
	for _, m := range metrics {
		names = append(names, m.Name)
	}
	src := cmpOr(w.Source, "local")
	now := s.Clock.Now().UTC().Format(time.RFC3339)
	lock, upsert := ``, `INSERT INTO stats_days(grp, day, zone, source, at) VALUES(?,?,?,?,?)
		ON CONFLICT(grp, day) DO UPDATE SET zone=excluded.zone, source=excluded.source, at=excluded.at`
	if db.Backend == storage.BackendTiDB {
		lock = ` FOR UPDATE`
		upsert = `INSERT INTO stats_days(grp, day, zone, source, at) VALUES(?,?,?,?,?)
			ON DUPLICATE KEY UPDATE zone=VALUES(zone), source=VALUES(source), at=VALUES(at)`
	}
	return db.WithWriterContext(ctx, func(tx *sql.Tx) error {
		var at string
		err := tx.QueryRowContext(ctx, `SELECT at FROM stats_days WHERE grp=? AND day=?`+lock, w.Group, w.Day).Scan(&at)
		switch {
		case err == nil && !w.Force:
			return fmt.Errorf("%w: %s %s (finished %s)", errStatsFinalized, w.Group, w.Day, at)
		case err != nil && !errors.Is(err, sql.ErrNoRows):
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM stats_daily WHERE day=? AND metric IN (?`+strings.Repeat(",?", len(metrics)-1)+`)`, names...); err != nil {
			return err
		}
		for _, e := range enc {
			m, _ := s.statsLookup(e.key.metric)
			vals, err := statsEncodeVals(m, e.vals, e.hist)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO stats_daily(day, metric, dims, n, vals) VALUES(?,?,?,?,?)`,
				e.key.day, e.key.metric, e.key.dims, e.n, vals); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, upsert, w.Group, w.Day, w.Zone, src, now)
		return err
	})
}

// statsFinishedDays lists the days of a group the rollup finished, from first to last (inclusive,
// "" for no bound), oldest first.
func (s *Service) statsFinishedDays(ctx context.Context, group, first, last string) ([]string, error) {
	db, err := s.adminDB()
	if err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	q, args := `SELECT day FROM stats_days WHERE grp=?`, []any{group}
	if first != "" {
		q, args = q+` AND day>=?`, append(args, first)
	}
	if last != "" {
		q, args = q+` AND day<=?`, append(args, last)
	}
	rows, err := db.Reader().QueryContext(ctx, q+` ORDER BY day`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Action shapes (catalog: stats.export, stats.rollup). The Actions are implementation_deferred in
// this contract; the parts that build them return these.

// statsExportInput is stats.export's input. The period is cut to whole ISO weeks in UTC (H5 rule 1).
type statsExportInput struct {
	From string `json:"from,omitempty"` // first day (YYYY-MM-DD); default 12 weeks before To
	To   string `json:"to,omitempty"`   // last day; default the end of the last finished week
	// ResetID replaces the install id before building the file (the console's "new id" button).
	ResetID bool `json:"reset_id,omitempty"`
}

// statsExportDoc is the exported file (stats.export output, the CLI's --file).
type statsExportDoc struct {
	Schema      int                `json:"schema"` // 1
	GeneratedAt string             `json:"generated_at"`
	Install     statsExportInstall `json:"install"`
	Period      statsExportPeriod  `json:"period"`
	// Settings: booleans, enumerations and bucketed numbers only; no names, hostnames, zones or keys (H5 rule 6).
	Settings map[string]any   `json:"settings"`
	Weekly   []statsExportRow `json:"weekly"`
	Totals   []statsExportRow `json:"totals"`
}

// statsExportInstall describes the install. ID is random, made at the first export and resettable (H5 rule 7).
type statsExportInstall struct {
	ID        string `json:"id"`
	Version   string `json:"version"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	Tier      string `json:"tier"`
	FirstSeen string `json:"first_seen"` // a week ("2026-W47"), not a date
}

// statsExportPeriod is the exported period as ISO weeks ("2026-W35" .. "2026-W47").
type statsExportPeriod struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// statsExportRow is one metric row of a week (Weekly) or of the whole period (Totals). Every Dims value
// is a member of its dimension's Values; a bucketed metric gives Range or a rounded N.
type statsExportRow struct {
	Week   string             `json:"week,omitempty"`
	Metric string             `json:"metric"`
	Dims   map[string]string  `json:"dims,omitempty"`
	N      int64              `json:"n,omitempty"`
	Range  string             `json:"range,omitempty"`
	Vals   map[string]float64 `json:"vals,omitempty"`
	Hist   []int64            `json:"h,omitempty"`
}

// statsRollupInput is stats.rollup's input (operator only: CLI or console).
type statsRollupInput struct {
	Group string `json:"group,omitempty"` // one derived group; empty = every derived group
	Since string `json:"since,omitempty"` // recompute from this day (needs Force for finished days)
	Force bool   `json:"force,omitempty"` // refused for days outside any source's retention window
}

// statsRollupOutput is what one rollup run did.
type statsRollupOutput struct {
	Zone     string            `json:"zone"`
	Finished map[string]int    `json:"finished"`          // days finished in this run, per group
	Through  map[string]string `json:"through"`           // last finished day, per group
	Skipped  map[string]string `json:"skipped,omitempty"` // group → why it was not rolled up (a reason code)
}
