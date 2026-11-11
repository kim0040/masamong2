package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"masamong/internal/action"
	"masamong/internal/chat"
	"masamong/internal/config"
	"masamong/internal/memory/engine"
	"masamong/locales"
)

// restEvent is one call the bot made to the fake Discord.
type restEvent struct {
	Method, Path string
	Content      string // a post's text
	Flags        int
	Reply        string // the message a post replies to
	Parse        []string
	parseSet     bool
}

// restLog is a fake Discord that numbers the messages posted and keeps every call in order. gate, when set,
// holds the first post until it is closed; failPost refuses the posts it names (by number, from 1).
type restLog struct {
	mu          sync.Mutex
	events      []restEvent
	posts       int
	failPost    map[int]bool
	losePost    map[int]bool // the post was accepted but the REST answer never came back
	failDelete  int          // the first N deletes fail
	deletes     int
	historyBody string
	gate        chan struct{}
	entered     chan struct{}
}

func (r *restLog) RoundTrip(req *http.Request) (*http.Response, error) {
	reply := func(status int, body string) *http.Response {
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}
	}
	switch {
	case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/messages"):
		raw, _ := io.ReadAll(req.Body)
		var sent struct {
			Content string `json:"content"`
			Flags   int    `json:"flags"`
			Ref     *struct {
				MessageID string `json:"message_id"`
			} `json:"message_reference"`
			Mentions *struct {
				Parse []string `json:"parse"`
			} `json:"allowed_mentions"`
		}
		_ = json.Unmarshal(raw, &sent)
		r.mu.Lock()
		r.posts++
		n := r.posts
		ev := restEvent{Method: req.Method, Path: req.URL.Path, Content: sent.Content, Flags: sent.Flags}
		if sent.Ref != nil {
			ev.Reply = sent.Ref.MessageID
		}
		if sent.Mentions != nil {
			ev.parseSet, ev.Parse = true, sent.Mentions.Parse
		}
		fail := r.failPost[n]
		lose := r.losePost[n]
		gate, entered := r.gate, r.entered
		if n == 1 {
			r.gate, r.entered = nil, nil
		} else {
			gate, entered = nil, nil
		}
		r.mu.Unlock()
		if entered != nil {
			close(entered)
		}
		if gate != nil {
			<-gate
		}
		r.mu.Lock()
		r.events = append(r.events, ev)
		r.mu.Unlock()
		if fail {
			return reply(http.StatusForbidden, `{"code":50013,"message":"Missing Permissions"}`), nil
		}
		if lose {
			return nil, context.DeadlineExceeded
		}
		return reply(http.StatusOK, fmt.Sprintf(`{"id":"i%d"}`, n)), nil
	case req.Method == http.MethodDelete:
		r.mu.Lock()
		r.deletes++
		failDelete := r.deletes <= r.failDelete
		r.events = append(r.events, restEvent{Method: req.Method, Path: req.URL.Path})
		r.mu.Unlock()
		if failDelete {
			return reply(http.StatusInternalServerError, `{"message":"delete failed"}`), nil
		}
		return reply(http.StatusNoContent, ""), nil
	case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/messages"):
		r.mu.Lock()
		body := r.historyBody
		r.mu.Unlock()
		if body == "" {
			body = `[]`
		}
		return reply(http.StatusOK, body), nil
	}
	return reply(http.StatusOK, `{}`), nil
}

func (r *restLog) list() []restEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]restEvent(nil), r.events...)
}

// shape renders the calls as "POST text" / "DELETE id" lines for a readable order check.
func (r *restLog) shape(interimTexts []string) []string {
	var out []string
	for _, e := range r.list() {
		switch {
		case e.Method == http.MethodDelete:
			out = append(out, "DELETE "+e.Path[strings.LastIndex(e.Path, "/")+1:])
		case slices.Contains(interimTexts, e.Content):
			out = append(out, "POST interim")
		default:
			out = append(out, "POST other")
		}
	}
	return out
}

// interimTexts lists every line the bot may post for m's locale.
func interimTexts(g *Gateway, m *discordgo.MessageCreate) []string {
	var out []string
	for i := 1; i <= interimVariants; i++ {
		out = append(out, messages.T(g.noticeLocale(m), fmt.Sprintf("discord.chat.interim.%d", i)))
	}
	for i := 1; i <= interimMoreVariants; i++ {
		out = append(out, messages.T(g.noticeLocale(m), fmt.Sprintf("discord.chat.interim_more.%d", i)))
	}
	return out
}

