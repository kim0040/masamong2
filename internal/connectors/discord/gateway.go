package discord

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/bwmarrin/discordgo"
	"github.com/gorilla/websocket"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/chat"
	"masamong/internal/config"
	"masamong/internal/conversation"
	"masamong/internal/crash"
	"masamong/internal/errclass"
	"masamong/internal/memory/engine"
	"masamong/internal/providers"
	"masamong/internal/vision"
)

// Gateway is the live Discord connector (09). It is the single sender for this instance: every outbound
// message goes through this session, with mentions disabled, and every command is an Application Action.
type Gateway struct {
	svc                        *application.Service
	cfg                        config.Discord
	log                        *slog.Logger
	sess                       *discordgo.Session
	connected                  atomic.Bool
	botID                      atomic.Pointer[string]
	dmMembers                  sync.Map // guild:user -> short-lived membership result
	dmSweptAt                  atomic.Int64
	followIDs                  sync.Map        // message IDs the follow-up window accepted, until their turn starts
	pictureFollowIDs           sync.Map        // picture-only message ID → the words of the call it continues (lookback.go)
	pictures                   recentPictures  // each channel's last few messages, for pictures sent apart from the words (lookback.go)
	seen                       seenSet         // message IDs handled lately: a replayed MESSAGE_CREATE is dropped (inbound.go)
	turns                      turnRegistry    // replies in flight, by channel and person, for a stop word (inbound.go)
	channelKinds               sync.Map        // channel ID → *channelKind: is it a thread (threads.go)
	kindSweptAt                atomic.Int64    // unix nanoseconds of the last channelKinds sweep
	workCtx                    context.Context // replies and commands; cancelled after the shutdown grace
	cancelWork                 context.CancelFunc
	stopping                   atomic.Bool
	more                       moreStore       // the rest of trimmed replies (shape.go)
	interims                   interimRegistry // the bot's "still looking" lines: never captured, their delete never reconciled (interim.go)
	interimHook                func(*interim)  // tests: sees each turn's line as it is made
	botName                    atomic.Pointer[string]
	mu                         sync.Mutex
	lastSync                   string
	startupMu                  sync.RWMutex
	startup                    application.DiscordStartupStatus
	open                       func() error
	token                      string
	retryWait                  func(context.Context, time.Duration) bool
	retryJitter                func(time.Duration) time.Duration
	messageEventApply          func(messageReconciliation) error
	cancelPictures             func(channelID string, messageIDs []string) // tests; nil means the service's queue
	reactForTest               func(context.Context, string, string, string) error
	reconciliationWait         func(context.Context, time.Duration) bool
	reconciliationMu           sync.Mutex
	reconciliationRefreshMu    sync.Mutex                                // orders snapshots without holding up queue writes during legacy scans
	legacyQuarantineCache      map[string]legacyQuarantineClassification // guarded by reconciliationRefreshMu
	reconciliationRemove       func(string) error                        // tests; nil uses durable removal
	reconciliationWrite        func(string, messageReconciliation) error // tests; nil uses atomic writes
	reconciliationRead         func(string) ([]byte, error)              // tests; nil reads the queued file
	legacyReconciliationStep   func(int)                                 // tests; pauses migration before a record is queued
	legacyReconciliationReader func(io.Reader) io.Reader                 // tests; observes legacy scans and injects decoder read failures
	// editOrderMu orders a live edit against the drain's apply of a queued one for the same bot: without
	// it a queued older edit read just before a newer live edit went through could be applied after it
	// and bring back the words the newer edit removed.
	editOrderMu sync.Mutex
	// reconciliationSeq names delete files; reconciliationPending is true while a deferred change may be
	// on disk (so a delete that succeeds knows to look for edits it supersedes); reconciliationWake
	// tells the background drain a delete or an idle queue needs work (nil in tests that drain by hand).
	reconciliationSeq        atomic.Uint64
	reconciliationPending    atomic.Bool
	reconciliationBlocked    atomic.Int64
	reconciliationDeletes    atomic.Bool
	reconciliationLegacy     atomic.Bool
	reconciliationStateReady bool      // guarded by reconciliationMu
	reconciliationBlockedLog time.Time // guarded by reconciliationMu
	reconciliationWake       chan struct{}
	reconciliationBackingOff atomic.Bool
	reconciliationCtx        context.Context
	reconciliationCancel     context.CancelFunc
	reconciliationDone       chan struct{}
	noticeMu                 sync.Mutex
	notices                  map[string]time.Time // "user:channel" and "channel" -> last refusal notice
	// timeout for one Action execution. The reply that follows (the result, or the error card after a
	// timeout) is sent with a fresh context of finalDeliveryTimeout, so a timed-out action still shows
	// its failure.
	ActionTimeout time.Duration
	// ctx is the daemon run: stopping the daemon cancels the work handlers started. inflight counts
	// that work so shutdown can let it finish before the database closes. workMu serializes admission
	// with Wait starting the drain, so no handler can be added after Wait begins.
	ctx      context.Context
	workMu   sync.Mutex
	draining bool
	inflight sync.WaitGroup
	// merge answers a person's split messages as one turn (merge.go)
	merge merger
	// health is the gateway diagnostic state (health.go)
	health gatewayHealth
}

var ErrNoToken = errors.New("discord: token not configured")

const (
	startupRetryInitial = time.Second
	startupRetryMax     = 30 * time.Second
	// A refusal notice ("replies are paused") goes to one person once per cooldown, and to one channel
	// once per channelNoticeCooldown, so a busy channel is never answered by a stream of notices.
	chatNoticeCooldown    = 5 * time.Minute
	channelNoticeCooldown = time.Minute
	// finalDeliveryTimeout bounds the last edit or send of a command, on a context that does not carry
	// the action's own (possibly expired) deadline.
	finalDeliveryTimeout = 10 * time.Second
	// A chat turn gets chatTurnTimeout once it has its place in the channel (a turn that draws a picture
	// takes long on top of planning and answering). The wait for that place is bounded by chatQueueWait:
	// a message that waited longer is stale and is answered with the "catching up" notice. chatTurnCeiling
	// bounds the whole call, whatever happens in between. The wait is 120 s because a normal message
	// can stand behind a picture turn (p90 88 s) or behind a turn that plans and answers (about 105 s)
	// in the same channel, and must not end as the "catching up" notice only for that.
	chatTurnTimeout = application.ChatTurnTimeout
	chatQueueWait   = application.ChatQueueWait
	chatTurnCeiling = application.ChatTurnCeiling
	// interactionAckTimeout: Discord drops an interaction that is not answered within 3 seconds.
	interactionAckTimeout = 2500 * time.Millisecond
)

// Start resolves the token, returns the gateway, and retries transient initial Open failures in the
// background. A missing token is reported as ErrNoToken so the daemon can keep serving everything else.
func Start(ctx context.Context, svc *application.Service, cfg config.Discord, log *slog.Logger) (*Gateway, error) {
	if log == nil {
		log = slog.Default()
	}
	cred, err := providers.Resolve(cfg.TokenRef)
	if err != nil {
		return nil, err
	}
	if !cred.Present() {
		return nil, ErrNoToken
	}
	sess, err := discordgo.New("Bot " + cred.Value())
	if err != nil {
		return nil, err
	}
	g := &Gateway{svc: svc, cfg: cfg, log: log, sess: sess, ActionTimeout: 40 * time.Second, ctx: ctx,
		open: sess.Open, token: cred.Value(), retryWait: waitContext, retryJitter: jittered, notices: map[string]time.Time{}}
	g.workCtx, g.cancelWork = context.WithCancel(context.WithoutCancel(ctx))
	// Changes deferred by an earlier run (or an older file format) are applied in the background: the
	// gateway and the console must not wait on a store that may be slow or still failing.
	g.reconciliationPending.Store(true)
	g.reconciliationWake = make(chan struct{}, 1)
	g.startReconciliationDrain(ctx)
	g.merge.fire = g.answerHeld
	g.merge.forget = g.forgetFollow
	// direct messages carry personal commands (fortune, notices, privacy) and scheduled deliveries' replies
	// reactions: the ones people put on the bot's replies are its feedback (feedback.go); not privileged
	sess.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildMessages | discordgo.IntentsDirectMessages | discordgo.IntentsGuildMessageReactions |
		discordgo.IntentGuildMessagePolls // votes on the bot's polls (poll_sync.go); not privileged
	if cfg.MessageContent {
		sess.Identify.Intents |= discordgo.IntentMessageContent
	}
	sess.StateEnabled = true
	// the last messages of each channel stay in the state cache, so a deleted one still has its author
	// and text for the server log (serverlog.go)
	sess.State.MaxMessageCount = stateMessages
	sess.AddHandler(g.onGatewayEvent)
	sess.AddHandler(g.onReady)
	sess.AddHandler(g.onDisconnect)
	sess.AddHandler(g.onResumed)
	sess.AddHandler(g.onInteraction)
	sess.AddHandler(g.onMessage)
	sess.AddHandler(g.onMessageUpdate)
	sess.AddHandler(g.onMessageDelete)
	sess.AddHandler(g.onMessageDeleteBulk)
	sess.AddHandler(g.onThreadCreate)
	sess.AddHandler(g.onThreadUpdate)
	sess.AddHandler(g.onThreadDelete)
	sess.AddHandler(g.onPollVoteAdd)
	sess.AddHandler(g.onPollVoteRemove)
	sess.AddHandler(g.onReactionAdd)
	sess.AddHandler(g.onReactionRemove)
	svc.Discord = g
	SetCatalog(svc.I18n)
	svc.SetCommandMenu(commandHints())
	events, unsubscribe := svc.Subscribe()
	go func() {
		defer unsubscribe()
		for {
			select {
			case <-ctx.Done():
				g.shutdown(sess)
				return
			case ev := <-events:
				// A tool switched on/off for commands changes the slash command set; the hash check makes
				// unrelated config edits a no-op.
				if ev.Kind == "config" && g.connected.Load() && g.cfg.RegisterCommands {
					g.work(func(ctx context.Context) { g.syncCommands(ctx) })
				}
			}
		}
	}()
	g.startConnect()
	return g, nil
}

// startConnect begins the first connection attempt in the background. The attempt is counted, so
// shutdown waits for a handshake in progress before the database closes, and doctor reports
// "connecting" instead of a disconnect until the first attempt has an outcome.
func (g *Gateway) startConnect() {
	g.setStartupStatus(application.DiscordStartupStatus{Connecting: true})
	g.work(func(context.Context) { g.connect() })
}

// connect retries transient failures from the first gateway Open. DiscordGo owns reconnects after
// Open succeeds; this loop only owns the initial connection attempt.
func (g *Gateway) connect() {
	ctx := g.runCtx()
	attempt := 0
	delay := startupRetryInitial
	for ctx.Err() == nil {
		attempt++
		err := g.open()
		if err == nil {
			if ctx.Err() != nil {
				_ = g.sess.Close()
				return
			}
			// READY may land just before or just after Open returns; whichever comes second clears it
			g.startupMu.Lock()
			g.startup = application.DiscordStartupStatus{Attempts: attempt, Connecting: !g.connected.Load()}
			g.startupMu.Unlock()
			return
		}
		err = fmt.Errorf("discord open: %w", redactToken(err, g.token))
		if permanentOpenError(err) {
			g.setStartupStatus(application.DiscordStartupStatus{Attempts: attempt, LastError: err.Error()})
			g.log.Error("discord: gateway startup failed", "attempt", attempt, "err", err)
			return
		}
		jitter := g.retryJitter
		if jitter == nil {
			jitter = jittered
		}
		wait := jitter(delay)
		next := time.Now().Add(wait).UTC().Format(time.RFC3339Nano)
		g.setStartupStatus(application.DiscordStartupStatus{Attempts: attempt, LastError: err.Error(), NextRetry: next})
		g.log.Warn("discord: gateway open failed; retrying", "attempt", attempt, "next_retry", next, "err", err)
		waitForRetry := g.retryWait
		if waitForRetry == nil {
			waitForRetry = waitContext
		}
		if !waitForRetry(ctx, wait) {
			g.setStartupStatus(application.DiscordStartupStatus{Attempts: attempt, LastError: err.Error()})
			return
		}
		if delay < startupRetryMax/2 {
			delay *= 2
		} else {
			delay = startupRetryMax
		}
	}
}

// noteConnected records READY or RESUMED: the gateway is up, so the first connection is no longer pending.
func (g *Gateway) noteConnected() {
	g.startupMu.Lock()
	g.connected.Store(true)
	g.startup.Connecting = false
	g.startupMu.Unlock()
}

func (g *Gateway) setStartupStatus(status application.DiscordStartupStatus) {
	g.startupMu.Lock()
	g.startup = status
	g.startupMu.Unlock()
}

// StartupStatus is consumed by application doctor without depending on the Discord package.
func (g *Gateway) StartupStatus() application.DiscordStartupStatus {
	g.startupMu.RLock()
	defer g.startupMu.RUnlock()
	return g.startup
}

// permanentOpenError tells a failure that retrying cannot fix (the token or the requested intents are
// refused) from one that can pass (network, Discord outage).
func permanentOpenError(err error) bool {
	if errors.Is(err, discordgo.ErrUnauthorized) {
		return true
	}
	var rest *discordgo.RESTError
	if errors.As(err, &rest) && rest.Response != nil && (rest.Response.StatusCode == 401 || rest.Response.StatusCode == 403) {
		return true
	}
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) {
		switch closeErr.Code {
		case 4004, // authentication failed
			4010, 4011, 4012, // invalid shard, sharding required, invalid API version
			4013, 4014: // invalid intents, intents not enabled in the Developer Portal
			return true
		}
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "invalid token") || strings.Contains(message, "authentication failed")
}

