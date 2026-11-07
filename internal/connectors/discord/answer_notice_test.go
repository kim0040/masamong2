package discord

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"masamong/internal/action"
	"masamong/internal/config"
)

// answerModels is a fake model host: the name-call check answers with judge, the planner with a plan that
// needs nothing, the answer with reply (or with a refusal when failAnswer is set).
type answerModels struct {
	judge      string
	reply      string
	failAnswer bool
}

func (a *answerModels) server(t *testing.T) *httptest.Server {
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
		answer := a.reply
		switch {
		case strings.Contains(prompt, "about_bot"):
			answer = a.judge
		case strings.Contains(prompt, "tool_plan"):
			answer = `{"memory_requirement":"none","tool_plan":[]}`
		case a.failAnswer:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"refused","type":"invalid_request_error"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": answer}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "cost": 0.00001},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// posting fails the first failFirst message posts (a permission refusal, which is not retried) and then
// takes the rest; it records every message post.
type posting struct {
	mu        sync.Mutex
	failFirst int
	posts     []string
}

func (p *posting) RoundTrip(req *http.Request) (*http.Response, error) {
	status, body := http.StatusOK, `{"id":"1"}`
	if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/messages") {
		raw, _ := io.ReadAll(req.Body)
		var sent struct {
			Content string `json:"content"`
		}
		_ = json.Unmarshal(raw, &sent)
		p.mu.Lock()
		p.posts = append(p.posts, sent.Content)
		if len(p.posts) <= p.failFirst {
			status, body = http.StatusForbidden, `{"code":50013,"message":"Missing Permissions"}`
		}
		p.mu.Unlock()
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
}

func (p *posting) bodies() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.posts...)
}

// answerGateway is a gateway whose chat runs on the fake models and whose posts go to the fake Discord.
func answerGateway(t *testing.T, models *answerModels, discord *posting) *Gateway {
	t.Helper()
	srv := models.server(t)
	svc := svcForTest(t)
	SetCatalog(svc.I18n)
	if res := svc.Execute(context.Background(), action.Request{Action: "access.policy", Input: mustJSON(map[string]any{"server_approval": false})}); !res.OK {
		t.Fatal("could not disable server approval for the fixture")
	}
	t.Setenv("FAKE_ANSWER_MODEL_KEY", "test")
	raw, err := config.Marshal(svc.Doc())
	if err != nil {
		t.Fatal(err)
	}
	doc, err := config.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	doc.Providers["answer-test"] = config.Provider{Kind: config.KindOpenAI, BaseURL: srv.URL, CredentialRef: "env:FAKE_ANSWER_MODEL_KEY"}
	doc.Models["planner"] = config.Model{Provider: "answer-test", Model: "shared/model"}
	doc.Models["response"] = config.Model{Provider: "answer-test", Model: "shared/model"}
	doc.Memory.Retrieve = "off"
	doc.Chat.ToolMode = "planner"
	doc.Guilds = map[string]config.GuildSettings{"g": {Channels: map[string]config.ChannelPolicy{"c": {Respond: "mention", NameCall: "addressed"}}}}
	encoded, err := config.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if res := svc.Execute(context.Background(), action.Request{Action: "config.apply", Input: mustJSON(map[string]any{
		"yaml": string(encoded), "base_revision": svc.Revision(),
	}), Locale: "en"}); !res.OK {
		t.Fatalf("could not configure the fake models: %+v", res.Error)
	}
	sess, err := discordgo.New("Bot test")
	if err != nil {
		t.Fatal(err)
	}
	sess.Client = &http.Client{Transport: discord}
	return &Gateway{svc: svc, sess: sess, log: slog.New(slog.NewTextHandler(io.Discard, nil)), ctx: context.Background(), notices: map[string]time.Time{}}
}

func answerMessage(text string) *discordgo.MessageCreate {
	return &discordgo.MessageCreate{Message: &discordgo.Message{ID: "m1", ChannelID: "c", GuildID: "g", Content: text,
		Author: &discordgo.User{ID: "u1", Username: "yeonghui"}}}
}

func (g *Gateway) noticeText(m *discordgo.MessageCreate) string {
	return messages.T(g.noticeLocale(m), "discord.chat.failed")
}

// An answer that was made and could not be posted at all (the first send was refused) gets the short
// failure line, on a fresh context; an answer of which a part went out gets none.
func TestAnswerThatReachedNoChannelGetsTheFailureLine(t *testing.T) {
	fastReplyBackoff(t)
	t.Run("nothing went out", func(t *testing.T) {
		discord := &posting{failFirst: 1}
		g := answerGateway(t, &answerModels{reply: "응, 알겠어."}, discord)
		m := answerMessage("@bot 안녕")
		g.answer(context.Background(), g.sess, m, "안녕", turnCall{addressed: true})
		// the reply, refused, and then the failure line
		if posts := discord.bodies(); len(posts) != 2 || posts[1] != g.noticeText(m) {
			t.Fatalf("posts %q: want the refused reply and the failure line", posts)
		}
	})
	t.Run("the first part went out, the second did not", func(t *testing.T) {
		discord := &posting{}
		g := answerGateway(t, &answerModels{reply: strings.Repeat("긴 답변이야. ", 400)}, discord) // several messages
		g.sess.Client = &http.Client{Transport: failSecond{discord}}
		m := answerMessage("@bot 안녕")
		g.answer(context.Background(), g.sess, m, "안녕", turnCall{addressed: true})
		posts := discord.bodies()
		if len(posts) < 2 {
			t.Fatalf("%d posts: the answer should have been split", len(posts))
		}
		for _, p := range posts {
			if p == g.noticeText(m) {
				t.Fatal("a failure line followed an answer of which a part went out")
			}
		}
	})
}

// failSecond refuses the second message post and takes every other.
type failSecond struct{ *posting }

func (f failSecond) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := f.posting.RoundTrip(req)
	if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/messages") && len(f.posting.bodies()) == 2 {
		resp.StatusCode = http.StatusForbidden
		resp.Body = io.NopCloser(strings.NewReader(`{"code":50013,"message":"Missing Permissions"}`))
	}
	return resp, err
}

// A name said alone is a call once the name check says so: a turn that then fails tells the person, and one
// the check found not to be meant for the bot says nothing.
func TestNameCallThatFailsTellsOnlyThoseItWasMeantFor(t *testing.T) {
	for _, tc := range []struct {
		name     string
		judge    string
		wantLine bool
	}{
		{"meant for the bot", `{"to":"bot","tone":"neutral"}`, true},
		{"only about the bot", `{"to":"about_bot","tone":"neutral"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			models := &answerModels{judge: tc.judge, reply: "응", failAnswer: true}
			discord := &posting{}
			g := answerGateway(t, models, discord)
			m := answerMessage("bot 어제 얘기 기억해?")
			g.answer(context.Background(), g.sess, m, "bot 어제 얘기 기억해?", turnCall{nameOnly: true})
			posts := discord.bodies()
			if tc.wantLine != (len(posts) == 1 && posts[0] == g.noticeText(m)) || !tc.wantLine && len(posts) != 0 {
				t.Fatalf("posts %q, want a failure line: %v", posts, tc.wantLine)
			}
		})
	}
}