// fakeClock stands in for time.AfterFunc: the test fires what was scheduled.
type fakeClock struct {
	mu      sync.Mutex
	delay   time.Duration
	f       func()
	stopped bool
	timers  int
}

func (c *fakeClock) afterFunc(d time.Duration, f func()) func() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.delay, c.f, c.stopped = d, f, false
	c.timers++
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		was := !c.stopped
		c.stopped = true
		return was
	}
}

func (c *fakeClock) fire() {
	c.mu.Lock()
	f, stopped := c.f, c.stopped
	c.mu.Unlock()
	if f != nil && !stopped {
		f()
	}
}

// interimFixture is a gateway on the fake models with the fake Discord; make builds a turn's line.
func interimFixture(t *testing.T, mode string) (*Gateway, *restLog, *fakeClock) {
	return interimFixtureWith(t, mode, &answerModels{reply: "응, 알겠어."})
}

func interimFixtureWith(t *testing.T, mode string, models *answerModels) (*Gateway, *restLog, *fakeClock) {
	t.Helper()
	log := &restLog{}
	g := answerGateway(t, models, nil)
	g.sess.Client = &http.Client{Transport: log}
	if mode != "" {
		doc := g.svc.Doc()
		doc.Chat.Progress = mode
		applyDoc(t, g, doc)
	}
	return g, log, &fakeClock{}
}

// applyDoc stores doc as the running configuration.
func applyDoc(t *testing.T, g *Gateway, doc config.Document) {
	t.Helper()
	encoded, err := config.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if res := g.svc.Execute(context.Background(), action.Request{Action: "config.apply", Input: mustJSON(map[string]any{
		"yaml": string(encoded), "base_revision": g.svc.Revision(),
	}), Locale: "en"}); !res.OK {
		t.Fatalf("config.apply: %+v", res.Error)
	}
}

func makeInterim(g *Gateway, clock *fakeClock, base context.Context, m *discordgo.MessageCreate) *interim {
	ip := g.newInterim(base, g.sess, m)
	ip.afterFunc = clock.afterFunc
	ip.pick = func(int) int { return 0 }
	return ip
}

// A deep search posts at once, as a silent reply that mentions no one, and the line is deleted before the
// first part of the answer is sent.
func TestInterimIsDeletedBeforeTheAnswerIsSent(t *testing.T) {
	g, log, _ := interimFixture(t, "")
	m := answerMessage("@bot 오늘 환율 알려줘")
	g.interimHook = func(ip *interim) {
		ip.pick = func(int) int { return 0 }
		ip.show(chat.Progress{Stage: chat.ProgressDeep, Round: 1, Searches: 1})
		ip.posts.Wait() // the turn is still going when the line lands
	}
	g.answer(context.Background(), g.sess, m, "오늘 환율 알려줘", turnCall{addressed: true})
	texts := interimTexts(g, m)
	if got, want := log.shape(texts), []string{"POST interim", "DELETE i1", "POST other"}; !slices.Equal(got, want) {
		t.Fatalf("calls %q, want %q", got, want)
	}
	first := log.list()[0]
	if first.Content != texts[0] || first.Flags&int(discordgo.MessageFlagsSuppressNotifications) == 0 || first.Reply != "m1" || !first.parseSet || len(first.Parse) != 0 {
		t.Fatalf("the line is not a silent reply without mentions: %+v", first)
	}
	if !g.interims.deleted("i1") {
		t.Fatal("the line's delete event would not be recognised")
	}
}

