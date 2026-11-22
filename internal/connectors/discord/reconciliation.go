package discord

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"masamong/internal/application"
	"masamong/internal/fsutil"
)

// messageReconciliationFile is the first format: one JSON array rewritten whole on every failed
// message change. It is only read now, once, to move its events into the directory below.
const messageReconciliationFile = "discord-message-reconciliation.json"

// messageReconciliationDir holds one durable file per pending change, so a failed edit or delete costs
// one small write whatever is already waiting, and the drain holds one event in memory at a time.
//
//	e-<hash>.json  the latest edit of one message; the name comes from the message, so a newer edit
//	               replaces the older one in place and a message never has more than one
//	d-<seq>.json   one delete (or bulk delete); a delete also removes the edit files of its messages
//
// Nothing is dropped to keep the queue small: only edits that a newer edit or a delete made pointless
// are ever removed.
const messageReconciliationDir = "discord-message-reconciliation"

var messageRetryBackoff = [...]time.Duration{100 * time.Millisecond, 400 * time.Millisecond}

var errReconciliationBlocked = errors.New("discord: unreadable reconciliation intents require recovery")
var errReconciliationFormat = errors.New("discord: invalid reconciliation array or record")

const (
	// The background drain retries a failing store with this growing wait, so a store that stays down
	// costs a few attempts an hour instead of a pass over the whole queue every few seconds.
	reconciliationDrainInitial = 5 * time.Second
	reconciliationDrainMax     = 5 * time.Minute
	// A pass gives up after this many failures in a row: the store is most likely down, and every
	// further attempt would only wait for the same error.
	reconciliationPassFailures = 5
	reconciliationBatch        = 64
)

type messageReconciliation struct {
	Operation  string    `json:"operation"`
	GuildID    string    `json:"guild_id"`
	ChannelID  string    `json:"channel_id"`
	MessageIDs []string  `json:"message_ids"`
	Content    string    `json:"content,omitempty"`
	At         time.Time `json:"at"`
	// SameFiles says the edit kept the message's attachments, as far as the connector can tell. Without it (an old
	// queue file, a message no longer in the state cache) the edit is applied in full whatever its words: a reply may
	// be built on a picture the edit took away.
	SameFiles bool `json:"same_files,omitempty"`
}

// messageReconciler owns the durable queue of message changes that could not be applied when they
// arrived: the files, their locks, the recovery state, and the background drain with its backoff. The
// gateway only says where the data lives and how one change is applied (reconcilerHost). It applies a
// change outside the queue lock: the lock guards the files, never a call into the store.
type messageReconciler struct {
	host     reconcilerHost
	bindOnce sync.Once
	mu       sync.Mutex
	// refreshMu orders snapshots without holding up queue writes during legacy scans
	refreshMu             sync.Mutex
	legacyQuarantineCache map[string]legacyQuarantineClassification // guarded by refreshMu
	// editOrderMu orders a live edit against the drain's apply of a queued one for the same bot: without
	// it a queued older edit read just before a newer live edit went through could be applied after it
	// and bring back the words the newer edit removed.
	editOrderMu sync.Mutex
	// seq names delete files; pending is true while a deferred change may be on disk (so a delete that
	// succeeds knows to look for edits it supersedes); wake tells the background drain a delete or an idle
	// queue needs work (nil in tests that drain by hand).
	seq        atomic.Uint64
	pending    atomic.Bool
	blocked    atomic.Int64
	deletes    atomic.Bool
	legacy     atomic.Bool
	stateReady bool      // guarded by mu
	blockedLog time.Time // guarded by mu
	wake       chan struct{}
	backingOff atomic.Bool
	// ctx, cancel and done belong to the background drain; the gateway sets and reads them under its
	// work admission lock (startReconciliationDrain, Wait, shutdown).
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	// test hooks; nil means the real behaviour
	wait         func(context.Context, time.Duration) bool // retry clock; nil waits on the context
	jitter       func(time.Duration) time.Duration         // drain backoff; nil uses jittered
	remove       func(string) error                        // nil uses durable removal
	write        func(string, messageReconciliation) error // nil uses atomic writes
	read         func(string) ([]byte, error)              // nil reads the queued file
	legacyStep   func(int)                                 // pauses migration before a record is queued
	legacyReader func(io.Reader) io.Reader                 // observes legacy scans and injects decoder read failures
}

