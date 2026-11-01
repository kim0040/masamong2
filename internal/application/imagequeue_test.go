package application_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/chat"
	"masamong/internal/config"
	"masamong/internal/observability"
	"masamong/internal/tools"
)

// gatedImages is a picture provider whose answer waits for the test: each call blocks until open() (or
// the caller gives up), so a test decides when a picture is "done". With fail set it refuses at once.
type gatedImages struct {
	url   string
	calls atomic.Int32
	gate  chan struct{}
	once  sync.Once
	fail  atomic.Bool
	big   atomic.Bool // answer with a picture over Discord's upload limit
}

func (g *gatedImages) open() { g.once.Do(func() { close(g.gate) }) }

func newGatedImages(t *testing.T) *gatedImages {
	g := &gatedImages{gate: make(chan struct{})}
	png := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("\x89PNG fake"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		g.calls.Add(1)
		if g.fail.Load() {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"refused","type":"invalid_request_error"}}`))
			return
		}
		select {
		case <-g.gate:
		case <-r.Context().Done():
			return
		}
		png := png
		if g.big.Load() {
			png = "data:image/png;base64," + base64.StdEncoding.EncodeToString(make([]byte, 11<<20))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "",
			"images": []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": png}}}}}}, "usage": map[string]any{"cost": 0.03}})
	}))
	g.url = srv.URL
	t.Cleanup(srv.Close)
	t.Cleanup(g.open) // runs before srv.Close: a handler still waiting must not hold it
	return g
}

type postedPicture struct {
	channel, replyTo, imageID string
	card                      action.Card
}

type postedLine struct{ channel, replyTo, text string }

// pictureGateway is a connected Discord that records what the bot posts on its own.
type pictureGateway struct {
	mu       sync.Mutex
	guilds   []application.GuildInfo
	pictures []postedPicture
	lines    []postedLine
	perms    int64 // what Permissions answers; attach_files by default
	down     bool  // a reconnect: not connected, and no servers listed
	picErr   error // what PostPicture answers instead of posting
}

const attachFiles = int64(1<<10 | 1<<11 | 1<<14 | 1<<15)

func (g *pictureGateway) Connected() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return !g.down
}
func (g *pictureGateway) BotUser() (string, string)                 { return "bot", "masamong" }
func (g *pictureGateway) Channels(string) []application.ChannelInfo { return nil }
func (g *pictureGateway) Permissions(string, string) (int64, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.perms, true
}
func (g *pictureGateway) Guilds() []application.GuildInfo {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.down {
		return nil
	}
	return g.guilds
}

func (g *pictureGateway) setDown(down bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.down = down
}

func (g *pictureGateway) PostPicture(_ context.Context, channelID, replyTo string, card action.Card, imageID string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.picErr != nil {
		return "", g.picErr
	}
	g.pictures = append(g.pictures, postedPicture{channelID, replyTo, imageID, card})
	return "p", nil
}

func (g *pictureGateway) PostChannel(_ context.Context, channelID, replyTo, text string, _ *action.Card) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.lines = append(g.lines, postedLine{channelID, replyTo, text})
	return "l", nil
}

func (g *pictureGateway) counts() (pictures, lines int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.pictures), len(g.lines)
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// idle waits until the picture queue has nothing waiting and nothing being drawn.
func idle(t *testing.T, svc *application.Service) {
	t.Helper()
	waitFor(t, "the picture queue to go idle", func() bool {
		waiting, running := application.ImageQueueStateForTest(svc)
		return waiting == 0 && !running
	})
}

// settle gives work that must NOT happen a moment to (wrongly) start.
func settle() { time.Sleep(150 * time.Millisecond) }

func imageChatSvc(t *testing.T) (*application.Service, *gatedImages, *pictureGateway) {
	t.Helper()
	img := newGatedImages(t)
	svc := setupSvc(t)
	serveAnyServer(t, svc)
	t.Setenv("FAKE_IMG_KEY", "k")
	applyDoc(t, svc, func(d *config.Document) {
		d.Discord.Enabled = true
		d.Providers["img"] = config.Provider{Kind: config.KindOpenAI, BaseURL: img.url, CredentialRef: "env:FAKE_IMG_KEY"}
		d.Models["image"] = config.Model{Provider: "img", Model: "google/gemini-image"}
	})
	gw := &pictureGateway{guilds: []application.GuildInfo{{ID: "g"}}, perms: attachFiles}
	svc.Discord = gw
	return svc, img, gw
}

// drawFor asks for a picture the way the chat tool loop does, as a person in room g/c replying to message msg.
func drawFor(svc *application.Service, user, msg, prompt string) chat.ToolResult {
	run := application.ChatToolForTest(svc, application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: msg, Message: prompt, Speaker: "철수", SpeakerID: user})
	// a queued call returns at once; one that would draw in the turn gives up after 3s, so a regression fails fast
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return run(ctx, "images.generate", map[string]any{"prompt": prompt})
}

func TestChatPictureIsQueuedAtOnceAndPostedLaterAsAReply(t *testing.T) {
	svc, img, gw := imageChatSvc(t)
	began := time.Now()
	res := drawFor(svc, "u1", "m1", "우주복 입은 햄스터")
	if took := time.Since(began); took > 3*time.Second {
		t.Fatalf("the chat turn waited %v for the picture", took)
	}
	if !res.OK || res.Status != "queued" || res.Data["status"] != "queued" || res.Data["job_id"] == "" {
		t.Fatalf("not queued: %+v", res)
	}
	if res.Data["card"] != nil {
		t.Fatal("a queued picture has no card to post after the reply")
	}
	waitFor(t, "the draw to start", func() bool { return img.calls.Load() == 1 })
	if p, l := gw.counts(); p != 0 || l != 0 {
		t.Fatalf("posted before the picture was done: %d pictures, %d lines", p, l)
	}
	img.open()
	waitFor(t, "the picture post", func() bool { p, _ := gw.counts(); return p == 1 })
	gw.mu.Lock()
	post := gw.pictures[0]
	gw.mu.Unlock()
	if post.channel != "c" || post.replyTo != "m1" || post.card.Title == "" || strings.Contains(post.card.Text, "햄스터") {
		t.Fatalf("picture post: %+v", post)
	}
	if f, _, err := svc.ImageFile(post.imageID); err != nil {
		t.Fatalf("the posted picture is not kept: %v", err)
	} else {
		f.Close()
	}
	if img.calls.Load() != 1 {
		t.Fatalf("provider calls = %d", img.calls.Load())
	}
}

func TestRepeatedPictureRequestIsTheSameJob(t *testing.T) {
	svc, img, gw := imageChatSvc(t)
	first := drawFor(svc, "u1", "m1", "Hamster in a space suit")
	second := drawFor(svc, "u1", "m2", "  hamster   in a SPACE suit!! ")
	if first.Status != "queued" || second.Status != "already_queued" || !second.OK || second.Data["job_id"] != first.Data["job_id"] {
		t.Fatalf("first %+v, repeat %+v", first, second)
	}
	img.open()
	idle(t, svc)
	if p, _ := gw.counts(); p != 1 || img.calls.Load() != 1 {
		t.Fatalf("one job, one picture: %d posts, %d provider calls", p, img.calls.Load())
	}
}

func TestPictureQueueCapsPerPersonAndInTotal(t *testing.T) {
	svc, img, gw := imageChatSvc(t)
	if r := drawFor(svc, "u1", "m1", "a cat"); r.Status != "queued" {
		t.Fatalf("u1: %+v", r)
	}
	waitFor(t, "the first draw to start", func() bool { return img.calls.Load() == 1 })
	// the same person while theirs is being drawn: one at a time
	mine := drawFor(svc, "u1", "m1b", "a dog")
	if mine.OK || !strings.Contains(mine.Error, "drawing one right now") {
		t.Fatalf("per-person cap: %+v", mine)
	}
	// three others may wait (the one being drawn is not counted), a fourth may not
	for i, u := range []string{"u2", "u3", "u4"} {
		if r := drawFor(svc, u, "m"+u, "picture "+u); r.Status != "queued" || r.Data["ahead"] != i+1 {
			t.Fatalf("%s: %+v", u, r)
		}
	}
	over := drawFor(svc, "u5", "mu5", "picture u5")
	if over.OK || !strings.Contains(over.Error, "line is full") || strings.Contains(over.Error, "drawing one right now") {
		t.Fatalf("total cap must say the line is full, not that the person has one: %+v", over)
	}
	img.open()
	idle(t, svc)
	if p, _ := gw.counts(); p != 4 || img.calls.Load() != 4 {
		t.Fatalf("%d pictures, %d provider calls; the refused ones must not be drawn", p, img.calls.Load())
	}
	// after they are done the person may ask again
	if r := drawFor(svc, "u1", "m9", "a dog"); r.Status != "queued" {
		t.Fatalf("u1 again: %+v", r)
	}
}

func TestFailedPictureSaysSoInTheChannel(t *testing.T) {
	svc, img, gw := imageChatSvc(t)
	img.fail.Store(true)
	if r := drawFor(svc, "u1", "m1", "a cat"); r.Status != "queued" {
		t.Fatalf("%+v", r)
	}
	waitFor(t, "the failure line", func() bool { _, l := gw.counts(); return l == 1 })
	gw.mu.Lock()
	line, pictures := gw.lines[0], len(gw.pictures)
	gw.mu.Unlock()
	if line.channel != "c" || line.replyTo != "m1" || line.text != svc.Msg("en", "image.failed") || line.text == "" || pictures != 0 {
		t.Fatalf("failure post: %+v (%d pictures)", line, pictures)
	}
}

func TestQueuedPicturesAreDroppedAtShutdownWithoutPosting(t *testing.T) {
	svc, img, gw := imageChatSvc(t)
	drawFor(svc, "u1", "m1", "a cat")
	waitFor(t, "the first draw to start", func() bool { return img.calls.Load() == 1 })
	if r := drawFor(svc, "u2", "m2", "a dog"); r.Status != "queued" {
		t.Fatalf("u2: %+v", r)
	}
	done := make(chan struct{})
	go func() { _ = svc.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown waits for the queue")
	}
	settle()
	if p, l := gw.counts(); p != 0 || l != 0 {
		t.Fatalf("posted at shutdown: %d pictures, %d lines", p, l)
	}
	if img.calls.Load() != 1 {
		t.Fatalf("the waiting job was drawn: %d provider calls", img.calls.Load())
	}
	if r := drawFor(svc, "u3", "m3", "a bird"); r.Status == "queued" {
		t.Fatal("a closed service queued a job")
	}
}

func TestAsyncOffKeepsThePictureSynchronous(t *testing.T) {
	svc, img, gw := imageChatSvc(t)
	img.open()
	applyDoc(t, svc, func(d *config.Document) {
		s := d.Tools["images.generate"]
		s.Defaults = map[string]any{"async": false}
		d.Tools["images.generate"] = s
	})
	res := drawFor(svc, "u1", "m1", "a cat")
	if !res.OK || res.Data["status"] != "ok" || res.Data["image_id"] == "" || res.Data["card"] == nil {
		t.Fatalf("async off must draw before answering: %+v", res)
	}
	settle()
	if p, l := gw.counts(); p != 0 || l != 0 {
		t.Fatalf("the sync picture is posted by the reply path, not the queue: %d, %d", p, l)
	}
}

func TestPictureRequestsOutsideChatStaySynchronous(t *testing.T) {
	svc, img, gw := imageChatSvc(t)
	img.open()
	// a chat turn with no message to reply to (a slash command's chat), and the command itself
	noMsg := application.ChatToolForTest(svc, application.ChatInput{GuildID: "g", ChannelID: "c", Message: "x", SpeakerID: "u1"})
	if r := noMsg(context.Background(), "images.generate", map[string]any{"prompt": "a cat"}); !r.OK || r.Data["status"] != "ok" {
		t.Fatalf("no message: %+v", r)
	}
	cmd := as(svc, "images.generate", map[string]any{"prompt": "a dog"}, &action.Invoker{Via: "command", GuildID: "g", ChannelID: "c", UserID: "u1"})
	if !cmd.OK || cmd.Output["status"] != "ok" {
		t.Fatalf("command: %+v", cmd)
	}
	// the console's own test (no invoker)
	if r := exec(svc, "images.generate", map[string]any{"prompt": "a bird"}); !r.OK || r.Output["status"] != "ok" {
		t.Fatalf("console: %+v", r)
	}
	settle()
	if p, _ := gw.counts(); p != 0 {
		t.Fatalf("%d pictures went through the queue", p)
	}
}

func TestPictureJobsAreDroppedSilentlyForPolicyReasons(t *testing.T) {
	type change struct {
		name  string
		drops int64 // jobs dropped: the waiting one, plus the one being drawn when the change hits it too
		apply func(t *testing.T, svc *application.Service)
	}
	for _, c := range []change{
		{"channel paused", 2, func(t *testing.T, svc *application.Service) {
			if r := exec(svc, "runtime.pause", map[string]any{"scope": "channel", "guild_id": "g", "channel_id": "c"}); !r.OK {
				t.Fatal(r.Error)
			}
		}},
		{"requester blocked", 1, func(t *testing.T, svc *application.Service) {
			applyDoc(t, svc, func(d *config.Document) { d.Access.BlockedUsers = append(d.Access.BlockedUsers, "u2") })
		}},
		{"requester withdrew consent", 1, func(t *testing.T, svc *application.Service) {
			if r := asUser(svc, "u2", "privacy.consent", map[string]any{"scope": "memory", "decision": "withdraw"}); !r.OK {
				t.Fatal(r.Error)
			}
		}},
		{"picture tool switched off", 2, func(t *testing.T, svc *application.Service) {
			if r := exec(svc, "tools.configure", map[string]any{"id": "images.generate", "enabled": "off"}); !r.OK {
				t.Fatal(r.Error)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			svc, img, gw := imageChatSvc(t)
			drawFor(svc, "u1", "m1", "a cat")
			waitFor(t, "the first draw to start", func() bool { return img.calls.Load() == 1 })
			if r := drawFor(svc, "u2", "m2", "a dog"); r.Status != "queued" {
				t.Fatalf("u2: %+v", r)
			}
			c.apply(t, svc)
			img.open()
			waitFor(t, "the drops", func() bool { return application.ImageDropsForTest(svc) >= c.drops })
			idle(t, svc)
			if got := application.ImageDropsForTest(svc); got != c.drops {
				t.Fatalf("dropped %d jobs, want %d", got, c.drops)
			}
			if img.calls.Load() != 1 {
				t.Fatalf("the dropped job was drawn: %d provider calls", img.calls.Load())
			}
			if _, l := gw.counts(); l != 0 {
				t.Fatalf("a dropped job posted a line")
			}
			gw.mu.Lock()
			defer gw.mu.Unlock()
			for _, p := range gw.pictures {
				if p.replyTo == "m2" {
					t.Fatalf("a dropped job posted its picture")
				}
			}
		})
	}
}

// A reconnecting gateway (not connected, no servers listed for a moment) is not a reason to throw away a
// picture that is paid for: the post waits for it and then goes out.
func TestPictureWaitsOutAReconnectBeforePosting(t *testing.T) {
	svc, img, gw := imageChatSvc(t)
	if r := drawFor(svc, "u1", "m1", "a cat"); r.Status != "queued" {
		t.Fatalf("%+v", r)
	}
	waitFor(t, "the draw to start", func() bool { return img.calls.Load() == 1 })
	gw.setDown(true)
	img.open()
	time.Sleep(400 * time.Millisecond) // the picture is done and waiting for the gateway
	if p, l := gw.counts(); p != 0 || l != 0 || application.ImageDropsForTest(svc) != 0 {
		t.Fatalf("while the gateway is down nothing is posted or dropped: %d, %d, %d drops", p, l, application.ImageDropsForTest(svc))
	}
	gw.setDown(false)
	waitFor(t, "the picture post", func() bool { p, _ := gw.counts(); return p == 1 })
	if application.ImageDropsForTest(svc) != 0 {
		t.Fatal("the job was dropped")
	}
}

// Discord stays away for good: the picture that is already paid for is still tried (posting is REST), and
// the job that has not started is not drawn; its person hears image.failed.
func TestPictureThatCannotStartForADisconnectedGatewaySaysFailed(t *testing.T) {
	restore := application.SetImageConnectWaitForTest(150*time.Millisecond, 10*time.Millisecond)
	defer restore()
	svc, img, gw := imageChatSvc(t)
	drawFor(svc, "u1", "m1", "a cat")
	waitFor(t, "the first draw to start", func() bool { return img.calls.Load() == 1 })
	if r := drawFor(svc, "u2", "m2", "a dog"); r.Status != "queued" {
		t.Fatalf("%+v", r)
	}
	gw.setDown(true)
	img.open()
	idle(t, svc)
	gw.mu.Lock()
	defer gw.mu.Unlock()
	if len(gw.pictures) != 1 || gw.pictures[0].replyTo != "m1" {
		t.Fatalf("the paid-for picture must be tried anyway: %+v", gw.pictures)
	}
	if len(gw.lines) != 1 || gw.lines[0].replyTo != "m2" || gw.lines[0].text != svc.Msg("en", "image.failed") {
		t.Fatalf("lines: %+v", gw.lines)
	}
	if img.calls.Load() != 1 {
		t.Fatalf("the job that could not start was drawn: %d provider calls", img.calls.Load())
	}
}

func TestPictureIsNotDrawnWhereTheBotCannotAttachFiles(t *testing.T) {
	svc, img, gw := imageChatSvc(t)
	img.open()
	gw.mu.Lock()
	gw.perms = 1<<10 | 1<<11 | 1<<14 // no attach_files
	gw.mu.Unlock()
	if r := drawFor(svc, "u1", "m1", "a cat"); r.Status != "queued" {
		t.Fatalf("%+v", r)
	}
	idle(t, svc)
	if img.calls.Load() != 0 {
		t.Fatalf("drew a picture that cannot be posted: %d provider calls", img.calls.Load())
	}
	if p, l := gw.counts(); p != 0 || l != 1 {
		t.Fatalf("%d pictures, %d lines", p, l)
	}
}

func TestFailedPicturePostSaysSoUnlessItWasAPermissionOrTheRequestIsGone(t *testing.T) {
	for name, tc := range map[string]struct {
		err   error
		lines int
		drops int64
	}{
		"refused or retries used up": {application.ErrNotDelivered, 1, 0},
		"file missing":               {errors.New("discord: generated picture is not available"), 1, 0},
		"missing permission":         {application.ErrPostForbidden, 0, 0},
		"request deleted":            {application.ErrReplyTargetGone, 0, 1},
	} {
		t.Run(name, func(t *testing.T) {
			svc, img, gw := imageChatSvc(t)
			img.open()
			gw.mu.Lock()
			gw.picErr = tc.err
			gw.mu.Unlock()
			if r := drawFor(svc, "u1", "m1", "a cat"); r.Status != "queued" {
				t.Fatalf("%+v", r)
			}
			idle(t, svc)
			p, l := gw.counts()
			if p != 0 || l != tc.lines || application.ImageDropsForTest(svc) != tc.drops {
				t.Fatalf("%d pictures, %d lines (want %d), %d drops (want %d)", p, l, tc.lines, application.ImageDropsForTest(svc), tc.drops)
			}
		})
	}
}

func TestPictureOverDiscordsUploadLimitSaysFailedInsteadOfPosting(t *testing.T) {
	svc, img, gw := imageChatSvc(t)
	img.big.Store(true)
	img.open()
	if r := drawFor(svc, "u1", "m1", "a cat"); r.Status != "queued" {
		t.Fatalf("%+v", r)
	}
	idle(t, svc)
	if p, l := gw.counts(); p != 0 || l != 1 {
		t.Fatalf("%d pictures, %d lines", p, l)
	}
}

// The requester deletes the request while the job waits or is being drawn: nothing of it is drawn
// (waiting) or posted (drawn), and no failure line is posted for it either.
func TestDeletedRequestCancelsItsPicture(t *testing.T) {
	svc, img, gw := imageChatSvc(t)
	drawFor(svc, "u1", "m1", "a cat")
	waitFor(t, "the first draw to start", func() bool { return img.calls.Load() == 1 })
	drawFor(svc, "u2", "m2", "a dog")
	svc.CancelPictureJobs("other-channel", []string{"m2"}) // another channel's message with the same ID is not it
	if waiting, _ := application.ImageQueueStateForTest(svc); waiting != 1 {
		t.Fatalf("a message of another channel cancelled a job: %d waiting", waiting)
	}
	svc.CancelPictureJobs("c", []string{"m2"})
	if waiting, _ := application.ImageQueueStateForTest(svc); waiting != 0 {
		t.Fatalf("the waiting job was not cancelled: %d", waiting)
	}
	svc.CancelPictureJobs("c", []string{"m1"}) // the one being drawn
	img.open()
	idle(t, svc)
	if p, l := gw.counts(); p != 0 || l != 0 || img.calls.Load() != 1 {
		t.Fatalf("%d pictures, %d lines, %d draws", p, l, img.calls.Load())
	}
	if application.ImageDropsForTest(svc) != 2 {
		t.Fatalf("drops = %d", application.ImageDropsForTest(svc))
	}
	// the person may ask again
	if r := drawFor(svc, "u2", "m3", "a dog"); r.Status != "queued" {
		t.Fatalf("%+v", r)
	}
	idle(t, svc)
	if p, _ := gw.counts(); p != 1 {
		t.Fatalf("%d pictures", p)
	}
}

func TestRepeatedRequestReportsItsPlaceInTheLine(t *testing.T) {
	svc, img, _ := imageChatSvc(t)
	drawFor(svc, "u1", "m1", "a cat")
	waitFor(t, "the first draw to start", func() bool { return img.calls.Load() == 1 })
	drawFor(svc, "u2", "m2", "a dog")
	again1 := drawFor(svc, "u1", "m3", "A cat.")
	again2 := drawFor(svc, "u2", "m4", "a dog")
	if again1.Status != "already_queued" || again1.Data["ahead"] != 0 || again2.Status != "already_queued" || again2.Data["ahead"] != 1 {
		t.Fatalf("the running job is place 0, the waiting one after it: %+v / %+v", again1.Data, again2.Data)
	}
}

func TestSameWordsInAnotherAspectRatioAreAnotherRequest(t *testing.T) {
	svc, img, _ := imageChatSvc(t)
	run := application.ChatToolForTest(svc, application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "m1", SpeakerID: "u1"})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if r := run(ctx, "images.generate", map[string]any{"prompt": "a cat", "aspect_ratio": "1:1"}); r.Status != "queued" {
		t.Fatalf("%+v", r)
	}
	r := run(ctx, "images.generate", map[string]any{"prompt": "a cat", "aspect_ratio": "16:9"})
	if r.Status == "already_queued" {
		t.Fatalf("another ratio is another picture: %+v", r)
	}
	_ = img
}

// A repeat that the queue answers, and a refusal because the line is full, do not use up the person's
// daily picture allowance.
func TestQueueAnswersDoNotCountAgainstTheDailyLimit(t *testing.T) {
	svc, img, _ := imageChatSvc(t)
	applyDoc(t, svc, func(d *config.Document) {
		s := d.Tools["images.generate"]
		s.Limits = tools.Limits{PerUserPerDay: 1}
		d.Tools["images.generate"] = s
	})
	if r := drawFor(svc, "u1", "m1", "a cat"); r.Status != "queued" {
		t.Fatalf("%+v", r)
	}
	waitFor(t, "the draw to start", func() bool { return img.calls.Load() == 1 })
	for i := 0; i < 3; i++ { // repeats, and another request while theirs is running
		if r := drawFor(svc, "u1", "m2", "a cat"); r.Status != "already_queued" {
			t.Fatalf("repeat %d counted against the limit: %+v", i, r)
		}
	}
	if r := drawFor(svc, "u1", "m3", "a dog"); !strings.Contains(r.Error, "drawing one right now") {
		t.Fatalf("%+v", r)
	}
	img.open()
	idle(t, svc)
	// the one real picture used the allowance of one
	if r := drawFor(svc, "u1", "m4", "a bird"); r.OK {
		t.Fatalf("the daily limit of 1 was not applied: %+v", r)
	}
}

// After shutdown a request gets a short failure, not a synchronous draw.
func TestClosedServiceDoesNotDrawSynchronously(t *testing.T) {
	svc, img, gw := imageChatSvc(t)
	drawFor(svc, "u1", "m1", "a cat") // starts the worker
	waitFor(t, "the draw to start", func() bool { return img.calls.Load() == 1 })
	_ = svc.Close()
	r := drawFor(svc, "u2", "m2", "a dog")
	if r.OK || img.calls.Load() != 1 {
		t.Fatalf("a closed service answered %+v after %d draws", r, img.calls.Load())
	}
	if p, l := gw.counts(); p != 0 || l != 0 {
		t.Fatalf("%d, %d", p, l)
	}
}

func TestBackgroundPictureIsRecordedUnderItsTurn(t *testing.T) {
	svc, img, gw := imageChatSvc(t)
	img.open()
	ctx := observability.WithTurn(context.Background(), "turn-img-1")
	run := application.ChatToolForTest(svc, application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "m1", SpeakerID: "u1"})
	if r := run(ctx, "images.generate", map[string]any{"prompt": "a cat"}); r.Status != "queued" {
		t.Fatalf("%+v", r)
	}
	waitFor(t, "the picture post", func() bool { p, _ := gw.counts(); return p == 1 })
	waitFor(t, "the ledger rows", func() bool { return len(application.ToolCallStatusesForTest(svc, "turn-img-1")) >= 2 })
	got := application.ToolCallStatusesForTest(svc, "turn-img-1")
	if len(got) != 2 || got[0] != "images.generate:queued" || got[1] != "images.generate:ok" {
		t.Fatalf("ledger rows of the turn: %v", got)
	}
}

func TestPictureToolSettingIsForTheOperatorOnly(t *testing.T) {
	svc, _, _ := imageChatSvc(t)
	tl, _ := tools.Find(svc.ToolRegistryForTest(), "images.generate")
	var operatorOnly bool
	for _, in := range tl.Inputs {
		if in.Name == "async" {
			operatorOnly = in.Operator
		}
	}
	if !operatorOnly {
		t.Fatal("async must be an operator-only input")
	}
	res := exec(svc, "tools.list", nil)
	if !res.OK {
		t.Fatal(res.Error)
	}
	raw, _ := json.Marshal(res.Output)
	var out struct {
		Tools []struct {
			ID    string         `json:"id"`
			Queue map[string]any `json:"queue"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tl := range out.Tools {
		if tl.ID == "images.generate" {
			found = tl.Queue != nil && tl.Queue["waiting"] == float64(0) && tl.Queue["running"] == false
		}
	}
	if !found {
		t.Fatalf("tools.list shows no queue for the picture tool: %s", raw)
	}
}
