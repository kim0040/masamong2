package application

import (
	"context"
	"errors"
	"fmt"
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
func (s *Service) doctor(ctx context.Context, req action.Request) action.Result {
	lap := s.laps(ctx, "doctor")
	defer lap.done()
	features := map[string]any{}
	attention := []map[string]any{}
	// reason is a code (features.reason.<code>, "code:arg" for one argument) or a provider's own error
	// text; reason_text is what a person reads, in the request's language.
	put := func(name, status, reason string) {
		item := map[string]any{"status": status}
		if reason != "" {
			item["reason"] = reason
			code, arg, _ := strings.Cut(reason, ":")
			if s.I18n != nil && s.I18n.Has("features.reason."+code) {
				item["reason_text"] = s.msg(req, "features.reason."+code, "arg", arg)
			}
		}
		features[name] = item
	}
	featureOff := func(name string) bool { return s.Doc().Features != nil && !s.Doc().Features[name] }
	if s.Store == nil {
		put("storage", "unconfigured", "not_setup")
	} else {
		put("storage", "ready", s.Store.Backend)
	}
	put("jobs", "ready", "")
	put("config", "ready", "")
	limit := memlimit.Current()
	if limit.Bytes == 0 {
		put("go_memory_limit", "off", "go_memory_limit_off")
	} else {
		put("go_memory_limit", "ready", fmt.Sprintf("go_memory_limit:%d MiB (%s)", limit.Bytes/(1<<20), limit.Source))
	}
	if m, ok := features["go_memory_limit"].(map[string]any); ok {
		m["bytes"], m["source"], m["automatic"] = limit.Bytes, limit.Source, limit.Automatic
	}
	// The tier the daemon installed at startup. Until then Current is the medium
	// default, so a library user who never starts masamongd still gets a line.
	profile := hostprofile.Current()
	put("host_profile", "ready", "host_profile:"+profile.DoctorDetail())
	checkTimezone := func(feature, tz string) {
		if tz == "" {
			put(feature, "ready", "")
		} else if _, err := time.LoadLocation(tz); err != nil {
			if _, warned := warnedZoneSettings.LoadOrStore(feature+"="+tz, true); !warned {
				s.Log.Error("unknown configured timezone", "setting", feature, "timezone", tz, "err", err)
			}
			put(feature, "error", "timezone_invalid:"+tz)
			attention = append(attention, map[string]any{"item": feature, "reason": "timezone_invalid:" + tz})
		} else {
			put(feature, "ready", "")
		}
	}
	checkTimezone("timezone", s.Doc().I18n.Timezone)
	checkTimezone("memory_timezone", s.Doc().Memory.NaiveTimezone)
	switch {
	case !s.WebBuild:
		put("web", "disabled", "noweb_build")
	case !s.Doc().Admin.Web.Enabled:
		put("web", "disabled", "admin.web.enabled=false")
	default:
		put("web", "ready", "")
	}
	switch {
	case !s.Doc().Discord.Enabled:
		put("discord", "disabled", "discord.enabled=false")
	case s.Discord == nil:
		put("discord", "pending", "gateway_not_started")
	case s.Discord.Connected():
		// connected by the SDK's own callbacks, yet Discord has stopped acknowledging heartbeats: a stuck
		// socket looks like this. A warning only; DiscordGo owns reconnecting.
		if h, ok := s.gatewayHealth(); ok && h.State == GatewayCheck {
			put("discord", "degraded", "gateway_stale")
			attention = append(attention, map[string]any{"item": "discord", "reason": "gateway_stale",
				"reason_text": s.msg(req, "features.reason.gateway_stale")})
		} else {
			put("discord", "ready", "")
		}
	default:
		status, reportsStartup := s.Discord.(DiscordStartupReporter)
		startup := DiscordStartupStatus{}
		if reportsStartup {
			startup = status.StartupStatus()
		}
		if startup.LastError != "" {
			state, reason := "pending", "gateway_retrying"
			if startup.NextRetry == "" {
				state, reason = "error", "gateway_start_failed"
			}
			put("discord", state, reason)
			attention = append(attention, map[string]any{"item": "discord", "reason": reason})
		} else if startup.Connecting {
			// the first attempt (or the wait for READY) is still running: not a failure
			put("discord", "pending", "gateway_connecting")
		} else {
			put("discord", "error", "gateway_disconnected")
			attention = append(attention, map[string]any{"item": "discord", "reason": "gateway_disconnected"})
		}
	}

	// channels where Discord refused a reply for lack of permission (LIVE-16), as long as the operator
	// alert for them is fresh
	lap.mark("discord")
	if n := s.deliveryPermissionProblems(); n > 0 {
		arg := strconv.Itoa(n)
		attention = append(attention, map[string]any{"item": "discord_delivery", "reason": "delivery_permission:" + arg,
			"reason_text": s.msg(req, "features.reason.delivery_permission", "arg", arg)})
	}
	// Discord edits and deletes that could not be applied are quarantined for an operator; edits stay paused
	// until they are handled, which used to show only in the log (ADR-16: nothing an operator must do
	// may live only in logs).
	if n := s.reconciliationBlocked(); n > 0 {
		arg := strconv.Itoa(n)
		put("discord_reconciliation", "degraded", "reconciliation_blocked:"+arg) // the console's Doctor tab lists features only
		attention = append(attention, map[string]any{"item": "discord_reconciliation", "reason": "reconciliation_blocked:" + arg,
			"reason_text": s.msg(req, "features.reason.reconciliation_blocked", "arg", arg)})
	}
	lap.mark("delivery_reconciliation")
	if left := s.leftoverList(); len(left) > 0 {
		put("maintenance_files", "error", "maintenance_leftovers:"+strconv.Itoa(len(left)))
		attention = append(attention, map[string]any{"item": "maintenance_files", "reason": "maintenance_leftovers:" + strconv.Itoa(len(left)),
			"reason_text": s.msg(req, "features.reason.maintenance_leftovers", "arg", strconv.Itoa(len(left)))})
	}
	var lastRestore RestoreResult
	lap.mark("leftovers")
	if ok, _ := s.kvGet("restore:last", &lastRestore); ok && !lastRestore.OK && lastRestore.RollbackOK && lastRestore.At != "" {
		if at, err := time.Parse(time.RFC3339, lastRestore.At); err == nil && s.Clock.Now().Sub(at) < 72*time.Hour {
			attention = append(attention, map[string]any{"item": "restore", "reason": "restore_failed:" + lastRestore.Name,
				"reason_text": s.msg(req, "features.reason.restore_failed", "arg", lastRestore.Name)})
		}
	}
	if problems := s.cfgProblems.Load(); problems != nil {
		for _, p := range *problems {
			put(p.Off, "error", "config_setting_rejected:"+p.Path)
			attention = append(attention, map[string]any{"item": p.Off, "reason": "config_setting_rejected:" + p.Path, "detail": p.Reason,
				"reason_text": s.msg(req, "features.reason.config_setting_rejected", "arg", p.Path+": "+p.Reason)})
		}
	}
	for _, st := range s.Registry().Statuses() {
		if st.Status == "error" {
			attention = append(attention, map[string]any{"item": "provider:" + st.ID, "reason": st.Error})
		}
	}
	liveAny := func(feature string, kinds ...string) {
		if s.Doc().Features != nil && !s.Doc().Features[feature] {
			put(feature, "disabled", "feature_flag")
			return
		}
		found := false
		var providerErr string
		for _, st := range s.Registry().Statuses() {
			matches := false
			for _, kind := range kinds {
				matches = matches || st.Kind == kind
			}
			if !matches {
				continue
			}
			found = true
			if st.Configured && !st.Disabled {
				put(feature, "ready", "configured")
				return
			}
			if st.Status == "error" {
				providerErr = st.Error
			}
		}
		switch {
		case providerErr != "":
			put(feature, "error", providerErr)
		case found:
			put(feature, "unconfigured", "no_key")
		default:
			put(feature, "unconfigured", "no_provider")
		}
	}
	if s.practice {
		put("weather", "ready", "practice_sample")
		put("disasters", "ready", "practice_sample")
	} else {
		liveAny("weather", config.KindOpenMeteo, config.KindKMA)
		liveAny("disasters", config.KindKMA)
	}
	liveAny("finance", config.KindFinnhub)
	liveAny("search", websearch.Kinds...) // any search service with a key
	if s.Doc().Features == nil || s.Doc().Features["polls"] {
		put("polls", "ready", "")
	} else {
		put("polls", "disabled", "feature_flag")
	}
	// the chat model is ready when the provider the response role points at has its key (an instance
	// may list several model providers, some without keys)
	llmReady := false
	switch a, err := s.llmAdapter("response"); {
	case err != nil:
		put("llm", "unconfigured", "needs_chat_model")
	case !a.Configured():
		put("llm", "unconfigured", "provider_key_missing")
	default:
		llmReady = true
		put("llm", "ready", "configured")
	}
	// fortune: the chat model writes it
	switch {
	case featureOff("fortune"):
		put("fortune", "disabled", "feature_flag")
	case !llmReady:
		put("fortune", "unconfigured", "needs_chat_model")
	default:
		put("fortune", "ready", "")
	}
	// notices: public school boards from the catalog (built in, or the instance's own file)
	if featureOff("notices") {
		put("notices", "disabled", "feature_flag")
	} else if _, _, err := s.noticeCatalog(); err != nil {
		put("notices", "error", err.Error())
	} else {
		put("notices", "ready", "")
	}
	// images: a model in the image role whose provider has its key
	if featureOff("images") {
		put("images", "disabled", "feature_flag")
	} else if a, err := s.llmAdapter("image"); err != nil {
		put("images", "unconfigured", "needs_image_model")
	} else if !a.Configured() {
		put("images", "unconfigured", "provider_key_missing")
	} else {
		put("images", "ready", "configured")
	}
	// vision: the optional image-recognition role. Unset is fine (pictures go the other ways) and so is the
	// chat switch being off; set, its provider needs its key, and the model must not be one that refused
	// pictures lately or whose provider is in its outage cool-down
	if m, set := modelOf(s.Doc(), "vision"); !set {
		put("vision", "off", "vision_unset")
	} else if !s.Doc().Chat.VisionOn() {
		put("vision", "off", "vision_off")
	} else if a, err := s.llmAdapter("vision"); err != nil {
		put("vision", "unconfigured", "needs_vision_model")
	} else if !a.Configured() {
		put("vision", "unconfigured", "provider_key_missing")
	} else if r := (chatRoute{model: m.Model, provider: m.Provider, a: a}); !s.visionUsable(s.Doc(), r) {
		put("vision", "degraded", "vision_unsupported")
	} else if s.visionCoolingNow(r) {
		put("vision", "degraded", "vision_cooling")
	} else {
		put("vision", "ready", "configured")
	}
	lap.mark("models_features")
	memStatus, memDetail := s.memoryDoctor()
	lap.mark("memory")
	put("memory", memStatus, memDetail)
	switch {
	case s.Doc().Memory.Retrieve == "off":
		put("retrieval", "off", "memory.retrieve=off")
	case memStatus == "ready":
		put("retrieval", "ready", "")
	default:
		put("retrieval", memStatus, memDetail)
	}
	// identity: who is who, resolved from the same memory index
	put("identity", memStatus, memDetail)
	if s.Doc().Memory.MessageVectorsOn() && (memStatus == "ready" || memStatus == "pending_index") {
		// listed while memory.message_vectors is on (the default) and memory is usable: memory's own line
		// already says why it is not, and a second "unconfigured" line would be noise. pending_messages:<n>
		// is the work an upgrade leaves behind until a pass or a reindex embeds it
		status, reason := s.messageVectorDoctor(ctx, memStatus)
		put("message_vectors", status, reason)
		lap.mark("message_vectors")
	}
	if memStatus == "ready" || memStatus == "pending_index" {
		// measurement only (ADR-18 stage 0): how many rooms are resident, an estimate of their Go heap, the
		// full-text pages and the last rebuild's stage-boundary heap samples; numbers, never names
		s.putMemoryResidency(ctx, req, features, put)
		lap.mark("residency")
	}
	// participation: joining a conversation unasked needs the switch and the chat model (its judge)
	switch {
	case !s.Doc().Participation.Enabled || s.Doc().Participation.Mode == "off":
		put("participation", "off", "participation_off")
	case !llmReady:
		put("participation", "unconfigured", "needs_chat_model")
	default:
		put("participation", "ready", "participation_"+s.Doc().Participation.Mode)
	}
	put("eval", "ready", "")

	// permission problems with the data folder that the daemon lives with (Windows ACLs)
	dataWarnings := fsutil.Warnings()
	for _, w := range dataWarnings {
		attention = append(attention, map[string]any{"item": "data_folder", "reason": "data_folder_permissions", "detail": w})
	}

	info := s.storageInfo()
	out := map[string]any{
		"instance":        s.Doc().Instance.ID,
		"profile":         s.Doc().Limits.Profile,
		"revision":        s.Revision(),
		"version":         Version,
		"storage":         info,
		"storage_backend": info["backend"],
		"features":        features,
		"providers":       s.Registry().Statuses(),
		"attention":       attention,
		"single_ready":    false,
		"memory_complete": false,
		"title":           s.msg(req, "doctor.title"),
	}
	if len(dataWarnings) > 0 {
		out["data_folder_warnings"] = dataWarnings
	}
	if s.Discord != nil {
		if status, ok := s.Discord.(DiscordStartupReporter); ok {
			startup := status.StartupStatus()
			if startup.Attempts > 0 {
				out["discord_startup"] = startup
			}
		}
	}
	for k, v := range redactedEnvNote() {
		out[k] = v
	}
	return action.OK(req, out)
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