func jittered(delay time.Duration) time.Duration {
	if delay <= 1 {
		return delay
	}
	// Half-to-full jitter within the current exponential ceiling avoids synchronized retries while
	// never exceeding the configured maximum.
	return time.Duration(rand.Int63n(int64(delay/2)+1)) + delay/2
}

func waitContext(ctx context.Context, delay time.Duration) bool {
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Replies in progress at shutdown get turnGrace to finish (the session stays open meanwhile); then they
// are cancelled, and a turn that did not make it tells its caller the bot is restarting (LIVE-20).
var (
	turnGrace  = 12 * time.Second
	afterGrace = 3 * time.Second
)

// shutdown stops taking new work, lets replies in progress finish, then closes the session.
func (g *Gateway) shutdown(sess *discordgo.Session) {
	g.workMu.Lock()
	g.draining = true
	drainDone := g.reconciliationDone
	g.workMu.Unlock()
	g.stopping.Store(true)
	done := make(chan struct{})
	go func() {
		g.inflight.Wait()
		if drainDone != nil {
			<-drainDone
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(turnGrace):
		if g.cancelWork != nil {
			g.cancelWork()
		}
		select {
		case <-done:
		case <-time.After(afterGrace):
		}
	}
	if g.cancelWork != nil {
		g.cancelWork()
	}
	_ = sess.Close()
	g.connected.Store(false)
}

// work runs fn in its own goroutine, bound to the daemon run and counted for Wait.
func (g *Gateway) work(fn func(ctx context.Context)) {
	ctx, ok := g.beginWork()
	if !ok {
		return
	}
	go func() {
		defer g.inflight.Done()
		defer crash.Recover(g.log, "discord reply")
		fn(ctx)
	}()
}

func (g *Gateway) beginWork() (context.Context, bool) {
	ctx := g.runCtx()
	g.workMu.Lock()
	defer g.workMu.Unlock()
	if g.draining || ctx.Err() != nil {
		return ctx, false
	}
	g.inflight.Add(1)
	if g.workCtx != nil {
		// outlives the run context by the shutdown grace, so a reply in progress can still be sent
		return g.workCtx, true
	}
	return ctx, true
}

func (g *Gateway) runCtx() context.Context {
	if g.ctx == nil {
		return context.Background()
	}
	return g.ctx
}

// Wait closes work admission, then blocks until replies, commands and gateway handlers in progress
// finish, or timeout passes. The daemon calls it on shutdown before the database closes.
func (g *Gateway) Wait(timeout time.Duration) bool {
	g.workMu.Lock()
	g.draining = true
	drainDone, cancelDrain := g.reconciliationDone, g.reconciliationCancel
	g.workMu.Unlock()
	if cancelDrain != nil {
		cancelDrain()
	}

	done := make(chan struct{})
	go func() {
		g.inflight.Wait()
		if drainDone != nil {
			<-drainDone
		}
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// locale is the language for replies in a guild: the guild's configured locale, then the instance
// default.
func (g *Gateway) locale(guildID string) string {
	if gs, ok := g.svc.Doc().Guilds[guildID]; ok && gs.Locale != "" {
		return gs.Locale
	}
	return g.svc.Doc().I18n.DefaultLocale
}

// prefixTable is the "!word" table with the current config's tool aliases.
func (g *Gateway) prefixTable() PrefixTable {
	var tc []ToolCommand
	for _, c := range g.svc.ToolCommands() {
		tc = append(tc, ToolCommand{Tool: c.Tool, Aliases: c.Aliases, Custom: c.Custom, FirstParam: c.FirstParam})
	}
	return NewPrefixTable(tc)
}

func (g *Gateway) commandToolEnabled(action string) bool {
	return g.svc.ToolCommandEnabled(action) && g.svc.ToolCommandReady(action)
}

// renderInvoked renders a result for the person who ran the command. Only someone who may administer
// the bot (an operator, or a server manager where managers may) also sees the error code.
func (g *Gateway) renderInvoked(res action.Result, cmd Command, ictx InvokeContext) Rendered {
	r := g.renderCommand(res, cmd, ictx.Locale, ictx.Manager)
	if !res.OK && res.Error != nil && res.Error.Code != "" && r.Embed != nil && g.mayAdmin(ictx) {
		r.Embed.Footer = &discordgo.MessageEmbedFooter{Text: res.Error.Code}
	}
	return r
}

// mayAdmin mirrors the application's rule for the admin command.
func (g *Gateway) mayAdmin(ictx InvokeContext) bool {
	if g.svc == nil || ictx.UserID == "" {
		return false
	}
	return g.svc.IsOperator(ictx.UserID) || ictx.Manager && ictx.GuildID != "" && g.svc.Doc().Access.GuildAdminsAllowed()
}

func (g *Gateway) renderCommand(res action.Result, cmd Command, locale string, manager bool) Rendered {
	if res.Action != "help.version" {
		return Render(res, cmd, locale)
	}
	var slash, prefixes []string
	var registeredCommands []*discordgo.ApplicationCommand
	if g.cfg.RegisterCommands { // slash commands exist only when the bot registers them
		registeredCommands = ApplicationCommandsFor(g.commandToolEnabled)
	}
	for _, ac := range registeredCommands {
		// Discord defaults an omitted type to a chat-input command; localized() relies on that default
		// for the payload, so it is represented as zero here.
		if ac.Type != 0 && ac.Type != discordgo.ChatApplicationCommand {
			continue
		}
		registered, ok := ByName(ac.Name)
		if !ok || registered.ManagersOnly && !manager {
			continue
		}
		label := "/" + ac.Name
		if local := LocalName(registered, locale); local != registered.Name {
			label += " (" + local + ")"
		}
		slash = append(slash, label+" — "+LocalDesc(registered, locale))
	}

	if !g.cfg.MessageContent || g.cfg.CommandPrefix == "" {
		// "!" commands are read from message text: without the message-content intent (or a prefix)
		// they cannot run, so help does not offer them
		return RenderHelp(locale, slash, nil)
	}
	commands := g.prefixTable()
	aliases := map[string]bool{}
	for _, c := range Commands() {
		if c.ManagersOnly && !manager || !g.commandToolEnabled(c.Action) {
			continue
		}
		for _, alias := range aliasesForLocale(c.Name, locale) {
			aliases[alias] = true
		}
	}
	for _, c := range g.svc.ToolCommands() {
		if g.commandToolEnabled(c.Tool) {
			for _, alias := range c.Aliases {
				aliases[alias] = true
			}
		}
	}
	aliasNames := make([]string, 0, len(aliases))
	for alias := range aliases {
		aliasNames = append(aliasNames, alias)
	}
	sort.Strings(aliasNames)
	for _, alias := range aliasNames {
		resolved, ok := commands.byAlias(alias)
		if !ok || resolved.ManagersOnly && !manager || !g.commandToolEnabled(resolved.Action) {
			continue
		}
		label := g.cfg.CommandPrefix + alias
		prefixes = append(prefixes, label+" — "+LocalDesc(resolved, locale))
	}
	return RenderHelp(locale, slash, prefixes)
}

// redactedError shows the text with the token removed and still unwraps to the original error, so
// errors.Is and errors.As keep working on the typed errors discordgo returns.
type redactedError struct {
	text string
	err  error
}

func (e *redactedError) Error() string { return e.text }
func (e *redactedError) Unwrap() error { return e.err }

func redactToken(err error, token string) error {
	if token == "" || err == nil {
		return err
	}
	text := err.Error()
	if !strings.Contains(text, token) {
		return err
	}
	return &redactedError{text: strings.ReplaceAll(text, token, "<redacted>"), err: err}
}

// ---- application.DiscordInfo ----

func (g *Gateway) Connected() bool { return g.connected.Load() }

func (g *Gateway) BotUser() (string, string) {
	id, name := g.botID.Load(), g.botName.Load()
	if id == nil || name == nil {
		return "", ""
	}
	return *id, *name
}

func (g *Gateway) Guilds() []application.GuildInfo {
	var out []application.GuildInfo
	if g.sess.State == nil {
		return out
	}
	g.sess.State.RLock()
	defer g.sess.State.RUnlock()
	for _, gu := range g.sess.State.Guilds {
		info := application.GuildInfo{ID: gu.ID, Name: gu.Name, Members: gu.MemberCount}
		if !gu.JoinedAt.IsZero() {
			info.JoinedAt = gu.JoinedAt.UTC().Format(time.RFC3339)
		}
		out = append(out, info)
	}
	return out
}

// LeaveGuild takes the bot out of a server (an operator turned the server down).
func (g *Gateway) LeaveGuild(ctx context.Context, guildID string) error {
	return g.sess.GuildLeave(guildID, discordgo.WithContext(ctx))
}

func (g *Gateway) Channels(guildID string) []application.ChannelInfo {
	var out []application.ChannelInfo
	gu, err := g.sess.State.Guild(guildID)
	if err != nil {
		return out
	}
	g.sess.State.RLock()
	defer g.sess.State.RUnlock()
	for _, c := range gu.Channels {
		if c.Type != discordgo.ChannelTypeGuildText && c.Type != discordgo.ChannelTypeGuildNews {
			continue
		}
		out = append(out, application.ChannelInfo{ID: c.ID, GuildID: guildID, Name: c.Name, Type: "text"})
	}
	return out
}

// Permissions is the bot's effective permission bits in a channel (from the gateway's state cache).
func (g *Gateway) Permissions(guildID, channelID string) (int64, bool) {
	id, _ := g.BotUser()
	if id == "" || g.sess.State == nil {
		return 0, false
	}
	p, err := g.sess.State.UserChannelPermissions(id, channelID)
	if err != nil {
		return 0, false
	}
	return p, true
}

// ---- handlers ----

func (g *Gateway) onReady(s *discordgo.Session, r *discordgo.Ready) {
	defer crash.Recover(g.log, "discord ready")
	if _, ok := g.beginWork(); !ok {
		return // the daemon is stopping: the database may be closing, nothing should be seeded or synced
	}
	defer g.inflight.Done()
	id, name := r.User.ID, r.User.Username
	g.botID.Store(&id)
	g.botName.Store(&name)
	g.noteConnected()
	g.log.Info("discord ready", "bot", name, "guilds", len(r.Guilds))
	// the servers the bot is in when server approval first meets a connection stay served
	ids := make([]string, 0, len(r.Guilds))
	for _, gu := range r.Guilds {
		ids = append(ids, gu.ID)
	}
	g.svc.SeedServers(ids)
	if expected := g.svc.Doc().Instance.ExpectedBotID; expected != "" && expected != id {
		g.log.Error("discord: connected bot id does not match instance.expected_bot_id; refusing to register commands", "got", id)
		return
	}
	if g.cfg.RegisterCommands {
		g.work(func(ctx context.Context) { g.syncCommands(ctx) })
	}
	// A Ready means a new session: whatever was posted since the last one never reached onMessage.
	// (A resumed session replays missed events itself and sends no Ready.)
	if g.cfg.MessageContent {
		g.work(func(ctx context.Context) {
			// let the guild state fill in first
			select {
			case <-time.After(5 * time.Second):
			case <-ctx.Done():
				return
			}
			g.svc.BackfillAfterConnect(ctx)
		})
	}
}

// onResumed restores the connected state after DiscordGo resumes the existing session. The READY
// backfill is intentionally omitted: Discord replays missed gateway events on a successful resume.
func (g *Gateway) onResumed(_ *discordgo.Session, _ *discordgo.Resumed) {
	defer crash.Recover(g.log, "discord resumed")
	if _, ok := g.beginWork(); !ok {
		return
	}
	defer g.inflight.Done()
	g.noteConnected()
	g.log.Info("discord resumed")
	if expected := g.svc.Doc().Instance.ExpectedBotID; expected != "" {
		id, _ := g.BotUser()
		if expected != id {
			g.log.Error("discord: resumed bot id does not match instance.expected_bot_id; refusing to register commands", "got", id)
			return
		}
	}
	if g.cfg.RegisterCommands {
		g.work(func(ctx context.Context) { g.syncCommands(ctx) })
	}
}

// History reads a channel's messages after afterID (application.DiscordHistory).
func (g *Gateway) History(ctx context.Context, channelID, afterID string, limit int) ([]application.HistoryMessage, error) {
	msgs, err := g.sess.ChannelMessages(channelID, limit, "", afterID, "", discordgo.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	out := make([]application.HistoryMessage, 0, len(msgs))
	for _, m := range msgs {
		if m == nil || m.Author == nil {
			continue
		}
		out = append(out, application.HistoryMessage{ID: m.ID, AuthorID: m.Author.ID, AuthorName: historyName(m),
			Content: m.Content, At: m.Timestamp, Bot: m.Author.Bot})
	}
	return out, nil
}

// historyName is the name people saw on a message read back from history (as displayName).
func historyName(m *discordgo.Message) string {
	if m.Member != nil && strings.TrimSpace(m.Member.Nick) != "" {
		return m.Member.Nick
	}
	if strings.TrimSpace(m.Author.GlobalName) != "" {
		return m.Author.GlobalName
	}
	return m.Author.Username
}

func (g *Gateway) onDisconnect(s *discordgo.Session, d *discordgo.Disconnect) {
	g.connected.Store(false)
	g.health.disconnects.Add(1)
	g.log.Warn("discord disconnected; discordgo will resume automatically")
}

// syncCommands bulk-overwrites the slash commands, once per payload hash, per configured guild (or globally).
func (g *Gateway) syncCommands(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	cmds := ApplicationCommandsFor(g.commandToolEnabled)
	h := hashCommands(cmds)
	if g.lastSync == h {
		return
	}
	id, _ := g.BotUser()
	targets := g.cfg.GuildIDs
	if len(targets) == 0 {
		targets = []string{""}
	}
	for _, guild := range targets {
		if ctx.Err() != nil {
			return
		}
		if _, err := g.sess.ApplicationCommandBulkOverwrite(id, guild, cmds, discordgo.WithContext(ctx)); err != nil {
			g.log.Error("discord: command sync failed", "guild", guild, "err", err)
			return
		}
	}
	g.lastSync = h
	g.log.Info("discord: commands synced", "count", len(cmds), "guilds", len(targets), "hash", h)
}

func localeOf(i *discordgo.Interaction) string {
	if i == nil {
		return "en"
	}
	if i.Locale != "" {
		return string(i.Locale)
	}
	if i.GuildLocale != nil {
		return string(*i.GuildLocale)
	}
	return "en"
}

func actorID(i *discordgo.Interaction) string {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User.ID
	}
	if i.User != nil {
		return i.User.ID
	}
	return ""
}

// interactionName is the invoker's display name in an interaction.
func interactionName(i *discordgo.Interaction) string {
	if i.Member != nil {
		if i.Member.Nick != "" {
			return i.Member.Nick
		}
		if i.Member.User != nil {
			if i.Member.User.GlobalName != "" {
				return i.Member.User.GlobalName
			}
			return i.Member.User.Username
		}
	}
	if i.User != nil {
		if i.User.GlobalName != "" {
			return i.User.GlobalName
		}
		return i.User.Username
	}
	return ""
}

// manages reports Discord's own verdict that someone runs the server: Administrator or Manage Server.
func manages(perms int64) bool {
	return perms&(discordgo.PermissionAdministrator|discordgo.PermissionManageGuild) != 0
}

// interactionManager reads the invoker's permissions Discord sends with an interaction.
func interactionManager(i *discordgo.Interaction) bool {
	return i.GuildID != "" && i.Member != nil && manages(i.Member.Permissions)
}

// messageManager works the author's permissions out from the gateway state (owner or role grants).
func (g *Gateway) messageManager(s *discordgo.Session, m *discordgo.MessageCreate) bool {
	if m.GuildID == "" || s.State == nil {
		return false
	}
	if gd, err := s.State.Guild(m.GuildID); err == nil && gd.OwnerID == m.Author.ID {
		return true
	}
	perms, err := s.State.MessagePermissions(m.Message)
	return err == nil && manages(perms)
}

func (g *Gateway) onInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	defer crash.Recover(g.log, "discord interaction")
	switch i.Type {
	case discordgo.InteractionApplicationCommand:
		g.handleCommand(s, i)
	case discordgo.InteractionMessageComponent:
		g.handleComponent(s, i)
	}
}

func (g *Gateway) handleCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.ApplicationCommandData()
	if data.CommandType == discordgo.MessageApplicationCommand && data.Name == rateCommandName {
		g.handleRate(s, i)
		return
	}
	cmd, ok := ByName(data.Name)
	if !ok {
		return
	}
	opts := map[string]string{}
	for _, o := range data.Options {
		switch o.Type {
		case discordgo.ApplicationCommandOptionInteger:
			opts[o.Name] = fmt.Sprint(o.IntValue())
		case discordgo.ApplicationCommandOptionBoolean:
			opts[o.Name] = fmt.Sprint(o.BoolValue())
		default:
			opts[o.Name] = fmt.Sprint(o.Value)
		}
	}
	ictx := InvokeContext{ChannelID: i.ChannelID, GuildID: i.GuildID, UserID: actorID(i.Interaction), Locale: localeOf(i.Interaction),
		Manager: interactionManager(i.Interaction)}
	ictx.Private = cmd.Ephemeral || i.GuildID == ""
	ictx.UserName = interactionName(i.Interaction)
	if cmd.Name == "poll" {
		ctx, cancel := context.WithTimeout(g.runCtx(), g.ActionTimeout)
		defer cancel()
		res := g.svc.Execute(ctx, requestFor(cmd, opts, ictx, "dc:"+i.ID))
		r := g.renderInvoked(res, cmd, ictx)
		data := &discordgo.InteractionResponseData{AllowedMentions: noMentions}
		if r.Poll != nil && res.OK {
			data.Poll = r.Poll
		} else if r.Embed != nil {
			data.Embeds = r.Embeds()
			if !res.OK {
				data.Flags = discordgo.MessageFlagsEphemeral
			}
		}
		if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: data,
		}, discordgo.WithContext(ctx)); err != nil {
			g.log.Warn("discord: poll respond failed", "cmd", cmd.Name, "err", err)
		} else if data.Poll != nil {
			if posted, err := s.InteractionResponse(i.Interaction, discordgo.WithContext(ctx)); err == nil {
				g.svc.LinkPollMessage(pollIDOf(res.Output), i.ChannelID, posted.ID)
			}
		}
		return
	}
	flags := discordgo.MessageFlags(0)
	if cmd.Ephemeral {
		flags = discordgo.MessageFlagsEphemeral
	}
	ackCtx, cancelAck := g.ackContext()
	defer cancelAck()
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseDeferredChannelMessageWithSource, Data: &discordgo.InteractionResponseData{Flags: flags}}, discordgo.WithContext(ackCtx)); err != nil {
		g.log.Warn("discord: ack failed", "cmd", cmd.Name, "err", err)
		return
	}
	g.work(func(base context.Context) {
		ctx, cancel := context.WithTimeout(base, g.ActionTimeout)
		defer cancel()
		req := requestFor(cmd, opts, ictx, "dc:"+i.ID)
		mark := g.svc.SettledMark() // before the command reads anything: its result may go on into a chat turn
		res := g.svc.Execute(ctx, req)
		if application.Unanswered(res) && !ictx.Private {
			// the reply is posted inside the turn: the conversation keeps only what reached Discord
			deliver := func(deliverCtx context.Context, out chat.Result) string {
				if deliverCtx.Err() != nil {
					return ""
				}
				parts := splitMessage(strings.TrimSpace(out.Answer), 1900)
				if len(parts) == 0 || parts[0] == "" {
					return "" // the command's card is shown instead
				}
				first := parts[0]
				edited, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &first, AllowedMentions: noMentions}, discordgo.WithContext(deliverCtx))
				if err != nil {
					g.log.Warn("discord: response edit failed", "cmd", cmd.Name, "err", err)
					return ""
				}
				said, ids := first, []string{edited.ID}
				for _, p := range parts[1:] {
					if deliverCtx.Err() != nil {
						break
					}
					sent, err := s.ChannelMessageSendComplex(i.ChannelID, &discordgo.MessageSend{Content: p, AllowedMentions: noMentions}, discordgo.WithContext(deliverCtx))
					if err != nil {
						g.log.Warn("discord: reply failed", "err", err)
						break
					}
					said += p
					ids = append(ids, sent.ID)
				}
				g.svc.NoteReplyMessages(out.TurnID, i.GuildID, i.ChannelID, ids)
				g.postToolArtifacts(deliverCtx, s, i.GuildID, i.ChannelID, nil, out.Tools)
				return said
			}
			if _, ok := g.commandFollowUp(base, s, res, application.ChatInput{GuildID: i.GuildID, ChannelID: i.ChannelID, MessageID: "dc:" + i.ID,
				Message: slashText(cmd, data.Options), Speaker: ictx.UserName, SpeakerID: ictx.UserID, Locale: ictx.Locale, Deliver: deliver, ReadFrom: mark}); ok {
				return
			}
		}
		edit := g.responseEdit(res, cmd, ictx)
		// the action's context may be spent (it timed out): the result or error card gets its own
		final, cancelFinal := g.finalContext(base)
		defer cancelFinal()
		if _, err := s.InteractionResponseEdit(i.Interaction, edit, discordgo.WithContext(final)); err != nil {
			g.log.Warn("discord: response edit failed", "cmd", cmd.Name, "err", err)
		}
	})
}

