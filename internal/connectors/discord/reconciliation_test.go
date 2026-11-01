package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"masamong/internal/application"
	"masamong/internal/config"
	"masamong/internal/fsutil"
	"masamong/internal/memory/engine"
)

func TestMessageHandlersRetryAndPersistReconciliation(t *testing.T) {
	svc := svcForTest(t)
	g := &Gateway{svc: svc, cfg: config.Discord{MessageContent: true}, ctx: context.Background(),
		log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	var waits []time.Duration
	g.reconciliationWait = func(_ context.Context, d time.Duration) bool {
		waits = append(waits, d)
		return true
	}
	calls := 0
	g.messageEventApply = func(messageReconciliation) error {
		calls++
		if calls < 3 {
			return errors.New("temporary store failure")
		}
		return nil
	}
	at := time.Date(2026, 10, 5, 1, 2, 3, 0, time.UTC)
	g.onMessageUpdate(nil, &discordgo.MessageUpdate{Message: &discordgo.Message{ID: "m-edit", GuildID: "g", ChannelID: "c", Content: "revised", EditedTimestamp: &at}})
	if calls != 3 || len(waits) != 2 || waits[0] != messageRetryBackoff[0] || waits[1] != messageRetryBackoff[1] {
		t.Fatalf("edit retries calls=%d waits=%v", calls, waits)
	}
	dir := filepath.Join(svc.DataRoot, messageReconciliationDir)
	if names := reconciliationNames(t, dir); len(names) != 0 {
		t.Fatalf("successful retry left reconciliation files: %v", names)
	}

	calls, waits = 0, nil
	var applied []messageReconciliation
	failing := true
	g.messageEventApply = func(event messageReconciliation) error {
		calls++
		if failing {
			return errors.New("store remains unavailable")
		}
		applied = append(applied, event)
		return nil
	}
	g.onMessageUpdate(nil, &discordgo.MessageUpdate{Message: &discordgo.Message{ID: "m-deferred", GuildID: "g", ChannelID: "c", Content: "latest", EditedTimestamp: &at}})
	g.onMessageDeleteBulk(nil, &discordgo.MessageDeleteBulk{GuildID: "g", ChannelID: "c", Messages: []string{"m1", "m2"}})
	g.onMessageDelete(nil, &discordgo.MessageDelete{Message: &discordgo.Message{ID: "m3", GuildID: "g", ChannelID: "c"}})
	if calls != 9 {
		t.Fatalf("persistent failures were not retried three times per event: %d calls", calls)
	}
	names := reconciliationNames(t, dir)
	if len(names) != 3 {
		t.Fatalf("persisted reconciliation files = %v", names)
	}
	for _, name := range names {
		if err := fsutil.VerifyPrivateFile(filepath.Join(dir, name)); err != nil {
			t.Fatalf("reconciliation data is not private: %v", err)
		}
	}
	failing = true
	calls = 0
	if err := g.drainMessageReconciliation(); err == nil || calls == 0 {
		t.Fatalf("failed drain calls=%d err=%v", calls, err)
	}
	if after := reconciliationNames(t, dir); !reflect.DeepEqual(after, names) {
		t.Fatalf("failed drain discarded work: %v -> %v", names, after)
	}
	failing = false
	if err := g.drainMessageReconciliation(); err != nil {
		t.Fatalf("drain reconciliation: %v", err)
	}
	if len(applied) != 3 {
		t.Fatalf("drain applied %d events, want 3", len(applied))
	}
	// deletes are applied before edits, so a privacy intent is never overtaken by an older edit
	if applied[0].Operation != "delete" || applied[1].Operation != "delete" || applied[2].Operation != "edit" || applied[2].Content != "latest" {
		t.Fatalf("wrong drain order: %+v", applied)
	}
	if left := reconciliationNames(t, dir); len(left) != 0 {
		t.Fatalf("completed reconciliation files remain: %v", left)
	}
}

func reconciliationNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func reconciliationTestGateway(t *testing.T) (*Gateway, string) {
	t.Helper()
	svc := svcForTest(t)
	g := &Gateway{svc: svc, ctx: context.Background(), log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	g.reconciliationWait = func(context.Context, time.Duration) bool { return true }
	return g, filepath.Join(svc.DataRoot, messageReconciliationDir)
}

// A thousand failed edits of one message leave one small file holding the newest text, not a thousand
// events; an older edit that arrives late does not replace a newer one.
func TestDeferredEditsKeepOnlyTheLatestPerMessage(t *testing.T) {
	g, dir := reconciliationTestGateway(t)
	base := time.Date(2026, 10, 5, 1, 0, 0, 0, time.UTC)
	for i := 0; i < 1000; i++ {
		if err := g.queueMessageReconciliation(messageReconciliation{Operation: "edit", GuildID: "g", ChannelID: "c",
			MessageIDs: []string{"m1"}, Content: fmt.Sprintf("v%d", i), At: base.Add(time.Duration(i) * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := g.queueMessageReconciliation(messageReconciliation{Operation: "edit", GuildID: "g", ChannelID: "c",
		MessageIDs: []string{"m1"}, Content: "stale", At: base}); err != nil {
		t.Fatal(err)
	}
	if err := g.queueMessageReconciliation(messageReconciliation{Operation: "edit", GuildID: "g", ChannelID: "c",
		MessageIDs: []string{"m2"}, Content: "other", At: base}); err != nil {
		t.Fatal(err)
	}
	if names := reconciliationNames(t, dir); len(names) != 2 {
		t.Fatalf("files = %v, want one per edited message", names)
	}
	var applied []messageReconciliation
	g.messageEventApply = func(e messageReconciliation) error { applied = append(applied, e); return nil }
	if err := g.drainMessageReconciliation(); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, e := range applied {
		got[e.MessageIDs[0]] = e.Content
	}
	if len(applied) != 2 || got["m1"] != "v999" || got["m2"] != "other" {
		t.Fatalf("applied %+v", applied)
	}
}

// A delete supersedes the edits of its messages, whether the delete itself was deferred or went through,
// and a deferred delete is never dropped.
func TestDeleteSupersedesDeferredEditsAndIsKept(t *testing.T) {
	g, dir := reconciliationTestGateway(t)
	at := time.Date(2026, 10, 5, 1, 0, 0, 0, time.UTC)
	edit := func(id string) messageReconciliation {
		return messageReconciliation{Operation: "edit", GuildID: "g", ChannelID: "c", MessageIDs: []string{id}, Content: "secret " + id, At: at}
	}
	for _, id := range []string{"m1", "m2", "m3"} {
		if err := g.queueMessageReconciliation(edit(id)); err != nil {
			t.Fatal(err)
		}
	}
	// deferred delete of m1: written first, then the edit is removed
	if err := g.queueMessageReconciliation(messageReconciliation{Operation: "delete", GuildID: "g", ChannelID: "c", MessageIDs: []string{"m1"}, At: at}); err != nil {
		t.Fatal(err)
	}
	// delete of m2 that goes through at once
	g.messageEventApply = func(messageReconciliation) error { return nil }
	g.applyMessageChange(messageReconciliation{Operation: "delete", GuildID: "g", ChannelID: "c", MessageIDs: []string{"m2"}})

	var applied []messageReconciliation
	g.messageEventApply = func(e messageReconciliation) error { applied = append(applied, e); return nil }
	if err := g.drainMessageReconciliation(); err != nil {
		t.Fatal(err)
	}
	if len(applied) != 2 || applied[0].Operation != "delete" || applied[0].MessageIDs[0] != "m1" ||
		applied[1].Operation != "edit" || applied[1].MessageIDs[0] != "m3" {
		t.Fatalf("applied %+v, want delete m1 then edit m3 only", applied)
	}
	if left := reconciliationNames(t, dir); len(left) != 0 {
		t.Fatalf("files left: %v", left)
	}
}

// An edit replaced while the drain was applying the older one stays queued instead of being removed.
func TestDrainKeepsAnEditThatChangedWhileItWasApplied(t *testing.T) {
	g, dir := reconciliationTestGateway(t)
	at := time.Date(2026, 10, 5, 1, 0, 0, 0, time.UTC)
	if err := g.queueMessageReconciliation(messageReconciliation{Operation: "edit", GuildID: "g", ChannelID: "c", MessageIDs: []string{"m1"}, Content: "v1", At: at}); err != nil {
		t.Fatal(err)
	}
	var applied []string
	g.messageEventApply = func(e messageReconciliation) error {
		applied = append(applied, e.Content)
		if e.Content == "v1" {
			return g.queueMessageReconciliation(messageReconciliation{Operation: "edit", GuildID: "g", ChannelID: "c", MessageIDs: []string{"m1"}, Content: "v2", At: at.Add(time.Second)})
		}
		return nil
	}
	if err := g.drainMessageReconciliation(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(applied, []string{"v1", "v2"}) || len(reconciliationNames(t, dir)) != 0 {
		t.Fatalf("applied %v, files %v", applied, reconciliationNames(t, dir))
	}
}

// The first file format (one JSON array) is read once, element by element, and moved into the queue.
func TestLegacyReconciliationArrayIsMigrated(t *testing.T) {
	g, dir := reconciliationTestGateway(t)
	at := time.Date(2026, 10, 5, 1, 0, 0, 0, time.UTC)
	legacy := []messageReconciliation{
		{Operation: "edit", GuildID: "g", ChannelID: "c", MessageIDs: []string{"m1"}, Content: "old", At: at},
		{Operation: "edit", GuildID: "g", ChannelID: "c", MessageIDs: []string{"m1"}, Content: "new", At: at.Add(time.Second)},
		{Operation: "delete", GuildID: "g", ChannelID: "c", MessageIDs: []string{"m2", "m3"}, At: at},
	}
	body, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(g.svc.DataRoot, messageReconciliationFile)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	var applied []messageReconciliation
	g.messageEventApply = func(e messageReconciliation) error { applied = append(applied, e); return nil }
	if err := g.drainMessageReconciliation(); err != nil {
		t.Fatal(err)
	}
	if len(applied) != 2 || applied[0].Operation != "delete" || len(applied[0].MessageIDs) != 2 || applied[1].Content != "new" {
		t.Fatalf("applied %+v", applied)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy file remains: %v", err)
	}
	if left := reconciliationNames(t, dir); len(left) != 0 {
		t.Fatalf("files left: %v", left)
	}
}

// A damaged old file is set aside once (what could be read was moved), not read again at every start.
func TestDamagedLegacyReconciliationIsSetAside(t *testing.T) {
	g, _ := reconciliationTestGateway(t)
	path := filepath.Join(g.svc.DataRoot, messageReconciliationFile)
	good := `{"operation":"delete","guild_id":"g","channel_id":"c","message_ids":["m1"],"at":"2026-10-05T01:00:00Z"}`
	if err := os.WriteFile(path, []byte(`[`+good+`,{"operation":"ed`), 0o600); err != nil {
		t.Fatal(err)
	}
	var applied []messageReconciliation
	g.messageEventApply = func(e messageReconciliation) error { applied = append(applied, e); return nil }
	if err := g.drainMessageReconciliation(); !errors.Is(err, errReconciliationBlocked) {
		t.Fatalf("damaged intent must remain blocked: %v", err)
	}
	if len(applied) != 1 || applied[0].MessageIDs[0] != "m1" {
		t.Fatalf("readable part not applied: %+v", applied)
	}
	if _, err := os.Stat(path + ".unresolved.corrupt"); err != nil {
		t.Fatalf("damaged file not kept aside: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("damaged file still in place: %v", err)
	}
}

func TestReconciliationCorruptDeleteBlocksEditsAndSurvivesRestart(t *testing.T) {
	g, dir := reconciliationTestGateway(t)
	event := messageReconciliation{Operation: "edit", ChannelID: "c", MessageIDs: []string{"m1"}, Content: "pending", At: time.Date(2026, 10, 5, 1, 0, 0, 0, time.UTC)}
	if err := g.queueMessageReconciliation(event); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "d-damaged.json")
	// A delete of this same message was truncated before its durable edit cleanup happened.
	if err := fsutil.WriteAtomic(path, []byte(`{"operation":"delete","message_ids":["m1"`), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	g.log = slog.New(slog.NewTextHandler(&logs, nil))
	calls := 0
	g.messageEventApply = func(messageReconciliation) error { calls++; return nil }
	for pass := 0; pass < 2; pass++ {
		if err := g.drainMessageReconciliation(); !errors.Is(err, errReconciliationBlocked) {
			t.Fatalf("pass %d did not report blocked recovery", pass)
		}
		if calls != 0 || !g.reconciliationPending.Load() || g.reconciliationBlocked.Load() != 1 {
			t.Fatal("unreadable delete allowed an edit or completion")
		}
	}
	if bytes.Count(logs.Bytes(), []byte("recovery state changed")) != 1 {
		t.Fatal("unchanged blocked state was logged more than once")
	}
	if body, err := os.ReadFile(path + ".corrupt"); err != nil || !bytes.Equal(body, []byte(`{"operation":"delete","message_ids":["m1"`)) {
		t.Fatal("quarantine did not preserve delete bytes")
	}
	restarted := &Gateway{svc: g.svc, ctx: context.Background(), log: g.log, messageEventApply: g.messageEventApply}
	restarted.reconciliationPending.Store(true)
	if err := restarted.drainMessageReconciliation(); !errors.Is(err, errReconciliationBlocked) || restarted.reconciliationBlocked.Load() != 1 {
		t.Fatal("restart forgot unresolved delete")
	}
	event.At = event.At.Add(time.Minute)
	restarted.applyMessageChange(event)
	if calls != 0 {
		t.Fatal("live edit bypassed unresolved delete")
	}
	if err := os.Remove(path + ".corrupt"); err != nil {
		t.Fatal(err)
	}
	if err := restarted.drainMessageReconciliation(); err != nil || calls != 1 || restarted.reconciliationPending.Load() {
		t.Fatalf("recovered queue did not settle: %v", err)
	}
}

func TestReconciliationUnreadableLegacyArrayBlocksEdits(t *testing.T) {
	good := `{"operation":"edit","channel_id":"c","message_ids":["m1"],"content":"pending","at":"2026-10-05T01:00:00Z"}`
	for _, tc := range []struct{ name, body string }{
		{"corrupt-delete", `[` + good + `,{"operation":"delete","message_ids":["m1"`},
		{"missing-close", `[` + good},
		{"trailing-data", `[` + good + `] {}`},
		{"unknown-operation", `[` + good + `,{"operation":"unknown","message_ids":["m1"]}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, _ := reconciliationTestGateway(t)
			path := filepath.Join(g.svc.DataRoot, messageReconciliationFile)
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			calls := 0
			g.messageEventApply = func(messageReconciliation) error { calls++; return nil }
			for pass := 0; pass < 2; pass++ {
				if err := g.drainMessageReconciliation(); !errors.Is(err, errReconciliationBlocked) || calls != 0 || !g.reconciliationPending.Load() {
					t.Fatalf("unresolved array allowed an edit or completion: %v", err)
				}
			}
			if body, err := os.ReadFile(path + ".unresolved.corrupt"); err != nil || string(body) != tc.body {
				t.Fatal("unreadable legacy bytes were lost")
			}
		})
	}
}

func TestReconciliationUnknownQueuedOperationIsBlocked(t *testing.T) {
	g, dir := reconciliationTestGateway(t)
	if err := writeReconciliationItem(filepath.Join(dir, "d-unknown.json"), messageReconciliation{Operation: "unknown", MessageIDs: []string{"m1"}}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	g.messageEventApply = func(messageReconciliation) error { calls++; return nil }
	for pass := 0; pass < 2; pass++ {
		if err := g.drainMessageReconciliation(); !errors.Is(err, errReconciliationBlocked) || calls != 0 || g.reconciliationBlocked.Load() != 1 {
			t.Fatalf("unknown operation was retried instead of blocked: %v", err)
		}
	}
}

func TestReconciliationLegacyMigrationRetriesAreIdempotent(t *testing.T) {
	g, dir := reconciliationTestGateway(t)
	legacy := []messageReconciliation{
		{Operation: "delete", MessageIDs: []string{"m1"}},
		{Operation: "delete", MessageIDs: []string{"m2"}},
		{Operation: "edit", ChannelID: "c", MessageIDs: []string{"m3"}, Content: "pending"},
		{Operation: "delete", MessageIDs: []string{"m4"}},
	}
	body, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(g.svc.DataRoot, messageReconciliationFile)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	fixed := time.Date(2026, 10, 5, 1, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, fixed, fixed); err != nil {
		t.Fatal(err)
	}
	g.reconciliationWrite = func(path string, item messageReconciliation) error {
		if item.MessageIDs[0] == "m4" {
			return errors.New("injected later write failure")
		}
		return writeReconciliationItem(path, item)
	}
	var names []string
	var snapshots [][]byte
	for pass := 0; pass < 3; pass++ {
		if err := g.migrateLegacyReconciliation(g.svc.DataRoot); err == nil {
			t.Fatal("injected migration failure was ignored")
		}
		got := reconciliationNames(t, dir)
		if len(got) != 3 {
			t.Fatalf("retry created extra files: got %d, want 3", len(got))
		}
		if pass == 0 {
			names = got
		} else if !reflect.DeepEqual(names, got) {
			t.Fatal("migration names changed across retries")
		}
		for i, name := range got {
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			if pass == 0 {
				snapshots = append(snapshots, data)
			} else if !bytes.Equal(snapshots[i], data) {
				t.Fatal("retry manufactured a new timestamp or payload")
			}
		}
		// A process restart loses sequence counters but must preserve source names and timestamps.
		g = &Gateway{svc: g.svc, ctx: context.Background(), log: g.log, reconciliationWrite: g.reconciliationWrite}
	}
	g.reconciliationWrite = nil
	calls := 0
	g.messageEventApply = func(messageReconciliation) error { calls++; return nil }
	if err := g.drainMessageReconciliation(); err != nil || calls != 4 || len(reconciliationNames(t, dir)) != 0 {
		t.Fatalf("completed migration did not apply four unique records: calls=%d err=%v", calls, err)
	}
}

func TestReconciliationLegacyRemovalFailureDoesNotDuplicateRecords(t *testing.T) {
	g, dir := reconciliationTestGateway(t)
	path := filepath.Join(g.svc.DataRoot, messageReconciliationFile)
	if err := os.WriteFile(path, []byte(`[{"operation":"delete","message_ids":["m1"]}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	g.reconciliationRemove = func(string) error { return errors.New("injected legacy removal failure") }
	var first []string
	for pass := 0; pass < 3; pass++ {
		if err := g.migrateLegacyReconciliation(g.svc.DataRoot); err == nil {
			t.Fatal("source removal failure was ignored")
		}
		names := reconciliationNames(t, dir)
		if pass == 0 {
			first = names
		}
		if len(names) != 1 || !reflect.DeepEqual(first, names) {
			t.Fatal("legacy removal retry duplicated an already migrated delete")
		}
	}
}

func TestReconciliationApplyPanicKeepsFileAndBackgroundDrainRetries(t *testing.T) {
	g, dir := reconciliationTestGateway(t)
	if err := g.queueMessageReconciliation(messageReconciliation{Operation: "delete", MessageIDs: []string{"m1"}}); err != nil {
		t.Fatal(err)
	}
	waiting, retry := make(chan time.Duration, 1), make(chan struct{})
	g.retryJitter = func(d time.Duration) time.Duration { return d }
	g.reconciliationWait = func(ctx context.Context, delay time.Duration) bool {
		waiting <- delay
		select {
		case <-retry:
			return true
		case <-ctx.Done():
			return false
		}
	}
	var calls atomic.Int32
	applied := make(chan struct{}, 1)
	g.messageEventApply = func(messageReconciliation) error {
		if calls.Add(1) == 1 {
			panic("injected apply panic")
		}
		applied <- struct{}{}
		return nil
	}
	g.startReconciliationDrain(context.Background())
	t.Cleanup(func() { g.Wait(5 * time.Second) })
	select {
	case delay := <-waiting:
		if delay != reconciliationDrainInitial {
			t.Fatal("panic did not enter the initial backoff")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("panic permanently stopped the drain")
	}
	if len(reconciliationNames(t, dir)) != 1 {
		t.Fatal("panic discarded the queued privacy intent")
	}
	accounted := make(chan struct{})
	go func() { g.inflight.Wait(); close(accounted) }()
	select {
	case <-accounted:
	case <-time.After(5 * time.Second):
		t.Fatal("panic leaked the inflight work count")
	}
	close(retry)
	select {
	case <-applied:
	case <-time.After(5 * time.Second):
		t.Fatal("drain did not retry after the panic")
	}
	if !g.Wait(5 * time.Second) {
		t.Fatal("shutdown did not join the recovered drain")
	}
	select {
	case <-g.reconciliationDone:
	default:
		t.Fatal("Wait reported completion before drain exit")
	}
	if len(reconciliationNames(t, dir)) != 0 {
		t.Fatal("successful retry did not remove its file")
	}
}

func TestReconciliationWaitJoinsAndCancelsPausedMigration(t *testing.T) {
	g, dir := reconciliationTestGateway(t)
	path := filepath.Join(g.svc.DataRoot, messageReconciliationFile)
	if err := os.WriteFile(path, []byte(`[{"operation":"delete","message_ids":["m1"]},{"operation":"delete","message_ids":["m2"]}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	paused, resume := make(chan struct{}), make(chan struct{})
	g.legacyReconciliationStep = func(int) { close(paused); <-resume }
	g.startReconciliationDrain(context.Background())
	t.Cleanup(func() { g.Wait(5 * time.Second) })
	<-paused
	returned := make(chan bool, 1)
	go func() { returned <- g.Wait(5 * time.Second) }()
	select {
	case <-g.reconciliationCtx.Done():
	case <-returned:
		close(resume)
		t.Fatal("Wait completed without cancelling or joining migration")
	case <-time.After(5 * time.Second):
		close(resume)
		t.Fatal("Wait did not cancel the migration context")
	}
	select {
	case <-returned:
		close(resume)
		t.Fatal("Wait reported completion while migration was paused")
	default:
	}
	close(resume)
	if !<-returned {
		t.Fatal("cancelled migration did not exit within shutdown timeout")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("cancelled migration lost its source")
	}
	if _, err := os.Stat(path + ".corrupt"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cancellation was misclassified as corrupt input")
	}
	if len(reconciliationNames(t, dir)) != 0 {
		t.Fatal("migration queued records after cancellation")
	}
}

// The background drain applies a deferred change without anything waiting on it, backs off while the
// store keeps failing, and sleeps until new work is deferred.
func TestBackgroundDrainRetriesWithGrowingWaitAndIsWokenByNewWork(t *testing.T) {
	g, dir := reconciliationTestGateway(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g.ctx = ctx
	g.reconciliationWake = make(chan struct{}, 1)
	g.retryJitter = func(d time.Duration) time.Duration { return d }
	var mu sync.Mutex
	var waits []time.Duration
	g.reconciliationWait = func(_ context.Context, d time.Duration) bool {
		mu.Lock()
		waits = append(waits, d)
		mu.Unlock()
		return true
	}
	var failures atomic.Int32
	applied := make(chan messageReconciliation, 4)
	g.messageEventApply = func(e messageReconciliation) error {
		if failures.Add(1) <= 3 {
			return errors.New("store down")
		}
		applied <- e
		return nil
	}
	if err := g.queueMessageReconciliation(messageReconciliation{Operation: "delete", GuildID: "g", ChannelID: "c", MessageIDs: []string{"m1"}, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { g.runReconciliationDrain(ctx); close(done) }()
	select {
	case e := <-applied:
		if e.Operation != "delete" {
			t.Fatalf("applied %+v", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("background drain never applied the change")
	}
	mu.Lock()
	got := append([]time.Duration(nil), waits...)
	mu.Unlock()
	want := []time.Duration{reconciliationDrainInitial, 2 * reconciliationDrainInitial, 4 * reconciliationDrainInitial}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("drain waits = %v, want %v", got, want)
	}
	// idle now: a new deferral wakes it
	if err := g.queueMessageReconciliation(messageReconciliation{Operation: "delete", GuildID: "g", ChannelID: "c", MessageIDs: []string{"m2"}, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-applied:
		if e.MessageIDs[0] != "m2" {
			t.Fatalf("applied %+v", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("idle drain was not woken")
	}
	cancel()
	<-done
	if left := reconciliationNames(t, dir); len(left) != 0 {
		t.Fatalf("files left: %v", left)
	}
}

// A deferred delete is never given up on: it stays queued across any number of failed passes.
func TestFailedDrainNeverDropsADelete(t *testing.T) {
	g, dir := reconciliationTestGateway(t)
	g.messageEventApply = func(messageReconciliation) error { return errors.New("down") }
	for i := 0; i < 50; i++ {
		if err := g.queueMessageReconciliation(messageReconciliation{Operation: "delete", GuildID: "g", ChannelID: "c", MessageIDs: []string{fmt.Sprintf("m%d", i)}, At: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	for pass := 0; pass < 3; pass++ {
		if err := g.drainMessageReconciliation(); err == nil {
			t.Fatal("drain reported success while the store is down")
		}
	}
	if left := reconciliationNames(t, dir); len(left) != 50 {
		t.Fatalf("%d delete files left, want 50", len(left))
	}
}

// A deleted request (one message, or a bulk delete) tells the picture queue which messages of which
// channel are gone, so a picture asked for by them is not drawn or posted.
func TestMessageDeleteCancelsThePicturesOfTheDeletedRequests(t *testing.T) {
	svc := svcForTest(t)
	g := &Gateway{svc: svc, ctx: context.Background(), log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	g.messageEventApply = func(messageReconciliation) error { return nil }
	type cancel struct {
		channel string
		ids     []string
	}
	var got []cancel
	g.cancelPictures = func(channelID string, ids []string) { got = append(got, cancel{channelID, ids}) }

	g.onMessageDelete(nil, &discordgo.MessageDelete{Message: &discordgo.Message{ID: "m1", GuildID: "g", ChannelID: "c1"}})
	g.onMessageDeleteBulk(nil, &discordgo.MessageDeleteBulk{GuildID: "g", ChannelID: "c2", Messages: []string{"m2", "m3"}})
	g.onMessageDelete(nil, &discordgo.MessageDelete{Message: &discordgo.Message{ID: "", GuildID: "g", ChannelID: "c3"}}) // no ID: nothing to cancel
	want := []cancel{{"c1", []string{"m1"}}, {"c2", []string{"m2", "m3"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cancelled %+v, want %+v", got, want)
	}
}

// A newer edit that goes through live removes an older queued edit of the same message, so the drain
// never applies the older text over it (it could bring back words the person took out).
func TestLiveEditSupersedesAnOlderQueuedEdit(t *testing.T) {
	g, dir := reconciliationTestGateway(t)
	at := time.Date(2026, 10, 5, 1, 0, 0, 0, time.UTC)
	if err := g.queueMessageReconciliation(messageReconciliation{Operation: "edit", GuildID: "g", ChannelID: "c",
		MessageIDs: []string{"m1"}, Content: "old text with a secret", At: at}); err != nil {
		t.Fatal(err)
	}
	if err := g.queueMessageReconciliation(messageReconciliation{Operation: "edit", GuildID: "g", ChannelID: "c",
		MessageIDs: []string{"m2"}, Content: "queued newer than live", At: at.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	g.messageEventApply = func(messageReconciliation) error { return nil }
	g.applyMessageChange(messageReconciliation{Operation: "edit", GuildID: "g", ChannelID: "c", MessageIDs: []string{"m1"}, Content: "new text", At: at.Add(time.Minute)})
	g.applyMessageChange(messageReconciliation{Operation: "edit", GuildID: "g", ChannelID: "c", MessageIDs: []string{"m2"}, Content: "live but older", At: at.Add(time.Minute)})

	var applied []messageReconciliation
	g.messageEventApply = func(e messageReconciliation) error { applied = append(applied, e); return nil }
	if err := g.drainMessageReconciliation(); err != nil {
		t.Fatal(err)
	}
	if len(applied) != 1 || applied[0].MessageIDs[0] != "m2" || applied[0].Content != "queued newer than live" {
		t.Fatalf("applied %+v, want only the queued edit that is newer than the live one", applied)
	}
	if left := reconciliationNames(t, dir); len(left) != 0 {
		t.Fatalf("files left: %v", left)
	}
}

func reconciliationCapturedEdit(t *testing.T, g *Gateway) messageReconciliation {
	t.Helper()
	at := time.Date(2026, 10, 5, 1, 0, 0, 0, time.UTC)
	eng, err := g.svc.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", MessageID: "m1", AuthorID: "test-author", Content: "original", At: at}); err != nil {
		t.Fatal(err)
	}
	g.svc.Observe(application.ObservedMessage{GuildID: "g", ChannelID: "c", MessageID: "m1", Content: "original", At: at, FromBot: true})
	return messageReconciliation{Operation: "edit", GuildID: "g", ChannelID: "c", MessageIDs: []string{"m1"}, Content: "removed secret", At: at.Add(time.Minute)}
}

func assertReconciliationRevision(t *testing.T, g *Gateway, want string) {
	t.Helper()
	var text string
	if err := g.svc.Store.Reader().QueryRow(`SELECT content FROM memory_observations WHERE native_message_id='m1'`).Scan(&text); err != nil || text != want {
		t.Fatalf("source revision mismatch: err=%v", err)
	}
	// Inspect the live copy only after every edit has returned; no background worker writes this window.
	lines := reflect.ValueOf(g.svc).Elem().FieldByName("live").FieldByName("chans").MapIndex(reflect.ValueOf("g:c"))
	if lines.Len() != 1 || lines.Index(0).FieldByName("Text").String() != want {
		t.Fatal("live window does not hold the current revision")
	}
}

func TestReconciliationStaleEditCannotReturnAfterRemovalFailure(t *testing.T) {
	g, dir := reconciliationTestGateway(t)
	old := reconciliationCapturedEdit(t, g)
	if err := g.queueMessageReconciliation(old); err != nil {
		t.Fatal(err)
	}
	g.reconciliationRemove = func(string) error { return errors.New("injected removal failure") }
	newer := old
	newer.At, newer.Content = old.At.Add(time.Minute), "current"
	g.applyMessageChange(newer)
	assertReconciliationRevision(t, g, "current")
	if _, err := os.Stat(filepath.Join(dir, editFileName("c", "m1"))); err != nil {
		t.Fatal("fault injection did not keep the old file")
	}
	if err := g.drainMessageReconciliation(); err == nil {
		t.Fatal("removal failure must remain pending")
	}
	assertReconciliationRevision(t, g, "current")
	g.reconciliationRemove = nil
	if err := g.drainMessageReconciliation(); err != nil {
		t.Fatal(err)
	}
	assertReconciliationRevision(t, g, "current")
}

func TestReconciliationStaleLegacyEditCannotReturnAfterLiveEdit(t *testing.T) {
	g, _ := reconciliationTestGateway(t)
	old := reconciliationCapturedEdit(t, g)
	body, err := json.Marshal([]messageReconciliation{old})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(g.svc.DataRoot, messageReconciliationFile), body, 0o600); err != nil {
		t.Fatal(err)
	}
	paused, resume := make(chan struct{}), make(chan struct{})
	g.legacyReconciliationStep = func(int) { close(paused); <-resume }
	done := make(chan error, 1)
	go func() { done <- g.drainMessageReconciliation() }()
	<-paused
	newer := old
	newer.At, newer.Content = old.At.Add(time.Minute), "current"
	g.applyMessageChange(newer)
	close(resume)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	assertReconciliationRevision(t, g, "current")
}

func TestReconciliationEqualRevisionCannotReplaceLiveText(t *testing.T) {
	g, _ := reconciliationTestGateway(t)
	event := reconciliationCapturedEdit(t, g)
	event.Content = "original" // the source text stays the same, but the accepted intent advances
	g.applyMessageChange(event)
	assertReconciliationRevision(t, g, "original")
	event.Content = "obsolete"
	if err := g.queueMessageReconciliation(event); err != nil {
		t.Fatal(err)
	}
	if err := g.drainMessageReconciliation(); err != nil {
		t.Fatal(err)
	}
	assertReconciliationRevision(t, g, "original")
}
