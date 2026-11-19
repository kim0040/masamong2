package application_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"masamong/internal/application"
	"masamong/internal/chat"
	"masamong/internal/config"
	"masamong/internal/memory/engine"
)

// ownAnswerRoom drives one channel through the production sequence: a person asks the bot something, the
// bot answers (stored as chat turns and posted as one or more messages the live window sees), the room keeps
// talking for a while, and the same person calls the bot again. Times are simulated in the past; the rows the
// store stamps with the wall clock (chat_turns.created_at) are moved to the simulated time after each turn.
type ownAnswerRoom struct {
	t       *testing.T
	svc     *application.Service
	mu      sync.Mutex
	answer  string // what the response model says next
	prompts []string
	t0      time.Time
	seq     int
	plan    string // the planner's plan for the next turn ("" plans nothing)
	// noBotPosts leaves the bot's posted replies out of the live window
	noBotPosts bool
}

func newOwnAnswerRoom(t *testing.T) *ownAnswerRoom {
	r := &ownAnswerRoom{t: t, t0: time.Now().Add(-40 * time.Minute)}
	var plan, resp recordingServer
	planURL := plan.start(t, func(p string) string {
		switch {
		case strings.Contains(p, "about_bot"):
			return `{"to":"bot","tone":"neutral"}` // the name-call check hears it said to the bot
		case strings.Contains(p, "refers_to"):
			return `{"refers_to": []}`
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.plan != "" {
			return r.plan
		}
		return `{"memory_requirement":"none","tool_plan":[]}`
	})
	respURL := resp.start(t, func(p string) string {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.prompts = append(r.prompts, p)
		return r.answer
	})
	r.svc = routedSvc(t, planURL, respURL)
	applyDoc(t, r.svc, func(d *config.Document) {
		if d.Guilds == nil {
			d.Guilds = map[string]config.GuildSettings{}
		}
		d.Memory.Capture = true
		d.Guilds["g"] = config.GuildSettings{Channels: map[string]config.ChannelPolicy{"c": {Respond: "mention", Capture: true}}}
	})
	return r
}

func (r *ownAnswerRoom) at(d time.Duration) time.Time { return r.t0.Add(d) }

// human posts a person's message: the gateway captures it (memory_observations) and observes it (live window).
func (r *ownAnswerRoom) human(id, author, name, text string, at time.Time, mentionBot bool) {
	r.t.Helper()
	r.post(application.ObservedMessage{MessageID: id, AuthorID: author, AuthorName: name, Content: text, At: at}, mentionBot)
}

func (r *ownAnswerRoom) post(m application.ObservedMessage, mentionBot bool) {
	r.t.Helper()
	m.GuildID, m.ChannelID = "g", "c"
	eng, err := r.svc.MemoryEngine()
	if err != nil {
		r.t.Fatal(err)
	}
	if _, err := eng.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", MessageID: m.MessageID, AuthorID: m.AuthorID, AuthorName: m.AuthorName,
		Content: m.Content, At: m.At}); err != nil {
		r.t.Fatal(err)
	}
	if mentionBot {
		m.Mentions = []string{"bot"}
	}
	r.svc.Observe(m)
}

// ownCall is one person calling the bot and the bot's answer.
type ownCall struct {
	id, author, name, text string
	at, answeredAt         time.Time
	how                    string // "name" (the bot's name only), "mention", or "reply" (a Discord reply to replyTo)
	replyTo                string
	ping                   bool   // a reply that pings the bot: Discord lists it among the message's mentions
	answer                 string // its bubbles are separated by " / "; only the first replies to the call
	plan                   string // the planner's plan for this turn
}

