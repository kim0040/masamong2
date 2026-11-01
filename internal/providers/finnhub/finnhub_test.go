package finnhub

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"masamong/internal/providers"
)

func TestParseQuote(t *testing.T) {
	q, err := ParseQuote("AAPL", "application/json", []byte(`{"c":227.5,"d":1.2,"dp":0.53,"h":229,"l":225.1,"o":226,"pc":226.3,"t":1757894400}`))
	if err != nil || q.Current != 227.5 || q.At.IsZero() {
		t.Fatalf("%v %+v", err, q)
	}
	if _, err := ParseQuote("NOPE", "application/json", []byte(`{"c":0,"d":null,"dp":null,"h":0,"l":0,"o":0,"pc":0,"t":0}`)); !errors.Is(err, ErrUnknownSymbol) {
		t.Fatalf("all-zero must be unknown symbol: %v", err)
	}
	if _, err := ParseQuote("X", "application/json", []byte(`{"error":"You don't have access to this resource."}`)); err == nil {
		t.Fatal("error field")
	}
}

func TestQuoteStatusMapping(t *testing.T) {
	status := 429
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Finnhub-Token") != "tok" {
			t.Errorf("token header missing")
		}
		w.WriteHeader(status)
	}))
	defer srv.Close()
	t.Setenv("FH", "tok")
	cred, _ := providers.Resolve("env:FH")
	c := providers.NewClient(time.Second, nil)
	c.MaxRetries = 0
	a := New(c, srv.URL, cred)
	if _, err := a.Quote(context.Background(), "aapl"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("429: %v", err)
	}
	status = 403
	if _, err := a.Quote(context.Background(), "AAPL"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("403: %v", err)
	}
	a.Cred = providers.Credential{}
	if _, err := a.Quote(context.Background(), "AAPL"); !errors.Is(err, providers.ErrNoCredential) {
		t.Fatal("no credential must not call")
	}
}
