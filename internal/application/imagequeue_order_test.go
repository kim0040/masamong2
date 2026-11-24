package application_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/application"
	"masamong/internal/chat"
	"masamong/internal/config"
	"masamong/internal/testutil"
)

// A picture asked for in chat is posted after the turn's reply ("그리는 중" first), even when it is drawn
// before the reply is written; a turn that delivers nothing still gets its picture once it is over, and a
// turn that never ends holds it for a bounded time only.

// pictureTurnSvc is imageChatSvc with chat models whose planner asks for a picture. respond answers the
// reply model's request (nil: a plain line).
func pictureTurnSvc(t *testing.T, respond http.HandlerFunc) (*application.Service, *gatedImages, *pictureGateway) {
	t.Helper()
	svc, img, gw := imageChatSvc(t)
	var planCalls atomic.Int32
	planURL := chatServer(t, `{"memory_requirement":"none","tool_plan":[{"tool_name":"images.generate","parameters":{"prompt":"a cat"}}]}`, "stop", &planCalls)
	if respond == nil {
		respond = func(w http.ResponseWriter, _ *http.Request) { writeChatAnswer(w, "그리는 중이야~") }
	}
	resp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		respond(w, r)
	}))
	t.Cleanup(resp.Close)
	svc.SetEnvForTest(t, "FAKE_OR_KEY", "k")
	applyDoc(t, svc, func(d *config.Document) {
		d.Providers["plan-host"] = config.Provider{Kind: config.KindOpenAI, BaseURL: planURL, CredentialRef: "env:FAKE_OR_KEY"}
		d.Providers["resp-host"] = config.Provider{Kind: config.KindOpenAI, BaseURL: resp.URL, CredentialRef: "env:FAKE_OR_KEY"}
		d.Providers["openmeteo"] = config.Provider{Kind: config.KindOpenMeteo, Disabled: true}
		d.Models["planner"] = config.Model{Provider: "plan-host", Model: "shared/model"}
		d.Models["response"] = config.Model{Provider: "resp-host", Model: "shared/model"}
		d.Memory.Retrieve = "off"
		d.Chat.ToolMode = "planner"
	})
	return svc, img, gw
}

func writeChatAnswer(w http.ResponseWriter, content string) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "cost": 0.00001},
	})
}

// settle gives a finished picture the time it would need to be posted, had it not been held.
const settle = 300 * time.Millisecond

func TestChatPictureIsPostedAfterTheReplyEvenWhenItIsDoneFirst(t *testing.T) {
	t.Parallel()
	svc, img, gw := pictureTurnSvc(t, nil)
	img.open() // the picture is done as soon as it is asked for
	var delivered atomic.Bool
	var picturesAtReply atomic.Int32
	res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "m1", Message: "고양이 그려줘",
		Speaker: "철수", SpeakerID: "u1", Deliver: func(_ context.Context, r chat.Result) string {
			testutil.WaitFor(t, 5*time.Second, "the draw to start", func() bool { return img.calls.Load() == 1 })
			time.Sleep(settle)
			p, _ := gw.counts()
			picturesAtReply.Store(int32(p))
			delivered.Store(true)
			return r.Answer
		}})
	if err != nil {
		t.Fatal(err)
	}
	if !delivered.Load() || res.Answer == "" {
		t.Fatalf("no reply delivered: %+v", res)
	}
	if n := picturesAtReply.Load(); n != 0 {
		t.Fatalf("the picture was posted before the reply (%d pictures when the reply went out)", n)
	}
	testutil.WaitFor(t, 5*time.Second, "the picture after the reply", func() bool { p, _ := gw.counts(); return p == 1 })
	gw.mu.Lock()
	post := gw.pictures[0]
	gw.mu.Unlock()
	if post.channel != "c" || post.replyTo != "m1" {
		t.Fatalf("picture post: %+v", post)
	}
}

