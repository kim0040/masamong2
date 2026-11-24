package application

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"masamong/internal/chat"
	"masamong/internal/clock"
	"masamong/internal/config"
	"masamong/internal/features/reminders"
	"masamong/internal/lang"
	"masamong/internal/memory/store"
	"masamong/internal/providers"
	"masamong/internal/providers/openai"
	"masamong/internal/tools"
)

// SetEnvForTest gives this service the value an environment variable would give it, for the
// credential references "env:NAME" it resolves, without touching the process environment (a test
// that sets that cannot run in parallel). An empty value is a variable that is not set. Set it before
// the configuration that uses the reference is applied: a registry is built from what it resolves then.
func (s *Service) SetEnvForTest(t testing.TB, name, value string) {
	t.Helper()
	s.hooks.credentials.setForTest(t, name, value)
}

func (c *credentialStandIns) setForTest(t testing.TB, name, value string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
	c.refs.Store("env:"+name, "file:"+path)
}

// OpenOptionsForTest are what a test gives a service before it starts.
type OpenOptionsForTest struct {
	Env     map[string]string // variables (SetEnvForTest): the first configuration is built with them
	Version string            // the running version ("" = the build's)
}

// OpenWithOptionsForTest is Open for a service that has o from the start.
func OpenWithOptionsForTest(t testing.TB, dir string, o OpenOptionsForTest) (*Service, error) {
	t.Helper()
	return openService(dir, nil, false, nil, func(h *serviceHooks) {
		for name, value := range o.Env {
			h.credentials.setForTest(t, name, value)
		}
		if o.Version != "" {
			h.version.Store(&o.Version)
		}
	})
}

// CredentialForTest reads a credential reference the way this service does (SetEnvForTest applies).
func (s *Service) CredentialForTest(ref string) (providers.Credential, error) {
	return s.credential(ref)
}

// ReopenForTest opens prev's data root again the way a restart would: the environment's variables that
// SetEnvForTest gave prev are still there, and so is the running version.
func ReopenForTest(prev *Service, clk clock.Clock, webBuild bool) (*Service, error) {
	return openService(prev.DataRoot, clk, webBuild, nil, func(h *serviceHooks) {
		h.version.Store(prev.hooks.version.Load())
		prev.hooks.credentials.refs.Range(func(k, v any) bool {
			h.credentials.refs.Store(k, v)
			return true
		})
	})
}

// SetDocForTest swaps the live configuration without the apply flow (no revision, no audit), for
// tests that need a shape config.apply would refuse.
func (s *Service) SetDocForTest(doc config.Document) { s.setDoc(doc, s.Revision()) }

// SetPendingRecountGapForTest shortens the least time between two counts of messages waiting for a vector.
func (s *Service) SetPendingRecountGapForTest(d time.Duration) {
	s.pendingMsgs.mu.Lock()
	s.pendingMsgs.gap = d
	s.pendingMsgs.mu.Unlock()
}

// MemoryPassForTest runs one tick of the memory index worker.
func (s *Service) MemoryPassForTest(ctx context.Context) { s.memoryPass(ctx) }

// IndexQueueForTest is the recorded index queue (rooms waiting for a pass).
func (s *Service) IndexQueueForTest() []string { return s.queuedIndexRooms() }

// ToolRegistryForTest is the tools the service offers, with the operator's settings applied.
func (s *Service) ToolRegistryForTest() []tools.Resolved { return s.toolRegistry() }

// ToolCallStatusesForTest lists a turn's call-ledger tool rows as "tool:status".
func ToolCallStatusesForTest(s *Service, turnID string) []string {
	db, err := s.adminDB()
	if err != nil {
		return nil
	}
	rows, err := db.DB.Query(`SELECT tool, status FROM tool_calls WHERE turn_id=? ORDER BY id`, turnID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var tool, status string
		if rows.Scan(&tool, &status) == nil {
			out = append(out, tool+":"+status)
		}
	}
	return out
}

// ImageDropsForTest counts the picture jobs dropped so far (policy, deletion, shutdown).
func ImageDropsForTest(s *Service) int64 { return s.imgDrops.Load() }

