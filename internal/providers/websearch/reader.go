package websearch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"masamong/internal/providers"
	"masamong/internal/providers/decode"
)

// Reader turns a web page into readable text through Jina Reader (GET r.jina.ai/<url>): menus, ads
// and footers removed, the article as Markdown. It works without a key (20 requests a minute); a key
// raises the limit. The page is fetched by Jina, never by this process.
type Reader struct {
	c    *providers.Client
	base string
	cred providers.Credential
}

// ReaderBaseURL is Jina Reader's address.
const ReaderBaseURL = "https://r.jina.ai"

// NewReader builds the reader; cred may be empty.
func NewReader(client *providers.Client, baseURL string, cred providers.Credential) *Reader {
	if baseURL == "" {
		baseURL = ReaderBaseURL
	}
	return &Reader{c: client, base: strings.TrimRight(baseURL, "/"), cred: cred}
}

// Page is one read page.
type Page struct {
	Title     string
	URL       string
	Content   string
	Truncated bool
	Tokens    int
}

// ErrNotPublic refuses addresses that are not public web pages (loopback, private networks, internal
// names, embedded credentials): they are never handed to an outside service.
var ErrNotPublic = errors.New("not a public web address")

// PublicURL checks and normalizes an address the bot was asked to read.
func PublicURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return "", ErrNotPublic
	}
	host := strings.ToLower(u.Hostname())
	if p := u.Port(); p != "" && p != "80" && p != "443" {
		return "", ErrNotPublic
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
			return "", ErrNotPublic
		}
	} else if !strings.Contains(host, ".") || host == "localhost" || strings.HasSuffix(host, ".localhost") ||
		strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") || strings.HasSuffix(host, ".lan") || strings.HasSuffix(host, ".home.arpa") {
		return "", ErrNotPublic
	}
	u.Fragment = ""
	return u.String(), nil
}

// Read fetches pageURL as text, cut to maxChars runes.
func (r *Reader) Read(ctx context.Context, pageURL string, maxChars int) (Page, error) {
	target, err := PublicURL(pageURL)
	if err != nil {
		return Page{}, err
	}
	h := map[string]string{"Accept": "application/json", "X-Timeout": "15", "X-Return-Format": "markdown"}
	if r.cred.Present() {
		h["Authorization"] = "Bearer " + r.cred.Value()
	}
	resp, err := r.c.Get(ctx, r.base+"/"+target, h)
	if err != nil {
		var pe *providers.Error
		if errors.As(err, &pe) && (pe.Status == 401 || pe.Status == 403) && r.cred.Present() {
			return Page{}, fmt.Errorf("%w: %s", ErrAuth, serviceMessage(resp))
		}
		if msg := serviceMessage(resp); msg != "" {
			return Page{}, fmt.Errorf("%w: %s", err, msg)
		}
		return Page{}, err
	}
	return ParseReader(resp.ContentType, resp.Body, maxChars)
}

// ParseReader reads {"code":200,"data":{"title","url","content","usage":{"tokens"}}}.
func ParseReader(contentType string, body []byte, maxChars int) (Page, error) {
	obj, err := decode.Object(contentType, body)
	if err != nil {
		return Page{}, err
	}
	if code, ok := decode.Number(obj["code"]); ok && code != 200 {
		return Page{}, fmt.Errorf("reader: %s", clip(decode.Str(obj["readableMessage"])+" "+decode.Str(obj["message"])))
	}
	var d struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Content string `json:"content"`
		Usage   struct {
			Tokens int `json:"tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(obj["data"], &d); err != nil || strings.TrimSpace(d.Content) == "" {
		return Page{}, fmt.Errorf("%w: reader data", decode.ErrUnexpected)
	}
	if maxChars <= 0 {
		maxChars = 6000
	}
	p := Page{Title: d.Title, URL: d.URL, Content: strings.TrimSpace(d.Content), Tokens: d.Usage.Tokens}
	if r := []rune(p.Content); len(r) > maxChars {
		p.Content, p.Truncated = string(r[:maxChars])+"…", true
	}
	return p, nil
}

// String is a short description for logs (never the content).
func (p Page) String() string {
	return p.URL + " (" + strconv.Itoa(len([]rune(p.Content))) + " chars)"
}
