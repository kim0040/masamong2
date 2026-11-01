package application_test

import (
	"encoding/json"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/clock"
)

// /랭킹 counts each person's messages per channel (never the text): the bot's own messages and people
// who withdrew their memory consent are not counted, other channels do not mix in, and the periods are
// local calendar days, this week and this month.
func TestActivityRankingCountsPeoplePerChannel(t *testing.T) {
	svc := setupSvc(t)
	for _, c := range []string{"c1", "c2"} {
		if r := exec(svc, "channels.configure", map[string]any{"guild_id": "g1", "id": c, "respond": "mention"}); !r.OK {
			t.Fatal(r.Error)
		}
	}
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC) // a Thursday
	svc.Clock = clock.Fixed{T: now}
	say := func(user, name, channel string, at time.Time, bot bool) {
		svc.Observe(application.ObservedMessage{GuildID: "g1", ChannelID: channel, MessageID: user + at.String(), AuthorID: user, AuthorName: name, Content: "본문은 저장되지 않는다", At: at, FromBot: bot})
	}
	for i := 0; i < 5; i++ {
		say("1001", "영희", "c1", now.Add(-time.Duration(i)*time.Minute), false)
	}
	for i := 0; i < 3; i++ {
		say("1002", "철수", "c1", now.Add(-time.Duration(i)*time.Hour), false)
	}
	say("1002", "철수", "c1", now.AddDate(0, 0, -20), false) // last month: only in "all"
	say("1003", "민수", "c2", now, false)                    // another channel
	say("bot", "마사몽", "c1", now, true)                     // the bot itself
	_ = asUser(svc, "1004", "privacy.consent", map[string]any{"scope": "memory", "decision": "withdraw"})
	say("1004", "숨김", "c1", now, false)
	rank := func(period string) map[string]any {
		raw, _ := json.Marshal(map[string]any{"period": period, "scope": "discord:c1"})
		r := svc.Execute(t.Context(), action.Request{Action: "activity.ranking", Input: raw, Invoker: &action.Invoker{Via: "command", GuildID: "g1", ChannelID: "c1", UserID: "1001"}})
		if !r.OK {
			t.Fatalf("%s: %+v", period, r.Error)
		}
		m, _ := jsonRoundTrip(r.Output)
		return m
	}
	week := rank("week")
	top := week["top"].([]any)
	first := top[0].(map[string]any)
	if len(top) != 2 || first["name"] != "영희" || first["count"].(float64) != 5 || week["total_messages"].(float64) != 8 || first["share"] != "62.5" {
		t.Fatalf("this week in c1: %v", week)
	}
	if all := rank("all"); all["total_messages"].(float64) != 9 {
		t.Fatalf("everything kept: %v", all)
	}
	if r := svc.Execute(t.Context(), action.Request{Action: "activity.ranking", Input: json.RawMessage(`{"period":"year","scope":"discord:c1"}`)}); r.OK {
		t.Fatal("unknown period accepted")
	}
}