func TestPictureOfATurnThatFailedIsPostedWhenTheTurnEnds(t *testing.T) {
	t.Parallel()
	var picturesBeforeEnd atomic.Int32
	picturesBeforeEnd.Store(-1)
	var img *gatedImages
	var gw *pictureGateway
	svc, img, gw := pictureTurnSvc(t, func(w http.ResponseWriter, _ *http.Request) {
		// the reply model fails, after the picture is done
		if img.calls.Load() >= 1 {
			time.Sleep(settle)
			p, _ := gw.counts()
			picturesBeforeEnd.CompareAndSwap(-1, int32(p))
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"refused","type":"invalid_request_error"}}`))
	})
	application.SetImageTurnWaitForTest(svc, time.Minute) // only the turn's end may let it out in time
	img.open()
	// whatever the turn ends with (an error, or a fallback line that did not go out), it delivers nothing
	_, _ = svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "m1", Message: "고양이 그려줘",
		Speaker: "철수", SpeakerID: "u1", Deliver: func(context.Context, chat.Result) string { return "" }})
	ended := time.Now()
	if img.calls.Load() != 1 {
		t.Fatalf("the picture was not queued by the turn: %d provider calls", img.calls.Load())
	}
	if n := picturesBeforeEnd.Load(); n > 0 {
		t.Fatalf("the picture was posted while the turn was still running (%d)", n)
	}
	testutil.WaitFor(t, 5*time.Second, "the picture after the failed turn", func() bool { p, _ := gw.counts(); return p == 1 })
	if took := time.Since(ended); took > 5*time.Second {
		t.Fatalf("the picture waited %v after its turn ended", took)
	}
}

func TestHeldPictureIsPostedAtTheBoundWhenItsTurnNeverEnds(t *testing.T) {
	t.Parallel()
	svc, img, gw := imageChatSvc(t)
	const bound = 700 * time.Millisecond
	application.SetImageTurnWaitForTest(svc, bound)
	never := make(chan struct{}) // the turn that queued it never ends
	run := application.ChatToolInTurnForTest(svc, application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "m1", Message: "a cat", Speaker: "철수", SpeakerID: "u1"}, never)
	img.open()
	queued := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if r := run(ctx, "images.generate", map[string]any{"prompt": "a cat"}); r.Status != "queued" {
		t.Fatalf("%+v", r)
	}
	testutil.WaitFor(t, 10*time.Second, "the picture at the bound", func() bool { p, _ := gw.counts(); return p == 1 })
	if took := time.Since(queued); took < bound-50*time.Millisecond {
		t.Fatalf("posted after %v, before its turn ended and before the bound (%v)", took, bound)
	}
	idle(t, svc)
}

func TestFailureLineIsHeldForTheTurnToo(t *testing.T) {
	t.Parallel()
	svc, img, gw := imageChatSvc(t)
	img.fail.Store(true)
	application.SetImageTurnWaitForTest(svc, time.Minute)
	ended := make(chan struct{})
	run := application.ChatToolInTurnForTest(svc, application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "m1", Message: "a cat", Speaker: "철수", SpeakerID: "u1"}, ended)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if r := run(ctx, "images.generate", map[string]any{"prompt": "a cat"}); r.Status != "queued" {
		t.Fatalf("%+v", r)
	}
	testutil.WaitFor(t, 5*time.Second, "the failed draw", func() bool { return img.calls.Load() == 1 })
	time.Sleep(settle)
	if p, l := gw.counts(); p != 0 || l != 0 {
		t.Fatalf("posted before the turn ended: %d pictures, %d lines", p, l)
	}
	close(ended)
	testutil.WaitFor(t, 5*time.Second, "the failure line after the turn", func() bool { _, l := gw.counts(); return l == 1 })
}

func TestShutdownWhileAPictureWaitsForItsTurnPostsNothing(t *testing.T) {
	t.Parallel()
	svc, img, gw := imageChatSvc(t)
	application.SetImageTurnWaitForTest(svc, time.Minute)
	never := make(chan struct{})
	run := application.ChatToolInTurnForTest(svc, application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "m1", Message: "a cat", Speaker: "철수", SpeakerID: "u1"}, never)
	img.open()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if r := run(ctx, "images.generate", map[string]any{"prompt": "a cat"}); r.Status != "queued" {
		t.Fatalf("%+v", r)
	}
	testutil.WaitFor(t, 5*time.Second, "the draw", func() bool { return img.calls.Load() == 1 })
	time.Sleep(settle) // the picture is done and held
	done := make(chan struct{})
	go func() { _ = svc.Close(); close(done) }()
	select {
	case <-done: // Close waits for the queue's worker: it has returned
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown waits for the held picture")
	}
	if p, l := gw.counts(); p != 0 || l != 0 {
		t.Fatalf("posted at shutdown: %d pictures, %d lines", p, l)
	}
}
