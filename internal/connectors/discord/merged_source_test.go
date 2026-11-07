package discord

import (
	"context"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"masamong/internal/application"
)

func dmEdit(id, words string, before *discordgo.Message, files ...*discordgo.MessageAttachment) *discordgo.MessageUpdate {
	at := time.Now().Add(time.Minute)
	return &discordgo.MessageUpdate{Message: &discordgo.Message{ID: id, ChannelID: "dm-fixture", Content: words, EditedTimestamp: &at, Attachments: files,
		Author: &discordgo.User{ID: "11111"}}, BeforeUpdate: before}
}

func dmCount(t *testing.T, g *Gateway, query string, args ...any) int {
	t.Helper()
	var n int
	if err := g.svc.Store.DB.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func drainDM(t *testing.T, g *Gateway) {
	t.Helper()
	waitDMWork(g)
	if err := g.svc.DrainRedactions(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// Two messages within the merge window are one turn under the last one's ID. Changing the first has to reach the
// turn, its reply and the turn's record, by the way a real burst gets there.
func TestChangingTheFirstOfAMergedBurstReachesTheTurn(t *testing.T) {
	for _, change := range []string{"delete", "edit"} {
		t.Run(change, func(t *testing.T) {
			g, _ := dmGateway(t, true)
			sendDM(g, dmMessage("dm-earlier", "Synthetic earlier message"))
			first, second := dmMessage("dm-first", "Synthetic first fragment"), dmMessage("dm-second", "Synthetic second fragment")
			g.onMessage(g.sess, first)
			waitDMWork(g)
			g.onMessage(g.sess, second)
			waitDMWork(g)
			flushDM(g, second)
			if n := dmCount(t, g, `SELECT COUNT(*) FROM chat_turns WHERE native_id='dm-second' AND content LIKE '%first fragment%'`); n != 1 {
				t.Fatalf("the two messages were not merged into one turn: %d rows: this proves nothing", n)
			}
			if n := dmCount(t, g, `SELECT COUNT(*) FROM turn_log WHERE body LIKE '%first fragment%'`); n != 1 {
				t.Fatalf("the turn's record does not hold the words: %d: this proves nothing", n)
			}
			if change == "delete" {
				g.onMessageDelete(g.sess, &discordgo.MessageDelete{Message: &discordgo.Message{ID: "dm-first", ChannelID: "dm-fixture"}})
			} else {
				g.onMessageUpdate(g.sess, dmEdit("dm-first", "Synthetic corrected words", nil))
			}
			drainDM(t, g)
			if n := dmCount(t, g, `SELECT COUNT(*) FROM chat_turns WHERE native_id='dm-second'`); n != 0 {
				t.Fatalf("%d rows of the merged turn stayed", n)
			}
			for _, query := range []string{`SELECT COUNT(*) FROM chat_turns WHERE content LIKE '%first fragment%'`, `SELECT COUNT(*) FROM turn_log WHERE body LIKE '%first fragment%'`} {
				if n := dmCount(t, g, query); n != 0 {
					t.Fatalf("%s: the words stayed in %d rows", query, n)
				}
			}
			if n := dmCount(t, g, `SELECT COUNT(*) FROM turn_log WHERE json_extract(body,'$.message_id')='dm-second' AND json_extract(body,'$.answer')<>''`); n != 0 {
				t.Fatalf("the turn's record keeps its answer: %d", n)
			}
			if n := dmCount(t, g, `SELECT COUNT(*) FROM chat_turns WHERE native_id='dm-earlier' AND role='assistant'`); n != 1 {
				t.Fatalf("the reply before the burst: %d rows", n)
			}
		})
	}
}

// A picture sent right after a call without one comes with the call's words: those are the first message's too.
func TestAPictureContinuingACallHoldsTheCallsMessage(t *testing.T) {
	first, second := dmMessage("a", "x"), dmMessage("b", "y")
	if got := fragmentIDs([]*discordgo.MessageCreate{first, second}, []string{"call", "a", "b", ""}); len(got) != 2 || got[0] != "a" || got[1] != "call" {
		t.Fatalf("the messages a turn holds apart from its own: %v", got)
	}
	g := &Gateway{svc: svcForTest(t)}
	bot := "bot"
	g.botID.Store(&bot)
	now := time.Now()
	ask := lookbackMsg("105", "me", "<@bot> 이 사진 번역해줘")
	ask.Mentions = []*discordgo.User{{ID: bot}}
	g.pictures.note(ask, now)
	if got := g.heldCall([]*discordgo.MessageCreate{ask}); len(got.images) != 0 {
		t.Fatalf("no picture yet: %+v", got)
	}
	pic := lookbackMsg("106", "me", "", "a3")
	g.pictures.note(pic, now)
	if !g.notePictureFollow(pic) {
		t.Fatal("the picture should continue the call")
	}
	got := g.heldCall([]*discordgo.MessageCreate{pic})
	if got.continues == "" || len(got.continuedFrom) != 1 || got.continuedFrom[0] != "105" {
		t.Fatalf("the continued call's message: %+v", got)
	}
}

// An edit that left the words and the files changes nothing built on the message. Files changed, or not known to be
// the same, and it is applied in full: a reply may be built on a picture the edit took away.
func TestAnEditKeepingWordsAndFiles(t *testing.T) {
	const words = "Synthetic words of a message"
	file := func(id string) *discordgo.MessageAttachment {
		return &discordgo.MessageAttachment{ID: id, URL: "https://cdn.example/" + id, Filename: id + ".png"}
	}
	for _, c := range []struct {
		name   string
		update *discordgo.MessageUpdate
		kept   bool
	}{
		{"same words and no files", dmEdit("dm-one", words, &discordgo.Message{Content: words}), true},
		{"same words and files", dmEdit("dm-one", words, &discordgo.Message{Content: words, Attachments: []*discordgo.MessageAttachment{file("a"), file("b")}}, file("b"), file("a")), true},
		{"a file taken away", dmEdit("dm-one", words, &discordgo.Message{Content: words, Attachments: []*discordgo.MessageAttachment{file("a"), file("b")}}, file("a")), false},
		{"a file added", dmEdit("dm-one", words, &discordgo.Message{Content: words}, file("a")), false},
		{"the message not in the state cache", dmEdit("dm-one", words, nil), false},
		{"other words", dmEdit("dm-one", "Synthetic other words", &discordgo.Message{Content: words}), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			g, _ := dmGateway(t, true)
			sendDM(g, dmMessage("dm-one", words))
			if n := dmCount(t, g, `SELECT COUNT(*) FROM chat_turns WHERE native_id='dm-one' AND role='assistant'`); n != 1 {
				t.Fatalf("no reply to test with: %d", n)
			}
			g.onMessageUpdate(g.sess, c.update)
			drainDM(t, g)
			replies := dmCount(t, g, `SELECT COUNT(*) FROM chat_turns WHERE native_id='dm-one' AND role='assistant'`)
			tombstones := dmCount(t, g, `SELECT COUNT(*) FROM privacy_tombstones WHERE source_id='dm-one'`)
			if c.kept && (replies != 1 || tombstones != 0) {
				t.Fatalf("an edit that changed nothing: %d replies, %d tombstones", replies, tombstones)
			}
			if !c.kept && (replies != 0 || tombstones == 0) {
				t.Fatalf("an edit applied in part: %d replies, %d tombstones", replies, tombstones)
			}
		})
	}
}

// A message with a picture, by the way a real one gets here: the gateway takes the mark before it picks the pictures, so
// the reply on an earlier message's edited words is kept. That the picture cannot be fetched changes nothing about it.
func TestATurnWithAPictureAfterASettledEdit(t *testing.T) {
	g, _ := dmGateway(t, true)
	sendDM(g, dmMessage("dm-secret", "Synthetic old words"))
	g.onMessageUpdate(g.sess, dmEdit("dm-secret", "Synthetic corrected words", nil))
	drainDM(t, g)
	if n := dmCount(t, g, `SELECT COUNT(*) FROM privacy_settled WHERE source_id='dm-secret'`); n != 1 {
		t.Fatalf("the edit did not settle: %d: this proves nothing", n)
	}
	m := dmMessage("dm-picture", "Synthetic look at this")
	m.Attachments = []*discordgo.MessageAttachment{{ID: "a1", URL: "http://127.0.0.1:1/cat.png", ContentType: "image/png", Filename: "cat.png"}}
	sendDM(g, m)
	if n := dmCount(t, g, `SELECT COUNT(*) FROM chat_turns WHERE native_id='dm-picture' AND role='assistant'`); n != 1 {
		t.Fatalf("the reply of a turn with a picture was refused after a settled edit: %d rows", n)
	}
}

// A command that goes on into a turn: the gateway takes the mark before the command runs, and the turn's answer stays.
func TestACommandTurnAfterASettledEdit(t *testing.T) {
	g, _ := dmGateway(t, true)
	sendDM(g, dmMessage("dm-secret", "Synthetic old words"))
	g.onMessageUpdate(g.sess, dmEdit("dm-secret", "Synthetic corrected words", nil))
	drainDM(t, g)
	if n := dmCount(t, g, `SELECT COUNT(*) FROM privacy_settled WHERE source_id='dm-secret'`); n != 1 {
		t.Fatalf("the edit did not settle: %d: this proves nothing", n)
	}
	cmd, ok := ByName("weather")
	if !ok {
		t.Fatal("weather command missing")
	}
	g.runCommand(g.sess, dmMessage("weather-dm", "!weather Synthetic"), cmd, map[string]string{"region": "Synthetic"})
	waitDMWork(g)
	if n := dmCount(t, g, `SELECT COUNT(*) FROM turn_log WHERE json_extract(body,'$.message_id')='weather-dm' AND json_extract(body,'$.answer')<>''`); n != 1 {
		t.Fatalf("the answer of a command turn was blanked after a settled edit: %d kept", n)
	}
}

// The same by the slash command's way there.
func TestASlashCommandTurnAfterASettledEdit(t *testing.T) {
	g, _ := dmGateway(t, true)
	g.notices = map[string]time.Time{}
	ctx := context.Background()
	if _, err := g.svc.ChatTurn(ctx, application.ChatInput{GuildID: "g1", ChannelID: "c1", MessageID: "secret", Message: "Synthetic old words", Speaker: "u", SpeakerID: "11111"}); err != nil {
		t.Fatal(err)
	}
	if err := g.svc.MessageEdited("g1", "c1", "secret", "Synthetic corrected words", time.Now()); err != nil {
		t.Fatal(err)
	}
	drainDM(t, g)
	if n := dmCount(t, g, `SELECT COUNT(*) FROM privacy_settled WHERE source_id='secret'`); n != 1 {
		t.Fatalf("the edit did not settle: %d: this proves nothing", n)
	}
	g.handleCommand(g.sess, &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{ID: "i1", AppID: "app", Token: "tok", Type: discordgo.InteractionApplicationCommand,
		ChannelID: "c1", GuildID: "g1", Member: &discordgo.Member{User: &discordgo.User{ID: "11111", Username: "u"}},
		Data: discordgo.ApplicationCommandInteractionData{Name: "weather", Options: []*discordgo.ApplicationCommandInteractionDataOption{{Name: "region", Type: discordgo.ApplicationCommandOptionString, Value: "Synthetic"}}}}})
	if !g.Wait(10 * time.Second) {
		t.Fatal("command work did not finish")
	}
	if n := dmCount(t, g, `SELECT COUNT(*) FROM turn_log WHERE json_extract(body,'$.message_id')='dc:i1' AND json_extract(body,'$.answer')<>''`); n != 1 {
		t.Fatalf("the answer of a slash command turn was blanked after a settled edit: %d kept", n)
	}
}

// An edit that kept the words leaves no tombstone. An older edit still in the queue (a restart replays what a failed
// removal left behind) is older than it all the same, and a newer one is not.
func TestAnOlderEditInTheQueueIsNotCurrentAfterAnEditThatKeptTheWords(t *testing.T) {
	g, _ := dmGateway(t, true)
	at := time.Now().Add(time.Hour)
	sendDM(g, dmMessage("dm-kept", "Synthetic words"))
	waitDMWork(g)
	kept, err := g.svc.MessageKeptItsWords("", "dm-fixture", "dm-kept", "Synthetic words", at)
	if err != nil || !kept {
		t.Fatalf("kept %v, %v", kept, err)
	}
	event := func(when time.Time) messageReconciliation {
		return messageReconciliation{Operation: "edit", ChannelID: "dm-fixture", MessageIDs: []string{"dm-kept"}, Content: "words taken out later", At: when}
	}
	if current, err := g.reconciliationEditCurrent(event(at.Add(-time.Minute))); err != nil || current {
		t.Fatalf("the older edit: current %v, %v", current, err)
	}
	if current, err := g.reconciliationEditCurrent(event(at.Add(time.Minute))); err != nil || !current {
		t.Fatalf("a newer edit: current %v, %v", current, err)
	}
}
