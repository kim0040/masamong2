package application

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"masamong/internal/action"
	"masamong/internal/config"
	"masamong/internal/fsutil"
	"masamong/internal/hostprofile"
	"masamong/internal/memlimit"
	"masamong/internal/memory/engine"
	"masamong/internal/providers/websearch"
	"masamong/internal/storage"
)

// warnedZoneSettings keeps a bad configured zone to one log line per setting and value: the console
// and the CLI poll doctor, so logging on every call would fill the rotating log.
var warnedZoneSettings sync.Map

// doctor reports one status per capability; the GUI and CLI render the reason next to the status. A
// feature without its key does not answer (no_key): only the practice instance answers from built-in
// samples (practice_sample), so a real instance never shows sample data as if it were live.
//
// The report is the result of running doctorChecks in order. A check adds feature items (put) and
// attention lines (flag); a later check may read what an earlier one found (llmReady, memStatus).
func (s *Service) doctor(ctx context.Context, req action.Request) action.Result {
	lap := s.laps(ctx, "doctor")
	defer lap.done()
	r := &doctorRun{s: s, ctx: ctx, req: req, features: map[string]any{}, attention: []map[string]any{}}
	for _, c := range doctorChecks {
		c.run(r)
		lap.mark(c.name)
	}
	return action.OK(req, r.report())
}

// doctorRun is one pass of the doctor: the request it answers, the items and attention lines the
// checks have added so far, and the findings later checks build on.
type doctorRun struct {
	s         *Service
	ctx       context.Context
	req       action.Request
	features  map[string]any
	attention []map[string]any

	llmReady             bool   // the chat model's provider has its key (set by checkLLM)
	memStatus, memDetail string // memory readiness (set by checkMemory)
	dataWarnings         []string
}

// doctorCheck is one named step of the doctor. The name labels the step in the slow-request log.
type doctorCheck struct {
	name string
	run  func(r *doctorRun)
}

// doctorChecks is the doctor's list, in the order its items and attention lines are produced.
var doctorChecks = []doctorCheck{
	{"core", checkCore},
	{"memory_limit", checkMemoryLimit},
	{"host_profile", checkHostProfile},
	{"timezones", checkTimezones},
	{"web", checkWeb},
	{"discord", checkDiscord},
	{"delivery_permission", checkDeliveryPermission},
	{"reconciliation", checkReconciliation},
	{"leftovers", checkLeftovers},
	{"restore", checkLastRestore},
	{"config_problems", checkConfigProblems},
	{"providers", checkProviders},
	{"tool_keys", checkToolKeys},
	{"live_features", checkLiveFeatures},
	{"llm", checkLLM},
	{"fortune", checkFortune},
	{"notices", checkNotices},
	{"images", checkImages},
	{"vision", checkVision},
	{"memory", checkMemory},
	{"participation", checkParticipation},
	{"eval", checkEval},
	{"data_folder", checkDataFolder},
}

// put records one feature item. reason is a code (features.reason.<code>, "code:arg" for one
// argument) or a provider's own error text; reason_text is what a person reads, in the request's
// language.
func (r *doctorRun) put(name, status, reason string) {
	item := map[string]any{"status": status}
	if reason != "" {
		item["reason"] = reason
		code, arg, _ := strings.Cut(reason, ":")
		if r.s.I18n != nil && r.s.I18n.Has("features.reason."+code) {
			item["reason_text"] = r.s.msg(r.req, "features.reason."+code, "arg", arg)
		}
	}
	r.features[name] = item
}

// flag adds an attention line: something an operator should look at.
func (r *doctorRun) flag(item, reason string) {
	r.attention = append(r.attention, map[string]any{"item": item, "reason": reason})
}

// flagText is flag with the reason written out in the request's language; arg fills the message's
// placeholder when it has one.
func (r *doctorRun) flagText(item, reason, key string, arg ...any) {
	if len(arg) > 0 {
		arg = []any{"arg", arg[0]}
	}
	r.attention = append(r.attention, map[string]any{"item": item, "reason": reason,
		"reason_text": r.s.msg(r.req, "features.reason."+key, arg...)})
}