// call runs one call: the message, the turn, and the bot's posted reply (one Discord message per bubble, the
// first a reply to the call, as the gateway posts them). The turn's chat_turns rows are moved to answeredAt.
// It returns the turn's attribution and the bubbles' message IDs.
func (r *ownAnswerRoom) call(c ownCall) (map[string]any, []string) {
	r.t.Helper()
	m := application.ObservedMessage{MessageID: c.id, AuthorID: c.author, AuthorName: c.name, Content: c.text, At: c.at}
	if c.how == "reply" {
		m.ReplyTo = c.replyTo
	}
	r.post(m, c.how == "mention" || c.ping)
	eng, err := r.svc.MemoryEngine()
	if err != nil {
		r.t.Fatal(err)
	}
	var before int64
	_ = eng.Store.DB.DB.QueryRow(`SELECT COALESCE(MAX(id),0) FROM chat_turns`).Scan(&before)
	r.mu.Lock()
	r.answer, r.plan = c.answer, c.plan
	r.mu.Unlock()
	var ids []string
	res, err := r.svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: c.id,
		Message: c.text, Speaker: c.name, SpeakerID: c.author, NameOnly: c.how == "name",
		Deliver: func(_ context.Context, res chat.Result) string {
			for i, part := range strings.Split(res.Answer, " / ") {
				r.seq++
				id := fmt.Sprintf("botmsg%d", r.seq)
				ids = append(ids, id)
				if r.noBotPosts {
					continue
				}
				bm := application.ObservedMessage{GuildID: "g", ChannelID: "c", MessageID: id, AuthorID: "bot", AuthorName: "마사몽",
					Content: part, At: c.answeredAt.Add(time.Duration(i) * time.Second), FromBot: true}
				if i == 0 {
					bm.ReplyTo = c.id
				}
				r.svc.Observe(bm)
			}
			r.svc.NoteReplyMessages(res.TurnID, "g", "c", ids)
			return res.Answer
		}})
	if err != nil || res.Answer == "" {
		r.t.Fatalf("turn %s: %+v %v", c.id, res, err)
	}
	if _, err := eng.Store.DB.DB.Exec(`UPDATE chat_turns SET created_at=? WHERE id>?`, c.answeredAt.UTC().Format(time.RFC3339), before); err != nil {
		r.t.Fatal(err)
	}
	return r.attribution(res.TurnID), ids
}

func (r *ownAnswerRoom) attribution(turnID string) map[string]any {
	r.t.Helper()
	turn := out(r.t, r.svc, "turns.get", map[string]any{"id": turnID})["turn"].(map[string]any)
	body, _ := turn["body"].(map[string]any)
	ctx, _ := body["context"].(map[string]any)
	att, _ := ctx["attribution"].(map[string]any)
	return att
}

func (r *ownAnswerRoom) lastPrompt() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.prompts[len(r.prompts)-1]
}

// shown lists which of the labelled tokens the last answer prompt holds.
func (r *ownAnswerRoom) shown(tokens [][2]string) string {
	prompt := r.lastPrompt()
	var got []string
	for _, w := range tokens {
		if strings.Contains(prompt, w[1]) {
			got = append(got, w[0])
		}
	}
	return strings.Join(got, " ")
}

// callerAnswersStored counts the bot's answers to a person still kept in chat_turns.
func (r *ownAnswerRoom) callerAnswersStored(speakerID string) int {
	r.t.Helper()
	eng, err := r.svc.MemoryEngine()
	if err != nil {
		r.t.Fatal(err)
	}
	var n int
	if err := eng.Store.DB.DB.QueryRow(`SELECT COUNT(*) FROM chat_turns WHERE role='assistant' AND speaker_id=?`, speakerID).Scan(&n); err != nil {
		r.t.Fatal(err)
	}
	return n
}

// talk fills span after start with n of other people's lines and otherCalls of their exchanges with the bot.
func (r *ownAnswerRoom) talk(start, span time.Duration, n, otherCalls int) {
	r.t.Helper()
	total := n + otherCalls
	for i := 0; i < total; i++ {
		at := r.at(start + span*time.Duration(i)/time.Duration(total))
		if i < otherCalls {
			who := fmt.Sprintf("o%d", i%3)
			r.call(ownCall{id: fmt.Sprintf("oreq%d", i), author: who, name: "Other" + who, text: fmt.Sprintf("@마사몽 OTHERREQ%d 날씨 어때", i),
				at: at, answeredAt: at.Add(10 * time.Second), how: "mention", answer: fmt.Sprintf("OTHERANS%d 맑아", i)})
			continue
		}
		who := fmt.Sprintf("p%d", i%4)
		r.human(fmt.Sprintf("room%d", i), who, "Person"+who, fmt.Sprintf("ROOMLINE%d 게임 얘기", i), at, false)
	}
}

var ownTokens = [][2]string{{"req1", "OWNREQ"}, {"ans1", "OWNANS1"}, {"req2", "OWNFOLLOW"}, {"ans2", "OWNANS2"}}

