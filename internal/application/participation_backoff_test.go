package application_test

import (
	"testing"
	"time"

	"masamong/internal/application"
	"masamong/internal/features/privacy"
)

func filteredOf(t *testing.T, svc *application.Service) map[string]int {
	t.Helper()
	return exec(svc, "participation.mode", map[string]any{"guild_id": "g", "channel_id": "c"}).Output["filtered"].(map[string]int)
}

func waitConv(t *testing.T, svc *application.Service, kind, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, d := range svc.ConversationLogForTest(kind) {
			if d == want {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no %s/%s in %v", kind, want, svc.ConversationLogForTest(kind))
}

// "조용히 해" right after a remark mutes the channel; the same words between people do nothing.
func TestParticipationQuietCueMutes(t *testing.T) {
	svc, gw, _ := partSvc(t, "conservative")
	say(svc, "q0", "u2", "조용히 해") // no remark of the bot's yet: said to people
	time.Sleep(100 * time.Millisecond)
	if len(svc.ConversationLogForTest("backoff")) != 0 {
		t.Fatal("a quiet cue between people is not for the bot")
	}
	say(svc, "m1", "u1", "누가 텐트 챙기기로 했지?")
	if log := partLog(t, svc, 1); log[0]["state"] != "spoken" || gw.postCount() != 1 {
		t.Fatalf("spoken: %+v", log[0])
	}
	say(svc, "m2", "u2", "조용히 해!")
	waitConv(t, svc, "backoff", "quiet_cue")
	say(svc, "m3", "u3", "그럼 버너는 누가 가져와?")
	time.Sleep(100 * time.Millisecond)
	if f := filteredOf(t, svc); f["muted"] != 1 {
		t.Fatalf("muted channel: %v", f)
	}
}

// Right after someone called the bot it does not chime in; "끼어들기 꺼" is the speaker's own opt-out.
func TestParticipationAfterCallAndChimeOff(t *testing.T) {
	svc, gw, _ := partSvc(t, "conservative")
	svc.Observe(application.ObservedMessage{GuildID: "g", ChannelID: "c", MessageID: "c1", AuthorID: "u9", AuthorName: "u9",
		Content: "@마사몽 안녕", At: time.Now(), Addressed: true, BotCall: "mention"})
	say(svc, "m1", "u1", "누가 텐트 챙기기로 했지?")
	time.Sleep(150 * time.Millisecond)
	if f := filteredOf(t, svc); f["after_call"] != 1 || gw.postCount() != 0 {
		t.Fatalf("just called: %v posts %d", f, gw.postCount())
	}
	say(svc, "m2", "u4", "끼어들기 꺼")
	waitConv(t, svc, "backoff", "chime_off")
	if svc.ConsentAllows("u4", privacy.Participation) {
		t.Fatal("the speaker's chiming-in consent is withdrawn")
	}
	if !svc.ConsentAllows("u1", privacy.Participation) {
		t.Fatal("only the speaker's own")
	}
}

// "끼어들기 꺼" is the same withdrawal as the privacy command: what collect mode recorded about the speaker
// goes with it.
func TestChimeOffCueDropsCollectedRows(t *testing.T) {
	svc, _, _ := partSvc(t, "collect")
	say(svc, "m1", "u1", "누가 텐트 챙기기로 했지?")
	if rows := waitRows(t, svc, 1); rows[0].Mode != "collect" {
		t.Fatalf("collected: %+v", rows)
	}
	switchMode(t, svc, "conservative")
	say(svc, "m2", "u1", "끼어들기 꺼")
	waitConv(t, svc, "backoff", "chime_off")
	for _, r := range plRows(t, svc) {
		if r.Mode == "collect" {
			t.Fatalf("a collected row about the speaker stays: %+v", r)
		}
	}
}

// A 👎 on a remark of its own mutes the channel.
func TestParticipationDislikeMutes(t *testing.T) {
	svc, gw, _ := partSvc(t, "conservative")
	say(svc, "m1", "u1", "누가 텐트 챙기기로 했지?")
	if log := partLog(t, svc, 1); log[0]["state"] != "spoken" || gw.postCount() != 1 {
		t.Fatalf("spoken: %+v", log[0])
	}
	if !svc.RateReply("g", "msg1", "u2", false, "") {
		t.Fatal("the remark is a reply of the bot's")
	}
	waitConv(t, svc, "backoff", "disliked")
	say(svc, "m3", "u3", "그럼 버너는 누가 가져와?")
	time.Sleep(100 * time.Millisecond)
	if f := filteredOf(t, svc); f["muted"] != 1 {
		t.Fatalf("muted channel: %v", f)
	}
}

func TestParticipationMuteSurvivesRestart(t *testing.T) {
	svc, gw, _ := partSvc(t, "conservative")
	say(svc, "m1", "u1", "누가 텐트 챙기기로 했지?")
	if log := partLog(t, svc, 1); log[0]["state"] != "spoken" || gw.postCount() != 1 {
		t.Fatal("expected a participation reply before the quiet cue")
	}
	say(svc, "m2", "u2", "조용히 해!")
	waitConv(t, svc, "backoff", "quiet_cue")
	svc = restartServiceForTest(t, svc)
	say(svc, "m3", "u3", "누가 버너 가져와?")
	if f := filteredOf(t, svc); f["muted"] != 1 {
		t.Fatal("participation mute was not restored after restart")
	}
}

func TestParticipationDislikeAfterRestartMutes(t *testing.T) {
	svc, gw, _ := partSvc(t, "conservative")
	say(svc, "m1", "u1", "누가 텐트 챙기기로 했지?")
	if log := partLog(t, svc, 1); log[0]["state"] != "spoken" || gw.postCount() != 1 {
		t.Fatal("expected a participation reply before restart")
	}
	svc = restartServiceForTest(t, svc)
	if !svc.RateReply("g", "msg1", "u2", false, "") {
		t.Fatal("the persisted reply was not available for rating")
	}
	waitConv(t, svc, "backoff", "disliked")
	say(svc, "m2", "u3", "누가 버너 가져와?")
	if f := filteredOf(t, svc); f["muted"] != 1 {
		t.Fatal("a dislike after restart did not mute participation")
	}
}
