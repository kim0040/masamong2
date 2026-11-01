package discord

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

	"masamong/internal/fsutil"
)

func TestReconciliationDeleteCleanupPanicKeepsDurableIntent(t *testing.T) {
	for _, bulk := range []bool{false, true} {
		for _, stage := range []string{"merge", "pictures"} {
			t.Run(stage+map[bool]string{false: "/single", true: "/bulk"}[bulk], func(t *testing.T) {
				g, dir := reconciliationTestGateway(t)
				var logs bytes.Buffer
				g.log = slog.New(slog.NewTextHandler(&logs, nil))
				g.messageEventApply = func(messageReconciliation) error { return errors.New("store unavailable") }
				panicked := false
				fail := func() { panicked = true; panic("private-cleanup-marker") }
				g.cancelPictures = func(string, []string) {
					if stage == "pictures" {
						fail()
					}
				}
				if stage == "merge" {
					p := &pendingTurn{msgs: []*discordgo.MessageCreate{{Message: &discordgo.Message{ID: "intent"}}}}
					g.merge.pending = map[string]*pendingTurn{"held": p}
					g.merge.byID = map[string]pendingMessage{"intent": {key: "held", turn: p}}
					g.merge.forget = func(string) { fail() }
				}
				if bulk {
					g.onMessageDeleteBulk(nil, &discordgo.MessageDeleteBulk{ChannelID: "channel", Messages: []string{"intent", "second"}})
				} else {
					g.onMessageDelete(nil, &discordgo.MessageDelete{Message: &discordgo.Message{ID: "intent", ChannelID: "channel"}})
				}
				names := reconciliationNames(t, dir)
				if !panicked || len(names) != 1 {
					t.Fatal("cleanup panic lost the delete intent")
				}
				item, err := readReconciliationItem(filepath.Join(dir, names[0]))
				if err != nil || item.Operation != "delete" || item.MessageIDs[0] != "intent" || (bulk && len(item.MessageIDs) != 2) {
					t.Fatal("durable delete payload changed")
				}
				if bytes.Contains(logs.Bytes(), []byte("private-cleanup-marker")) {
					t.Fatal("cleanup panic value reached logs")
				}
				g.messageEventApply = func(messageReconciliation) error { return nil }
				if err := g.drainMessageReconciliation(); err != nil || len(reconciliationNames(t, dir)) != 0 {
					t.Fatal("delete did not replay")
				}
			})
		}
	}
}

func TestReconciliationMalformedLegacyTimestampIsBlockedWithoutLoggingValues(t *testing.T) {
	for _, body := range []string{
		`[{"operation":"delete","guild_id":"private-guild","channel_id":"private-channel","message_ids":["private-message"],"content":"private-text","at":"private-timestamp"}]`,
		`[{"operation":"delete","message_ids":["private-message"],"at":123}]`,
		`[{"operation":"delete","message_ids":["private-message"],"at":"2026-99-99T00:00:00Z"}]`,
	} {
		t.Run(body[len(body)-10:], func(t *testing.T) {
			g, _ := reconciliationTestGateway(t)
			path := filepath.Join(g.svc.DataRoot, messageReconciliationFile)
			if err := fsutil.WriteAtomic(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			g.log = slog.New(slog.NewTextHandler(&logs, nil))
			for pass := 0; pass < 3; pass++ {
				if err := g.drainMessageReconciliation(); !errors.Is(err, errReconciliationBlocked) || g.reconciliationBlocked.Load() != 1 {
					t.Fatal("malformed timestamp was retried instead of quarantined and counted")
				}
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("corrupt legacy source was not moved")
			}
			if data, err := os.ReadFile(path + ".unresolved.corrupt"); err != nil || string(data) != body {
				t.Fatal("quarantine did not preserve bytes")
			}
			for _, secret := range []string{"private-guild", "private-channel", "private-message", "private-text", "private-timestamp", "2026-99-99"} {
				if strings.Contains(logs.String(), secret) {
					t.Fatal("legacy field value reached logs")
				}
			}
		})
	}
}
