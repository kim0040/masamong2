// Package relnotes holds the release notes: one JSON file per version under release-notes/, embedded in
// the binary so a build knows every note up to its own version without a network. The notes sort each
// change into what people feel in Discord (user) and what only the operator sees: install, console,
// configuration, internals (operator). The package validates the files, selects the notes between two
// versions, and renders them as Markdown (GitHub release, console) or as a short Discord card.
//
// The bot, the console and tools/relnotes all go through this package; no other code reads the files.
package relnotes

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Languages are the languages a note may carry: the console languages (locales/ui). ko and en are
// required; any other language that is missing falls back to en when a note is rendered.
var Languages = []string{"ko", "en", "es", "ja", "zh-Hans"}

// RepoURL is the only address an item may link to (or anything below it).
const RepoURL = "https://github.com/kim0040/masamong2"

const (
	maxItems     = 12  // per audience and language
	maxItemRunes = 200 // one short sentence
)

// Audience selects which part of a note is shown.
type Audience string

const (
	// AudienceUser is what a Discord user feels: new features and fixed problems in chat.
	AudienceUser Audience = "user"
	// AudienceOperator is what only the person running the bot sees: install, console, config, internals.
	AudienceOperator Audience = "operator"
	// AudienceAll is both, user items first.
	AudienceAll Audience = "all"
)

// Note is one version's release notes (release-notes/<Version>.json).
type Note struct {
	Version  string              `json:"version"`  // "v1.5.1"
	Date     string              `json:"date"`     // "2026-11-22"
	User     map[string][]string `json:"user"`     // language → short plain sentences
	Operator map[string][]string `json:"operator"` // language → short plain sentences
}

// Parse decodes and validates one note file. Unknown fields are an error.
func Parse(data []byte) (Note, error) {
	var n Note
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&n); err != nil {
		return Note{}, fmt.Errorf("release note: %w", err)
	}
	if dec.More() {
		return Note{}, fmt.Errorf("release note: extra data after the JSON object")
	}
	if err := n.Validate(); err != nil {
		return Note{}, err
	}
	return n, nil
}

// Marshal writes the note as the repository stores it: indented JSON (languages in alphabetical
// order, nothing HTML-escaped) and a trailing newline. A missing list is written as [].
func Marshal(n Note) ([]byte, error) {
	fix := func(m map[string][]string) map[string][]string {
		out := make(map[string][]string, len(m))
		for k, v := range m {
			if v == nil {
				v = []string{}
			}
			out[k] = v
		}
		return out
	}
	n.User, n.Operator = fix(n.User), fix(n.Operator)
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(n); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

var (
	noteVersionRE = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$`)
	urlRE         = regexp.MustCompile(`(?i)(?:https?://|www\.|ftp://)[^\s)>\]]*`)
	snowflakeRE   = regexp.MustCompile(`[0-9]{17,20}`)
)

// Validate checks a note against the schema: a semantic version and a real date; ko and en present for
// both audiences; every other language one of Languages; each language with as many items as en; items
// short, plain (one line, no list markers), with no link except one to the repository, no Discord
// mention or ID-sized number, and not an unedited draft line ("TODO: …").
func (n Note) Validate() error {
	if !noteVersionRE.MatchString(n.Version) {
		return fmt.Errorf("release note: version %q is not vMAJOR.MINOR.PATCH", n.Version)
	}
	if _, err := time.Parse("2006-01-02", n.Date); err != nil {
		return fmt.Errorf("release note %s: date %q is not YYYY-MM-DD", n.Version, n.Date)
	}
	for _, a := range []struct {
		name string
		m    map[string][]string
	}{{"user", n.User}, {"operator", n.Operator}} {
		if err := validateAudience(n.Version, a.name, a.m); err != nil {
			return err
		}
	}
	if len(n.User["en"])+len(n.Operator["en"]) == 0 {
		return fmt.Errorf("release note %s: no items at all", n.Version)
	}
	return nil
}

func validateAudience(version, name string, m map[string][]string) error {
	for _, req := range []string{"ko", "en"} {
		if _, ok := m[req]; !ok {
			return fmt.Errorf("release note %s: %s has no %q list (write an empty list when there is nothing)", version, name, req)
		}
	}
	want := len(m["en"])
	for lang, items := range m {
		known := false
		for _, k := range Languages {
			known = known || k == lang
		}
		if !known {
			return fmt.Errorf("release note %s: %s language %q is not a console language %v", version, name, lang, Languages)
		}
		if len(items) > maxItems {
			return fmt.Errorf("release note %s: %s.%s has %d items (at most %d)", version, name, lang, len(items), maxItems)
		}
		if len(items) != want {
			return fmt.Errorf("release note %s: %s.%s has %d items but %s.en has %d (same items, same order)", version, name, lang, len(items), name, want)
		}
		for i, it := range items {
			if err := validateItem(it); err != nil {
				return fmt.Errorf("release note %s: %s.%s[%d]: %w", version, name, lang, i, err)
			}
		}
	}
	return nil
}

func validateItem(s string) error {
	t := strings.TrimSpace(s)
	switch {
	case t == "":
		return fmt.Errorf("empty item")
	case t != s:
		return fmt.Errorf("item has leading or trailing spaces")
	case utf8.RuneCountInString(s) > maxItemRunes:
		return fmt.Errorf("item is longer than %d characters; split it or shorten it", maxItemRunes)
	case strings.IndexFunc(s, func(r rune) bool { return unicode.IsControl(r) }) >= 0:
		return fmt.Errorf("item has a line break or control character")
	case strings.HasPrefix(s, "TODO:"):
		return fmt.Errorf("item is an unedited draft line")
	case startsWithMarker(s):
		return fmt.Errorf("item starts with a Markdown list or heading marker; write the sentence only")
	case strings.Contains(s, "<@") || strings.Contains(s, "<#") || strings.Contains(strings.ToLower(s), "@everyone") || strings.Contains(strings.ToLower(s), "@here"):
		return fmt.Errorf("item has a Discord mention")
	case snowflakeRE.MatchString(s):
		return fmt.Errorf("item has an ID-sized number")
	}
	for _, u := range urlRE.FindAllString(s, -1) {
		if !strings.HasPrefix(u, RepoURL) || (len(u) > len(RepoURL) && !strings.ContainsRune("/#?", rune(u[len(RepoURL)]))) {
			return fmt.Errorf("item links to %q; only %s is allowed", u, RepoURL)
		}
	}
	return nil
}

// startsWithMarker reports whether s begins like a Markdown list item, quote or heading.
func startsWithMarker(s string) bool {
	r, _ := utf8.DecodeRuneInString(s)
	if strings.ContainsRune("-*+#>•", r) {
		return true
	}
	digits := strings.TrimLeft(s, "0123456789")
	return digits != s && (strings.HasPrefix(digits, ". ") || strings.HasPrefix(digits, ") "))
}
