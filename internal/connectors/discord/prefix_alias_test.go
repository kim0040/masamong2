package discord

import "testing"

// Configured words for a tool replace the words of its first command only: the other commands on the same
// Action (!이번달운세, !올해운세, !별자리 on fortune.query) keep their own words.
func TestAliasOverrideKeepsOtherCommandsOnTheAction(t *testing.T) {
	tbl := NewPrefixTable(shippedCatalog(), []ToolCommand{{Tool: "fortune.query", Aliases: []string{"점괘", "luck"}}})
	for word, want := range map[string]string{
		"점괘": "fortune", "luck": "fortune",
		"이번달운세": "fortune-month", "올해운세": "fortune-year", "별자리": "zodiac",
	} {
		c, _, ok := tbl.Parse("!", "!"+word)
		if !ok || c.Name != want {
			t.Errorf("!%s -> ok=%v cmd=%q, want %q", word, ok, c.Name, want)
		}
	}
	// the default word of the overridden command is replaced, not added to
	if c, _, ok := tbl.Parse("!", "!운세"); ok && c.Name == "fortune" {
		t.Errorf("!운세 still opens fortune after the operator replaced its words")
	}
	// without overrides every word opens its own command
	for word, want := range map[string]string{"운세": "fortune", "이번달운세": "fortune-month", "별자리": "zodiac"} {
		if c, _, ok := (PrefixTable{}).Parse("!", "!"+word); !ok || c.Name != want {
			t.Errorf("default !%s -> ok=%v cmd=%q, want %q", word, ok, c.Name, want)
		}
	}
}
