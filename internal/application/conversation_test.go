package application_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"masamong/internal/application"
	"masamong/internal/chat"
	"masamong/internal/config"
	"masamong/internal/tools"
)

// recordingServer answers chat completions: refer prompts with refer, other planner prompts with plan,
// and everything else with answer. It keeps every prompt it was sent.
type recordingServer struct {
	mu      sync.Mutex
	prompts []string
}

func (rs *recordingServer) start(t *testing.T, reply func(prompt string) string) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var b strings.Builder
		for _, m := range req.Messages {
			b.WriteString(m.Content + "\n")
		}
		prompt := b.String()
		rs.mu.Lock()
		rs.prompts = append(rs.prompts, prompt)
		rs.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
			"message": map[string]any{"role": "assistant", "content": reply(prompt)}, "finish_reason": "stop"}},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "cost": 0.00001}})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func (rs *recordingServer) all() []string {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return append([]string(nil), rs.prompts...)
}

// The user's example: one friend talks to the bot while two others pick a game. The game talk never
// reaches the planner or the answer; the bot's earlier exchange with the caller does.
func TestBusyRoomShowsOnlyTheCallersConversation(t *testing.T) {
	var plan, resp recordingServer
	planURL := plan.start(t, func(p string) string {
		if strings.Contains(p, "refers_to") {
			return `{"refers_to": []}`
		}
		return `{"memory_requirement":"none","tool_plan":[]}`
	})
	respURL := resp.start(t, func(string) string { return "응 우산 챙겨" })
	svc := routedSvc(t, planURL, respURL)
	now := time.Now()
	obs := func(id, author, name, text string, sec int, mentionBot bool, reply string) {
		m := application.ObservedMessage{GuildID: "g", ChannelID: "c", MessageID: id, AuthorID: author, AuthorName: name, Content: text,
			At: now.Add(time.Duration(sec) * time.Second), FromBot: author == "bot", ReplyTo: reply}
		if mentionBot {
			m.Mentions = []string{"bot"}
		}
		svc.Observe(m)
	}
	obs("1", "uc", "Chris", "@마사몽 내일 서울 비 와?", -60, true, "")
	obs("2", "ua", "Alex", "오늘 밤 롤 할 사람", -55, false, "")
	obs("3", "ud", "Dana", "ㄴㄴ 발로란트 하자", -50, false, "")
	obs("4", "bot", "마사몽", "내일 오후부터 비 소식 있어", -45, false, "1")
	obs("5", "ua", "Alex", "발로 몇 시에 켬?", -20, false, "")
	obs("6", "uc", "Chris", "@마사몽 그럼 우산 챙겨야 돼?", -5, true, "")
	res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "6",
		Message: "@마사몽 그럼 우산 챙겨야 돼?", Speaker: "Chris", SpeakerID: "uc"})
	if err != nil || res.Answer == "" {
		t.Fatalf("turn: %+v %v", res, err)
	}
	for _, p := range append(plan.all(), resp.all()...) {
		if strings.Contains(p, "refers_to") {
			// the classifier is shown the other lines on purpose, as candidates
			continue
		}
		for _, game := range []string{"롤 할 사람", "발로란트", "발로 몇 시"} {
			if strings.Contains(p, game) {
				t.Fatalf("game talk %q reached a prompt:\n%s", game, p)
			}
		}
	}
	answer := resp.all()[0]
	if !strings.Contains(answer, "내일 서울 비 와?") || !strings.Contains(answer, "내일 오후부터 비 소식") {
		t.Fatalf("the caller's earlier exchange is missing:\n%s", answer)
	}
}