func (r *doctorRun) featureOff(name string) bool {
	return r.s.Doc().Features != nil && !r.s.Doc().Features[name]
}

// report assembles the answer from what the checks found.
func (r *doctorRun) report() map[string]any {
	s := r.s
	info := s.storageInfo()
	out := map[string]any{
		"instance":        s.Doc().Instance.ID,
		"profile":         s.Doc().Limits.Profile,
		"revision":        s.Revision(),
		"version":         Version,
		"storage":         info,
		"storage_backend": info["backend"],
		"features":        r.features,
		"providers":       s.Registry().Statuses(),
		"attention":       r.attention,
		"single_ready":    false,
		"memory_complete": false,
		"title":           s.msg(r.req, "doctor.title"),
	}
	if len(r.dataWarnings) > 0 {
		out["data_folder_warnings"] = r.dataWarnings
	}
	if s.Discord() != nil {
		if status, ok := s.Discord().(DiscordStartupReporter); ok {
			startup := status.StartupStatus()
			if startup.Attempts > 0 {
				out["discord_startup"] = startup
			}
		}
	}
	for k, v := range redactedEnvNote() {
		out[k] = v
	}
	return out
}

func checkCore(r *doctorRun) {
	if r.s.Store == nil {
		r.put("storage", "unconfigured", "not_setup")
	} else {
		r.put("storage", "ready", r.s.Store.Backend)
	}
	r.put("jobs", "ready", "")
	r.put("config", "ready", "")
}

func checkMemoryLimit(r *doctorRun) {
	limit := memlimit.Current()
	if limit.Bytes == 0 {
		r.put("go_memory_limit", "off", "go_memory_limit_off")
	} else {
		r.put("go_memory_limit", "ready", fmt.Sprintf("go_memory_limit:%d MiB (%s)", limit.Bytes/(1<<20), limit.Source))
	}
	if m, ok := r.features["go_memory_limit"].(map[string]any); ok {
		m["bytes"], m["source"], m["automatic"] = limit.Bytes, limit.Source, limit.Automatic
	}
}

// checkHostProfile reports the tier the daemon installed at startup. Until then Current is the
// medium default, so a library user who never starts masamongd still gets a line.
func checkHostProfile(r *doctorRun) {
	r.put("host_profile", "ready", "host_profile:"+hostprofile.Current().DoctorDetail())
}

func checkTimezones(r *doctorRun) {
	r.checkTimezone("timezone", r.s.Doc().I18n.Timezone)
	r.checkTimezone("memory_timezone", r.s.Doc().Memory.NaiveTimezone)
}

func (r *doctorRun) checkTimezone(feature, tz string) {
	if tz == "" {
		r.put(feature, "ready", "")
	} else if _, err := time.LoadLocation(tz); err != nil {
		if _, warned := warnedZoneSettings.LoadOrStore(feature+"="+tz, true); !warned {
			r.s.Log.Error("unknown configured timezone", "setting", feature, "timezone", tz, "err", err)
		}
		r.put(feature, "error", "timezone_invalid:"+tz)
		r.flag(feature, "timezone_invalid:"+tz)
	} else {
		r.put(feature, "ready", "")
	}
}

func checkWeb(r *doctorRun) {
	switch {
	case !r.s.WebBuild:
		r.put("web", "disabled", "noweb_build")
	case !r.s.Doc().Admin.Web.Enabled:
		r.put("web", "disabled", "admin.web.enabled=false")
	default:
		r.put("web", "ready", "")
	}
}

