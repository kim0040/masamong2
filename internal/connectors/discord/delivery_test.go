package discord

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/gorilla/websocket"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/config"
)

// recordedCall is one REST request the fake Discord saw: its method and path, and whether its context
// had already expired or carried a deadline when it was sent.
type recordedCall struct {
	method, path string
	deadline     time.Duration // time left on the request context, 0 if it had none
	body         string        // the request body, for tests that check what was said
}

// fakeDiscord answers every REST call with success and records them. Like net/http's transport it
// refuses a call whose context is already done, so a missing record means a lost message.
type fakeDiscord struct {
	mu        sync.Mutex
	calls     []recordedCall
	responses map[string]fakeDiscordResponse
}

type fakeDiscordResponse struct {
	status int
	body   string
}

func (f *fakeDiscord) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		return nil, err // what net/http's transport does with a request whose context is done
	}
	call := recordedCall{method: req.Method, path: req.URL.Path}
	if req.Body != nil {
		raw, _ := io.ReadAll(req.Body)
		call.body = string(raw)
	}
	if d, ok := req.Context().Deadline(); ok {
		call.deadline = time.Until(d)
	}
	f.mu.Lock()
	f.calls = append(f.calls, call)
	response, overridden := fakeDiscordResponse{}, false
	for suffix, candidate := range f.responses {
		if strings.HasSuffix(req.URL.Path, suffix) {
			response, overridden = candidate, true
			break
		}
	}
	f.mu.Unlock()
	body := `{"id":"1"}`
	status := http.StatusOK
	if strings.HasSuffix(req.URL.Path, "/callback") {
		body, status = "", http.StatusNoContent
	}
	if overridden {
		body, status = response.body, response.status
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
}

func (f *fakeDiscord) setResponse(pathSuffix string, status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.responses == nil {
		f.responses = map[string]fakeDiscordResponse{}
	}
	f.responses[pathSuffix] = fakeDiscordResponse{status: status, body: body}
}

func (f *fakeDiscord) find(method, pathSuffix string) (recordedCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c.method == method && strings.HasSuffix(c.path, pathSuffix) {
			return c, true
		}
	}
	return recordedCall{}, false
}

func gatewayWithFakeDiscord(t *testing.T) (*Gateway, *fakeDiscord) {
	t.Helper()
	svc := svcForTest(t)
	SetCatalog(svc.I18n)
	sess, err := discordgo.New("Bot test")
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeDiscord{}
	sess.Client = &http.Client{Transport: fake}
	g := &Gateway{svc: svc, sess: sess, log: slog.New(slog.NewTextHandler(io.Discard, nil)), ctx: context.Background(),
		ActionTimeout: -time.Second, notices: map[string]time.Time{}}
	return g, fake
}

// A command that used up its action timeout must still show the member why: the error card is sent on
// a fresh short context, not on the spent one.
func TestSlashCommandAnswersAfterItsActionTimedOut(t *testing.T) {
	g, fake := gatewayWithFakeDiscord(t)
	i := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{ID: "i1", AppID: "app", Token: "tok", Type: discordgo.InteractionApplicationCommand,
		ChannelID: "c1", GuildID: "g1", Data: discordgo.ApplicationCommandInteractionData{Name: "help"}}}
	g.handleCommand(g.sess, i)
	if !g.Wait(5 * time.Second) {
		t.Fatal("command work did not finish")
	}
	edit, ok := fake.find(http.MethodPatch, "/messages/@original")
	if !ok {
		t.Fatalf("the final edit never reached Discord (a spent context drops it): %+v", fake.calls)
	}
	if edit.deadline <= 0 || edit.deadline > finalDeliveryTimeout {
		t.Fatalf("final edit deadline %v, want a fresh window of at most %v", edit.deadline, finalDeliveryTimeout)
	}
}

func TestPrefixCommandAnswersAfterItsActionTimedOut(t *testing.T) {
	g, fake := gatewayWithFakeDiscord(t)
	cmd, _ := ByName("help")
	m := &discordgo.MessageCreate{Message: &discordgo.Message{ID: "m1", ChannelID: "c1", GuildID: "g1", Author: &discordgo.User{ID: "u1", Username: "u"}}}
	g.runCommand(g.sess, m, cmd, nil)
	if _, ok := fake.find(http.MethodPost, "/channels/c1/messages"); !ok {
		t.Fatalf("a timed-out prefix command posted nothing: %+v", fake.calls)
	}
}