// When the call points at the other talk ("what do you think of that?"), the classifier's pick brings
// exactly that line in.
func TestPointedAtLineComesIn(t *testing.T) {
	var plan, resp recordingServer
	planURL := plan.start(t, func(p string) string {
		if strings.Contains(p, "refers_to") {
			return `{"refers_to": ["m1"]}`
		}
		return `{"memory_requirement":"none","tool_plan":[]}`
	})
	respURL := resp.start(t, func(string) string { return "괜찮은데?" })
	svc := routedSvc(t, planURL, respURL)
	now := time.Now()
	svc.Observe(application.ObservedMessage{GuildID: "g", ChannelID: "c", MessageID: "1", AuthorID: "ua", AuthorName: "Alex",
		Content: "이번 주말 제주도 갈까", At: now.Add(-30 * time.Second)})
	svc.Observe(application.ObservedMessage{GuildID: "g", ChannelID: "c", MessageID: "2", AuthorID: "ud", AuthorName: "Dana",
		Content: "점심 뭐 먹지", At: now.Add(-20 * time.Second)})
	svc.Observe(application.ObservedMessage{GuildID: "g", ChannelID: "c", MessageID: "3", AuthorID: "uc", AuthorName: "Chris",
		Content: "@마사몽 저거 어때?", At: now, Mentions: []string{"bot"}})
	if _, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "3",
		Message: "@마사몽 저거 어때?", Speaker: "Chris", SpeakerID: "uc"}); err != nil {
		t.Fatal(err)
	}
	answer := resp.all()[0]
	if !strings.Contains(answer, "제주도") || strings.Contains(answer, "점심") {
		t.Fatalf("expected only the pointed-at line:\n%s", answer)
	}
}

// attribution: off keeps the old behaviour (every recent stored line), for channels that want it.
func TestAttributionOffKeepsEverything(t *testing.T) {
	var plan, resp recordingServer
	planURL := plan.start(t, func(string) string { return `{"memory_requirement":"none","tool_plan":[]}` })
	respURL := resp.start(t, func(string) string { return "ok" })
	svc := routedSvc(t, planURL, respURL)
	applyDoc(t, svc, func(d *config.Document) {
		if d.Guilds == nil {
			d.Guilds = map[string]config.GuildSettings{}
		}
		d.Guilds["g"] = config.GuildSettings{Channels: map[string]config.ChannelPolicy{"c": {Respond: "mention", Attribution: "off"}}}
	})
	svc.Observe(application.ObservedMessage{GuildID: "g", ChannelID: "c", MessageID: "1", AuthorID: "ua", AuthorName: "Alex",
		Content: "롤 할 사람", At: time.Now().Add(-10 * time.Second)})
	if _, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "2",
		Message: "@마사몽 안녕", Speaker: "Chris", SpeakerID: "uc"}); err != nil {
		t.Fatal(err)
	}
	for _, p := range plan.all() {
		if strings.Contains(p, "refers_to") {
			t.Fatal("no classifier when attribution is off")
		}
	}
}

// A message that only says the bot's name is answered when the classifier hears it said to the bot, and
// left alone (and logged) when it is talk about the bot.
func TestNameOnlyCall(t *testing.T) {
	for _, c := range []struct {
		verdict string
		answer  bool
	}{{`{"to":"bot","tone":"neutral"}`, true}, {`{"to":"about_bot","tone":"playful"}`, false}, {`not json`, false}} {
		var plan, resp recordingServer
		planURL := plan.start(t, func(p string) string {
			if strings.Contains(p, "about_bot") {
				return c.verdict
			}
			return `{"memory_requirement":"none","tool_plan":[]}`
		})
		respURL := resp.start(t, func(string) string { return "왜 불러" })
		svc := routedSvc(t, planURL, respURL)
		applyDoc(t, svc, func(d *config.Document) {
			if d.Guilds == nil {
				d.Guilds = map[string]config.GuildSettings{}
			}
			d.Guilds["g"] = config.GuildSettings{Channels: map[string]config.ChannelPolicy{"c": {Respond: "mention"}}}
		})
		res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "9",
			Message: "마사몽 또 고장났네 ㅋㅋ", Speaker: "Chris", SpeakerID: "uc", NameOnly: true})
		if c.answer && (err != nil || res.Answer == "") {
			t.Fatalf("%s: expected an answer, got %v %q", c.verdict, err, res.Answer)
		}
		if !c.answer && !errors.Is(err, application.ErrNotAddressed) {
			t.Fatalf("%s: expected ErrNotAddressed, got %v", c.verdict, err)
		}
		if !c.answer && len(resp.all()) != 0 {
			t.Fatalf("%s: the answer model was called", c.verdict)
		}
		if !c.answer {
			// the turn record keeps the words (the room logs them) and what the check decided
			r := exec(svc, "turns.list", map[string]any{"limit": 1})
			list := wire(t, r.Output)["turns"].([]any)
			b := list[0].(map[string]any)["body"].(map[string]any)
			nc, _ := b["name_call"].(map[string]any)
			want := map[string]string{`{"to":"about_bot","tone":"playful"}`: "about_bot", `not json`: "unparsed"}[c.verdict]
			if b["message"] != "마사몽 또 고장났네 ㅋㅋ" || b["speaker"] != "Chris" || nc["decision"] != want {
				t.Fatalf("%s: record %+v", c.verdict, b)
			}
		}
	}
}