// TestOwnEarlierExchangeReachesTheNextCall reproduces a production turn: a person called the bot by name for
// a paper, the bot answered (and answered a follow-up 30 s later), the busy room talked on for 17 minutes,
// and the same person called the bot by name again ("summarize it"). Each variant records which of the
// person's earlier lines reach the second call's prompt: req1/ans1 (the request and the bot's answer),
// req2/ans2 (the follow-up and its answer).
//
// Before the fix the exchange was lost whenever the bot's answer line was in neither the live window (in
// memory, empty after a restart) nor the last 24 chat_turns rows channelLines read, because a stored room
// message (memory_observations) carries no mark that it was said to the bot. A turn now reads every kept row,
// and the stored request marks its line as said to the bot (Line.ToBot).
func TestOwnEarlierExchangeReachesTheNextCall(t *testing.T) {
	const all = "req1 ans1 req2 ans2"
	for _, c := range []struct {
		name       string
		mention    bool // @mention instead of the bot's name
		restart    bool // the daemon restarts after the exchange (the live window starts empty)
		noBotPosts bool // the live window never saw the bot's posted replies
		others     int  // other people's lines between the exchange and the second call
		otherCalls int  // other people's exchanges with the bot in between (two chat_turns rows each)
		want       string
	}{
		{name: "name call, live window, 5 lines", others: 5, want: all},
		{name: "name call, live window, 30 lines", others: 30, want: all},
		{name: "mention, live window, 30 lines", mention: true, others: 30, want: all},
		{name: "name call, restart, 5 lines", restart: true, others: 5, want: all},
		{name: "name call, restart, 30 lines", restart: true, others: 30, want: all},
		{name: "mention, restart, 30 lines", mention: true, restart: true, others: 30, want: all},
		// chat_turns: 2 rows for each exchange; the table keeps 40
		{name: "name call, live window, 12 other exchanges", others: 6, otherCalls: 12, want: all},
		{name: "name call, live window, 20 other exchanges (past the 40-row cap)", others: 6, otherCalls: 20, want: all},
		{name: "name call, restart, 11 other exchanges", restart: true, others: 6, otherCalls: 11, want: all},
		{name: "name call, restart, 12 other exchanges", restart: true, others: 6, otherCalls: 12, want: all},
		{name: "mention, restart, 12 other exchanges", mention: true, restart: true, others: 6, otherCalls: 12, want: all},
		// the rows are gone: chat_turns keeps 40 rows of a channel, whatever their age
		{name: "name call, restart, 20 other exchanges (past the 40-row cap)", restart: true, others: 6, otherCalls: 20, want: ""},
		{name: "name call, bot posts not in live window, 12 other exchanges", noBotPosts: true, others: 6, otherCalls: 12, want: all},
		{name: "mention, bot posts not in live window, 12 other exchanges", mention: true, noBotPosts: true, others: 6, otherCalls: 12, want: all},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newOwnAnswerRoom(t)
			r.noBotPosts = c.noBotPosts
			how := "name"
			if c.mention {
				how = "mention"
			}
			// the request and the bot's answer, then a follow-up 30 s later and its answer
			r.call(ownCall{id: "req1", author: "u1", name: "Caller", text: "마사몽 OWNREQ 논문 찾아줘", at: r.at(0), answeredAt: r.at(20 * time.Second),
				how: how, answer: "OWNANS1 찾은 논문은 이거야"})
			att, _ := r.call(ownCall{id: "req2", author: "u1", name: "Caller", text: "마사몽 OWNFOLLOW 그거 저자 누구야", at: r.at(30 * time.Second),
				answeredAt: r.at(50 * time.Second), how: how, answer: "OWNANS2 저자는 이 사람이야"})
			if n, _ := att["included"].(float64); n < 3 {
				t.Fatalf("the follow-up 30 s later did not include the exchange: %v", att)
			}
			if c.restart {
				r.svc = restartServiceForTest(t, r.svc)
			}
			r.talk(time.Minute, 16*time.Minute, c.others, c.otherCalls)
			stored := r.callerAnswersStored("u1")
			att, _ = r.call(ownCall{id: "req3", author: "u1", name: "Caller", text: "마사몽 그거 요약해줘", at: r.at(17*time.Minute + 30*time.Second),
				answeredAt: r.at(17*time.Minute + 50*time.Second), how: how, answer: "요약"})
			got := r.shown(ownTokens)
			t.Logf("in prompt %q; caller's answers still in chat_turns %d; attribution %v", got, stored, att)
			if got != c.want {
				t.Fatalf("in prompt %q, want %q (attribution %v)", got, c.want, att)
			}
		})
	}
}