// Every way out of a turn takes the line back, and none of them lets an answer follow it.
func TestInterimIsClearedOnEveryExit(t *testing.T) {
	cases := []struct {
		name  string
		setup func(g *Gateway, m *discordgo.MessageCreate, cancelBase func())
		call  turnCall
		model *answerModels
	}{
		{"the turn fails", func(*Gateway, *discordgo.MessageCreate, func()) {}, turnCall{addressed: true}, &answerModels{reply: "x", failAnswer: true}},
		{"the person says stop", func(g *Gateway, m *discordgo.MessageCreate, _ func()) { g.turns.stop(m.ChannelID, m.Author.ID) }, turnCall{addressed: true}, &answerModels{reply: "x"}},
		{"the daemon is stopping", func(g *Gateway, _ *discordgo.MessageCreate, cancelBase func()) { g.stopping.Store(true); cancelBase() }, turnCall{addressed: true}, &answerModels{reply: "x"}},
		{"the name was not a call", func(*Gateway, *discordgo.MessageCreate, func()) {}, turnCall{nameOnly: true}, &answerModels{judge: `{"to":"about_bot","tone":"neutral"}`, reply: "x"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g, log, _ := interimFixtureWith(t, "", c.model)
			m := answerMessage("bot 오늘 뉴스 알려줘")
			base, cancelBase := context.WithCancel(context.Background())
			defer cancelBase()
			g.ctx = base
			g.interimHook = func(ip *interim) {
				ip.show(chat.Progress{Stage: chat.ProgressDeep, Round: 1, Searches: 1})
				ip.posts.Wait()
				c.setup(g, m, cancelBase)
			}
			g.answer(base, g.sess, m, "오늘 뉴스 알려줘", c.call)
			shape := log.shape(interimTexts(g, m))
			posted, deleted := 0, 0
			for _, line := range shape {
				switch {
				case line == "POST interim":
					posted++
				case strings.HasPrefix(line, "DELETE"):
					deleted++
				}
			}
			if posted != 1 || deleted != 1 || shape[1] != "DELETE i1" {
				t.Fatalf("calls %q: the line must be posted once and deleted right after", shape)
			}
		})
	}
}

// In slow mode an ordinary search posts only if the turn is still unanswered when the timer runs out,
// and the line is a first-round one. A second round of searches, in slow mode, uses that same set.
func TestInterimSlowModeWaitsForTheTimer(t *testing.T) {
	g, log, clock := interimFixture(t, "slow")
	m := answerMessage("질문")
	ip := makeInterim(g, clock, context.Background(), m)
	ip.show(chat.Progress{Stage: chat.ProgressSearch, Round: 1, Searches: 1})
	ip.show(chat.Progress{Stage: chat.ProgressSearch, Round: 1, Searches: 2}) // one timer per turn
	if len(log.list()) != 0 || clock.timers != 1 || clock.delay != interimSlowDelay {
		t.Fatalf("a search must only arm one %s timer: events %d, timers %d, delay %v", interimSlowDelay, len(log.list()), clock.timers, clock.delay)
	}
	clock.fire()
	texts := interimTexts(g, m)
	if got := log.shape(texts); !slices.Equal(got, []string{"POST interim"}) || log.list()[0].Content != messages.T(g.noticeLocale(m), "discord.chat.interim.1") {
		t.Fatalf("the unanswered turn must get a first-round line: %q, text %q", got, log.list()[0].Content)
	}
	ip.clear()
	if got := log.shape(texts); !slices.Equal(got, []string{"POST interim", "DELETE i1"}) {
		t.Fatalf("calls %q", got)
	}

	// answered before the timer: nothing is ever posted
	g2, log2, clock2 := interimFixture(t, "slow")
	ip2 := makeInterim(g2, clock2, context.Background(), m)
	ip2.show(chat.Progress{Stage: chat.ProgressSearch, Round: 1, Searches: 1})
	ip2.clear()
	clock2.fire()
	if len(log2.list()) != 0 {
		t.Fatalf("a turn answered in time posted %+v", log2.list())
	}

	// a second round still speaks at once, with a first-round line rather than a "still looking further" one
	g3, log3, _ := interimFixture(t, "slow")
	ip3 := makeInterim(g3, &fakeClock{}, context.Background(), m)
	ip3.show(chat.Progress{Stage: chat.ProgressMore, Round: 2, Searches: 1})
	ip3.posts.Wait()
	if len(log3.list()) != 1 || log3.list()[0].Content != messages.T(g3.noticeLocale(m), "discord.chat.interim.1") {
		t.Fatalf("slow mode posted %+v", log3.list())
	}
}