// The follow-up window: off by default, where it only records what it would have done; in window mode it
// answers the person the bot just answered, and closes when they turn to someone else.
func TestFollowUpWindow(t *testing.T) {
	var plan, resp recordingServer
	planURL := plan.start(t, func(string) string { return `{"memory_requirement":"none","tool_plan":[]}` })
	respURL := resp.start(t, func(string) string { return "내일은 맑아. 우산 필요해?" })
	svc := routedSvc(t, planURL, respURL)
	setFollow := func(mode string) {
		applyDoc(t, svc, func(d *config.Document) {
			if d.Guilds == nil {
				d.Guilds = map[string]config.GuildSettings{}
			}
			d.Guilds["g"] = config.GuildSettings{Channels: map[string]config.ChannelPolicy{"c": {Respond: "mention",
				Conversation: config.ChannelConversation{Follow: mode}}}}
		})
	}
	turn := func(id string) {
		t.Helper()
		svc.Observe(application.ObservedMessage{GuildID: "g", ChannelID: "c", MessageID: id, AuthorID: "uc", AuthorName: "Chris",
			Content: "@마사몽 내일 날씨", At: time.Now(), Mentions: []string{"bot"}})
		if _, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: id,
			Message: "@마사몽 내일 날씨", Speaker: "Chris", SpeakerID: "uc",
			Deliver: func(_ context.Context, r chat.Result) string { return r.Answer }}); err != nil {
			t.Fatal(err)
		}
	}
	msg := func(id, text string, mentions ...string) application.ObservedMessage {
		m := application.ObservedMessage{GuildID: "g", ChannelID: "c", MessageID: id, AuthorID: "uc", AuthorName: "Chris",
			Content: text, At: time.Now(), Mentions: mentions}
		svc.Observe(m)
		return m
	}
	setFollow("")
	turn("1")
	if !svc.FollowWindowOpen("g", "c", "uc") {
		t.Fatal("a delivered reply opens the window")
	}
	if svc.FollowUp(context.Background(), msg("2", "응 챙길게")) {
		t.Fatal("off by default: never answers")
	}
	setFollow("window")
	turn("3")
	if !svc.FollowUp(context.Background(), msg("4", "응 챙길게")) {
		t.Fatal("window mode answers a clear follow-up")
	}
	turn("5")
	if svc.FollowUp(context.Background(), msg("6", "@Alex 너도 가?", "ua")) || svc.FollowWindowOpen("g", "c", "uc") {
		t.Fatal("turning to a friend is not a follow-up and closes the window")
	}
	rows := svc.ConversationLogForTest("follow")
	if len(rows) != 3 || rows[0] != "would_answer" || rows[1] != "answer" || rows[2] != "skip" {
		t.Fatalf("decisions logged: %v", rows)
	}
}

