package websearch

import (
	"net/url"
	"sort"
	"strings"
)

// CanonicalURL is the address two services' results are compared by: scheme and host in lower case,
// no fragment, no tracking parameters (utm_*, fbclid, gclid), no www. or m. in front of the host, no
// trailing slash, no default port; http and https are one page. An address that does not parse is
// returned trimmed, so it only equals itself.
func CanonicalURL(raw string) string {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
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
	for _, kv := range strings.Split(u.RawQuery, "&") {
		name, _, _ := strings.Cut(kv, "=")
		low := strings.ToLower(name)
		if kv == "" || strings.HasPrefix(low, "utm_") || low == "fbclid" || low == "gclid" {
			continue
		}
		keep = append(keep, kv)
	}
	sort.Strings(keep)
	out := "https://" + host + path
	if len(keep) > 0 {
		out += "?" + strings.Join(keep, "&")
	}
	return out
}

// MergeResults interleaves result lists by rank (every list's first result, then every list's second,
// in the order the lists are given) and drops a page already taken under another address of it
// (CanonicalURL), or a copy of it on the same site under another path (the same title and the same
// snippet: sameText). The page kept is the first one met; what it lacks (text, date, snippet) is filled
// from the copy that was dropped. The order is fixed by the inputs alone.
func MergeResults(lists ...[]Result) []Result {
	var out []Result
	at := map[string]int{}
	for rank := 0; ; rank++ {
		more := false
		for _, list := range lists {
			if rank >= len(list) {
				continue
			}
			more = true
			r := list[rank]
			key := CanonicalURL(r.URL)
			i, dup := at[key]
			if !dup {
				if alias := sameText(key, r); alias != "" {
					if i, dup = at[alias]; !dup {
						at[alias] = len(out)
					}
				}
			}
			if dup {
				if _, seen := at[key]; !seen {
					at[key] = i // this address is a copy of the page kept: met again, it is merged too
				}
				k := &out[i]
				if k.Text == "" {
					k.Text = r.Text
				}
				if k.Published == "" {
					k.Published = r.Published
				}
				if k.Snippet == "" {
					k.Snippet = r.Snippet
				}
				if k.Title == "" {
					k.Title = r.Title
				}
				if alias := sameText(CanonicalURL(k.URL), *k); alias != "" {
					if _, seen := at[alias]; !seen {
						at[alias] = i // the kept page now has the title and snippet a copy is known by
					}
				}
				continue
			}
			at[key] = len(out)
			out = append(out, r)
		}
		if !more {
			return out
		}
	}
}

// sameText is the key a page is a copy of another under: its site, title and the start of its snippet,
// folded. Pages of one site often share a title (a school's notices are all "입학정보"), so the snippet
// must match too, and a title under eight characters or a snippet under twenty is too short to tell a
// copy from another notice: such a result is only itself ("").
func sameText(canonical string, r Result) string {
	fold := func(s string, n int) string {
		s = strings.ToLower(strings.Join(strings.Fields(s), " "))
		if rs := []rune(s); len(rs) > n {
			s = string(rs[:n])
		}
		return s
	}
	title, snippet := fold(r.Title, 200), fold(r.Snippet, 80)
	if len([]rune(title)) < 8 || len([]rune(snippet)) < 20 {
		return ""
	}
	u, err := url.Parse(canonical)
	if err != nil || u.Host == "" {
		return ""
	}
	return "same:" + u.Host + "|" + title + "|" + snippet
}