// ImageQueueStateForTest is how many picture jobs wait and whether one is being drawn.
func ImageQueueStateForTest(s *Service) (waiting int, running bool) { return s.imageQueueState() }

// SetImageConnectWaitForTest shortens how long the service's picture jobs wait for a reconnecting
// gateway. Call it before the jobs start.
func SetImageConnectWaitForTest(s *Service, wait, poll time.Duration) {
	s.hooks.imageConnectWait, s.hooks.imageConnectPoll = wait, poll
}

// SetImageTurnWaitForTest shortens how long the service's finished pictures wait for their turn's reply.
// Call it before the jobs are queued.
func SetImageTurnWaitForTest(s *Service, wait time.Duration) { s.hooks.imageTurnWait = wait }

// ChatToolInTurnForTest is ChatToolForTest for a call made inside a turn that ends when ended is closed.
func ChatToolInTurnForTest(s *Service, in ChatInput, ended chan struct{}) chat.ToolRunner {
	in.turnEnded = ended
	return s.chatTool(in, chat.Voice{Pack: lang.For("en")})
}

// RunFortuneMorningForTest runs one pass of the morning briefings (the scheduler runs it every minute).
func RunFortuneMorningForTest(s *Service, ctx context.Context) { s.runFortuneMorning(ctx) }

// SetChatDeadlineForTest shortens how long one model call of a role may take in s; it returns the restore.
func SetChatDeadlineForTest(s *Service, role string, d time.Duration) func() {
	old := s.hooks.chatDeadlines.Load()
	m := map[string]time.Duration{}
	if old != nil {
		for k, v := range *old {
			m[k] = v
		}
	}
	m[role] = d
	s.hooks.chatDeadlines.Store(&m)
	return func() { s.hooks.chatDeadlines.Store(old) }
}

// FailChatToolForTest makes a chat tool answer "unavailable" in one channel; it returns the undo.
func FailChatToolForTest(s *Service, channelID, tool string) func() {
	key := faultKey(s, channelID, tool)
	s.toolFaults.Store(key, true)
	return func() { s.toolFaults.Delete(key) }
}

// ChatResultForTest is a chat turn's result.
type ChatResultForTest = chat.Result

// DisasterTickForTest runs one scheduler minute of the disaster alerts (earthquakes, and warnings when
// five minutes have passed since the last warning check).
func DisasterTickForTest(s *Service, ctx context.Context) { s.disasterTick(ctx) }

// SetReferencePricesForTest points the reference price list at a local server.
func SetReferencePricesForTest(s *Service, url string) { s.referenceURL = url }

// SetDiscordAPIForTest points discord.check at a local server.
func SetDiscordAPIForTest(s *Service, base string) { s.discordAPI = base }

// AllowLocalImagesForTest lets a test serve the pictures of a chat turn from a local server.
func AllowLocalImagesForTest(s *Service) { s.visionLocal = true }

// AllowLocalNoticeBoardsForTest lets a test read boards from a local server.
func AllowLocalNoticeBoardsForTest(s *Service) { s.noticeFetchPrivate = true }

// CollectNoticesForTest runs one collection of the boards given (all needed ones when empty).
func CollectNoticesForTest(s *Service, ctx context.Context, only []string) (CollectReport, error) {
	return s.collectNotices(ctx, only)
}

// DeliverNoticesForTest runs one pass of the notice deliveries.
func DeliverNoticesForTest(s *Service, ctx context.Context) { s.deliverNotices(ctx) }

// DeliverRemindersForTest runs one pass of the reminder deliveries; RecoverReminders is the start-up step.
func DeliverRemindersForTest(s *Service, ctx context.Context) { s.deliverReminders(ctx) }
func RecoverRemindersForTest(s *Service)                      { s.recoverReminders() }

// SetParticipationWait shortens the quiet wait of participation candidates.
func SetParticipationWait(s *Service, d time.Duration) {
	s.part.mu.Lock()
	s.part.wait = func(string) time.Duration { return d }
	s.part.mu.Unlock()
}