// Banter (experimental): a playful mention of the bot gets nothing while off, only a record in shadow,
// and in on mode one light line, then emoji once the hourly lines are used; a 👎-less room keeps going.
func TestBanterLadder(t *testing.T) {
	var plan, resp recordingServer
	planURL := plan.start(t, func(p string) string {
		if strings.Contains(p, "about_bot") {
			return `{"to":"about_bot","tone":"playful"}`
		}
		return `{"memory_requirement":"none","tool_plan":[]}`
	})
	respURL := resp.start(t, func(string) string { return "고장은 너 기분이겠지" })
	svc := routedSvc(t, planURL, respURL)
	set := func(mode string) {
		applyDoc(t, svc, func(d *config.Document) {
			if d.Guilds == nil {
				d.Guilds = map[string]config.GuildSettings{}
			}
			d.Guilds["g"] = config.GuildSettings{Channels: map[string]config.ChannelPolicy{"c": {Respond: "mention",
				Conversation: config.ChannelConversation{Banter: mode}}}}
		})
	}
	turn := func(id string) (chat.Result, error) {
		return svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: id,
			Message: "마사몽 또 고장났네 ㅋㅋ", Speaker: "Chris", SpeakerID: "uc", NameOnly: true,
			Deliver: func(_ context.Context, r chat.Result) string { return r.Answer }})
	}
	set("")
	if _, err := turn("1"); !errors.Is(err, application.ErrNotAddressed) {
		t.Fatalf("off: %v", err)
	}
	set("shadow")
	if _, err := turn("2"); !errors.Is(err, application.ErrNotAddressed) {
		t.Fatalf("shadow: %v", err)
	}
	if len(resp.all()) != 0 {
		t.Fatal("off and shadow never call the answer model")
	}
	if got := svc.ConversationLogForTest("banter"); len(got) != 1 || got[0] != "would_line" {
		t.Fatalf("shadow records what it would have done: %v", got)
	}
	set("on")
	for i, want := range []string{"line", "line", "emoji"} {
		res, err := turn(strconv.Itoa(10 + i))
		var react application.ReactOnly
		switch want {
		case "line":
			if err != nil || res.Answer == "" {
				t.Fatalf("turn %d: want a line, got %v", i, err)
			}
		case "emoji":
			if !errors.As(err, &react) || react.Emoji == "" {
				t.Fatalf("turn %d: want an emoji, got %v", i, err)
			}
		}
	}
	last := resp.all()[len(resp.all())-1]
	if !strings.Contains(last, "장난스럽게") {
		t.Fatal("a banter line carries the pack's banter rules")
	}
	if got := svc.ConversationLogForTest("banter"); len(got) != 4 || got[3] != "emoji" {
		t.Fatalf("every rung is logged: %v", got)
	}
	// where replies are switched off, a name alone costs nothing: no classifier call, no emoji
	calls := len(plan.all())
	applyDoc(t, svc, func(d *config.Document) {
		d.Tools["chat.respond"] = tools.Settings{Enabled: boolp(false)}
	})
	if _, err := turn("20"); !errors.Is(err, application.ErrChatDisabled) || len(plan.all()) != calls {
		t.Fatalf("replies off: %v, %d classifier calls", err, len(plan.all())-calls)
	}
}

func restartServiceForTest(t *testing.T, svc *application.Service) *application.Service {
	t.Helper()
	dir := svc.DataRoot
	if err := svc.Close(); err != nil {
		t.Fatal("close service before simulated restart")
	}
	next, err := application.Open(dir, nil, false)
	if err != nil {
		t.Fatal("reopen service for simulated restart")
	}
	t.Cleanup(func() { _ = next.Close() })
	return next
}