// reconcilerHost is what the queue needs from the gateway.
type reconcilerHost interface {
	reconciliationRoot() (string, error)
	// applyMessageEvent applies a live change; applyQueuedMessageEvent applies a queued one as gateway
	// work, refusing once shutdown has begun.
	applyMessageEvent(messageReconciliation) error
	applyQueuedMessageEvent(messageReconciliation) error
	runCtx() context.Context
	logger() *slog.Logger
}

// bind attaches the host once. The reconciler is a field of the gateway, so a gateway built as a literal
// (tests) gets a working queue on first use.
func (r *messageReconciler) bind(host reconcilerHost) *messageReconciler {
	r.bindOnce.Do(func() { r.host = host })
	return r
}

func (r *messageReconciler) log() *slog.Logger {
	return r.host.logger()
}

// Keep panic values out of diagnostics: callbacks can carry private message data.
func recoverReconciliationPanic(log *slog.Logger, stage string, err *error) {
	if recover() != nil {
		failure := errors.New("discord: reconciliation " + stage + " panicked")
		if err != nil {
			*err = failure
		}
		log.Error(failure.Error(), "stack", string(debug.Stack()))
	}
}

func runReconciliationStep(log *slog.Logger, stage string, step func() error) (err error) {
	defer recoverReconciliationPanic(log, stage, &err)
	return step()
}

func (r *messageReconciler) reconciliationDirPath() (string, error) {
	root, err := r.host.reconciliationRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, messageReconciliationDir), nil
}

func (r *messageReconciler) applyMessageChange(event messageReconciliation) {
	if len(event.MessageIDs) == 0 {
		return
	}
	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}
	if event.Operation == "edit" {
		r.editOrderMu.Lock()
		defer r.editOrderMu.Unlock()
		if r.pending.Load() {
			if err := r.ensureReconciliationState(); err != nil || r.deletes.Load() ||
				r.legacy.Load() || r.blocked.Load() > 0 {
				if persistErr := r.queueMessageReconciliation(event); persistErr == nil {
					return
				} else {
					r.log().Error("discord: blocked edit could not be saved; attempting application", "err", persistErr)
				}
			}
		}
	}
	if err := r.retryMessageChange(event); err != nil {
		if persistErr := r.queueMessageReconciliation(event); persistErr != nil {
			r.log().Error("discord: message change failed and reconciliation could not be saved", "operation", event.Operation,
				"messages", len(event.MessageIDs), "err", errors.Join(err, persistErr))
			return
		}
		r.log().Warn("discord: message change deferred for reconciliation", "operation", event.Operation,
			"messages", len(event.MessageIDs), "err", err)
		return
	}
	switch event.Operation {
	case "delete":
		// Edits still waiting for these messages would only be applied to nothing; drop them now.
		r.supersedeQueuedEdits(event)
	case "edit":
		// A waiting edit older than this one is stale: applied later it would bring back what this edit
		// changed (words a person took out of their message).
		r.supersedeOlderEdit(event)
	}
}

// supersedeOlderEdit removes the queued edit of event's message when it is not newer than event.
func (r *messageReconciler) supersedeOlderEdit(event messageReconciliation) {
	if !r.pending.Load() || len(event.MessageIDs) != 1 {
		return
	}
	dir, err := r.reconciliationDirPath()
	if err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	path := filepath.Join(dir, editFileName(event.ChannelID, event.MessageIDs[0]))
	if old, err := readReconciliationItem(path); err == nil && !old.At.After(event.At) {
		if err := r.removeReconciliation(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			r.log().Warn("discord: superseded edit could not be removed", "err", err)
		}
	}
}

func (r *messageReconciler) retryMessageChange(event messageReconciliation) error {
	ctx := r.host.runCtx()
	var lastErr error
	for attempt := 0; ; attempt++ {
		if err := r.host.applyMessageEvent(event); err == nil {
			return nil
		} else {
			lastErr = err
		}
		// the service is shutting down: a retry cannot succeed, so the caller queues the change for the next start
		if errors.Is(lastErr, application.ErrServiceClosing) || attempt == len(messageRetryBackoff) {
			return lastErr
		}
		if !r.waiter()(ctx, messageRetryBackoff[attempt]) {
			return errors.Join(lastErr, ctx.Err())
		}
	}
}