func checkDiscord(r *doctorRun) {
	s := r.s
	switch {
	case !s.Doc().Discord.Enabled:
		r.put("discord", "disabled", "discord.enabled=false")
	case s.Discord() == nil:
		r.put("discord", "pending", "gateway_not_started")
	case s.Discord().Connected():
		// connected by the SDK's own callbacks, yet Discord has stopped acknowledging heartbeats: a stuck
		// socket looks like this. A warning only; DiscordGo owns reconnecting.
		if h, ok := s.gatewayHealth(); ok && h.State == GatewayCheck {
			r.put("discord", "degraded", "gateway_stale")
			r.flagText("discord", "gateway_stale", "gateway_stale")
		} else {
			r.put("discord", "ready", "")
		}
	default:
		status, reportsStartup := s.Discord().(DiscordStartupReporter)
		startup := DiscordStartupStatus{}
		if reportsStartup {
			startup = status.StartupStatus()
		}
		if startup.LastError != "" {
			state, reason := "pending", "gateway_retrying"
			if startup.NextRetry == "" {
				state, reason = "error", "gateway_start_failed"
			}
			r.put("discord", state, reason)
			r.flag("discord", reason)
		} else if startup.Connecting {
			// the first attempt (or the wait for READY) is still running: not a failure
			r.put("discord", "pending", "gateway_connecting")
		} else {
			r.put("discord", "error", "gateway_disconnected")
			r.flag("discord", "gateway_disconnected")
		}
	}
}

// checkDeliveryPermission lists channels where Discord refused a reply for lack of permission
// (LIVE-16), as long as the operator alert for them is fresh.
func checkDeliveryPermission(r *doctorRun) {
	if n := r.s.deliveryPermissionProblems(); n > 0 {
		arg := strconv.Itoa(n)
		r.flagText("discord_delivery", "delivery_permission:"+arg, "delivery_permission", arg)
	}
}

// checkReconciliation reports Discord edits and deletes that could not be applied: they are
// quarantined for an operator and edits stay paused until they are handled, which used to show only in
// the log (ADR-16: nothing an operator must do may live only in logs).
func checkReconciliation(r *doctorRun) {
	if n := r.s.reconciliationBlocked(); n > 0 {
		arg := strconv.Itoa(n)
		r.put("discord_reconciliation", "degraded", "reconciliation_blocked:"+arg) // the console's Doctor tab lists features only
		r.flagText("discord_reconciliation", "reconciliation_blocked:"+arg, "reconciliation_blocked", arg)
	}
}

func checkLeftovers(r *doctorRun) {
	if left := r.s.leftoverList(); len(left) > 0 {
		arg := strconv.Itoa(len(left))
		r.put("maintenance_files", "error", "maintenance_leftovers:"+arg)
		r.flagText("maintenance_files", "maintenance_leftovers:"+arg, "maintenance_leftovers", arg)
	}
}

// checkLastRestore flags a restore that failed (and was rolled back) in the last 72 hours.
func checkLastRestore(r *doctorRun) {
	var last RestoreResult
	if ok, _ := r.s.kvGet("restore:last", &last); ok && !last.OK && last.RollbackOK && last.At != "" {
		if at, err := time.Parse(time.RFC3339, last.At); err == nil && r.s.Clock.Now().Sub(at) < 72*time.Hour {
			r.flagText("restore", "restore_failed:"+last.Name, "restore_failed", last.Name)
		}
	}
}

// checkConfigProblems lists settings of the committed configuration this release no longer accepts.
func checkConfigProblems(r *doctorRun) {
	if problems := r.s.cfgProblems.Load(); problems != nil {
		for _, p := range *problems {
			r.put(p.Off, "error", "config_setting_rejected:"+p.Path)
			r.attention = append(r.attention, map[string]any{"item": p.Off, "reason": "config_setting_rejected:" + p.Path, "detail": p.Reason,
				"reason_text": r.s.msg(r.req, "features.reason.config_setting_rejected", "arg", p.Path+": "+p.Reason)})
		}
	}
}

func checkProviders(r *doctorRun) {
	for _, st := range r.s.Registry().Statuses() {
		if st.Status == "error" {
			r.flag("provider:"+st.ID, st.Error)
		}
	}
}

