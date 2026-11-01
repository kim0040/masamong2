package discord

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/bwmarrin/discordgo"

	"masamong/internal/config"
	"masamong/internal/fsutil"
)

func TestReconciliationRecoveryScopesAndOperatorResolution(t *testing.T) {
	for _, tc := range []struct {
		name, body    string
		root, blocked bool
	}{
		{"e-old.json.corrupt", `{`, false, false},
		{"unrelated.corrupt", `{`, false, false},
		{messageReconciliationFile + ".corrupt", `[{"operation":"edit"`, true, false},
		{messageReconciliationFile + ".corrupt", `[{"content":"operation","other":"delete"}]`, true, false},
		{messageReconciliationFile + ".corrupt", `[{"extra":{"operation":"delete"},"operation":"edit"}]`, true, false},
		{messageReconciliationFile + ".corrupt", `[{"operation":"delete"`, true, true},
		{messageReconciliationFile + ".unresolved.corrupt", `{`, true, true},
		{"d-old.json.corrupt", `{`, false, true},
	} {
		t.Run(tc.name+fmt.Sprint(tc.blocked), func(t *testing.T) {
			g, dir := reconciliationTestGateway(t)
			base := dir
			if tc.root {
				base = g.svc.DataRoot
			}
			path := filepath.Join(base, tc.name)
			if err := fsutil.WriteAtomic(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			g.log = slog.New(slog.NewTextHandler(&logs, nil))
			calls := 0
			g.messageEventApply = func(messageReconciliation) error { calls++; return nil }
			event := messageReconciliation{Operation: "edit", ChannelID: "c", MessageIDs: []string{"other"}, At: time.Now()}
			if err := g.queueMessageReconciliation(event); err != nil {
				t.Fatal(err)
			}
			err := g.drainMessageReconciliation()
			if tc.blocked {
				if !errors.Is(err, errReconciliationBlocked) || calls != 0 || g.reconciliationBlocked.Load() != 1 {
					t.Fatal("privacy blocker bypassed")
				}
				if !bytes.Contains(logs.Bytes(), []byte(tc.name)) || !bytes.Contains(logs.Bytes(), []byte("resolved/")) {
					t.Fatal("missing actionable recovery log")
				}
				resolved := filepath.Join(base, "resolved")
				if err := os.MkdirAll(resolved, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(path, filepath.Join(resolved, tc.name)); err != nil {
					t.Fatal(err)
				}
				if err := g.drainMessageReconciliation(); err != nil {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if calls != 1 || g.reconciliationBlocked.Load() != 0 || g.reconciliationPending.Load() {
				t.Fatal("resolved or unrelated quarantine kept edits blocked")
			}
		})
	}
}

func TestReconciliationQueuedReadExcludesAtomicWriters(t *testing.T) {
	for _, operation := range []string{"edit", "delete"} {
		t.Run(operation, func(t *testing.T) {
			g, dir := reconciliationTestGateway(t)
			if err := g.queueMessageReconciliation(messageReconciliation{Operation: operation, MessageIDs: []string{"intent"}}); err != nil {
				t.Fatal(err)
			}
			names := reconciliationNames(t, dir)
			reads := 0
			g.reconciliationRead = func(path string) ([]byte, error) {
				reads++
				if g.reconciliationMu.TryLock() {
					g.reconciliationMu.Unlock()
					return nil, errors.New("atomic writer could overlap the open read handle")
				}
				return os.ReadFile(path)
			}
			g.messageEventApply = func(messageReconciliation) error { return nil }
			if err := g.drainOne(dir, names[0]); err != nil || reads != 1 || len(reconciliationNames(t, dir)) != 0 {
				t.Fatal("queued read did not exclude writers or complete application")
			}
		})
	}
}

func TestReconciliationMigrationPanicReleasesQueueLock(t *testing.T) {
	g, _ := reconciliationTestGateway(t)
	path := filepath.Join(g.svc.DataRoot, messageReconciliationFile)
	if err := fsutil.WriteAtomic(path, []byte(`[{"operation":"delete","message_ids":["legacy"]}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	g.reconciliationWrite = func(string, messageReconciliation) error { panic("injected write panic") }
	panicked := false
	func() {
		defer func() { panicked = recover() != nil }()
		_ = g.migrateLegacyReconciliation(g.svc.DataRoot)
	}()
	if !panicked {
		t.Fatal("write panic was not injected")
	}
	g.reconciliationWrite = nil
	queued := make(chan error, 1)
	go func() {
		queued <- g.queueMessageReconciliation(messageReconciliation{Operation: "delete", MessageIDs: []string{"new"}})
	}()
	select {
	case err := <-queued:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("migration panic left the queue mutex locked")
	}
	g.messageEventApply = func(messageReconciliation) error { return nil }
	if err := g.drainMessageReconciliation(); err != nil {
		t.Fatal("preserved migration source did not recover")
	}
}

func TestReconciliationLegacyDecoderIOFailuresPreserveSource(t *testing.T) {
	for _, prefix := range []string{"", "[", `[{"operation":`, `[{"operation":"delete","message_ids":["intent"]}`, `[{"operation":"delete","message_ids":["intent"]}]`} {
		t.Run(fmt.Sprint(len(prefix)), func(t *testing.T) {
			g, _ := reconciliationTestGateway(t)
			path := filepath.Join(g.svc.DataRoot, messageReconciliationFile)
			original := []byte(`[{"operation":"delete","message_ids":["intent"]}]`)
			if err := fsutil.WriteAtomic(path, original, 0o600); err != nil {
				t.Fatal(err)
			}
			injected := errors.New("temporary decoder read failure")
			g.legacyReconciliationReader = func(io.Reader) io.Reader {
				return io.MultiReader(strings.NewReader(prefix), iotest.ErrReader(injected))
			}
			if err := g.migrateLegacyReconciliation(g.svc.DataRoot); !errors.Is(err, injected) {
				t.Fatal("decoder read failure was quarantined instead of retried")
			}
			if body, err := os.ReadFile(path); err != nil || !bytes.Equal(body, original) || g.reconciliationBlocked.Load() != 0 {
				t.Fatal("decoder read failure damaged or quarantined the source")
			}
			g.legacyReconciliationReader = nil
			g.messageEventApply = func(messageReconciliation) error { return nil }
			if err := g.drainMessageReconciliation(); err != nil {
				t.Fatal("source did not recover after the temporary read error")
			}
		})
	}
}

func TestReconciliationLegacyInputClassification(t *testing.T) {
	for _, input := range []string{"empty", "malformed", "io-error", "cancelled"} {
		t.Run(input, func(t *testing.T) {
			g, _ := reconciliationTestGateway(t)
			path := filepath.Join(g.svc.DataRoot, messageReconciliationFile)
			if input == "io-error" {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			} else {
				body := []byte(`[{"operation":"delete"`)
				if input == "empty" {
					body = nil
				}
				if err := fsutil.WriteAtomic(path, body, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if input == "cancelled" {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				g.reconciliationCtx = ctx
			}
			err := g.migrateLegacyReconciliation(g.svc.DataRoot)
			switch input {
			case "empty":
				if err != nil || g.reconciliationBlocked.Load() != 0 {
					t.Fatal("empty legacy source was quarantined as an unknown delete")
				}
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("empty legacy source was not removed")
				}
			case "malformed":
				if !errors.Is(err, errReconciliationBlocked) {
					t.Fatal("damaged legacy source did not retain its unresolved intent")
				}
			default:
				if err == nil || errors.Is(err, errReconciliationBlocked) {
					t.Fatal("temporary read failure was classified as corrupt input")
				}
				if _, err := os.Stat(path); err != nil {
					t.Fatal("temporary read failure lost its retryable source")
				}
			}
		})
	}
}

func TestReconciliationMalformedRevisionsDoNotRetryForever(t *testing.T) {
	for _, target := range []string{"tombstone", "observation"} {
		t.Run(target, func(t *testing.T) {
			g, dir := reconciliationTestGateway(t)
			event := reconciliationCapturedEdit(t, g)
			g.applyMessageChange(event)
			query := `UPDATE privacy_tombstones SET at='unparseable' WHERE kind='message' AND source_id='m1'`
			if target == "observation" {
				query = `UPDATE memory_observations SET edited_at='unparseable' WHERE native_message_id='m1'`
			}
			result, err := g.svc.Store.DB.Exec(query)
			if err != nil {
				t.Fatal(err)
			}
			if rows, err := result.RowsAffected(); err != nil || rows != 1 {
				t.Fatal("malformed revision fixture was not installed")
			}
			event.At = event.At.Add(time.Minute)
			if err := g.queueMessageReconciliation(event); err != nil {
				t.Fatal(err)
			}
			calls := 0
			g.messageEventApply = func(messageReconciliation) error { calls++; return nil }
			if err := g.drainMessageReconciliation(); err != nil || calls != 1 || len(reconciliationNames(t, dir)) != 0 {
				t.Fatal("malformed timestamp left an edit retrying forever")
			}
		})
	}
}

func TestReconciliationRevisionQueryUsesDrainCancellation(t *testing.T) {
	g, _ := reconciliationTestGateway(t)
	event := reconciliationCapturedEdit(t, g)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	g.reconciliationCtx = ctx
	calls := 0
	g.messageEventApply = func(messageReconciliation) error { calls++; return nil }
	if err := g.applyMessageEvent(event); !errors.Is(err, context.Canceled) || calls != 0 {
		t.Fatal("revision query ignored drain cancellation")
	}
}

func TestReconciliationLivePanicPersistsEveryHandlerIntent(t *testing.T) {
	for _, operation := range []string{"edit", "delete", "bulk-delete"} {
		t.Run(operation, func(t *testing.T) {
			g, dir := reconciliationTestGateway(t)
			g.cfg = config.Discord{MessageContent: true}
			var logs bytes.Buffer
			g.log = slog.New(slog.NewTextHandler(&logs, nil))
			calls := 0
			g.messageEventApply = func(messageReconciliation) error {
				calls++
				panic("private-panic-marker")
			}
			at := time.Date(2026, 10, 5, 1, 0, 0, 0, time.UTC)
			message := &discordgo.Message{ID: "intent", ChannelID: "c", GuildID: "g", Content: "revised", EditedTimestamp: &at}
			switch operation {
			case "edit":
				g.onMessageUpdate(nil, &discordgo.MessageUpdate{Message: message})
			case "delete":
				g.onMessageDelete(nil, &discordgo.MessageDelete{Message: message})
			case "bulk-delete":
				g.onMessageDeleteBulk(nil, &discordgo.MessageDeleteBulk{GuildID: "g", ChannelID: "c", Messages: []string{"intent", "second"}})
			}
			if calls != 3 {
				t.Fatal("live panic bypassed the normal retry path")
			}
			names := reconciliationNames(t, dir)
			if len(names) != 1 {
				t.Fatal("live panic lost its durable intent")
			}
			queued, err := readReconciliationItem(filepath.Join(dir, names[0]))
			if err != nil || queued.MessageIDs[0] != "intent" || (operation == "edit" && (queued.Content != message.Content || !queued.At.Equal(at))) ||
				(operation == "bulk-delete" && len(queued.MessageIDs) != 2) {
				t.Fatal("panic recovery changed the saved intent")
			}
			if bytes.Contains(logs.Bytes(), []byte("private-panic-marker")) || !bytes.Contains(logs.Bytes(), []byte("stack=")) {
				t.Fatal("panic diagnostics exposed its value or omitted the stack")
			}
			// A fresh gateway must find and apply the event saved before the handler returned.
			restarted := &Gateway{svc: g.svc, ctx: context.Background(), log: g.log}
			replayed := 0
			restarted.messageEventApply = func(event messageReconciliation) error { replayed++; return nil }
			if err := restarted.drainMessageReconciliation(); err != nil || replayed != 1 || len(reconciliationNames(t, dir)) != 0 {
				t.Fatal("saved panic intent did not survive restart and recovery")
			}
		})
	}
}

func TestReconciliationBlockedBackoffWakesForNewDelete(t *testing.T) {
	g, dir := reconciliationTestGateway(t)
	if err := fsutil.WriteAtomic(filepath.Join(dir, "d-unresolved.json.corrupt"), []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	g.reconciliationCtx = ctx
	g.reconciliationWake = make(chan struct{}, 1)
	g.reconciliationWait = nil // exercise the production timer and wake select
	waiting := make(chan time.Duration, 4)
	g.retryJitter = func(delay time.Duration) time.Duration {
		waiting <- delay
		return time.Hour // no wall-clock retry can make this test pass
	}
	applied := make(chan messageReconciliation, 1)
	g.messageEventApply = func(event messageReconciliation) error { applied <- event; return nil }
	done := make(chan struct{})
	go func() { g.runReconciliationDrain(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("drain did not cancel its backoff")
		}
	})
	select {
	case <-waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("drain never entered blocked backoff")
	}
	if err := g.queueMessageReconciliation(messageReconciliation{Operation: "delete", MessageIDs: []string{"new"}}); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-applied:
		if event.Operation != "delete" || event.MessageIDs[0] != "new" {
			t.Fatal("wake applied the wrong intent")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("new delete was held until the blocked backoff expired")
	}
	select {
	case <-waiting: // the woken pass completed before we inspect its files
	case <-time.After(5 * time.Second):
		t.Fatal("woken pass did not finish")
	}
	if names, err := listReconciliation(dir, "d-", 1, nil); err != nil || len(names) != 0 || g.reconciliationBlocked.Load() != 1 {
		t.Fatal("wake did not preserve the blocker and finish the healthy delete")
	}
}

func TestReconciliationCorruptEditIsIsolatedAndCollisionPreserved(t *testing.T) {
	g, dir := reconciliationTestGateway(t)
	name := editFileName("c", "damaged")
	path := filepath.Join(dir, name)
	if err := fsutil.WriteAtomic(path+".corrupt", []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fsutil.WriteAtomic(path, []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	g.messageEventApply = func(messageReconciliation) error { calls++; return nil }
	if err := g.queueMessageReconciliation(messageReconciliation{Operation: "edit", ChannelID: "c", MessageIDs: []string{"other"}}); err != nil {
		t.Fatal(err)
	}
	if err := g.drainMessageReconciliation(); err != nil || calls != 1 || g.reconciliationBlocked.Load() != 0 {
		t.Fatalf("damaged edit blocked unrelated work: %v", err)
	}
	for suffix, want := range map[string]string{".corrupt": "original", ".1.corrupt": "broken"} {
		if body, err := os.ReadFile(path + suffix); err != nil || string(body) != want {
			t.Fatal("quarantine collision lost bytes")
		}
	}
}

func TestReconciliationLiveEditUsesCachedQueueState(t *testing.T) {
	g, dir := reconciliationTestGateway(t)
	g.reconciliationPending.Store(true)
	if err := g.refreshReconciliationBlocked(g.svc.DataRoot, dir); err != nil {
		t.Fatal(err)
	}
	// An external file is picked up on the next drain scan, never on every live edit.
	if err := fsutil.WriteAtomic(filepath.Join(dir, "d-external.json.corrupt"), []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	g.messageEventApply = func(messageReconciliation) error { calls++; return nil }
	g.applyMessageChange(messageReconciliation{Operation: "edit", ChannelID: "c", MessageIDs: []string{"other"}})
	if calls != 1 {
		t.Fatal("live edit enumerated the queue instead of using cached state")
	}
	if err := g.refreshReconciliationBlocked(g.svc.DataRoot, dir); err != nil {
		t.Fatal(err)
	}
	g.applyMessageChange(messageReconciliation{Operation: "edit", ChannelID: "c", MessageIDs: []string{"other"}})
	if calls != 1 {
		t.Fatal("live edit ignored refreshed privacy blocker")
	}
}

func TestReconciliationMigrationFailureStillDrainsDurableDeletes(t *testing.T) {
	for _, failure := range []string{"write", "remove"} {
		t.Run(failure, func(t *testing.T) {
			g, dir := reconciliationTestGateway(t)
			path := filepath.Join(g.svc.DataRoot, messageReconciliationFile)
			if err := fsutil.WriteAtomic(path, []byte(`[{"operation":"edit","message_ids":["legacy"]}]`), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, operation := range []string{"delete", "edit"} {
				if err := g.queueMessageReconciliation(messageReconciliation{Operation: operation, MessageIDs: []string{operation}}); err != nil {
					t.Fatal(err)
				}
			}
			injected := errors.New("temporary legacy migration failure")
			if failure == "write" {
				g.reconciliationWrite = func(path string, event messageReconciliation) error {
					if event.MessageIDs[0] == "legacy" {
						return injected
					}
					return writeReconciliationItem(path, event)
				}
			} else {
				g.reconciliationRemove = func(target string) error {
					if target == path {
						return injected
					}
					return fsutil.RemoveDurable(target)
				}
			}
			var applied []messageReconciliation
			g.messageEventApply = func(event messageReconciliation) error { applied = append(applied, event); return nil }
			for pass := 0; pass < 3; pass++ {
				if err := g.drainMessageReconciliation(); !errors.Is(err, injected) {
					t.Fatalf("migration failure was not retained: %v", err)
				}
				if len(applied) != 1 || applied[0].Operation != "delete" {
					t.Fatal("legacy failure stopped durable deletes or allowed edits")
				}
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatal("failed migration discarded its source")
			}
			g.reconciliationWrite, g.reconciliationRemove = nil, nil
			if err := g.drainMessageReconciliation(); err != nil {
				t.Fatal(err)
			}
			if len(applied) != 3 || len(reconciliationNames(t, dir)) != 0 || g.reconciliationPending.Load() {
				t.Fatal("migration recovery did not finish the waiting edits")
			}
		})
	}
}