// responseEdit is the slash command's final response: its card (with the generated picture attached), or,
// for a picture that cannot be posted (over Discord's upload limit, or its file missing), the image.failed
// text without the file.
func (g *Gateway) responseEdit(res action.Result, cmd Command, ictx InvokeContext) *discordgo.WebhookEdit {
	r := g.renderInvoked(res, cmd, ictx)
	f, failed := g.pictureFile(res, r.Embed)
	if failed {
		failure := g.svc.Msg(ictx.Locale, "image.failed")
		return &discordgo.WebhookEdit{Content: &failure, Embeds: ptrEmbeds([]*discordgo.MessageEmbed{}), AllowedMentions: noMentions}
	}
	edit := &discordgo.WebhookEdit{Embeds: ptrEmbeds(r.Embeds()), AllowedMentions: noMentions}
	if r.Components != nil {
		edit.Components = &r.Components
	}
	if f != nil {
		edit.Files = []*discordgo.File{f}
	}
	return edit
}

// finalContext is the context for the last edit or send of a command: short, derived from the daemon
// run (so stopping still cancels it) and independent of the action's timeout.
func (g *Gateway) finalContext(base context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(base, finalDeliveryTimeout)
}

// ackContext bounds the first answer to an interaction: Discord drops it after 3 seconds anyway.
func (g *Gateway) ackContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(g.runCtx(), interactionAckTimeout)
}

func (g *Gateway) handleComponent(s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.MessageComponentData()
	parts := strings.Split(data.CustomID, ":")
	ictx := InvokeContext{ChannelID: i.ChannelID, GuildID: i.GuildID, UserID: actorID(i.Interaction), Locale: localeOf(i.Interaction),
		Manager: interactionManager(i.Interaction), UserName: interactionName(i.Interaction)}
	ictx.Private = true // button answers are ephemeral (polls set their own visibility below)
	if parts[0] == "fb" {
		g.handleRateComponent(s, i, parts)
		return
	}
	if parts[0] == "more" && len(parts) == 2 {
		g.handleMore(s, i, parts[1])
		return
	}
	if parts[0] == "thr" {
		g.handleThreadComponent(s, i, parts)
		return
	}
	if len(parts) >= 3 && parts[0] == "poll" {
		cmd, req, ok := buttonRequest(parts, ictx, "dc:"+i.ID)
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(g.runCtx(), 2500*time.Millisecond) // the interaction must be answered within 3s
		defer cancel()
		res := g.svc.Execute(ctx, req)
		r := Render(res, cmd, ictx.Locale)
		flags := discordgo.MessageFlags(0)
		if r.Ephemeral || !res.OK || parts[1] == "vote" {
			flags = discordgo.MessageFlagsEphemeral
		}
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{Embeds: r.Embeds(), Flags: flags, AllowedMentions: noMentions},
		}, discordgo.WithContext(ctx))
		return
	}
	req, ok := featureButton(parts, ictx, "dc:"+i.ID)
	if !ok {
		return
	}
	// feature buttons may call a model (a fortune): acknowledge now, answer privately when done
	ackCtx, cancelAck := context.WithTimeout(g.runCtx(), 2500*time.Millisecond)
	defer cancelAck()
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral}}, discordgo.WithContext(ackCtx)); err != nil {
		g.log.Warn("discord: button ack failed", "err", err)
		return
	}
	g.work(func(base context.Context) {
		ctx, cancel := context.WithTimeout(base, g.ActionTimeout)
		defer cancel()
		res := g.svc.Execute(ctx, req)
		r := g.renderInvoked(res, Command{Name: parts[0], Action: req.Action}, ictx)
		edit := &discordgo.WebhookEdit{Embeds: ptrEmbeds(r.Embeds()), AllowedMentions: noMentions}
		if r.Components != nil {
			edit.Components = &r.Components
		}
		final, cancelFinal := g.finalContext(base)
		defer cancelFinal()
		if _, err := s.InteractionResponseEdit(i.Interaction, edit, discordgo.WithContext(final)); err != nil {
			g.log.Warn("discord: button response failed", "err", err)
		}
	})
}

// PostChannel posts to a channel on the bot's own initiative (application.ChannelPoster): a scheduled
// reminder or an unasked remark, as a reply to replyTo when given. Nobody is mentioned.
func (g *Gateway) PostChannel(ctx context.Context, channelID, replyTo, text string, card *action.Card) (string, error) {
	msg := &discordgo.MessageSend{AllowedMentions: noMentions}
	if replyTo != "" {
		msg.Reference = &discordgo.MessageReference{MessageID: replyTo, ChannelID: channelID, FailIfNotExists: ptrBool(false)}
	}
	if card != nil {
		r := renderCard(*card, false)
		msg.Embeds, msg.Components = r.Embeds(), r.Components
	}
	parts := splitMessage(strings.TrimSpace(text), 1900)
	if text != "" {
		msg.Content = parts[0]
	}
	sent, err := g.postRetrying(ctx, channelID, msg)
	if err != nil {
		return "", sendError(err)
	}
	for _, p := range parts[1:] {
		if text == "" || ctx.Err() != nil {
			break
		}
		if _, err := g.postRetrying(ctx, channelID, &discordgo.MessageSend{Content: p, AllowedMentions: noMentions}); err != nil {
			return sent.ID, nil // the first part is out: the reminder or remark was delivered
		}
	}
	return sent.ID, nil
}