// ManualParticipationForTest returns steps for the quiet wait and the work queued by Observe. A stopped
// timer preserves cancellation and shutdown, and draining only Observe's work leaves service loops alone.
func ManualParticipationForTest(s *Service) (finishWait func(guildID, channelID string) bool, drain func()) {
	var queued []func(context.Context)
	s.part.mu.Lock()
	s.part.afterFunc = func(time.Duration, func()) *time.Timer {
		timer := time.NewTimer(time.Hour)
		timer.Stop()
		return timer
	}
	s.part.startWork = func(_ string, fn func(context.Context)) { queued = append(queued, fn) }
	s.part.mu.Unlock()
	finishWait = func(guildID, channelID string) bool {
		s.part.mu.Lock()
		c := s.part.chans[guildID+":"+channelID]
		var cand *partCandidate
		if c != nil {
			cand = c.pending
		}
		s.part.mu.Unlock()
		if cand == nil {
			return false
		}
		s.considerCandidate(cand)
		return true
	}
	drain = func() {
		s.part.mu.Lock()
		work := queued
		queued = nil
		s.part.mu.Unlock()
		for _, fn := range work {
			fn(context.Background())
		}
	}
	return finishWait, drain
}

// CollectWatchingRowForTest reports whether a particular collected row still waits for its next event.
func CollectWatchingRowForTest(s *Service, rowID string) bool {
	s.part.mu.Lock()
	defer s.part.mu.Unlock()
	for _, c := range s.part.chans {
		for _, w := range c.watches {
			if w.row == rowID {
				return true
			}
		}
	}
	return false
}

// SetCollectNextWindow shortens how long a collected row waits for what happened next.
func SetCollectNextWindow(s *Service, d time.Duration) {
	s.part.mu.Lock()
	s.part.nextWindow = d
	s.part.mu.Unlock()
}

// SweepCollectNext is the scheduler's minute step for collected rows.
func SweepCollectNext(s *Service) { s.sweepCollectNext() }

// CollectWatching is how many collected rows still wait for what happened next.
func CollectWatching(s *Service) int {
	s.part.mu.Lock()
	defer s.part.mu.Unlock()
	n := 0
	for _, c := range s.part.chans {
		n += len(c.watches)
	}
	return n
}

// MarkReminderSendingForTest leaves a reminder as a stop in the middle of its send would.
func MarkReminderSendingForTest(s *Service, id string) {
	db, _ := s.adminDB()
	_, _ = db.DB.Exec(`UPDATE reminders SET status='sending' WHERE id=?`, id)
}

// SaveOfferForTest records a plan offer read from text, as participation would.
func SaveOfferForTest(s *Service, guildID, channelID, text string) (string, error) {
	loc := s.instanceZone()
	w, err := reminders.Parse(text, s.Clock.Now(), loc, lang.For("ko", "ko").Time, reminders.Options{})
	if err != nil {
		return "", err
	}
	return s.saveReminderOffer(guildID, channelID, "m1", w.Rest, w.Matched, loc, w)
}

// AlertTickForTest runs one pass of the operator alerts; NoteStartForTest records a daemon start.
func (s *Service) AlertTickForTest(ctx context.Context) { s.alertTick(ctx) }
func (s *Service) NoteStartForTest()                    { s.noteUptime("start", "") }

// UseUpdateAPIForTest points s's release check at url (with %s for the repository).
func UseUpdateAPIForTest(s *Service, url string) {
	s.hooks.updateAPI.Store(&url)
}

// NewerVersionForTest exposes the version comparison.
func NewerVersionForTest(latest, current string) bool { return newerVersion(latest, current) }

// UseSelfUpdateForTest points s's release downloads at a local server and its running programs at dir.
func UseSelfUpdateForTest(s *Service, downloadPrefix, dir string) (restore func()) {
	oldExe := s.hooks.executable.Load()
	s.hooks.downloads.Store(&downloadPrefix)
	exe := func() (string, error) { return filepath.Join(dir, exeName("masamongd")), nil }
	s.hooks.executable.Store(&exe)
	return func() { s.hooks.executable.Store(oldExe) }
}

