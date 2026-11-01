// Package i18n resolves message keys against the embedded catalogs (15장).
// Missing keys fall back to English, then to a safe generic string; a raw key never reaches the user surface.
package i18n

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"sync"

	"masamong/locales"
)

type Catalog struct {
	mu   sync.RWMutex
	msgs map[string]map[string]string // locale -> key -> text
}

// New loads the embedded catalogs. It panics only if the embedded JSON is malformed, which is a build error.
func New() *Catalog {
	c := &Catalog{msgs: map[string]map[string]string{}}
	if err := c.LoadFS(locales.FS); err != nil {
		panic(fmt.Sprintf("i18n: embedded catalog invalid: %v", err))
	}
	return c
}

// LoadFS merges every *.json in fsys. Separate calls may override keys, but files within
// one FS must have distinct normalized locales. An invalid load changes nothing.
func (c *Catalog) LoadFS(fsys fs.FS) error {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return err
	}
	incoming := make(map[string]map[string]string)
	seen := make(map[string]string)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		locale, ok := canonical(strings.TrimSuffix(e.Name(), ".json"))
		if !ok {
			return fmt.Errorf("invalid catalog filename %q", e.Name())
		}
		if previous, exists := seen[locale]; exists {
			return fmt.Errorf("catalogs %q and %q both normalize to %q", previous, e.Name(), locale)
		}
		seen[locale] = e.Name()
		b, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return err
		}
		m := map[string]string{}
		if err := json.Unmarshal(b, &m); err != nil {
			return fmt.Errorf("%s: %w", e.Name(), err)
		}
		incoming[locale] = m
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	merged := make(map[string]map[string]string, len(c.msgs)+len(incoming))
	for locale, messages := range c.msgs {
		merged[locale] = make(map[string]string, len(messages))
		for k, v := range messages {
			merged[locale][k] = v
		}
	}
	for locale, messages := range incoming {
		if merged[locale] == nil {
			merged[locale] = make(map[string]string)
		}
		for k, v := range messages {
			merged[locale][k] = v
		}
	}
	for locale, messages := range merged {
		if locale == "en" {
			continue
		}
		for key, text := range messages {
			if base, ok := merged["en"][key]; ok && !SamePlaceholders(base, text) {
				return fmt.Errorf("catalog %s key %s: placeholders differ from English", locale, key)
			}
		}
	}
	c.msgs = merged
	return nil
}

// T resolves key for locale. args are name/value pairs substituted into {name} placeholders.
// Unresolved placeholders are removed so a template never leaks to the screen.
func (c *Catalog) T(locale, key string, args ...any) string {
	locale = Normalize(locale)
	c.mu.RLock()
	text := ""
	if loc, ok := c.msgs[locale]; ok {
		text = loc[key]
	}
	if text == "" {
		text = c.msgs["en"][key]
	}
	if text == "" {
		text = c.msgs["en"]["generic.error"]
	}
	c.mu.RUnlock()
	if text == "" {
		text = "An error occurred."
	}
	return Fill(text, args...)
}

// Lookup returns key's text in exactly this locale (no fallback), for callers that build per-locale
// maps such as Discord name localizations.
func (c *Catalog) Lookup(locale, key string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.msgs[Normalize(locale)][key]
	return v, ok && v != ""
}

// Has reports whether key exists in the English base catalog.
func (c *Catalog) Has(key string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.msgs["en"][key]
	return ok
}

// Locales lists the loaded bundles.
func (c *Catalog) Locales() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]string, 0, len(c.msgs))
	for k := range c.msgs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// MissingKeys returns English keys absent from locale (used by locales check).
