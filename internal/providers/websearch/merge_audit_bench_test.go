package websearch

import (
	"fmt"
	"testing"
)

// Audit-only benchmark (scratch copy, not in the repo): same inputs on v1.2.0 and v1.2.1.
func auditLists() [][]Result {
	var lists [][]Result
	for s := 0; s < 4; s++ {
		var l []Result
		for i := 0; i < 10; i++ {
			// overlapping pages across services, tracking params, www/m. variants, same-site copies
			host := []string{"https://www.example.com", "https://m.example.com", "http://example.com", "https://news.site.kr"}[s]
			url := fmt.Sprintf("%s/article/%d/?utm_source=x&id=%d", host, (i+s)%12, i%3)
			title := fmt.Sprintf("기사 제목 %d — Example News", (i+s)%12)
			snip := fmt.Sprintf("요약 문장 %d 입니다. 서울 날씨와 관련된 내용이 들어 있습니다.", (i+s)%12)
			if i%4 == 0 {
				url = fmt.Sprintf("%s/copy/%d", host, i) // same text, different path
			}
			l = append(l, Result{Title: title, URL: url, Snippet: snip, Text: snip + " " + snip})
		}
		lists = append(lists, l)
	}
	return lists
}

func BenchmarkMergeResults(b *testing.B) {
	lists := auditLists()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = MergeResults(lists...)
	}
}

func BenchmarkCanonicalURL(b *testing.B) {
	urls := []string{"https://www.Example.com:443/a/b/?utm_source=x&b=2&a=1#frag", "http://m.news.site.kr/view?id=7&fbclid=z", "not a url"}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		for _, u := range urls {
			_ = CanonicalURL(u)
		}
	}
}

func BenchmarkRewordedQueries(b *testing.B) {
	q := Query{Text: "2026년 반도체 수출 동향과 전망 논문", Depth: "deep", Topic: "research"}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = RewordedQueries(q)
	}
}
