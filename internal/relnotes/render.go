package relnotes

import (
	"fmt"
	"strings"
)

// Lang maps any language tag to the language a note is rendered in: one of Languages, or "en" when the
// note has nothing better ("fr", "zh-TW", ""). Case and "_" are forgiven ("ko-KR", "zh_CN").
func Lang(tag string) string {
	t := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(tag), "_", "-"))
	base, rest, _ := strings.Cut(t, "-")
	switch base {
	case "ko", "en", "es", "ja":
		return base
	case "zh":
		if rest == "" || rest == "cn" || rest == "sg" || strings.HasPrefix(rest, "hans") {
			return "zh-Hans"
		}
	}
	return "en"
}

// pick returns the items of lang, or of en when the note has no list for it.
func pick(m map[string][]string, lang string) []string {
	if items, ok := m[Lang(lang)]; ok {
		return items
	}
	return m["en"]
}

// UserItems returns the user-facing sentences in lang (en when the note lacks that language).
func (n Note) UserItems(lang string) []string { return pick(n.User, lang) }

// OperatorItems returns the operator-facing sentences in lang (en when the note lacks that language).
func (n Note) OperatorItems(lang string) []string { return pick(n.Operator, lang) }

// Has reports whether the note has anything to show for the audience.
func (n Note) Has(a Audience) bool {
	switch a {
	case AudienceUser:
		return len(n.User["en"]) > 0
	case AudienceOperator:
		return len(n.Operator["en"]) > 0
	default:
		return len(n.User["en"])+len(n.Operator["en"]) > 0
	}
}

type heading struct{ user, operator string }

var headings = map[string]heading{
	"ko":      {"새 기능과 수정", "운영자용"},
	"en":      {"What's new", "For operators"},
	"es":      {"Novedades", "Para operadores"},
	"ja":      {"新機能と修正", "運用者向け"},
	"zh-Hans": {"新功能与修复", "面向运维"},
}

// sections returns the (heading, items) pairs of the audience in lang, skipping empty ones.
func sections(n Note, a Audience, lang string) (out []struct {
	Title string
	Items []string
}) {
	lang = Lang(lang)
	h := headings[lang]
	add := func(title string, items []string) {
		if len(items) > 0 {
			out = append(out, struct {
				Title string
				Items []string
			}{title, items})
		}
	}
	if a == AudienceUser || a == AudienceAll {
		add(h.user, n.UserItems(lang))
	}
	if a == AudienceOperator || a == AudienceAll {
		add(h.operator, n.OperatorItems(lang))
	}
	return out
}

// Render writes the note as Markdown for the audience in lang: "## v1.5.1 (2026-11-22)" and a
// "###" section with a bullet list for each audience that has items. It returns "" when the audience
// has nothing to show, so callers skip it.
func Render(n Note, a Audience, lang string) string {
	secs := sections(n, a, lang)
	if len(secs) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## %s (%s)\n", n.Version, n.Date)
	for _, s := range secs {
		fmt.Fprintf(&b, "\n### %s\n\n", s.Title)
		for _, it := range s.Items {
			fmt.Fprintf(&b, "- %s\n", it)
		}
	}
	return b.String()
}

// cardLimit keeps a card well inside Discord's 2000-character message limit.
const cardLimit = 1500

// Card writes the note as a short Discord message for the audience in lang: a bold version line and
// one "•" line per item (an audience label before the operator items when both are shown). Items that
// would pass about 1500 characters are cut and counted as "… (+N)". It returns "" when there is
// nothing to show for the audience.
func Card(n Note, a Audience, lang string) string {
	secs := sections(n, a, lang)
	if len(secs) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "**%s** · %s", n.Version, n.Date)
	omitted := 0
	size := len([]rune(b.String()))
	for i, s := range secs {
		if len(secs) > 1 && i > 0 {
			b.WriteString("\n**" + s.Title + "**")
		}
		for _, it := range s.Items {
			line := "\n• " + it
			if size+len([]rune(line)) > cardLimit {
				omitted++
				continue
			}
			b.WriteString(line)
			size += len([]rune(line))
		}
	}
	if omitted > 0 {
		fmt.Fprintf(&b, "\n… (+%d)", omitted)
	}
	return b.String()
}

// RenderRelease writes the Markdown the GitHub release carries: English first, then Korean, each with
// its user and operator sections, then a link to the commit comparison when prev (the previous tag) is
// given. tools/relnotes writes it to release-notes/<version>.md; the release workflow publishes it.
func RenderRelease(n Note, prev string) string {
	var b strings.Builder
	for i, lang := range []string{"en", "ko"} {
		secs := sections(n, AudienceAll, lang)
		if i == 1 && len(secs) > 0 && b.Len() > 0 {
			b.WriteString("\n")
		}
		for j, s := range secs {
			if j > 0 {
				b.WriteString("\n")
			}
			fmt.Fprintf(&b, "## %s\n\n", s.Title)
			for _, it := range s.Items {
				fmt.Fprintf(&b, "- %s\n", it)
			}
		}
	}
	if prev != "" {
		fmt.Fprintf(&b, "\n**Full Changelog**: %s/compare/%s...%s\n", RepoURL, prev, n.Version)
	}
	return b.String()
}