// PostPicture posts a generated picture into a channel on the bot's own initiative
// (application.ImagePoster): the card with the file attached, as a reply to replyTo, retried like any
// post of ours. Nobody is mentioned. The picture shows what a person asked for, so when the message it
// replies to is gone nothing is posted (ErrReplyTargetGone), not even without the reference.
func (g *Gateway) PostPicture(ctx context.Context, channelID, replyTo string, card action.Card, imageID string) (string, error) {
	r := renderCard(card, false)
	msg := &discordgo.MessageSend{Embeds: r.Embeds(), AllowedMentions: noMentions}
	if replyTo != "" {
		msg.Reference = &discordgo.MessageReference{MessageID: replyTo, ChannelID: channelID, FailIfNotExists: ptrBool(true)}
	}
	f := g.attachment(action.Result{OK: true, Output: map[string]any{"image_id": imageID}}, r.Embed)
	if f == nil {
		return "", errors.New("discord: generated picture is not available")
	}
	msg.Files = []*discordgo.File{f}
	sent, err := g.postRetryingRef(ctx, channelID, msg, true)
	if err != nil {
		switch {
		case errors.Is(err, application.ErrReplyTargetGone):
			return "", err
		case classifyREST(err) == errclass.Permission:
			return "", fmt.Errorf("%w: %v", application.ErrPostForbidden, err)
		}
		return "", sendError(err)
	}
	return sent.ID, nil
}

// postRetrying posts one message on the bot's own initiative with a nonce, retrying a transient failure
// (5xx, timeout, one rate limit) like replies do; a reference to a deleted message is dropped. The nonce
// makes a retry of a post whose answer was lost come back as the first post, not a second copy.
func (g *Gateway) postRetrying(ctx context.Context, channelID string, msg *discordgo.MessageSend) (*discordgo.Message, error) {
	return g.postRetryingRef(ctx, channelID, msg, false)
}

// postRetryingRef is postRetrying; with strictRef a reference to a deleted message is an error
// (application.ErrReplyTargetGone) instead of being dropped.
func (g *Gateway) postRetryingRef(ctx context.Context, channelID string, msg *discordgo.MessageSend, strictRef bool) (*discordgo.Message, error) {
	nonce := newNonce()
	limited := false
	for attempt := 0; ; attempt++ {
		sent, err := postReply(ctx, g.sess, channelID, msg, nonce)
		if err == nil {
			return sent, nil
		}
		if msg.Reference != nil && replyTargetGone(err) {
			if strictRef {
				return nil, application.ErrReplyTargetGone
			}
			msg.Reference = nil
			continue
		}
		class := classifyREST(err)
		if class == errclass.Permission {
			g.svc.NoteDeliveryProblem("", channelID, class)
		}
		if class == errclass.RateLimited {
			if limited {
				return nil, err
			}
			limited = true
		}
		if !class.Retryable() || attempt >= len(replyBackoff) || !sleepCtx(ctx, replyBackoff[attempt]) {
			return nil, err
		}
	}
}

// EditChannel rewrites one of the bot's own posts with a new card (application.ChannelEditor). The ID is
// kept from the post, so no read permission is needed.
func (g *Gateway) EditChannel(ctx context.Context, channelID, messageID string, card *action.Card) error {
	r := renderCard(*card, false)
	edit := discordgo.NewMessageEdit(channelID, messageID)
	edit.Embeds = ptrEmbeds(r.Embeds())
	edit.AllowedMentions = noMentions
	_, err := g.sess.ChannelMessageEditComplex(edit, discordgo.WithContext(ctx))
	return err
}

func ptrBool(v bool) *bool { return &v }

// sendError tells a definite refusal (Discord answered: nothing was posted) from an unknown outcome.
func sendError(err error) error {
	var rest *discordgo.RESTError
	if errors.As(err, &rest) && rest.Response != nil {
		return fmt.Errorf("%w: %v", application.ErrNotDelivered, err)
	}
	return err
}

// SendDM delivers a card to a user's direct messages (application.DiscordSender).
func (g *Gateway) SendDM(ctx context.Context, userID string, card action.Card, imageID string) error {
	ch, err := g.sess.UserChannelCreate(userID, discordgo.WithContext(ctx))
	if err != nil {
		return dmError(err)
	}
	r := renderCard(card, false)
	msg := &discordgo.MessageSend{Embeds: r.Embeds(), Components: r.Components, AllowedMentions: noMentions}
	if imageID != "" {
		if f := g.attachment(action.Result{OK: true, Output: map[string]any{"image_id": imageID}}, r.Embed); f != nil {
			msg.Files = []*discordgo.File{f}
		}
	}
	if _, err := g.sess.ChannelMessageSendComplex(ch.ID, msg, discordgo.WithContext(ctx)); err != nil {
		return dmError(err)
	}
	return nil
}

// dmError marks Discord's "cannot send messages to this user" (50007) as permanent.
func dmError(err error) error {
	var rest *discordgo.RESTError
	if errors.As(err, &rest) && rest.Message != nil && rest.Message.Code == discordgo.ErrCodeCannotSendMessagesToThisUser {
		return application.ErrDMClosed
	}
	return sendError(err)
}

func ptrEmbeds(e []*discordgo.MessageEmbed) *[]*discordgo.MessageEmbed { return &e }

// attachment is the generated picture of an images.generate result, shown in the embed.
func (g *Gateway) attachment(res action.Result, e *discordgo.MessageEmbed) *discordgo.File {
	id, _ := res.Output["image_id"].(string)
	if !res.OK || id == "" {
		return nil
	}
	rc, mime, err := g.svc.ImageFile(id)
	if err != nil {
		g.log.Warn("discord: generated picture missing", "err", err)
		return nil
	}
	name := id + map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/webp": ".webp", "image/gif": ".gif"}[mime]
	if e != nil {
		e.Image = &discordgo.MessageEmbedImage{URL: "attachment://" + name}
	}
	// the reader is consumed by the send; the file is small and closed once read
	data, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		return nil
	}
	return &discordgo.File{Name: name, ContentType: mime, Reader: bytes.NewReader(data)}
}

// featureButton maps a card button (action.CardButton ID "<feature>:<verb>:<args>") to its Action.
// The person pressing is always the invoker: a button never acts for whoever posted it.
func featureButton(parts []string, ictx InvokeContext, requestID string) (action.Request, bool) {
	if len(parts) < 2 {
		return action.Request{}, false
	}
	var act string
	var in map[string]any
	switch parts[0] + ":" + parts[1] {
	case "privacy:grant":
		if len(parts) < 3 {
			return action.Request{}, false
		}
		act, in = "privacy.consent", map[string]any{"scope": parts[2], "decision": "grant", "confirmed": true}
	case "fortune:unsub": // the morning fortune's own stop button
		act, in = "fortune.subscribe", map[string]any{"op": "unsubscribe"}
	case "notice:pause": // notice:pause:<kind>, the digest's stop button
		if len(parts) < 3 || (parts[2] != "school" && parts[2] != "transfer") {
			return action.Request{}, false
		}
		act, in = "notices.subscribe", map[string]any{"kind": parts[2], "op": "pause"}
	case "remind:pick", "remind:dm", "remind:here", "remind:cancel", "remind:join", "remind:pause", "remind:resume":
		// remind:<verb>:<id>[:<n>]; the reminder's owner is checked by the Action
		if len(parts) < 3 {
			return action.Request{}, false
		}
		switch parts[1] {
		case "pick":
			if len(parts) < 4 {
				return action.Request{}, false
			}
			n, err := strconv.Atoi(parts[3])
			if err != nil {
				return action.Request{}, false
			}
			act, in = "reminders.update", map[string]any{"op": "pick", "id": parts[2], "n": n}
		case "dm":
			act, in = "reminders.update", map[string]any{"op": "confirm", "id": parts[2], "destination": "dm"}
		case "here":
			act, in = "reminders.update", map[string]any{"op": "confirm", "id": parts[2], "destination": "channel"}
		case "cancel":
			act, in = "reminders.update", map[string]any{"op": "cancel", "id": parts[2]}
		case "pause", "resume":
			act, in = "reminders.update", map[string]any{"op": parts[1], "id": parts[2]}
		case "join":
			act, in = "reminders.propose", map[string]any{"offer_id": parts[2]}
		}
	default:
		if b, ok := extraButtons[parts[0]]; ok {
			act, in, ok = b(parts)
			if !ok {
				return action.Request{}, false
			}
		} else {
			return action.Request{}, false
		}
	}
	return action.Request{RequestID: requestID, Action: act, Input: mustJSON(in), Locale: ictx.Locale,
		Invoker: &action.Invoker{Via: "command", GuildID: ictx.GuildID, ChannelID: ictx.ChannelID, UserID: ictx.UserID, Private: ictx.GuildID == "", Name: ictx.UserName}}, true
}

// extraButtons are feature button families registered next to their commands (feature -> parser).
var extraButtons = map[string]func(parts []string) (string, map[string]any, bool){}

// buttonRequest maps a poll button (poll:vote:<id>:<option>, poll:result:<id>, poll:close:<id>) to
// its Action. Votes and closes apply to the poll as it is when the press arrives (latest), so people
// pressing at the same moment all count instead of failing on each other's revision.
func buttonRequest(parts []string, ictx InvokeContext, requestID string) (Command, action.Request, bool) {
	if len(parts) < 3 || parts[0] != "poll" {
		return Command{}, action.Request{}, false
	}
	pollID := parts[2]
	switch parts[1] {
	case "vote":
		if len(parts) < 4 {
			return Command{}, action.Request{}, false
		}
		return Command{Name: "vote"}, action.Request{RequestID: requestID, Action: "polls.vote", Locale: ictx.Locale, Input: mustJSON(map[string]any{
			"id": pollID, "scope": "discord:" + ictx.ChannelID, "participant": ictx.UserID, "option_ids": []string{parts[3]}, "latest": true,
		})}, true
	case "result":
		return Command{Name: "results"}, action.Request{RequestID: requestID, Action: "polls.result", Locale: ictx.Locale, Input: mustJSON(map[string]any{"id": pollID})}, true
	case "close":
		return Command{Name: "close"}, action.Request{RequestID: requestID, Action: "polls.close", Locale: ictx.Locale, Input: mustJSON(map[string]any{
			"id": pollID, "scope": "discord:" + ictx.ChannelID, "latest": true,
		})}, true
	}
	return Command{}, action.Request{}, false
}

// onMessage bridges conversation and prefix commands; DMs need no message-content intent.
func (g *Gateway) onMessage(s *discordgo.Session, m *discordgo.MessageCreate) {
	defer crash.Recover(g.log, "discord message")
	if m.Author == nil || (!g.cfg.MessageContent && m.GuildID != "") {
		return
	}
	if !g.seen.first(m.ID, time.Now()) {
		return // Discord replayed it (RESUME): no second capture, no second reply
	}
	if m.Author.Bot {
		if p := g.botID.Load(); p != nil && *p == m.Author.ID {
			if ours, drop := g.interims.ownCreate(m.ChannelID, m.ID, m.Content); ours {
				if drop {
					// the send returned no answer, but Discord still created the line. Take it
					// back and do not count it as the channel moving on.
					g.deleteInterimLine(s, m.ChannelID, m.ID)
				}
				return
			}
		}
		if p := g.botID.Load(); p != nil && *p == m.Author.ID && m.GuildID != "" {
			// the bot's own message: participation counts it as the channel moving on
			o := application.ObservedMessage{GuildID: m.GuildID, ChannelID: m.ChannelID, MessageID: m.ID, AuthorID: m.Author.ID,
				AuthorName: displayName(m), Content: m.Content, At: m.Timestamp, FromBot: true}
			linksOf(m, &o)
			g.svc.Observe(o)
		}
		return
	}
	cmd, opts, isCmd := g.prefixTable().Parse(g.cfg.CommandPrefix, m.Content)
	if m.GuildID != "" && !g.svc.GuildAllowed(m.GuildID) {
		// a server waiting for an operator's approval: nothing is stored, observed or answered. Only an
		// operator's "!" commands and consent (which can always be withdrawn) run; a slash command gets
		// the notice, while "!" stays quiet so a busy server is not flooded with it.
		if isCmd && (g.svc.IsOperator(m.Author.ID) || cmd.Action == "privacy.consent") {
			g.runCommand(s, m, cmd, opts)
		}
		return
	}
	g.pictures.note(m, time.Now()) // early: the order calls and pictures are seen in should be Discord's
	if m.GuildID == "" {
		if g.svc.BlockedIn("", m.Author.ID) {
			return
		}
		if isCmd {
			g.runCommand(s, m, cmd, opts)
			return
		}
		// held here, in arrival order; whether DM chat is open is checked when the held turn starts
		// (answerHeld), off the event loop, since it may ask Discord
		if !g.stopTurn(m) {
			g.notePictureFollow(m)
			g.pictures.used(m.ChannelID, []string{m.ID})
			g.merge.add(m, false)
		}
		return
	}
	g.noteThread(s, m.GuildID, m.ChannelID) // before the policy: a thread follows its parent's
	pol := g.svc.ChannelPolicyFor(m.GuildID, m.ChannelID)
	g.capture(m, pol)
	answer := !isCmd && g.shouldAnswer(m, pol)
	// the rest of a split request: the same person's next message while their call is being held, or
	// the picture they send right after a call that came without one
	follows := !isCmd && !answer && pol.Respond == "mention" && !g.toSomeone(m) && g.merge.waiting(m)
	if !isCmd && !follows && !g.toSomeone(m) && g.notePictureFollow(m) {
		follows = !answer // a room that answers everything answers it anyway; the mark makes it addressed
	}
	if m.GuildID != "" {
		o := application.ObservedMessage{GuildID: m.GuildID, ChannelID: m.ChannelID, MessageID: m.ID, AuthorID: m.Author.ID,
			AuthorName: displayName(m), Content: bodyOf(m.Message), At: m.Timestamp, Addressed: isCmd || answer || follows || pol.Respond == "always", ToSomeone: g.toSomeone(m), BotCall: g.botCallKind(m)}
		linksOf(m, &o)
		g.svc.Observe(o)
	}
	if g.svc.BlockedIn(m.GuildID, m.Author.ID) {
		return // ignored: no commands, no replies
	}
	stopped := !isCmd && !g.toSomeone(m) && g.stopTurn(m) // a stop word cancels the reply being written for this person here
	if !isCmd && !g.toSomeone(m) && g.svc.ThreadStopCue(m.GuildID, m.ChannelID, m.Author.ID, m.Content) {
		g.work(func(ctx context.Context) { _ = g.svc.CloseThread(ctx, m.ChannelID, "stop") })
		return
	}
	if stopped {
		return // it stopped a reply; it is not a request of its own
	}
	if isCmd {
		g.runCommand(s, m, cmd, opts)
		return
	}
	if answer || follows {
		if answer {
			g.svc.NoteDirectCall(m.GuildID, m.ChannelID, m.Author.ID)
		}
		if follows || g.mentionsBot(m) {
			g.pictures.used(m.ChannelID, []string{m.ID}) // a picture a call holds is not another call's
		}
		g.merge.add(m, answer && g.isBareCall(m))
		return
	}
	// the person the bot just answered, carrying on without a tag: the follow-up window decides (it is
	// off by default and then only records what it would have done)
	if m.GuildID != "" && pol.Respond == "mention" && g.svc.FollowWindowOpen(m.GuildID, m.ChannelID, m.Author.ID) {
		o := application.ObservedMessage{GuildID: m.GuildID, ChannelID: m.ChannelID, MessageID: m.ID, AuthorID: m.Author.ID,
			AuthorName: displayName(m), Content: bodyOf(m.Message), At: m.Timestamp}
		linksOf(m, &o)
		g.work(func(ctx context.Context) {
			if g.svc.FollowUp(ctx, o) {
				g.followIDs.Store(m.ID, true)
				g.pictures.used(m.ChannelID, []string{m.ID})
				g.merge.add(m, false)
			}
		})
	}
}