// checkToolKeys finds operator tools with a key written into their headers or web address (a
// configuration an earlier release accepted): the value is exported and backed up as if it were not a
// secret, so the operator is told which tools (ids only) to move to credential_ref.
func checkToolKeys(r *doctorRun) {
	if ids := r.s.toolsWithDirectKeys(); len(ids) > 0 {
		arg := strings.Join(ids, ", ")
		r.put("tool_keys", "degraded", "tool_direct_key:"+arg)
		r.flagText("tool_keys", "tool_direct_key:"+arg, "tool_direct_key", arg)
	}
}

// liveAny reports a feature that needs one of several provider kinds to be configured.
func (r *doctorRun) liveAny(feature string, kinds ...string) {
	if r.s.Doc().Features != nil && !r.s.Doc().Features[feature] {
		r.put(feature, "disabled", "feature_flag")
		return
	}
	found := false
	var providerErr string
	for _, st := range r.s.Registry().Statuses() {
		if !slices.Contains(kinds, st.Kind) {
			continue
		}
		found = true
		if st.Configured && !st.Disabled {
			r.put(feature, "ready", "configured")
			return
		}
		if st.Status == "error" {
			providerErr = st.Error
		}
	}
	switch {
	case providerErr != "":
		r.put(feature, "error", providerErr)
	case found:
		r.put(feature, "unconfigured", "no_key")
	default:
		r.put(feature, "unconfigured", "no_provider")
	}
}

func checkLiveFeatures(r *doctorRun) {
	if r.s.practice {
		r.put("weather", "ready", "practice_sample")
		r.put("disasters", "ready", "practice_sample")
	} else {
		r.liveAny("weather", config.KindOpenMeteo, config.KindKMA)
		r.liveAny("disasters", config.KindKMA)
	}
	r.liveAny("finance", config.KindFinnhub)
	r.liveAny("search", websearch.Kinds...) // any search service with a key
	if r.s.Doc().Features == nil || r.s.Doc().Features["polls"] {
		r.put("polls", "ready", "")
	} else {
		r.put("polls", "disabled", "feature_flag")
	}
}

// checkLLM: the chat model is ready when the provider the response role points at has its key (an
// instance may list several model providers, some without keys).
func checkLLM(r *doctorRun) {
	switch a, err := r.s.llmAdapter("response"); {
	case err != nil:
		r.put("llm", "unconfigured", "needs_chat_model")
	case !a.Configured():
		r.put("llm", "unconfigured", "provider_key_missing")
	default:
		r.llmReady = true
		r.put("llm", "ready", "configured")
	}
}

// checkFortune: the chat model writes it.
func checkFortune(r *doctorRun) {
	switch {
	case r.featureOff("fortune"):
		r.put("fortune", "disabled", "feature_flag")
	case !r.llmReady:
		r.put("fortune", "unconfigured", "needs_chat_model")
	default:
		r.put("fortune", "ready", "")
	}
}

// checkNotices: public school boards from the catalog (built in, or the instance's own file).
func checkNotices(r *doctorRun) {
	if r.featureOff("notices") {
		r.put("notices", "disabled", "feature_flag")
	} else if _, _, err := r.s.noticeCatalog(); err != nil {
		r.put("notices", "error", err.Error())
	} else {
		r.put("notices", "ready", "")
	}
}

// checkImages: a model in the image role whose provider has its key.
func checkImages(r *doctorRun) {
	if r.featureOff("images") {
		r.put("images", "disabled", "feature_flag")
	} else if a, err := r.s.llmAdapter("image"); err != nil {
		r.put("images", "unconfigured", "needs_image_model")
	} else if !a.Configured() {
		r.put("images", "unconfigured", "provider_key_missing")
	} else {
		r.put("images", "ready", "configured")
	}
}