func TestFinalDeliveryStopsWithTheDaemon(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	g := &Gateway{ctx: ctx}
	final, done := g.finalContext(g.runCtx())
	defer done()
	cancel()
	if final.Err() == nil {
		t.Fatal("the final delivery must be cancelled when the daemon stops")
	}
}

func TestRateRepliesAreBoundedByAContext(t *testing.T) {
	g, fake := gatewayWithFakeDiscord(t)
	i := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{ID: "i1", Token: "tok", Type: discordgo.InteractionApplicationCommand,
		Data: discordgo.ApplicationCommandInteractionData{Name: rateCommandName, CommandType: discordgo.MessageApplicationCommand, TargetID: "m9"}}}
	g.handleRate(g.sess, i)
	call, ok := fake.find(http.MethodPost, "/callback")
	if !ok || call.deadline <= 0 || call.deadline > 3*time.Second {
		t.Fatalf("rate respond must carry the 3 s interaction deadline: %+v ok=%v", call, ok)
	}
	fake.calls = nil
	i.Interaction.Type = discordgo.InteractionMessageComponent
	i.Interaction.Data = discordgo.MessageComponentInteractionData{CustomID: "fb:up:m9"}
	g.handleRateComponent(g.sess, i, []string{"fb", "up", "m9"})
	if call, ok := fake.find(http.MethodPost, "/callback"); !ok || call.deadline <= 0 || call.deadline > 3*time.Second {
		t.Fatalf("rate button update must carry the 3 s interaction deadline: %+v ok=%v", call, ok)
	}
}

func TestPermanentOpenErrorReadsTheTypedErrorChain(t *testing.T) {
	const token = "tok-secret-123"
	rest := &discordgo.RESTError{Response: &http.Response{StatusCode: 401}}
	for _, tc := range []struct {
		name      string
		err       error
		permanent bool
	}{
		{"unauthorized", discordgo.ErrUnauthorized, true},
		{"REST 401", rest, true},
		{"REST 500", &discordgo.RESTError{Response: &http.Response{StatusCode: 500}}, false},
		{"authentication failed", &websocket.CloseError{Code: 4004}, true},
		{"invalid shard", &websocket.CloseError{Code: 4010}, true},
		{"sharding required", &websocket.CloseError{Code: 4011}, true},
		{"invalid API version", &websocket.CloseError{Code: 4012}, true},
		{"invalid intents", &websocket.CloseError{Code: 4013}, true},
		{"disallowed intents", &websocket.CloseError{Code: 4014, Text: "Disallowed intent(s)."}, true},
		{"abnormal closure", &websocket.CloseError{Code: 1006}, false},
		{"rate limited", &websocket.CloseError{Code: 4008}, false},
		{"network", errors.New("dial tcp: lookup discord.com: no such host"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// the way connect() wraps what Open returned, with the token redacted
			err := fmt.Errorf("discord open: %w", redactToken(fmt.Errorf("gateway %s: %w", token, tc.err), token))
			if strings.Contains(err.Error(), token) {
				t.Fatalf("token leaked into %q", err)
			}
			if got := permanentOpenError(err); got != tc.permanent {
				t.Fatalf("permanent = %v, want %v", got, tc.permanent)
			}
		})
	}
	var closeErr *websocket.CloseError
	err := fmt.Errorf("discord open: %w", redactToken(fmt.Errorf("x %s: %w", token, &websocket.CloseError{Code: 4014}), token))
	if !errors.As(err, &closeErr) || closeErr.Code != 4014 {
		t.Fatal("redacting the token must keep the error chain")
	}
}

func TestMissingMessageContentIntentStopsStartupRetries(t *testing.T) {
	calls := 0
	g := &Gateway{ctx: context.Background(), log: slog.New(slog.NewTextHandler(io.Discard, nil)), token: "t",
		open:      func() error { calls++; return &websocket.CloseError{Code: 4014, Text: "Disallowed intent(s)."} },
		retryWait: func(context.Context, time.Duration) bool { t.Fatal("a refused intent was retried"); return false }}
	g.connect()
	if s := g.StartupStatus(); calls != 1 || s.NextRetry != "" || s.LastError == "" {
		t.Fatalf("calls %d status %+v", calls, s)
	}
}