func (r *messageReconciler) waiter() func(context.Context, time.Duration) bool {
	if r.wait != nil {
		return r.wait
	}
	return waitContext
}

func (r *messageReconciler) removeReconciliation(path string) error {
	if r.remove != nil {
		return r.remove(path)
	}
	return fsutil.RemoveDurable(path)
}

// editFileName names the file of one message's pending edit. The hash keeps any ID text out of the
// path and makes the name safe on every filesystem.
func editFileName(channelID, messageID string) string {
	sum := sha256.Sum256([]byte(channelID + "\x00" + messageID))
	return "e-" + hex.EncodeToString(sum[:16]) + ".json"
}

func (r *messageReconciler) nextDeleteFileName() string {
	return fmt.Sprintf("d-%020d-%06d.json", time.Now().UnixNano(), r.seq.Add(1)%1_000_000)
}

// queueMessageReconciliation makes a failed change durable before the handler returns. A delete is
// written first and only then are the edits it supersedes removed, so a crash between the two leaves
// both and never neither.
func (r *messageReconciler) queueMessageReconciliation(event messageReconciliation) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.queueLocked(event)
}

func (r *messageReconciler) queueLocked(event messageReconciliation) error {
	err := r.queueNamedLocked(event, "")
	if err == nil && (event.Operation == "delete" || !r.backingOff.Load()) {
		// Deletes can make progress despite a privacy blocker. More edits cannot repair a failed
		// store, but the first deferral must still wake a drain sleeping on an empty queue.
		r.wakeReconciliation()
	}
	return err
}

func (r *messageReconciler) queueNamedLocked(event messageReconciliation, deleteName string) error {
	dir, err := r.reconciliationDirPath()
	if err != nil {
		return err
	}
	switch event.Operation {
	case "edit":
		if len(event.MessageIDs) != 1 {
			return errors.New("discord: edit reconciliation must name one message")
		}
		path := filepath.Join(dir, editFileName(event.ChannelID, event.MessageIDs[0]))
		if old, err := readReconciliationItem(path); err == nil && old.At.After(event.At) {
			return nil // a newer edit is already waiting
		}
		if err := r.writeReconciliation(path, event); err != nil {
			return err
		}
	case "delete":
		if deleteName == "" {
			deleteName = r.nextDeleteFileName()
		}
		if err := r.writeReconciliation(filepath.Join(dir, deleteName), event); err != nil {
			return err
		}
		r.deletes.Store(true)
		r.removeEditFilesLocked(dir, event)
	default:
		return errors.New("discord: unknown message reconciliation operation")
	}
	r.pending.Store(true)
	return nil
}

func (r *messageReconciler) removeEditFilesLocked(dir string, event messageReconciliation) {
	for _, id := range event.MessageIDs {
		if err := os.Remove(filepath.Join(dir, editFileName(event.ChannelID, id))); err != nil && !errors.Is(err, os.ErrNotExist) {
			r.log().Warn("discord: superseded edit could not be removed", "err", err)
		}
	}
}

// supersedeQueuedEdits removes the waiting edits of messages that were just deleted. It costs nothing
// while no change has ever been deferred.
func (r *messageReconciler) supersedeQueuedEdits(event messageReconciliation) {
	if !r.pending.Load() {
		return
	}
	dir, err := r.reconciliationDirPath()
	if err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removeEditFilesLocked(dir, event)
}

func (r *messageReconciler) wakeReconciliation() {
	if r.wake == nil {
		return
	}
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func readReconciliationItem(path string) (messageReconciliation, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return messageReconciliation{}, err
	}
	var item messageReconciliation
	if err := json.Unmarshal(b, &item); err != nil {
		return messageReconciliation{}, err
	}
	return item, nil
}

func writeReconciliationItem(path string, event messageReconciliation) error {
	b, err := json.Marshal(event)
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(path, b, 0o600)
}

func (r *messageReconciler) writeReconciliation(path string, event messageReconciliation) error {
	if r.write != nil {
		return r.write(path, event)
	}
	return writeReconciliationItem(path, event)
}

// listReconciliation returns up to limit queued file names that start with prefix and are not in skip.
func listReconciliation(dir, prefix string, limit int, skip map[string]bool) ([]string, error) {
	f, err := os.Open(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var names []string
	for len(names) < limit {
		entries, err := f.ReadDir(reconciliationBatch)
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".json") || skip[name] {
				continue
			}
			names = append(names, name)
			if len(names) == limit {
				break
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return names, err
		}
	}
	return names, nil
}