// A deep stage cancels the pending timer instead of posting twice; "more" posts at once in slow and deep.
func TestInterimModes(t *testing.T) {
	for _, tc := range []struct {
		mode   string
		stage  string
		wantN  int
		timers int
	}{
		{"slow", chat.ProgressDeep, 1, 0},
		{"slow", chat.ProgressMore, 1, 0},
		{"slow", chat.ProgressSearch, 0, 1},
		{"deep", chat.ProgressDeep, 1, 0},
		{"deep", chat.ProgressMore, 1, 0},
		{"deep", chat.ProgressSearch, 0, 0},
		{"off", chat.ProgressDeep, 0, 0},
		{"off", chat.ProgressMore, 0, 0},
		{"off", chat.ProgressSearch, 0, 0},
	} {
		t.Run(tc.mode+"/"+tc.stage, func(t *testing.T) {
			g, log, clock := interimFixture(t, tc.mode)
			ip := makeInterim(g, clock, context.Background(), answerMessage("질문"))
			ip.show(chat.Progress{Stage: tc.stage, Round: 1, Searches: 1})
			ip.posts.Wait()
			if got := len(log.list()); got != tc.wantN || clock.timers != tc.timers {
				t.Fatalf("posts %d (want %d), timers %d (want %d)", got, tc.wantN, clock.timers, tc.timers)
			}
		})
	}

	g, log, clock := interimFixture(t, "slow")
	ip := makeInterim(g, clock, context.Background(), answerMessage("질문"))
	ip.show(chat.Progress{Stage: chat.ProgressSearch})
	ip.show(chat.Progress{Stage: chat.ProgressDeep})
	ip.posts.Wait()
	ip.show(chat.Progress{Stage: chat.ProgressMore})
	ip.posts.Wait()
	clock.fire() // the timer was stopped by the post
	if len(log.list()) != 1 {
		t.Fatalf("one line per turn, got %+v", log.list())
	}
}

// Progress that arrives after the answer started (clear ran in Deliver) posts nothing.
func TestInterimProgressAfterDeliverPostsNothing(t *testing.T) {
	g, log, clock := interimFixture(t, "slow")
	ip := makeInterim(g, clock, context.Background(), answerMessage("질문"))
	ip.clear()
	for _, stage := range []string{chat.ProgressDeep, chat.ProgressMore, chat.ProgressSearch} {
		ip.show(chat.Progress{Stage: stage})
	}
	ip.posts.Wait()
	clock.fire()
	if len(log.list()) != 0 {
		t.Fatalf("a cleared line posted %+v", log.list())
	}
}

// A post still on its way when the answer starts is waited for and deleted: the channel never keeps it.
func TestInterimPostLandingAfterClearDeletesItself(t *testing.T) {
	g, log, clock := interimFixture(t, "slow")
	gate, entered := make(chan struct{}), make(chan struct{})
	log.gate, log.entered = gate, entered
	ip := makeInterim(g, clock, context.Background(), answerMessage("질문"))
	ip.show(chat.Progress{Stage: chat.ProgressDeep})
	<-entered // the post is on its way
	cleared := make(chan struct{})
	go func() { ip.clear(); close(cleared) }()
	select {
	case <-cleared:
		t.Fatal("clear returned while the line was still being sent")
	case <-time.After(50 * time.Millisecond):
	}
	close(gate)
	select {
	case <-cleared:
	case <-time.After(5 * time.Second):
		t.Fatal("clear did not return after the post landed")
	}
	ip.posts.Wait()
	if got := log.shape(interimTexts(g, answerMessage("질문"))); !slices.Equal(got, []string{"POST interim", "DELETE i1"}) {
		t.Fatalf("calls %q", got)
	}
}

// The line failing to go out changes nothing about the answer and is not retried or left registered.
func TestInterimSendFailureLeavesTheAnswerAlone(t *testing.T) {
	g, log, _ := interimFixture(t, "")
	log.failPost = map[int]bool{1: true}
	m := answerMessage("@bot 오늘 환율 알려줘")
	g.interimHook = func(ip *interim) {
		ip.show(chat.Progress{Stage: chat.ProgressDeep})
		ip.posts.Wait()
		ip.show(chat.Progress{Stage: chat.ProgressMore}) // a failed line is not tried again
		ip.posts.Wait()
	}
	g.answer(context.Background(), g.sess, m, "오늘 환율 알려줘", turnCall{addressed: true})
	if got, want := log.shape(interimTexts(g, m)), []string{"POST interim", "POST other"}; !slices.Equal(got, want) {
		t.Fatalf("calls %q, want %q", got, want)
	}
	if answer := log.list()[1].Content; answer != "응, 알겠어." {
		t.Fatalf("the answer was %q", answer)
	}
	g.interims.mu.Lock()
	defer g.interims.mu.Unlock()
	if len(g.interims.pending) != 0 || len(g.interims.ids) != 0 {
		t.Fatalf("a failed line stayed registered: %+v %+v", g.interims.pending, g.interims.ids)
	}
}