func TestStartupIsConnectingUntilReady(t *testing.T) {
	svc := svcForTest(t)
	g := &Gateway{svc: svc, ctx: context.Background(), log: slog.New(slog.NewTextHandler(io.Discard, nil)), token: "t"}
	release := make(chan struct{})
	g.open = func() error { <-release; return nil }
	g.setStartupStatus(application.DiscordStartupStatus{Connecting: true})
	if !g.StartupStatus().Connecting {
		t.Fatal("the first attempt in flight is connecting")
	}
	done := make(chan struct{})
	go func() { g.connect(); close(done) }()
	close(release)
	<-done
	// Open returned but READY has not been handled yet
	if s := g.StartupStatus(); !s.Connecting || s.Attempts != 1 {
		t.Fatalf("between Open and READY: %+v", s)
	}
	g.onReady(&discordgo.Session{}, &discordgo.Ready{User: &discordgo.User{ID: "bot", Username: "m"}})
	if s := g.StartupStatus(); s.Connecting || !g.Connected() {
		t.Fatalf("after READY: %+v connected=%v", s, g.Connected())
	}
	// READY handled before Open returned: nothing stays pending
	g2 := &Gateway{svc: svc, ctx: context.Background(), log: g.log, token: "t"}
	g2.open = func() error {
		g2.onReady(&discordgo.Session{}, &discordgo.Ready{User: &discordgo.User{ID: "bot", Username: "m"}})
		return nil
	}
	g2.connect()
	if s := g2.StartupStatus(); s.Connecting {
		t.Fatalf("READY first, then Open returns: %+v", s)
	}
}

func TestConnectIsCountedAndReadyIsIgnoredAfterShutdown(t *testing.T) {
	svc := svcForTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	g := &Gateway{svc: svc, ctx: ctx, log: slog.New(slog.NewTextHandler(io.Discard, nil)), token: "t"}
	release := make(chan struct{})
	g.open = func() error { <-release; return nil }
	g.startConnect()
	if g.Wait(30 * time.Millisecond) {
		t.Fatal("shutdown must wait for a handshake in progress")
	}
	cancel()
	close(release)
	if !g.Wait(5 * time.Second) {
		t.Fatal("connect did not finish after it was released")
	}
	g.onReady(&discordgo.Session{}, &discordgo.Ready{User: &discordgo.User{ID: "bot", Username: "m"}})
	if g.Connected() {
		t.Fatal("READY after shutdown started must not seed servers or mark the gateway connected")
	}
}

type blockingLifecycleHandler struct {
	message string
	entered chan struct{}
	release chan struct{}
}

func (h *blockingLifecycleHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *blockingLifecycleHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Message == h.message {
		close(h.entered)
		<-h.release
	}
	return nil
}
func (h *blockingLifecycleHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *blockingLifecycleHandler) WithGroup(string) slog.Handler      { return h }

func TestReadyAndResumedHandlersAreIncludedInShutdownDrain(t *testing.T) {
	svc := svcForTest(t)
	for _, tc := range []struct {
		name    string
		message string
		handle  func(*Gateway)
	}{
		{name: "READY", message: "discord ready", handle: func(g *Gateway) {
			g.onReady(&discordgo.Session{}, &discordgo.Ready{User: &discordgo.User{ID: "bot", Username: "test"}})
		}},
		{name: "RESUMED", message: "discord resumed", handle: func(g *Gateway) {
			g.onResumed(&discordgo.Session{}, &discordgo.Resumed{})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			h := &blockingLifecycleHandler{message: tc.message, entered: make(chan struct{}), release: make(chan struct{})}
			g := &Gateway{svc: svc, ctx: ctx, log: slog.New(h)}
			finished := make(chan struct{})
			go func() { tc.handle(g); close(finished) }()
			<-h.entered
			cancel()
			if g.Wait(20 * time.Millisecond) {
				t.Fatal("shutdown reported drained while the lifecycle handler was still active")
			}
			close(h.release)
			<-finished
			if !g.Wait(time.Second) {
				t.Fatal("shutdown did not drain after the lifecycle handler finished")
			}
		})
	}
}

func botMessage(id, content string, mentions ...string) *discordgo.MessageCreate {
	m := &discordgo.MessageCreate{Message: &discordgo.Message{ID: id, ChannelID: "c1", GuildID: "g1", Content: content, Author: &discordgo.User{ID: "u1", Username: "kim"}}}
	for _, u := range mentions {
		m.Mentions = append(m.Mentions, &discordgo.User{ID: u})
	}
	return m
}

