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
	"masamong/internal/memory/blocks"
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

func TestDeferredReplyPublishesAtItsEdit(t *testing.T) {
	for _, multi := range []bool{false, true} {
		t.Run(fmt.Sprintf("multipart=%t", multi), func(t *testing.T) {
			r := newOwnAnswerRoom(t)
			r.answer = "FIRSTDELIVERY"
			if multi {
				r.answer += " SECONDDELIVERY"
			}
			var queued string
			_, err := r.svc.ChatTurn(t.Context(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "dc:slash",
				SpeakerID: "a", Speaker: "Asker", Message: "SLASH", Deliver: func(ctx context.Context, res chat.Result) string {
					now := time.Now()
					queued = snowflake(now)
					ids, ends := []string{snowflake(now.Add(-time.Minute))}, []int{len("FIRSTDELIVERY")}
					if multi {
						// The single-part case has only the queued call's ID, no live observation.
						r.human(queued, "a", "Asker", "FOLLOW", now, true)
						ids = append(ids, snowflake(now.Add(time.Second)))
						ends = append(ends, len(res.Answer))
					}
					application.NoteFirstSend(ctx)
					r.svc.NoteReplyMessages(res.TurnID, "g", "c", ids, ends...)
					return res.Answer
				}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := r.svc.ChatTurn(t.Context(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: queued, SpeakerID: "a", Speaker: "Asker", Message: "FOLLOW"}); err != nil {
				t.Fatal(err)
			}
			if got := r.shown([][2]string{{"first", "FIRSTDELIVERY"}, {"second", "SECONDDELIVERY"}}); got != "" {
				t.Fatalf("placeholder exposed text before its edit: %s", got)
			}
		})
	}
}

func TestInFlightEditAddressAfterRestart(t *testing.T) {
	for _, reply := range []bool{false, true} {
		t.Run(fmt.Sprintf("reply=%t", reply), func(t *testing.T) {
			r := newOwnAnswerRoom(t)
			m := application.ObservedMessage{MessageID: "asked", AuthorID: "a", AuthorName: "Asker", Content: "original", At: r.at(0)}
			if reply {
				m.ReplyTo, m.ReplyAuthorID = "prior-post", "bot"
			}
			r.post(m, true)
			r.human("separator", "b", "Other", "separate topic", r.at(time.Minute), false)
			r.answer = "initial answer"
			_, err := r.svc.ChatTurn(t.Context(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "asked",
				Message: "original", Speaker: "Asker", SpeakerID: "a", Deliver: func(ctx context.Context, res chat.Result) string {
					if err := r.svc.MessageEdited("g", "c", "asked", "REVISIONTOKEN", r.at(2*time.Minute)); err != nil {
						t.Fatal(err)
					}
					if err := r.svc.DrainRedactions(ctx); err != nil {
						t.Fatal(err)
					}
					return res.Answer
				}})
			if err != nil {
				t.Fatal(err)
			}
			r.svc = restartServiceForTest(t, r.svc)
			r.call(ownCall{id: "fresh", author: "a", name: "Asker", text: "fresh", how: "mention", at: r.at(5 * time.Minute), answeredAt: r.at(6 * time.Minute), answer: "next"})
			if got := strings.Contains(r.lastPrompt(), "REVISIONTOKEN"); got != reply {
				t.Fatalf("edited words in cold prompt: %t, want %t", got, reply)
			}
		})
	}
}

func TestLegacyAliasReplaysBeforeCommit(t *testing.T) {
	r := newOwnAnswerRoom(t)
	r.human("request", "a", "Asker", "LEGACYREQ", r.at(0), true)
	db := r.svc.Store.DB
	post := snowflake(r.at(time.Second))
	stamp := r.at(4 * time.Second).UTC().Format(time.RFC3339Nano)
	for role, text := range map[string]string{"user": "LEGACYREQ", "assistant": "LEGACYANS"} {
		if _, err := db.Exec(`INSERT INTO chat_turns(channel_key,role,speaker,content,created_at,native_id,speaker_id,evidence_state)
			VALUES(?,?,'Asker',?,?,'request','a',1)`, blocks.ChannelKey("g", "c"), role, text, stamp); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO reply_messages(message_id,turn_id,guild_id,channel_id,at) VALUES(?,'legacy-turn','g','c',?)`, post, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO turn_log(id,at,day,hour,guild_id,channel_id,outcome,tools,hits,cost_usd,latency_ms,lab,body,speaker_ref,evidence_state)
		VALUES('legacy-turn',?,'',0,'g','c','answered','',0,0,0,0,'{"message_id":"request"}','',1)`, stamp); err != nil {
		t.Fatal(err)
	}
	r.call(ownCall{id: "queued", author: "b", name: "Replier", text: "follow up", at: r.at(2 * time.Second),
		answeredAt: r.at(5 * time.Second), how: "reply", replyTo: post, answer: "next"})
	if !strings.Contains(r.lastPrompt(), "LEGACYANS") {
		t.Fatal("reply cannot see the delivered legacy answer")
	}
}

func TestOlderAnswerThroughPartAliasIsClipped(t *testing.T) {
	r := newOwnAnswerRoom(t)
	_, parts := r.call(ownCall{id: "request", author: "a", name: "Asker", text: "request",
		at: r.at(0), answeredAt: r.at(time.Second), how: "mention",
		answer: "FIRSTPART / " + strings.Repeat("x", 450) + " ENDMARK"})
	r.call(ownCall{id: "reply", author: "b", name: "Replier", text: "follow up",
		at: r.at(time.Minute), answeredAt: r.at(2 * time.Minute), how: "reply", replyTo: parts[1], answer: "next"})
	if got := r.shown([][2]string{{"start", "FIRSTPART"}, {"tail", "ENDMARK"}}); got != "start tail" {
		t.Fatalf("reply to the current exchange shows %q, want the full answer", got)
	}
	r.call(ownCall{id: "later", author: "b", name: "Replier", text: "another follow up",
		at: r.at(3 * time.Minute), answeredAt: r.at(4 * time.Minute), how: "mention", answer: "final"})
	if got := r.shown([][2]string{{"start", "FIRSTPART"}, {"tail", "ENDMARK"}}); got != "start" {
		t.Fatalf("reply alias shows %q, want only the clipped answer", got)
	}
}

func TestReplyToOlderExchangePreservesNewerTail(t *testing.T) {
	r := newOwnAnswerRoom(t)
	long := strings.Repeat("x", 450)
	var target string
	for i, tail := range []string{"OLDESTEND", "TARGETEND", "NEWESTEND"} {
		id := fmt.Sprintf("request%d", i)
		_, parts := r.call(ownCall{id: id, author: "a", name: "Asker", how: "mention", text: id,
			at: r.at(time.Duration(i) * time.Minute), answeredAt: r.at(time.Duration(i)*time.Minute + time.Second), answer: long + tail})
		if i == 1 {
			target = parts[0]
		}
	}
	r.call(ownCall{id: "reply", author: "a", name: "Asker", how: "reply", replyTo: target, text: "follow",
		at: r.at(3 * time.Minute), answeredAt: r.at(4 * time.Minute), answer: "final"})
	if got := r.shown([][2]string{{"oldest", "OLDESTEND"}, {"target", "TARGETEND"}, {"newest", "NEWESTEND"}}); got != "target newest" {
		t.Fatalf("visible tails: %q", got)
	}
}

func TestMorePartsJoinOnlyTheirSurvivingAnswer(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		t.Run(fmt.Sprintf("deleted=%t", deleted), func(t *testing.T) {
			r := newOwnAnswerRoom(t)
			_, head := r.call(ownCall{id: "request", author: "a", name: "Asker", text: "MOREQUESTION", at: r.at(0),
				answeredAt: r.at(time.Second), how: "mention", answer: "HEADTOKEN"})
			if deleted {
				changeSource(t, r.svc, "delete", "request", "a")
			}
			for i := 0; i < 2; i++ { // duplicate notification must not double the delivered text
				if err := r.svc.NoteReplyContinuation("g", "c", head[0], "RESTTOKEN", []string{"more-a", "more-b"}); err != nil {
					t.Fatal(err)
				}
			}
			if deleted && r.callerAnswersStored("a") != 0 {
				t.Fatal("more recreated the removed answer")
			}
			r.svc = restartServiceForTest(t, r.svc)
			r.call(ownCall{id: "reply", author: "b", name: "Replier", text: "follow up", at: r.at(time.Minute),
				answeredAt: r.at(time.Minute + time.Second), how: "reply", replyTo: "more-b", answer: "next"})
			want := 1
			if deleted {
				want = 0
			}
			for _, token := range []string{"MOREQUESTION", "HEADTOKEN", "RESTTOKEN"} {
				if n := strings.Count(r.lastPrompt(), token); n != want {
					t.Fatalf("reply through a continuation after restart: %s count %d, want %d", token, n, want)
				}
			}
		})
	}
}

func TestReplyToUnthreadedPostKeepsItsTail(t *testing.T) {
	r := newOwnAnswerRoom(t)
	r.svc.Observe(application.ObservedMessage{GuildID: "g", ChannelID: "c", MessageID: "unthreaded",
		AuthorID: "bot", FromBot: true, At: r.at(0), Content: strings.Repeat("h", 500) + "CURRENTTAIL"})
	r.call(ownCall{id: "reply", author: "b", name: "Replier", text: "follow up", at: r.at(time.Second),
		answeredAt: r.at(2 * time.Second), how: "reply", replyTo: "unthreaded", answer: "next"})
	if !strings.Contains(r.lastPrompt(), "CURRENTTAIL") {
		t.Fatal("direct reply to a post without a request lost its tail")
	}
}

func TestReplyDuringContinuationSend(t *testing.T) {
	r := newOwnAnswerRoom(t)
	_, head := r.call(ownCall{id: "request", author: "a", name: "Asker", text: "ORIGIN",
		at: r.at(0), answeredAt: r.at(time.Second), how: "mention", answer: strings.Repeat("h", 650)})
	first := snowflake(r.at(2 * time.Second))
	last := snowflake(r.at(4 * time.Second))
	done := make(chan error, 1)
	err := r.svc.DeliverReplyContinuation(t.Context(), "g", "c", head[0], func() (string, []string, []int) {
		// Discord has displayed first, but its REST send has not returned an ID
		// and no gateway observation of the continuation is available.
		r.post(application.ObservedMessage{MessageID: "queued", AuthorID: "b", AuthorName: "Replier",
			Content: "follow up", ReplyTo: first, At: r.at(3 * time.Second)}, false)
		go func() {
			_, err := r.svc.ChatTurn(t.Context(), application.ChatInput{GuildID: "g", ChannelID: "c",
				MessageID: "queued", SpeakerID: "b", Speaker: "Replier", Message: "follow up"})
			done <- err
		}()
		for deadline := time.Now().Add(5 * time.Second); r.svc.TurnsInFlightForTest() == 0; {
			if time.Now().After(deadline) {
				t.Fatal("replier did not queue during send")
			}
			time.Sleep(time.Millisecond)
		}
		return "RESTFIRSTRESTLAST", []string{first, last}, []int{len("RESTFIRST"), len("RESTFIRSTRESTLAST")}
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := r.shown([][2]string{{"request", "ORIGIN"}, {"first", "RESTFIRST"}, {"later", "RESTLAST"}}); got != "request first" {
		t.Fatalf("reply during continuation send saw %q", got)
	}
	r.svc = restartServiceForTest(t, r.svc)
	r.call(ownCall{id: "later", author: "c", name: "Other", text: "follow up", at: r.at(time.Minute),
		answeredAt: r.at(2 * time.Minute), how: "reply", replyTo: last, answer: "next"})
	if got := r.shown([][2]string{{"request", "ORIGIN"}, {"last", "RESTLAST"}}); got != "request last" {
		t.Fatalf("continuation alias after restart saw %q", got)
	}
}

func TestFailedWritesNeverClearDeliveredPosts(t *testing.T) {
	for _, tc := range []struct {
		name, trigger string
	}{
		{"registration", `BEFORE INSERT ON reply_messages WHEN NEW.message_id='botmsg2' BEGIN SELECT RAISE(FAIL,'synthetic failure'); END`},
		{"assistant write", `BEFORE INSERT ON chat_turns WHEN NEW.role='assistant' BEGIN SELECT RAISE(FAIL,'synthetic failure'); END`},
		{"assistant declined", `BEFORE INSERT ON chat_turns WHEN NEW.role='assistant' BEGIN SELECT RAISE(IGNORE); END`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newOwnAnswerRoom(t)
			if _, err := r.svc.Store.DB.Exec(`CREATE TRIGGER refuse_write ` + tc.trigger); err != nil {
				t.Fatal(err)
			}
			_, parts := r.call(ownCall{id: "request", author: "a", name: "Asker", text: "request", at: r.at(0),
				answeredAt: r.at(time.Second), how: "mention", answer: "FAILEDHEAD / FAILEDPART"})
			var published int
			if tc.name == "registration" && r.callerAnswersStored("a") != 1 {
				t.Fatal("a failed alias write discarded a successfully delivered answer")
			}
			if err := r.svc.Store.DB.QueryRow(`SELECT COUNT(*) FROM reply_messages WHERE request_id='request'`).Scan(&published); err != nil || published != 0 {
				t.Fatalf("%d posts published without a kept answer: %v", published, err)
			}
			r.call(ownCall{id: "reply", author: "b", name: "Replier", text: "follow up", at: r.at(time.Minute),
				answeredAt: r.at(2 * time.Minute), how: "reply", replyTo: parts[1], answer: "next"})
			if strings.Contains(r.lastPrompt(), "FAILEDPART") {
				t.Fatal("unverified post entered the replier's context")
			}
		})
	}
}

func TestPruneFailureStillLinksCommittedReply(t *testing.T) {
	r := newOwnAnswerRoom(t)
	db := r.svc.Store.DB
	if _, err := db.Exec(`WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<40)
		INSERT INTO chat_turns(channel_key,role,content,created_at) SELECT ?,'user','seed',? FROM n`,
		blocks.ChannelKey("g", "c"), r.at(-time.Hour).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER fail_prune BEFORE DELETE ON chat_turns BEGIN SELECT RAISE(FAIL,'synthetic prune failure'); END`); err != nil {
		t.Fatal(err)
	}
	_, parts := r.call(ownCall{id: "request", author: "a", name: "Asker", text: "question", at: r.at(0),
		answeredAt: r.at(time.Second), how: "mention", answer: "head / tail"})
	for _, id := range parts {
		var request string
		if err := db.QueryRow(`SELECT request_id FROM reply_messages WHERE message_id=?`, id).Scan(&request); err != nil || request != "request" {
			t.Fatalf("part %s not linked to committed answer: %q (%v)", id, request, err)
		}
	}
}

func TestContinuationStaysWithItsExactDelivery(t *testing.T) {
	r := newOwnAnswerRoom(t)
	c := ownCall{id: "request", author: "a", name: "Asker", text: "ORIGIN",
		at: r.at(0), answeredAt: r.at(time.Second), how: "mention", answer: strings.Repeat("h", 650)}
	_, head := r.call(c)
	rest := snowflake(r.at(2 * time.Second))
	if err := r.svc.NoteReplyContinuation("g", "c", head[0], "ORIGINALREST", []string{rest}); err != nil {
		t.Fatal(err)
	}
	c.answer, c.answeredAt = strings.Repeat("x", 700), r.at(3*time.Second)
	r.call(c)
	r.svc = restartServiceForTest(t, r.svc)
	r.call(ownCall{id: "reply", author: "b", name: "Replier", text: "follow up", at: r.at(time.Minute),
		answeredAt: r.at(2 * time.Minute), how: "reply", replyTo: rest, answer: "next"})
	if !strings.Contains(r.lastPrompt(), "ORIGINALREST") {
		t.Fatal("a later delivery for the same request displaced the continuation")
	}
}

func TestHistoryCutoffBeforeContinuation(t *testing.T) {
	for _, mode := range []string{"on", "off"} {
		t.Run(mode, func(t *testing.T) {
			r := newOwnAnswerRoom(t)
			applyDoc(t, r.svc, func(d *config.Document) {
				policy := d.Guilds["g"].Channels["c"]
				policy.Attribution = mode
				d.Guilds["g"].Channels["c"] = policy
			})
			_, head := r.call(ownCall{id: "request", author: "a", name: "Asker", text: "start",
				at: r.at(0), answeredAt: r.at(time.Second), how: "mention", answer: "VISIBLEHEAD"})
			if err := r.svc.NoteReplyContinuation("g", "c", head[0], "UNPOSTEDREST", []string{snowflake(r.at(4 * time.Second))}); err != nil {
				t.Fatal(err)
			}
			r.call(ownCall{id: "future", author: "a", name: "Asker", text: "another question",
				at: r.at(5 * time.Second), answeredAt: r.at(6 * time.Second), how: "mention", answer: "FUTUREANSWER"})
			r.call(ownCall{id: "queued", author: "a", name: "Asker", text: "follow up",
				at: r.at(2 * time.Second), answeredAt: r.at(7 * time.Second), how: "mention", answer: "next"})
			if got := r.shown([][2]string{{"head", "VISIBLEHEAD"}, {"rest", "UNPOSTEDREST"}, {"future", "FUTUREANSWER"}}); got != "head" {
				t.Fatalf("earlier call saw %q", got)
			}
		})
	}
}

func TestReviewOwnBurstKeepsFullRequest(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart=%t", restart), func(t *testing.T) {
			r := newOwnAnswerRoom(t)
			r.human("first", "a", "Asker", "BURSTFIRST", r.at(0), false)
			r.human("last", "a", "Asker", "BURSTLAST", r.at(time.Second), false)
			r.answer = "BURSTANSWER"
			res, err := r.svc.ChatTurn(t.Context(), application.ChatInput{GuildID: "g", ChannelID: "c",
				MessageID: "last", Message: "BURSTFIRST\nBURSTLAST", Speaker: "Asker", SpeakerID: "a", Fragments: []string{"first"}})
			if err != nil || res.Answer == "" {
				t.Fatalf("burst turn: %v", err)
			}
			answered := r.at(20 * time.Second)
			if restart {
				answered = r.at(40 * time.Minute) // only the answer is inside the later call's room window
			}
			if _, err := r.svc.Store.DB.Exec(`UPDATE chat_turns SET spoken_at=? WHERE role='assistant'`, answered.UTC().Format(time.RFC3339Nano)); err != nil {
				t.Fatal(err)
			}
			if restart {
				r.svc = restartServiceForTest(t, r.svc)
			}
			r.call(ownCall{id: "next", author: "a", name: "Asker", text: "follow up", at: r.at(time.Hour),
				answeredAt: r.at(time.Hour + time.Second), how: "mention", answer: "next answer"})
			if got := r.shown([][2]string{{"first", "BURSTFIRST"}, {"last", "BURSTLAST"}, {"answer", "BURSTANSWER"}}); got != "first last answer" {
				t.Fatalf("included %s", got)
			}
			changeSource(t, r.svc, "delete", "first", "a")
			if n := chainCount(t, r.svc, `SELECT COUNT(*) FROM chat_turns WHERE native_id='next' AND role='assistant'`); n != 0 {
				t.Fatal("answer that read the first fragment survived its deletion")
			}
		})
	}
}

func TestReviewOwnCountsExchangesNotBubbles(t *testing.T) {
	r := newOwnAnswerRoom(t)
	r.call(ownCall{id: "good", author: "a", name: "Asker", text: "GOODREQ", at: r.at(0),
		answeredAt: r.at(time.Second), how: "mention", answer: "GOODANS"})
	r.call(ownCall{id: "failed", author: "a", name: "Asker", text: "FAILREQ", at: r.at(10 * time.Minute),
		answeredAt: r.at(11 * time.Minute), how: "mention", answer: "FAILANS-A / FAILANS-B / FAILANS-C",
		plan: `{"memory_requirement":"none","tool_plan":[{"tool_name":"weather","parameters":{"region":"Atlantis","source":"global"}}]}`})
	if n := r.callerAnswersStored("a"); n != 1 {
		t.Fatalf("expected only the successful assistant row, got %d", n)
	}
	r.call(ownCall{id: "now", author: "a", name: "Asker", text: "follow up", at: r.at(time.Hour),
		answeredAt: r.at(time.Hour + time.Second), how: "mention", answer: "next"})
	if got := r.shown([][2]string{{"good", "GOODANS"}, {"failed", "FAILANS"}, {"failed request", "FAILREQ"}}); got != "good" {
		t.Fatalf("prior exchanges: %s", got)
	}
}

func TestFailedToolPostIsNotAReplyAlias(t *testing.T) {
	r := newOwnAnswerRoom(t)
	_, parts := r.call(ownCall{id: "failed", author: "a", name: "Asker", text: "FAILREQ", at: r.at(0),
		answeredAt: r.at(time.Second), how: "mention", answer: "FAILANS-A / FAILANS-B",
		plan: `{"memory_requirement":"none","tool_plan":[{"tool_name":"weather","parameters":{"region":"Atlantis","source":"global"}}]}`})
	r.call(ownCall{id: "other", author: "b", name: "Replier", text: "follow up", at: r.at(time.Minute),
		answeredAt: r.at(2 * time.Minute), how: "reply", replyTo: parts[1], answer: "next"})
	if got := r.shown([][2]string{{"answer", "FAILANS"}, {"request", "FAILREQ"}}); got != "" {
		t.Fatalf("reply through a failed turn included %s", got)
	}
}

func TestReviewPostedAliasBeforeTurnLogCommit(t *testing.T) {
	r := newOwnAnswerRoom(t)
	r.t0 = time.Now()
	// Leave the first turn in exactly the state visible after gate release but before
	// its diagnostic insert. No hook or goroutine needs to race the log writer.
	if _, err := r.svc.Store.DB.Exec(`CREATE TRIGGER hold_record BEFORE INSERT ON turn_log
		WHEN json_extract(NEW.body,'$.message_id')='request' BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
	r.human("request", "a", "Asker", "GAPREQUEST", r.at(0), true)
	r.answer = "GAPFIRST / GAPLATER"
	_, err := r.svc.ChatTurn(t.Context(), application.ChatInput{GuildID: "g", ChannelID: "c",
		MessageID: "request", Message: "GAPREQUEST", Speaker: "Asker", SpeakerID: "a",
		Deliver: func(_ context.Context, res chat.Result) string {
			r.svc.NoteReplyMessages(res.TurnID, "g", "c", []string{"first", "later"})
			return res.Answer
		}})
	if err != nil {
		t.Fatal(err)
	}
	if n := chainCount(t, r.svc, `SELECT COUNT(*) FROM turn_log`); n != 0 {
		t.Fatal("first turn already logged")
	}
	r.call(ownCall{id: "reply", author: "b", name: "Replier", text: "follow up", at: r.at(time.Minute),
		answeredAt: r.at(2 * time.Minute), how: "reply", replyTo: "later", answer: "next"})
	if got := r.shown([][2]string{{"request", "GAPREQUEST"}, {"first", "GAPFIRST"}, {"later", "GAPLATER"}}); got != "request first later" {
		t.Fatalf("reply before log insert included %s", got)
	}
}

func TestReviewRealQueuedFollowupBeforeCommit(t *testing.T) {
	for _, observed := range []bool{true, false} {
		t.Run(fmt.Sprintf("observed=%t", observed), func(t *testing.T) {
			r := newOwnAnswerRoom(t)
			r.answer = "DELIVEREDANSWER\nLATERBUBBLE"
			r.human("initial", "a", "Asker", "FIRSTREQUEST", r.at(0), true)
			done := make(chan error, 1)
			_, err := r.svc.ChatTurn(t.Context(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "initial",
				Message: "FIRSTREQUEST", Speaker: "Asker", SpeakerID: "a", Deliver: func(_ context.Context, res chat.Result) string {
					sent := r.at(time.Second)
					id := snowflake(sent)
					r.svc.Observe(application.ObservedMessage{GuildID: "g", ChannelID: "c", MessageID: "tool-post", ReplyTo: "initial",
						FromBot: true, AuthorID: "bot", Content: "attachment", At: r.at(500 * time.Millisecond)})
					if observed {
						id = "observed-answer" // exercise the live timestamp without a snowflake
						r.svc.Observe(application.ObservedMessage{GuildID: "g", ChannelID: "c", MessageID: id, ReplyTo: "initial",
							FromBot: true, AuthorID: "bot", Content: "DELIVEREDANSWER", At: sent})
					}
					r.human("queued", "a", "Asker", "NEXTREQUEST", r.at(2*time.Second), true)
					r.mu.Lock()
					r.answer = "NEXTANSWER"
					r.mu.Unlock()
					go func() {
						_, err := r.svc.ChatTurn(t.Context(), application.ChatInput{GuildID: "g", ChannelID: "c",
							MessageID: "queued", Message: "NEXTREQUEST", Speaker: "Asker", SpeakerID: "a"})
						done <- err
					}()
					for deadline := time.Now().Add(5 * time.Second); r.svc.TurnsInFlightForTest() < 2; {
						if time.Now().After(deadline) {
							t.Error("follow-up did not queue before first commit")
							break
						}
						time.Sleep(time.Millisecond)
					}
					last := snowflake(r.at(3 * time.Second))
					r.svc.NoteReplyMessages(res.TurnID, "g", "c", []string{id, last}, len("DELIVEREDANSWER"), len(res.Answer))
					return res.Answer
				}})
			if err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("queued turn did not finish")
			}
			if got := r.shown([][2]string{{"request", "FIRSTREQUEST"}, {"answer", "DELIVEREDANSWER"}, {"late", "LATERBUBBLE"}}); got != "request answer" {
				t.Fatalf("queued call saw %s", got)
			}
			var stamp string
			if err := r.svc.Store.DB.QueryRow(`SELECT spoken_at FROM chat_turns WHERE native_id='initial' AND role='assistant'`).Scan(&stamp); err != nil {
				t.Fatal(err)
			}
			if sent, err := time.Parse(time.RFC3339Nano, stamp); err != nil || sent.UnixMilli() != r.at(time.Second).UnixMilli() {
				t.Fatalf("stored delivery time %q, want %v", stamp, r.at(time.Second))
			}
		})
	}
}

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
	// Without a gateway observation or a Discord snowflake this fixture's synthetic
	// connector cannot convey its simulated delivery time.
	if _, err := eng.Store.DB.DB.Exec(`UPDATE chat_turns SET created_at=?, spoken_at=CASE WHEN role='assistant' AND ? THEN ? ELSE spoken_at END WHERE id>?`,
		c.answeredAt.UTC().Format(time.RFC3339), r.noBotPosts, c.answeredAt.UTC().Format(time.RFC3339Nano), before); err != nil {
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
