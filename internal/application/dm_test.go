package application_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/clock"
	"masamong/internal/config"
	"masamong/internal/memory/engine"
	"masamong/internal/tools"
)

type dmGuildMembership struct {
	application.DiscordInfo
	allowed bool
}

func (g dmGuildMembership) SharedGuildMember(context.Context, string) bool { return g.allowed }

func dmService(t *testing.T) *application.Service {
	t.Helper()
	var plan, response atomic.Int32
	svc := routedSvc(t, chatServer(t, `{"memory_requirement":"none","tool_plan":[]}`, "stop", &plan), chatServer(t, "Synthetic answer", "stop", &response))
	applyDoc(t, svc, func(d *config.Document) { d.Access.Operators = []string{"11111", "22222"}; d.I18n.DefaultLocale = "en" })
	return svc
}

func dmInput(user string) application.ChatInput {
	return application.ChatInput{ChannelID: "dm-" + user, MessageID: "dm-message", Message: "Synthetic request", SpeakerID: user, Speaker: "Synthetic speaker"}
}

func dmCount(t *testing.T, svc *application.Service, tool string) int {
	t.Helper()
	var n int
	if err := svc.Store.DB.QueryRow(`SELECT COALESCE(SUM(n),0) FROM tool_usage WHERE tool=? AND scope='all'`, tool).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestDMPoliciesRefuseBeforeTurnAndLeaveCommandsAvailable(t *testing.T) {
	svc := dmService(t)
	for _, tc := range []struct {
		mode                            string
		member, operator, blocked, want bool
	}{
		{"members", true, false, false, true}, {"members", false, false, false, false}, {"members", false, true, false, true},
		{"operators", true, false, false, false}, {"operators", false, true, false, true}, {"off", true, true, false, false}, {"members", true, true, true, false},
	} {
		applyDoc(t, svc, func(d *config.Document) {
			d.DM.Allow = tc.mode
			d.Access.BlockedUsers = nil
			if tc.blocked {
				d.Access.BlockedUsers = []string{"11111"}
			}
		})
		svc.Discord = dmGuildMembership{allowed: tc.member}
		user := "33333"
		if tc.operator {
			user = "11111"
		}
		if got := svc.DMChatAllowed(context.Background(), user); got != tc.want {
			t.Fatal("incorrect DM admission policy")
		}
		if !tc.want {
			var before, after int
			_ = svc.Store.DB.QueryRow(`SELECT COUNT(*) FROM turn_log`).Scan(&before)
			_, err := svc.ChatTurn(context.Background(), dmInput(user))
			if !errors.Is(err, application.ErrDMClosedChat) {
				t.Fatal("disallowed conversation reached turn")
			}
			_ = svc.Store.DB.QueryRow(`SELECT COUNT(*) FROM turn_log`).Scan(&after)
			if before != after || dmCount(t, svc, "dm.chat") != 0 {
				t.Fatal("refusal created a turn or counter")
			}
		}
		if tc.mode == "off" {
			res := as(svc, "help.version", nil, &action.Invoker{Via: "command", ChannelID: "dm-command", UserID: user})
			if !res.OK {
				t.Fatal("DM chat off blocked a command")
			}
		}
	}
}

func TestDMQuotasAreAtomicPersistAndResetAtInstanceMidnight(t *testing.T) {
	for _, tc := range []struct {
		name                             string
		userLimit, totalLimit, chatLimit int
		secondUser, scope                string
	}{
		{"user", 1, 10, 0, "11111", "per_user_per_day"}, {"all", 10, 1, 0, "22222", "per_day"}, {"existing-chat", 10, 10, 1, "22222", "chat"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := dmService(t)
			applyDoc(t, svc, func(d *config.Document) {
				d.DM.PerUserPerDay = tc.userLimit
				d.DM.PerDay = tc.totalLimit
				d.I18n.Timezone = "Asia/Seoul"
				d.Tools["chat.respond"] = tools.Settings{Limits: tools.Limits{PerDay: tc.chatLimit}}
			})
			at := time.Date(2026, 10, 13, 14, 59, 0, 0, time.UTC)
			svc.Clock = clock.Fixed{T: at}
			if _, err := svc.ChatTurn(context.Background(), dmInput("11111")); err != nil {
				t.Fatal(err)
			}
			listed := exec(svc, "tools.list", nil)
			info, ok := listed.Output["dm"].(map[string]any)
			if !listed.OK || !ok || info["used_today"] != 1 || info["per_user_per_day"] != tc.userLimit || info["per_day"] != tc.totalLimit {
				t.Fatal("DM daily usage read action")
			}
			if !svc.ClaimDMNotice("11111", "closed") {
				t.Fatal("first notice not admitted")
			}
			root := svc.DataRoot
			if err := svc.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := application.Open(root, clock.Fixed{T: at}, false)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if reopened.ClaimDMNotice("11111", "closed") {
				t.Fatal("restart reset daily notice")
			}
			_, err = reopened.ChatTurn(context.Background(), dmInput(tc.secondUser))
			var limit application.DMLimited
			if tc.scope == "chat" {
				if !errors.Is(err, application.ErrChatLimited) {
					t.Fatal("existing chat quota bypassed")
				}
			} else if !errors.As(err, &limit) || limit.Scope != tc.scope {
				t.Fatal("DM quota lost at restart")
			}
			if dmCount(t, reopened, "dm.chat") != 1 || dmCount(t, reopened, "chat.respond") != 1 {
				t.Fatal("refused turn consumed a quota")
			}
			if !reopened.ClaimDMNotice(tc.secondUser, tc.scope) || reopened.ClaimDMNotice(tc.secondUser, tc.scope) {
				t.Fatal("notice was not daily")
			}
			reopened.Clock = clock.Fixed{T: at.Add(time.Minute)}
			if _, err := reopened.ChatTurn(context.Background(), dmInput(tc.secondUser)); err != nil {
				t.Fatal("instance midnight did not reset quota")
			}
			if !reopened.ClaimDMNotice(tc.secondUser, tc.scope) {
				t.Fatal("instance midnight did not reset notice")
			}
		})
	}
}

