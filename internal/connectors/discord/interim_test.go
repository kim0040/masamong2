package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"masamong/internal/action"
	"masamong/internal/chat"
	"masamong/internal/config"
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
	mu       sync.Mutex
	events   []restEvent
	posts    int
	failPost map[int]bool
	gate     chan struct{}
	entered  chan struct{}
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
		return reply(http.StatusOK, fmt.Sprintf(`{"id":"i%d"}`, n)), nil
	case req.Method == http.MethodDelete:
		r.mu.Lock()
		r.events = append(r.events, restEvent{Method: req.Method, Path: req.URL.Path})
		r.mu.Unlock()
		return reply(http.StatusNoContent, ""), nil
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

// In slow mode an ordinary search posts only if the turn is still unanswered when the timer runs out.
func TestInterimSlowModeWaitsForTheTimer(t *testing.T) {
	g, log, clock := interimFixture(t, "slow")
	m := answerMessage("질문")
	ip := makeInterim(g, clock, context.Background(), m)
	ip.show(chat.Progress{Stage: chat.ProgressSearch, Round: 1, Searches: 1})
	ip.show(chat.Progress{Stage: chat.ProgressSearch, Round: 1, Searches: 2}) // one timer per turn
	if len(log.list()) != 0 || clock.timers != 1 || clock.delay != 8*time.Second {
		t.Fatalf("a search must only arm one 8 s timer: events %d, timers %d, delay %v", len(log.list()), clock.timers, clock.delay)
	}
	clock.fire()
	texts := interimTexts(g, m)
	if got := log.shape(texts); !slices.Equal(got, []string{"POST interim"}) || log.list()[0].Content != messages.T(g.noticeLocale(m), "discord.chat.interim_more.1") {
		t.Fatalf("the unanswered turn must get the second-set line: %q", got)
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
	if !g.interims.ownCreate("c", "i1", text) {
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
		if !r.ownCreate("c", "id", "text") {
			t.Fatal("pending text not recognised")
		}
		r.bind("c", "text", "id") // the REST answer comes second
		if r.ownCreate("c", "other", "text") {
			t.Fatal("one expectation matched two messages")
		}
		if !r.deleted("id") || r.deleted("id") {
			t.Fatal("the delete is skipped once")
		}
	})
	t.Run("answer first", func(t *testing.T) {
		var r interimRegistry
		r.expect("c", "text")
		r.bind("c", "text", "id")
		if !r.ownCreate("c", "id", "text") {
			t.Fatal("bound ID not recognised")
		}
		if r.ownCreate("c", "other", "text") {
			t.Fatal("a bound line left its expectation behind")
		}
	})
	t.Run("same text twice, another channel, another text", func(t *testing.T) {
		var r interimRegistry
		r.expect("c", "text")
		r.expect("c", "text")
		if r.ownCreate("d", "x", "text") || r.ownCreate("c", "x", "other") {
			t.Fatal("matched the wrong channel or text")
		}
		if !r.ownCreate("c", "a", "text") || !r.ownCreate("c", "b", "text") || r.ownCreate("c", "c", "text") {
			t.Fatal("two lines with one text")
		}
	})
	t.Run("old entries are forgotten", func(t *testing.T) {
		var r interimRegistry
		r.expect("c", "text")
		r.bind("c", "other", "id")
		r.mu.Lock()
		r.pending[interimKey("c", "text")] = interimPending{n: 1, at: time.Now().Add(-2 * interimIDTTL)}
		r.ids["id"] = time.Now().Add(-2 * interimIDTTL)
		r.mu.Unlock()
		if r.ownCreate("c", "id", "text") {
			t.Fatal("an expired entry still matched")
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