func banterTestService(t *testing.T) (*application.Service, func(string)) {
	t.Helper()
	var plan, resp recordingServer
	planURL := plan.start(t, func(p string) string {
		if strings.Contains(p, "about_bot") {
			return `{"to":"about_bot","tone":"playful"}`
		}
		return `{"memory_requirement":"none","tool_plan":[]}`
	})
	respURL := resp.start(t, func(string) string { return "reply" })
	svc := routedSvc(t, planURL, respURL)
	set := func(mode string) {
		applyDoc(t, svc, func(d *config.Document) {
			if d.Guilds == nil {
				d.Guilds = map[string]config.GuildSettings{}
			}
			d.Guilds["g"] = config.GuildSettings{Channels: map[string]config.ChannelPolicy{"c": {Respond: "mention",
				Conversation: config.ChannelConversation{Banter: mode}}}}
		})
	}
	set("on")
	return svc, set
}

func runBanterTurn(svc *application.Service, id string) (chat.Result, error) {
	return svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: id,
		Message: "마사몽 또 고장났네 ㅋㅋ", Speaker: "speaker", SpeakerID: "speaker-id", NameOnly: true,
		Deliver: func(_ context.Context, r chat.Result) string { return r.Answer }})
}

func TestBanterCapsSurviveRestart(t *testing.T) {
	svc, _ := banterTestService(t)
	for _, id := range []string{"cap-1", "cap-2"} {
		res, err := runBanterTurn(svc, id)
		if err != nil || res.Answer == "" {
			t.Fatal("expected a sent banter line")
		}
	}
	svc = restartServiceForTest(t, svc)
	res, err := runBanterTurn(svc, "cap-after-restart")
	var react application.ReactOnly
	if !errors.As(err, &react) || react.Emoji == "" || res.Answer != "" {
		t.Fatal("hourly line cap was not restored after restart")
	}
}

func TestBanterDislikeMappingSurvivesRestart(t *testing.T) {
	svc, _ := banterTestService(t)
	res, err := runBanterTurn(svc, "dislike-1")
	if err != nil || res.Answer == "" || res.TurnID == "" {
		t.Fatal("expected a sent banter line with a turn")
	}
	svc.NoteReplyMessages(res.TurnID, "g", "c", []string{"posted-reply"})
	svc = restartServiceForTest(t, svc)
	if !svc.RateReply("g", "posted-reply", "rater", false, "") {
		t.Fatal("rating of the persisted reply was not accepted")
	}
	res, err = runBanterTurn(svc, "dislike-after-restart")
	var react application.ReactOnly
	if !errors.As(err, &react) || react.Emoji == "" || res.Answer != "" {
		t.Fatal("a persisted dislike did not mute banter lines")
	}
}

func TestBanterDislikeMuteSurvivesRestart(t *testing.T) {
	svc, _ := banterTestService(t)
	res, err := runBanterTurn(svc, "mute-1")
	if err != nil || res.Answer == "" || res.TurnID == "" {
		t.Fatal("expected a sent banter line with a turn")
	}
	svc.NoteReplyMessages(res.TurnID, "g", "c", []string{"muted-reply"})
	if !svc.RateReply("g", "muted-reply", "rater", false, "") {
		t.Fatal("the dislike was not accepted")
	}
	svc = restartServiceForTest(t, svc)
	res, err = runBanterTurn(svc, "mute-after-restart")
	var react application.ReactOnly
	if !errors.As(err, &react) || react.Emoji == "" || res.Answer != "" {
		t.Fatal("the dislike mute was lost on restart")
	}
}

func TestFailedBanterReactionDoesNotConsumeCap(t *testing.T) {
	svc, _ := banterTestService(t)
	for _, id := range []string{"line-1", "line-2"} {
		if res, err := runBanterTurn(svc, id); err != nil || res.Answer == "" {
			t.Fatal("expected a sent banter line")
		}
	}
	for i := 0; i < 5; i++ {
		res, err := runBanterTurn(svc, strconv.Itoa(i))
		var react application.ReactOnly
		if !errors.As(err, &react) || react.Emoji == "" || res.Answer != "" {
			t.Fatalf("failed reaction %d consumed the emoji cap", i+1)
		}
		// No Sent notification models MessageReactionAdd failing in the gateway.
	}
}

