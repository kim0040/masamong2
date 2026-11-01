package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/clock"
)

// A day of personal features the way people use them in Discord: a "!" command goes through the
// prefix table and requestFor to the Action, its card is rendered within Discord's limits, and a
// button press comes back through featureButton, as the gateway does (without a live session).

func scenarioService(t *testing.T) *application.Service {
	t.Helper()
	svc, err := application.Open(t.TempDir(), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	if r := svc.Execute(context.Background(), action.Request{Action: "setup.resume", Input: json.RawMessage(`{"instance":"scenario"}`), Locale: "ko"}); !r.OK {
		t.Fatalf("setup: %+v", r.Error)
	}
	// a server the bot was already in (approval has its own scenario below)
	if r := svc.Execute(context.Background(), action.Request{Action: "access.policy", Input: json.RawMessage(`{"server_approval":false}`)}); !r.OK {
		t.Fatalf("approval: %+v", r.Error)
	}
	seoul, _ := time.LoadLocation("Asia/Seoul")
	svc.Clock = clock.Fixed{T: time.Date(2026, 9, 28, 10, 0, 0, 0, seoul)}
	return svc
}

func person(id, name string) InvokeContext {
	return InvokeContext{GuildID: "g", ChannelID: "c", UserID: id, Locale: "ko", UserName: name}
}

// runPrefix is a "!" message: parsed, executed and rendered.
func runPrefix(t *testing.T, svc *application.Service, ictx InvokeContext, text string) (action.Result, Rendered) {
	t.Helper()
	cmd, opts, ok := PrefixTable{}.Parse("!", text)
	if !ok {
		t.Fatalf("%q is not a command", text)
	}
	res := svc.Execute(context.Background(), requestFor(cmd, opts, ictx, "dc:test"))
	r := Render(res, cmd, ictx.Locale)
	checkRendered(t, text, r)
	return res, r
}

// press is a button press on a rendered card.
func press(t *testing.T, svc *application.Service, ictx InvokeContext, customID string) (action.Result, Rendered) {
	t.Helper()
	req, ok := featureButton(strings.Split(customID, ":"), ictx, "dc:press")
	if !ok {
		t.Fatalf("button %q is not handled", customID)
	}
	res := svc.Execute(context.Background(), req)
	r := Render(res, Command{Action: req.Action}, ictx.Locale)
	checkRendered(t, customID, r)
	return res, r
}

func buttons(r Rendered) []*discordgo.Button {
	var out []*discordgo.Button
	for _, c := range r.Components {
		if row, ok := c.(discordgo.ActionsRow); ok {
			for _, b := range row.Components {
				if btn, ok := b.(discordgo.Button); ok {
					out = append(out, &btn)
				}
			}
		}
		if row, ok := c.(*discordgo.ActionsRow); ok {
			for _, b := range row.Components {
				if btn, ok := b.(*discordgo.Button); ok {
					out = append(out, btn)
				}
			}
		}
	}
	return out
}

func buttonWith(t *testing.T, r Rendered, prefix string) string {
	t.Helper()
	for _, b := range buttons(r) {
		if strings.HasPrefix(b.CustomID, prefix) {
			return b.CustomID
		}
	}
	t.Fatalf("no %s button in %+v", prefix, buttons(r))
	return ""
}

// checkRendered holds a card to Discord's message limits.
func checkRendered(t *testing.T, where string, r Rendered) {
	t.Helper()
	if r.Embed == nil {
		t.Fatalf("%s: no embed", where)
	}
	total := 0
	for _, e := range append([]*discordgo.MessageEmbed{r.Embed}, r.More...) {
		if utf8.RuneCountInString(e.Title) > 256 || utf8.RuneCountInString(e.Description) > 4096 || len(e.Fields) > 25 {
			t.Fatalf("%s: embed over the limits: %+v", where, e)
		}
		total += utf8.RuneCountInString(e.Title) + utf8.RuneCountInString(e.Description)
		for _, f := range e.Fields {
			total += utf8.RuneCountInString(f.Name) + utf8.RuneCountInString(f.Value)
		}
	}
	if total > 6000 {
		t.Fatalf("%s: %d characters in embeds (max 6000)", where, total)
	}
	if len(r.Components) > 5 {
		t.Fatalf("%s: %d rows (max 5)", where, len(r.Components))
	}
	for _, b := range buttons(r) {
		if len(b.CustomID) > 100 || utf8.RuneCountInString(b.Label) > 80 {
			t.Fatalf("%s: button over the limits: %+v", where, b)
		}
	}
}

func embedText(r Rendered) string {
	var b strings.Builder
	for _, e := range append([]*discordgo.MessageEmbed{r.Embed}, r.More...) {
		b.WriteString(e.Title + "\n" + e.Description + "\n")
		for _, f := range e.Fields {
			b.WriteString(f.Name + ": " + f.Value + "\n")
		}
	}
	return b.String()
}

func TestScenarioReminderThroughDiscord(t *testing.T) {
	svc := scenarioService(t)
	cheolsu, younghee := person("100001", "철수"), person("200002", "영희")

	// "!알림 내일 오후 3시 회의": a proposal with where-to buttons, only for the one who asked
	res, r := runPrefix(t, svc, cheolsu, "!알림 내일 오후 3시 회의")
	if !res.OK || !strings.Contains(embedText(r), "회의") || !strings.Contains(embedText(r), "15:00") {
		t.Fatalf("proposal: %s", embedText(r))
	}
	dm := buttonWith(t, r, "remind:dm:")
	cancel := buttonWith(t, r, "remind:cancel:")
	// someone else pressing is refused
	if res, _ := press(t, svc, younghee, dm); res.OK {
		t.Fatal("another person confirmed the reminder")
	}
	if res, r := press(t, svc, cheolsu, dm); !res.OK || res.Output["status"] != "scheduled" || !strings.Contains(embedText(r), "예약") {
		t.Fatalf("confirm: %+v %s", res.Output, embedText(r))
	}
	// "/reminders": the list with its cancel button
	var list Command
	for _, c := range Commands() {
		if c.Name == "reminders" {
			list = c
		}
	}
	lres := svc.Execute(context.Background(), requestFor(list, nil, cheolsu, "dc:list"))
	lr := Render(lres, list, "ko")
	checkRendered(t, "/reminders", lr)
	if !strings.Contains(embedText(lr), "회의") {
		t.Fatalf("list: %s", embedText(lr))
	}
	if res, _ := press(t, svc, cheolsu, cancel); res.Output["status"] != "cancelled" {
		t.Fatalf("cancel: %+v", res.Output)
	}

	// "3시" alone: pick one of two times first
	_, r = runPrefix(t, svc, younghee, "!알림 3시 반에 전화")
	if picks := 0; true {
		for _, b := range buttons(r) {
			if strings.HasPrefix(b.CustomID, "remind:pick:") {
				picks++
			}
		}
		if picks != 2 {
			t.Fatalf("two times to pick: %+v", buttons(r))
		}
	}
	_, r = press(t, svc, younghee, buttonWith(t, r, "remind:pick:"))
	if res, _ := press(t, svc, younghee, buttonWith(t, r, "remind:here:")); res.Output["status"] != "scheduled" {
		t.Fatalf("channel reminder: %+v", res.Output)
	}

	// words it cannot read get the help, not a guess
	if res, r := runPrefix(t, svc, cheolsu, "!알림 회의 잊지마"); res.Output["status"] != "no_time" || !strings.Contains(embedText(r), "!알림") {
		t.Fatalf("no time: %+v %s", res.Output, embedText(r))
	}
}

func TestScenarioConsentAndSettingsThroughDiscord(t *testing.T) {
	svc := scenarioService(t)
	minsu := person("300003", "민수")
	// withdrawing participation in the person's own words
	res, r := runPrefix(t, svc, minsu, "!개인정보 철회 끼어들기")
	if !res.OK || !strings.Contains(embedText(r), "먼저 말 걸기") {
		t.Fatalf("withdraw: %+v %s", res.Output, embedText(r))
	}
	if svc.ConsentAllows("300003", "participation") {
		t.Fatal("participation must be withdrawn")
	}
	// the status card lists every purpose
	_, r = runPrefix(t, svc, minsu, "!개인정보")
	for _, want := range []string{"대화 기억", "운세", "먼저 말 걸기"} {
		if !strings.Contains(embedText(r), want) {
			t.Fatalf("status lacks %s: %s", want, embedText(r))
		}
	}
	// asking to agree to fortune shows the notice and the agree button, pressing agrees
	_, r = runPrefix(t, svc, minsu, "!개인정보 동의 운세")
	if res, _ := press(t, svc, minsu, buttonWith(t, r, "privacy:grant:")); !res.OK || !svc.ConsentAllows("300003", "fortune") {
		t.Fatalf("agree: %+v", res)
	}
	// personal settings
	if res, _ := runPrefix(t, svc, minsu, "!내설정 시간대 Asia/Tokyo"); !res.OK {
		t.Fatalf("settings: %+v", res)
	}
}

// Proactive participation is "먼저 말 걸기" everywhere a Korean member or manager reads it, and each
// older name keeps working as a word for it ("능동참여" was taught by the unknown-scope error).
func TestParticipationHasOneKoreanNameAndKeepsItsAliases(t *testing.T) {
	svc := scenarioService(t)
	name, _ := svc.I18n.Lookup("ko", "privacy.scope.participation")
	if name != "먼저 말 걸기" {
		t.Fatalf("consent scope name %q", name)
	}
	for _, key := range []string{"privacy.scope_unknown", "privacy.participation.notice", "privacy.card.participation_default",
		"access.chime", "access.bad_op", "access.bad_mode", "discord.command.admin.choice.mode.shadow"} {
		got, ok := svc.I18n.Lookup("ko", key)
		if !ok || !strings.Contains(got, name) {
			t.Errorf("%s does not use %q: %q", key, name, got)
		}
		if strings.Contains(got, "능동참여") || strings.Contains(got, "끼어들") {
			t.Errorf("%s still uses an older name: %q", key, got)
		}
	}
	// the privacy notice's own withdraw hint must be a command that works
	notice, _ := svc.I18n.Lookup("ko", "privacy.participation.notice")
	hint := regexp.MustCompile("`(!개인정보 철회 [^`]+)`").FindStringSubmatch(notice)
	if hint == nil {
		t.Fatalf("the notice must show the withdraw command: %q", notice)
	}
	for i, word := range []string{"먼저말걸기", "능동참여", "끼어들기", "참여"} {
		minsu := person(fmt.Sprint(300010+i), "민수")
		// the Korean unknown-scope text names this scope, and each word withdraws it
		if res, r := runPrefix(t, svc, minsu, "!개인정보 철회 "+word); !res.OK || svc.ConsentAllows(minsu.UserID, "participation") || !strings.Contains(embedText(r), name) {
			t.Fatalf("withdraw with %q: %+v %s", word, res.Error, embedText(r))
		}
	}
	minsu := person("300020", "민수")
	if res, _ := runPrefix(t, svc, minsu, hint[1]); !res.OK || svc.ConsentAllows(minsu.UserID, "participation") {
		t.Fatalf("the notice's hint %q does not withdraw: %+v", hint[1], res.Error)
	}
	// a manager can also say it as the bot's help teaches
	manager := inServer("100001", "철수")
	manager.Manager = true
	for _, word := range []string{"먼저말걸기", "능동참여", "끼어들기"} {
		if res, _ := runPrefix(t, svc, manager, "!관리 "+word+" 켜기"); !res.OK || res.Output["participation"] != "conservative" {
			t.Fatalf("manager word %q: %+v %v", word, res.Error, res.Output)
		}
	}
}

// inServer is a person in a server with a real (numeric) ID.
func inServer(id, name string) InvokeContext {
	p := person(id, name)
	p.GuildID = "500000000000000005"
	return p
}

func TestScenarioServerManagerThroughDiscord(t *testing.T) {
	svc := scenarioService(t)
	manager := inServer("100001", "철수")
	manager.Manager = true
	const g = "500000000000000005"
	// "!관리 차단 @영희": blocked in this server; the card names who
	res, r := runPrefix(t, svc, manager, "!관리 차단 <@200002>")
	if !res.OK || !strings.Contains(embedText(r), "<@200002>") || !svc.BlockedIn(g, "200002") {
		t.Fatalf("block: %+v %s", res.Error, embedText(r))
	}
	if res, _ := runPrefix(t, svc, inServer("200002", "영희"), "!알림 내일 3시 회의"); res.OK {
		t.Fatal("a person blocked here still used a command")
	}
	if res, _ := runPrefix(t, svc, manager, "!관리 해제 200002"); !res.OK || svc.BlockedIn(g, "200002") {
		t.Fatalf("unblock: %+v", res.Error)
	}
	// "!관리 끼어들기 켜기" means it may speak; "!관리 기억 끄기" stops saving messages here
	if res, _ := runPrefix(t, svc, manager, "!관리 끼어들기 켜기"); !res.OK || res.Output["participation"] != "conservative" {
		t.Fatalf("chime: %+v %v", res.Error, res.Output)
	}
	if res, _ := runPrefix(t, svc, manager, "!관리 기억 끄기"); !res.OK || res.Output["capture"] != false {
		t.Fatalf("memory: %+v %v", res.Error, res.Output)
	}
	// a member gets the refusal, not a change
	if res, _ := runPrefix(t, svc, inServer("300003", "민수"), "!관리 정지"); res.OK || svc.Paused(g, "c") {
		t.Fatal("a member paused the channel")
	}
}

func TestScenarioServerWaitingForApproval(t *testing.T) {
	svc := scenarioService(t)
	if r := svc.Execute(context.Background(), action.Request{Action: "access.policy", Input: json.RawMessage(`{"server_approval":true}`)}); !r.OK {
		t.Fatalf("approval: %+v", r.Error)
	}
	// "g" has no rooms set up and nobody approved it: a command gets the notice card
	res, r := runPrefix(t, svc, person("100001", "철수"), "!알림 내일 3시 회의")
	if res.OK || !strings.Contains(embedText(r), "승인") {
		t.Fatalf("pending server: %+v %s", res.Output, embedText(r))
	}
	// withdrawing consent still works there
	if res, _ := runPrefix(t, svc, person("100001", "철수"), "!개인정보 철회 기억"); !res.OK {
		t.Fatalf("consent in a pending server: %+v", res.Error)
	}
}

func TestAdminSlashCommandIsForManagers(t *testing.T) {
	for _, ac := range ApplicationCommands() {
		if ac.Name != "admin" {
			if ac.DefaultMemberPermissions != nil {
				t.Fatalf("/%s hidden from members", ac.Name)
			}
			continue
		}
		if ac.DefaultMemberPermissions == nil || *ac.DefaultMemberPermissions != discordgo.PermissionManageGuild {
			t.Fatalf("/admin visible to everyone: %v", ac.DefaultMemberPermissions)
		}
		names := map[string]bool{}
		for _, o := range ac.Options {
			names[o.Name] = true
			if len(o.Description) > 100 {
				t.Fatalf("option %s description over 100", o.Name)
			}
		}
		if !names["user"] || len(ac.Description) > 100 {
			t.Fatalf("/admin options %v, description %d", names, len(ac.Description))
		}
		return
	}
	t.Fatal("no /admin")
}
