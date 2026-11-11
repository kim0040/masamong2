package websearch

import (
	"net/url"
	"strings"
)

// FetchTarget is the address to ask a page reader for when a search result links to raw, and whether
// the page is worth asking for at all. Not public: no. Naver blog posts: the desktop address
// (blog.naver.com/<id>/<no>) holds the post in an iframe the reader never sees, so the mobile one
// (m.blog.naver.com/<id>/<no>) is asked for. Naver and Daum cafes are behind a login: no.
func FetchTarget(raw string) (string, bool) {
	target, err := PublicURL(raw)
	if err != nil {
		return "", false
	}
	u, err := url.Parse(target)
	if err != nil {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	switch host {
	case "cafe.naver.com", "m.cafe.naver.com", "cafe.daum.net", "m.cafe.daum.net":
		return "", false
	case "blog.naver.com", "www.blog.naver.com":
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) == 2 && parts[0] != "" && isDigits(parts[1]) {
			return "https://m.blog.naver.com/" + parts[0] + "/" + parts[1], true
		}
	}
	return target, true
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
