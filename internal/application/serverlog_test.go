package application_test

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/config"
	"masamong/internal/errclass"
)

// logGateway is a server with a #logs and a #general channel that records the cards posted.
type logGateway struct {
	fakeGateway
	mu    sync.Mutex
	cards map[string][]action.Card // channel → cards
}

func (g *logGateway) Channels(guildID string) []application.ChannelInfo {
	return []application.ChannelInfo{{ID: "9000001", GuildID: guildID, Name: "general", Type: "text"}, {ID: "9000002", GuildID: guildID, Name: "logs", Type: "text"}}
}

func (g *logGateway) PostChannel(_ context.Context, channelID, _, _ string, card *action.Card) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cards == nil {
		g.cards = map[string][]action.Card{}
	}
	g.cards[channelID] = append(g.cards[channelID], *card)
	return "posted", nil
}

// posted waits for n cards in channel (the server log posts in the background).
func (g *logGateway) posted(t *testing.T, channel string, n int) []action.Card {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		g.mu.Lock()
		got := append([]action.Card(nil), g.cards[channel]...)
		g.mu.Unlock()
		if len(got) >= n || time.Now().After(deadline) {
			if len(got) != n {
				t.Fatalf("%s: %d cards, want %d: %+v", channel, len(got), n, got)
			}
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func logSvc(t *testing.T) (*application.Service, *logGateway) {
	t.Helper()
	svc := setupSvc(t)
	applyDoc(t, svc, func(d *config.Document) {
		d.Discord.Enabled, d.I18n.DefaultLocale, d.Access.Servers = true, "ko", []string{"g"}
	})
	gw := &logGateway{fakeGateway: fakeGateway{bits: 1<<10 | 1<<11 | 1<<14 | 1<<16}}
	svc.Discord = gw
	setLog(t, svc, config.GuildLog{Channel: config.GuildLogAuto})
	return svc, gw
}

// msgID is a Discord ID made at t.
func msgID(t time.Time, n int) string {
	return strconv.FormatUint(uint64(t.UnixMilli()-1420070400000)<<22|uint64(n), 10)
}

func setLog(t *testing.T, svc *application.Service, l config.GuildLog) {
	t.Helper()
	applyDoc(t, svc, func(d *config.Document) {
		if d.Guilds == nil {
			d.Guilds = map[string]config.GuildSettings{}
		}
		d.Guilds["g"] = config.GuildSettings{Log: l}
	})
}

func fieldOf(c action.Card, name string) string {
	for _, f := range c.Fields {
		if f.Name == name {
			return f.Value
		}
	}
	return ""
}

func TestServerLogPostsDeletesWithTheirText(t *testing.T) {
	svc, gw := logSvc(t)
	written := time.Date(2026, 10, 21, 3, 4, 5, 0, time.UTC)
	svc.LogDeletes("g", "9000001", []application.DeletedMessage{{ID: msgID(written, 1), AuthorID: "111", Text: "내일 3시에 보자", Files: []string{"a.png"}}})
	c := gw.posted(t, "9000002", 1)[0]
	if !strings.Contains(c.Title, "삭제") || fieldOf(c, "작성자") != "<@111>" || fieldOf(c, "채널") != "<#9000001>" || c.Text != "내일 3시에 보자\n📎 첨부: a.png" {
		t.Fatalf("card %+v", c)
	}
	if got := fieldOf(c, "작성 시각"); got != "<t:"+strconv.FormatInt(written.Unix(), 10)+":f>" {
		t.Fatalf("written %q", got)
	}

	// a bot's message, the log channel's own, a DM: nothing
	svc.LogDeletes("g", "9000001", []application.DeletedMessage{{ID: msgID(written, 2), AuthorID: "222", AuthorBot: true}})
	svc.LogDeletes("g", "9000002", []application.DeletedMessage{{ID: msgID(written, 3), AuthorID: "111"}})
	svc.LogDeletes("", "dm", []application.DeletedMessage{{ID: msgID(written, 4), AuthorID: "111"}})
	settle()
	gw.posted(t, "9000002", 1)

	// many at once are one entry, with the authors named once and the unknown ones counted
	svc.LogDeletes("g", "9000001", []application.DeletedMessage{{ID: msgID(written, 5), AuthorID: "111", Text: "하나\n둘"}, {ID: msgID(written.Add(time.Hour), 6), AuthorID: "111", Text: "셋"},
		{ID: msgID(written, 7), AuthorID: "333", Text: "넷"}, {ID: msgID(written, 8)}})
	bulk := gw.posted(t, "9000002", 2)[1]
	if !strings.Contains(bulk.Title, "4") || fieldOf(bulk, "작성자") != "<@111>, <@333>\n작성자를 알 수 없는 메시지 1개" || !strings.Contains(fieldOf(bulk, "작성 시각"), " – ") {
		t.Fatalf("bulk %+v", bulk)
	}
	if want := "<@111>: 하나 둘\n<@111>: 셋\n<@333>: 넷\n알 수 없음: (마사몽이 내용을 보지 못한 메시지)"; bulk.Text != want {
		t.Fatalf("bulk text %q", bulk.Text)
	}

	// with the text turned off, only who, where and when
	f := false
	setLog(t, svc, config.GuildLog{Channel: config.GuildLogAuto, DeletedText: &f})
	svc.LogDeletes("g", "9000001", []application.DeletedMessage{{ID: msgID(written, 9), AuthorID: "111", Text: "비밀"}})
	if c := gw.posted(t, "9000002", 3)[2]; c.Text != "" || strings.Contains(c.Footer, "기억") {
		t.Fatalf("text off %+v", c)
	}
}

func TestServerLogFindsTheStoredMessageAndSkipsItsOwnReplies(t *testing.T) {
	svc, gw := logSvc(t)
	if _, err := svc.MemoryEngine(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for _, q := range []string{
		`INSERT INTO memory_observations(observation_id,platform,collection,source_room,room,native_message_id,actor_label,actor_account,content,parsed_time,record_kind,imported_at)
		 VALUES('o1','discord','c','r','r','5001','철수','444','hi','` + now + `','human_text','` + now + `')`,
		`INSERT INTO reply_messages(message_id,turn_id,guild_id,channel_id,at) VALUES('5002','t','g','900','` + now + `')`,
	} {
		if _, err := svc.Store.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	svc.LogDeletes("g", "9000001", []application.DeletedMessage{{ID: "5002"}})
	svc.LogDeletes("g", "9000001", []application.DeletedMessage{{ID: "5001"}})
	c := gw.posted(t, "9000002", 1)[0]
	if fieldOf(c, "작성자") != "<@444>" || c.Text != "hi" {
		t.Fatalf("card %+v", c)
	}
}

func TestServerLogFollowsItsSettings(t *testing.T) {
	svc, gw := logSvc(t)
	f := false
	setLog(t, svc, config.GuildLog{Channel: config.GuildLogAuto, Deletes: &f})
	svc.LogDeletes("g", "9000001", []application.DeletedMessage{{ID: "7001", AuthorID: "111"}})
	svc.LogProblem("g", "9000001", "reply_failed", "", errclass.Timeout)
	gw.posted(t, "9000002", 1)

	setLog(t, svc, config.GuildLog{Channel: "9000001"})
	svc.LogDeletes("g", "9000001", []application.DeletedMessage{{ID: "7002", AuthorID: "111"}}) // the log channel's own
	svc.LogDeletes("g", "9000002", []application.DeletedMessage{{ID: "7003", AuthorID: "111"}})
	gw.posted(t, "9000001", 1)

	setLog(t, svc, config.GuildLog{Channel: "off"})
	svc.LogDeletes("g", "9000001", []application.DeletedMessage{{ID: "7004", AuthorID: "111"}})
	settle()
	gw.posted(t, "9000001", 1)
	gw.posted(t, "9000002", 1)

	shown := exec(svc, "config.show", nil).Output["yaml"].(string)
	bad := regexp.MustCompile(`channel: ["']?off["']?`).ReplaceAllString(shown, "channel: logs")
	if v := exec(svc, "config.validate", map[string]any{"yaml": bad}); bad == shown || v.OK || !strings.Contains(v.Error.Message, "log.channel") {
		t.Fatalf("a channel name was taken as the log channel: %+v", v)
	}
}

func TestServerLogPostsAProblemOncePerWhile(t *testing.T) {
	svc, gw := logSvc(t)
	svc.LogProblem("g", "9000001", "tool_failed", "search.web", errclass.RateLimited)
	svc.LogProblem("g", "9000001", "tool_failed", "search.web", errclass.RateLimited)
	svc.LogProblem("g", "9000001", "reply_failed", "", errclass.Timeout)
	cards := gw.posted(t, "9000002", 2)
	if cards[0].Tone != "warn" || !strings.Contains(cards[0].Title, "일부 오류") || fieldOf(cards[0], "원인") != "요청이 너무 많음" || strings.Contains(cards[0].Title, "search.web") {
		t.Fatalf("tool problem %+v", cards[0])
	}
	if !strings.Contains(cards[1].Title, "답을 하지 못했어요") || fieldOf(cards[1], "원인") != "응답 시간 초과" {
		t.Fatalf("reply problem %+v", cards[1])
	}
}

func TestServerLogTestAndListView(t *testing.T) {
	svc, gw := logSvc(t)
	r := exec(svc, "serverlog.test", map[string]any{"guild_id": "g"})
	if !r.OK || r.Output["channel_id"] != "9000002" || r.Output["source"] != "auto" {
		t.Fatalf("%+v %+v", r.Output, r.Error)
	}
	gw.posted(t, "9000002", 1)
	setLog(t, svc, config.GuildLog{Channel: "off"})
	if r := exec(svc, "serverlog.test", map[string]any{"guild_id": "g"}); r.OK || r.Error.Args["reason"] != "off" {
		t.Fatalf("off: %+v", r)
	}
	// unset is off: a server's #logs channel gets nothing until its managers turn the log on
	setLog(t, svc, config.GuildLog{})
	svc.LogDeletes("g", "9000001", []application.DeletedMessage{{ID: "8001", AuthorID: "111"}})
	if r := exec(svc, "serverlog.test", map[string]any{"guild_id": "g"}); r.OK || r.Error.Args["reason"] != "off" {
		t.Fatalf("unset: %+v", r)
	}
	settle()
	gw.posted(t, "9000002", 1)
}
