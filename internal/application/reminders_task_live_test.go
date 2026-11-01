package application_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"masamong/internal/application"
	"masamong/internal/clock"
	"masamong/internal/config"
)

// TestLiveScheduledTask runs the whole scheduled-task conversation against real models and real search
// services: a request missing its topic is asked about, the answer sets the task, the task runs at its
// time and posts a news roundup, and saying so cancels it. It needs MASAMONG_LIVE_TASK=1 and keys in
// MASAMONG_PROBE_OPENROUTER and at least one of MASAMONG_PROBE_{LINKUP,EXA,KAKAO,TAVILY,SERPER,BRAVE,TINYFISH}.
// It prints each step for a person to read; it fails only when a step does not happen at all.
func TestLiveScheduledTask(t *testing.T) {
	if os.Getenv("MASAMONG_LIVE_TASK") == "" || os.Getenv("MASAMONG_PROBE_OPENROUTER") == "" {
		t.Skip("set MASAMONG_LIVE_TASK=1 and the MASAMONG_PROBE_* keys")
	}
	t.Setenv("MASAMONG_OFFLINE", "") // the package's tests run offline; this one is the exception
	seoul, _ := time.LoadLocation("Asia/Seoul")
	now := time.Now().In(seoul)
	svc := setupSvc(t)
	serveAnyServer(t, svc)
	applyDoc(t, svc, func(d *config.Document) {
		d.Discord.Enabled = true
		d.I18n.Timezone = "Asia/Seoul"
		d.Memory.Retrieve = "off"
		d.Providers["openrouter"] = config.Provider{Kind: config.KindOpenAI, BaseURL: "https://openrouter.ai/api/v1", CredentialRef: "env:MASAMONG_PROBE_OPENROUTER"}
		for id, kind := range map[string]string{"linkup": config.KindLinkup, "exa": config.KindExa, "kakao": config.KindKakaoSearch, "tavily": config.KindTavily,
			"serper": config.KindSerper, "brave": config.KindBrave, "tinyfish": config.KindTinyFish} {
			env := "MASAMONG_PROBE_" + strings.ToUpper(id)
			d.Providers[id] = config.Provider{Kind: kind, CredentialRef: "env:" + env, Disabled: os.Getenv(env) == ""}
		}
		// the models to try, when not the defaults: MASAMONG_LIVE_PLANNER and MASAMONG_LIVE_RESPONSE
		for role, env := range map[string]string{"planner": "MASAMONG_LIVE_PLANNER", "response": "MASAMONG_LIVE_RESPONSE"} {
			if m := os.Getenv(env); m != "" {
				d.Models[role] = config.Model{Provider: "openrouter", Model: m}
			}
		}
	})
	if r := exec(svc, "channels.configure", map[string]any{"guild_id": "g", "id": "c", "persona_ref": "friendly_ko"}); !r.OK {
		t.Fatalf("%+v", r.Error)
	}
	gw := &postGateway{}
	svc.Discord = gw
	svc.Clock = clock.Fixed{T: now}
	// each turn comes 30 seconds after the last; a later one is the untagged follow-up the window answers
	at := now
	turn := func(n int, msg string) map[string]string {
		t.Helper()
		at = at.Add(30 * time.Second)
		svc.Clock = clock.Fixed{T: at}
		res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: fmt.Sprintf("m%d", n),
			Message: msg, Speaker: "철수", SpeakerID: "u1", FollowUp: n == 2})
		if err != nil {
			t.Fatalf("turn %d: %v", n, err)
		}
		statuses := map[string]string{}
		fmt.Printf("\n[%d] 철수: %s\n", n, msg)
		for _, tr := range res.Tools {
			st, _ := tr.Data["status"].(string)
			statuses[tr.Tool] = st
			fmt.Printf("    tool %s → %s %v ok=%v status=%q err=%q\n", tr.Tool, st, brief(tr.Data), tr.OK, tr.Status, tr.Error)
		}
		fmt.Printf("    마사몽: %s\n", res.Answer)
		return statuses
	}
	first := turn(1, "마사몽 매일 오전 9시에 뉴스 정리해서 알려줘")
	if first["reminders.propose"] == "scheduled" {
		t.Errorf("a roundup of news about nothing in particular was set without asking")
	}
	second := turn(2, "음 국제 정세로 해줘")
	if second["reminders.propose"] != "scheduled" {
		t.Fatalf("the answer did not set the task: %v", second)
	}
	list := as(svc, "reminders.update", map[string]any{"op": "list"}, viaChat("u1")).Output["reminders"].([]map[string]any)
	if len(list) != 1 {
		t.Fatalf("tasks: %+v", list)
	}
	fmt.Printf("\n    saved: %v\n", list[0])
	next := time.Date(now.Year(), now.Month(), now.Day(), 9, 0, 20, 0, seoul)
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	svc.Clock = clock.Fixed{T: next}
	began := time.Now()
	application.DeliverReminders(svc, context.Background())
	if gw.postCount() != 1 {
		t.Fatalf("nothing posted at %s", next)
	}
	fmt.Printf("\n[%s run, %.1fs] posted:\n%s\n", next.Format("01-02 15:04"), time.Since(began).Seconds(), gw.posts[0])
	at = next.Add(time.Hour)
	third := turn(3, "아침 뉴스 예약 이제 취소해줘")
	if third["reminders.update"] != "cancelled" {
		t.Errorf("not cancelled by words: %v", third)
	}
}

// brief is a tool result without long texts (what the step decided, not the pages).
func brief(data map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range data {
		switch k {
		case "results", "card", "reminder":
			if list, ok := v.([]any); ok {
				out[k] = fmt.Sprintf("%d items", len(list))
			}
			continue
		}
		if s, ok := v.(string); ok && len([]rune(s)) > 120 {
			v = string([]rune(s)[:120]) + "…"
		}
		out[k] = v
	}
	return out
}