func TestOnlyAddressedTurnsGetRefusalNotices(t *testing.T) {
	svc := svcForTest(t)
	SetCatalog(svc.I18n)
	g := &Gateway{svc: svc, log: slog.New(slog.NewTextHandler(io.Discard, nil)), notices: map[string]time.Time{}}
	bot := "bot"
	g.botID.Store(&bot)

	if g.addressesBot([]*discordgo.MessageCreate{botMessage("m1", "lunch anyone?")}) {
		t.Fatal("a plain message in an answer-everything room did not address the bot")
	}
	if !g.addressesBot([]*discordgo.MessageCreate{botMessage("m1", "hi", "bot")}) {
		t.Fatal("a mention addresses the bot")
	}
	reply := botMessage("m2", "yes")
	reply.ReferencedMessage = &discordgo.Message{Author: &discordgo.User{ID: "bot"}}
	if !g.addressesBot([]*discordgo.MessageCreate{botMessage("m1", "first"), reply}) {
		t.Fatal("a reply to the bot addresses it, even when it is not the last of the held messages")
	}

	var sent int
	send := func(context.Context, *discordgo.MessageSend) error { sent++; return nil }
	g.sendChatNoticeWith(context.Background(), botMessage("m3", "lunch?"), "discord.chat.limited", false, send)
	if sent != 0 {
		t.Fatal("a turn the bot started on its own must not be answered with a refusal")
	}
	g.sendChatNoticeWith(context.Background(), botMessage("m4", "hi", "bot"), "discord.chat.limited", true, send)
	if sent != 1 {
		t.Fatalf("an addressed turn gets its notice, sent=%d", sent)
	}
}

func TestRefusalNoticesAreCappedPerChannel(t *testing.T) {
	svc := svcForTest(t)
	SetCatalog(svc.I18n)
	g := &Gateway{svc: svc, log: slog.New(slog.NewTextHandler(io.Discard, nil)), notices: map[string]time.Time{}}
	var sent int
	send := func(context.Context, *discordgo.MessageSend) error { sent++; return nil }
	for i := 0; i < 40; i++ { // forty different people address the bot in one channel
		m := botMessage(fmt.Sprintf("m%d", i), "hi", "bot")
		m.Author = &discordgo.User{ID: fmt.Sprintf("u%d", i)}
		g.sendChatNoticeWith(context.Background(), m, "discord.chat.paused", true, send)
	}
	if sent != 1 {
		t.Fatalf("one channel got %d notices within a minute, want 1", sent)
	}
	other := botMessage("x", "hi", "bot")
	other.ChannelID = "c2"
	g.sendChatNoticeWith(context.Background(), other, "discord.chat.paused", true, send)
	if sent != 2 {
		t.Fatal("another channel has its own allowance")
	}
	// a minute later another person is heard again, the first person still waits out the 5 minutes
	g.noticeMu.Lock()
	for k := range g.notices {
		if !strings.Contains(k, ":") {
			g.notices[k] = time.Now().Add(-channelNoticeCooldown - time.Second)
		}
	}
	g.noticeMu.Unlock()
	again := botMessage("y", "hi", "bot")
	again.Author = &discordgo.User{ID: "u0"}
	g.sendChatNoticeWith(context.Background(), again, "discord.chat.paused", true, send)
	if sent != 2 {
		t.Fatal("the same person is limited to one notice per five minutes")
	}
	fresh := botMessage("z", "hi", "bot")
	fresh.Author = &discordgo.User{ID: "u99"}
	g.sendChatNoticeWith(context.Background(), fresh, "discord.chat.paused", true, send)
	if sent != 3 {
		t.Fatal("after the channel window another person is heard")
	}
}

func TestHelpOffersOnlyWhatCanRun(t *testing.T) {
	svc := svcForTest(t)
	SetCatalog(svc.I18n)
	withoutWorldWeather(t, svc)
	help := func(cfg config.Discord) string {
		g := &Gateway{svc: svc, cfg: cfg}
		return g.renderCommand(action.Result{OK: true, Action: "help.version"}, Command{Name: "help", Action: "help.version"}, "ko", false).Embed.Description
	}
	both := help(config.Discord{CommandPrefix: "!", MessageContent: true, RegisterCommands: true})
	for _, want := range []string{"**슬래시 명령**", "**접두 명령**", "/poll", "!도움말"} {
		if !strings.Contains(both, want) {
			t.Errorf("help with both surfaces misses %q: %s", want, both)
		}
	}
	noIntent := help(config.Discord{CommandPrefix: "!", MessageContent: false, RegisterCommands: true})
	if strings.Contains(noIntent, "!도움말") || strings.Contains(noIntent, "접두 명령") || !strings.Contains(noIntent, "/poll") {
		t.Errorf("without the message-content intent \"!\" commands cannot run and must not be listed: %s", noIntent)
	}
	noSlash := help(config.Discord{CommandPrefix: "!", MessageContent: true, RegisterCommands: false})
	if strings.Contains(noSlash, "/poll") || strings.Contains(noSlash, "슬래시 명령") || !strings.Contains(noSlash, "!도움말") {
		t.Errorf("slash commands that are not registered must not be listed: %s", noSlash)
	}
	noPrefix := help(config.Discord{CommandPrefix: "", MessageContent: true, RegisterCommands: true})
	if strings.Contains(noPrefix, "접두 명령") {
		t.Errorf("an empty prefix turns the bridge off: %s", noPrefix)
	}
}