// drainMessageReconciliation makes one pass over the queue: deletes first, so no edit is ever applied
// before a delete that was waiting, then edits. It stops on the first sign that the store is still
// down; whatever is left stays on disk for the next pass.
func (r *messageReconciler) drainMessageReconciliation() error {
	root, err := r.host.reconciliationRoot()
	if err != nil {
		return err
	}
	dir := filepath.Join(root, messageReconciliationDir)
	migrationErr := r.migrateLegacyReconciliation(root)
	// Already durable deletes do not depend on migrating or removing the legacy source. An
	// unfinished migration can hide deletes, so only the edit phase waits for it to succeed.
	if err := r.drainPrefix(dir, "d-"); err != nil {
		return errors.Join(migrationErr, err) // deletes that did not apply hold the edits back
	}
	if err := r.refreshReconciliationBlocked(root, dir); err != nil {
		return errors.Join(migrationErr, err)
	}
	if migrationErr != nil {
		return migrationErr
	}
	if r.blocked.Load() > 0 {
		return errReconciliationBlocked
	}
	if err := r.drainPrefix(dir, "e-"); err != nil {
		return err
	}
	r.settleReconciliation(dir)
	return nil
}

func (r *messageReconciler) drainPrefix(dir, prefix string) error {
	failed := map[string]bool{}
	var firstErr error
	consecutive := 0
	for {
		names, err := listReconciliation(dir, prefix, reconciliationBatch, failed)
		if err != nil {
			return errors.Join(firstErr, err)
		}
		if len(names) == 0 {
			return firstErr
		}
		for _, name := range names {
			if err := r.reconciliationContext().Err(); err != nil {
				return errors.Join(firstErr, err)
			}
			if err := r.drainOne(dir, name); err != nil {
				failed[name] = true
				if errors.Is(err, errReconciliationBlocked) {
					// Quarantine made progress. Its privacy blocker holds edits, but does not mean
					// the store is down and must not delay the healthy deletes behind it.
					consecutive = 0
					continue
				}
				firstErr = errors.Join(firstErr, err)
				if consecutive++; consecutive >= reconciliationPassFailures {
					return firstErr
				}
				continue
			}
			consecutive = 0
		}
	}
}

// drainOne applies one queued change and removes its file. An edit is removed only if the file still
// holds what was applied: a newer edit that replaced it meanwhile stays for the next pass.
func (r *messageReconciler) drainOne(dir, name string) error {
	path := filepath.Join(dir, name)
	if strings.HasPrefix(name, "e-") {
		// read and apply under the edit order lock, so a newer live edit either lands first and has
		// removed this file, or waits until this older one is applied
		r.editOrderMu.Lock()
		defer r.editOrderMu.Unlock()
	}
	b, err := func() ([]byte, error) {
		// Windows readers can prevent rename-over while the handle is open. Share the queue
		// mutex with atomic writers, releasing it before application can queue another event.
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.read != nil {
			return r.read(path)
		}
		return os.ReadFile(path)
	}()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var item messageReconciliation
	if err := json.Unmarshal(b, &item); err != nil || !validReconciliationItem(item) {
		return r.quarantineReconciliation(path)
	}
	err = r.host.applyQueuedMessageEvent(item)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if strings.HasPrefix(name, "e-") {
		if now, err := os.ReadFile(path); err == nil && !bytes.Equal(now, b) {
			return nil
		}
	}
	return r.removeReconciliation(path)
}

// settleReconciliation clears the pending flag once the queue is really empty, so deletes stop looking
// for edit files to remove.
func (r *messageReconciler) settleReconciliation(dir string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.blocked.Load() > 0 {
		return
	}
	for _, prefix := range []string{"d-", "e-"} {
		if names, err := listReconciliation(dir, prefix, 1, nil); err != nil || len(names) > 0 {
			return
		}
	}
	r.pending.Store(false)
}

func validReconciliationItem(item messageReconciliation) bool {
	if len(item.MessageIDs) == 0 || (item.Operation != "delete" && (item.Operation != "edit" || len(item.MessageIDs) != 1)) {
		return false
	}
	for _, id := range item.MessageIDs {
		if id == "" {
			return false
		}
	}
	return true
}

