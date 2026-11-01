package discord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/chat"
	"masamong/internal/clock"
	"masamong/internal/config"
	"masamong/internal/errclass"
)

func svcForTest(t *testing.T) *application.Service {
	t.Helper()
	svc, err := application.Open(t.TempDir(), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	if res := svc.Execute(context.Background(), action.Request{Action: "setup.resume"}); !res.OK {
		t.Fatalf("setup %+v", res)
	}
	return svc
}

func TestGatewayCountsOnlySuccessfulBanterReactions(t *testing.T) {
	svc := gatewayBanterService(t)
	sess, err := discordgo.New("Bot test")
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeDiscord{}
	sess.Client = &http.Client{Transport: fake}
	g := &Gateway{svc: svc, sess: sess, log: slog.New(slog.NewTextHandler(io.Discard, nil)), ctx: context.Background()}
	for _, id := range []string{"line-1", "line-2"} {
		res, err := gatewayBanterTurn(svc, id)
		if err != nil || res.Answer == "" {
			t.Fatal("expected a delivered banter line")
		}
	}

	fake.setResponse("/@me", http.StatusInternalServerError, `{}`)
	failed, err := gatewayBanterTurn(svc, "failed-reaction")
	var react application.ReactOnly
	if !errors.As(err, &react) {
		t.Fatal("expected a reaction response")
	}
	g.sendBanterReaction(context.Background(), sess, "c", "failed-message", react)
	if failed.Answer != "" {
		t.Fatal("a reaction-only turn should not contain a text reply")
	}

	fake.setResponse("/@me", http.StatusNoContent, "")
	for i := 0; i < 4; i++ {
		_, err := gatewayBanterTurn(svc, "successful-reaction-"+strconv.Itoa(i))
		if !errors.As(err, &react) || react.Emoji == "" {
			t.Fatal("a failed Discord reaction consumed an emoji slot")
		}
		g.sendBanterReaction(context.Background(), sess, "c", "successful-message-"+strconv.Itoa(i), react)
	}
	if _, err := gatewayBanterTurn(svc, "after-successful-reaction-cap"); !errors.Is(err, application.ErrNotAddressed) {
		t.Fatal("successful Discord reactions did not consume the emoji cap")
	}
	if _, ok := fake.find(http.MethodPut, "/@me"); !ok {
		t.Fatal("the fake Discord session did not receive reaction requests")
	}
}

func gatewayBanterService(t *testing.T) *application.Service {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var request struct {
			Messages []struct{ Content string } `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.Messages) == 0 {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		prompt := request.Messages[len(request.Messages)-1].Content
		answer := "reply"
		if strings.Contains(prompt, "about_bot") {
			answer = `{"to":"about_bot","tone":"playful"}`
		} else if strings.Contains(prompt, "tool_plan") {
			answer = `{"memory_requirement":"none","tool_plan":[]}`
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": answer}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "cost": 0.00001},
		})
	}))
	t.Cleanup(srv.Close)
	svc := svcForTest(t)
	policy := mustJSON(map[string]any{"server_approval": false})
	if res := svc.Execute(context.Background(), action.Request{Action: "access.policy", Input: policy}); !res.OK {
		t.Fatal("could not disable server approval for the fixture")
	}
	t.Setenv("FAKE_GATEWAY_MODEL_KEY", "test")
	raw, err := config.Marshal(svc.Doc())
	if err != nil {
		t.Fatal("could not copy config")
	}
	doc, err := config.Parse(raw)
	if err != nil {
		t.Fatal("could not parse config copy")
	}
	doc.Providers["gateway-test"] = config.Provider{Kind: config.KindOpenAI, BaseURL: srv.URL, CredentialRef: "env:FAKE_GATEWAY_MODEL_KEY"}
	doc.Models["planner"] = config.Model{Provider: "gateway-test", Model: "shared/model"}
	doc.Models["response"] = config.Model{Provider: "gateway-test", Model: "shared/model"}
	doc.Memory.Retrieve = "off"
	doc.Chat.ToolMode = "planner"
	doc.Guilds = map[string]config.GuildSettings{"g": {Channels: map[string]config.ChannelPolicy{"c": {
		Respond: "mention", NameCall: "addressed", Conversation: config.ChannelConversation{Banter: "on"},
	}}}}
	encoded, err := config.Marshal(doc)
	if err != nil {
		t.Fatal("could not encode test config")
	}
	res := svc.Execute(context.Background(), action.Request{Action: "config.apply", Input: mustJSON(map[string]any{
		"yaml": string(encoded), "base_revision": svc.Revision(),
	}), Locale: "en"})
	if !res.OK {
		t.Fatal("could not configure the local model")
	}
	return svc
}

func gatewayBanterTurn(svc *application.Service, id string) (chat.Result, error) {
	return svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: id,
		Message: "playful mention", Speaker: "speaker", SpeakerID: "speaker", NameOnly: true,
		Deliver: func(_ context.Context, r chat.Result) string { return r.Answer }})
}

// withoutWorldWeather switches the keyless worldwide source off, so weather has no source at all
// (the KMA key is not set in tests either).
func withoutWorldWeather(t *testing.T, svc *application.Service) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"ops": []any{map[string]any{"path": []any{"providers", "openmeteo", "disabled"}, "value": true}}})
	if res := svc.Execute(context.Background(), action.Request{Action: "config.patch", Input: raw}); !res.OK {
		t.Fatalf("disable open-meteo %+v", res.Error)
	}
}

func TestStartWithoutTokenIsNotFatal(t *testing.T) {
	svc := svcForTest(t)
	t.Setenv("MASAMONG_DISCORD_TOKEN", "")
	_, err := Start(context.Background(), svc, config.Discord{Enabled: true, TokenRef: "env:MASAMONG_DISCORD_TOKEN"}, nil)
	if err != ErrNoToken {
		t.Fatalf("want ErrNoToken, got %v", err)
	}
	if _, err := Start(context.Background(), svc, config.Discord{Enabled: true, TokenRef: "raw-token-value"}, nil); err == nil || strings.Contains(err.Error(), "raw-token") == false {
		// A bare value is rejected as a malformed ref; the message names the ref, not a secret we resolved.
		t.Fatalf("bad ref: %v", err)
	}
}

func TestCommandNamesAreDiscordSafe(t *testing.T) {
	svc := svcForTest(t)
	SetCatalog(svc.I18n)
	seen := map[string]bool{}
	for _, c := range ApplicationCommands() {
		if c.Type == discordgo.MessageApplicationCommand {
			continue // Apps menu items may have spaces and capitals (format_test checks them)
		}
		if c.Name == "" || len(c.Name) > 32 || strings.ToLower(c.Name) != c.Name || strings.Contains(c.Name, " ") {
			t.Fatalf("bad name %q", c.Name)
		}
		if seen[c.Name] {
			t.Fatalf("duplicate %q", c.Name)
		}
		seen[c.Name] = true
		if len(c.Description) == 0 || len(c.Description) > 100 {
			t.Fatalf("desc %q", c.Name)
		}
		if (*c.NameLocalizations)[discordgo.Korean] == "" {
			t.Fatalf("no ko name for %q", c.Name)
		}
		for _, o := range c.Options {
			if o.Name != strings.ToLower(o.Name) || len(o.Description) == 0 || len(o.Description) > 100 {
				t.Fatalf("option %q on %q", o.Name, c.Name)
			}
		}
	}
	if got := CommandsHash(); got != "5276ecb7465f396" {
		t.Fatalf("command registration hash = %q", got)
	}
}

func TestDiscordDeliveryEntrypointsReportAndClassifyFailures(t *testing.T) {
	g, fake := gatewayWithFakeDiscord(t)
	fastReplyBackoff(t)
	ctx := context.Background()
	id, err := g.PostChannel(ctx, "c", "", "hello", nil)
	if err != nil || id != "1" {
		t.Fatalf("PostChannel = %q, %v", id, err)
	}
	if err := g.EditChannel(ctx, "c", "m1", &action.Card{Title: "updated"}); err != nil {
		t.Fatalf("EditChannel: %v", err)
	}
	if err := g.SendDM(ctx, "u1", action.Card{Title: "private"}, ""); err != nil {
		t.Fatalf("SendDM: %v", err)
	}
	said, ids, _ := g.sendReply(ctx, g.sess, "g", "c", &discordgo.MessageReference{MessageID: "m1", ChannelID: "c"}, "answer")
	if said != "answer" || len(ids) != 1 || ids[0] != "1" {
		t.Fatalf("sendReply = %q %v", said, ids)
	}

	fake.setResponse("/channels/denied/messages", http.StatusForbidden, `{"message":"missing permissions","code":50013}`)
	if _, err := g.PostChannel(ctx, "denied", "", "hello", nil); !errors.Is(err, application.ErrNotDelivered) {
		t.Fatalf("permission refusal = %v", err)
	}
	fake.setResponse("/channels/c/messages/m-edit", http.StatusInternalServerError, `{"message":"temporary","code":0}`)
	if err := g.EditChannel(ctx, "c", "m-edit", &action.Card{Title: "updated"}); err == nil {
		t.Fatal("EditChannel hid a Discord API failure")
	}
	fake.setResponse("/users/@me/channels", http.StatusForbidden, `{"message":"cannot send","code":50007}`)
	if err := g.SendDM(ctx, "closed", action.Card{Title: "private"}, ""); !errors.Is(err, application.ErrDMClosed) {
		t.Fatalf("closed DM classification = %v", err)
	}
	fake.setResponse("/channels/c/messages", http.StatusInternalServerError, `{"message":"temporary","code":0}`)
	if said, ids, rep := g.sendReply(ctx, g.sess, "g", "c", nil, "answer"); said != "" || len(ids) != 0 || rep.Sent != 0 || rep.Class != errclass.Overloaded {
		t.Fatalf("failed reply reported delivery: %q %v", said, ids)
	}
}

func TestPrefixParsing(t *testing.T) {
	cmd, opts, ok := ParsePrefix("!", "!날씨 부산")
	if !ok || cmd.Name != "weather" || opts["region"] != "부산" {
		t.Fatalf("%v %+v %v", ok, cmd, opts)
	}
	cmd, opts, ok = ParsePrefix("!", "!투표 점심? | 김밥, 라면, 돈까스")
	if !ok || cmd.Name != "poll" || opts["question"] != "점심?" || opts["answers"] != "김밥, 라면, 돈까스" {
		t.Fatalf("poll %v %+v", ok, opts)
	}
	if _, _, ok := ParsePrefix("!", "안녕 !날씨"); ok {
		t.Fatal("prefix must be at the start")
	}
	if _, _, ok := ParsePrefix("!", "!unknowncmd x"); ok {
		t.Fatal("unknown alias")
	}
	if _, _, ok := ParsePrefix("", "!날씨"); ok {
		t.Fatal("empty prefix disables the bridge")
	}
}

func TestRenderNeverMentionsAndLabelsFixtures(t *testing.T) {
	svc := svcForTest(t)
	withoutWorldWeather(t, svc)
	cmd, _ := ByName("weather")
	res := svc.Execute(context.Background(), requestFor(cmd, map[string]string{"region": "서울"}, InvokeContext{Locale: "ko"}, "t"))
	r := Render(res, cmd, "ko")
	if r.Embed == nil || !strings.Contains(r.Embed.Description, "인증키") {
		t.Fatalf("unconfigured weather must say so in Korean: %+v", r.Embed)
	}
	if noMentions.Parse == nil || len(noMentions.Parse) != 0 {
		t.Fatal("allowed mentions must be an explicit empty parse list")
	}
	// Error rendering uses the localized message key, hides operator codes, and keeps retry guidance.
	fail := action.Result{OK: false, Action: "weather.query", Error: &action.Error{Code: "PROVIDER_UNAVAILABLE", MessageKey: "generic.error", Message: "developer timeout", Retryable: true}}
	r = Render(fail, cmd, "en")
	if r.Embed.Footer != nil || !strings.Contains(r.Embed.Description, "An error occurred.") || strings.Contains(r.Embed.Description, "developer timeout") || !strings.Contains(r.Embed.Description, "Try again") || !r.Ephemeral {
		t.Fatalf("error embed %+v", r.Embed)
	}
}

func TestRenderUsesLocalizedErrorKeyInsteadOfRawErrorOrCode(t *testing.T) {
	cmd, _ := ByName("weather")
	res := action.Result{OK: false, Action: cmd.Action, Error: &action.Error{
		Code: "INVALID_INPUT", MessageKey: "privacy.scope_unknown", Message: `C:\private\data\masamong.db: invalid input`,
		Args: map[string]any{"scope": `C:\private\data\masamong.db`},
	}}
	r := Render(res, cmd, "ko")
	if r.Embed.Footer != nil || strings.Contains(r.Embed.Description, `C:\private\data\masamong.db`) || strings.Contains(r.Embed.Description, "INVALID_INPUT") {
		t.Fatalf("member-visible error leaked developer details: %+v", r.Embed)
	}
}

func TestInitialGatewayOpenRetriesWithBoundedBackoff(t *testing.T) {
	calls := 0
	var waits []time.Duration
	g := &Gateway{
		ctx: context.Background(), log: slog.New(slog.NewTextHandler(io.Discard, nil)), token: "secret-token",
		open: func() error {
			calls++
			if calls <= 8 {
				return errors.New("temporary gateway outage")
			}
			return nil
		},
		retryJitter: func(delay time.Duration) time.Duration { return delay },
		retryWait: func(_ context.Context, delay time.Duration) bool {
			waits = append(waits, delay)
			return true
		},
	}
	g.connect()
	if calls != 9 || g.StartupStatus().Attempts != 9 {
		t.Fatalf("Open calls/status attempts = %d/%d, want 9/9", calls, g.StartupStatus().Attempts)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second, 30 * time.Second}
	if fmt.Sprint(waits) != fmt.Sprint(want) {
		t.Fatalf("retry waits = %v, want %v", waits, want)
	}
	if status := g.StartupStatus(); status.LastError != "" || status.NextRetry != "" {
		t.Fatalf("successful retry retained error state: %+v", status)
	}
	for i := 0; i < 100; i++ {
		if delay := jittered(startupRetryMax); delay < startupRetryMax/2 || delay > startupRetryMax {
			t.Fatalf("jitter outside bounded range: %v", delay)
		}
	}
}

func TestInvalidDiscordTokenStopsStartupRetries(t *testing.T) {
	calls := 0
	g := &Gateway{
		ctx: context.Background(), log: slog.New(slog.NewTextHandler(io.Discard, nil)), token: "private-token",
		open:      func() error { calls++; return errors.New("authentication failed: invalid token") },
		retryWait: func(context.Context, time.Duration) bool { t.Fatal("invalid token was retried"); return false },
	}
	g.connect()
	status := g.StartupStatus()
	if calls != 1 || status.Attempts != 1 || status.NextRetry != "" || !strings.Contains(status.LastError, "invalid token") || strings.Contains(status.LastError, "private-token") {
		t.Fatalf("invalid token status/calls = %+v/%d", status, calls)
	}
}

func TestMissingDiscordTokenReturnsWithoutStartingRetries(t *testing.T) {
	svc := svcForTest(t)
	if _, err := Start(context.Background(), svc, config.Discord{}, slog.New(slog.NewTextHandler(io.Discard, nil))); !errors.Is(err, ErrNoToken) {
		t.Fatalf("missing-token startup error = %v", err)
	}
}

func TestCancelledDiscordStartupRetryClearsNextAttempt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := &Gateway{
		ctx: ctx, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		open:        func() error { return errors.New("temporary gateway outage") },
		retryJitter: func(delay time.Duration) time.Duration { return delay },
		retryWait:   func(ctx context.Context, _ time.Duration) bool { cancel(); <-ctx.Done(); return false },
	}
	g.connect()
	status := g.StartupStatus()
	if status.Attempts != 1 || status.LastError == "" || status.NextRetry != "" {
		t.Fatalf("cancelled retry state = %+v", status)
	}
}

func TestReadyDisconnectResumeRestoresScheduledDeliveryGate(t *testing.T) {
	svc := svcForTest(t)
	policy, _ := json.Marshal(map[string]any{"server_approval": false})
	if res := svc.Execute(context.Background(), action.Request{Action: "access.policy", Input: policy}); !res.OK {
		t.Fatalf("disable server approval: %+v", res.Error)
	}
	seoul, err := time.LoadLocation("Asia/Seoul")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.October, 5, 10, 0, 0, 0, seoul)
	svc.Clock = clock.Fixed{T: now}
	sess := &discordgo.Session{} // injected fake session; handlers do not perform REST during READY/RESUMED
	g := &Gateway{svc: svc, cfg: config.Discord{}, log: slog.New(slog.NewTextHandler(io.Discard, nil)), sess: sess, ctx: context.Background()}
	g.onReady(sess, &discordgo.Ready{User: &discordgo.User{ID: "bot", Username: "Masamong"}})
	if !g.Connected() {
		t.Fatal("READY did not set connected")
	}
	g.onDisconnect(sess, &discordgo.Disconnect{})
	if g.Connected() {
		t.Fatal("disconnect did not clear connected")
	}
	g.onResumed(sess, &discordgo.Resumed{})
	if !g.Connected() {
		t.Fatal("RESUMED did not restore the scheduled-send gate")
	}

	svc.Discord = g
	inv := &action.Invoker{Via: "command", GuildID: "g", ChannelID: "c", UserID: "u1", Name: "Taylor"}
	proposal := svc.Execute(context.Background(), action.Request{Action: "reminders.propose", Locale: "en", Invoker: inv, Input: mustJSON(map[string]any{"text": "in 5 minutes stretch"})})
	if !proposal.OK {
		t.Fatalf("reminder proposal: %+v", proposal.Error)
	}
	card, ok := proposal.Output["card"].(action.Card)
	if !ok {
		t.Fatalf("proposal card missing: %+v", proposal.Output)
	}
	reminder := ""
	for _, button := range card.Buttons {
		if strings.HasPrefix(button.ID, "remind:cancel:") {
			reminder = strings.TrimPrefix(button.ID, "remind:cancel:")
		}
	}
	if reminder == "" {
		t.Fatalf("reminder id missing from proposal card: %+v", card)
	}
	confirmed := svc.Execute(context.Background(), action.Request{Action: "reminders.update", Locale: "en", Invoker: inv, Input: mustJSON(map[string]any{"op": "confirm", "id": reminder, "destination": "dm"})})
	if !confirmed.OK {
		t.Fatalf("reminder confirmation: %+v", confirmed.Error)
	}
	svc.Clock = clock.Fixed{T: now.Add(6 * time.Minute)}
	if svc.Discord == nil || !svc.Discord.Connected() {
		t.Fatal("the reminder scheduler's Connected gate stayed closed after resume")
	}
}

func TestChatRefusalNoticesAreLocalizedAndRateLimited(t *testing.T) {
	svc := svcForTest(t)
	g := &Gateway{svc: svc, log: slog.New(slog.NewTextHandler(io.Discard, nil)), notices: map[string]time.Time{}}
	m := &discordgo.MessageCreate{Message: &discordgo.Message{ID: "m1", ChannelID: "c1", GuildID: "g1", Author: &discordgo.User{ID: "u1"}}}
	for _, tc := range []struct {
		err error
		key string
	}{
		{application.ErrChatBudget, "discord.chat.budget"},
		{application.ErrChatLimited, "discord.chat.limited"},
		{application.ErrChatBusy, "discord.chat.busy"},
		{application.ErrChatPaused, "discord.chat.paused"},
	} {
		if got := chatNoticeKey(tc.err); got != tc.key {
			t.Errorf("%v notice key = %q, want %q", tc.err, got, tc.key)
		}
	}
	key := "discord.chat.paused"
	var bodies []string
	SetCatalog(svc.I18n)
	want := messages.T(g.locale("g1"), key)
	send := func(_ context.Context, msg *discordgo.MessageSend) error {
		bodies = append(bodies, msg.Content)
		return nil
	}
	g.sendChatNoticeWith(context.Background(), m, key, true, send)
	g.sendChatNoticeWith(context.Background(), m, key, true, send)
	if len(bodies) != 1 || bodies[0] != want {
		t.Fatalf("localized notice send count/content = %d/%q", len(bodies), bodies)
	}
	other := &discordgo.MessageCreate{Message: &discordgo.Message{ID: "m2", ChannelID: "c2", GuildID: "g1", Author: &discordgo.User{ID: "u1"}}}
	g.sendChatNoticeWith(context.Background(), other, key, true, send)
	if len(bodies) != 2 {
		t.Fatalf("different channel should get its own notice window: %d", len(bodies))
	}
	if chatNoticeKey(application.ErrChatDisabled) != "" {
		t.Fatal("disabled chat must remain silent")
	}
}

func TestSendReplyStopsAfterContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := &Gateway{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	sends := 0
	said, ids, _ := g.sendReplyParts(ctx, []string{"first", "second"}, nil, func(ctx context.Context, _ *discordgo.MessageSend, _ string) (*discordgo.Message, error) {
		if ctx.Err() != nil {
			t.Fatal("send received an expired context")
		}
		sends++
		cancel()
		return &discordgo.Message{ID: "sent-first"}, nil
	}, nil)
	if sends != 1 || said != "first" || len(ids) != 1 || ids[0] != "sent-first" {
		t.Fatalf("cancelled split reply = sends %d, text %q, ids %v", sends, said, ids)
	}
}

func TestHelpListsOnlyReadyLiveSurfaces(t *testing.T) {
	svc := svcForTest(t)
	SetCatalog(svc.I18n)
	withoutWorldWeather(t, svc)
	g := &Gateway{svc: svc, cfg: config.Discord{CommandPrefix: "!", MessageContent: true, RegisterCommands: true}}
	r := g.renderCommand(action.Result{OK: true, Action: "help.version"}, Command{Name: "help", Action: "help.version"}, "ko", false)
	description := r.Embed.Description
	for _, want := range []string{"**슬래시 명령**", "**접두 명령**", "/poll", "!도움말", "답변 평가"} {
		if !strings.Contains(description, want) {
			t.Errorf("help missing %q: %s", want, description)
		}
	}
	for _, absent := range []string{"/fortune-month", "/subscribe", "!날씨", "/weather"} {
		if strings.Contains(description, absent) {
			t.Errorf("help includes unavailable or unregistered command %q: %s", absent, description)
		}
	}
}

func TestPollFlowThroughRenderedButtons(t *testing.T) {
	svc := svcForTest(t)
	cmd, _ := ByName("poll")
	ictx := InvokeContext{ChannelID: "c1", UserID: "u1", Locale: "ko"}
	res := svc.Execute(context.Background(), requestFor(cmd, map[string]string{"question": "점심?", "options": "김밥, 라면"}, ictx, "t"))
	if !res.OK {
		t.Fatalf("create %+v", res)
	}
	r := Render(res, cmd, "ko")
	if r.Poll == nil || len(r.Poll.Answers) != 2 {
		t.Fatalf("native poll %+v", r.Poll)
	}
	if r.Poll.Question.Text != "점심?" || r.Poll.Duration != 24 {
		t.Fatalf("poll question/duration %+v", r.Poll)
	}
	if r.Poll.Answers[0].Media.Text != "김밥" {
		t.Fatalf("answers %+v", r.Poll.Answers)
	}
	pollID := fmt.Sprint(res.Output["id"])
	in, _ := json.Marshal(map[string]any{"id": pollID, "scope": "discord:c1", "participant": "u1", "option_ids": []string{"o1"}, "base_revision": 1})
	v := svc.Execute(context.Background(), action.Request{Action: "polls.vote", Input: in})
	if !v.OK || v.Revision != 2 {
		t.Fatalf("vote %+v", v)
	}
	in, _ = json.Marshal(map[string]any{"id": pollID, "scope": "discord:c2", "participant": "u2", "option_ids": []string{"o1"}, "base_revision": 2})
	if v := svc.Execute(context.Background(), action.Request{Action: "polls.vote", Input: in}); v.OK || v.Error.Code != "SCOPE_MISMATCH" {
		t.Fatalf("scope %+v", v)
	}
	in, _ = json.Marshal(map[string]any{"id": pollID})
	rr := Render(svc.Execute(context.Background(), action.Request{Action: "polls.result", Input: in}), Command{Name: "results"}, "ko")
	if !strings.Contains(rr.Embed.Description, "김밥 — 1") {
		t.Fatalf("results %s", rr.Embed.Description)
	}
}

func TestDiscordTimestampsUseFieldMarkdown(t *testing.T) {
	res := action.Result{OK: true, Action: "weather.query", Output: map[string]any{
		"region": "서울", "temp_c": 12.0, "live": true, "status": "ok",
		"observed_at": "2026-09-16T07:00:00Z",
	}}
	r := Render(res, Command{Name: "weather"}, "ko")
	if r.Embed == nil {
		t.Fatal("embed")
	}
	if r.Embed.Footer != nil && strings.Contains(r.Embed.Footer.Text, "<t:") {
		t.Fatalf("timestamp markdown is not parsed in footers: %+v", r.Embed.Footer)
	}
	found := false
	for _, f := range r.Embed.Fields {
		if strings.Contains(f.Value, "<t:") && strings.Contains(f.Value, ":f>") {
			found = true
		}
	}
	if !found {
		t.Fatalf("want <t:unix:f> in embed fields, got %+v", r.Embed.Fields)
	}
}

// Simultaneous button presses all count: each applies to the poll as it is when it arrives, inside one
// transaction, instead of failing (or silently overwriting) on another voter's revision.
func TestSimultaneousButtonVotesAllCount(t *testing.T) {
	svc := svcForTest(t)
	cmd, _ := ByName("poll")
	res := svc.Execute(context.Background(), requestFor(cmd, map[string]string{"question": "점심?", "options": "김밥, 라면"},
		InvokeContext{ChannelID: "c1", UserID: "u0", Locale: "ko"}, "t"))
	if !res.OK {
		t.Fatalf("create %+v", res)
	}
	pollID := fmt.Sprint(res.Output["id"])
	const voters = 12
	var wg sync.WaitGroup
	for i := 0; i < voters; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, req, ok := buttonRequest([]string{"poll", "vote", pollID, "o1"}, InvokeContext{ChannelID: "c1", UserID: fmt.Sprintf("u%d", i)}, fmt.Sprintf("dc:%d", i))
			if !ok {
				t.Error("button not recognised")
				return
			}
			if r := svc.Execute(context.Background(), req); !r.OK {
				t.Errorf("voter %d: %+v", i, r.Error)
			}
		}(i)
	}
	wg.Wait()
	_, req, _ := buttonRequest([]string{"poll", "result", pollID}, InvokeContext{ChannelID: "c1"}, "dc:r")
	if got := toInt(svc.Execute(context.Background(), req).Output["unique_voters"]); got != voters {
		t.Fatalf("%d of %d votes stored", got, voters)
	}
	// a press from another channel is still refused by the poll's scope
	_, req, _ = buttonRequest([]string{"poll", "vote", pollID, "o1"}, InvokeContext{ChannelID: "other", UserID: "x"}, "dc:x")
	if r := svc.Execute(context.Background(), req); r.OK || r.Error.Code != "SCOPE_MISMATCH" {
		t.Fatalf("foreign channel vote: %+v", r)
	}
}
