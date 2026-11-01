package application

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"masamong/internal/action"
	"masamong/internal/catalog"
	"masamong/internal/clock"
	"masamong/internal/config"
	"masamong/internal/crash"
	"masamong/internal/errcode"
	"masamong/internal/fsutil"
	"masamong/internal/i18n"
	"masamong/internal/lang"
	"masamong/internal/observability"
	"masamong/internal/providers"
	"masamong/internal/providers/openai"
	"masamong/internal/storage"
	"masamong/internal/tools"
)

// Version is stamped by the build; the default marks a local build.
var Version = "0.2.0-dev"

// DiscordInfo is what the daemon exposes about the live gateway so Actions can report on it
// without importing the connector (02장 §2: Application Services own the DB; transports don't).
type DiscordInfo interface {
	Connected() bool
	BotUser() (id, name string)
	Guilds() []GuildInfo
	Channels(guildID string) []ChannelInfo
	// Permissions returns the bot's permission bits in a channel; ok=false when unknown.
	Permissions(guildID, channelID string) (int64, bool)
}

// DiscordStartupStatus reports initial gateway connection retries to doctor. LastError is sanitized
// by the connector before publishing it; NextRetry is empty when no retry is scheduled.
type DiscordStartupStatus struct {
	Attempts  int    `json:"attempts"`
	LastError string `json:"last_error,omitempty"`
	NextRetry string `json:"next_retry,omitempty"`
	// Connecting is true from the start of the first connection attempt until the gateway is READY, so
	// doctor shows "connecting" instead of a disconnect.
	Connecting bool `json:"connecting,omitempty"`
}

// DiscordStartupReporter is optional so existing and test DiscordInfo implementations stay small.
type DiscordStartupReporter interface {
	StartupStatus() DiscordStartupStatus
}

// DiscordSender is implemented by a gateway that can send direct messages (scheduled deliveries).
type DiscordSender interface {
	// SendDM sends a card to a user, with a generated picture when imageID is set. A permanent refusal
	// (the user blocks DMs from the server's members) is reported as ErrDMClosed.
	SendDM(ctx context.Context, userID string, card action.Card, imageID string) error
}

// ErrDMClosed means Discord refused a direct message to this user for good.
var ErrDMClosed = errors.New("discord: direct messages to this user are closed")

// ErrNotDelivered means Discord answered and the message was not posted (a definite failure that may
// be retried). Any other send error leaves the outcome unknown: it may have been posted.
var ErrNotDelivered = errors.New("discord: message not delivered")

// ChannelPoster is implemented by a gateway that can post to a channel on its own (a scheduled
// reminder, an unasked remark). It returns the posted message's ID.
type ChannelPoster interface {
	PostChannel(ctx context.Context, channelID, replyTo, text string, card *action.Card) (string, error)
}

// ChannelEditor is implemented by a gateway that can rewrite one of its own posts (an earthquake
// incident's message as aftershocks come).
type ChannelEditor interface {
	EditChannel(ctx context.Context, channelID, messageID string, card *action.Card) error
}

type GuildInfo struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Members  int    `json:"members,omitempty"`
	JoinedAt string `json:"joined_at,omitempty"`
}

// GuildLeaver is implemented by a gateway that can take the bot out of a server.
type GuildLeaver interface {
	LeaveGuild(ctx context.Context, guildID string) error
}