// A direct message gets the same line, deleted the same way; its delete event is skipped too.
func TestInterimInDirectMessage(t *testing.T) {
	g, log, clock := interimFixture(t, "slow")
	m := answerMessage("질문")
	m.GuildID = ""
	m.ChannelID = "dm-channel"
	ip := makeInterim(g, clock, context.Background(), m)
	ip.show(chat.Progress{Stage: chat.ProgressDeep})
	ip.posts.Wait()
	ip.clear()
	if got := log.shape(interimTexts(g, m)); !slices.Equal(got, []string{"POST interim", "DELETE i1"}) {
		t.Fatalf("calls %q", got)
	}
	if first := log.list()[0]; !strings.Contains(first.Path, "dm-channel") || first.Reply != "m1" {
		t.Fatalf("the line must reply in the DM channel: %+v", first)
	}
	var applied []messageReconciliation
	g.messageEventApply = func(e messageReconciliation) error { applied = append(applied, e); return nil }
	g.onMessageDelete(nil, &discordgo.MessageDelete{Message: &discordgo.Message{ID: "i1", ChannelID: "dm-channel"}})
	if len(applied) != 0 {
		t.Fatalf("the DM line's delete was reconciled: %+v", applied)
	}
}

// Neither the line's create event nor its delete event reaches memory or the delete reconciliation.
func TestInterimEventsAreNeitherCapturedNorReconciled(t *testing.T) {
	g, log, clock := interimFixture(t, "slow")
	bot := "bot-id"
	g.botID.Store(&bot)
	m := answerMessage("질문")
	var applied []messageReconciliation
	g.messageEventApply = func(e messageReconciliation) error { applied = append(applied, e); return nil }

	ip := makeInterim(g, clock, context.Background(), m)
	ip.show(chat.Progress{Stage: chat.ProgressDeep})
	ip.posts.Wait()
	text := log.list()[0].Content
	if ours, drop := g.interims.ownCreate("c", "i1", text); !ours || drop {
		t.Fatal("the line's own create event is not recognised")
	}
	create := &discordgo.MessageCreate{Message: &discordgo.Message{ID: "i1", ChannelID: "c", GuildID: "g", Content: text, Author: &discordgo.User{ID: bot, Bot: true}}}
	g.onMessage(g.sess, create) // returns before Observe: the channel has not moved on, nothing is stored
	ip.clear()
	g.onMessageDelete(nil, &discordgo.MessageDelete{Message: &discordgo.Message{ID: "i1", ChannelID: "c", GuildID: "g"}})
	if len(applied) != 0 {
		t.Fatalf("the line's delete was reconciled: %+v", applied)
	}
	// the control: a delete of any other message is reconciled
	g.onMessageDelete(nil, &discordgo.MessageDelete{Message: &discordgo.Message{ID: "other", ChannelID: "c", GuildID: "g"}})
	if len(applied) != 1 {
		t.Fatalf("a normal delete must still be reconciled: %+v", applied)
	}
	// and a bulk delete that took the line along drops only the line
	g.interims.mu.Lock()
	g.interims.ids = map[string]time.Time{"i2": time.Now()}
	g.interims.mu.Unlock()
	g.onMessageDeleteBulk(nil, &discordgo.MessageDeleteBulk{Messages: []string{"i2", "x"}, ChannelID: "c", GuildID: "g"})
	if len(applied) != 2 || len(applied[1].MessageIDs) != 1 || applied[1].MessageIDs[0] != "x" {
		t.Fatalf("bulk delete applied %+v", applied)
	}
}

