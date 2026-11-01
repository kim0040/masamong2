package discord

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestBangAliasesMatchSandboxParity(t *testing.T) {
	root := findRepoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "contracts", "sandbox", "tool-parity.json"))
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		AgentTools []struct {
			ID   string   `json:"id"`
			Bang []string `json:"bang"`
		} `json:"agent_tools"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{}
	for _, row := range spec.AgentTools {
		want[row.ID] = row.Bang
	}
	sandboxBang := map[string]string{}
	for id, aliases := range want {
		for _, a := range aliases {
			sandboxBang[a] = id
		}
	}
	for _, cmd := range Commands() {
		if cmd.DiscordOnly {
			// not an agent tool the sandbox runs: it only must not take a sandbox word
			if _, listed := want[cmd.Action]; listed {
				t.Fatalf("command %s is DiscordOnly but %s is a sandbox agent tool", cmd.Name, cmd.Action)
			}
			for _, a := range cmd.Aliases {
				if id, taken := sandboxBang[a]; taken {
					t.Fatalf("Discord-only command %s alias %q is the sandbox's !%s for %s", cmd.Name, a, a, id)
				}
			}
			continue
		}
		aliases, ok := want[cmd.Action]
		if !ok {
			t.Fatalf("Discord command %s action %s missing from contracts/sandbox/tool-parity.json", cmd.Name, cmd.Action)
		}
		got := map[string]struct{}{}
		for _, a := range cmd.Aliases {
			got[a] = struct{}{}
		}
		for _, a := range aliases {
			if _, exists := got[a]; !exists {
				t.Fatalf("product command %s missing bang alias %q (keep in sync with tool-parity.json)", cmd.Name, a)
			}
		}
		for a := range got {
			found := false
			for _, b := range aliases {
				if a == b {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("product command %s extra alias %q not in tool-parity.json", cmd.Name, a)
			}
		}
	}
}

func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("go.mod not found")
	return ""
}

// Reverse direction: every sandbox `!` alias in the contract must be a Discord alias for the same
// Action, so anything a tester types in the sandbox works the same way on the real bot.
func TestSandboxBangAliasesExistOnDiscord(t *testing.T) {
	root := findRepoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "contracts", "sandbox", "tool-parity.json"))
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		AgentTools []struct {
			ID   string   `json:"id"`
			Bang []string `json:"bang"`
		} `json:"agent_tools"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	for _, row := range spec.AgentTools {
		for _, alias := range row.Bang {
			cmd, ok := ByAlias(alias)
			if !ok {
				t.Fatalf("sandbox alias !%s (%s) has no Discord command", alias, row.ID)
			}
			if cmd.Action != row.ID {
				t.Fatalf("alias !%s maps to %s on Discord but %s in the sandbox contract", alias, cmd.Action, row.ID)
			}
		}
	}
}