// TestReplyToTheBotKeepsTheCallersExchange is the production turn as the ops records show it: the person
// called the bot by name (req1), then answered the bot's reply with a Discord reply (req2, 30 s later), and
// 17 minutes later replied to the bot's answer again (req3) — no name, no mention. The bot's answers are
// posted as two bubbles; only the first is a Discord reply to the request. The live window is intact.
func TestReplyToTheBotKeepsTheCallersExchange(t *testing.T) {
	const all = "req1 ans1 req2 ans2"
	for _, c := range []struct {
		name      string
		req2Reply bool // req2 is a Discord reply to the first bubble of ans1 (else a name call)
		toLater   bool // req3 replies to the second bubble of ans2 (else the first)
		ping      bool // the replies ping the bot (Discord lists it among their mentions)
		want      string
	}{
		{name: "req2 name call, req3 replies to the first bubble", want: all},
		{name: "req2 name call, req3 replies to a later bubble", toLater: true, want: all},
		{name: "req2 reply, req3 replies to the first bubble", req2Reply: true, want: all},
		{name: "req2 reply, req3 replies to a later bubble", req2Reply: true, toLater: true, want: all},
		{name: "req2 reply, req3 replies to the first bubble, ping", req2Reply: true, ping: true, want: all},
		{name: "req2 reply, req3 replies to a later bubble, ping", req2Reply: true, toLater: true, ping: true, want: all},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newOwnAnswerRoom(t)
			_, ans1 := r.call(ownCall{id: "req1", author: "u1", name: "Caller", text: "마사몽 OWNREQ 논문 찾아줘", at: r.at(0),
				answeredAt: r.at(20 * time.Second), how: "name", answer: "OWNANS1-A 찾은 논문은 / OWNANS1-B 이거야"})
			req2 := ownCall{id: "req2", author: "u1", name: "Caller", text: "마사몽 OWNFOLLOW 그거 저자 누구야", at: r.at(30 * time.Second),
				answeredAt: r.at(50 * time.Second), how: "name", answer: "OWNANS2-A 저자는 / OWNANS2-B 이 사람이야"}
			if c.req2Reply {
				req2.how, req2.replyTo, req2.ping, req2.text = "reply", ans1[0], c.ping, "OWNFOLLOW 그거 저자 누구야"
			}
			att, ans2 := r.call(req2)
			t.Logf("req2 attribution %v", att)
			r.talk(time.Minute, 16*time.Minute, 5, 0)
			req3 := ownCall{id: "req3", author: "u1", name: "Caller", text: "그거 요약해줘", at: r.at(17*time.Minute + 30*time.Second),
				answeredAt: r.at(17*time.Minute + 50*time.Second), how: "reply", replyTo: ans2[0], ping: c.ping, answer: "요약"}
			if c.toLater {
				req3.replyTo = ans2[1]
			}
			att, _ = r.call(req3)
			got := r.shown(ownTokens)
			t.Logf("in prompt %q; attribution %v", got, att)
			if got != c.want {
				t.Fatalf("in prompt %q, want %q (attribution %v)", got, c.want, att)
			}
		})
	}
}

// A reply to any message the bot's answer was posted as (its first bubble, a later one) is a reply to that
// answer: another person who replies to it gets the answer and the request it answered, also after a restart
// (the posted messages are tied to their turn in reply_messages).
func TestReplyToAPostedPartBringsItsAnswer(t *testing.T) {
	for _, c := range []struct {
		name    string
		part    int
		restart bool
	}{
		{name: "first bubble", part: 0},
		{name: "later bubble", part: 2},
		{name: "first bubble, restart", part: 0, restart: true},
		{name: "later bubble, restart", part: 2, restart: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newOwnAnswerRoom(t)
			_, parts := r.call(ownCall{id: "req1", author: "u1", name: "Asker", text: "@마사몽 OWNREQ 논문 찾아줘", at: r.at(0),
				answeredAt: r.at(20 * time.Second), how: "mention", answer: "OWNANS1-A 찾은 / OWNANS1-B 논문은 / OWNANS1-C 이거야"})
			if c.restart {
				r.svc = restartServiceForTest(t, r.svc)
			}
			r.talk(time.Minute, 5*time.Minute, 4, 0)
			att, _ := r.call(ownCall{id: "req2", author: "u2", name: "Replier", text: "그게 무슨 뜻이야", at: r.at(7 * time.Minute),
				answeredAt: r.at(7*time.Minute + 20*time.Second), how: "reply", replyTo: parts[c.part], answer: "설명"})
			got := r.shown([][2]string{{"req1", "OWNREQ"}, {"ans1-a", "OWNANS1-A"}, {"ans1-c", "OWNANS1-C"}})
			if got != "req1 ans1-a ans1-c" {
				t.Fatalf("in prompt %q (attribution %v)", got, att)
			}
			if strings.Count(r.lastPrompt(), "OWNANS1-C") != 1 {
				t.Fatalf("the answer is shown more than once:\n%s", r.lastPrompt())
			}
		})
	}
}

