package application_test

import (
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/config"
	"masamong/internal/memory/engine"
)

func TestChannelSummaryCachesAndFoldsInNewMessages(t *testing.T) {
	var calls atomic.Int32
	url := chatServer(t, "## 핵심 3줄\n- 캠핑 간다", "stop", &calls)
	svc := routedSvc(t, url, url)
	applyDoc(t, svc, func(d *config.Document) {
		d.Memory.Capture = true
		d.Models["summary"] = d.Models["response"]
		d.Guilds = map[string]config.GuildSettings{"g": {Channels: map[string]config.ChannelPolicy{"c": {Respond: "mention", Capture: true}}}}
	})
	eng, _ := svc.MemoryEngine()
	base := time.Now().Add(-time.Hour)
	add := func(from, n int) {
		for i := from; i < from+n; i++ {
			_, _ = eng.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", MessageID: strconv.Itoa(1000 + i), AuthorID: "u", AuthorName: "철수",
				Content: "캠핑 얘기 " + strconv.Itoa(i), At: base.Add(time.Duration(i) * time.Second)})
		}
	}
	in := map[string]any{"guild_id": "g", "channel_id": "c"}
	if r := exec(svc, "summaries.channel", in); !r.OK || r.Output["status"] != "empty" {
		t.Fatalf("no conversation yet: %+v", r)
	}
	if r := exec(svc, "summaries.channel", map[string]any{"guild_id": "g", "channel_id": "c", "saved": true}); !r.OK || r.Output["status"] != "none" {
		t.Fatalf("nothing saved yet: %+v", r)
	}
	add(0, 10)
	r := exec(svc, "summaries.channel", in)
	if !r.OK || r.Output["incremental"] != false || !strings.Contains(r.Output["summary"].(string), "캠핑") {
		t.Fatalf("first summary: %+v", r)
	}
	// nothing new: the stored summary, no model call
	before := calls.Load()
	if r := exec(svc, "summaries.channel", in); r.Output["cached"] != true || calls.Load() != before {
		t.Fatalf("cached: %+v", r)
	}
	if r := exec(svc, "summaries.channel", map[string]any{"guild_id": "g", "channel_id": "c", "saved": true}); r.Output["status"] != "ok" || calls.Load() != before {
		t.Fatalf("saved summary without a model call: %+v", r)
	}
	add(10, 3)
	if r := exec(svc, "summaries.channel", in); r.Output["incremental"] != true || r.Output["read"] != 3 {
		t.Fatalf("incremental: %+v", r)
	}
	// from Discord the channel is where the command was typed
	r = as(svc, "summaries.channel", map[string]any{"refresh": true}, &action.Invoker{Via: "command", GuildID: "g", ChannelID: "c", UserID: "u"})
	if !r.OK || r.Output["incremental"] != false {
		t.Fatalf("refresh from Discord: %+v", r)
	}
	if _, ok := r.Output["card"].(action.Card); !ok {
		t.Fatal("a card for Discord")
	}
}

// An edit or deletion before the last message leaves the anchor where it was; the saved summary still
// goes, so the next one is made from the messages as they now are.
func TestChannelSummaryDropsWhatAnEditOrDeletionChanged(t *testing.T) {
	var calls atomic.Int32
	url := chatServer(t, "## 핵심 3줄\n- 금요일 8시 모임", "stop", &calls)
	svc := routedSvc(t, url, url)
	applyDoc(t, svc, func(d *config.Document) {
		d.Memory.Capture = true
		d.Models["summary"] = d.Models["response"]
		d.Guilds = map[string]config.GuildSettings{"g": {Channels: map[string]config.ChannelPolicy{"c": {Respond: "mention", Capture: true}}}}
	})
	eng, _ := svc.MemoryEngine()
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 6; i++ {
		_, _ = eng.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", MessageID: strconv.Itoa(2000 + i), AuthorID: "u", AuthorName: "철수",
			Content: "모임 얘기 " + strconv.Itoa(i), At: base.Add(time.Duration(i) * time.Second)})
	}
	in := map[string]any{"guild_id": "g", "channel_id": "c"}
	saved := map[string]any{"guild_id": "g", "channel_id": "c", "saved": true}
	if r := exec(svc, "summaries.channel", in); !r.OK || r.Output["cached"] == true {
		t.Fatalf("first summary: %+v", r)
	}
	svc.MessageEdited("g", "c", "2002", "토요일 8시로 바꿈", time.Now())
	if r := exec(svc, "summaries.channel", saved); r.Output["status"] != "none" {
		t.Fatalf("an edit keeps the old summary: %+v", r)
	}
	before := calls.Load()
	if r := exec(svc, "summaries.channel", in); r.Output["cached"] == true || r.Output["incremental"] != false || calls.Load() == before {
		t.Fatalf("the summary after an edit is made again from the messages: %+v", r)
	}
	svc.MessagesDeleted("g", "c", []string{"2003"})
	if r := exec(svc, "summaries.channel", saved); r.Output["status"] != "none" {
		t.Fatalf("a deletion keeps the old summary: %+v", r)
	}
	// another channel's summary stays
	if r := exec(svc, "summaries.channel", in); !r.OK {
		t.Fatal(r)
	}
	svc.MessagesDeleted("g", "other", []string{"no-such"})
	if r := exec(svc, "summaries.channel", saved); r.Output["status"] != "ok" {
		t.Fatalf("an unrelated deletion dropped the summary: %+v", r)
	}
}
