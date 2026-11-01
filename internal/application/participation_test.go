package application_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	osexec "os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/application"
	"masamong/internal/config"
)

// judgeServer answers the participation judge with judge (JSON) and every other call with answer;
// delay slows the judge down (a conversation can move on meanwhile).
type judgeServer struct {
	mu     sync.Mutex
	judge  string
	answer string
	delay  time.Duration
	judged atomic.Int32
	// hold, when set, parks every judge call until release is closed and tells entered it is parked
	hold        bool
	entered     chan struct{}
	release     chan struct{}
	releaseOnce sync.Once
}

func (j *judgeServer) releaseHold() {
	j.mu.Lock()
	release := j.release
	j.mu.Unlock()
	if release != nil {
		j.releaseOnce.Do(func() { close(release) })
	}
}

func (j *judgeServer) set(judge string) {
	j.mu.Lock()
	j.judge = judge
	j.mu.Unlock()
}

func (j *judgeServer) start(t *testing.T) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		j.mu.Lock()
		content, delay := j.answer, time.Duration(0)
		if strings.Contains(string(body), `\"speak\"`) {
			j.judged.Add(1)
			content, delay = j.judge, j.delay
		} else if strings.Contains(string(body), "tool_plan") {
			content = `{"memory_requirement":"none","tool_plan":[]}`
		}
		hold, entered, release := j.hold, j.entered, j.release
		j.mu.Unlock()
		if hold && strings.Contains(string(body), `\"speak\"`) {
			ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
			defer cancel()
			select {
			case entered <- struct{}{}:
			case <-release:
			case <-ctx.Done():
				return
			}
			select {
			case <-release:
			case <-ctx.Done():
				return
			}
		}
		time.Sleep(delay)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "cost": 0.00001},
		})
	}))
	t.Cleanup(func() { j.releaseHold(); srv.Close() })
	return srv.URL
}

func partSvc(t *testing.T, mode string) (*application.Service, *postGateway, *judgeServer) {
	js := &judgeServer{judge: `{"speak": true, "kind": "answer", "score": 0.9, "reason": "unanswered question"}`, answer: "도윤이가 챙기기로 했어."}
	url := js.start(t)
	svc := routedSvc(t, url, url)
	applyDoc(t, svc, func(d *config.Document) {
		d.Discord.Enabled = true
		d.Participation = config.Participation{Enabled: true, Mode: mode}
		d.Memory.Capture = true
		d.Guilds = map[string]config.GuildSettings{"g": {Locale: "ko", Channels: map[string]config.ChannelPolicy{"c": {Respond: "mention", Participation: mode, Capture: true}}}}
	})
	gw := &postGateway{}
	svc.Discord = gw
	application.SetParticipationWait(svc, 30*time.Millisecond)
	return svc, gw, js
}

func say(svc *application.Service, id, user, text string) {
	svc.Observe(application.ObservedMessage{GuildID: "g", ChannelID: "c", MessageID: id, AuthorID: user, AuthorName: user, Content: text, At: time.Now()})
}