// A request the bot took by its name alone is said to the bot wherever the line comes from: the live window
// (the turn marks it) and the stored message (its stored request marks it). Here the turn's tool failed, so
// only the request is stored, and the bot's posted reply is not there to link it.
func TestNameCalledRequestStaysToTheBot(t *testing.T) {
	for _, c := range []struct {
		name    string
		restart bool
	}{
		{name: "live window, bot post not seen"},
		{name: "restart", restart: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newOwnAnswerRoom(t)
			r.noBotPosts = !c.restart
			r.call(ownCall{id: "req1", author: "u1", name: "Caller", text: "마사몽 OWNREQ 아틀란티스 날씨", at: r.at(0), answeredAt: r.at(20 * time.Second),
				how: "name", answer: "OWNANS1 못 찾았어",
				plan: `{"memory_requirement":"none","tool_plan":[{"tool_name":"weather","parameters":{"region":"Atlantis","source":"global"}}]}`})
			if n := r.callerAnswersStored("u1"); n != 0 {
				t.Fatalf("the failed tool exchange stored %d answers", n)
			}
			if c.restart {
				r.svc = restartServiceForTest(t, r.svc)
			}
			r.talk(time.Minute, 5*time.Minute, 4, 0)
			att, _ := r.call(ownCall{id: "req2", author: "u1", name: "Caller", text: "마사몽 다시 해봐", at: r.at(7 * time.Minute),
				answeredAt: r.at(7*time.Minute + 20*time.Second), how: "name", answer: "응"})
			if got := r.shown([][2]string{{"req1", "OWNREQ"}}); got != "req1" {
				t.Fatalf("the earlier request is missing (attribution %v)", att)
			}
		})
	}
}

// The caller's own last exchanges with the bot come in even when the room moved on past the 30-minute window:
// the newest three within six hours, the bot's older answers clipped. An older one, another person's exchange
// and the room's talk stay out.
func TestOwnExchangesReachPastTheWindow(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart %v", restart), func(t *testing.T) {
			r := newOwnAnswerRoom(t)
			r.t0 = time.Now().Add(-8 * time.Hour)
			ask := func(id, token string, at time.Duration, answer string) {
				r.call(ownCall{id: id, author: "u1", name: "Caller", text: "마사몽 " + token + " 알려줘", at: r.at(at), answeredAt: r.at(at + 20*time.Second),
					how: "name", answer: answer})
			}
			ask("old", "REQOLD", 0, "ANSOLD 이거야")
			ask("a", "REQA", 5*time.Hour, "ANSA 이거야")
			ask("b", "REQB", 5*time.Hour+10*time.Minute, "ANSB 이거야")
			ask("c", "REQC", 5*time.Hour+20*time.Minute, "ANSC 이거야")
			ask("d", "REQD", 5*time.Hour+30*time.Minute, "ANSD "+strings.Repeat("가", 600)+" ANSDEND")
			r.call(ownCall{id: "o1", author: "u2", name: "Other", text: "@마사몽 OTHERREQ 알려줘", at: r.at(5*time.Hour + 40*time.Minute),
				answeredAt: r.at(5*time.Hour + 41*time.Minute), how: "mention", answer: "OTHERANS 이거야"})
			if restart {
				r.svc = restartServiceForTest(t, r.svc)
			}
			r.talk(6*time.Hour, 50*time.Minute, 6, 0)
			att, _ := r.call(ownCall{id: "now", author: "u1", name: "Caller", text: "마사몽 아까 그거 다시", at: r.at(7 * time.Hour),
				answeredAt: r.at(7*time.Hour + 20*time.Second), how: "name", answer: "응"})
			got := r.shown([][2]string{{"old", "REQOLD"}, {"a", "REQA"}, {"b", "REQB"}, {"b'", "ANSB"}, {"c", "REQC"}, {"c'", "ANSC"},
				{"d", "REQD"}, {"d'", "ANSD"}, {"d-end", "ANSDEND"}, {"other", "OTHER"}, {"room", "ROOMLINE"}})
			if got != "b b' c c' d d'" {
				t.Fatalf("in prompt %q, want the last three exchanges, the long answer clipped (attribution %v)", got, att)
			}
			if reasons, _ := att["reasons"].(map[string]any); reasons["own"] != float64(6) {
				t.Fatalf("reasons %v", att["reasons"])
			}
		})
	}
}