// checkVision reports the optional image-recognition role. Unset is fine (pictures go the other
// ways) and so is the chat switch being off; set, its provider needs its key, and the model must not
// be one that refused pictures lately or whose provider is in its outage cool-down.
func checkVision(r *doctorRun) {
	s := r.s
	if m, set := modelOf(s.Doc(), "vision"); !set {
		r.put("vision", "off", "vision_unset")
	} else if !s.Doc().Chat.VisionOn() {
		r.put("vision", "off", "vision_off")
	} else if a, err := s.llmAdapter("vision"); err != nil {
		r.put("vision", "unconfigured", "needs_vision_model")
	} else if !a.Configured() {
		r.put("vision", "unconfigured", "provider_key_missing")
	} else if rt := (chatRoute{model: m.Model, provider: m.Provider, a: a}); !s.visionUsable(s.Doc(), rt) {
		r.put("vision", "degraded", "vision_unsupported")
	} else if s.visionCoolingNow(rt) {
		r.put("vision", "degraded", "vision_cooling")
	} else {
		r.put("vision", "ready", "configured")
	}
}

// checkMemory reports memory, retrieval, identity and, when memory is usable, the message-vector
// index and the resident-room measurements.
func checkMemory(r *doctorRun) {
	s := r.s
	r.memStatus, r.memDetail = s.memoryDoctor()
	r.put("memory", r.memStatus, r.memDetail)
	switch {
	case s.Doc().Memory.Retrieve == "off":
		r.put("retrieval", "off", "memory.retrieve=off")
	case r.memStatus == "ready":
		r.put("retrieval", "ready", "")
	default:
		r.put("retrieval", r.memStatus, r.memDetail)
	}
	// identity: who is who, resolved from the same memory index
	r.put("identity", r.memStatus, r.memDetail)
	usable := r.memStatus == "ready" || r.memStatus == "pending_index"
	if s.Doc().Memory.MessageVectorsOn() && usable {
		// listed while memory.message_vectors is on (the default) and memory is usable: memory's own line
		// already says why it is not, and a second "unconfigured" line would be noise. pending_messages:<n>
		// is the work an upgrade leaves behind until a pass or a reindex embeds it
		status, reason := s.messageVectorDoctor(r.ctx, r.memStatus)
		r.put("message_vectors", status, reason)
	}
	if usable {
		// measurement only (ADR-18 stage 0): how many rooms are resident, an estimate of their Go heap, the
		// full-text pages and the last rebuild's stage-boundary heap samples; numbers, never names
		s.putMemoryResidency(r.ctx, r.req, r.features, r.put)
	}
}

// checkParticipation: joining a conversation unasked needs the switch and the chat model (its judge).
func checkParticipation(r *doctorRun) {
	p := r.s.Doc().Participation
	switch {
	case !p.Enabled || p.Mode == "off":
		r.put("participation", "off", "participation_off")
	case !r.llmReady:
		r.put("participation", "unconfigured", "needs_chat_model")
	default:
		r.put("participation", "ready", "participation_"+p.Mode)
	}
}

func checkEval(r *doctorRun) { r.put("eval", "ready", "") }

// checkDataFolder flags permission problems with the data folder that the daemon lives with
// (Windows ACLs).
func checkDataFolder(r *doctorRun) {
	r.dataWarnings = fsutil.Warnings()
	for _, w := range r.dataWarnings {
		r.attention = append(r.attention, map[string]any{"item": "data_folder", "reason": "data_folder_permissions", "detail": w})
	}
}

func (s *Service) storageInfo() map[string]any {
	if s.Store == nil {
		return map[string]any{"backend": "", "status": "unconfigured"}
	}
	info := map[string]any{
		"backend":          s.Store.Backend,
		"mysql_compatible": s.Store.Backend == storage.BackendTiDB,
		"target":           s.Store.Path(),
	}
	if s.Store.IsSQLite() {
		info["engine"] = "sqlite"
	} else {
		info["engine"] = "tidb"
		info["protocol"] = "mysql"
	}
	return info
}

