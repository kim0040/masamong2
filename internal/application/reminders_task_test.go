package application_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/clock"
	"masamong/internal/config"
)

func viaChat(user string) *action.Invoker {
	return &action.Invoker{Via: "chat", GuildID: "g", ChannelID: "c", UserID: user, Name: "철수"}
}

// "매일 오전 9시에 뉴스 정리해서 알려줘": the run needs a topic, so the loop passes the question to ask.
// Nothing is saved, no card is posted, and the answer is told to ask instead of claiming it was set.
func TestTaskMissingDetailIsAskedNotSaved(t *testing.T) {
	seoul, _ := time.LoadLocation("Asia/Seoul")
	svc, _ := remindSvc(t, time.Date(2026, 10, 15, 20, 0, 0, 0, seoul))
	r := as(svc, "reminders.propose", map[string]any{"kind": "task", "when": "오전 9시", "repeat": "daily", "what": "뉴스 정리",
		"ask": "어떤 분야 뉴스로 할까요?"}, viaChat("u1"))
	if !r.OK || r.Output["status"] != "needs_detail" || r.Output["question"] != "어떤 분야 뉴스로 할까요?" || r.Output["card"] != nil {
		t.Fatalf("needs_detail: %+v", r)
	}
	if list := as(svc, "reminders.update", map[string]any{"op": "list"}, viaChat("u1")); len(list.Output["reminders"].([]map[string]any)) != 0 {
		t.Fatalf("something was saved: %+v", list.Output)
	}
}

// Once the request is whole the task is set at once, in the channel it was asked in, and the chat answer
// gets what was set (no card). Saying "뉴스 예약 취소해 줘" finds it by its words and cancels it.
func TestTaskSetFromChatAndCancelledByWords(t *testing.T) {
	seoul, _ := time.LoadLocation("Asia/Seoul")
	svc, _ := remindSvc(t, time.Date(2026, 10, 15, 20, 0, 0, 0, seoul))
	r := as(svc, "reminders.propose", map[string]any{"kind": "task", "when": "오전 9시", "repeat": "daily", "what": "국제 정세 뉴스",
		"task": "오늘 국제 정세 주요 뉴스를 찾아 5개로 짧게 정리해 줘"}, viaChat("u1"))
	if !r.OK || r.Output["status"] != "scheduled" || r.Output["kind"] != "task" || r.Output["card"] != nil {
		t.Fatalf("set: %+v", r)
	}
	if r.Output["repeat"] == "none" || r.Output["first_run"] == "" || r.Output["where"] == "" || !strings.Contains(r.Output["task"].(string), "국제 정세") {
		t.Fatalf("what the answer confirms: %+v", r.Output)
	}
	// a plain reminder from the same person stays as it was: proposed, with its card
	if p := as(svc, "reminders.propose", map[string]any{"when": "내일 오후 3시", "what": "약 먹기"}, viaChat("u1")); p.Output["status"] != "proposed" || p.Output["card"] == nil {
		t.Fatalf("reminder: %+v", p)
	}
	// two of them: words that fit both change nothing and list them
	if amb := as(svc, "reminders.update", map[string]any{"op": "cancel"}, viaChat("u1")); amb.Output["status"] != "ambiguous" || amb.Output["changed"] != false {
		t.Fatalf("no words, two reminders: %+v", amb.Output)
	}
	c := as(svc, "reminders.update", map[string]any{"op": "cancel", "match": "뉴스"}, viaChat("u1"))
	if !c.OK || c.Output["status"] != "cancelled" || c.Output["name"] != "국제 정세 뉴스" {
		t.Fatalf("cancel by words: %+v", c)
	}
	// someone else's words never reach it
	if other := as(svc, "reminders.update", map[string]any{"op": "cancel", "match": "약"}, viaChat("u2")); other.Output["status"] != "not_found" {
		t.Fatalf("another person: %+v", other.Output)
	}
}

// When due, a task runs as a bot turn over the read-only tools and its answer is posted in the channel
// under a one-line header; the series goes on to the next morning.
func TestTaskRunsWhenDueAndPostsTheAnswer(t *testing.T) {
	seoul, _ := time.LoadLocation("Asia/Seoul")
	now := time.Date(2026, 10, 15, 20, 0, 0, 0, seoul)
	svc, f := chatSvc(t, `{"memory_requirement":"none","question_kind":"other","tool_plan":[]}`)
	serveAnyServer(t, svc)
	applyDoc(t, svc, func(d *config.Document) { d.Discord.Enabled = true; d.I18n.Timezone = "Asia/Seoul" })
	gw := &postGateway{}
	svc.Discord = gw
	svc.Clock = clock.Fixed{T: now}
	r := as(svc, "reminders.propose", map[string]any{"kind": "task", "when": "오전 9시", "repeat": "daily", "what": "국제 정세 뉴스",
		"task": "오늘 국제 정세 주요 뉴스를 찾아 정리해 줘"}, viaChat("u1"))
	if r.Output["status"] != "scheduled" {
		t.Fatalf("set: %+v", r)
	}
	svc.Clock = clock.Fixed{T: time.Date(2026, 10, 16, 9, 0, 20, 0, seoul)}
	application.DeliverReminders(svc, context.Background())
	if gw.postCount() != 1 || !strings.Contains(gw.posts[0], "c: -# ") || !strings.Contains(gw.posts[0], "캠핑") {
		t.Fatalf("posted: %+v", gw.posts)
	}
	answer := f.prompt("fake/response")
	if !strings.Contains(answer, "오늘 국제 정세 주요 뉴스를 찾아 정리해 줘") || !strings.Contains(answer, "예약해 둔 작업") {
		t.Fatalf("the run's answer prompt: %s", answer)
	}
	list := as(svc, "reminders.update", map[string]any{"op": "list"}, viaChat("u1")).Output["reminders"].([]map[string]any)
	if len(list) != 1 || list[0]["status"] != "scheduled" || !strings.Contains(list[0]["next"].(string), "17") {
		t.Fatalf("the series goes on: %+v", list)
	}
}