func TestSentBanterReactionConsumesCap(t *testing.T) {
	svc, _ := banterTestService(t)
	for _, id := range []string{"sent-line-1", "sent-line-2"} {
		if res, err := runBanterTurn(svc, id); err != nil || res.Answer == "" {
			t.Fatal("expected a sent banter line")
		}
	}
	for i := 0; i < 4; i++ {
		_, err := runBanterTurn(svc, strconv.Itoa(100+i))
		var react application.ReactOnly
		if !errors.As(err, &react) || react.Emoji == "" {
			t.Fatal("expected an emoji rung")
		}
		if i == 0 {
			_, err = runBanterTurn(svc, "before-first-reaction-sent")
			if !errors.As(err, &react) || react.Emoji == "" {
				t.Fatal("selecting an emoji consumed the cap before delivery")
			}
		}
		react.Sent()
		var raw string
		if err := svc.Store.DB.QueryRow(`SELECT emoji FROM banter_channel_state WHERE guild_id='g' AND channel_id='c'`).Scan(&raw); err != nil {
			t.Fatal("could not inspect the reaction usage")
		}
		var counted []time.Time
		if err := json.Unmarshal([]byte(raw), &counted); err != nil || len(counted) != i+1 {
			t.Fatal("one successful Sent notification should count exactly once")
		}
	}
	if _, err := runBanterTurn(svc, "sent-emoji-cap"); !errors.Is(err, application.ErrNotAddressed) {
		t.Fatal("successfully sent reactions did not use the emoji cap")
	}
}

func TestFailedBanterLineDoesNotConsumeCap(t *testing.T) {
	svc, _ := banterTestService(t)
	for _, id := range []string{"failed-line-1", "failed-line-2"} {
		_, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: id,
			Message: "마사몽 또 고장났네 ㅋㅋ", Speaker: "speaker", SpeakerID: "speaker-id", NameOnly: true,
			Deliver: func(context.Context, chat.Result) string { return "" }})
		if err != nil {
			t.Fatal("failed delivery should not fail the chat turn")
		}
	}
	for _, id := range []string{"after-failed-lines-1", "after-failed-lines-2"} {
		res, err := runBanterTurn(svc, id)
		var react application.ReactOnly
		if err != nil || res.Answer == "" || errors.As(err, &react) {
			t.Fatal("failed deliveries consumed the line cap")
		}
	}
	_, err := runBanterTurn(svc, "after-two-successful-lines")
	var react application.ReactOnly
	if !errors.As(err, &react) || react.Emoji == "" {
		t.Fatal("successful line deliveries should still consume the line cap")
	}
}

func TestBanterDBWriteFailureDoesNotBlockReply(t *testing.T) {
	svc, _ := banterTestService(t)
	if _, err := svc.Store.DB.Exec(`DROP TABLE banter_channel_state`); err != nil {
		t.Fatal("could not simulate a banter database write failure")
	}
	res, err := runBanterTurn(svc, "db-write-failure")
	if err != nil || res.Answer == "" {
		t.Fatal("banter state persistence failure blocked the reply")
	}
}

