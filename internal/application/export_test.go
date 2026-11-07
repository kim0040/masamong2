package application

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"masamong/internal/chat"
	"masamong/internal/config"
	"masamong/internal/features/reminders"
	"masamong/internal/lang"
	"masamong/internal/providers/openai"
	"masamong/internal/tools"
)

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

// SetImageConnectWaitForTest shortens how long a picture job waits for a reconnecting gateway.
func SetImageConnectWaitForTest(wait, poll time.Duration) (restore func()) {
	w, p := imageConnectWait, imageConnectPoll
	imageConnectWait, imageConnectPoll = wait, poll
	return func() { imageConnectWait, imageConnectPoll = w, p }
}

// RunFortuneMorning runs one pass of the morning briefings (the scheduler runs it every minute).
func RunFortuneMorning(s *Service, ctx context.Context) { s.runFortuneMorning(ctx) }

// SetChatDeadlineForTest shortens how long one model call of a role may take; it returns the restore.
func SetChatDeadlineForTest(role string, d time.Duration) func() {
	old := chatDeadline[role]
	chatDeadline[role] = d
	return func() { chatDeadline[role] = old }
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

// AllowLocalNoticeBoards lets a test read boards from a local server.
func AllowLocalNoticeBoards(s *Service) { s.noticeFetchPrivate = true }

// CollectNotices runs one collection of the boards given (all needed ones when empty).
func CollectNotices(s *Service, ctx context.Context, only []string) (CollectReport, error) {
	return s.collectNotices(ctx, only)
}

// DeliverNotices runs one pass of the notice deliveries.
func DeliverNotices(s *Service, ctx context.Context) { s.deliverNotices(ctx) }

// DeliverReminders runs one pass of the reminder deliveries; RecoverReminders is the start-up step.
func DeliverReminders(s *Service, ctx context.Context) { s.deliverReminders(ctx) }
func RecoverReminders(s *Service)                      { s.recoverReminders() }

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

// UseUpdateAPIForTest points the release check at url (with %s for the repository) and sets the version.
func UseUpdateAPIForTest(url, version string) (restore func()) {
	oldURL, oldVer := updateAPI, Version
	updateAPI, Version = url, version
	return func() { updateAPI, Version = oldURL, oldVer }
}

// NewerVersionForTest exposes the version comparison.
func NewerVersionForTest(latest, current string) bool { return newerVersion(latest, current) }

// UseSelfUpdateForTest points release downloads at a local server and the running programs at dir.
func UseSelfUpdateForTest(downloadPrefix, dir string) (restore func()) {
	oldPrefix, oldSelf := updateDownloadPrefix, selfExecutable
	updateDownloadPrefix = downloadPrefix
	selfExecutable = func() (string, error) { return filepath.Join(dir, exeName("masamongd")), nil }
	return func() { updateDownloadPrefix, selfExecutable = oldPrefix, oldSelf }
}

// UpdateAssetNameForTest is this computer's release archive name.
func UpdateAssetNameForTest() string { return updateAssetName() }

// SetVersionForTest changes the running version.
func SetVersionForTest(v string) (restore func()) {
	old := Version
	Version = v
	return func() { Version = old }
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

// SetSearchDeadlineForTest shortens how long search.web waits for one service at a depth; it returns the restore.
func SetSearchDeadlineForTest(depth string, d time.Duration) func() {
	old := searchDeadline[depth]
	searchDeadline[depth] = d
	return func() { searchDeadline[depth] = old }
}