// The create event can arrive before the REST call returns the ID (the registry knows the text then), or
// after; two lines with the same text at once are both recognised; an old entry is forgotten.
func TestInterimRegistry(t *testing.T) {
	t.Run("event first", func(t *testing.T) {
		var r interimRegistry
		r.expect("c", "text")
		if ours, _ := r.ownCreate("c", "id", "text"); !ours {
			t.Fatal("pending text not recognised")
		}
		r.bind("c", "text", "id") // the REST answer comes second
		if ours, _ := r.ownCreate("c", "other", "text"); ours {
			t.Fatal("one expectation matched two messages")
		}
		if !r.deleted("id") || !r.deleted("id") {
			t.Fatal("a second delete of the line must still be swallowed")
		}
	})
	t.Run("answer first", func(t *testing.T) {
		var r interimRegistry
		r.expect("c", "text")
		r.bind("c", "text", "id")
		if ours, _ := r.ownCreate("c", "id", "text"); !ours {
			t.Fatal("bound ID not recognised")
		}
		if ours, _ := r.ownCreate("c", "other", "text"); ours {
			t.Fatal("a bound line left its expectation behind")
		}
	})
	t.Run("same text twice, another channel, another text", func(t *testing.T) {
		var r interimRegistry
		r.expect("c", "text")
		r.expect("c", "text")
		if ours, _ := r.ownCreate("d", "x", "text"); ours {
			t.Fatal("matched the wrong channel")
		}
		if ours, _ := r.ownCreate("c", "x", "other"); ours {
			t.Fatal("matched the wrong text")
		}
		if ours, _ := r.ownCreate("c", "a", "text"); !ours {
			t.Fatal("the first of two lines with one text")
		}
		if ours, _ := r.ownCreate("c", "b", "text"); !ours {
			t.Fatal("the second of two lines with one text")
		}
		if ours, _ := r.ownCreate("c", "c", "text"); ours {
			t.Fatal("two lines with one text matched a third")
		}
	})
	t.Run("old entries are forgotten", func(t *testing.T) {
		var r interimRegistry
		r.expect("c", "text")
		r.bind("c", "other", "id")
		r.mu.Lock()
		r.pending[interimKey("c", "text")] = []interimSlot{{token: 1, at: time.Now().Add(-2 * interimIDTTL)}}
		r.ids["id"] = time.Now().Add(-2 * interimIDTTL)
		r.mu.Unlock()
		if ours, _ := r.ownCreate("c", "id", "text"); ours {
			t.Fatal("an expired entry still matched")
		}
	})
	t.Run("delete before create, and a duplicate delete", func(t *testing.T) {
		var r interimRegistry
		r.expect("c", "text")
		r.bind("c", "text", "id")
		if !r.deleted("id") {
			t.Fatal("the line's delete was not recognised")
		}
		if ours, drop := r.ownCreate("c", "id", "text"); !ours || drop {
			t.Fatal("a create of a deleted line must be ours and not deleted again")
		}
		if !r.deleted("id") {
			t.Fatal("a duplicate delete must be swallowed")
		}
	})
	t.Run("bulk delete before create", func(t *testing.T) {
		var r interimRegistry
		r.expect("c", "text")
		r.bind("c", "text", "id")
		if kept := r.withoutInterims([]string{"id", "x"}); len(kept) != 1 || kept[0] != "x" {
			t.Fatalf("bulk delete kept %v", kept)
		}
		if ours, drop := r.ownCreate("c", "id", "text"); !ours || drop {
			t.Fatal("a create after the bulk delete must still be ours")
		}
		// the tombstone also swallows a later bulk delete
		if kept := r.withoutInterims([]string{"id", "y"}); len(kept) != 1 || kept[0] != "y" {
			t.Fatalf("a second bulk delete kept %v", kept)
		}
	})
	t.Run("a tombstone expires", func(t *testing.T) {
		var r interimRegistry
		r.mu.Lock()
		r.tombstones = map[string]time.Time{"id": time.Now().Add(-2 * interimTombstoneTTL)}
		r.mu.Unlock()
		if ours, _ := r.ownCreate("c", "id", "text"); ours || r.deleted("id") {
			t.Fatal("an expired tombstone matched")
		}
		if kept := r.withoutInterims([]string{"id"}); len(kept) != 1 || kept[0] != "id" {
			t.Fatalf("an expired tombstone was filtered from a bulk delete: %v", kept)
		}
	})
	t.Run("an ambiguous expectation expires", func(t *testing.T) {
		var r interimRegistry
		token := r.expect("c", "text")
		if id := r.lose(token); id != "" {
			t.Fatal("nothing was created yet")
		}
		r.mu.Lock()
		slots := r.pending[interimKey("c", "text")]
		if len(slots) != 1 || !slots[0].ambiguous {
			t.Fatalf("the lost send did not stay registered: %+v", slots)
		}
		slots[0].at = time.Now().Add(-2 * interimAmbiguousTTL)
		r.pending[interimKey("c", "text")] = slots
		r.mu.Unlock()
		if ours, _ := r.ownCreate("c", "late", "text"); ours {
			t.Fatal("an expired lost send still matched")
		}
	})
}

// The catalogs carry the variants in every language, each different from the others of its set.
func TestInterimLinesExistInEveryLanguage(t *testing.T) {
	for _, loc := range []string{"ko", "en", "ja", "es", "zh-Hans"} {
		raw, err := locales.FS.ReadFile(loc + ".json")
		if err != nil {
			t.Fatal(err)
		}
		var cat map[string]string
		if err := json.Unmarshal(raw, &cat); err != nil {
			t.Fatal(err)
		}
		for key, n := range map[string]int{"discord.chat.interim": interimVariants, "discord.chat.interim_more": interimMoreVariants} {
			seen := map[string]bool{}
			for i := 1; i <= n; i++ {
				line := cat[fmt.Sprintf("%s.%d", key, i)]
				if strings.TrimSpace(line) == "" || seen[line] || strings.Contains(line, "@") || strings.Contains(line, "{") {
					t.Errorf("%s %s.%d = %q", loc, key, i, line)
				}
				seen[line] = true
			}
			if _, extra := cat[fmt.Sprintf("%s.%d", key, n+1)]; extra {
				t.Errorf("%s has more %s variants than the code picks from", loc, key)
			}
		}
	}
}

