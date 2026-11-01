package application_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"masamong/internal/application"
	"masamong/internal/config"
	"masamong/internal/memory/engine"
)

// historyGateway serves a channel history of ascending snowflakes and records the pages asked for.
type historyGateway struct {
	fakeGateway
	msgs  []application.HistoryMessage
	calls []string
}

func (h *historyGateway) History(_ context.Context, _ string, after string, limit int) ([]application.HistoryMessage, error) {
	h.calls = append(h.calls, after)
	a, _ := strconv.ParseUint(after, 10, 64)
	// like Discord: the `limit` messages right after `after`, newest first
	var page []application.HistoryMessage
	for _, m := range h.msgs {
		if id, _ := strconv.ParseUint(m.ID, 10, 64); id > a && len(page) < limit {
			page = append(page, m)
		}
	}
	out := make([]application.HistoryMessage, 0, len(page))
	for i := len(page) - 1; i >= 0; i-- {
		out = append(out, page[i])
	}
	return out, nil
}

func snowflake(t time.Time) string {
	return strconv.FormatUint(uint64(t.UnixMilli()-1420070400000)<<22, 10)
}

func TestBackfillCatchesUpFromTheLastStoredMessage(t *testing.T) {
	svc := setupSvc(t)
	applyDoc(t, svc, func(d *config.Document) {
		d.Memory.Capture = true
		d.Discord.MessageContent = true
		d.Guilds = map[string]config.GuildSettings{"g": {Channels: map[string]config.ChannelPolicy{
			"c": {Respond: "mention", Capture: true}, "never": {Respond: "mention", Capture: true}, "off": {Respond: "mention"}}}}
	})
	now := time.Now()
	gw := &historyGateway{}
	for i := 0; i < 150; i++ {
		at := now.Add(-time.Duration(150-i) * time.Minute)
		m := application.HistoryMessage{ID: snowflake(at), AuthorID: "u", AuthorName: "철수", Content: "놓친 말 " + strconv.Itoa(i), At: at}
		if i%50 == 0 {
			m.Bot, m.Content = true, "봇의 말"
		}
		gw.msgs = append(gw.msgs, m)
	}
	svc.Discord = gw
	eng, err := svc.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	// the bot was running until message 20
	for _, m := range gw.msgs[1:21] {
		if _, err := eng.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", MessageID: m.ID, AuthorID: m.AuthorID, AuthorName: m.AuthorName, Content: m.Content, At: m.At}); err != nil {
			t.Fatal(err)
		}
	}
	res := exec(svc, "memory.backfill", nil)
	if !res.OK {
		t.Fatalf("%+v", res.Error)
	}
	rep := asMap(t, res.Output)["report"].(map[string]any)
	chans := rep["channels"].([]any)
	if len(chans) != 2 {
		t.Fatalf("only capturing channels are read: %v", chans)
	}
	byID := map[string]map[string]any{}
	for _, c := range chans {
		m := c.(map[string]any)
		byID[m["channel_id"].(string)] = m
	}
	if byID["never"]["skipped"] != "never_captured" {
		t.Fatalf("a channel never captured must not be read: %v", byID["never"])
	}
	// 129 messages after the last stored one, 2 of them the bot's
	if got := byID["c"]["stored"].(float64); got != 127 {
		t.Fatalf("stored %v, want 127: %v", got, byID["c"])
	}
	if len(gw.calls) < 2 || gw.calls[0] != gw.msgs[20].ID {
		t.Fatalf("catch-up must continue from the last stored message: %v", gw.calls)
	}
	msgs, _ := eng.Store.RecentMessages(engine.RoomKey("g", "c"), 1000)
	if len(msgs) != 147 {
		t.Fatalf("room holds %d messages", len(msgs))
	}
	// a second run finds nothing new
	res = exec(svc, "memory.backfill", map[string]any{"guild_id": "g", "channel_id": "c"})
	if r := asMap(t, res.Output)["report"].(map[string]any); r["stored"].(float64) != 0 {
		t.Fatalf("second run stored again: %v", r)
	}
	st := asMap(t, exec(svc, "memory.backfill", map[string]any{"status": true}).Output)
	if st["days"].(float64) != 7 || st["last"] == nil {
		t.Fatalf("status: %v", st)
	}
	if r := exec(svc, "memory.backfill", map[string]any{"guild_id": "g", "channel_id": "off"}); r.OK {
		t.Fatal("a channel that does not capture must be refused")
	}
}

