package application_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"masamong/internal/application"
	"masamong/internal/config"
	"masamong/internal/memory/engine"
)

// ownAnswerRoom drives one channel through the production sequence: a person asks the bot something, the
// bot answers (stored as chat turns and posted as a reply the live window sees), the room keeps talking for
// a while, and the same person calls the bot again. Times are simulated in the past; the rows the store
// stamps with the wall clock (chat_turns.created_at) are moved to the simulated time after each turn.
type ownAnswerRoom struct {
	t       *testing.T
	svc     *application.Service
	mu      sync.Mutex
	answer  string // what the response model says next
	prompts []string
	t0      time.Time
	seq     int
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
	eng, err := r.svc.MemoryEngine()
	if err != nil {
		r.t.Fatal(err)
	}
	if _, err := eng.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", MessageID: id, AuthorID: author, AuthorName: name,
		Content: text, At: at}); err != nil {
		r.t.Fatal(err)
	}
	m := application.ObservedMessage{GuildID: "g", ChannelID: "c", MessageID: id, AuthorID: author, AuthorName: name, Content: text, At: at}
	if mentionBot {
		m.Mentions = []string{"bot"}
	}
	r.svc.Observe(m)
}

// call is a person calling the bot: the message, the turn, and the bot's posted reply (a Discord reply to the
// call, as the gateway posts it) at answeredAt. The turn's chat_turns rows are moved to answeredAt.
func (r *ownAnswerRoom) call(id, author, name, text string, at, answeredAt time.Time, mention bool, answer string) map[string]any {
	r.t.Helper()
	r.human(id, author, name, text, at, mention)
	eng, err := r.svc.MemoryEngine()
	if err != nil {
		r.t.Fatal(err)
	}
	var before int64
	_ = eng.Store.DB.DB.QueryRow(`SELECT COALESCE(MAX(id),0) FROM chat_turns`).Scan(&before)
	r.mu.Lock()
	r.answer = answer
	r.mu.Unlock()
	res, err := r.svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: id,
		Message: text, Speaker: name, SpeakerID: author, NameOnly: !mention})
	if err != nil || res.Answer == "" {
		r.t.Fatalf("turn %s: %+v %v", id, res, err)
	}
	if _, err := eng.Store.DB.DB.Exec(`UPDATE chat_turns SET created_at=? WHERE id>?`, answeredAt.UTC().Format(time.RFC3339), before); err != nil {
		r.t.Fatal(err)
	}
	r.seq++
	if r.noBotPosts {
		return r.attribution(res.TurnID)
	}
	r.svc.Observe(application.ObservedMessage{GuildID: "g", ChannelID: "c", MessageID: fmt.Sprintf("botmsg%d", r.seq), AuthorID: "bot",
		AuthorName: "마사몽", Content: answer, At: answeredAt, FromBot: true, ReplyTo: id})
	return r.attribution(res.TurnID)
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

// TestOwnEarlierExchangeReachesTheNextCall reproduces a production turn: a person called the bot by name for
// a paper, the bot answered (and answered a follow-up 30 s later), the busy room talked on for 17 minutes,
// and the same person called the bot by name again ("summarize it"). The turn's attribution kept only the
// call. Each variant records which of the person's earlier lines reach the second call's prompt: req1/ans1
// (the request and the bot's answer), req2/ans2 (the follow-up and its answer). want is what the code does
// today; every variant that does not keep all four is the defect.
//
// Cause: the caller's earlier request joins the conversation only through the bot's answer line
// (conversation.Attribute, bot_reply) or a mention of the bot. A stored room message (memory_observations)
// carries no mention, so a name-called request has no mark of its own, and the bot's answer line exists only
// in the live window (in memory, empty after a restart) or in the last 24 chat_turns rows channelLines reads
// (conversation.go), which other people's exchanges push out long before the 30-minute attribution window
// ends, while the table still keeps 40 rows.
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
		// chat_turns: 2 rows for each exchange; channelLines reads the last 24, the table keeps 40
		{name: "name call, live window, 12 other exchanges", others: 6, otherCalls: 12, want: all},
		{name: "name call, live window, 20 other exchanges (past the 40-row cap)", others: 6, otherCalls: 20, want: all},
		{name: "name call, restart, 11 other exchanges", restart: true, others: 6, otherCalls: 11, want: "req2 ans2"},
		{name: "name call, restart, 12 other exchanges", restart: true, others: 6, otherCalls: 12, want: ""},
		{name: "mention, restart, 12 other exchanges", mention: true, restart: true, others: 6, otherCalls: 12, want: ""},
		{name: "name call, restart, 20 other exchanges (past the 40-row cap)", restart: true, others: 6, otherCalls: 20, want: ""},
		{name: "name call, bot posts not in live window, 12 other exchanges", noBotPosts: true, others: 6, otherCalls: 12, want: ""},
		{name: "mention, bot posts not in live window, 12 other exchanges", mention: true, noBotPosts: true, others: 6, otherCalls: 12, want: "req1 req2"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newOwnAnswerRoom(t)
			r.noBotPosts = c.noBotPosts
			// the request and the bot's answer, then a follow-up 30 s later and its answer
			r.call("req1", "u1", "Caller", "마사몽 OWNREQ 논문 찾아줘", r.at(0), r.at(20*time.Second), c.mention, "OWNANS1 찾은 논문은 이거야")
			att := r.call("req2", "u1", "Caller", "마사몽 OWNFOLLOW 그거 저자 누구야", r.at(30*time.Second), r.at(50*time.Second), c.mention, "OWNANS2 저자는 이 사람이야")
			if n, _ := att["included"].(float64); n < 3 {
				t.Fatalf("the follow-up 30 s later did not include the exchange: %v", att)
			}
			if c.restart {
				r.svc = restartServiceForTest(t, r.svc)
			}
			// the room talks on for 17 minutes; other callers' exchanges interleave
			span := 16 * time.Minute
			total := c.others + c.otherCalls
			for i := 0; i < total; i++ {
				at := r.at(time.Minute + span*time.Duration(i)/time.Duration(total))
				if i < c.otherCalls {
					who := fmt.Sprintf("o%d", i%3)
					r.call(fmt.Sprintf("oreq%d", i), who, "Other"+who, fmt.Sprintf("@마사몽 OTHERREQ%d 날씨 어때", i), at, at.Add(10*time.Second), true,
						fmt.Sprintf("OTHERANS%d 맑아", i))
					continue
				}
				who := fmt.Sprintf("p%d", i%4)
				r.human(fmt.Sprintf("room%d", i), who, "Person"+who, fmt.Sprintf("ROOMLINE%d 게임 얘기", i), at, false)
			}
			stored := r.callerAnswersStored("u1")
			att = r.call("req3", "u1", "Caller", "마사몽 그거 요약해줘", r.at(17*time.Minute+30*time.Second), r.at(17*time.Minute+50*time.Second), c.mention, "요약")
			prompt := r.lastPrompt()
			var got []string
			for _, w := range []struct{ label, token string }{{"req1", "OWNREQ"}, {"ans1", "OWNANS1"}, {"req2", "OWNFOLLOW"}, {"ans2", "OWNANS2"}} {
				if strings.Contains(prompt, w.token) {
					got = append(got, w.label)
				}
			}
			t.Logf("in prompt %q; caller's answers still in chat_turns %d; attribution %v", strings.Join(got, " "), stored, att)
			if strings.Join(got, " ") != c.want {
				t.Fatalf("in prompt %q, want %q (attribution %v)", strings.Join(got, " "), c.want, att)
			}
		})
	}
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