// messageVectorDoctor reports the message-vector index (memory.message_vectors): the messages of closed
// leaves still waiting for a vector are "pending_messages:<n>".
func (s *Service) messageVectorDoctor(ctx context.Context, memStatus string) (string, string) {
	if memStatus != "ready" && memStatus != "pending_index" {
		return memStatus, "needs_memory"
	}
	eng, err := s.MemoryEngine()
	if err != nil {
		return "error", "engine_unavailable"
	}
	space := eng.MessageSpace()
	dims := eng.MessageVectorDims()
	wait, err := eng.MessageVectorWaitReason()
	if err != nil {
		return "error", "leaf_width_query_failed"
	}
	if ix := eng.Index(); wait == "" && ix != nil && ix.MessageSpace == space && ix.MsgVecs.Dims > 0 {
		dims = ix.MsgVecs.Dims
	}
	width := ""
	if dims > 0 {
		width = ";message_dims:" + fmt.Sprint(dims)
	}
	_, stored, err := eng.Store.MessageVectorCounts(space)
	if err != nil {
		return "error", "vector_count_failed"
	}
	pending, err := s.countPendingNow(ctx, eng, space, stored) // shared with the console (one count at a time)
	if errors.Is(err, errPendingCounting) {
		return "pending_index", "pending_counting"
	}
	if err != nil {
		return "error", "pending_count_failed"
	}
	if pending > 0 {
		reason := fmt.Sprintf("pending_messages:%d%s", pending, width)
		if wait != "" {
			reason += ";waiting:" + wait
		}
		return "pending_index", reason
	}
	if wait != "" {
		return "pending_index", "waiting:" + wait + width
	}
	return "ready", fmt.Sprintf("messages_embedded:%d%s", stored, width)
}

// memoryDoctor reports memory readiness without loading the index (doctor must stay cheap).
func (s *Service) memoryDoctor() (string, string) {
	a, err := s.llmAdapter("embed")
	if err != nil {
		return "unconfigured", "needs_embed_model"
	}
	if !a.Configured() {
		return "unconfigured", "embed_key_missing"
	}
	eng, err := s.MemoryEngine()
	if err != nil {
		return "error", err.Error()
	}
	// two counts over every leaf: the dashboard asks twice per load and the status page polls, so a
	// "ready" answer is reused for a few seconds while the space and the loaded index stay the same
	space := eng.EmbedSpace()
	key := space + "|" + strconv.FormatInt(eng.LoadedAt().UnixNano(), 10)
	c := &s.memDoctorCache
	c.mu.Lock()
	if c.key == key && time.Since(c.at) < memDoctorFresh {
		st, why := c.status, c.reason
		c.mu.Unlock()
		return st, why
	}
	c.mu.Unlock()
	st, why := s.memoryDoctorCount(eng, space)
	c.mu.Lock()
	if st == "ready" {
		c.key, c.at, c.status, c.reason = key, time.Now(), st, why
	} else {
		c.key = ""
	}
	c.mu.Unlock()
	return st, why
}

type memDoctorCache struct {
	mu             sync.Mutex
	key            string
	at             time.Time
	status, reason string
}

// memDoctorFresh outlasts the console overview's 30-second poll, so an open console counts every leaf
// once every two minutes instead of on every poll (the key still changes the moment the index reloads)
const memDoctorFresh = 2 * time.Minute

func (s *Service) memoryDoctorCount(eng *engine.Engine, space string) (string, string) {
	counts, err := eng.Store.Counts()
	if err != nil {
		return "error", err.Error()
	}
	if counts["observations"] == 0 {
		return "empty", "no_messages"
	}
	// search reads only leaves embedded by the current model: after a model change the others count
	// for nothing until they are indexed again
	total, inSpace, err := eng.Store.SpaceCounts(space)
	if err != nil {
		return "error", err.Error()
	}
	switch {
	case total == 0:
		return "pending_index", "needs_reindex"
	case inSpace == 0:
		return "pending_index", "embed_model_changed"
	}
	return "ready", fmt.Sprintf("leaves_embedded:%d", inSpace)
}