func TestBackfillStopsAtTheWindow(t *testing.T) {
	svc := setupSvc(t)
	applyDoc(t, svc, func(d *config.Document) {
		d.Memory.Capture, d.Memory.BackfillDays = true, 1
		d.Discord.MessageContent = true
		d.Guilds = map[string]config.GuildSettings{"g": {Channels: map[string]config.ChannelPolicy{"c": {Respond: "mention", Capture: true}}}}
	})
	now := time.Now()
	old := now.Add(-72 * time.Hour)
	gw := &historyGateway{msgs: []application.HistoryMessage{
		{ID: snowflake(old), AuthorID: "u", AuthorName: "a", Content: "사흘 전", At: old},
		{ID: snowflake(now.Add(-48 * time.Hour)), AuthorID: "u", AuthorName: "a", Content: "이틀 전", At: now.Add(-48 * time.Hour)},
		{ID: snowflake(now.Add(-time.Hour)), AuthorID: "u", AuthorName: "a", Content: "한 시간 전", At: now.Add(-time.Hour)},
	}}
	svc.Discord = gw
	eng, _ := svc.MemoryEngine()
	m := gw.msgs[0]
	_, _ = eng.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", MessageID: m.ID, AuthorID: "u", AuthorName: "a", Content: m.Content, At: m.At})
	res := exec(svc, "memory.backfill", nil)
	rep := asMap(t, res.Output)["report"].(map[string]any)
	if rep["stored"].(float64) != 1 {
		t.Fatalf("only the last day is read back: %v", rep)
	}
}

// Backfill stores what live capture would have: a blocked person's messages are still part of the
// conversation (blocking stops replies); a person who withdrew memory consent is left out.
func TestBackfillFollowsMemoryConsent(t *testing.T) {
	svc := setupSvc(t)
	applyDoc(t, svc, func(d *config.Document) {
		d.Memory.Capture = true
		d.Discord.MessageContent = true
		d.Access.BlockedUsers = []string{"20002"}
		d.Guilds = map[string]config.GuildSettings{"g": {Channels: map[string]config.ChannelPolicy{"c": {Respond: "mention", Capture: true}}}}
	})
	_ = asUser(svc, "30003", "privacy.consent", map[string]any{"scope": "memory", "decision": "withdraw"})
	now := time.Now()
	at := func(h int) time.Time { return now.Add(-time.Duration(h) * time.Hour) }
	gw := &historyGateway{msgs: []application.HistoryMessage{
		{ID: snowflake(at(5)), AuthorID: "10001", AuthorName: "a", Content: "처음", At: at(5)},
		{ID: snowflake(at(4)), AuthorID: "10001", AuthorName: "a", Content: "남는 말", At: at(4)},
		{ID: snowflake(at(3)), AuthorID: "20002", AuthorName: "b", Content: "차단된 사람", At: at(3)},
		{ID: snowflake(at(2)), AuthorID: "30003", AuthorName: "c", Content: "철회한 사람", At: at(2)},
	}}
	svc.Discord = gw
	eng, _ := svc.MemoryEngine()
	m := gw.msgs[0]
	_, _ = eng.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", MessageID: m.ID, AuthorID: m.AuthorID, AuthorName: "a", Content: m.Content, At: m.At})
	res := exec(svc, "memory.backfill", nil)
	if rep := asMap(t, res.Output)["report"].(map[string]any); rep["stored"].(float64) != 2 {
		t.Fatalf("everyone but the person who withdrew: %v", rep)
	}
}