func (c *Catalog) MissingKeys(locale string) []string {
	locale = Normalize(locale)
	c.mu.RLock()
	defer c.mu.RUnlock()
	var out []string
	for k := range c.msgs["en"] {
		if _, ok := c.msgs[locale][k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Fill substitutes {name} placeholders from name/value pairs and strips any left over.
func Fill(text string, args ...any) string {
	for i := 0; i+1 < len(args); i += 2 {
		name, _ := args[i].(string)
		text = strings.ReplaceAll(text, "{"+name+"}", fmt.Sprint(args[i+1]))
	}
	if strings.Contains(text, "{") {
		var b strings.Builder
		depth := 0
		for _, r := range text {
			switch {
			case r == '{':
				depth++
			case r == '}' && depth > 0:
				depth--
			case depth == 0:
				b.WriteRune(r)
			}
		}
		text = strings.Join(strings.Fields(b.String()), " ")
	}
	return text
}

// Normalize maps requested tags to catalog/pack filenames: ordinary region variants
// use the primary language (fr-CA → fr); Unicode/private-use extensions
// are validated then ignored (fr-u-nu-latn → fr). zh, zh-CN, zh-SG and
// zh-Hans use zh-Hans, while traditional Chinese uses zh-Hant. Filename
// collisions after normalization are invalid within one FS; support is decided
// by Resolve/Get, not Normalize. Empty requests map to en; malformed ones
// produce an unloaded tag and therefore fall back to English via Resolve/T.
func Normalize(tag string) string {
	if strings.TrimSpace(tag) == "" {
		return "en"
	}
	locale, ok := canonical(tag)
	if !ok {
		return ""
	}
	return locale
}

func canonical(tag string) (string, bool) {
	t := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(tag, "_", "-")))
	parts := strings.Split(t, "-")
	if len(parts[0]) < 2 || len(parts[0]) > 8 || !asciiLetters(parts[0]) {
		return "", false
	}
	baseEnd := len(parts)
	seenExtensions := map[string]bool{}
	for i := 1; i < len(parts); {
		part := parts[i]
		if len(part) == 1 {
			if !asciiAlnum(part) || seenExtensions[part] || i+1 == len(parts) {
				return "", false
			}
			if baseEnd == len(parts) {
				baseEnd = i
			}
			seenExtensions[part] = true
			i++
			if part == "x" { // private-use subtags may be one character long
				for ; i < len(parts); i++ {
					if len(parts[i]) < 1 || len(parts[i]) > 8 || !asciiAlnum(parts[i]) {
						return "", false
					}
				}
				break
			}
			count := 0
			for i < len(parts) && len(parts[i]) != 1 {
				if len(parts[i]) < 2 || len(parts[i]) > 8 || !asciiAlnum(parts[i]) {
					return "", false
				}
				count++
				i++
			}
			if count == 0 {
				return "", false
			}
			continue
		}
		if baseEnd != len(parts) || len(part) < 2 || len(part) > 8 || !asciiAlnum(part) {
			return "", false
		}
		i++
	}
	base := parts[:baseEnd]
	if base[0] != "zh" {
		return base[0], true
	}
	if len(base) == 1 || base[1] == "hans" || base[1] == "cn" || base[1] == "sg" {
		return "zh-Hans", true
	}
	if base[1] == "hant" || base[1] == "tw" || base[1] == "hk" || base[1] == "mo" {
		return "zh-Hant", true
	}
	return strings.Join(base, "-"), true // an unknown variant must not select Simplified Chinese
}

func asciiAlnum(s string) bool {
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func asciiLetters(s string) bool {
	for _, r := range s {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return true
}

var placeholderPattern = regexp.MustCompile(`\{[A-Za-z_][A-Za-z0-9_]*\}`)

// SamePlaceholders compares names AND multiplicity, ignoring JSON braces and
// schema examples such as {"key":...} or {...} rather than treating them as arguments.
func SamePlaceholders(english, translated string) bool {
	counts := make(map[string]int)
	for _, match := range placeholderPattern.FindAllString(english, -1) {
		counts[match]++
	}
	for _, match := range placeholderPattern.FindAllString(translated, -1) {
		counts[match]--
	}
	for _, count := range counts {
		if count != 0 {
			return false
		}
	}
	return true
}

// Resolve normalizes a tag and falls back to English when that locale is not loaded, so callers
// can hand it straight to T without producing untranslated keys.
func (c *Catalog) Resolve(tag string) string {
	norm := Normalize(tag)
	c.mu.RLock()
	defer c.mu.RUnlock()
	if _, ok := c.msgs[norm]; ok {
		return norm
	}
	return "en"
}