// linksOf copies what a message points at — the message it replies to and the accounts, roles or
// @everyone it mentions — for attribution (application/live.go).
func linksOf(m *discordgo.MessageCreate, o *application.ObservedMessage) {
	switch {
	case m.MessageReference != nil && m.MessageReference.Type == discordgo.MessageReferenceTypeForward:
		// a forward points at the message it copies, not at one it answers
	case m.MessageReference != nil:
		o.ReplyTo = m.MessageReference.MessageID
	case m.ReferencedMessage != nil:
		o.ReplyTo = m.ReferencedMessage.ID
	}
	for _, u := range m.Mentions {
		if u != nil {
			o.Mentions = append(o.Mentions, u.ID)
		}
	}
	o.MentionsOthers = len(m.MentionRoles) > 0 || m.MentionEveryone
}

// isBareCall reports a message that only calls the bot (merge.go).
func (g *Gateway) isBareCall(m *discordgo.MessageCreate) bool {
	if carriesMore(m.Message) {
		return false
	}
	id := ""
	if p := g.botID.Load(); p != nil {
		id = *p
	}
	return bareCall(m.Content, id, g.svc.Doc().BotNames())
}

// forgetFollow drops the follow-up mark of a held message that will never start a turn.
func (g *Gateway) forgetFollow(id string) {
	g.followIDs.Delete(id)
	g.pictureFollowIDs.Delete(id)
}

// answerHeld answers the messages the merger held as one turn, replying to the last of them.
func (g *Gateway) answerHeld(msgs []*discordgo.MessageCreate) {
	last := msgs[len(msgs)-1]
	mark := g.svc.SettledMark() // before the pictures are chosen: an edit settled by now has reached them
	call := g.heldCall(msgs)
	call.readFrom = mark
	text := mergedText(msgs)
	if call.continues != "" {
		// a picture sent right after a call without one: the call's words come again with it
		text = strings.TrimSpace(call.continues + "\n" + text)
	}
	call.fragments = fragmentIDs(msgs, call.continuedFrom)
	g.work(func(ctx context.Context) {
		if last.GuildID == "" && !g.svc.DMChatAllowed(ctx, last.Author.ID) {
			g.sendDMNotice(ctx, g.sess, last, "discord.chat.dm_closed", "closed")
			return
		}
		g.answer(ctx, g.sess, last, text, call)
	})
}