// partLog waits until the log has n entries and returns them, newest first.
func partLog(t *testing.T, svc *application.Service, n int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		r := exec(svc, "participation.mode", map[string]any{"guild_id": "g", "channel_id": "c"})
		var out struct {
			Log []map[string]any `json:"log"`
		}
		raw, _ := json.Marshal(r.Output)
		_ = json.Unmarshal(raw, &out)
		if len(out.Log) >= n {
			return out.Log
		}
		if time.Now().After(deadline) {
			t.Fatalf("waited for %d log entries, have %+v", n, out.Log)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestParticipationIsOffUnlessBothSwitchesAllow(t *testing.T) {
	svc, gw, js := partSvc(t, "shadow")
	applyDoc(t, svc, func(d *config.Document) { d.Participation.Enabled = false })
	say(svc, "m1", "u1", "누가 텐트 챙기기로 했지?")
	time.Sleep(150 * time.Millisecond)
	if js.judged.Load() != 0 || gw.postCount() != 0 {
		t.Fatal("the instance switch is off")
	}
	// the instance allows shadow at most: a conservative channel stays in shadow
	applyDoc(t, svc, func(d *config.Document) {
		d.Participation = config.Participation{Enabled: true, Mode: "shadow"}
		d.Guilds["g"].Channels["c"] = config.ChannelPolicy{Respond: "mention", Participation: "conservative"}
	})
	r := exec(svc, "participation.mode", map[string]any{"guild_id": "g", "channel_id": "c"})
	if r.Output["effective"] != "shadow" {
		t.Fatalf("effective mode: %+v", r.Output)
	}
}

func TestParticipationShadowRecordsWithoutSpeaking(t *testing.T) {
	svc, gw, _ := partSvc(t, "shadow")
	say(svc, "m1", "u1", "누가 텐트 챙기기로 했지?")
	log := partLog(t, svc, 1)
	if log[0]["state"] != "shadow" || !strings.Contains(log[0]["draft"].(string), "도윤") || gw.postCount() != 0 {
		t.Fatalf("shadow: %+v, posts %d", log[0], gw.postCount())
	}
	// the operator's verdict on it
	if r := exec(svc, "participation.mode", map[string]any{"review": log[0]["id"], "verdict": "good"}); !r.OK {
		t.Fatal(r.Error)
	}
	if r := exec(svc, "participation.mode", map[string]any{"guild_id": "g", "channel_id": "c"}); r.Output["reviewed"].(map[string]int)["good"] != 1 {
		t.Fatalf("reviewed: %+v", r.Output["reviewed"])
	}
}

func TestParticipationStaysOutOfWhatPeopleHandle(t *testing.T) {
	svc, gw, js := partSvc(t, "conservative")
	application.SetParticipationWait(svc, 300*time.Millisecond)
	// someone else answers during the wait: no judgement, no remark
	say(svc, "m1", "u1", "누가 텐트 챙기기로 했지?")
	say(svc, "m2", "u2", "내가 챙겨 갈게")
	log := partLog(t, svc, 1)
	if log[0]["state"] != "cancelled" || log[0]["reason"] != "people_answered" || js.judged.Load() != 0 {
		t.Fatalf("people answered: %+v (judged %d)", log[0], js.judged.Load())
	}
	// messages to the bot or to someone else are not candidates
	svc.Observe(application.ObservedMessage{GuildID: "g", ChannelID: "c", MessageID: "m3", AuthorID: "u1", Content: "마사몽 이거 뭐야?", Addressed: true})
	svc.Observe(application.ObservedMessage{GuildID: "g", ChannelID: "c", MessageID: "m4", AuthorID: "u1", Content: "@영희 너 언제 와?", ToSomeone: true})
	// a person who withdrew is never chimed in on
	if r := as(svc, "privacy.consent", map[string]any{"scope": "participation", "decision": "withdraw"}, inChannel("u9")); !r.OK {
		t.Fatal(r.Error)
	}
	say(svc, "m5", "u9", "누가 우산 가져왔어?")
	time.Sleep(500 * time.Millisecond)
	if js.judged.Load() != 0 || gw.postCount() != 0 {
		t.Fatalf("judged %d, posted %d", js.judged.Load(), gw.postCount())
	}
	st := exec(svc, "participation.mode", map[string]any{"guild_id": "g", "channel_id": "c"}).Output["filtered"].(map[string]int)
	if st["opted_out"] != 1 {
		t.Fatalf("filtered: %v", st)
	}
}

func TestParticipationConservativeSpeaksOnceWithinLimits(t *testing.T) {
	svc, gw, _ := partSvc(t, "conservative")
	say(svc, "m1", "u1", "누가 텐트 챙기기로 했지?")
	log := partLog(t, svc, 1)
	if log[0]["state"] != "spoken" || gw.postCount() != 1 || !strings.Contains(gw.posts[0], "도윤") {
		t.Fatalf("spoken: %+v %v", log[0], gw.posts)
	}
	eng, _ := svc.MemoryEngine()
	if turns, _ := eng.Store.RecentChatTurns("discord:g:c", 10); len(turns) != 2 || !strings.Contains(turns[1].Content, "도윤") {
		t.Fatalf("what was said is the chat context: %+v", turns)
	}
	// the cooldown keeps the next question to people
	say(svc, "m2", "u2", "그럼 버너는 누가 가져와?")
	time.Sleep(200 * time.Millisecond)
	if gw.postCount() != 1 {
		t.Fatal("spoke again inside the cooldown")
	}
	if st := exec(svc, "participation.mode", map[string]any{"guild_id": "g", "channel_id": "c"}).Output["filtered"].(map[string]int); st["cooldown"] != 1 {
		t.Fatalf("filtered: %v", st)
	}
}

func TestParticipationJudgeSilenceAndMovedConversation(t *testing.T) {
	svc, gw, js := partSvc(t, "conservative")
	js.set(`{"speak": false, "kind": "none", "score": 0.1, "reason": "rhetorical"}`)
	say(svc, "m1", "u1", "이게 말이 돼?")
	if log := partLog(t, svc, 1); log[0]["state"] != "silent" {
		t.Fatalf("silent: %+v", log[0])
	}
	// the conversation moves on while the judge thinks: nothing is said
	js.set(`{"speak": true, "kind": "answer", "score": 0.95, "reason": "q"}`)
	js.mu.Lock()
	js.delay = 300 * time.Millisecond
	js.mu.Unlock()
	svc2, gw2, js2 := partSvc(t, "conservative")
	js2.mu.Lock()
	js2.delay = 300 * time.Millisecond
	js2.mu.Unlock()
	say(svc2, "m1", "u1", "누가 텐트 챙기기로 했지?")
	time.Sleep(150 * time.Millisecond) // the wait is over, the judge is thinking
	say(svc2, "m2", "u1", "아 됐다 찾았어")
	log := partLog(t, svc2, 1)
	if log[0]["state"] != "cancelled" || log[0]["reason"] != "conversation_moved" || gw2.postCount() != 0 || gw.postCount() != 0 {
		t.Fatalf("moved on: %+v", log[0])
	}
	// the draft that was not said is not in the channel conversation either
	eng, _ := svc2.MemoryEngine()
	if turns, _ := eng.Store.RecentChatTurns("discord:g:c", 10); len(turns) != 0 {
		t.Fatalf("an unsent draft became chat context: %+v", turns)
	}
}

func TestParticipationOffersAPlanReminder(t *testing.T) {
	svc, gw, js := partSvc(t, "conservative")
	js.set(`{"speak": true, "kind": "event", "score": 0.9, "reason": "plan", "when": "토요일 저녁 7시", "what": "강남 모임"}`)
	say(svc, "m1", "u1", "토요일 저녁 7시에 강남에서 보자")
	log := partLog(t, svc, 1)
	if log[0]["state"] != "spoken" || gw.postCount() != 1 || !strings.Contains(gw.posts[0], "강남 모임") {
		t.Fatalf("offer: %+v %v", log[0], gw.posts)
	}
}

// The console polls the list and status forms of writing Actions; they must not fill the change log.
func TestReadFormsLeaveNoChangeRecord(t *testing.T) {
	svc := setupSvc(t)
	count := func() int { return len(out(t, svc, "audit.list", nil)["entries"].([]any)) }
	before := count()
	for _, c := range []struct {
		act string
		in  map[string]any
	}{
		{"participation.mode", map[string]any{"guild_id": "g", "channel_id": "c"}},
		{"reminders.update", map[string]any{"op": "list"}},
		{"privacy.consent", nil},
		{"preferences.set", map[string]any{"user_id": "10001"}},
		{"access.policy", nil},
		{"fortune.subscribe", map[string]any{"op": "status"}},
		{"notices.subscribe", map[string]any{"op": "list"}},
		{"memory.backfill", map[string]any{"status": true}},
		{"memory.blocks.prepare", map[string]any{"source_room": "kakao:club", "dry_run": true}}, // a cost estimate
	} {
		_ = exec(svc, c.act, c.in)
	}
	if n := count(); n != before {
		t.Fatalf("reads added %d change records", n-before)
	}
	if r := exec(svc, "participation.mode", map[string]any{"guild_id": "g", "channel_id": "c", "mode": "shadow"}); !r.OK {
		t.Fatal(r.Error)
	}
	if n := count(); n != before+1 {
		t.Fatalf("a change is recorded: %d", n-before)
	}
}

// A person a server's admins blocked is not chimed in on in that server (the instance-wide list is
// not the only one).
func TestParticipationSkipsPeopleTheServerBlocked(t *testing.T) {
	svc, gw, js := partSvc(t, "conservative")
	applyDoc(t, svc, func(d *config.Document) {
		g := d.Guilds["g"]
		g.BlockedUsers = []string{"u1"}
		d.Guilds["g"] = g
	})
	say(svc, "m1", "u1", "누가 텐트 챙기기로 했지?")
	time.Sleep(300 * time.Millisecond)
	if js.judged.Load() != 0 || gw.postCount() != 0 {
		t.Fatalf("judged %d, posted %d", js.judged.Load(), gw.postCount())
	}
	st := exec(svc, "participation.mode", map[string]any{"guild_id": "g", "channel_id": "c"}).Output["filtered"].(map[string]int)
	if st["blocked"] != 1 {
		t.Fatalf("filtered: %v", st)
	}
}

// A candidate being judged is not pending any more, but its channel entry must outlive a sweep: if the
// entry is dropped and made again by the next message, the candidate's version matches the new entry's
// first message and the remark is posted into a conversation that has moved on.
func TestParticipationSweepKeepsAChannelWhoseCandidateIsBeingJudged(t *testing.T) {
	svc, gw, js := partSvc(t, "conservative")
	js.mu.Lock()
	js.hold, js.entered, js.release = true, make(chan struct{}, 1), make(chan struct{})
	js.mu.Unlock()
	// Registered after partSvc, so even a timeout releases the handler before service/server close.
	t.Cleanup(js.releaseHold)
	say(svc, "m1", "u1", "누가 텐트 챙기기로 했지?")
	select {
	case <-js.entered:
	case <-time.After(20 * time.Second):
		t.Fatal("the judge was never asked")
	}
	// over the cap, with this channel the quietest: it is the one the cap would drop
	application.AddParticipationChannelsForTest(svc, 600, time.Now().Add(time.Hour))
	application.TickParticipationSweepForTest(svc, time.Now())
	say(svc, "m2", "u2", "아 됐다 찾았어")
	js.releaseHold()
	log := partLog(t, svc, 1)
	if log[0]["state"] != "cancelled" || log[0]["reason"] != "conversation_moved" || gw.postCount() != 0 {
		t.Fatalf("the candidate was posted after its channel was swept: %+v, posts %d", log[0], gw.postCount())
	}
}

// Exercise a real Fatal path in a child test process: server cleanup must release the blocked handler
// even though the test never reaches its successful-path release.
func TestParticipationHeldRequestFailureCleanup(t *testing.T) {
	if os.Getenv("MASAMONG_TEST_HELD_JUDGE_FAILURE") == "1" {
		js := &judgeServer{hold: true, entered: make(chan struct{}, 1), release: make(chan struct{})}
		url := js.start(t)
		go func() {
			resp, err := http.Post(url+"/chat/completions", "application/json", strings.NewReader(`{"content":"\"speak\""}`))
			if err == nil {
				_ = resp.Body.Close()
			}
		}()
		select {
		case <-js.entered:
		case <-time.After(20 * time.Second):
			t.Fatal("held-request probe did not reach the judge")
		}
		t.Fatal("forced held-request failure")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := osexec.CommandContext(ctx, exe, "-test.run=^TestParticipationHeldRequestFailureCleanup$", "-test.timeout=25s")
	cmd.Env = append(os.Environ(), "MASAMONG_TEST_HELD_JUDGE_FAILURE=1")
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil || err == nil || !strings.Contains(string(out), "forced held-request failure") || strings.Contains(string(out), "test timed out") {
		t.Fatalf("failed-test cleanup did not finish: %v", err)
	}
}
