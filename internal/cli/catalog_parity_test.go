package cli

import (
	"strings"
	"testing"

	"masamong/internal/catalog"
)

// Every catalog Action must be reachable from the CLI by the name the catalog advertises
// (ADR-15: CLI and GUI expose the same Actions).
func TestEveryCatalogCLIRoutesToItsAction(t *testing.T) {
	for _, d := range catalog.All() {
		if d.CLI == "" {
			t.Fatalf("%s has no CLI name", d.ID)
		}
		var pos []string
		for _, w := range strings.Fields(d.CLI) {
			if !strings.HasPrefix(w, "-") {
				pos = append(pos, w)
			}
		}
		if got := mapAction(pos); got != d.ID {
			t.Fatalf("cli %q routes to %s, catalog says %s", d.CLI, got, d.ID)
		}
	}
}

func TestBareGroupDefaults(t *testing.T) {
	cases := map[string]string{
		"": "help.version", "config": "config.show", "jobs": "jobs.list", "channels": "channels.list",
		"polls": "polls.result", "notices": "notices.status", "backup": "backup.create", "finance": "finance.quote",
		"fortune": "fortune.query", "version": "help.version", "admin": "admin.open",
		"autostart": "runtime.autostart", "schedules": "schedules.list", "start": "launcher.start",
		"stop": "launcher.stop", "status": "launcher.status", "tray": "launcher.tray", "restart": "launcher.restart",
	}
	for in, want := range cases {
		var pos []string
		if in != "" {
			pos = strings.Fields(in)
		}
		if got := mapAction(pos); got != want {
			t.Fatalf("%q -> %s want %s", in, got, want)
		}
	}
}

func TestCoerceKeepsSnowflakesAsStrings(t *testing.T) {
	if v, ok := coerce("1003000000000000103").(string); !ok || v != "1003000000000000103" {
		t.Fatalf("snowflake must stay a string, got %#v", coerce("1003000000000000103"))
	}
	if v, ok := coerce("42").(int); !ok || v != 42 {
		t.Fatalf("short ints stay numeric, got %#v", coerce("42"))
	}
	if v, ok := coerce("true").(bool); !ok || !v {
		t.Fatal("bool")
	}
}

func TestAutostartShortForm(t *testing.T) {
	_, act, raw, _ := ParseArgs([]string{"autostart", "on"})
	if act != "runtime.autostart.set" || string(raw) != `{"enabled":true}` {
		t.Fatalf("%s %s", act, raw)
	}
}