// fragmentIDs lists the messages whose words the turn holds apart from the one it replies to, the last of the held
// ones: the rest of the burst and the call a picture continues.
func fragmentIDs(msgs []*discordgo.MessageCreate, continuedFrom []string) []string {
	last := msgs[len(msgs)-1].ID
	var out []string
	for _, m := range msgs {
		if m.ID != "" && m.ID != last {
			out = append(out, m.ID)
		}
	}
	for _, id := range continuedFrom {
		if id != "" && id != last && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// heldCall is how the held messages called the bot. Pictures are collected only for a turn addressed to
// the bot (a mention, its name, a reply to it, a follow-up the window chose, a line in the bot's own
// thread): a room that answers everything does not read the pictures of messages nobody said to it.
func (g *Gateway) heldCall(msgs []*discordgo.MessageCreate) turnCall {
	last := msgs[len(msgs)-1]
	continues, continuedFrom, owed := g.takePictureFollow(msgs)
	if last.GuildID == "" {
		call := turnCall{addressed: true, continues: continues, continuedFrom: continuedFrom}
		g.callPictures(msgs, &call)
		return call
	}
	addressed := g.addressesBot(msgs) || owed // owed: the picture the person owed their call a moment ago
	// only the bot's name, no mention and no reply to it: the turn first asks whether it was said to the
	// bot or about it (name_call: addressed)
	followUp := true
	for _, m := range msgs {
		if _, ok := g.followIDs.LoadAndDelete(m.ID); !ok {
			followUp = false
		}
	}
	inThread := false // a participant's line in the bot's own open thread
	if v, ok := g.svc.ThreadFor(last.ChannelID, last.Author.ID); ok && v.BotOwned && v.Open {
		inThread = true
	}
	// only when the bot's name is what made this a call: not a mention or reply, not a follow-up the
	// window chose, not a line in the bot's own thread (those answer without any name)
	nameOnly := false
	if pol := g.svc.ChannelPolicyFor(last.GuildID, last.ChannelID); pol.Respond == "mention" && pol.NameCall == "addressed" && !followUp {
		anySure, anyName := false, false
		for _, m := range msgs {
			sure, byName := g.botCall(m)
			anySure, anyName = anySure || sure, anyName || byName
		}
		nameOnly = anyName && !anySure && !inThread
	}
	call := turnCall{addressed: addressed, nameOnly: nameOnly, followUp: followUp, continues: continues, continuedFrom: continuedFrom}
	if addressed || followUp || inThread {
		g.callPictures(msgs, &call)
	}
	return call
}

// callPictures fills the call's pictures: the ones it came with, else the one posted just before it
// (lookback.go). A call that ends up without any picture is remembered for a moment, so the picture sent
// right after it is answered as its rest.
func (g *Gateway) callPictures(msgs []*discordgo.MessageCreate, call *turnCall) {
	first, last := msgs[0], msgs[len(msgs)-1]
	now := time.Now()
	selection, imageOnly := g.callImages(msgs)
	call.images, call.imageOnly = selection.Refs, imageOnly
	call.imageTotal, call.imageSkipped = selection.Total, selection.Skipped
	if selection.Total > 0 {
		ids := make([]string, 0, len(selection.Refs))
		for _, r := range selection.Refs {
			ids = append(ids, r.MessageID)
		}
		g.pictures.used(last.ChannelID, ids)
		if call.continues != "" {
			call.imageOnly = false // the call's words come with the picture
		}
		return
	}
	if call.continues != "" {
		return
	}
	if refs, total := g.pictures.earlier(last.ChannelID, first.ID, last.Author.ID, now); len(refs) > 0 {
		plain := make([]*discordgo.Message, 0, len(msgs))
		for _, m := range msgs {
			plain = append(plain, m.Message)
		}
		id := ""
		if p := g.botID.Load(); p != nil {
			id = *p
		}
		call.images, call.imageTotal, call.imageEarlier = refs, total, true
		if total > len(refs) {
			call.imageSkipped = map[string]int{vision.ReasonOverLimit: total - len(refs)}
		}
		call.imageOnly = onlyCalls(plain, id, g.svc.Doc().BotNames())
		return
	}
	if !call.nameOnly { // a call by name alone may turn out not to be one
		ids := make([]string, 0, len(msgs))
		for _, m := range msgs {
			ids = append(ids, m.ID)
		}
		g.pictures.called(last.ChannelID, last.Author.ID, mergedText(msgs), ids, now)
	}
}

// notePictureFollow marks a picture-only message that continues the person's call answered a moment ago
// without a picture (lookback.go).
func (g *Gateway) notePictureFollow(m *discordgo.MessageCreate) bool {
	if !pictureOnly(m.Message) {
		return false
	}
	c, ok := g.pictures.follows(m.ChannelID, m.Author.ID, time.Now())
	if ok {
		g.pictureFollowIDs.Store(m.ID, c)
	}
	return ok
}

// takePictureFollow takes the marks notePictureFollow left on held messages: the continued call's words and the
// messages they came from.
func (g *Gateway) takePictureFollow(msgs []*discordgo.MessageCreate) (string, []string, bool) {
	text, owed := "", false
	var from []string
	for _, m := range msgs {
		if v, ok := g.pictureFollowIDs.LoadAndDelete(m.ID); ok {
			owed = true
			if c, _ := v.(pendingCall); c.text != "" {
				text, from = c.text, c.ids
			}
		}
	}
	return text, from, owed
}

// pictureOnly reports a message that is pictures and nothing else.
func pictureOnly(m *discordgo.Message) bool {
	if m == nil || strings.TrimSpace(m.Content) != "" || m.ReferencedMessage != nil || len(m.MessageSnapshots) > 0 || len(m.StickerItems) > 0 {
		return false
	}
	for _, a := range m.Attachments {
		if a != nil && vision.ImageType(a.ContentType, a.Filename) {
			return true
		}
	}
	return false
}

// turnCall is how a held message called the bot, and the pictures it was called with.
type turnCall struct {
	addressed, nameOnly, followUp bool
	images                        []vision.Ref
	imageOnly                     bool   // the messages said nothing but the call: the pictures are the request
	imageEarlier                  bool   // the pictures were posted just before the call, not with it
	continues                     string // the words of the call this picture-only message continues
	continuedFrom                 []string
	imageTotal                    int
	imageSkipped                  map[string]int
	fragments                     []string // the other messages whose words the turn holds (ChatInput.Fragments)
	readFrom                      int64    // the mark taken before the pictures were chosen (ChatInput.ReadFrom)
}

// callImages lists the pictures the bot was called with (inbound.go imageRefs) and whether they are the
// whole request.
func (g *Gateway) callImages(msgs []*discordgo.MessageCreate) (imageSelection, bool) {
	plain := make([]*discordgo.Message, 0, len(msgs))
	for _, m := range msgs {
		plain = append(plain, m.Message)
	}
	selection := selectImageRefs(plain)
	if len(selection.Refs) == 0 {
		return selection, false
	}
	id := ""
	if p := g.botID.Load(); p != nil {
		id = *p
	}
	return selection, onlyCalls(plain, id, g.svc.Doc().BotNames())
}

// addressesBot reports whether the person actually spoke to the bot in these messages (a mention, a
// reply to it, or its name). In a room set to answer everything most turns are the bot's own initiative,
// and only these may be told "I cannot answer right now".
func (g *Gateway) addressesBot(msgs []*discordgo.MessageCreate) bool {
	for _, m := range msgs {
		if g.mentionsBot(m) {
			return true
		}
	}
	return false
}

// toSomeone reports a message meant for another person: a reply to them or a mention of them.
func (g *Gateway) toSomeone(m *discordgo.MessageCreate) bool {
	id := ""
	if p := g.botID.Load(); p != nil {
		id = *p
	}
	if m.ReferencedMessage != nil && m.ReferencedMessage.Author != nil && m.ReferencedMessage.Author.ID != id {
		return true
	}
	for _, u := range m.Mentions {
		if u != nil && u.ID != id {
			return true
		}
	}
	return len(m.MentionRoles) > 0 || m.MentionEveryone
}

// onMessageUpdate applies an author's edit to what the bot stored (message text needs the privileged
// message-content intent). Discord also sends updates when a link preview loads; only real edits
// (EditedTimestamp set) change anything.
func (g *Gateway) onMessageUpdate(s *discordgo.Session, m *discordgo.MessageUpdate) {
	defer crash.Recover(g.log, "discord message update")
	if m.Message == nil || (!g.cfg.MessageContent && m.GuildID != "") || m.EditedTimestamp == nil || (m.Author != nil && m.Author.Bot) {
		return
	}
	g.merge.edit(m.ID, m.Content)
	g.pictures.edited(m.ChannelID, m.ID, m.Attachments)
	g.applyMessageChange(messageReconciliation{Operation: "edit", GuildID: m.GuildID, ChannelID: m.ChannelID,
		MessageIDs: []string{m.ID}, Content: m.Content, At: *m.EditedTimestamp, SameFiles: sameFiles(m.BeforeUpdate, m.Attachments)})
}

// sameFiles reports whether an edit kept the message's attachments. A message no longer in the state cache cannot
// say, and is taken as changed.
func sameFiles(before *discordgo.Message, after []*discordgo.MessageAttachment) bool {
	if before == nil {
		return false
	}
	ids := func(list []*discordgo.MessageAttachment) []string {
		out := make([]string, 0, len(list))
		for _, a := range list {
			if a != nil {
				out = append(out, a.ID)
			}
		}
		sort.Strings(out)
		return out
	}
	return slices.Equal(ids(before.Attachments), ids(after))
}

// stateMessages is how many recent messages per channel the state cache keeps.
const stateMessages = 100

// onMessageDelete and onMessageDeleteBulk remove deleted messages from memory. They run whatever the
// channel's capture setting is now: a message stored while capture was on is still removed. The server
// log hears of them first, while the stored message still has its author and text.
func (g *Gateway) onMessageDelete(s *discordgo.Session, m *discordgo.MessageDelete) {
	defer g.recoverReconciliationPanic("delete handler", nil)
	if m.Message == nil || m.ID == "" {
		return
	}
	if g.interims.deleted(m.ID) {
		return // the bot took its own "still looking" line back: nothing to log, forget or reconcile
	}
	_ = g.runReconciliationStep("server log", func() error {
		d := application.DeletedMessage{ID: m.ID}
		if b := m.BeforeDelete; b != nil && b.Author != nil {
			d.AuthorID, d.AuthorBot, d.Text = b.Author.ID, b.Author.Bot, b.Content
			for _, a := range b.Attachments {
				d.Files = append(d.Files, a.Filename)
			}
		}
		if g.svc != nil {
			g.svc.LogDeletes(m.GuildID, m.ChannelID, []application.DeletedMessage{d})
		}
		return nil
	})
	// Take the message out of a pending merged request first, or a merge window ending while the delete is
	// being stored sends its words to the model. Each cleanup recovers on its own, so a panic there cannot
	// skip storing the privacy intent.
	_ = g.runReconciliationStep("merge cleanup", func() error { g.merge.remove(m.ID); return nil })
	_ = g.runReconciliationStep("picture cleanup", func() error {
		g.pictures.forget(m.ChannelID, []string{m.ID})
		g.cancelPictureJobs(m.ChannelID, []string{m.ID})
		return nil
	})
	g.applyMessageChange(messageReconciliation{Operation: "delete", GuildID: m.GuildID, ChannelID: m.ChannelID,
		MessageIDs: []string{m.ID}, At: time.Now().UTC()})
}

func (g *Gateway) cancelPictureJobs(channelID string, messageIDs []string) {
	if g.cancelPictures != nil {
		g.cancelPictures(channelID, messageIDs)
		return
	}
	g.svc.CancelPictureJobs(channelID, messageIDs)
}

func (g *Gateway) onMessageDeleteBulk(s *discordgo.Session, m *discordgo.MessageDeleteBulk) {
	defer g.recoverReconciliationPanic("bulk delete handler", nil)
	if kept := g.interims.withoutInterims(m.Messages); len(kept) != len(m.Messages) {
		if len(kept) == 0 {
			return
		}
		copied := *m
		copied.Messages = kept
		m = &copied
	}
	_ = g.runReconciliationStep("server log", func() error {
		msgs := make([]application.DeletedMessage, len(m.Messages))
		for i, id := range m.Messages {
			msgs[i] = application.DeletedMessage{ID: id}
		}
		if g.svc != nil {
			g.svc.LogDeletes(m.GuildID, m.ChannelID, msgs)
		}
		return nil
	})
	for _, id := range m.Messages { // first, as in onMessageDelete
		_ = g.runReconciliationStep("merge cleanup", func() error { g.merge.remove(id); return nil })
	}
	_ = g.runReconciliationStep("picture cleanup", func() error {
		g.pictures.forget(m.ChannelID, m.Messages)
		g.cancelPictureJobs(m.ChannelID, m.Messages)
		return nil
	})
	g.applyMessageChange(messageReconciliation{Operation: "delete", GuildID: m.GuildID, ChannelID: m.ChannelID,
		MessageIDs: append([]string(nil), m.Messages...), At: time.Now().UTC()})
}

// displayName is what people see (server nickname, then global name, then username).
func displayName(m *discordgo.MessageCreate) string {
	if m.Member != nil && strings.TrimSpace(m.Member.Nick) != "" {
		return m.Member.Nick
	}
	if strings.TrimSpace(m.Author.GlobalName) != "" {
		return m.Author.GlobalName
	}
	return m.Author.Username
}

// messageAuthorName is a fetched message's author as people see them: nickname, display name, username.
func messageAuthorName(m *discordgo.Message) string {
	if m.Member != nil && strings.TrimSpace(m.Member.Nick) != "" {
		return m.Member.Nick
	}
	if strings.TrimSpace(m.Author.GlobalName) != "" {
		return m.Author.GlobalName
	}
	return m.Author.Username
}

// capture stores the message as channel memory when both the instance and the channel allow it.
func (g *Gateway) capture(m *discordgo.MessageCreate, pol config.ChannelPolicy) {
	if m.GuildID == "" {
		return
	}
	body := bodyOf(m.Message)
	if !g.svc.Doc().Memory.Capture || !pol.Capture || body == "" || !g.svc.MemoryAllowed(m.Author.ID) {
		return
	}
	eng, err := g.svc.MemoryEngine()
	if err != nil {
		return
	}
	at := m.Timestamp
	if at.IsZero() {
		at = time.Now()
	}
	if _, err := eng.Capture(engine.LiveMessage{GuildID: m.GuildID, ChannelID: m.ChannelID, MessageID: m.ID,
		AuthorID: m.Author.ID, AuthorName: displayName(m), Content: body, At: at}); err != nil {
		g.log.Warn("discord: capture failed", "err", err)
	}
}

func (g *Gateway) mentionsBot(m *discordgo.MessageCreate) bool {
	sure, byName := g.botCall(m)
	return sure || byName
}

// botCallKind is botCall as the word participation's collect mode records: "mention" (a mention of the bot
// or a reply to it), "name" (only its name) or "".
func (g *Gateway) botCallKind(m *discordgo.MessageCreate) string {
	sure, byName := g.botCall(m)
	switch {
	case sure:
		return "mention"
	case byName:
		return "name"
	}
	return ""
}

// botCall is how a message calls the bot: surely (a mention of it, or a reply to one of its messages) or
// only by saying its name, which may be talk about the bot rather than to it ("마사몽 또 고장났네ㅋㅋ").
// The name must stand as a word (conversation.CallsName): a bot called "Sam" is not called by "Samsung".
func (g *Gateway) botCall(m *discordgo.MessageCreate) (sure, byName bool) {
	id := ""
	if p := g.botID.Load(); p != nil {
		id = *p
	}
	for _, u := range m.Mentions {
		if u != nil && u.ID == id {
			return true, false
		}
	}
	if m.ReferencedMessage != nil && m.ReferencedMessage.Author != nil && m.ReferencedMessage.Author.ID == id {
		return true, false
	}
	content := strings.ToLower(m.Content)
	for _, name := range g.svc.Doc().BotNames() {
		if n := strings.ToLower(strings.TrimSpace(name)); n != "" && conversation.CallsName(content, n) {
			return false, true
		}
	}
	return false, false
}

func (g *Gateway) shouldAnswer(m *discordgo.MessageCreate, pol config.ChannelPolicy) bool {
	sure, byName := g.botCall(m)
	if pol.Respond != "off" {
		if answer, ok := g.threadAnswer(m, pol, sure); ok {
			return answer
		}
	}
	switch pol.Respond {
	case "always":
		// a room where the bot answers everything still leaves alone what is said to someone else: a
		// reply to them, a mention of them, or their name (LIVE-13), unless the bot was called too
		return sure || !(g.toSomeone(m) || g.svc.CallsSomeoneElse(m.GuildID, m.ChannelID, m.Author.ID, m.Content))
	case "mention":
		return sure || (byName && pol.NameCall != "off")
	}
	return false
}

func (g *Gateway) runCommand(s *discordgo.Session, m *discordgo.MessageCreate, cmd Command, opts map[string]string) {
	ictx := InvokeContext{ChannelID: m.ChannelID, GuildID: m.GuildID, UserID: m.Author.ID, Locale: g.locale(m.GuildID), Manager: g.messageManager(s, m),
		Private: m.GuildID == "", UserName: displayName(m)}
	ctx, cancel := context.WithTimeout(g.runCtx(), g.ActionTimeout)
	defer cancel()
	mark := g.svc.SettledMark() // before the command reads anything: its result may go on into a chat turn
	res := g.svc.Execute(ctx, requestFor(cmd, opts, ictx, "dc:"+m.ID))
	if application.Unanswered(res) && (!ictx.Private || m.GuildID == "") { // a DM: CommandToChat checks DM chat is open
		// no answer from the tool: carry on as a conversation, off the event loop
		g.work(func(base context.Context) {
			// the reply is posted inside the turn: the conversation keeps only what reached Discord
			deliver := func(deliverCtx context.Context, out chat.Result) string {
				if deliverCtx.Err() != nil {
					return ""
				}
				if strings.TrimSpace(out.Answer) == "" {
					return "" // the command's card is shown instead
				}
				g.postToolArtifacts(deliverCtx, s, m.GuildID, m.ChannelID, m.Reference(), out.Tools)
				said, ids, rep := g.sendReply(deliverCtx, s, m.GuildID, m.ChannelID, m.Reference(), out.Answer)
				g.svc.NoteReplyMessages(out.TurnID, m.GuildID, m.ChannelID, ids)
				g.svc.NoteDelivery(out.TurnID, m.GuildID, m.ChannelID, rep)
				return said
			}
			selection, _ := g.callImages([]*discordgo.MessageCreate{m})
			if _, ok := g.commandFollowUp(base, s, res, application.ChatInput{GuildID: m.GuildID, ChannelID: m.ChannelID, MessageID: m.ID,
				Message: m.Content, Speaker: displayName(m), SpeakerID: m.Author.ID, Deliver: deliver, Images: selection.Refs, ReadFrom: mark,
				OnRefusal: func(err error) {
					var limit application.DMLimited
					if m.GuildID == "" && errors.As(err, &limit) {
						g.sendDMNotice(base, s, m, "discord.chat.dm_limited", limit.Scope)
					}
				},
				ImageTotal: selection.Total, ImageSkipped: selection.Skipped}); ok {
				return
			}
			g.sendCommandCard(base, s, m, cmd, ictx, res)
		})
		return
	}
	g.sendCommandCard(g.runCtx(), s, m, cmd, ictx, res)
}

// sendCommandCard posts a "!" command's result as its card (or a native poll). base is the daemon run:
// the post gets its own short context, so a command that used up its action timeout still shows why.
func (g *Gateway) sendCommandCard(base context.Context, s *discordgo.Session, m *discordgo.MessageCreate, cmd Command, ictx InvokeContext, res action.Result) {
	ctx, cancel := g.finalContext(base)
	defer cancel()
	r := g.renderInvoked(res, cmd, ictx)
	msg := &discordgo.MessageSend{AllowedMentions: noMentions, Reference: m.Reference()}
	if r.Poll != nil && res.OK {
		msg.Poll = r.Poll
	} else if f, failed := g.pictureFile(res, r.Embed); failed { // Discord would refuse the file, or it is gone: one line, like a queued picture
		msg.Content = g.svc.Msg(ictx.Locale, "image.failed")
	} else {
		msg.Embeds = r.Embeds()
		if r.Components != nil {
			msg.Components = r.Components
		}
		if f != nil {
			msg.Files = []*discordgo.File{f}
		}
	}
	if ctx.Err() != nil {
		return
	}
	sent, err := s.ChannelMessageSendComplex(m.ChannelID, msg, discordgo.WithContext(ctx))
	if err != nil {
		g.log.Warn("discord: send failed", "cmd", cmd.Name, "err", err)
		return
	}
	if msg.Poll != nil {
		g.svc.LinkPollMessage(pollIDOf(res.Output), m.ChannelID, sent.ID)
	}
}

// answer runs one chat turn and replies. Mentions stay disabled; long answers are split at 2000 chars.
// Turns in different channels run in parallel; the application keeps each channel in arrival order.
func (g *Gateway) answer(base context.Context, s *discordgo.Session, m *discordgo.MessageCreate, text string, call turnCall) {
	nameOnly, followUp := call.nameOnly, call.followUp
	// addressed: the person called the bot. A name alone is not a call until the name check says it was
	// one (the Addressed callback below, which the turn calls from its own goroutine); a notice goes only
	// to someone who called.
	var addressed atomic.Bool
	addressed.Store(call.addressed)
	visionNotice := ""
	// the turn's own time (chatTurnTimeout, the previous bot's pictures: p90 88 s) starts when it has its
	// place in the channel, not while it waits (ChatInput.TurnTimeout)
	ctx, cancel := context.WithTimeout(base, chatTurnCeiling)
	defer cancel()
	ctx, stopReply := context.WithCancelCause(ctx)
	defer stopReply(nil)
	defer g.turns.add(m.ChannelID, m.Author.ID, stopReply)()
	typing := newTypingKeepalive(s, m.ChannelID, g.log)
	startTyping := func() { typing.start(ctx) }
	defer typing.stop()
	// the "still looking" line of a turn that searches long. Every way out of this function deletes it (on
	// a context of its own: the turn's is cancelled by the stop word and expires on its timeout).
	still := g.newInterim(base, s, m)
	defer still.clear()
	// a call by name alone types only once the bot knows it was said to it (else it would type, then
	// stay quiet)
	if !nameOnly {
		startTyping()
	}
	// replied: something of the answer reached the channel (a part of the text or a post of a tool).
	// unsent: there was an answer to post and none of it did.
	delivered, replied, unsent := false, false, false
	// the reply is posted inside the turn, so the channel's next turn starts after it and the
	// conversation keeps only what reached Discord
	_, err := g.svc.ChatTurn(ctx, application.ChatInput{GuildID: m.GuildID, ChannelID: m.ChannelID, MessageID: m.ID, Message: text,
		Speaker: displayName(m), SpeakerID: m.Author.ID, MaxWait: chatQueueWait, TurnTimeout: chatTurnTimeout, NameOnly: nameOnly, FollowUp: followUp, Addressed: func() { addressed.Store(true); startTyping() },
		Images: call.images, ImageOnly: call.imageOnly, ImageEarlier: call.imageEarlier, ImageTotal: call.imageTotal, ImageSkipped: call.imageSkipped,
		Fragments: call.fragments, ReadFrom: call.readFrom,
		OnVisionNotice: func(note string) { visionNotice = note },
		OnProgress:     still.show,
		Deliver: func(deliverCtx context.Context, res chat.Result) string {
			typing.stop()
			still.clear() // first: the line is gone before any part of the answer is posted
			if deliverCtx.Err() != nil {
				g.dropThreadOffer(res.TurnID, "not_sent")
				unsent = strings.TrimSpace(res.Answer) != ""
				return ""
			}
			posted := g.postToolArtifacts(deliverCtx, s, m.GuildID, m.ChannelID, m.Reference(), res.Tools)
			replied = posted
			if strings.TrimSpace(res.Answer) == "" {
				g.log.Warn("discord: chat turn produced no reply text", "tools", len(res.Tools))
				delivered = true // nothing to say is not a cut-off reply
				replied = true   // nor an answer that did not come: there was none to send
				return ""
			}
			shape := g.svc.ChannelPolicyFor(m.GuildID, m.ChannelID).Conversation.ReplyShape
			sent, ids, rep := g.sendShapedFor(deliverCtx, s, m.GuildID, m.ChannelID, m.Reference(), res.Answer, visionNotice, shape, deepSearched(res.Tools))
			g.svc.NoteReplyMessages(res.TurnID, m.GuildID, m.ChannelID, ids)
			g.svc.NoteDelivery(res.TurnID, m.GuildID, m.ChannelID, rep)
			delivered = rep.Sent == rep.Parts && rep.Parts > 0
			if sent != "" || rep.Sent > 0 {
				replied = true // from here on the person has been answered, in whole or in part
			}
			unsent = !replied
			if sent != "" && m.GuildID != "" {
				g.offerThread(deliverCtx, s, m, res.TurnID, ids)
			} else {
				g.dropThreadOffer(res.TurnID, "not_sent")
			}
			return sent
		}})
	still.clear() // before any notice below: a refusal or a failure line must not stand next to "still looking"
	if errors.Is(context.Cause(ctx), errTurnStopped) {
		g.log.Debug("discord: reply stopped by the person") // quiet: no notice, no "restarting"
		return
	}
	var react application.ReactOnly
	var dmLimit application.DMLimited
	switch {
	case errors.As(err, &dmLimit):
		g.sendDMNotice(ctx, s, m, "discord.chat.dm_limited", dmLimit.Scope)
		return
	case errors.Is(err, application.ErrDMClosedChat):
		g.sendDMNotice(ctx, s, m, "discord.chat.dm_closed", "closed")
		return
	case errors.As(err, &react):
		// a playful mention answered with an emoji on the message (banter, experimental)
		g.sendBanterReaction(ctx, s, m.ChannelID, m.ID, react)
		return
	case errors.Is(err, application.ErrNotAddressed):
		g.log.Debug("discord: reply skipped", "reason", "name mentioned, not addressed")
		return
	case errors.Is(err, application.ErrChatDisabled):
		g.log.Debug("discord: reply skipped", "reason", err)
		return
	case errors.Is(err, application.ErrNoVision), errors.Is(err, application.ErrImageUnreadable):
		// only a picture, and nothing could look at it: a short honest line instead of a guess
		g.sendChatNotice(ctx, s, m, chatNoticeKey(err), addressed.Load())
		return
	case errors.Is(err, application.ErrChatLimited), errors.Is(err, application.ErrChatBusy),
		errors.Is(err, application.ErrChatPaused):
		g.log.Debug("discord: reply skipped", "reason", err)
		g.sendChatNotice(ctx, s, m, chatNoticeKey(err), addressed.Load())
		return
	case base.Err() != nil && g.stopping.Load() && !delivered && (err == nil || errors.Is(err, context.Canceled)):
		// the daemon is stopping and this turn ran out of grace — while thinking, or while its reply was
		// going out: say so instead of going quiet
		nctx, ncancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer ncancel()
		g.sendChatNotice(nctx, s, m, "discord.chat.restarting", addressed.Load())
		return
	case errors.Is(err, context.Canceled) && base.Err() != nil:
		return
	case errors.Is(err, application.ErrChatBudget):
		g.log.Warn("discord: reply skipped: monthly model budget reached")
		g.sendChatNotice(ctx, s, m, chatNoticeKey(err), addressed.Load())
		return
	case err == nil && unsent && !replied:
		// the answer was made and none of it reached the channel (the first send failed, or the turn's time
		// ran out before it): the person called the bot and got nothing, so the same short line as for a turn
		// that failed, on a context that does not carry the turn's own (possibly expired) deadline
		g.log.Warn("discord: chat answer not delivered")
		if key := chatFailureKey(errNothingDelivered, replied); key != "" && base.Err() == nil && !g.stopping.Load() {
			nctx, ncancel := g.finalContext(base)
			defer ncancel()
			g.sendChatNotice(nctx, s, m, key, addressed.Load())
		}
	case err != nil:
		g.log.Warn("discord: chat turn failed", "err", err)
		// the person called the bot and got nothing: one short line, on a context that does not carry the
		// turn's own (possibly expired) deadline. Not when the turn already answered, or the daemon stops.
		if key := chatFailureKey(err, replied); key != "" && base.Err() == nil && !g.stopping.Load() {
			nctx, ncancel := g.finalContext(base)
			defer ncancel()
			g.sendChatNotice(nctx, s, m, key, addressed.Load())
		}
	}
}

// errNothingDelivered stands for a turn that made its answer and could not post any of it.
var errNothingDelivered = errors.New("discord: the answer reached no channel")

// chatFailureKey is the notice for a turn that failed with an unexpected error (a model that timed out
// included): none once the turn has answered, and none for a cancellation.
func chatFailureKey(err error, replied bool) string {
	if err == nil || replied || errors.Is(err, context.Canceled) {
		return ""
	}
	return "discord.chat.failed"
}

func (g *Gateway) sendBanterReaction(ctx context.Context, s *discordgo.Session, channelID, messageID string, react application.ReactOnly) {
	if err := s.MessageReactionAdd(channelID, messageID, react.Emoji, discordgo.WithContext(ctx)); err == nil {
		react.Sent()
	}
}

func chatNoticeKey(err error) string {
	switch {
	case errors.Is(err, application.ErrChatLimited):
		return "discord.chat.limited"
	case errors.Is(err, application.ErrChatBusy):
		return "discord.chat.busy"
	case errors.Is(err, application.ErrChatPaused):
		return "discord.chat.paused"
	case errors.Is(err, application.ErrChatBudget):
		return "discord.chat.budget"
	case errors.Is(err, application.ErrNoVision):
		return "discord.chat.no_vision"
	case errors.Is(err, application.ErrImageUnreadable):
		return "discord.chat.image_unreadable"
	default:
		return ""
	}
}

func (g *Gateway) sendChatNotice(ctx context.Context, s *discordgo.Session, m *discordgo.MessageCreate, key string, addressed bool) {
	g.sendChatNoticeWith(ctx, m, key, addressed, func(ctx context.Context, msg *discordgo.MessageSend) error {
		_, err := s.ChannelMessageSendComplex(m.ChannelID, msg, discordgo.WithContext(ctx))
		return err
	})
}

func (g *Gateway) sendChatNoticeWith(ctx context.Context, m *discordgo.MessageCreate, key string, addressed bool, send func(context.Context, *discordgo.MessageSend) error) {
	if key == "" || !addressed || ctx.Err() != nil || m == nil || m.Author == nil || !g.claimNotice(m.Author.ID, m.ChannelID) {
		return
	}
	msg := &discordgo.MessageSend{Content: messages.T(g.noticeLocale(m), key), Reference: m.Reference(), AllowedMentions: noMentions}
	if err := send(ctx, msg); err != nil {
		g.log.Warn("discord: chat refusal notice failed", "err", err)
	}
}

// claimNotice allows one refusal notice per person per channel per chatNoticeCooldown and one per
// channel per channelNoticeCooldown, whoever it is for.
func (g *Gateway) claimNotice(userID, channelID string) bool {
	now := time.Now()
	userKey, channelKey := userID+":"+channelID, channelID
	g.noticeMu.Lock()
	defer g.noticeMu.Unlock()
	if g.notices == nil {
		g.notices = map[string]time.Time{}
	}
	if last, ok := g.notices[userKey]; ok && now.Sub(last) < chatNoticeCooldown {
		return false
	}
	if last, ok := g.notices[channelKey]; ok && now.Sub(last) < channelNoticeCooldown {
		return false
	}
	for k, last := range g.notices {
		if now.Sub(last) >= chatNoticeCooldown {
			delete(g.notices, k)
		}
	}
	g.notices[userKey], g.notices[channelKey] = now, now
	return true
}

// sendReply posts a chat answer, split at 1900 characters; only the first part replies to the message.
// It returns the text that was posted (the parts before a failed one), the IDs of the messages it
// sent (the reply's feedback is tied to them) and how the delivery went.
func (g *Gateway) sendReply(ctx context.Context, s *discordgo.Session, guildID, channelID string, ref *discordgo.MessageReference, text string) (string, []string, application.DeliveryReport) {
	return g.sendReplyParts(ctx, splitMessage(strings.TrimSpace(text), 1900), ref,
		func(ctx context.Context, msg *discordgo.MessageSend, nonce string) (*discordgo.Message, error) {
			return postReply(ctx, s, channelID, msg, nonce)
		},
		func(class errclass.Class) {
			if g.svc != nil {
				g.svc.NoteDeliveryProblem(guildID, channelID, class)
			}
		})
}

// replyPost is a reply with a nonce: discordgo's MessageSend has no such field, so the body is built
// here. With enforce_nonce Discord answers a repeated nonce with the first message instead of posting a
// second copy, so a part that was sent but whose answer was lost can be sent again safely.
type replyPost struct {
	*discordgo.MessageSend
	Nonce        string `json:"nonce,omitempty"`
	EnforceNonce bool   `json:"enforce_nonce,omitempty"`
}

func postReply(ctx context.Context, s *discordgo.Session, channelID string, msg *discordgo.MessageSend, nonce string) (*discordgo.Message, error) {
	endpoint := discordgo.EndpointChannelMessages(channelID)
	post := replyPost{MessageSend: msg, Nonce: nonce, EnforceNonce: nonce != ""}
	var body []byte
	var err error
	if len(msg.Files) > 0 { // a picture: multipart, with the same nonce on every retry
		for _, f := range msg.Files { // a retry reads the file again from its start
			if sk, ok := f.Reader.(io.Seeker); ok {
				_, _ = sk.Seek(0, io.SeekStart)
			}
		}
		contentType, raw, encErr := discordgo.MultipartBodyWithJSON(post, msg.Files)
		if encErr != nil {
			return nil, encErr
		}
		body, err = s.RequestRaw("POST", endpoint, contentType, raw, endpoint, 0, discordgo.WithContext(ctx))
	} else {
		body, err = s.RequestWithBucketID("POST", endpoint, post, endpoint, discordgo.WithContext(ctx))
	}
	if err != nil {
		return nil, err
	}
	var m discordgo.Message
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("discord: reply response: %w", err)
	}
	return &m, nil
}

// replyBackoff is the wait before each retry of a part (so a part is tried up to len+1 times).
var replyBackoff = []time.Duration{600 * time.Millisecond, 1800 * time.Millisecond}

// newNonce is a random value of at most 25 characters (Discord's limit), made once per part.
func newNonce() string {
	var b [8]byte
	if _, err := crand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}

// sleepCtx waits d, or returns false as soon as ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// sendReplyParts posts the parts in order and stops at the first one that cannot be delivered.
//   - Every part has one nonce that stays the same across its retries.
//   - Network, timeout, 5xx and rate-limit failures are retried after replyBackoff (a 429 once: discordgo
//     already waited for the limit itself).
//   - A first part refused because the message it replies to is gone is sent again at once without the
//     reference.
//   - A missing permission is not retried; noteProblem tells the operators.
func (g *Gateway) sendReplyParts(ctx context.Context, parts []string, ref *discordgo.MessageReference,
	send func(context.Context, *discordgo.MessageSend, string) (*discordgo.Message, error),
	noteProblem func(errclass.Class)) (string, []string, application.DeliveryReport) {
	var said strings.Builder
	var ids []string
	rep := application.DeliveryReport{Parts: len(parts)}
	for i, part := range parts {
		if ctx.Err() != nil {
			rep.Class = errclass.Canceled
			break
		}
		msg := &discordgo.MessageSend{Content: part, AllowedMentions: noMentions}
		if i == 0 {
			msg.Reference = ref
		}
		nonce := newNonce()
		attempt, limited := 0, false
		var sent *discordgo.Message
		var class errclass.Class
		for {
			var err error
			sent, err = send(ctx, msg, nonce)
			if err == nil {
				class = errclass.None
				break
			}
			class = classifyREST(err)
			if msg.Reference != nil && replyTargetGone(err) {
				msg.Reference, rep.ReferenceDropped = nil, true
				rep.Retries++
				continue
			}
			g.log.Warn("discord: reply part failed", "part", i+1, "of", len(parts), "attempt", attempt+1, "class", class)
			if class == errclass.RateLimited {
				if limited {
					break
				}
				limited = true
			}
			if !class.Retryable() || attempt >= len(replyBackoff) || ctx.Err() != nil {
				break
			}
			if !sleepCtx(ctx, replyBackoff[attempt]) {
				class = errclass.Canceled
				break
			}
			attempt++
			rep.Retries++
		}
		if class != errclass.None {
			rep.Class = class
			if class != errclass.Canceled && noteProblem != nil {
				noteProblem(class)
			}
			break
		}
		said.WriteString(part)
		ids = append(ids, sent.ID)
		rep.Sent++
		application.NoteFirstSend(ctx) // the turn's first_send timing: a no-op unless ctx is a chat reply's
	}
	rep.MessageIDs = ids
	return said.String(), ids, rep
}

// commandFollowUp runs a command that had no answer as a chat turn (application.CommandToChat) with the
// same time a chat reply gets; ok is false when the command's own card should be shown instead.
func (g *Gateway) commandFollowUp(base context.Context, s *discordgo.Session, res action.Result, in application.ChatInput) (chat.Result, bool) {
	ctx, cancel := context.WithTimeout(base, 180*time.Second)
	defer cancel()
	_ = s.ChannelTyping(in.ChannelID, discordgo.WithContext(ctx))
	return g.svc.CommandToChat(ctx, res, in)
}

// postToolArtifacts sends what a chat-called tool produced that is more than text: a poll the
// planner created is posted as a native Discord poll, exactly as the /poll command would, and a picture
// asked for in conversation ("고양이 그려줘") is posted like the /image command's card. It reports whether
// any of it reached the channel.
func (g *Gateway) postToolArtifacts(ctx context.Context, s *discordgo.Session, guildID, channelID string, ref *discordgo.MessageReference, results []chat.ToolResult) (posted bool) {
	for _, t := range results {
		if ctx.Err() != nil {
			return posted
		}
		if t.Tool == "images.generate" && t.OK && t.Status != "dry_run" {
			if c, ok := t.Data["card"].(action.Card); ok {
				r := renderCard(c, false)
				msg := &discordgo.MessageSend{Embeds: r.Embeds(), AllowedMentions: noMentions}
				if ref != nil {
					rc := *ref // strict: a picture whose request is gone is not posted (like the queue's)
					rc.FailIfNotExists = ptrBool(true)
					msg.Reference = &rc
				}
				// The failure line (image.failed) is posted only when the picture definitely did not go out:
				// over Discord's upload limit, the file missing, or Discord answered with a refusal. Not when
				// the request is gone, the bot lacks the permission (the operators are told, a line would not
				// get through), the turn was cancelled, or the outcome is unknown (the picture may be there).
				var f *discordgo.File
				if toolBytes(t.Data) <= application.ImageUploadLimit {
					f = g.attachment(action.Result{OK: true, Output: t.Data}, r.Embed)
				}
				if f != nil {
					msg.Files = []*discordgo.File{f}
					_, err := g.postRetryingRef(ctx, channelID, msg, true)
					if err == nil {
						posted = true
						continue
					}
					g.log.Warn("discord: chat picture post failed", "err", err)
					switch {
					case ctx.Err() != nil:
						return posted
					case errors.Is(err, application.ErrReplyTargetGone):
						continue
					case classifyREST(err) == errclass.Permission:
						continue // postRetryingRef has already noted the delivery problem
					case !errors.Is(sendError(err), application.ErrNotDelivered):
						continue // no answer from Discord: the picture may have been posted
					}
				}
				if ctx.Err() != nil {
					return posted
				}
				fail := &discordgo.MessageSend{Content: g.svc.Msg(g.locale(guildID), "image.failed"), AllowedMentions: noMentions, Reference: ref}
				if _, err := g.postRetrying(ctx, channelID, fail); err != nil {
					g.log.Warn("discord: chat picture failure line not posted", "err", err)
				} else {
					posted = true
				}
			}
			continue
		}
		if t.Tool == "reminders.propose" && t.OK && t.Status != "dry_run" {
			// the proposal with its confirm buttons: only the person who asked can press them
			if c, ok := t.Data["card"].(action.Card); ok {
				r := renderCard(c, false)
				msg := &discordgo.MessageSend{Embeds: r.Embeds(), Components: r.Components, AllowedMentions: noMentions, Reference: ref}
				if _, err := s.ChannelMessageSendComplex(channelID, msg, discordgo.WithContext(ctx)); err != nil {
					g.log.Warn("discord: reminder proposal post failed", "err", err)
				} else {
					posted = true
				}
			}
			continue
		}
		if t.Tool != "polls.create" || !t.OK || t.Status == "dry_run" {
			continue
		}
		r := Render(action.Result{OK: true, Action: t.Tool, Output: t.Data}, Command{Name: "poll", Action: t.Tool}, g.locale(guildID))
		if r.Poll == nil {
			continue
		}
		sentPoll, err := s.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{Poll: r.Poll, AllowedMentions: noMentions}, discordgo.WithContext(ctx))
		if err != nil {
			g.log.Warn("discord: chat poll post failed", "err", err)
			continue
		}
		g.svc.LinkPollMessage(pollIDOf(t.Data), channelID, sentPoll.ID)
		posted = true
	}
	return posted
}

