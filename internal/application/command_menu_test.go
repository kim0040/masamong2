package application_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/config"
	"masamong/internal/tools"
)

// "What can you do?" names the commands of the tools the conversation cannot call (a fortune goes
// through its consent command), in the asker's language with the room's prefix; a tool the planner
// calls itself is not repeated there, and a feature that is off is not offered.
func TestHelpNamesCommandOnlyFeatures(t *testing.T) {
	var calls atomic.Int32
	url := chatServer(t, "ok", "stop", &calls)
	svc := setupSvc(t)
	t.Setenv("FAKE_OR_KEY", "k")
	applyDoc(t, svc, func(d *config.Document) {
		d.Providers["host"] = config.Provider{Kind: config.KindOpenAI, BaseURL: url, CredentialRef: "env:FAKE_OR_KEY"}
		d.Models["response"] = config.Model{Provider: "host", Model: "m"}
		d.Models["planner"] = config.Model{Provider: "host", Model: "m"}
		d.Features["fortune"] = true
		d.Features["notices"] = false
	})
	help := func() map[string]any {
		t.Helper()
		r := svc.Execute(context.Background(), action.Request{Action: "help.version", Locale: "ko"})
		if !r.OK {
			t.Fatalf("%+v", r.Error)
		}
		raw, _ := json.Marshal(r.Output)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		return m
	}
	if _, ok := help()["command_only"]; ok {
		t.Fatal("no Discord connector, no command list")
	}
	svc.SetCommandMenu([]application.CommandHint{{Name: "fortune", Action: "fortune.query"}, {Name: "zodiac", Action: "fortune.query"},
		{Name: "image", Action: "images.generate"}, {Name: "notices", Action: "notices.status"}})
	raw, _ := json.Marshal(help()["command_only"])
	got := string(raw)
	if !strings.Contains(got, `"!운세"`) || !strings.Contains(got, `"!별자리"`) || !strings.Contains(got, "구독") {
		t.Fatalf("fortune commands missing: %s", got)
	}
	if strings.Contains(got, "image") || strings.Contains(got, "그림") || strings.Contains(got, "notices") || strings.Contains(got, "공지") {
		t.Fatalf("chat tools and switched-off features are not command-only: %s", got)
	}
	// a command the operator switched off is not offered: it would only answer "not here"
	off := false
	applyDoc(t, svc, func(d *config.Document) {
		ts := d.Tools["fortune.query"]
		ts.Commands = &off
		if d.Tools == nil {
			d.Tools = map[string]tools.Settings{}
		}
		d.Tools["fortune.query"] = ts
	})
	if v, ok := help()["command_only"]; ok {
		t.Fatalf("command surface off, still offered: %v", v)
	}
}
