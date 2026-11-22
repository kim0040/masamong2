package discord

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

// A held turn fires on a timer's own goroutine: a panic while it starts is recovered and reported there,
// instead of taking the daemon down.
func TestMergedTurnPanicIsRecoveredInFlush(t *testing.T) {
	var logs bytes.Buffer
	mg := &merger{log: slog.New(slog.NewTextHandler(&logs, nil))}
	fired := 0
	mg.fire = func([]*discordgo.MessageCreate) { fired++; panic("synthetic failure") }
	m := &discordgo.MessageCreate{Message: &discordgo.Message{ID: "m1", ChannelID: "c", Author: &discordgo.User{ID: "u"}, Content: "hello"}}
	mg.add(m, false)
	mg.mu.Lock()
	p := mg.pending[mergeKey(m)]
	p.timer.Stop()
	generation := p.generation
	mg.mu.Unlock()
	escaped := func() (v any) {
		defer func() { v = recover() }()
		mg.flush(mergeKey(m), p, generation) // what the time.AfterFunc goroutine runs
		return nil
	}()
	if escaped != nil {
		t.Fatalf("a panic escaped flush: %v", escaped)
	}
	if fired != 1 || !strings.Contains(logs.String(), "panic recovered") || mg.waiting(m) {
		t.Fatalf("fired=%d waiting=%v logs=%q", fired, mg.waiting(m), logs.String())
	}
}
