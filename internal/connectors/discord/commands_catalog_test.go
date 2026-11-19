package discord

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"masamong/internal/docnames"
	"masamong/internal/i18n"
	"masamong/locales"
)

// slashCommands is the registered slash surface: PrefixOnly entries are extra "!" words, not
// slash commands.
func slashCommands() []Command {
	var out []Command
	for _, c := range Commands() {
		if !c.PrefixOnly {
			out = append(out, c)
		}
	}
	return out
}

// TestSlashNamesInCatalogs keeps message and console strings honest: a string that names a slash
// command must use the name its own locale shows. Discord registers the canonical English name
// plus each locale's discord.command.<name>.name, so a Japanese member sees /運勢, never /fortune.
// "!" prefix words are not checked: their aliases merge every locale.
func TestSlashNamesInCatalogs(t *testing.T) {
	cat := i18n.New()
	if err := cat.LoadFS(locales.UIFS); err != nil {
		t.Fatalf("ui catalogs: %v", err)
	}
	cmds := slashCommands()
	name := func(locale string, c Command) string {
		if v, ok := cat.Lookup(locale, "discord.command."+c.Name+".name"); ok {
			return v
		}
		return c.Name
	}
	// known: every spelling a command goes by in some locale (longest first); valid: a locale's own
	valid := map[string]map[string]bool{}
	var known []string
	seen := map[string]bool{}
	add := func(n string) {
		if !seen[n] {
			seen[n] = true
			known = append(known, n)
		}
	}
	valid["en"] = map[string]bool{}
	for _, c := range cmds {
		valid["en"][c.Name] = true
		add(c.Name)
	}
	for _, loc := range cat.Locales() {
		if loc == "en" {
			continue
		}
		valid[loc] = map[string]bool{}
		for _, c := range cmds {
			n := name(loc, c)
			valid[loc][n] = true
			add(n)
		}
	}
	sort.Slice(known, func(i, j int) bool { return len(known[i]) > len(known[j]) })

	check := func(source, locale, text string) {
		// `/own`(`/other`) shows another language's name as an aside; that form is allowed
		var asides []*regexp.Regexp
		for _, c := range cmds {
			own := name(locale, c)
			var others []string
			for loc := range valid {
				if n := name(loc, c); n != own {
					others = append(others, n)
				}
			}
			if c.Name != own {
				others = append(others, c.Name)
			}
			for _, o := range others {
				asides = append(asides, docnames.Aside(own, o))
			}
		}
		docnames.Scan(text, asides, func(w string, _ int) {
			if got := docnames.Match(w, known); got != "" && !valid[locale][got] {
				t.Errorf("%s: /%s is not the %s command name", source, w, locale)
			}
		})
	}
	for _, bundle := range []fs.FS{locales.FS, locales.UIFS} {
		err := fs.WalkDir(bundle, ".", func(p string, e fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				return nil
			}
			raw, err := fs.ReadFile(bundle, p)
			if err != nil {
				return err
			}
			var msgs map[string]string
			if err := json.Unmarshal(raw, &msgs); err != nil {
				t.Fatalf("%s: %v", p, err)
			}
			locale := i18n.Normalize(strings.TrimSuffix(e.Name(), ".json"))
			if _, ok := valid[locale]; !ok {
				locale = "en"
			}
			for k, v := range msgs {
				check(p+" "+k, locale, v)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// TestRegistryNamesMatchCommands keeps the names tools/doccheck parses out of this file in step
// with the commands it actually registers: a new slash command is checked in docs, and a renamed or
// prefix-only one stops being treated as a slash name.
func TestRegistryNamesMatchCommands(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(findRepoRoot(t), "internal/connectors/discord/commands.go"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := docnames.RegistryNames(raw)
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, c := range slashCommands() {
		want = append(want, c.Name)
	}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("registry names %v, want %v", got, want)
	}
}
