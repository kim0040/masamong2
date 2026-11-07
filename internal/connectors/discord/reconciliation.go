package discord

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"masamong/internal/fsutil"
	"masamong/internal/memory/store"
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

// Keep panic values out of diagnostics: callbacks can carry private message data.
func (g *Gateway) recoverReconciliationPanic(stage string, err *error) {
	if recover() != nil {
		failure := errors.New("discord: reconciliation " + stage + " panicked")
		if err != nil {
			*err = failure
		}
		g.log.Error(failure.Error(), "stack", string(debug.Stack()))
	}
}

func (g *Gateway) runReconciliationStep(stage string, step func() error) (err error) {
	defer g.recoverReconciliationPanic(stage, &err)
	return step()
}

func (g *Gateway) reconciliationRoot() (string, error) {
	if g.svc == nil || g.svc.DataRoot == "" {
		return "", errors.New("discord: message reconciliation has no data root")
	}
	return g.svc.DataRoot, nil
}

func (g *Gateway) reconciliationDirPath() (string, error) {
	root, err := g.reconciliationRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, messageReconciliationDir), nil
}

func (g *Gateway) applyMessageChange(event messageReconciliation) {
	if len(event.MessageIDs) == 0 {
		return
	}
	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}
	if event.Operation == "edit" {
		g.editOrderMu.Lock()
		defer g.editOrderMu.Unlock()
		if g.reconciliationPending.Load() {
			if err := g.ensureReconciliationState(); err != nil || g.reconciliationDeletes.Load() ||
				g.reconciliationLegacy.Load() || g.reconciliationBlocked.Load() > 0 {
				if persistErr := g.queueMessageReconciliation(event); persistErr == nil {
					return
				} else {
					g.log.Error("discord: blocked edit could not be saved; attempting application", "err", persistErr)
				}
			}
		}
	}
	if err := g.retryMessageChange(event); err != nil {
		if persistErr := g.queueMessageReconciliation(event); persistErr != nil {
			g.log.Error("discord: message change failed and reconciliation could not be saved", "operation", event.Operation,
				"messages", len(event.MessageIDs), "err", errors.Join(err, persistErr))
			return
		}
		g.log.Warn("discord: message change deferred for reconciliation", "operation", event.Operation,
			"messages", len(event.MessageIDs), "err", err)
		return
	}
	switch event.Operation {
	case "delete":
		// Edits still waiting for these messages would only be applied to nothing; drop them now.
		g.supersedeQueuedEdits(event)
	case "edit":
		// A waiting edit older than this one is stale: applied later it would bring back what this edit
		// changed (words a person took out of their message).
		g.supersedeOlderEdit(event)
	}
}

// supersedeOlderEdit removes the queued edit of event's message when it is not newer than event.
func (g *Gateway) supersedeOlderEdit(event messageReconciliation) {
	if !g.reconciliationPending.Load() || len(event.MessageIDs) != 1 {
		return
	}
	dir, err := g.reconciliationDirPath()
	if err != nil {
		return
	}
	g.reconciliationMu.Lock()
	defer g.reconciliationMu.Unlock()
	path := filepath.Join(dir, editFileName(event.ChannelID, event.MessageIDs[0]))
	if old, err := readReconciliationItem(path); err == nil && !old.At.After(event.At) {
		if err := g.removeReconciliation(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			g.log.Warn("discord: superseded edit could not be removed", "err", err)
		}
	}
}

func (g *Gateway) retryMessageChange(event messageReconciliation) error {
	ctx := g.runCtx()
	var lastErr error
	for attempt := 0; ; attempt++ {
		if err := g.applyMessageEvent(event); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if attempt == len(messageRetryBackoff) {
			return lastErr
		}
		if !g.reconciliationWaiter()(ctx, messageRetryBackoff[attempt]) {
			return errors.Join(lastErr, ctx.Err())
		}
	}
}

func (g *Gateway) reconciliationWaiter() func(context.Context, time.Duration) bool {
	if g.reconciliationWait != nil {
		return g.reconciliationWait
	}
	return waitContext
}