// A damaged edit is isolated to that intent. Unreadable deletes and newly damaged legacy arrays
// can hide an unknown set of deletions, so they hold edits until an operator resolves them.
func (r *messageReconciler) quarantineReconciliation(path string) error {
	if err := r.renameReconciliationQuarantine(path); err != nil {
		return err
	}
	root, err := r.host.reconciliationRoot()
	if err != nil {
		return err
	}
	if err := r.refreshReconciliationBlocked(root, filepath.Join(root, messageReconciliationDir)); err != nil {
		return err
	}
	if r.blocked.Load() > 0 {
		return errReconciliationBlocked
	}
	return nil
}

func (r *messageReconciler) renameReconciliationQuarantine(path string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	base := path
	if filepath.Base(path) == messageReconciliationFile {
		base += ".unresolved"
	}
	target := base + ".corrupt"
	for suffix := 1; ; suffix++ {
		if _, err := os.Stat(target); errors.Is(err, os.ErrNotExist) {
			break
		} else if err != nil {
			return err
		}
		target = fmt.Sprintf("%s.%d.corrupt", base, suffix)
	}
	return os.Rename(path, target)
}

// The live edit path initializes the snapshot once, then uses only atomics. Queue writes update
// delete presence under the same mutex; drain passes refresh it, including operator recovery.
func (r *messageReconciler) ensureReconciliationState() error {
	r.mu.Lock()
	ready := r.stateReady
	r.mu.Unlock()
	if ready {
		return nil
	}
	root, err := r.host.reconciliationRoot()
	if err != nil {
		return err
	}
	return r.refreshReconciliationBlocked(root, filepath.Join(root, messageReconciliationDir))
}

func (r *messageReconciler) refreshReconciliationBlocked(root, dir string) error {
	// A legacy quarantine may be hundreds of MB. Classify outside the queue lock, then recount
	// while excluding queue writers so this snapshot cannot erase a concurrently queued delete.
	r.refreshMu.Lock()
	defer r.refreshMu.Unlock()
	if err := r.cacheLegacyQuarantines(root); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.refreshReconciliationBlockedLocked(root, dir)
}

type legacyQuarantineClassification struct {
	size      int64
	mtime     time.Time
	hasDelete bool
}

func (c legacyQuarantineClassification) matches(info os.FileInfo) bool {
	return c.size == info.Size() && c.mtime.Equal(info.ModTime())
}

func oldLegacyQuarantine(name string) bool {
	return strings.HasPrefix(name, messageReconciliationFile) && strings.HasSuffix(name, ".corrupt") &&
		!strings.HasPrefix(name, messageReconciliationFile+".unresolved")
}