func TestErrorDetailsSurviveWhenTheyAreNotPathsOrSecrets(t *testing.T) {
	svc := svcForTest(t)
	SetCatalog(svc.I18n)
	cmd, _ := ByName("weather")
	fail := func(region string) Rendered {
		return Render(action.Result{OK: false, Action: cmd.Action, Error: &action.Error{Code: "INVALID_INPUT", MessageKey: "weather.place_unknown",
			Args: map[string]any{"region": region}}}, cmd, "en")
	}
	for _, kept := range []string{"USD/KRW", "서울 강남구", "Sankt Pölten", "A/B"} {
		if d := fail(kept).Embed.Description; !strings.Contains(d, "'"+kept+"'") {
			t.Errorf("%q must survive in %q", kept, d)
		}
	}
	generic := messages.T("en", "generic.error")
	for _, hidden := range []string{`C:\Users\me\masamong.db`, `C:/Users/me/x`, `\\server\share`, "/etc/masamong/config.yaml", "~/.masamong/data",
		"sk-proj-0123456789abcdefghijklmnopqrstuvwxyz", "my token is abc"} {
		d := fail(hidden).Embed.Description
		if strings.Contains(d, hidden) || strings.Contains(d, "''") || !strings.Contains(d, generic) {
			t.Errorf("%q must fall back to the generic line, not %q", hidden, d)
		}
	}
}

func TestOnlyAdministratorsSeeTheErrorCode(t *testing.T) {
	svc := svcForTest(t)
	SetCatalog(svc.I18n)
	g := &Gateway{svc: svc}
	if res := svc.Execute(context.Background(), action.Request{Action: "access.policy", Input: mustJSON(map[string]any{"operators": []string{"1234567890"}})}); !res.OK {
		t.Fatalf("operators: %+v", res.Error)
	}
	cmd, _ := ByName("weather")
	fail := action.Result{OK: false, Action: cmd.Action, Error: &action.Error{Code: "PROVIDER_UNAVAILABLE", MessageKey: "generic.error"}}
	if r := g.renderInvoked(fail, cmd, InvokeContext{UserID: "1234567890", GuildID: "g1", Locale: "en"}); r.Embed.Footer == nil || r.Embed.Footer.Text != "PROVIDER_UNAVAILABLE" {
		t.Fatalf("an operator sees the code: %+v", r.Embed.Footer)
	}
	if r := g.renderInvoked(fail, cmd, InvokeContext{UserID: "someone", GuildID: "g1", Locale: "en"}); r.Embed.Footer != nil {
		t.Fatalf("a member must not see the code: %+v", r.Embed.Footer)
	}
}

// Refusal notices tell a member only that the bot cannot answer now: nothing about a monthly budget,
// a limit or a reset, in any language.
func TestRefusalNoticesStayNeutral(t *testing.T) {
	svc := svcForTest(t)
	SetCatalog(svc.I18n)
	banned := map[string][]string{
		"en":      {"month", "budget", "limit"},
		"ko":      {"이번 달", "한도", "예산", "횟수"},
		"ja":      {"今月", "上限", "予算", "回数"},
		"zh-Hans": {"本月", "额度", "上限", "预算", "次数"},
		"es":      {"mes", "límite", "presupuesto"},
	}
	for locale, words := range banned {
		for _, key := range []string{"discord.chat.paused", "discord.chat.busy", "discord.chat.limited", "discord.chat.budget"} {
			text, ok := messages.Lookup(locale, key)
			if !ok || text == "" {
				t.Errorf("%s has no %s", locale, key)
				continue
			}
			for _, word := range words {
				if strings.Contains(strings.ToLower(text), word) {
					t.Errorf("%s %s discloses %q: %s", locale, key, word, text)
				}
			}
		}
	}
}