func TestBanterDropsFutureUsageAfterClockMovesBack(t *testing.T) {
	svc, _ := banterTestService(t)
	future := time.Now().UTC().Add(2 * time.Hour)
	futureJSON, _ := json.Marshal([]time.Time{future, future.Add(time.Second), future.Add(2 * time.Second), future.Add(3 * time.Second)})
	linesJSON, _ := json.Marshal([]time.Time{future, future.Add(time.Second)})
	// the mute lies beyond now+banterMute, which a real mute never does
	if _, err := svc.Store.DB.Exec(`INSERT INTO banter_channel_state(guild_id,channel_id,lines,emoji,muted_until) VALUES('g','c',?,?,?)`,
		string(linesJSON), string(futureJSON), future.Add(time.Hour).Format(time.RFC3339Nano)); err != nil {
		t.Fatal("could not seed future banter state")
	}
	for _, id := range []string{"future-use-1", "future-use-2"} {
		res, err := runBanterTurn(svc, id)
		var react application.ReactOnly
		if err != nil || res.Answer == "" || errors.As(err, &react) {
			t.Fatal("future line timestamps or mute remained active")
		}
	}
	for i := 0; i < 4; i++ {
		_, err := runBanterTurn(svc, "future-emoji-"+strconv.Itoa(i))
		var react application.ReactOnly
		if !errors.As(err, &react) || react.Emoji == "" {
			t.Fatal("future emoji timestamps consumed the cap")
		}
		react.Sent()
	}
	if _, err := runBanterTurn(svc, "after-future-cap"); !errors.Is(err, application.ErrNotAddressed) {
		t.Fatal("four actual reactions should exhaust the hourly emoji cap")
	}
}

func TestBanterDropsFutureTurnMappingAfterClockMovesBack(t *testing.T) {
	svc, _ := banterTestService(t)
	if _, err := svc.Store.DB.Exec(`INSERT INTO banter_turns(turn_id,guild_id,channel_id,created_at) VALUES(?,?,?,?)`,
		"future-turn", "g", "c", banterTestStamp(time.Now().UTC().Add(time.Hour))); err != nil {
		t.Fatal("could not seed a future turn mapping")
	}
	svc.NoteReplyMessages("future-turn", "g", "c", []string{"future-reply"})
	if !svc.RateReply("g", "future-reply", "rater", false, "") {
		t.Fatal("the saved reply should accept feedback")
	}
	var remaining int
	if err := svc.Store.DB.QueryRow(`SELECT COUNT(*) FROM banter_turns WHERE turn_id='future-turn'`).Scan(&remaining); err != nil {
		t.Fatal("could not inspect the future turn mapping")
	}
	if remaining != 0 {
		t.Fatal("a future turn mapping survived pruning")
	}
	res, err := runBanterTurn(svc, "after-future-turn")
	var react application.ReactOnly
	if err != nil || res.Answer == "" || errors.As(err, &react) {
		t.Fatal("a future turn mapping muted the channel")
	}
}

func TestBanterTurnMappingPrunesByAgeAndCount(t *testing.T) {
	svc, _ := banterTestService(t)
	now := time.Now().UTC()
	if _, err := svc.Store.DB.Exec(`INSERT INTO banter_turns(turn_id,guild_id,channel_id,created_at) VALUES(?,?,?,?)`,
		"old-turn", "g", "c", banterTestStamp(now.Add(-25*time.Hour))); err != nil {
		t.Fatal("could not seed an old turn mapping")
	}
	for i := 0; i < 200; i++ {
		if _, err := svc.Store.DB.Exec(`INSERT INTO banter_turns(turn_id,guild_id,channel_id,created_at) VALUES(?,?,?,?)`,
			"recent-"+strconv.Itoa(i), "g", "c", banterTestStamp(now.Add(-time.Duration(i+1)*time.Second))); err != nil {
			t.Fatal("could not seed recent turn mappings")
		}
	}
	if res, err := runBanterTurn(svc, "new-turn-mapping"); err != nil || res.Answer == "" {
		t.Fatal("a new banter line should be delivered")
	}
	var old, total int
	if err := svc.Store.DB.QueryRow(`SELECT COUNT(*) FROM banter_turns WHERE turn_id='old-turn'`).Scan(&old); err != nil {
		t.Fatal("could not inspect old turn mapping")
	}
	if err := svc.Store.DB.QueryRow(`SELECT COUNT(*) FROM banter_turns`).Scan(&total); err != nil {
		t.Fatal("could not inspect turn mapping count")
	}
	if old != 0 || total != 200 {
		t.Fatal("turn mappings were not pruned to the age and row limits")
	}
}

func banterTestStamp(at time.Time) string { return at.UTC().Format("2006-01-02T15:04:05.000000000Z") }