func (r *messageReconciler) cacheLegacyQuarantines(root string) error {
	f, err := os.Open(root)
	if errors.Is(err, os.ErrNotExist) {
		r.legacyQuarantineCache = nil
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	current := make(map[string]legacyQuarantineClassification)
	for {
		entries, readErr := f.ReadDir(reconciliationBatch)
		for _, entry := range entries {
			if entry.IsDir() || !oldLegacyQuarantine(entry.Name()) {
				continue
			}
			path := filepath.Join(root, entry.Name())
			info, err := entry.Info()
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			cached, ok := r.legacyQuarantineCache[path]
			if !ok || !cached.matches(info) {
				hasDelete, err := r.legacyQuarantineHasDelete(path, r.reconciliationContext())
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				if err != nil {
					return err
				}
				cached = legacyQuarantineClassification{size: info.Size(), mtime: info.ModTime(), hasDelete: hasDelete}
			}
			current[path] = cached
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	// Removing resolved entries keeps memory proportional to the remaining old quarantines.
	r.legacyQuarantineCache = current
	return nil
}

// Old legacy quarantines predate unresolved-intent tracking. Only an identifiable delete in such
// a file is a blocker. Token scanning also recognizes a delete before a truncated object ends.
func (r *messageReconciler) legacyQuarantineHasDelete(path string, ctx context.Context) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	var source io.Reader = f
	if r.legacyReader != nil {
		source = r.legacyReader(source)
	}
	reader := &reconciliationReader{ctx: ctx, reader: source}
	dec := json.NewDecoder(reader)
	depth := 0
	var record, key, operation bool
	for {
		tok, err := dec.Token()
		if err != nil {
			if reader.err != nil {
				return false, reader.err
			}
			return false, nil
		}
		if delim, ok := tok.(json.Delim); ok {
			switch delim {
			case '[', '{':
				if depth == 2 && record {
					key, operation = true, false // a nested value is not an operation string
				}
				if depth == 1 && delim == '{' {
					record, key = true, true
				}
				depth++
			case ']', '}':
				depth--
				if depth < 2 {
					record = false
				}
			}
			continue
		}
		if depth == 2 && record {
			if key {
				operation = tok == "operation"
			} else if operation && tok == "delete" {
				return true, nil
			}
			key = !key
		}
	}
}

func (r *messageReconciler) refreshReconciliationBlockedLocked(root, dir string) error {
	var count int64
	var first string
	var deletes, legacy bool
	for _, path := range []string{root, dir} {
		if err := func() error {
			f, err := os.Open(path)
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			defer f.Close()
			for {
				entries, readErr := f.ReadDir(reconciliationBatch)
				for _, entry := range entries {
					if entry.IsDir() {
						continue
					}
					name := entry.Name()
					if path == dir && strings.HasPrefix(name, "d-") && strings.HasSuffix(name, ".json") {
						deletes = true
					}
					if path == root && name == messageReconciliationFile {
						legacy = true
					}
					if !strings.HasSuffix(name, ".corrupt") {
						continue
					}
					blocked := path == dir && strings.HasPrefix(name, "d-")
					if path == root && strings.HasPrefix(name, messageReconciliationFile) {
						blocked = strings.HasPrefix(name, messageReconciliationFile+".unresolved")
						if !blocked {
							info, err := entry.Info()
							if err != nil {
								return err
							}
							cached, ok := r.legacyQuarantineCache[filepath.Join(path, name)]
							// An operator may change the file between classification and recounting.
							// Keep edits blocked until the next pass classifies those new bytes.
							blocked = !ok || !cached.matches(info) || cached.hasDelete
						}
					}
					if blocked {
						count++
						if first == "" {
							first = filepath.Join(path, name)
						}
					}
				}
				if errors.Is(readErr, io.EOF) {
					return nil
				}
				if readErr != nil {
					return readErr
				}
			}
		}(); err != nil {
			return err
		}
	}
	old := r.blocked.Swap(count)
	r.deletes.Store(deletes)
	r.legacy.Store(legacy)
	r.stateReady = true
	if count > 0 {
		r.pending.Store(true)
		if old != count || time.Since(r.blockedLog) >= 6*time.Hour {
			r.log().Error("discord: message reconciliation recovery state changed; inspect file, handle privacy intent, then move it to resolved/; see docs/memory-operations.md", "blocked", count, "file", first)
			r.blockedLog = time.Now()
		}
	} else if old != count {
		r.log().Info("discord: message reconciliation recovery state changed", "blocked", count)
	}
	return nil
}

// migrateLegacyReconciliation moves the events of the first file format into the directory, reading the
// array element by element so a large file is never held whole. It runs in the background drain.
func (r *messageReconciler) migrateLegacyReconciliation(root string) error {
	ctx := r.reconciliationContext()
	if err := ctx.Err(); err != nil {
		return err
	}
	path := filepath.Join(root, messageReconciliationFile)
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	if info.Size() == 0 && !info.IsDir() {
		_ = f.Close()
		return r.removeReconciliation(path) // no intent can be hidden in an empty source
	}
	sum := sha256.New()
	if _, err := io.Copy(sum, &reconciliationReader{ctx: ctx, reader: f}); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		return err
	}
	source := hex.EncodeToString(sum.Sum(nil)[:16])
	moved := 0
	var readErr error
	var reader io.Reader = f
	if r.legacyReader != nil {
		reader = r.legacyReader(reader)
	}
	tracked := &reconciliationReader{ctx: ctx, reader: reader}
	dec := json.NewDecoder(tracked)
	if tok, err := dec.Token(); err != nil && !errors.Is(err, io.EOF) {
		readErr = err
	} else if err == nil && tok != json.Delim('[') {
		readErr = errReconciliationFormat
	} else if err != nil { // an empty file
		readErr = io.EOF
	}
	for readErr == nil && dec.More() {
		if err := ctx.Err(); err != nil {
			return err
		}
		var item messageReconciliation
		if err := dec.Decode(&item); err != nil {
			readErr = err
			break
		}
		if !validReconciliationItem(item) {
			readErr = errReconciliationFormat
			break
		}
		if item.At.IsZero() {
			item.At = info.ModTime().UTC() // retrying the same source must not manufacture a newer edit
		}
		if r.legacyStep != nil {
			r.legacyStep(moved)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		// The same source record always has the same name, including after a partial migration or
		// restart. Delete files must not multiply every time a later write fails.
		err := func() error {
			r.mu.Lock()
			defer r.mu.Unlock()
			return r.queueNamedLocked(item, fmt.Sprintf("d-legacy-%s-%020d.json", source, moved))
		}()
		if err != nil {
			_ = f.Close()
			return err // the file stays; the next pass moves it again (an edit is replaced by name)
		}
		moved++
	}
	if readErr == nil {
		if tok, err := dec.Token(); err != nil {
			readErr = err
		} else if tok != json.Delim(']') {
			readErr = errReconciliationFormat
		} else if _, err := dec.Token(); !errors.Is(err, io.EOF) {
			if err != nil {
				readErr = err
			} else {
				readErr = errReconciliationFormat
			}
		}
	}
	_ = f.Close()
	if err := ctx.Err(); err != nil {
		return err // cancellation is not evidence of a corrupt source
	}
	if readErr != nil {
		if tracked.err != nil {
			return tracked.err // only the reader can establish a transient read failure
		}
		return r.quarantineReconciliation(path)
	}
	r.log().Info("discord: old message reconciliation file moved", "events", moved)
	return r.removeReconciliation(path)
}

type reconciliationReader struct {
	ctx    context.Context
	reader io.Reader
	err    error // the underlying reader's failure, separate from invalid decoded field values
}

func (r *reconciliationReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		r.err = err
		return 0, err
	}
	n, err := r.reader.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		r.err = err
	}
	return n, err
}

