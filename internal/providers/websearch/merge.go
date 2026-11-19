package websearch

import (
	"net/url"
	"sort"
	"strings"
	"unicode"
)

// CanonicalURL is the address two services' results are compared by: scheme and host in lower case,
// no fragment, no tracking parameters (utm_*, fbclid, gclid), no www. or m. in front of the host, no
// trailing slash, no default port; http and https are one page. An address that does not parse is
// returned trimmed, so it only equals itself.
func CanonicalURL(raw string) string {
	c, _ := canonicalHost(raw)
	return c
}

// canonicalHost is CanonicalURL and the host it kept ("" when the address does not parse), so a caller
// that needs both does not parse twice.
func canonicalHost(raw string) (string, string) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw, ""
	}
	host := strings.ToLower(u.Hostname())
	for _, p := range []string{"www.", "m."} {
		if rest := strings.TrimPrefix(host, p); rest != host && strings.Contains(rest, ".") {
			host = rest
			break
		}
	}
	if p := u.Port(); p != "" && p != "80" && p != "443" {
		host += ":" + p
	}
	path := strings.TrimRight(u.EscapedPath(), "/")
	var keep []string
	for kv := range strings.SplitSeq(u.RawQuery, "&") {
		name, _, _ := strings.Cut(kv, "=")
		low := strings.ToLower(name)
		if kv == "" || strings.HasPrefix(low, "utm_") || low == "fbclid" || low == "gclid" {
			continue
		}
		keep = append(keep, kv)
	}
	if len(keep) > 1 {
		sort.Strings(keep)
	}
	out := "https://" + host + path
	if len(keep) > 0 {
		out += "?" + strings.Join(keep, "&")
	}
	return out, host
}

// MergeResults interleaves result lists by rank (every list's first result, then every list's second,
// in the order the lists are given) and drops a page already taken under another address of it
// (CanonicalURL), or a copy of it on the same site under another path (the same title and the same
// snippet: sameText). The page kept is the first one met; what it lacks (text, date, snippet) is filled
// from the copy that was dropped. The order is fixed by the inputs alone.
func MergeResults(lists ...[]Result) []Result {
	total := 0
	for _, list := range lists {
		total += len(list)
	}
	out := make([]Result, 0, min(total, 16))
	hosts := make([]string, 0, cap(out))    // each kept page's canonical host
	aliases := make([]textKey, 0, cap(out)) // each kept page's sameText key, remade when a copy fills its title or snippet
	at := make(map[string]int, total)
	byText := make(map[textKey]int, total)
	// fill merges a copy into the page kept: what it lacks is taken from the copy, and a title or
	// snippet now known gives the kept page a new key a later copy is known by.
	fill := func(i int, r Result) {
		k := &out[i]
		if k.Text == "" {
			k.Text = r.Text
		}
		if k.Published == "" {
			k.Published = r.Published
		}
		grew := false
		if k.Snippet == "" && r.Snippet != "" {
			k.Snippet, grew = r.Snippet, true
		}
		if k.Title == "" && r.Title != "" {
			k.Title, grew = r.Title, true
		}
		if grew {
			if a := sameText(hosts[i], *k); a != (textKey{}) {
				aliases[i] = a
				if _, seen := byText[a]; !seen {
					byText[a] = i // the kept page now has the title and snippet a copy is known by
				}
			}
		}
	}
	for rank := 0; ; rank++ {
		more := false
		for _, list := range lists {
			if rank >= len(list) {
				continue
			}
			more = true
			r := list[rank]
			key, host := canonicalHost(r.URL)
			i, dup := at[key]
			var alias textKey
			if !dup {
				if alias = sameText(host, r); alias != (textKey{}) {
					if i, dup = byText[alias]; !dup {
						byText[alias] = len(out)
					}
				}
			}
			if dup {
				if _, seen := at[key]; !seen {
					at[key] = i // this address is a copy of the page kept: met again, it is merged too
				}
				fill(i, r)
				continue
			}
			at[key] = len(out)
			hosts = append(hosts, host)
			aliases = append(aliases, alias)
			out = append(out, r)
		}
		if !more {
			return dropLateCopies(out, aliases)
		}
	}
}

// dropLateCopies drops a page that became a copy of an earlier one only once a later result filled in its
// title or snippet (a page met empty first, its copy under another address in between): the earlier page
// is kept, with what it lacks taken from the copy. aliases is each page's sameText key, from MergeResults.
func dropLateCopies(out []Result, aliases []textKey) []Result {
	first := map[textKey]int{}
	kept := out[:0]
	for i, r := range out {
		alias := aliases[i]
		if j, ok := first[alias]; ok && alias != (textKey{}) {
			k := &kept[j]
			if k.Text == "" {
				k.Text = r.Text
			}
			if k.Published == "" {
				k.Published = r.Published
			}
			continue
		}
		if alias != (textKey{}) {
			first[alias] = len(kept)
		}
		kept = append(kept, r)
	}
	return kept
}

// textKey is the key a page is a copy of another under: its site (the canonical host), title and
// snippet, folded. The zero key marks a result that is only itself.
type textKey struct {
	host, title, snippet string
}

// sameText is a page's copy key. Pages of one site often share a title (a school's notices are all
// "입학정보"), so the snippet must match too, and a title under eight characters or a snippet under
// twenty is too short to tell a copy from another notice: such a result is only itself (zero key).
func sameText(host string, r Result) textKey {
	if host == "" {
		return textKey{}
	}
	title, tl := fold(r.Title, 200)
	if tl < 8 {
		return textKey{}
	}
	snippet, sl := fold(r.Snippet, 300)
	if sl < 20 {
		return textKey{}
	}
	return textKey{host, title, snippet}
}

// fold is a string compared case- and space-insensitively: lower case, each run of white space one
// blank, capped at n runes; the count returned is its length in runes.
func fold(s string, n int) (string, int) {
	var b strings.Builder
	b.Grow(min(len(s), n))
	space, count := false, 0
	for _, r := range s {
		switch {
		case r >= 0xAC00 && r <= 0xD7A3:
			// a Hangul syllable: never white space, never cased
		case unicode.IsSpace(r):
			space = b.Len() > 0
			continue
		case 'A' <= r && r <= 'Z':
			r += 'a' - 'A'
		case r > '~':
			r = unicode.ToLower(r)
		}
		if space {
			space = false
			if count >= n {
				break
			}
			b.WriteByte(' ')
			count++
		}
		if count >= n {
			break
		}
		b.WriteRune(r)
		count++
	}
	return b.String(), count
}
