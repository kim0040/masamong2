package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// terminal output: CI and NO_COLOR force JSON, so the tests clear them (as CI sets CI=true)
func onTerminal(t *testing.T) {
	t.Setenv("CI", "")
	t.Setenv("NO_COLOR", "")
}

func TestUsageOnATerminalAndJSONForScripts(t *testing.T) {
	onTerminal(t)
	dir := t.TempDir()
	for _, args := range [][]string{{}, {"help"}, {"--help"}} {
		var out bytes.Buffer
		if code := run(context.Background(), append([]string{"--data-root", dir}, args...), &out, ioDiscard(), false, true); code != 0 {
			t.Fatalf("%v: exit %d", args, code)
		}
		if !strings.Contains(out.String(), "masamong open") || !strings.Contains(out.String(), "masamong stop") {
			t.Fatalf("%v: no usage:\n%s", args, out.String())
		}
	}
	var js bytes.Buffer
	run(context.Background(), []string{"--data-root", dir, "--output", "json"}, &js, ioDiscard(), false, true)
	if !strings.Contains(js.String(), `"action":"help.version"`) {
		t.Fatalf("scripts keep help.version: %s", js.String())
	}
}

func TestUnknownCommandSuggestsTheNearestOne(t *testing.T) {
	for typed, want := range map[string]string{"staus": "status", "opne": "open", "chanels list": "channels list", "backup craete": "backup create"} {
		if got := suggest(typed); got != want {
			t.Errorf("suggest(%q) = %q, want %q", typed, got, want)
		}
	}
	onTerminal(t)
	if got := suggest("completely different words"); got != "" {
		t.Errorf("far-off input suggested %q", got)
	}
	var errb bytes.Buffer
	if code := run(context.Background(), []string{"--data-root", t.TempDir(), "staus"}, ioDiscard(), &errb, false, true); code != 2 || !strings.Contains(errb.String(), "masamong status") {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
}

// Every group the usage text lists works on its own or with a verb from the catalog.
func TestUsageGroupsAreReal(t *testing.T) {
	routes := catalogRoutes()
	text := usageText()
	block := text[strings.Index(text, "groups:")+len("groups:"):]
	block = block[:strings.Index(block, "\n\n")]
	for _, g := range strings.Split(block, ",") {
		g = strings.TrimSpace(g)
		ok := false
		for k := range routes {
			ok = ok || k == g || strings.HasPrefix(k, g+" ")
		}
		if !ok {
			t.Errorf("group %q has no command", g)
		}
	}
}
