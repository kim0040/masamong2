// Package docnames holds the rule that a page or message names the slash command the reader's
// language shows, shared by tools/doccheck (Markdown pages) and the Discord command catalog test
// (message strings) so the two never drift: one token rule, one aside form, one registry parse.
package docnames

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Ignore marks a line whose "/word"s are routes or endpoints, not command mentions (Finnhub's
// /quote endpoint): a line holding <!-- doccheck:not-command --> is not checked.
const Ignore = "doccheck:not-command"

var (
	// word is a "/word" mention: not continuing a path, URL or closing tag (no letter, digit,
	// slash or angle bracket before it) and not a path segment, assignment or quoted route
	// (no . ~ = " ' before it).
	word = regexp.MustCompile(`(?:^|[^\p{L}\p{N}_/<>.~="'])/(\p{L}[\p{L}\p{N}_-]*)`)
	// fence is a fenced code block; command-shaped text inside it is a code sample, not a mention.
	fence = regexp.MustCompile("(?ms)^\\s*(```|~~~).*?^\\s*(```|~~~)")
)

// Aside matches another language's name shown right after the page's own: `/own`(`/other`),
// `/own` (`/other`), the plain /own(/other) and the full-width `/own`（`/other`）.
func Aside(own, other string) *regexp.Regexp {
	return regexp.MustCompile("`?" + regexp.QuoteMeta("/"+own) + "`?" + ` *[(（]` + "`?" + regexp.QuoteMeta("/"+other) + "`?" + `[)）]`)
}

// NonASCII reports whether s has a letter outside ASCII (a localized name like 날씨 or 天気).
func NonASCII(s string) bool {
	for _, r := range s {
		if r > unicode.MaxASCII {
			return true
		}
	}
	return false
}

// Match is the known name a "/word" mention starts with, or "". known must be longest first. A
// CJK name may run into a particle (날씨를, お知らせを); an ASCII name only when the rest is another
// script (/weather를), never more ASCII (/admins, /images are not the command).
func Match(w string, known []string) string {
	for _, k := range known {
		if !strings.HasPrefix(w, k) {
			continue
		}
		rest := w[len(k):]
		if r, _ := utf8.DecodeRuneInString(rest); rest == "" || NonASCII(k) || r > unicode.MaxASCII && unicode.IsLetter(r) {
			return k
		}
	}
	return ""
}

// WithoutCode is text with fenced code blocks blanked out: each removed line stays a line, so a
// line number counted on the result still matches the source.
func WithoutCode(text string) string {
	return blanked(text, fence)
}

// Scan calls found for every "/word" mention in text: outside fenced code, outside the aside
// forms, not a path segment (/a/b) and not on a line marked Ignore. line is the 1-based source
// line of the mention.
func Scan(text string, asides []*regexp.Regexp, found func(word string, line int)) {
	text = WithoutCode(text)
	for _, a := range asides {
		text = blanked(text, a)
	}
	for _, m := range word.FindAllStringSubmatchIndex(text, -1) {
		// /word/ continues a path (/status/x); a command name never does
		if m[3] < len(text) && text[m[3]] == '/' {
			continue
		}
		line := 1 + strings.Count(text[:m[0]], "\n")
		if strings.Contains(lineText(text, m[0]), Ignore) {
			continue
		}
		found(text[m[2]:m[3]], line)
	}
}

// blanked replaces every match with the same number of newlines, so a line number counted on the
// result still matches the source.
func blanked(text string, re *regexp.Regexp) string {
	return re.ReplaceAllStringFunc(text, func(s string) string {
		return strings.Repeat("\n", strings.Count(s, "\n"))
	})
}

func lineText(text string, off int) string {
	start := strings.LastIndex(text[:off], "\n") + 1
	if end := strings.Index(text[off:], "\n"); end >= 0 {
		return text[start : off+end]
	}
	return text[start:]
}

// RegistryNames is the slash-command registry a doc checker reads from the Commands() literal of
// internal/connectors/discord/commands.go: every entry's Name except the PrefixOnly ones (extra
// "!" words, not slash commands). It parses the source instead of pattern-matching so a Name:
// string elsewhere (an option choice, a comment) cannot sneak in; the command-catalog test
// compares this list with Commands() itself.
func RegistryNames(src []byte) ([]string, error) {
	f, err := parser.ParseFile(token.NewFileSet(), "commands.go", src, 0)
	if err != nil {
		return nil, err
	}
	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if g, ok := d.(*ast.FuncDecl); ok && g.Name.Name == "Commands" {
			fn = g
		}
	}
	if fn == nil || fn.Body == nil {
		return nil, errors.New("no func Commands in internal/connectors/discord/commands.go")
	}
	seen := map[string]bool{}
	var out []string
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		array, ok := lit.Type.(*ast.ArrayType)
		if !ok {
			return true
		}
		elt, ok := array.Elt.(*ast.Ident)
		if !ok || elt.Name != "Command" {
			return true
		}
		for _, e := range lit.Elts {
			cl, ok := e.(*ast.CompositeLit)
			if !ok {
				continue // a non-literal entry: the catalog test compares with Commands()
			}
			var name string
			prefixOnly := false
			for _, kv := range cl.Elts {
				k, ok := kv.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := k.Key.(*ast.Ident)
				if !ok {
					continue
				}
				switch key.Name {
				case "Name":
					if s, ok := k.Value.(*ast.BasicLit); ok && s.Kind == token.STRING {
						if v, err := strconv.Unquote(s.Value); err == nil {
							name = v
						}
					}
				case "PrefixOnly":
					if b, ok := k.Value.(*ast.Ident); ok && b.Name == "true" {
						prefixOnly = true
					}
				}
			}
			if name != "" && !prefixOnly && !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
		return true
	})
	if len(out) == 0 {
		return nil, errors.New("no command names found in internal/connectors/discord/commands.go")
	}
	sort.Strings(out)
	return out, nil
}
