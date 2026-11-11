package websearch

import (
	"net/url"
	"strings"
)

// FetchTarget is the address to ask a page reader for when a search result links to raw, and whether
// the page is worth asking for at all. Not public: no. Naver blog posts: the desktop address
// (blog.naver.com/<id>/<no>) holds the post in an iframe the reader never sees, so the mobile one
// (m.blog.naver.com/<id>/<no>) is asked for, also for the posts' query-string addresses
// (PostView.naver?blogId=<id>&logNo=<no>, /<id>?Redirect=Log&logNo=<no>). Naver and Daum cafes are behind a login: no.
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
		// the same posts under their other addresses: PostView.naver?blogId=X&logNo=N and /X?Redirect=Log&logNo=N
		if id, no, ok := naverBlogQuery(u, parts); ok {
			return "https://m.blog.naver.com/" + url.PathEscape(id) + "/" + no, true
		}
	}
	return target, true
}

// naverBlogQuery is the blog id and post number of the query-string forms of a desktop Naver blog post.
func naverBlogQuery(u *url.URL, parts []string) (id, no string, ok bool) {
	q := u.Query()
	no = q.Get("logNo")
	if !isDigits(no) {
		return "", "", false
	}
	switch {
	case len(parts) == 1 && (strings.EqualFold(parts[0], "PostView.naver") || strings.EqualFold(parts[0], "PostView.nhn")):
		id = q.Get("blogId")
	case len(parts) == 1 && parts[0] != "" && strings.EqualFold(q.Get("Redirect"), "Log"):
		id = parts[0]
	}
	if id == "" || strings.ContainsAny(id, "/?#& ") {
		return "", "", false
	}
	return id, no, true
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
