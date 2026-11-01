package discord

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"

	"masamong/internal/action"
)

// Discord's limits for application commands (docs: Application Commands → Registering a Command).
var cmdNameRE = regexp.MustCompile(`^[-_\p{L}\p{N}]{1,32}$`)

func checkName(t *testing.T, where, name string) {
	t.Helper()
	if !cmdNameRE.MatchString(name) || strings.ToLower(name) != name {
		t.Fatalf("%s: name %q must be 1-32 lowercase letters, digits, - or _", where, name)
	}
}

func checkDesc(t *testing.T, where, desc string) {
	t.Helper()
	if n := utf8.RuneCountInString(desc); n < 1 || n > 100 {
		t.Fatalf("%s: description %q has %d characters (1-100)", where, desc, n)
	}
}

func TestEverySlashCommandFitsDiscordLimits(t *testing.T) {
	// every catalog language is loaded, so every localization is checked
	cmds := ApplicationCommands()
	if len(cmds) > 100 {
		t.Fatalf("%d commands (max 100)", len(cmds))
	}
	for _, c := range cmds {
		if c.Type == discordgo.MessageApplicationCommand {
			// an Apps menu item: 1-32 characters, spaces and capitals allowed, no description
			names := []string{c.Name}
			if c.NameLocalizations != nil {
				for _, v := range *c.NameLocalizations {
					names = append(names, v)
				}
			}
			for _, n := range names {
				if l := utf8.RuneCountInString(n); l < 1 || l > 32 || c.Description != "" {
					t.Fatalf("message command %q: name 1-32 characters and no description", n)
				}
			}
			continue
		}
		checkName(t, c.Name, c.Name)
		checkDesc(t, c.Name, c.Description)
		total := utf8.RuneCountInString(c.Name) + utf8.RuneCountInString(c.Description)
		if c.NameLocalizations != nil {
			for loc, v := range *c.NameLocalizations {
				checkName(t, c.Name+"@"+string(loc), v)
			}
		}
		if c.DescriptionLocalizations != nil {
			for loc, v := range *c.DescriptionLocalizations {
				checkDesc(t, c.Name+"@"+string(loc), v)
			}
		}
		if len(c.Options) > 25 {
			t.Fatalf("%s: %d options", c.Name, len(c.Options))
		}
		optional := false
		seen := map[string]bool{}
		for _, o := range c.Options {
			where := c.Name + "." + o.Name
			checkName(t, where, o.Name)
			checkDesc(t, where, o.Description)
			if seen[o.Name] {
				t.Fatalf("%s: duplicate option", where)
			}
			seen[o.Name] = true
			if o.Required && optional {
				t.Fatalf("%s: required options must come before optional ones", where)
			}
			optional = optional || !o.Required
			for loc, v := range o.NameLocalizations {
				checkName(t, where+"@"+string(loc), v)
			}
			for loc, v := range o.DescriptionLocalizations {
				checkDesc(t, where+"@"+string(loc), v)
			}
			if len(o.Choices) > 25 {
				t.Fatalf("%s: %d choices", where, len(o.Choices))
			}
			for _, ch := range o.Choices {
				if n := utf8.RuneCountInString(ch.Name); n < 1 || n > 100 {
					t.Fatalf("%s: choice %q", where, ch.Name)
				}
				for loc, v := range ch.NameLocalizations {
					if n := utf8.RuneCountInString(v); n < 1 || n > 100 {
						t.Fatalf("%s: choice %q@%s", where, v, loc)
					}
				}
				if s, ok := ch.Value.(string); ok && utf8.RuneCountInString(s) > 100 {
					t.Fatalf("%s: choice value too long", where)
				}
			}
			total += utf8.RuneCountInString(o.Name) + utf8.RuneCountInString(o.Description)
		}
		if total > 4000 {
			t.Fatalf("%s: %d characters in all (max 4000)", c.Name, total)
		}
	}
}

// A card always renders inside Discord's embed limits, however long the text a model wrote, and
// Markdown Discord does not show in embeds (headers, tables) is turned into what it does show.
func TestCardsFitEmbedLimits(t *testing.T) {
	long := strings.Repeat("가나다라마바사 ", 1200) // ~9600 characters
	c := action.Card{Title: strings.Repeat("제목", 200), Text: "## 핵심 3줄\n- 하나\n| a | b |\n|---|---|\n| 1 | 2 |\n" + long, Footer: strings.Repeat("f", 3000)}
	for i := 0; i < 30; i++ {
		c.Fields = append(c.Fields, action.CardField{Name: strings.Repeat("n", 300), Value: strings.Repeat("v", 2000)})
	}
	for i := 0; i < 30; i++ {
		c.Buttons = append(c.Buttons, action.CardButton{ID: "x:y:" + strings.Repeat("z", 200), Label: strings.Repeat("l", 100)})
	}
	r := renderCard(c, false)
	embeds := append([]*discordgo.MessageEmbed{r.Embed}, r.More...)
	if len(embeds) > 10 {
		t.Fatalf("%d embeds (max 10)", len(embeds))
	}
	total := 0
	for _, e := range embeds {
		if utf8.RuneCountInString(e.Title) > 256 || utf8.RuneCountInString(e.Description) > 4096 || len(e.Fields) > 25 {
			t.Fatalf("embed over its limits: title %d desc %d fields %d", utf8.RuneCountInString(e.Title), utf8.RuneCountInString(e.Description), len(e.Fields))
		}
		total += utf8.RuneCountInString(e.Title) + utf8.RuneCountInString(e.Description)
		for _, f := range e.Fields {
			if utf8.RuneCountInString(f.Name) > 256 || utf8.RuneCountInString(f.Value) > 1024 {
				t.Fatal("field over its limits")
			}
			total += utf8.RuneCountInString(f.Name) + utf8.RuneCountInString(f.Value)
		}
		if e.Footer != nil {
			if utf8.RuneCountInString(e.Footer.Text) > 2048 {
				t.Fatal("footer over its limit")
			}
			total += utf8.RuneCountInString(e.Footer.Text)
		}
	}
	if total > 6000 {
		t.Fatalf("%d characters across the embeds of one message (max 6000)", total)
	}
	if strings.Contains(r.Embed.Description, "## ") || strings.Contains(r.Embed.Description, "|---|") {
		t.Fatalf("headers and tables must be converted: %q", r.Embed.Description[:120])
	}
	if !strings.Contains(r.Embed.Description, "**핵심 3줄**") {
		t.Fatalf("a header becomes a bold line: %q", r.Embed.Description[:60])
	}
	if len(r.Components) > 5 {
		t.Fatalf("%d action rows (max 5)", len(r.Components))
	}
	for _, row := range r.Components {
		btns := row.(discordgo.ActionsRow).Components
		if len(btns) > 5 {
			t.Fatal("more than 5 buttons in a row")
		}
		for _, b := range btns {
			bb := b.(discordgo.Button)
			if utf8.RuneCountInString(bb.Label) > 80 || len(bb.CustomID) > 100 {
				t.Fatalf("button over its limits: %d %d", utf8.RuneCountInString(bb.Label), len(bb.CustomID))
			}
		}
	}
}

func TestSplitMessageKeepsDiscordsLimit(t *testing.T) {
	for _, part := range splitMessage(strings.Repeat("한국어 문장입니다. ", 700), 1900) {
		if utf8.RuneCountInString(part) > 2000 {
			t.Fatalf("part of %d characters", utf8.RuneCountInString(part))
		}
	}
}
