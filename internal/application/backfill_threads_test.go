package application_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"masamong/internal/application"
	"masamong/internal/config"
	"masamong/internal/memory/engine"
)

// channelHistoryGateway records which channels' history was asked for.
type channelHistoryGateway struct {
	fakeGateway
	mu    sync.Mutex
	asked []string
}

func (h *channelHistoryGateway) History(_ context.Context, channelID, _ string, _ int) ([]application.HistoryMessage, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.asked = append(h.asked, channelID)
	return nil, nil
}

// A catch-up never reads a thread under a channel with capture off: not one named by the console (a thread
// closed long ago, or a private thread the application only knows by its stored parent, with a message
// stored before the parent was turned off), and not one whose own entry says capture: true. A thread under a channel that stores is still read.
func TestBackfillSkipsThreadsOfACaptureOffParent(t *testing.T) {
	svc := setupSvc(t)
	applyDoc(t, svc, func(d *config.Document) {
		d.Memory.Capture = true
		d.Discord.MessageContent = true
		d.Guilds = map[string]config.GuildSettings{"g": {Channels: map[string]config.ChannelPolicy{
			"off": {Respond: "mention", Capture: false},
			"on":  {Respond: "mention", Capture: true},
			"kid": {Respond: "mention", Capture: true}, // a thread of "off" with an entry of its own
		}}}
	})
	closed := time.Now().Add(-31 * 24 * time.Hour).UTC().Format(time.RFC3339)
	for id, parent := range map[string]string{"old": "off", "kid": "off", "oldon": "on"} {
		if _, err := svc.Store.DB.Exec(`INSERT INTO channel_threads(thread_id,guild_id,parent_id,state,created_at,last_at,closed_at)
			VALUES(?,?,?,?,?,?,?)`, id, "g", parent, "closed", closed, closed, closed); err != nil {
			t.Fatal(err)
		}
	}
	eng, err := svc.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	for i, ch := range []string{"old", "kid", "oldon", "on"} {
		// stored before the parent was turned off: a catch-up continues from the last stored message
		at := time.Now().Add(-time.Hour + time.Duration(i)*time.Second)
		if _, err := eng.Capture(engine.LiveMessage{GuildID: "g", ChannelID: ch, MessageID: snowflake(at), AuthorID: "u",
			AuthorName: "member", Content: "a synthetic line in " + ch, At: at}); err != nil {
			t.Fatal(err)
		}
	}
	// a private thread has no thread row: the connector stored its parent when it first saw it
	if err := svc.NoteThreadParent(context.Background(), "g", "priv", "off"); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-time.Hour + 10*time.Second)
	if _, err := eng.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "priv", MessageID: snowflake(at), AuthorID: "u",
		AuthorName: "member", Content: "a synthetic line in priv", At: at}); err != nil {
		t.Fatal(err)
	}
	gw := &channelHistoryGateway{}
	svc.SetDiscord(gw)
	for _, ch := range []string{"old", "kid", "priv"} {
		if r := exec(svc, "memory.backfill", map[string]any{"guild_id": "g", "channel_id": ch}); r.OK {
			t.Errorf("a catch-up of thread %s under a channel with capture off was not refused: %+v", ch, r.Output)
		}
	}
	if r := exec(svc, "memory.backfill", nil); !r.OK {
		t.Fatal(r.Error)
	}
	if r := exec(svc, "memory.backfill", map[string]any{"guild_id": "g", "channel_id": "oldon"}); !r.OK {
		t.Fatalf("a thread of a channel that stores is caught up: %+v", r.Error)
	}
	gw.mu.Lock()
	defer gw.mu.Unlock()
	seen := map[string]bool{}
	for _, ch := range gw.asked {
		seen[ch] = true
	}
	if seen["old"] || seen["kid"] || seen["priv"] || seen["off"] {
		t.Fatalf("history read for a room under capture off: %v", gw.asked)
	}
	if !seen["on"] || !seen["oldon"] {
		t.Fatalf("history of the storing rooms was not read: %v", gw.asked)
	}
}