// pictureFile is the file of a command result's generated picture. failed is true when the picture
// cannot be posted (over Discord's upload limit, which would refuse it, or its file missing or
// unreadable): the command says image.failed instead of a card claiming success. A result without a
// picture is neither.
func (g *Gateway) pictureFile(res action.Result, e *discordgo.MessageEmbed) (f *discordgo.File, failed bool) {
	id, _ := res.Output["image_id"].(string)
	if !res.OK || id == "" {
		return nil, false
	}
	if toolBytes(res.Output) > application.ImageUploadLimit {
		return nil, true
	}
	f = g.attachment(res, e)
	return f, f == nil
}

// toolBytes is the size of the picture a tool result reports (0 when it does not).
func toolBytes(data map[string]any) int {
	switch n := data["bytes"].(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

// splitMessage cuts text into parts of at most limit runes, each one a message of its own. It cuts at a
// blank line if it can, else at a line break, a sentence end or a space, and only then in the middle of
// a word; it never cuts inside a rune or an emoji sequence. A cut inside a ``` code block closes the
// block at the end of the part and opens it again (same language tag) at the start of the next, so no
// part leaves a fence open. Every part, markers included, fits the limit and none is empty (unless the
// text is: that gives one empty part). Discord folds a tall message, so a text that has to be split
// anyway is cut after softLines lines when that leaves a sizeable part and the cut is outside a code block.
func splitMessage(text string, limit int) []string {
	rest := []rune(text)
	const closer = "\n```"
	closerLen := len([]rune(closer))
	var out []string
	var prefix []rune // the code fence reopened at the start of this part
	open, info := false, ""
	for {
		if len(prefix)+len(rest) <= limit {
			if len(out) == 0 || strings.TrimSpace(string(rest)) != "" {
				out = append(out, string(prefix)+string(rest))
			}
			return out
		}
		room := limit - len(prefix)
		if room < closerLen+2 { // a limit too small for any markers: no fences
			prefix, open = nil, false
			room = limit
		}
		if n := nthLineEnd(rest, softLines); n >= room/4 && n > closerLen+2 && n < room {
			c, _ := pickCut(rest, n)
			if o, _ := fenceAfter(open, info, rest[:clearOfFenceLine(rest, c)]); !o {
				room = n
			}
		}
		cut, skip := pickCut(rest, room)
		cut = clearOfFenceLine(rest, cut)
		nowOpen, nowInfo := fenceAfter(open, info, rest[:cut])
		if nowOpen && cut > room-closerLen { // the closing marker must fit too
			cut, skip = pickCut(rest, room-closerLen)
			cut = clearOfFenceLine(rest, cut)
			nowOpen, nowInfo = fenceAfter(open, info, rest[:cut])
		}
		head := strings.TrimRight(string(rest[:cut]), " \t\r\n")
		rest = rest[cut:]
		for skip > 0 && len(rest) > 0 && (rest[0] == ' ' || rest[0] == '\n' || rest[0] == '\r' || rest[0] == '\t') {
			rest, skip = rest[1:], skip-1
		}
		if strings.TrimSpace(head) != "" {
			if nowOpen {
				head += closer
			}
			out = append(out, string(prefix)+head)
		}
		open, info = nowOpen, nowInfo
		prefix = nil
		if open {
			prefix = []rune("```" + info + "\n")
		}
	}
}

// softLines is how many lines a part of a long reply aims for (Discord folds taller messages).
const softLines = 17

// nthLineEnd is the index of the line break that ends the n-th line of r, or -1 when r has fewer lines.
func nthLineEnd(r []rune, n int) int {
	for i, c := range r {
		if c == '\n' {
			if n--; n == 0 {
				return i
			}
		}
	}
	return -1
}

// pickCut chooses where to cut rest so the head has at most max runes: the last blank line, line break,
// sentence end or space in the second half of that range, else the end of the range. It returns the
// cut index (the head is rest[:cut]) and how many whitespace runes at the cut to drop from the next part.
func pickCut(rest []rune, max int) (cut, skip int) {
	if max >= len(rest) {
		max = len(rest) - 1
	}
	if max < 1 {
		return 1, 0
	}
	lo := max / 2
	for i := max; i > lo; i-- { // blank line
		if rest[i] == '\n' && i+1 < len(rest) && rest[i+1] == '\n' {
			return i, 2
		}
	}
	for i := max; i > lo; i-- { // line break
		if rest[i] == '\n' {
			return i, 1
		}
	}
	for i := max - 1; i >= lo; i-- { // sentence end: the cut comes after the mark
		if isSentenceEnd(rest, i) && i+1 > lo {
			return i + 1, 1
		}
	}
	for i := max; i > lo; i-- { // space
		if rest[i] == ' ' || rest[i] == '\t' {
			return i, 1
		}
	}
	cut = max // hard cut, but never between the parts of one emoji or a letter and its accent
	for cut > 1 && (isJoiner(rest[cut]) || rest[cut-1] == 0x200D) {
		cut--
	}
	return cut, 0
}

// isSentenceEnd: . ! ? followed by a space or the end of the text, or the full-width marks (which have
// no space after them in Chinese and Japanese).
func isSentenceEnd(r []rune, i int) bool {
	switch r[i] {
	case '。', '！', '？':
		return true
	case '.', '!', '?':
		return i+1 >= len(r) || r[i+1] == ' ' || r[i+1] == '\n' || r[i+1] == '\t'
	}
	return false
}

// isJoiner: runes that belong to the one before them (zero-width joiner, variation selectors, skin tone
// modifiers, the keycap mark, combining marks).
func isJoiner(c rune) bool {
	return c == 0x200D || (c >= 0xFE00 && c <= 0xFE0F) || (c >= 0x1F3FB && c <= 0x1F3FF) || c == 0x20E3 || unicode.Is(unicode.Mn, c)
}

// clearOfFenceLine moves a cut that would split a line starting with ``` back to the start of that line
// (when that is not the start of the text), so a fence marker is never torn apart.
func clearOfFenceLine(r []rune, cut int) int {
	if cut >= len(r) || r[cut] == '\n' {
		return cut
	}
	start := cut
	for start > 0 && r[start-1] != '\n' {
		start--
	}
	end := cut
	for end < len(r) && r[end] != '\n' {
		end++
	}
	if start > 0 && start < cut && isFenceLine(r[start:end]) {
		return start
	}
	return cut
}

// isFenceLine: the line (or its start) opens or closes a ``` block (up to three spaces of indent).
func isFenceLine(line []rune) bool {
	i := 0
	for i < len(line) && i < 3 && line[i] == ' ' {
		i++
	}
	return i+3 <= len(line) && line[i] == '`' && line[i+1] == '`' && line[i+2] == '`'
}

// fenceAfter follows the ``` fences through seg, starting from the given state, and returns the state at
// its end: whether a block is open and its language tag.
func fenceAfter(open bool, info string, seg []rune) (bool, string) {
	start := 0
	for start <= len(seg) {
		end := start
		for end < len(seg) && seg[end] != '\n' {
			end++
		}
		if line := seg[start:end]; isFenceLine(line) {
			if open {
				open, info = false, ""
			} else {
				open, info = true, fenceInfo(line)
			}
		}
		start = end + 1
	}
	return open, info
}

// fenceInfo is the language tag after the opening ``` (the first word, kept short).
func fenceInfo(line []rune) string {
	i := 0
	for i < len(line) && (line[i] == ' ' || line[i] == '`') {
		i++
	}
	j := i
	for j < len(line) && line[j] != ' ' && line[j] != '\t' && line[j] != '\r' && line[j] != '`' {
		j++
	}
	if j-i > 20 {
		j = i + 20
	}
	return string(line[i:j])
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func toInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	}
	return 0
}

// slashText writes a slash command the way a person would type it ("/quote symbol:삼성전자"), for the
// chat turn that carries on when the command had no answer.
func slashText(cmd Command, opts []*discordgo.ApplicationCommandInteractionDataOption) string {
	parts := []string{"/" + cmd.Name}
	for _, o := range opts {
		parts = append(parts, o.Name+":"+fmt.Sprint(o.Value))
	}
	return strings.Join(parts, " ")
}