func TestDMKeepsRecentExchangesWithoutRoomMemoryOrNameGate(t *testing.T) {
	var plan atomic.Int32
	var mu sync.Mutex
	var prompts []string
	response := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		prompts = append(prompts, string(raw))
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "Synthetic remembered answer"}, "finish_reason": "stop"}}})
	}))
	defer response.Close()
	svc := routedSvc(t, chatServer(t, `{"memory_requirement":"required","tool_plan":[{"tool_name":"memory.search","parameters":{"query":"Synthetic"}}]}`, "stop", &plan), response.URL)
	applyDoc(t, svc, func(d *config.Document) {
		d.Access.Operators = []string{"11111"}
		d.I18n.DefaultLocale = "en"
		d.Memory.Retrieve = "auto"
		d.Memory.Capture = true
	})
	eng, err := svc.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Capture(engine.LiveMessage{GuildID: "guild", ChannelID: "room", MessageID: "server-message", AuthorID: "33333", Content: "SERVER_ONLY_SENTINEL", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	in := dmInput("11111")
	in.Message = "DM_HISTORY_SENTINEL"
	in.NameOnly = true
	in.FollowUp = true
	in.Proactive = true
	if _, err := svc.ChatTurn(context.Background(), in); err != nil {
		t.Fatal("DM was classified as a name call")
	}
	in.Message = "Second synthetic request"
	in.MessageID = "dm-message-2"
	if _, err := svc.ChatTurn(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	captured := append([]string(nil), prompts...)
	mu.Unlock()
	if len(captured) != 2 || !strings.Contains(captured[1], "DM_HISTORY_SENTINEL") || !strings.Contains(captured[1], "Synthetic remembered answer") {
		t.Fatal("DM short-term conversation missing")
	}
	for _, p := range captured {
		if strings.Contains(p, "SERVER_ONLY_SENTINEL") {
			t.Fatal("server memory entered DM")
		}
	}
	turns, err := eng.Store.RecentChatTurns("discord::"+in.ChannelID, 20)
	if err != nil || len(turns) != 4 {
		t.Fatal("DM exchange count")
	}
	other, _ := eng.Store.RecentChatTurns("discord:g:"+in.ChannelID, 20)
	if len(other) != 0 {
		t.Fatal("DM context leaked into server key")
	}
	var rawDM int
	if err := svc.Store.DB.QueryRow(`SELECT COUNT(*) FROM memory_observations WHERE room=?`, engine.RoomKey("", in.ChannelID)).Scan(&rawDM); err != nil || rawDM != 0 {
		t.Fatal("DM was captured into raw memory")
	}
	if svc.FollowWindowOpen("", in.ChannelID, in.SpeakerID) {
		t.Fatal("DM opened follow-up window")
	}
}

func TestDMUnansweredCommandContinuesConversation(t *testing.T) {
	svc := dmService(t)
	res := action.OK(action.Request{Action: "weather.query"}, map[string]any{"status": "unconfigured"})
	if out, ok := svc.CommandToChat(context.Background(), res, dmInput("11111")); !ok || out.Answer == "" {
		t.Fatal("DM command did not continue as conversation")
	}
	applyDoc(t, svc, func(d *config.Document) { d.DM.Allow = "off" })
	if _, ok := svc.CommandToChat(context.Background(), res, dmInput("11111")); ok {
		t.Fatal("disabled DM command continued as conversation")
	}
}

func TestDMAccountDeletionAndWithdrawalReachContext(t *testing.T) {
	for _, withdraw := range []bool{false, true} {
		t.Run(map[bool]string{false: "account-delete", true: "withdraw"}[withdraw], func(t *testing.T) {
			svc := dmService(t)
			eng, err := svc.MemoryEngine()
			if err != nil {
				t.Fatal(err)
			}
			in := dmInput("11111")
			if _, err := svc.ChatTurn(context.Background(), in); err != nil {
				t.Fatal(err)
			}
			if withdraw {
				r := as(svc, "privacy.consent", map[string]any{"scope": "memory", "decision": "withdraw"}, &action.Invoker{Via: "command", ChannelID: in.ChannelID, UserID: in.SpeakerID})
				if !r.OK {
					t.Fatal("DM withdrawal failed")
				}
				svc.DrainRedactions(context.Background())
			} else {
				if _, err := eng.Store.DeleteAccountMessages("discord", in.SpeakerID, time.Now().UTC().Format(time.RFC3339)); err != nil {
					t.Fatal(err)
				}
			}
			turns, err := eng.Store.RecentChatTurns("discord::"+in.ChannelID, 10)
			if err != nil || len(turns) != 0 {
				t.Fatal("DM personal deletion left context")
			}
			if withdraw {
				if svc.MemoryAllowed(in.SpeakerID) {
					t.Fatal("withdrawn DM person is still remembered")
				}
				if _, err := svc.ChatTurn(context.Background(), in); err != nil {
					t.Fatal(err)
				}
				turns, _ = eng.Store.RecentChatTurns("discord::"+in.ChannelID, 10)
				if len(turns) != 0 {
					t.Fatal("DM recreated withdrawn context")
				}
			}
		})
	}
}

func TestDMRetentionDropsOldContext(t *testing.T) {
	svc := dmService(t)
	in := dmInput("11111")
	if _, err := svc.ChatTurn(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Store.DB.Exec(`INSERT INTO chat_turns(channel_key,role,speaker,content,created_at) VALUES('discord::dm-old','user','','synthetic',?)`, time.Now().AddDate(-1, 0, 0).UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	if res := exec(svc, "schedules.run", map[string]any{"id": "usage_gc"}); !res.OK {
		t.Fatal("retention job failed")
	}
	var n int
	if err := svc.Store.DB.QueryRow(`SELECT COUNT(*) FROM chat_turns WHERE channel_key='discord::dm-old'`).Scan(&n); err != nil || n != 0 {
		t.Fatal("retention kept old DM context")
	}
}