func (r *messageReconciler) reconciliationContext() context.Context {
	if r.ctx != nil {
		return r.ctx
	}
	return r.host.runCtx()
}

// runReconciliationDrain applies deferred message changes in the background, so the gateway and the
// console start without waiting on them. It sleeps while the queue is empty and is woken by a deferral.
func (r *messageReconciler) runReconciliationDrain(ctx context.Context) {
	if r.done != nil {
		defer close(r.done)
	}
	defer recoverReconciliationPanic(r.log(), "drain", nil)
	delay := reconciliationDrainInitial
	for ctx.Err() == nil {
		// Notifications already queued are covered by this pass. Legacy records migrated by the
		// pass itself do not notify, otherwise a failed migration would defeat its own backoff.
		select {
		case <-r.wake:
		default:
		}
		err := runReconciliationStep(r.log(), "drain pass", r.drainMessageReconciliation)
		if err == nil {
			delay = reconciliationDrainInitial
			if !r.waitReconciliationIdle(ctx) {
				return
			}
			continue
		}
		r.backingOff.Store(true)
		wait := jittered(delay)
		if r.jitter != nil {
			wait = r.jitter(delay)
		}
		if !errors.Is(err, errReconciliationBlocked) {
			r.log().Warn("discord: message reconciliation not finished; will retry", "retry_in", wait.Round(time.Second).String(), "error_kind", fmt.Sprintf("%T", err))
		}
		if !r.waitReconciliationRetry(ctx, wait) {
			return
		}
		if delay *= 2; delay > reconciliationDrainMax {
			delay = reconciliationDrainMax
		}
	}
}

func (r *messageReconciler) waitReconciliationIdle(ctx context.Context) bool {
	r.backingOff.Store(false)
	// Edits queued after settleReconciliation while the previous backoff was still active did
	// not notify. Check after enabling notifications so neither side of that transition is lost.
	if r.pending.Load() {
		return true
	}
	select {
	case <-ctx.Done():
		return false
	case <-r.wake:
		return true
	}
}

func (r *messageReconciler) waitReconciliationRetry(ctx context.Context, delay time.Duration) bool {
	if r.wait != nil {
		return r.wait(ctx, delay) // tests override the retry clock
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-r.wake:
		return true
	case <-timer.C:
		return true
	}
}