// A deep search's reply is trimmed later and shows more; an ordinary one is trimmed as before.
func TestDeepSearchReplyIsTrimmedLater(t *testing.T) {
	para := strings.Repeat("가", 300)
	text := strings.Join([]string{para, para, para, para, para}, "\n\n") // 1,500 characters
	if _, rest := trimReplyAt(text, trimAt, trimHead); rest == "" {
		t.Fatal("an ordinary reply of this length must be trimmed")
	}
	if head, rest := trimReplyAt(text, trimAtDeep, trimHeadDeep); rest != "" || head != text {
		t.Fatalf("a deep reply of 1,500 characters must be shown whole (rest %d)", len([]rune(rest)))
	}
	long := strings.Join([]string{para, para, para, para, para, para, para}, "\n\n")
	if head, rest := trimReplyAt(long, trimAtDeep, trimHeadDeep); rest == "" || len([]rune(head)) < 900 {
		t.Fatalf("a longer deep reply keeps a longer head: %d / %d", len([]rune(head)), len([]rune(rest)))
	}
	deep := []chat.ToolResult{{Tool: "search.web", OK: true, Data: map[string]any{"depth": "deep"}}}
	standard := []chat.ToolResult{{Tool: "search.web", OK: true, Data: map[string]any{"depth": "standard"}}, {Tool: "weather.query", OK: true}}
	if !deepSearched(deep) || deepSearched(standard) || deepSearched(nil) {
		t.Fatal("deepSearched")
	}
}

// botCreate is a create event for the bot's own message, as the gateway would see it.
func botCreate(channelID, id, text string) *discordgo.MessageCreate {
	return &discordgo.MessageCreate{Message: &discordgo.Message{ID: id, ChannelID: channelID, GuildID: "g", Content: text,
		Author: &discordgo.User{ID: "bot-id", Username: "masamong", Bot: true}}}
}

func armInterimBot(g *Gateway) {
	bot := "bot-id"
	g.botID.Store(&bot)
	g.cfg.MessageContent = true
}

// A send whose REST answer never comes back may still have created the line. The create is deleted
// and not observed, whether it arrives while the call is still out or after the error returns.
func TestInterimAmbiguousSendIsDeletedWhenTheCreateArrives(t *testing.T) {
	line := func(g *Gateway, m *discordgo.MessageCreate) string {
		return messages.T(g.noticeLocale(m), "discord.chat.interim.1")
	}
	t.Run("create before the error", func(t *testing.T) {
		g, log, clock := interimFixture(t, "deep")
		armInterimBot(g)
		m := answerMessage("질문")
		gate, entered := make(chan struct{}), make(chan struct{})
		log.gate, log.entered = gate, entered
		log.losePost = map[int]bool{1: true}
		ip := makeInterim(g, clock, context.Background(), m)
		ip.show(chat.Progress{Stage: chat.ProgressDeep})
		<-entered
		g.onMessage(g.sess, botCreate(m.ChannelID, "made", line(g, m)))
		close(gate)
		ip.posts.Wait()
		if got := log.shape(interimTexts(g, m)); !slices.Equal(got, []string{"POST interim", "DELETE made"}) {
			t.Fatalf("calls %q", got)
		}
		var applied []messageReconciliation
		g.messageEventApply = func(e messageReconciliation) error { applied = append(applied, e); return nil }
		g.onMessageDelete(nil, &discordgo.MessageDelete{Message: &discordgo.Message{ID: "made", ChannelID: m.ChannelID, GuildID: "g"}})
		if len(applied) != 0 {
			t.Fatalf("the line's delete was reconciled: %+v", applied)
		}
	})
	t.Run("create after the error", func(t *testing.T) {
		g, log, clock := interimFixture(t, "deep")
		armInterimBot(g)
		m := answerMessage("질문")
		log.losePost = map[int]bool{1: true}
		ip := makeInterim(g, clock, context.Background(), m)
		ip.show(chat.Progress{Stage: chat.ProgressDeep})
		ip.posts.Wait()
		if len(log.list()) != 1 {
			t.Fatalf("the lost send deleted early: %+v", log.list())
		}
		g.onMessage(g.sess, botCreate(m.ChannelID, "late", line(g, m)))
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) && !slices.Equal(log.shape(interimTexts(g, m)), []string{"POST interim", "DELETE late"}) {
			time.Sleep(10 * time.Millisecond)
		}
		if got := log.shape(interimTexts(g, m)); !slices.Equal(got, []string{"POST interim", "DELETE late"}) {
			t.Fatalf("calls %q", got)
		}
		// a later bot message that is not the line is left alone
		g.onMessage(g.sess, botCreate(m.ChannelID, "answer", "응, 알겠어."))
		time.Sleep(30 * time.Millisecond)
		if got := log.shape(interimTexts(g, m)); !slices.Equal(got, []string{"POST interim", "DELETE late"}) {
			t.Fatalf("a normal bot message was deleted: %q", got)
		}
	})
}

