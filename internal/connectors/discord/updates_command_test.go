package discord

import (
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

	"masamong/internal/application"
)

// "/업데이트" and its "!" forms, in the words of the language packs: the notes of the running version,
// the one before it or one named, the person's own DM subscription, and the manager's room switch.
func TestScenarioUpdateNewsCommands(t *testing.T) {
	old := application.Version
	application.Version = "v1.5.1"
	defer func() { application.Version = old }()
	svc := scenarioService(t)
	minsu := inServer("300003", "민수")
	const g = "500000000000000005"

	res, r := runPrefix(t, svc, minsu, "!업데이트")
	if !res.OK || res.Output["version"] != "v1.5.1" || !strings.Contains(embedText(r), "v1.5.1 새 소식") || !strings.Contains(embedText(r), "• 사람별 기억") {
		t.Fatalf("now: %+v %s", res.Output, embedText(r))
	}
	if r.Embed.Footer == nil || !strings.Contains(r.Embed.Footer.Text, "/업데이트 이전") || !strings.Contains(r.Embed.Footer.Text, "v1.4.0") {
		t.Fatalf("the card says how to see the previous version: %+v", r.Embed.Footer)
	}
	if strings.Contains(embedText(r), "운영자용") {
		t.Fatalf("the chat view has no operator part: %s", embedText(r))
	}
	// the previous version, in every spelling the pack teaches
	for _, word := range []string{"이전", "지난"} {
		if res, _ := runPrefix(t, svc, minsu, "!업데이트 "+word); !res.OK || res.Output["version"] != "v1.4.0" {
			t.Fatalf("%s: %+v", word, res.Output)
		}
	}
	if res, _ := runPrefix(t, svc, minsu, "!업데이트 v1.3.0"); !res.OK || res.Output["version"] != "v1.3.0" {
		t.Fatalf("named: %+v", res.Output)
	}
	// v1.2.2 has user-facing notes, so it is the one before v1.3.0
	if res, _ := runPrefix(t, svc, minsu, "!업데이트 이전 v1.3.0"); !res.OK || res.Output["version"] != "v1.2.2" {
		t.Fatalf("the one before a named version: %+v", res.Output)
	}
	if res, _ := runPrefix(t, svc, minsu, "!업데이트 v9.9.9"); res.OK {
		t.Fatal("an unknown version is refused")
	}
	// the English words work too (the aliases merge every language)
	if res, _ := runPrefix(t, svc, minsu, "!updates previous"); !res.OK || res.Output["version"] != "v1.4.0" {
		t.Fatalf("english: %+v", res.Output)
	}

	// the person's own subscription
	if res, _ := runPrefix(t, svc, minsu, "!업데이트 상태"); !res.OK || res.Output["enabled"] != false {
		t.Fatalf("status: %+v", res.Output)
	}
	if res, r := runPrefix(t, svc, minsu, "!업데이트 구독"); !res.OK || res.Output["enabled"] != true || !strings.Contains(embedText(r), "켜짐") {
		t.Fatalf("subscribe: %+v %s", res.Output, embedText(r))
	}
	if res, _ := runPrefix(t, svc, minsu, "!업데이트 해지"); !res.OK || res.Output["enabled"] != false {
		t.Fatalf("unsubscribe: %+v", res.Output)
	}

	// the room switch is where a manager changes the room's other settings; a member cannot
	manager := inServer("100001", "철수")
	manager.Manager = true
	if res, _ := runPrefix(t, svc, minsu, "!관리 소식 켜기"); res.OK {
		t.Fatal("a member turned the room's news on")
	}
	if res, r := runPrefix(t, svc, manager, "!관리 소식 켜기"); !res.OK || svc.ChannelPolicyFor(g, "c").UpdateNews != "on" || !strings.Contains(embedText(r), "업데이트 소식") {
		t.Fatalf("on: %+v %s", res.Error, embedText(r))
	}
	if res, _ := runPrefix(t, svc, manager, "!관리 소식 끄기"); !res.OK || svc.ChannelPolicyFor(g, "c").UpdateNews != "" {
		t.Fatalf("off: %+v", res.Error)
	}
}

// The slash command is registered with its localized names, and every choice has a label.
func TestUpdatesCommandIsRegisteredInEveryLanguage(t *testing.T) {
	svc := scenarioService(t)
	names := map[string]string{"ko": "업데이트", "ja": "アップデート", "zh-CN": "更新", "es-ES": "novedades"}
	for _, c := range applicationCommandsIn(svc.I18n, nil) {
		if c.Name != "updates" {
			continue
		}
		for loc, want := range names {
			if got := (*c.NameLocalizations)[discordgo.Locale(loc)]; got != want {
				t.Errorf("%s name %q, want %q", loc, got, want)
			}
		}
		if len(c.Options) != 2 || len(c.Options[0].Choices) != 5 {
			t.Fatalf("options: %+v", c.Options)
		}
		for _, ch := range c.Options[0].Choices {
			if len(ch.NameLocalizations) < 4 {
				t.Errorf("choice %v lacks labels: %v", ch.Value, ch.NameLocalizations)
			}
		}
		return
	}
	t.Fatal("updates is not registered")
}
