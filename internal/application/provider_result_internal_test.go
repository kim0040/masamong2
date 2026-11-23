package application

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/errcode"
	"masamong/internal/i18n"
	"masamong/internal/providers"
	"masamong/internal/providers/openai"
	"masamong/internal/providers/websearch"
)

// The console action for a provider failure shows the error's class and HTTP status, never the
// service's own words or the address it was sent to: either can repeat the key back.
func TestProviderResultShowsClassAndStatusOnly(t *testing.T) {
	t.Parallel()
	const serviceWords = "service-says the key was refused"
	refuse := func(status int) (*httptest.Server, error) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"message":"` + serviceWords + `"}`))
		}))
		c := providers.NewClient(5*time.Second, nil)
		c.MaxRetries = 0
		var creds credentialStandIns
		creds.setForTest(t, "PR_KEY", "synthetic-provider-key-0123456789")
		cred, _ := creds.resolve("env:PR_KEY")
		searcher, err := websearch.New("tavily", c, srv.URL, cred)
		if err != nil {
			srv.Close()
			t.Fatal(err)
		}
		_, aerr := searcher.Search(context.Background(), websearch.Query{Text: "q", Max: 1})
		return srv, aerr
	}
	s := &Service{I18n: i18n.New()}
	assert := func(name string, err error, srv *httptest.Server, code string, status int) {
		t.Helper()
		defer srv.Close()
		res := s.providerResult(action.Request{Locale: "en"}, "tavily", err)
		if res.OK || res.Error.Code != code || res.Error.Args["status"] != status {
			t.Fatalf("%s: the error keeps its class and HTTP status, got %+v", name, res.Error)
		}
		raw, _ := json.Marshal(res.Error)
		for _, leak := range []string{serviceWords, srv.URL, "synthetic-provider-key", `"url"`, `"detail"`} {
			if strings.Contains(string(raw), leak) {
				t.Fatalf("%s: the console action carries %q: %s", name, leak, raw)
			}
		}
	}
	srv, err := refuse(401)
	assert("refused key", err, srv, errcode.ProviderRejected, 401)
	srv, err = refuse(500)
	assert("server error", err, srv, errcode.ProviderUnavailable, 500)

	// a typed service error keeps its HTTP status and shows neither its type nor its code: both are
	// the service's own words
	res := s.providerResult(action.Request{Locale: "en"}, "openai", &openai.APIError{Status: 503, Type: "service-type", Code: "service-code", Message: serviceWords})
	if res.OK || res.Error.Code != errcode.ProviderRejected || res.Error.Args["status"] != 503 {
		t.Fatalf("typed error keeps class and status: %+v", res.Error)
	}
	raw, _ := json.Marshal(res.Error)
	for _, leak := range []string{serviceWords, "service-type", "service-code"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("the console action carries %q: %s", leak, raw)
		}
	}
}
