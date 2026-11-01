package notices

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// Item is one notice found on a list page.
type Item struct {
	SourceID    string `json:"source_id"`
	ExternalID  string `json:"external_id"`
	Title       string `json:"title"`
	URL         string `json:"url"`
	Published   string `json:"published,omitempty"` // YYYY-MM-DD when the list shows a date
	Category    string `json:"category,omitempty"`
	Pinned      bool   `json:"pinned,omitempty"`
	Fingerprint string `json:"fingerprint"` // changes when what people see (title, link) changes
}

var (
	spaceRE = regexp.MustCompile(`[ \t\x{00a0}\r\n]+`)
	dateRE  = regexp.MustCompile(`(20\d{2})\s*[.\-/년]\s*(\d{1,2})\s*[.\-/월]\s*(\d{1,2})`)
	shortRE = regexp.MustCompile(`(?:^|[^\d.])(\d{2})\s*[.\-/]\s*(\d{1,2})\s*[.\-/]\s*(\d{1,2})(?:$|[^\d])`)
)

func inline(s string) string { return strings.TrimSpace(spaceRE.ReplaceAllString(s, " ")) }

// normDate finds a date in text: 2026.09.28, 2026-9-28, 2026년 9월 28일, or 26.09.28.
func normDate(text string) string {
	if m := dateRE.FindStringSubmatch(text); m != nil {
		return validDate(m[1], m[2], m[3])
	}
	if m := shortRE.FindStringSubmatch(text); m != nil {
		return validDate("20"+m[1], m[2], m[3])
	}
	return ""
}

func validDate(y, m, d string) string {
	var yy, mm, dd int
	fmt.Sscanf(y, "%d", &yy)
	fmt.Sscanf(m, "%d", &mm)
	fmt.Sscanf(d, "%d", &dd)
	t := time.Date(yy, time.Month(mm), dd, 0, 0, 0, 0, time.UTC)
	if t.Month() != time.Month(mm) || t.Day() != dd {
		return ""
	}
	return t.Format("2006-01-02")
}

func fingerprint(title, u string) string {
	sum := sha256.Sum256([]byte(title + "\n" + u))
	return hex.EncodeToString(sum[:])
}

func resolve(base, ref string) (string, bool) {
	b, err := url.Parse(base)
	if err != nil {
		return "", false
	}
	r, err := url.Parse(strings.TrimSpace(ref))
	if err != nil {
		return "", false
	}
	u := b.ResolveReference(r)
	if u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
		return "", false
	}
	u.Fragment = ""
	return u.String(), true
}

// ParseList reads a list page into notices, with warnings about what did not work (a selector that
// found nothing usually means the site changed).
func (c *Catalog) ParseList(html string, s Source) ([]Item, []string) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return nil, []string{"html_unreadable"}
	}
	if s.List.RowSelector != "" {
		return c.parseRows(doc, s)
	}
	return c.parseLinks(doc, s)
}

func (c *Catalog) parseRows(doc *goquery.Document, s Source) ([]Item, []string) {
	l := s.List
	var items []Item
	var warnings []string
	seen := map[string]bool{}
	rows := doc.Find(l.RowSelector)
	rows.Each(func(i int, row *goquery.Selection) {
		if includeRE := re(l.IncludeRegex); includeRE != nil {
			node := row
			if l.IncludeSelector != "" {
				node = row.Find(l.IncludeSelector).First()
			}
			if node.Length() == 0 || !includeRE.MatchString(inline(node.Text())) {
				return
			}
		}
		link := row.Find(l.LinkSelector).First()
		if link.Length() == 0 {
			return
		}
		raw, _ := link.Attr(l.IDAttribute)
		m := re(l.IDRegex).FindStringSubmatch(raw)
		if len(m) < 2 {
			warnings = append(warnings, fmt.Sprintf("row_%d:id_not_found", i))
			return
		}
		id := m[1]
		if seen[id] {
			return
		}
		var u string
		if l.DetailURLTemplate != "" {
			u = strings.ReplaceAll(l.DetailURLTemplate, "{id}", url.PathEscape(id))
		} else {
			href, _ := link.Attr("href")
			var ok bool
			if u, ok = resolve(s.ListURL, href); !ok {
				warnings = append(warnings, fmt.Sprintf("row_%d:bad_url", i))
				return
			}
		}
		titleNode := link
		if l.TitleSelector != "" {
			if t := link.Find(l.TitleSelector).First(); t.Length() > 0 {
				titleNode = t
			}
		}
		title := inline(titleNode.Text())
		if title == "" {
			warnings = append(warnings, fmt.Sprintf("row_%d:empty_title", i))
			return
		}
		it := Item{SourceID: s.ID, ExternalID: id, Title: title, URL: u, Fingerprint: fingerprint(title, u)}
		if l.DateSelector != "" {
			it.Published = normDate(inline(row.Find(l.DateSelector).First().Text()))
		}
		if l.CategorySelector != "" {
			it.Category = inline(row.Find(l.CategorySelector).First().Text())
		}
		for _, cl := range l.PinnedClasses {
			if row.HasClass(cl) {
				it.Pinned = true
			}
		}
		seen[id] = true
		items = append(items, it)
	})
	if rows.Length() == 0 {
		warnings = append(warnings, "row_selector_found_nothing")
	} else if len(items) == 0 {
		warnings = append(warnings, "rows_found_but_no_notices")
	}
	return items, warnings
}

// looksLikeTransfer applies the catalog's transfer filters to a link title.
func (c *Catalog) looksLikeTransfer(title string, mixed bool) bool {
	f := c.TransferFilters
	n := len([]rune(title))
	if n < 8 || n > 300 {
		return false
	}
	if f.announcement != nil && !f.announcement.MatchString(title) {
		return false
	}
	if f.exclude != nil && f.exclude.MatchString(title) {
		return false
	}
	if mixed && f.mixedInclude != nil && !f.mixedInclude.MatchString(title) {
		return false
	}
	return true
}

