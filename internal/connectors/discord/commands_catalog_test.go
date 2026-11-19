package discord

import (
	"encoding/json"
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"masamong/internal/i18n"
	"masamong/locales"
)

var slashWord = regexp.MustCompile(`(?:^|[^\p{L}\p{N}_/<>])/(\p{L}[\p{L}\p{N}_-]*)`)

// TestSlashNamesInCatalogs keeps message and console strings honest: a string that names a slash
// command must use the name its own locale shows. Discord registers the canonical English name
// plus each locale's discord.command.<name>.name, so a Japanese member sees /運勢, never /fortune.
// "!" prefix words are not checked: their aliases merge every locale.
func TestSlashNamesInCatalogs(t *testing.T) {
	cat := i18n.New()
	if err := cat.LoadFS(locales.UIFS); err != nil {
		t.Fatalf("ui catalogs: %v", err)
	}
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
	for _, c := range Commands() {
		valid["en"][c.Name] = true
		add(c.Name)
	}
	for _, loc := range cat.Locales() {
		if loc == "en" {
			continue
		}
		valid[loc] = map[string]bool{}
		for _, c := range Commands() {
			n := name(loc, c)
			valid[loc][n] = true
			add(n)
		}
	}
	sort.Slice(known, func(i, j int) bool { return len(known[i]) > len(known[j]) })

	check := func(source, locale, text string) {
		// `/own`(`/other`) shows another language's name as an aside; that form is allowed
		for _, c := range Commands() {
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
				text = annotation(own, o).ReplaceAllString(text, "")
			}
		}
		for _, m := range slashWord.FindAllStringSubmatchIndex(text, -1) {
			word := text[m[2]:m[3]]
			got := ""
			for _, k := range known {
				if !strings.HasPrefix(word, k) {
					continue
				}
				// a CJK name may run into a particle (날씨를); an ASCII name only when the rest is
				// another script, never more ASCII (/admins, /images are not the command)
				rest := word[len(k):]
				if r, _ := utf8.DecodeRuneInString(rest); rest == "" || nonASCII(k) || r > unicode.MaxASCII && unicode.IsLetter(r) {
					got = k
					break
				}
			}
			if got != "" && !valid[locale][got] {
				t.Errorf("%s: /%s is not the %s command name", source, word, locale)
			}
		}
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

func annotation(own, other string) *regexp.Regexp {
	return regexp.MustCompile("`?" + regexp.QuoteMeta("/"+own) + "`?" + ` *\(` + "`?" + regexp.QuoteMeta("/"+other) + "`?" + `\)`)
}

func nonASCII(s string) bool {
	for _, r := range s {
		if r > unicode.MaxASCII {
			return true
		}
	}
	return false
}