// UpdateAssetNameForTest is this computer's release archive name.
func UpdateAssetNameForTest() string { return updateAssetName() }

// SetVersionForTest changes the running version of s; it returns the restore.
func (s *Service) SetVersionForTest(v string) (restore func()) {
	old := s.hooks.version.Load()
	s.hooks.version.Store(&v)
	return func() { s.hooks.version.Store(old) }
}

// ForgetThreadsForTest empties the in-memory thread list, as a restart or the closed-thread sweep does.
func ForgetThreadsForTest(s *Service) {
	s.threads.mu.Lock()
	s.threads.threads = map[string]*threadInfo{}
	s.threads.mu.Unlock()
}

// ReminderContextForTest is the conversation lines a reminder shows when it fires.
func ReminderContextForTest(s *Service, id string) string {
	r, ok := s.reminderByID(id)
	if !ok {
		return ""
	}
	return s.reminderContextLines(r)
}

// ReminderContextRowsForTest is how many context rows are stored for a reminder (shown or not).
func ReminderContextRowsForTest(s *Service, id string) int {
	db, _ := s.adminDB()
	var n int
	_ = db.DB.QueryRow(`SELECT COUNT(*) FROM reminder_context WHERE reminder_id=?`, id).Scan(&n)
	return n
}

// DueSnapshot is the due reminders as one pass read them.
type DueSnapshot struct{ rows []reminder }

// SnapshotDueForTest reads the due list without delivering it; DeliverSnapshotForTest delivers a list read
// earlier, as a second process that read the table before another one leased would.
func SnapshotDueForTest(s *Service) DueSnapshot {
	rows, _ := s.listReminders(`status='scheduled' AND due_at<=?`, []any{s.Clock.Now().UTC().Format(time.RFC3339)}, 50)
	return DueSnapshot{rows}
}
func DeliverSnapshotForTest(s *Service, ctx context.Context, snap DueSnapshot) {
	s.deliverRows(ctx, snap.rows, s.Clock.Now())
}

// ChatToolForTest is the runner the chat tool loop uses for tools, for one turn.
func ChatToolForTest(s *Service, in ChatInput) chat.ToolRunner {
	return s.chatTool(in, chat.Voice{Pack: lang.For("en")})
}

// TaskToolForTest is the runner a scheduled task's turn uses for tools.
func TaskToolForTest(s *Service, in ChatInput) chat.ToolRunner {
	return s.turnTool(in, chat.Voice{Pack: lang.For("en")}, viaTask)
}

// CapContextForTest applies the limits on a reminder's conversation lines to author/text pairs.
func CapContextForTest(pairs [][2]string) [][2]string {
	var in []reminderContextLine
	for _, p := range pairs {
		in = append(in, reminderContextLine{Author: p[0], Text: p[1]})
	}
	var out [][2]string
	for _, l := range capContext(in) {
		out = append(out, [2]string{l.Author, l.Text})
	}
	return out
}

// RedactionJobForTest runs the erasure worker's pass for one job, as the durable queue would: a deletion
// or edit of messages, or (user set) a person's withdrawal.
func RedactionJobForTest(s *Service, ctx context.Context, operation, guild, channel, user string, messages ...string) error {
	db, err := s.adminDB()
	if err != nil {
		return err
	}
	return s.processRedactions(ctx, db, []redactionJob{{Guild: guild, Channel: channel, Messages: messages, Operation: operation, User: user}})
}

// MarkVisionRefusedForTest records that a provider's model has just refused pictures.
func MarkVisionRefusedForTest(s *Service, provider, model string) {
	s.visionRefused.Store(visionKey(chatRoute{provider: provider, model: model}), s.Clock.Now())
}

// MarkVisionCoolingForTest records that a provider's vision model has just failed on the wire.
func MarkVisionCoolingForTest(s *Service, provider, model string) {
	s.visionCooling.Store(visionKey(chatRoute{provider: provider, model: model}), s.Clock.Now())
}