func (g *Gateway) applyMessageEvent(event messageReconciliation) (err error) {
	defer func() {
		if recover() != nil {
			// Panic values may contain private text. Keep only the category and call stack, and
			// return through the normal retry/queue path for both live and deferred changes.
			err = errors.New("discord: reconciliation apply panicked")
			g.log.Error(err.Error(), "stack", string(debug.Stack()))
		}
	}()
	if event.Operation == "edit" && len(event.MessageIDs) == 1 {
		current, err := g.reconciliationEditCurrent(event)
		if err != nil || !current {
			return err
		}
	}
	if g.messageEventApply != nil {
		return g.messageEventApply(event)
	}
	if g.svc == nil {
		return errors.New("discord: message change has no application service")
	}
	switch event.Operation {
	case "edit":
		if len(event.MessageIDs) != 1 {
			return errors.New("discord: edit reconciliation must name one message")
		}
		if event.SameFiles {
			kept, err := g.svc.MessageKeptItsWords(event.GuildID, event.ChannelID, event.MessageIDs[0], event.Content, event.At)
			if err != nil || kept {
				return err
			}
		}
		return g.svc.MessageEdited(event.GuildID, event.ChannelID, event.MessageIDs[0], event.Content, event.At)
	case "delete":
		return g.svc.MessagesDeleted(event.GuildID, event.ChannelID, event.MessageIDs)
	default:
		return errors.New("discord: unknown message reconciliation operation")
	}
}

// The committed revision, not removal of a queue file, decides whether an edit may reach the live
// window. This also covers an old legacy record migrated after a newer live edit succeeded.
func (g *Gateway) reconciliationEditCurrent(event messageReconciliation) (bool, error) {
	if g.svc == nil || g.svc.Store == nil {
		return true, nil
	}
	reader := g.svc.Store.Reader()
	ctx, cancel := context.WithTimeout(g.reconciliationContext(), 5*time.Second)
	defer cancel()
	// an edit that kept the words leaves no tombstone, and is a revision all the same
	if newer, err := store.KeptRevisionNewer(ctx, reader, event.MessageIDs[0], event.At); err != nil || newer {
		return false, err
	}
	var raw string
	err := reader.QueryRowContext(ctx, `SELECT at FROM privacy_tombstones WHERE kind='message' AND source_id=?`, event.MessageIDs[0]).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	latest, err := time.Parse(time.RFC3339Nano, raw)
	if err == nil && !event.At.After(latest) {
		// An equal intent already has durable application replay. Replaying it here must not
		// overwrite live text with a different payload that happens to have the same timestamp.
		return false, nil
	}
	// Match the store's revision policy: an unparseable timestamp is not a permanent retry
	// error. Still check deletion and the observation revision before touching live effects.
	var edited, deleted string
	err = reader.QueryRowContext(ctx, `SELECT edited_at,deleted_at FROM memory_observations WHERE platform='discord' AND native_message_id=?`, event.MessageIDs[0]).Scan(&edited, &deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil || deleted != "" {
		return false, err
	}
	if edited != "" {
		applied, err := time.Parse(time.RFC3339Nano, edited)
		if err == nil && !event.At.After(applied) {
			return false, nil
		}
	}
	return true, nil
}

func (g *Gateway) removeReconciliation(path string) error {
	if g.reconciliationRemove != nil {
		return g.reconciliationRemove(path)
	}
	return fsutil.RemoveDurable(path)
}

// editFileName names the file of one message's pending edit. The hash keeps any ID text out of the
// path and makes the name safe on every filesystem.
func editFileName(channelID, messageID string) string {
	sum := sha256.Sum256([]byte(channelID + "\x00" + messageID))
	return "e-" + hex.EncodeToString(sum[:16]) + ".json"
}

func (g *Gateway) nextDeleteFileName() string {
	return fmt.Sprintf("d-%020d-%06d.json", time.Now().UnixNano(), g.reconciliationSeq.Add(1)%1_000_000)
}

// queueMessageReconciliation makes a failed change durable before the handler returns. A delete is
// written first and only then are the edits it supersedes removed, so a crash between the two leaves
// both and never neither.
func (g *Gateway) queueMessageReconciliation(event messageReconciliation) error {
	g.reconciliationMu.Lock()
	defer g.reconciliationMu.Unlock()
	return g.queueLocked(event)
}

func (g *Gateway) queueLocked(event messageReconciliation) error {
	err := g.queueNamedLocked(event, "")
	if err == nil && (event.Operation == "delete" || !g.reconciliationBackingOff.Load()) {
		// Deletes can make progress despite a privacy blocker. More edits cannot repair a failed
		// store, but the first deferral must still wake a drain sleeping on an empty queue.
		g.wakeReconciliation()
	}
	return err
}

func (g *Gateway) queueNamedLocked(event messageReconciliation, deleteName string) error {
	dir, err := g.reconciliationDirPath()
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
		if err := g.writeReconciliation(path, event); err != nil {
			return err
		}
	case "delete":
		if deleteName == "" {
			deleteName = g.nextDeleteFileName()
		}
		if err := g.writeReconciliation(filepath.Join(dir, deleteName), event); err != nil {
			return err
		}
		g.reconciliationDeletes.Store(true)
		g.removeEditFilesLocked(dir, event)
	default:
		return errors.New("discord: unknown message reconciliation operation")
	}
	g.reconciliationPending.Store(true)
	return nil
}