// A delete Discord does not accept is tried once more, after a short pause.
func TestInterimDeleteIsRetriedOnce(t *testing.T) {
	g, log, clock := interimFixture(t, "deep")
	log.failDelete = 1
	m := answerMessage("질문")
	ip := makeInterim(g, clock, context.Background(), m)
	var waited time.Duration
	ip.retryWait = func(d time.Duration) { waited = d }
	ip.show(chat.Progress{Stage: chat.ProgressDeep})
	ip.posts.Wait()
	ip.clear()
	if waited != interimDeleteRetry {
		t.Fatalf("retry pause %s", waited)
	}
	if got := log.shape(interimTexts(g, m)); !slices.Equal(got, []string{"POST interim", "DELETE i1", "DELETE i1"}) {
		t.Fatalf("calls %q", got)
	}
}

// discordSnowflake is the smallest message ID Discord could assign at t.
func discordSnowflake(t time.Time) string {
	ms := t.UnixMilli() - 1420070400000
	if ms < 0 {
		ms = 0
	}
	return strconv.FormatUint(uint64(ms)<<22, 10)
}

// Backfill reads channel history, and a "still looking" line can be sitting in that history. The
// catch-up stores no bot message (backfill.go skips m.Bot), so the line is not captured. A person's
// message that happens to use the same words is stored.
func TestBackfillDoesNotCaptureAnInterimLine(t *testing.T) {
	g, log, _ := interimFixture(t, "deep")
	doc := g.svc.Doc()
	doc.Memory.Capture = true
	doc.Discord.MessageContent = true
	gs := doc.Guilds["g"]
	ch := gs.Channels["c"]
	ch.Capture = true
	gs.Channels["c"] = ch
	doc.Guilds["g"] = gs
	applyDoc(t, g, doc)
	g.connected.Store(true)
	g.svc.Discord = g

	now := time.Now().UTC()
	anchor := discordSnowflake(now.Add(-time.Hour))
	lineID := discordSnowflake(now.Add(-30 * time.Minute))
	personID := discordSnowflake(now.Add(-20 * time.Minute))
	quoteID := discordSnowflake(now.Add(-10 * time.Minute))
	line := messages.T("ko", "discord.chat.interim.1")
	eng, err := g.svc.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", MessageID: anchor, AuthorID: "u1", AuthorName: "yeonghui", Content: "먼저", At: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	msg := func(id, author, content string, bot bool, at time.Time) string {
		return fmt.Sprintf(`{"id":%q,"channel_id":"c","content":%q,"timestamp":%q,"author":{"id":%q,"username":"n","bot":%t},"attachments":[],"mention_roles":[]}`,
			id, content, at.Format(time.RFC3339Nano), author, bot)
	}
	log.historyBody = "[" + strings.Join([]string{
		msg(quoteID, "u1", line, false, now.Add(-10*time.Minute)),
		msg(personID, "u1", "이어서", false, now.Add(-20*time.Minute)),
		msg(lineID, "bot-id", line, true, now.Add(-30*time.Minute)),
		msg(anchor, "u1", "먼저", false, now.Add(-time.Hour)),
	}, ",") + "]"

	rep, err := g.svc.Backfill(context.Background(), "test", "g", "c")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Stored != 2 {
		t.Fatalf("stored %d, want the person's two messages and not the line: %+v", rep.Stored, rep)
	}
	msgs, err := eng.Store.RecentMessages(engine.RoomKey("g", "c"), 20)
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, m := range msgs {
		texts = append(texts, m.Content)
	}
	if !slices.Equal(texts, []string{"먼저", "이어서", line}) {
		t.Fatalf("room holds %q", texts)
	}
}