// VisionAdapterForTest is the vision role's kept adapter for doc (the one chatRoutes uses).
func VisionAdapterForTest(s *Service, doc config.Document) (*openai.Adapter, error) {
	return s.visionAdapter(doc)
}

// TickParticipationSweepForTest runs the minute tick's per-channel sweep as of now.
func TickParticipationSweepForTest(s *Service, now time.Time) {
	s.part.mu.Lock()
	s.sweepPartChannels(now)
	s.part.mu.Unlock()
}

// AddParticipationChannelsForTest adds n empty participation channel entries last used at touched, as a
// server with many channels would.
func AddParticipationChannelsForTest(s *Service, n int, touched time.Time) {
	s.part.mu.Lock()
	defer s.part.mu.Unlock()
	if s.part.chans == nil {
		s.part.chans = map[string]*partChannel{}
	}
	for i := 0; i < n; i++ {
		s.part.gen++
		s.part.chans[fmt.Sprintf("filler:%d", i)] = &partChannel{gen: s.part.gen, touched: touched, filtered: map[string]int{}}
	}
}

// ParticipationChannelCountForTest is how many channel entries the participation engine holds.
func ParticipationChannelCountForTest(s *Service) int {
	s.part.mu.Lock()
	defer s.part.mu.Unlock()
	return len(s.part.chans)
}

// CapsCacheForTest is provider's capability cache, as an adapter built now is given it (shareCaps).
func CapsCacheForTest(s *Service, provider string) *openai.CapsCache {
	return s.modelCaps(provider, s.Doc().Providers[provider])
}

// LookupModelCapsForTest runs the role-model lookup a provider test starts in the background.
func LookupModelCapsForTest(s *Service, ctx context.Context, provider string) {
	s.lookupModelCaps(ctx, provider)
}

// SlowReadForTest logs one read that took longer than lapSlow, as a console read (background false) or as
// the alert loop's own (true).
func (s *Service) SlowReadForTest(ctx context.Context, background bool) {
	if background {
		ctx = withBackgroundRead(ctx)
	}
	l := s.laps(ctx, "probe")
	l.start = time.Now().Add(-lapSlow - time.Second)
	l.done()
}

// RunRetentionForTest runs one retention pass.
func (s *Service) RunRetentionForTest(ctx context.Context) (map[string]int64, error) {
	return s.runRetention(ctx)
}

// SetSearchDeadlineForTest shortens how long s's search.web waits for one service at a depth; it returns
// the restore.
func SetSearchDeadlineForTest(s *Service, depth string, d time.Duration) func() {
	return s.changeSearchLimits(func(l *searchLimits) { l.deadline[depth] = d })
}

// changeSearchLimits gives s a changed copy of its search limits; it returns the restore.
func (s *Service) changeSearchLimits(change func(*searchLimits)) func() {
	old := s.hooks.search.Load()
	l := s.searchLimits().clone()
	change(l)
	s.hooks.search.Store(l)
	return func() { s.hooks.search.Store(old) }
}

// AnnounceUpdatesForTest runs one pass of the update-news announcement.
func (s *Service) AnnounceUpdatesForTest(ctx context.Context) { s.announceUpdates(ctx) }

// AnnouncedForTest is the last version announced ("" when none is recorded); SetAnnouncedForTest writes it.
func (s *Service) AnnouncedForTest() string {
	var v string
	_, _ = s.kvGet(updatesAnnouncedKey, &v)
	return v
}
func (s *Service) SetAnnouncedForTest(v string) { _ = s.kvSet(updatesAnnouncedKey, v) }

// NewsSentForTest is the ledger: target -> state for a version.
func (s *Service) NewsSentForTest(version string) map[string]string {
	out := map[string]string{}
	db, err := s.adminDB()
	if err != nil {
		return out
	}
	rows, err := db.DB.Query(`SELECT target, state FROM update_news_sent WHERE version=?`, version)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var t, st string
		if rows.Scan(&t, &st) == nil {
			out[t] = st
		}
	}
	return out
}