// canonical drops paging and search parameters so the same notice has one URL.
func (c *Catalog) canonical(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return u
	}
	q := p.Query()
	for k := range q {
		for _, drop := range c.TransferFilters.DropQueryKeys {
			if strings.EqualFold(k, drop) {
				q.Del(k)
			}
		}
	}
	p.RawQuery, p.Fragment = q.Encode(), ""
	p.Scheme, p.Host = strings.ToLower(p.Scheme), strings.ToLower(p.Host)
	if p.Path == "" {
		p.Path = "/"
	}
	return p.String()
}

func (c *Catalog) parseLinks(doc *goquery.Document, s Source) ([]Item, []string) {
	l := s.List
	var items []Item
	var warnings []string
	seen := map[string]bool{}
	links := doc.Find(l.LinkSelector)
	links.Each(func(_ int, link *goquery.Selection) {
		valueNode := link
		if l.URLValueSelector != "" {
			valueNode = link.Find(l.URLValueSelector).First()
			if valueNode.Length() == 0 {
				return
			}
		}
		attr := l.URLValueAttribute
		if attr == "" {
			attr = "href"
		}
		raw, _ := valueNode.Attr(attr)
		raw = strings.TrimSpace(raw)
		if valueRE := re(l.URLValueRegex); valueRE != nil {
			m := valueRE.FindStringSubmatch(raw)
			if len(m) < 2 {
				return
			}
			raw = m[1]
		}
		if l.DetailURLTemplate != "" && raw != "" {
			raw = strings.ReplaceAll(l.DetailURLTemplate, "{value}", url.PathEscape(raw))
		}
		lower := strings.ToLower(raw)
		if raw == "" || strings.HasPrefix(lower, "#") || strings.HasPrefix(lower, "javascript:") || strings.HasPrefix(lower, "mailto:") || strings.HasPrefix(lower, "tel:") {
			return
		}
		if hrefRE := re(l.HrefRegex); hrefRE != nil && !hrefRE.MatchString(raw) {
			return
		}
		titleNode := link
		if l.TitleSelector != "" {
			if t := link.Find(l.TitleSelector).First(); t.Length() > 0 {
				titleNode = t
			}
		}
		title := inline(titleNode.Text())
		if l.TitleAttribute != "" {
			title, _ = titleNode.Attr(l.TitleAttribute)
			title = inline(title)
		}
		if tr := c.TransferFilters.titleTrailing; tr != nil {
			title = strings.TrimSpace(tr.ReplaceAllString(title, ""))
		}
		if s.Kind == KindTransfer && !c.looksLikeTransfer(title, s.MixedBoard) {
			return
		}
		u, ok := resolve(s.ListURL, raw)
		if !ok {
			return
		}
		u = c.canonical(u)
		pu, _ := url.Parse(u)
		if !hostAllowed(pu.Hostname(), s.AllowedHosts) {
			return
		}
		sum := sha256.Sum256([]byte(u))
		id := hex.EncodeToString(sum[:])[:24]
		if seen[id] {
			return
		}
		seen[id] = true
		it := Item{SourceID: s.ID, ExternalID: id, Title: title, URL: u, Fingerprint: fingerprint(title, u)}
		// the date is usually in the same row: look a few levels up
		for n, node := 0, link; n < 4 && node.Length() > 0 && it.Published == ""; n, node = n+1, node.Parent() {
			it.Published = normDate(inline(node.Text()))
		}
		items = append(items, it)
	})
	if links.Length() == 0 {
		warnings = append(warnings, "link_selector_found_nothing")
	} else if len(items) == 0 {
		warnings = append(warnings, "no_matching_notices")
	}
	return items, warnings
}

// Detail is what a notice page says.
type Detail struct {
	Title     string
	Body      string
	Published string
}

// ParseDetail reads a notice page: the source's selectors first, else the catalog's fallback
// containers with navigation, scripts and the like dropped.
func (c *Catalog) ParseDetail(html string, s Source) Detail {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return Detail{}
	}
	first := func(sels []string) *goquery.Selection {
		for _, sel := range sels {
			if n := doc.Find(sel).First(); n.Length() > 0 {
				return n
			}
		}
		return nil
	}
	var d Detail
	if n := first(s.Detail.TitleSelectors); n != nil {
		d.Title = inline(nodeText(n))
	}
	if n := first(s.Detail.DateSelectors); n != nil {
		d.Published = normDate(nodeText(n))
	}
	body := first(s.Detail.BodySelectors)
	if body == nil {
		for _, sel := range c.DetailFallback.Drop {
			doc.Find(sel).Remove()
		}
		body = first(c.DetailFallback.Containers)
	}
	if body != nil {
		d.Body = blockText(body)
	}
	return d
}

func nodeText(n *goquery.Selection) string {
	if goquery.NodeName(n) == "meta" {
		v, _ := n.Attr("content")
		return v
	}
	return n.Text()
}

// blockText is a node's text with one line per block, blank-line runs and repeats removed.
func blockText(n *goquery.Selection) string {
	n = n.Clone()
	n.Find("br, p, div, li, tr, h1, h2, h3, h4, h5, h6").Each(func(_ int, s *goquery.Selection) {
		s.AppendHtml("\n")
	})
	var out []string
	prev := ""
	for _, line := range strings.Split(n.Text(), "\n") {
		line = strings.TrimSpace(strings.Join(strings.Fields(line), " "))
		if line == "" || line == prev {
			continue
		}
		out = append(out, line)
		prev = line
	}
	return strings.Join(out, "\n")
}
