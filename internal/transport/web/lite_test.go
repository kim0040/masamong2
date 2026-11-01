//go:build !noweb

package web

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func liteGet(t *testing.T, c *http.Client, u string, lang string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", u, nil)
	if lang != "" {
		req.Header.Set("Accept-Language", lang)
	}
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

// The emergency page follows the console's language (not only Korean and English), signs in with a
// POST so the token never reaches the address bar or a log, and says where the token comes from.
func TestLiteSpeaksEveryConsoleLanguageAndSignsInByPost(t *testing.T) {
	s, ts := testServer(t)
	c := client()
	for lang, want := range map[string]string{"ja": "緊急管理", "zh-Hans": "应急管理", "es": "Consola de emergencia", "ko": "비상 관리", "en": "emergency console"} {
		code, page := liteGet(t, c, ts.URL+"/lite", lang)
		if code != 200 || !strings.Contains(page, want) || !strings.Contains(page, `<html lang="`+lang+`"`) {
			t.Errorf("%s: %d, want %q and lang in %.200s", lang, code, want, page)
		}
	}
	_, page := liteGet(t, c, ts.URL+"/lite", "en")
	if !strings.Contains(page, `method="post" action="/lite"`) || strings.Contains(page, `method="get"`) {
		t.Fatalf("the token form must POST: %s", page)
	}
	if !strings.Contains(page, "web-token") || !strings.Contains(page, "admin token") {
		t.Fatalf("the page says where the token is: %s", page)
	}

	post := func(token string) *http.Response {
		req, _ := http.NewRequest("POST", ts.URL+"/lite", strings.NewReader(url.Values{"token": {token}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		res, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	bad := post("nope")
	b, _ := io.ReadAll(bad.Body)
	bad.Body.Close()
	if bad.StatusCode != http.StatusUnauthorized || !strings.Contains(string(b), "does not match") {
		t.Fatalf("wrong token: %d %s", bad.StatusCode, b)
	}
	good := post(s.token)
	good.Body.Close()
	if good.StatusCode != http.StatusSeeOther || good.Header.Get("Location") != "/lite" {
		t.Fatalf("right token: %d %v", good.StatusCode, good.Header)
	}
	if _, page := liteGet(t, c, ts.URL+"/lite", "en"); !strings.Contains(page, `name="csrf"`) {
		t.Fatalf("signed in: the controls show: %s", page)
	}
}