type ChannelInfo struct {
	ID      string `json:"id"`
	GuildID string `json:"guild_id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
}

// Service is the single Application entry used by CLI, GUI, and the Discord connector.
type Service struct {
	DataRoot string
	Clock    clock.Clock
	I18n     *i18n.Catalog
	Store    *storage.Store
	CfgPath  string
	WebBuild bool // false when compiled with noweb
	Log      *slog.Logger
	Discord  DiscordInfo // nil until the daemon attaches a gateway
	// Daemon is set by masamongd: restart/stop requests have a reader only in the daemon process.
	Daemon bool

	// cfg is the live configuration. Discord handlers, console requests and background workers read
	// it concurrently while an operator applies a change, so it is swapped whole, never edited in place.
	cfg atomic.Pointer[configSnapshot]

	pendingMsgs pendingMessageCount // the console's count of messages waiting for a vector (pending_count.go)
	// cfgMu serializes config writers so two changes based on the same revision cannot both commit.
	cfgMu             sync.Mutex
	mu                sync.RWMutex
	blocksMu          sync.Mutex
	embedWidthAliases embedWidthAliasCache
	// indexQueueMu orders reads and writes of the recorded index queue (indexQueueKey)
	indexQueueMu sync.Mutex
	turns        turnGate     // chat turn concurrency (turns.go)
	imgq         imageQueue   // pictures asked for in chat, drawn in the background (imagequeue.go)
	imgDrops     atomic.Int64 // jobs of imgq dropped for policy, deletion or shutdown (tests wait on it)
	// jobCtx bounds background jobs to the daemon run (ResumeJobs sets it); nil = context.Background.
	jobCtx         context.Context
	jobsWG         sync.WaitGroup
	watchers       map[chan Event]struct{}
	practice       bool            // the console's practice instance: offline models, no Discord, no OS-level actions
	kv             kvCache         // recent admin_state values (see kvGet)
	peopleScans    peopleScanCache // people.servers groups, reused for a minute
	memDoctorCache memDoctorCache  // memoryDoctor's answer, reused for a few seconds
	residency      residencyCache  // readMemoryResidency's numbers, reused like memDoctorCache
	// noticeFetchPrivate lets tests read notice boards from a local server (never set in production)
	noticeFetchPrivate bool
	// discordAPI replaces discord.com/api/v10 for discord.check in tests ("" = Discord)
	discordAPI string
	// referenceURL replaces the public reference price list in tests ("" = hosts.json's)
	referenceURL string
	part         partEngine  // joining conversations unasked (participation.go)
	live         liveWindow  // the channels' last hours in memory (live.go)
	follow       followState // follow-up windows after a reply (followup.go)
	threads      threadState // threads and thread offers (threads.go)
	banter       banterState // playful mentions answered back (banter.go)
	toolsRefused sync.Map    // models that refused a request with tools: key → time of the refusal (toolloop.go)
	toolsTaken   sync.Map    // models that answered a request with tools in this process (toolloop.go)
	// visionRefused remembers the models that refused pictures ("<provider>|<model>" → when, vision.go)
	visionRefused sync.Map
	// modelBreaker rests models that keep failing so their fallbacks answer at once (failover.go);
	// shared by every adapter, which are built per call.
	modelBreakerOnce sync.Once
	modelBreaker     *openai.Breaker
	// visionCooling remembers the vision-role models whose last call timed out or failed on the wire
	// ("<provider>|<model>" → when): pictures skip them for visionCooldown (vision.go)
	visionCooling sync.Map
	visionAd      visionAdapterCache // the vision role's adapter, kept between turns (vision.go)
	// visionLocal lets tests serve pictures from a local server (never set in production)
	visionLocal    bool
	toolFaults     sync.Map       // test-injected tool outages (toolloop.go; never set in production)
	style          styleCache     // approved room style notes (roomstyle.go)
	serverLog      serverLogState // posts to the servers' log channels (serverlog.go)
	lifeMu         sync.Mutex
	lifeCtx        context.Context
	lifeCancel     context.CancelFunc
	closing        bool
	workWG         sync.WaitGroup
	ledger         ledgerWriter // the call ledgers' writer (ledger.go)
	memoryLedgerMu sync.Mutex
	memoryLedger   map[string]map[string]llmCall // job id → role → aggregated memory-model calls
	closeOnce      sync.Once
	closeErr       error
	eventWorker    atomic.Bool
	backupRunning  atomic.Bool
	uptimeMu       sync.Mutex
	disaster       disasterMem
	uptimeWritten  time.Time
	privacyMu      sync.Mutex
	// cfgProblems are settings of the committed configuration that were switched off at load because
	// this release refuses them (config.ParseCommitted); doctor lists them until the operator fixes them.
	cfgProblems atomic.Pointer[[]config.Problem]
	commandMenu atomic.Pointer[[]CommandHint] // the Discord connector's commands (command_menu.go)
	credit      creditCache                   // OpenRouter keys' own spending limits (credit.go)
	// leftovers are files of a finished restore or an interrupted backup that could not be removed
	// (CleanDataLeftovers); doctor lists them and the scheduler retries.
	leftovers atomic.Pointer[[]string]
	// redaction is the worker that erases copies of changed messages (evidence_index.go)
	redaction redactionState
}

// configSnapshot is one committed configuration with the provider registry built from it.
type configSnapshot struct {
	doc config.Document
	rev int
	reg *Registry
}

func (s *Service) snapshot() *configSnapshot {
	if c := s.cfg.Load(); c != nil {
		return c
	}
	return &configSnapshot{}
}

// Doc is the current configuration document. The returned value is a snapshot: treat its maps as
// read-only and call it once per operation when several fields must agree with each other.
func (s *Service) Doc() config.Document { return s.snapshot().doc }

// Revision is the committed config revision (0 before setup).
func (s *Service) Revision() int { return s.snapshot().rev }

// Registry is the provider registry built from the current configuration.
func (s *Service) Registry() *Registry { return s.snapshot().reg }

// Event is a change notification for SSE and TUI watchers (17장 §6).
type Event struct {
	ID       int64          `json:"id"`
	Kind     string         `json:"kind"` // job | config | provider | discord
	Ref      string         `json:"ref,omitempty"`
	Revision int            `json:"revision,omitempty"`
	Data     map[string]any `json:"data,omitempty"`
	At       time.Time      `json:"at"`
}

func Open(dataRoot string, clk clock.Clock, webBuild bool) (*Service, error) {
	if clk == nil {
		clk = clock.Real{}
	}
	if dataRoot == "" {
		return nil, fmt.Errorf("data-root required")
	}
	s := &Service{
		DataRoot: dataRoot,
		Clock:    clk,
		I18n:     i18n.New(),
		CfgPath:  filepath.Join(dataRoot, "config.yaml"),
		WebBuild: webBuild,
		Log:      slog.Default(),
		watchers: map[chan Event]struct{}{},
	}
	s.cfg.Store(&configSnapshot{doc: config.Document{}, reg: buildRegistry(config.Default("", dataRoot), s.Log, s.Clock.Now)})
	opened := false
	defer func() {
		if !opened {
			_ = s.Close()
		}
	}()
	// Offline commands must not open/migrate a half-restored or half-erased database.
	if maintenanceNeeded(dataRoot) {
		opened = true
		return s, nil
	}
	opt, err := storage.OptionsFromEnv(dataRoot, storage.Settings{})
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(dataRoot); err == nil {
		// folders made by older versions (or by hand) were readable by every account on the computer
		if err := fsutil.PrivateDir(dataRoot); err != nil {
			s.Log.Warn("data folder permissions not tightened", "err", err)
		}
	}
	shouldOpen := false
	switch opt.Backend {
	case storage.BackendTiDB:
		shouldOpen = opt.DSN != ""
	default:
		if _, err := os.Stat(opt.SQLitePath); err == nil {
			shouldOpen = true
		}
	}
	if shouldOpen {
		store, err := storage.OpenOptions(opt)
		if err != nil {
			return nil, err
		}
		s.Store = store
		if err := migrateMemory(store); err != nil {
			return nil, err
		}
		if err := s.reloadConfig(); err != nil {
			return nil, err
		}
	}
	if s.Store != nil {
		if err := s.Store.ResumeEvents(); err != nil {
			return nil, err
		}
		s.startEventWorker()
	}
	opened = true
	return s, nil
}

func (s *Service) Close() error {
	s.closeOnce.Do(func() {
		s.lifeMu.Lock()
		s.closing = true
		if s.lifeCancel != nil {
			s.lifeCancel()
		}
		s.lifeMu.Unlock()
		s.pendingMsgs.close()
		if v, ok := resyncQueues.LoadAndDelete(s); ok {
			q := v.(*resyncQueue)
			q.mu.Lock()
			if q.timer != nil {
				q.timer.Stop()
			}
			q.mu.Unlock()
		}
		s.workWG.Wait()
		s.part.mu.Lock()
		for _, c := range s.part.chans {
			if c.pending != nil {
				c.pending.timer.Stop()
			}
		}
		s.part.chans = nil
		s.part.mu.Unlock()
		if v, ok := memStates.LoadAndDelete(s); ok {
			st := v.(*memoryState)
			st.mu.Lock()
			if st.eng != nil {
				st.eng.Close()
				st.eng = nil
			}
			st.mu.Unlock()
		}
		if v, ok := rings.LoadAndDelete(s); ok {
			r := v.(*logRing)
			r.mu.Lock()
			r.closed = true
			r.entries = nil
			r.mu.Unlock()
		}
		for _, m := range []*sync.Map{&memoryOptOuts, &lifecycles, &priceStates, &summaryRevs, &toolUsages} {
			m.Delete(s)
		}
		s.mu.Lock()
		for ch := range s.watchers {
			close(ch)
		}
		s.watchers = nil
		s.mu.Unlock()
		if s.Store != nil {
			// all work has stopped: write the job aggregates and ledger rows that arrived after the
			// ledger writer did, while the database is still open
			s.flushAllMemoryLLMCalls()
			s.ledger.drain(s)
			adminReady.Delete(s.Store)
			s.closeErr = s.Store.Close()
		}
	})
	return s.closeErr
}

// beginWork registers before shutdown can start waiting; its context follows both the caller and
// the service lifetime. A worker scheduled after Close has begun must not touch service state.
func (s *Service) beginWork(parent context.Context) (context.Context, func(), bool) {
	s.lifeMu.Lock()
	defer s.lifeMu.Unlock()
	if s.closing {
		return nil, nil, false
	}
	if s.lifeCtx == nil {
		s.lifeCtx, s.lifeCancel = context.WithCancel(context.Background())
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(s.lifeCtx, cancel)
	s.workWG.Add(1)
	return ctx, func() { stop(); cancel(); s.workWG.Done() }, true
}

func (s *Service) ownedGo(name string, fn func(context.Context)) {
	ctx, done, ok := s.beginWork(context.Background())
	if !ok {
		return
	}
	// the logger for a crash report is read here, before the goroutine starts; the worker's own logging
	// still reads s.Log, so set it before starting workers (tests: setupSvcLog)
	log := s.Log
	go func() { defer done(); defer crash.Recover(log, name); fn(ctx) }()
}

func (s *Service) ownedGoContext(name string, parent context.Context, fn func(context.Context)) bool {
	ctx, done, ok := s.beginWork(parent)
	if !ok {
		return false
	}
	log := s.Log
	go func() { defer done(); defer crash.Recover(log, name); fn(ctx) }()
	return true
}

func (s *Service) ownedSingleFlight(parent context.Context, active *atomic.Bool, name string, fn func(context.Context)) bool {
	if !active.CompareAndSwap(false, true) {
		return false
	}
	if !s.ownedGoContext(name, parent, func(ctx context.Context) {
		defer active.Store(false)
		fn(ctx)
	}) {
		active.Store(false)
		return false
	}
	return true
}

// Ready reports whether setup has run (a committed config revision exists).
func (s *Service) Ready() bool { return s.Store != nil && s.Revision() > 0 }

func (s *Service) reloadConfig() error {
	if s.Store == nil {
		return nil
	}
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	st, err := loadCASState(s.Store, s.CfgPath)
	if err != nil {
		return err
	}
	if commit, discard := config.Recover(st); commit != nil || discard != nil {
		if discard != nil {
			_ = persistRevision(s.Store, *discard)
		}
		if commit != nil {
			_ = persistRevision(s.Store, *commit)
			st.Current = commit
			st.Prepared = nil
		}
	}
	if st.Current != nil {
		doc, problems, err := config.ParseCommitted(st.Current.Content)
		if err != nil {
			return err
		}
		s.setDoc(doc, st.Current.Revision)
		s.cfgProblems.Store(&problems)
		for _, p := range problems {
			s.Log.Error("config: a setting is no longer accepted, so it is switched off; the rest keeps running",
				"setting", p.Path, "switched_off", p.Off, "reason", p.Reason, "fix", "change the address in the console (Settings) or config.yaml, then enable it again")
		}
	}
	return nil
}

func (s *Service) setDoc(doc config.Document, rev int) {
	if s.practice {
		doc.Tools = practiceToolDefaults(doc.Tools)
	}
	regDoc := doc
	if s.practice {
		regDoc = offlineProviders(doc)
	}
	// Provider governors outlive this registry: requests on the old snapshot still hold their slots.
	reg := buildRegistry(regDoc, s.Log, s.Clock.Now)
	if !s.practice {
		reg.addBuiltinOpenMeteo() // worldwide weather needs no key
		reg.addBuiltinReader()    // reading a page needs no key either
	}
	// the registry is finished before it is published: readers never see a half-built one
	s.persistQuota(reg.Quota, doc)
	s.priceAdapters(reg, doc)
	s.cfg.Store(&configSnapshot{doc: doc, rev: rev, reg: reg})
	s.cfgProblems.Store(nil)
	// new settings may name another model or host: ask about pictures again
	s.visionRefused.Clear()
	s.visionCooling.Clear()
	s.visionAd.drop()
	if !s.practice { // the practice copy runs inside the real daemon and must not change its log level
		s.applyLogLevel(doc)
	}
}

// applyLogLevel sets the process log level from instance.log_level; MASAMONG_LOG_LEVEL, when set,
// wins so an operator can turn on debug for one run without editing the configuration. A bad value is
// reported and the level stays as it was: logging settings must never stop the bot.
func (s *Service) applyLogLevel(doc config.Document) {
	name, from := doc.Instance.LogLevel, "instance.log_level"
	if env := os.Getenv("MASAMONG_LOG_LEVEL"); env != "" {
		name, from = env, "MASAMONG_LOG_LEVEL"
	}
	if err := observability.SetLevel(name); err != nil {
		s.Log.Warn("log level not changed", "from", from, "err", err)
	}
}

// persistQuota counts provider calls per instance-zone day in the database, so neither a restart nor a
// config change (which rebuilds the registry) resets a provider's daily allowance.
func (s *Service) persistQuota(q *providers.Quota, doc config.Document) {
	zone := zoneOf(doc)
	q.Day = func(t time.Time) string { return t.In(zone).Format("2006-01-02") }
	u := s.toolUsage()
	if u == nil {
		return
	}
	q.Store = quotaStore{u: u, now: s.Clock.Now}
	q.OnError = func(err error) { s.Log.Warn("provider quota not saved", "err", err) }
}

type quotaStore struct {
	u   *tools.Usage
	now func() time.Time
}

func (q quotaStore) Counts(day string) (map[string]int, error) { return q.u.Counts(day, "provider") }
func (q quotaStore) Bump(day, provider string) error {
	return q.u.Bump(day, "provider", provider, q.now())
}

// offlineProviders keeps only the providers that never leave the process. The practice instance uses
// it so a provider added to the practice config cannot spend real keys or reach the network.
func offlineProviders(doc config.Document) config.Document {
	kept := map[string]config.Provider{}
	for id, p := range doc.Providers {
		if p.Kind == config.KindPractice || p.Kind == config.KindFake {
			kept[id] = p
		}
	}
	doc.Providers = kept
	return doc
}

// practiceBlocked are Actions that reach outside the practice instance (the OS, the real database, the
// real admins, real keys).
var practiceBlocked = map[string]bool{
	"storage.migrate": true, "backup.restore": true, "runtime.restart": true, "runtime.stop": true,
	"runtime.autostart.set": true, "providers.credentials": true, "setup.resume": true, "alerts.test": true,
	"serverlog.test": true, "updates.apply": true, "instance.reset": true,
}

// Execute is the one entry point. Transports translate input/output; nothing here knows about HTTP or Discord.
// Every state-changing Action lands in the change log (audit.list) with its outcome.
func (s *Service) Execute(ctx context.Context, req action.Request) action.Result {
	ctx, done, ok := s.beginWork(ctx)
	if !ok {
		return action.Fail(req, errcode.IO, "generic.error", "service closed", nil)
	}
	defer done()
	if s.Store == nil && maintenanceNeeded(s.DataRoot) {
		if req.Action == "runtime.doctor" || req.Action == "runtime.status" {
			return action.OK(req, MaintenanceStatus(s.DataRoot))
		}
		return action.Fail(req, errcode.IO, "generic.error", "maintenance recovery required; run masamongd --maintenance doctor", MaintenanceStatus(s.DataRoot))
	}
	if s.Store != nil && s.Store.HasPendingEvents("source-change") {
		group, _, _ := strings.Cut(req.Action, ".")
		switch group {
		case "messages", "memory", "chat", "trace", "turns", "lab", "eval", "summaries", "backup":
			res := action.Fail(req, errcode.IO, "generic.error", s.msg(req, "generic.error"), nil)
			res.Error.Retryable = true
			return res
		}
	}
	before := s.Revision()
	res := s.execute(ctx, req)
	if s.Store != nil {
		s.startEventWorker() // setup creates the database after Open
	}
	if desc, ok := catalog.Lookup(req.Action); ok {
		s.audit(req, desc, res, before)
	}
	return res
}

func (s *Service) execute(ctx context.Context, req action.Request) action.Result {
	if req.Action == "" {
		return action.Fail(req, errcode.ActionUnknown, "action.unknown", s.msg(req, "action.unknown"), nil)
	}
	desc, ok := catalog.Lookup(req.Action)
	if !ok {
		// Operator-defined tools (config tools.<id>.http) are Actions without a catalog entry.
		if r, isTool := tools.Find(s.toolRegistry(), req.Action); isTool && r.Kind == "http" {
			if s.Store == nil {
				return action.Fail(req, errcode.NotSetup, "generic.error", "run setup first", nil)
			}
			if s.practice {
				return action.Fail(req, errcode.InvalidInput, "practice.blocked", "not available in practice mode: HTTP tools call real servers", map[string]any{"action": req.Action})
			}
			req, refused := s.gateTool(req)
			if refused != nil {
				return *refused
			}
			if ctx == nil {
				ctx = context.Background()
			}
			return s.runHTTPTool(ctx, req, r)
		}
		return action.Fail(req, errcode.ActionUnknown, "action.unknown", s.msg(req, "action.unknown"), map[string]any{"action": req.Action})
	}
	if desc.Availability == catalog.Deferred {
		return action.Fail(req, errcode.ActionUnavailable, "action.unavailable", s.msg(req, "action.unavailable"), map[string]any{
			"action": req.Action, "reason": desc.DeferredReason,
		})
	}
	if s.practice && practiceBlocked[req.Action] {
		return action.Fail(req, errcode.InvalidInput, "practice.blocked", "not available in practice mode: this changes the real installation", map[string]any{"action": req.Action})
	}
	// the service must be stoppable and restartable before setup has created the database
	if s.Store == nil {
		switch req.Action {
		case "runtime.restart", "runtime.stop":
			return s.runtimeRestart(req)
		case "runtime.autostart", "runtime.autostart.set":
			return s.autostartAction(req)
		}
	}
	if req.Action != "setup.resume" && req.Action != "help.version" && s.Store == nil {
		return action.Fail(req, errcode.NotSetup, "generic.error", "run setup first", nil)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	req = s.applyUserDefaults(req)
	req, refused := s.gateTool(req)
	if refused != nil {
		return *refused
	}
	if refused := s.gateDiscord(req); refused != nil {
		return *refused
	}
	switch req.Action {
	case "setup.resume":
		return s.setup(ctx, req)
	case "runtime.doctor":
		return s.doctor(ctx, req)
	case "config.show":
		return s.configShow(req)
	case "config.diff", "config.validate":
		return s.configDiff(req)
	case "config.apply":
		return s.configApply(req)
	case "config.rollback":
		return s.configRollback(req)
	case "jobs.list", "jobs.get", "jobs.cancel", "jobs.retry", "jobs.watch":
		return s.jobsAction(req)
	case "help.version":
		return s.help(req)
	case "locales.list":
		return s.locales(req)
	case "weather.query":
		return s.weather(ctx, req)
	case "disasters.query":
		return s.disasters(ctx, req)
	case "fortune.query":
		return s.fortuneQuery(ctx, req)
	case "fortune.subscribe":
		return s.fortuneSubscribe(req)
	case "notices.status":
		return s.noticesStatus(ctx, req)
	case "notices.subscribe":
		return s.noticesSubscribe(ctx, req)
	case "transfer.recent":
		return s.transferRecent(req)
	case "migration.apply":
		return s.migrationApply(req)
	case "polls.create", "polls.vote", "polls.close", "polls.result":
		return s.polls(req)
	case "images.generate":
		return s.images(ctx, req)
	case "finance.quote", "finance.fx":
		return s.finance(ctx, req)
	case "search.web":
		return s.search(ctx, req)
	case "web.read":
		return s.webRead(ctx, req)
	case "places.search":
		return s.placesSearch(ctx, req)
	case "activity.ranking":
		return s.activityRanking(req)
	case "providers.list", "providers.test", "providers.credentials", "providers.add", "providers.remove", "models.list", "models.select", "models.recommend", "models.embed_width":
		return s.providers(ctx, req)
	case "discord.check":
		return s.discordCheck(ctx, req)
	case "disasters.alerts":
		return s.disasterAlertsStatus(req)
	case "channels.list", "channels.configure", "channels.invite", "channels.leave", "channels.attachments.add", "channels.attachments.remove", "channels.ops_log", "channels.view.show", "channels.view.attach", "channels.view.detach":
		return s.channels(req)
	case "memory.blocks.list", "memory.blocks.show", "memory.blocks.prepare", "memory.blocks.update", "memory.blocks.delete":
		return s.memoryBlocks(ctx, req)
	case "eval.run", "eval.runs":
		return s.evalAction(ctx, req)
	case "memory.review", "memory.reviews":
		return s.memoryReviewAction(req)
	case "memory.episodes.list", "memory.episodes.show", "memory.episodes.forget", "memory.episodes.extract", "memory.state.show":
		return s.roomMemoryAction(ctx, req)
	case "memory.mine.show", "memory.mine.forget":
		return s.mineAction(ctx, req)
	case "people.list", "people.merge":
		return s.peopleAction(req)
	case "people.servers":
		return s.peopleServers(req)
	case "imports.preview":
		return s.importsPreview(ctx, req)
	case "messages.search", "memory.search", "imports.start", "memory.reindex", "chat.respond":
		return s.memoryActions(ctx, req)
	case "memory.backfill":
		return s.memoryBackfill(ctx, req)
	case "summaries.channel":
		if !boolVal(inputMap(req), "saved") {
			// A summary reuses a stable holder ID. Finish older source erasures before rebuilding
			// it, so a delayed job cannot erase fresh text that already reflects that source edit.
			wait, cancel := context.WithTimeout(ctx, 30*time.Second)
			s.DrainRedactions(wait)
			cancel()
			if pending, err := s.redactionPending(); err != nil || pending {
				res := action.Fail(req, errcode.IO, "generic.error", s.msg(req, "generic.error"), nil)
				res.Error.Retryable = true
				return res
			}
		}
		return s.channelSummary(ctx, req)
	case "events.timeline":
		return s.eventsTimeline(ctx, req)
	case "personas.list", "personas.apply":
		return s.personas(req)
	case "tools.list", "tools.test", "tools.configure", "agent.simulate", "trace.show":
		return s.agent(ctx, req)
	case "stats.query", "reports.create":
		return s.stats(req)
	case "privacy.consent":
		return s.privacyConsentContext(ctx, req)
	case "preferences.set":
		return s.preferencesAction(req)
	case "access.policy":
		return s.accessPolicy(req)
	case "users.list":
		return s.usersList(req)
	case "reminders.propose":
		return s.remindersPropose(req)
	case "reminders.update":
		return s.remindersUpdate(req)
	case "participation.mode":
		return s.participationMode(req)
	case "conversation.log":
		return s.conversationLog(req)
	case "room.style.show", "room.style.build", "room.style.preview", "room.style.decide":
		return s.roomStyleAction(ctx, req)
	case "usage.budgets":
		return s.usage(req)
	case "usage.period":
		return s.usagePeriod(req)
	case "usage.reset":
		return s.usageReset(req)
	case "storage.status", "storage.migrate":
		return s.storageActions(ctx, req)
	case "admin.prefs", "admin.prefs.set", "runtime.pause", "runtime.status", "runtime.restart", "runtime.stop", "dashboard.overview",
		"turns.list", "turns.get", "audit.list", "stats.series", "lab.reviews", "lab.review", "config.document", "config.patch",
		"config.snapshots", "config.snapshot", "config.snapshot.delete", "config.snapshot.restore", "config.changes":
		return s.adminActions(ctx, req)
	case "backup.list", "backup.create", "backup.verify", "backup.restoreDryRun", "backup.restore", "backup.delete":
		return s.backup(ctx, req)
	case "schedules.list", "schedules.run":
		return s.schedulesAction(ctx, req)
	case "channels.permissions":
		return s.channelsPermissions(req)
	case "runtime.autostart", "runtime.autostart.set":
		return s.autostartAction(req)
	case "alerts.status", "alerts.test":
		return s.alertsAction(ctx, req)
	case "serverlog.test":
		return s.serverLogTest(ctx, req)
	case "feedback.summary":
		return s.feedbackAction(req)
	case "instance.reset":
		return s.instanceReset(req)
	case "updates.status", "updates.check", "updates.apply":
		return s.updatesAction(ctx, req)
	case "runtime.history":
		if _, err := s.adminDB(); err != nil {
			return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
		}
		return s.runtimeHistory(req)
	case "runtime.drain":
		in := inputMap(req)
		draining := true
		if _, ok := in["draining"]; ok {
			draining = boolVal(in, "draining")
		} else if _, ok := in["enabled"]; ok {
			draining = boolVal(in, "enabled")
		}
		pauseReq := req
		pauseReq.Action = "runtime.pause"
		pauseReq.Input, _ = json.Marshal(map[string]any{"scope": "all", "paused": draining, "reason": "runtime.drain"})
		res := s.runtimePause(pauseReq)
		res.Action = req.Action
		if res.OK {
			res.Output["draining"] = draining
			if draining {
				res.Output["effect"] = "new chat replies and commands are paused"
			} else {
				res.Output["effect"] = "chat replies and commands are resumed"
			}
		}
		return res
	case "runtime.logs":
		return s.runtimeLogs(req)
	default:
		return action.Fail(req, errcode.ActionUnknown, "action.unknown", s.msg(req, "action.unknown"), nil)
	}
}

// help is help.version plus what the bot can do right now: tools usable in conversation and the
// Discord prefix words, in the request's language (the planner uses it for "what can you do?"). A
// tool that cannot answer yet (no key, no model) is left out of both lists, so "what can you do?"
// never promises a quote the bot has no service for.
func (s *Service) help(req action.Request) action.Result {
	out := map[string]any{"version": Version, "name": "masamong", "web_build": s.WebBuild}
	p := lang.For(req.Locale, s.Doc().I18n.DefaultLocale)
	var chatTools, commands []any
	ready, byCommand, memo := map[string]bool{}, map[string]bool{}, map[string]string{}
	var asCommand *action.Invoker
	if req.Invoker != nil {
		c := *req.Invoker
		c.Via = "command"
		asCommand = &c
	}
	for _, r := range s.toolRegistry() {
		if !r.Enabled || r.ID == "chat.respond" || r.ID == "help.version" {
			continue
		}
		if ok, _ := s.toolReady(r, memo); !ok {
			continue
		}
		ready[r.ID] = true
		if ok, _ := s.toolAllowedHere(r, asCommand); ok && r.Commands {
			byCommand[r.ID] = true
		}
		if ok, _ := s.toolAllowedHere(r, req.Invoker); !ok {
			continue
		}
		if r.ID == "memory.search" && !s.memoryUsable(req.Invoker) {
			continue
		}
		if r.Chat {
			chatTools = append(chatTools, map[string]any{"id": r.ID, "description": toolInfo(r, p, s.Doc().Tools[r.ID].Params).Description})
		}
	}
	for _, c := range s.ToolCommands() {
		if len(c.Aliases) > 0 && ready[c.Tool] {
			commands = append(commands, map[string]any{"tool": c.Tool, "aliases": c.Aliases})
		}
	}
	out["tools"], out["command_prefix"] = chatTools, s.Doc().Discord.CommandPrefix
	if len(commands) > 0 {
		out["command_aliases"] = commands
	}
	if only := s.commandOnly(p.Tag, s.toolRegistry(), byCommand); len(only) > 0 {
		out["command_only"] = only
	}
	return action.OK(req, out)
}

func (s *Service) locales(req action.Request) action.Result {
	cov := map[string]any{}
	for _, l := range s.I18n.Locales() {
		cov[l] = map[string]any{"missing": len(s.I18n.MissingKeys(l))}
	}
	return action.OK(req, map[string]any{"bundled": s.Doc().I18n.Bundled, "loaded": s.I18n.Locales(), "fallback": "en", "coverage": cov})
}

func (s *Service) usage(req action.Request) action.Result {
	used := map[string]any{}
	for _, st := range s.Registry().Statuses() {
		used[st.ID] = map[string]any{"used_today": st.UsedToday, "daily_limit": st.DailyLimit}
	}
	out := map[string]any{
		"monthly_budget_usd": s.Doc().Limits.MonthlyBudgetUSD,
		"spent_usd":          "unknown", // never report 0 as fact when the ledger is unavailable
		"provider_calls":     used,
	}
	if reset, ok := s.activeSpendReset(s.Clock.Now()); ok {
		out["reset"] = spendResetInfo(reset)
	}
	if u := s.toolUsage(); u != nil {
		day, _ := s.toolDay()
		if calls, err := u.Today(day); err == nil {
			out["tool_calls_today"] = calls
		}
	}
	if credit := s.providerCredit(); len(credit) > 0 {
		out["key_credit"] = credit
	}
	if by, total, ok := s.monthSpend(); ok {
		out["spent_usd"] = fmt.Sprintf("%.4f", total)
		out["spent_by_kind_usd"] = by
		out["spent_basis"] = "provider usage.cost when reported, else per-call estimate; this month (UTC)"
	}
	return action.OK(req, out)
}

// Msg exposes the catalog to transports for chrome text (nav labels, headings).
func (s *Service) Msg(locale, key string, args ...any) string { return s.I18n.T(locale, key, args...) }

func (s *Service) HasMessage(key string) bool { return s.I18n != nil && s.I18n.Has(key) }

func (s *Service) msg(req action.Request, key string, args ...any) string {
	return s.I18n.T(req.Locale, key, args...)
}

// ---------------------------------------------------------------------------
// Watchers (SSE / TUI)
// ---------------------------------------------------------------------------

var eventSeq int64

func (s *Service) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 32)
	s.mu.Lock()
	if s.watchers == nil {
		close(ch)
		s.mu.Unlock()
		return ch, func() {}
	}
	s.watchers[ch] = struct{}{}
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		delete(s.watchers, ch)
		s.mu.Unlock()
	}
}

func (s *Service) emit(kind, ref string, data map[string]any) {
	s.mu.Lock()
	eventSeq++
	ev := Event{ID: eventSeq, Kind: kind, Ref: ref, Revision: s.Revision(), Data: data, At: s.Clock.Now()}
	for ch := range s.watchers {
		select {
		case ch <- ev:
		default: // slow consumer: drop; SSE clients resync from Last-Event-ID via a snapshot
		}
	}
	s.mu.Unlock()
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func newID(prefix string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

func envLooksLoaded() bool {
	// Explicitly do not read legacy variables for bootstrap. Presence is reported as ignored.
	for _, k := range []string{"DISCORD_TOKEN", "DISCORD_BOT_TOKEN", "OPENROUTER_API_KEY", "KMA_API_KEY"} {
		if os.Getenv(k) != "" {
			return true
		}
	}
	return false
}

func redactedEnvNote() map[string]any {
	return map[string]any{
		"legacy_env_present": envLooksLoaded(),
		"legacy_env_loaded":  false,
	}
}

func inputMap(req action.Request) map[string]any {
	m := map[string]any{}
	if len(req.Input) > 0 {
		// UseNumber: Discord snowflakes (18–19 digits) must not pass through float64.
		dec := json.NewDecoder(bytes.NewReader(req.Input))
		dec.UseNumber()
		_ = dec.Decode(&m)
	}
	return m
}

// strVal reads a string field. Numbers are accepted too: the CLI coerces numeric-looking flags
// (--guild_id 1234…) and the same Action must behave like the GUI, which posts strings.
func strVal(m map[string]any, k, fallback string) string {
	if v, ok := m[k]; ok {
		switch s := v.(type) {
		case string:
			if strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		case json.Number:
			return s.String()
		case int:
			return strconv.Itoa(s)
		case int64:
			return strconv.FormatInt(s, 10)
		case float64:
			return strconv.FormatFloat(s, 'f', -1, 64)
		}
	}
	return fallback
}

func intVal(m map[string]any, k string, fallback int) int {
	if v, ok := m[k]; ok {
		switch n := v.(type) {
		case float64:
			return int(n)
		case int:
			return n
		case json.Number:
			i, _ := n.Int64()
			return int(i)
		case string:
			var i int
			if _, err := fmt.Sscanf(strings.TrimSpace(n), "%d", &i); err == nil {
				return i
			}
		}
	}
	return fallback
}

func boolVal(m map[string]any, k string) bool {
	v, ok := m[k]
	if !ok {
		return false
	}
	switch b := v.(type) {
	case bool:
		return b
	case string:
		return b == "true" || b == "on" || b == "1"
	}
	return false
}

func strSlice(m map[string]any, k string) []string {
	v, ok := m[k]
	if !ok {
		return nil
	}
	switch arr := v.(type) {
	case []any:
		out := make([]string, 0, len(arr))
		for _, x := range arr {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return arr
	case string:
		var out []string
		for _, p := range strings.Split(arr, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	return nil
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}