// ClaimNewsForTest records a send as begun, the way a crash between the claim and the send leaves it.
func (s *Service) ClaimNewsForTest(version, target string) {
	if db, err := s.adminDB(); err == nil {
		_, _ = db.DB.Exec(`INSERT OR IGNORE INTO update_news_sent(version, target, state, at) VALUES(?,?,?,?)`, version, target, "sending", "2026-11-22T00:00:00Z")
	}
}

// UseNewsLimitsForTest sets s's pause between sends and the sends per pass; it returns the restore.
func UseNewsLimitsForTest(s *Service, pace time.Duration, perTick int) (restore func()) {
	old := s.hooks.news.Load()
	s.hooks.news.Store(&newsLimits{pace: pace, perTick: perTick})
	return func() { s.hooks.news.Store(old) }
}

// SimulatePreparedCrashForTest writes a prepared-but-uncommitted revision, the way a crash between
// config.BeginApply and the commit leaves the store; only cas_recover_test.go uses it.
func (s *Service) SimulatePreparedCrashForTest(candidate []byte, base int) (config.Revision, error) {
	st, err := loadCASState(s.Store, s.CfgPath)
	if err != nil {
		return config.Revision{}, err
	}
	prep, err := config.BeginApply(st, base, candidate)
	if err != nil {
		return config.Revision{}, err
	}
	if err := persistRevision(s.Store, prep); err != nil {
		return config.Revision{}, err
	}
	return prep, nil
}

// RecoverAfterCrashForTest replays the startup recovery path a crashed apply leaves behind.
func (s *Service) RecoverAfterCrashForTest() error {
	if err := s.reloadConfig(); err != nil {
		return err
	}
	if s.Revision() == 0 {
		return fmt.Errorf("no committed revision after recover")
	}
	return nil
}

// ConversationLogForTest lists the decisions of one kind, oldest first.
func (s *Service) ConversationLogForTest(kind string) []string {
	db, err := s.adminDB()
	if err != nil {
		return nil
	}
	rows, err := db.DB.Query(`SELECT decision FROM conversation_log WHERE kind=? ORDER BY created_at, rowid`, kind)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		_ = rows.Scan(&d)
		out = append(out, d)
	}
	return out
}

// ChannelRoomsForTest is a channel's memory scope.
func (s *Service) ChannelRoomsForTest(guildID, channelID string) []string {
	return s.channelRooms(guildID, channelID)
}

// HoldRedactionForTest keeps the redaction worker from starting, or lets it go again.
func (s *Service) HoldRedactionForTest(hold bool) { s.redaction.running.Store(hold) }

// QueueEditForTest commits an edit's intent, without the foreground apply MessageEdited does after it: a
// worker that has not reached the job yet.
func (s *Service) QueueEditForTest(guildID, channelID, messageID, content string, at time.Time) error {
	return s.queueSourceChange(guildID, channelID, []string{messageID}, "edit", content, at)
}

// SettledSourcesForTest lists the sources whose edits are finished.
func (s *Service) SettledSourcesForTest() []string {
	rows, err := s.Store.DB.Query(`SELECT source_id FROM privacy_settled`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			out = append(out, id)
		}
	}
	return out
}

// KeepLateReplyForTest writes a reply holding these sources, as a turn starting now would, and reports whether
// it was kept.
func (s *Service) KeepLateReplyForTest(guildID, channelID, native, speaker string, sources []string) bool {
	eng, err := s.MemoryEngine()
	if err != nil {
		return false
	}
	key := "discord:" + guildID + ":" + channelID
	if err := eng.Store.AppendChatTurns(key, []store.ChatTurn{{Role: "assistant", Content: "late", NativeID: native, SpeakerID: speaker, SourceIDs: sources, ReadAsOf: store.SettledMark(s.Store)}}, 40); err != nil {
		return false
	}
	var n int
	if s.Store.DB.QueryRow(`SELECT COUNT(*) FROM chat_turns WHERE channel_key=? AND native_id=? AND role='assistant'`, key, native).Scan(&n) != nil {
		return false
	}
	return n > 0
}