func (g *Gateway) removeEditFilesLocked(dir string, event messageReconciliation) {
	for _, id := range event.MessageIDs {
		if err := os.Remove(filepath.Join(dir, editFileName(event.ChannelID, id))); err != nil && !errors.Is(err, os.ErrNotExist) {
			g.log.Warn("discord: superseded edit could not be removed", "err", err)
		}
	}
}

// supersedeQueuedEdits removes the waiting edits of messages that were just deleted. It costs nothing
// while no change has ever been deferred.
func (g *Gateway) supersedeQueuedEdits(event messageReconciliation) {
	if !g.reconciliationPending.Load() {
		return
	}
	dir, err := g.reconciliationDirPath()
	if err != nil {
		return
	}
	g.reconciliationMu.Lock()
	defer g.reconciliationMu.Unlock()
	g.removeEditFilesLocked(dir, event)
}

func (g *Gateway) wakeReconciliation() {
	if g.reconciliationWake == nil {
		return
	}
	select {
	case g.reconciliationWake <- struct{}{}:
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

func (g *Gateway) writeReconciliation(path string, event messageReconciliation) error {
	if g.reconciliationWrite != nil {
		return g.reconciliationWrite(path, event)
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
func (g *Gateway) drainMessageReconciliation() error {
	root, err := g.reconciliationRoot()
	if err != nil {
		return err
	}
	dir := filepath.Join(root, messageReconciliationDir)
	migrationErr := g.migrateLegacyReconciliation(root)
	// Already durable deletes do not depend on migrating or removing the legacy source. An
	// unfinished migration can hide deletes, so only the edit phase waits for it to succeed.
	if err := g.drainPrefix(dir, "d-"); err != nil {
		return errors.Join(migrationErr, err) // deletes that did not apply hold the edits back
	}
	if err := g.refreshReconciliationBlocked(root, dir); err != nil {
		return errors.Join(migrationErr, err)
	}
	if migrationErr != nil {
		return migrationErr
	}
	if g.reconciliationBlocked.Load() > 0 {
		return errReconciliationBlocked
	}
	if err := g.drainPrefix(dir, "e-"); err != nil {
		return err
	}
	g.settleReconciliation(dir)
	return nil
}

func (g *Gateway) drainPrefix(dir, prefix string) error {
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
			if err := g.reconciliationContext().Err(); err != nil {
				return errors.Join(firstErr, err)
			}
			if err := g.drainOne(dir, name); err != nil {
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
func (g *Gateway) drainOne(dir, name string) error {
	path := filepath.Join(dir, name)
	if strings.HasPrefix(name, "e-") {
		// read and apply under the edit order lock, so a newer live edit either lands first and has
		// removed this file, or waits until this older one is applied
		g.editOrderMu.Lock()
		defer g.editOrderMu.Unlock()
	}
	b, err := func() ([]byte, error) {
		// Windows readers can prevent rename-over while the handle is open. Share the queue
		// mutex with atomic writers, releasing it before application can queue another event.
		g.reconciliationMu.Lock()
		defer g.reconciliationMu.Unlock()
		if g.reconciliationRead != nil {
			return g.reconciliationRead(path)
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
		return g.quarantineReconciliation(path)
	}
	err = g.applyQueuedMessageEvent(item)
	if err != nil {
		return err
	}
	g.reconciliationMu.Lock()
	defer g.reconciliationMu.Unlock()
	if strings.HasPrefix(name, "e-") {
		if now, err := os.ReadFile(path); err == nil && !bytes.Equal(now, b) {
			return nil
		}
	}
	return g.removeReconciliation(path)
}

func (g *Gateway) applyQueuedMessageEvent(item messageReconciliation) error {
	if _, ok := g.beginWork(); !ok {
		return context.Canceled
	}
	defer g.inflight.Done()
	return g.applyMessageEvent(item)
}

// settleReconciliation clears the pending flag once the queue is really empty, so deletes stop looking
// for edit files to remove.
func (g *Gateway) settleReconciliation(dir string) {
	g.reconciliationMu.Lock()
	defer g.reconciliationMu.Unlock()
	if g.reconciliationBlocked.Load() > 0 {
		return
	}
	for _, prefix := range []string{"d-", "e-"} {
		if names, err := listReconciliation(dir, prefix, 1, nil); err != nil || len(names) > 0 {
			return
		}
	}
	g.reconciliationPending.Store(false)
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
func (g *Gateway) quarantineReconciliation(path string) error {
	if err := g.renameReconciliationQuarantine(path); err != nil {
		return err
	}
	dir, err := g.reconciliationDirPath()
	if err != nil {
		return err
	}
	if err := g.refreshReconciliationBlocked(g.svc.DataRoot, dir); err != nil {
		return err
	}
	if g.reconciliationBlocked.Load() > 0 {
		return errReconciliationBlocked
	}
	return nil
}

func (g *Gateway) renameReconciliationQuarantine(path string) error {
	g.reconciliationMu.Lock()
	defer g.reconciliationMu.Unlock()
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
func (g *Gateway) ensureReconciliationState() error {
	g.reconciliationMu.Lock()
	ready := g.reconciliationStateReady
	g.reconciliationMu.Unlock()
	if ready {
		return nil
	}
	root, err := g.reconciliationRoot()
	if err != nil {
		return err
	}
	return g.refreshReconciliationBlocked(root, filepath.Join(root, messageReconciliationDir))
}

func (g *Gateway) refreshReconciliationBlocked(root, dir string) error {
	// A legacy quarantine may be hundreds of MB. Classify outside the queue lock, then recount
	// while excluding queue writers so this snapshot cannot erase a concurrently queued delete.
	g.reconciliationRefreshMu.Lock()
	defer g.reconciliationRefreshMu.Unlock()
	if err := g.cacheLegacyQuarantines(root); err != nil {
		return err
	}
	g.reconciliationMu.Lock()
	defer g.reconciliationMu.Unlock()
	return g.refreshReconciliationBlockedLocked(root, dir)
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

func (g *Gateway) cacheLegacyQuarantines(root string) error {
	f, err := os.Open(root)
	if errors.Is(err, os.ErrNotExist) {
		g.legacyQuarantineCache = nil
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
			cached, ok := g.legacyQuarantineCache[path]
			if !ok || !cached.matches(info) {
				hasDelete, err := g.legacyQuarantineHasDelete(path, g.reconciliationContext())
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
	g.legacyQuarantineCache = current
	return nil
}

// Old legacy quarantines predate unresolved-intent tracking. Only an identifiable delete in such
// a file is a blocker. Token scanning also recognizes a delete before a truncated object ends.
func (g *Gateway) legacyQuarantineHasDelete(path string, ctx context.Context) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	var source io.Reader = f
	if g.legacyReconciliationReader != nil {
		source = g.legacyReconciliationReader(source)
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

func (g *Gateway) refreshReconciliationBlockedLocked(root, dir string) error {
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
							cached, ok := g.legacyQuarantineCache[filepath.Join(path, name)]
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
	old := g.reconciliationBlocked.Swap(count)
	g.reconciliationDeletes.Store(deletes)
	g.reconciliationLegacy.Store(legacy)
	g.reconciliationStateReady = true
	if count > 0 {
		g.reconciliationPending.Store(true)
		if old != count || time.Since(g.reconciliationBlockedLog) >= 6*time.Hour {
			g.log.Error("discord: message reconciliation recovery state changed; inspect file, handle privacy intent, then move it to resolved/; see docs/memory-operations.md", "blocked", count, "file", first)
			g.reconciliationBlockedLog = time.Now()
		}
	} else if old != count {
		g.log.Info("discord: message reconciliation recovery state changed", "blocked", count)
	}
	return nil
}

// migrateLegacyReconciliation moves the events of the first file format into the directory, reading the
// array element by element so a large file is never held whole. It runs in the background drain.
func (g *Gateway) migrateLegacyReconciliation(root string) error {
	ctx := g.reconciliationContext()
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
		return g.removeReconciliation(path) // no intent can be hidden in an empty source
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
	if g.legacyReconciliationReader != nil {
		reader = g.legacyReconciliationReader(reader)
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
		if g.legacyReconciliationStep != nil {
			g.legacyReconciliationStep(moved)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		// The same source record always has the same name, including after a partial migration or
		// restart. Delete files must not multiply every time a later write fails.
		err := func() error {
			g.reconciliationMu.Lock()
			defer g.reconciliationMu.Unlock()
			return g.queueNamedLocked(item, fmt.Sprintf("d-legacy-%s-%020d.json", source, moved))
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
		return g.quarantineReconciliation(path)
	}
	g.log.Info("discord: old message reconciliation file moved", "events", moved)
	return g.removeReconciliation(path)
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

func (g *Gateway) reconciliationContext() context.Context {
	if g.reconciliationCtx != nil {
		return g.reconciliationCtx
	}
	return g.runCtx()
}

func (g *Gateway) startReconciliationDrain(ctx context.Context) {
	g.workMu.Lock()
	defer g.workMu.Unlock()
	if g.draining {
		return
	}
	g.reconciliationCtx, g.reconciliationCancel = context.WithCancel(ctx)
	g.reconciliationDone = make(chan struct{})
	go g.runReconciliationDrain(g.reconciliationCtx)
}

// runReconciliationDrain applies deferred message changes in the background, so the gateway and the
// console start without waiting on them. It sleeps while the queue is empty and is woken by a deferral.
func (g *Gateway) runReconciliationDrain(ctx context.Context) {
	if g.reconciliationDone != nil {
		defer close(g.reconciliationDone)
	}
	defer g.recoverReconciliationPanic("drain", nil)
	delay := reconciliationDrainInitial
	for ctx.Err() == nil {
		// Notifications already queued are covered by this pass. Legacy records migrated by the
		// pass itself do not notify, otherwise a failed migration would defeat its own backoff.
		select {
		case <-g.reconciliationWake:
		default:
		}
		err := g.runReconciliationStep("drain pass", g.drainMessageReconciliation)
		if err == nil {
			delay = reconciliationDrainInitial
			if !g.waitReconciliationIdle(ctx) {
				return
			}
			continue
		}
		g.reconciliationBackingOff.Store(true)
		wait := jittered(delay)
		if g.retryJitter != nil {
			wait = g.retryJitter(delay)
		}
		if !errors.Is(err, errReconciliationBlocked) {
			g.log.Warn("discord: message reconciliation not finished; will retry", "retry_in", wait.Round(time.Second).String(), "error_kind", fmt.Sprintf("%T", err))
		}
		if !g.waitReconciliationRetry(ctx, wait) {
			return
		}
		if delay *= 2; delay > reconciliationDrainMax {
			delay = reconciliationDrainMax
		}
	}
}

func (g *Gateway) waitReconciliationIdle(ctx context.Context) bool {
	g.reconciliationBackingOff.Store(false)
	// Edits queued after settleReconciliation while the previous backoff was still active did
	// not notify. Check after enabling notifications so neither side of that transition is lost.
	if g.reconciliationPending.Load() {
		return true
	}
	select {
	case <-ctx.Done():
		return false
	case <-g.reconciliationWake:
		return true
	}
}

func (g *Gateway) waitReconciliationRetry(ctx context.Context, delay time.Duration) bool {
	if g.reconciliationWait != nil {
		return g.reconciliationWait(ctx, delay) // tests override the retry clock
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-g.reconciliationWake:
		return true
	case <-timer.C:
		return true
	}
}

// ReconciliationBlocked is the number of quarantined recovery files that wait for an operator. It
// implements application.DiscordReconciliationReporter so the doctor and the console's notifications
// can show what used to be only an error log line. The count is kept by the recount that runs on every
// drain pass, so this is a cheap atomic read.
func (g *Gateway) ReconciliationBlocked() int {
	return int(g.reconciliationBlocked.Load())
}
