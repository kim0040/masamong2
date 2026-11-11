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
// (CanonicalURL). The page kept is the first one met; what it lacks (text, date, snippet) is filled
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
			if i, dup := at[key]; dup {
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