// TurnsInFlightForTest counts the entries of the running turns' registry.
func (s *Service) TurnsInFlightForTest() int {
	s.flights.mu.Lock()
	defer s.flights.mu.Unlock()
	return len(s.flights.byMessage)
}

// NoteEditForTest does the first thing MessageEdited does, without the rest: the moment before its intent commits.
func (s *Service) NoteEditForTest(messageID, words string, at time.Time) {
	s.flights.edited(messageID, words, at)
}

// NotedWordsForTest is what the running turns of the message have been told.
func (s *Service) NotedWordsForTest(messageID string) string {
	s.flights.mu.Lock()
	defer s.flights.mu.Unlock()
	for _, fl := range s.flights.byMessage[messageID] {
		return fl.words
	}
	return ""
}

// SeedThreadForTest copies the caller's turns of a channel to a new thread.
func (s *Service) SeedThreadForTest(guildID, parentID, threadID, callerID string) int {
	return len(s.seedThread(guildID, parentID, threadID, callerID))
}

// ChannelLineIDsForTest names the lines a turn in the channel would choose its conversation from.
func (s *Service) ChannelLineIDsForTest(guildID, channelID, messageID string) []string {
	eng, err := s.MemoryEngine()
	if err != nil {
		return nil
	}
	lines, _, _ := s.channelLines(ChatInput{GuildID: guildID, ChannelID: channelID, MessageID: messageID}, eng)
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = l.ID
	}
	return out
}

// ThreadBackedOffForTest reports whether the channel rests from thread offers at the service's clock.
func (s *Service) ThreadBackedOffForTest(guildID, channelID string) bool {
	return s.threadBackedOff(guildID, channelID, s.Clock.Now())
}

// LogThreadOfferForTest writes an "offer" row of the decision log at the given time.
func (s *Service) LogThreadOfferForTest(guildID, channelID string, at time.Time) {
	db, err := s.adminDB()
	if err != nil {
		return
	}
	_, _ = db.DB.Exec(`INSERT INTO conversation_log(id,guild_id,channel_id,created_at,kind,decision) VALUES(?,?,?,?,?,?)`,
		newID("cv_"), guildID, channelID, at.UTC().Format(time.RFC3339Nano), convThread, "offer")
}

// SetFetchSoftForTest shortens s's soft deadline of page reading (once two pages are in); it returns
// the restore.
func SetFetchSoftForTest(s *Service, d time.Duration) func() {
	return s.changeSearchLimits(func(l *searchLimits) { l.fetchSoft = d })
}

// SearchDeadlineForTest is how long search.web waits for one service of a kind at a depth.
func SearchDeadlineForTest(kind, depth string) time.Duration {
	return defaultSearchLimits.timeout(kind, depth)
}

// SearchTotalForTest is how long a whole search at a depth may take across the services it tries.
func SearchTotalForTest(depth string) time.Duration { return defaultSearchLimits.totalTimeout(depth) }

// SetSearchLimitsForTest sets one service's limit and the whole search's limit at a depth in s, and the
// time under which the next service is not asked; it returns the restore.
func SetSearchLimitsForTest(s *Service, depth string, own, total, minLeft time.Duration) func() {
	return s.changeSearchLimits(func(l *searchLimits) {
		l.deadline[depth], l.total[depth], l.minLeft = own, total, minLeft
		l.deadlineKind = map[string]time.Duration{}
	})
}

// SetFetchDeadlineForTest sets s's hard limit of page reading at a depth; it returns the restore.
func SetFetchDeadlineForTest(s *Service, depth string, d time.Duration) func() {
	return s.changeSearchLimits(func(l *searchLimits) { l.fetchHard[depth] = d })
}

// SetFetchBaseForTest points s's TinyFish Fetch at a test server (before its first page is read).
func SetFetchBaseForTest(s *Service, url string) {
	s.hooks.fetchBase.Store(&url)
}

// SetProbeTimeoutForTest shortens the limit of s's connection checks.
func SetProbeTimeoutForTest(s *Service, d time.Duration) {
	s.hooks.probeTimeout.Store(int64(d))
}
