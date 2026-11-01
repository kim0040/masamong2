package fx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"masamong/internal/providers"
)

const keyed = `{"result":"success","documentation":"x","base_code":"USD","conversion_rates":{"KRW":1387.12,"JPY":"147.3","EUR":0.92,"BAD":"n/a","ZERO":0},"time_last_update_unix":1757894401,"time_next_update_unix":1757980801}`
const open = `{"result":"success","provider":"https://www.exchangerate-api.com","base_code":"USD","rates":{"KRW":1387.1,"EUR":0.92}}`
const errBody = `{"result":"error","error-type":"invalid-key"}`

func TestParseBothFieldNames(t *testing.T) {
	k, err := Parse("application/json", []byte(keyed))
	if err != nil || k.Base != "USD" || k.Rates["KRW"] != 1387.12 || k.Rates["JPY"] != 147.3 {
		t.Fatalf("keyed: %v %+v", err, k)
	}
	if _, ok := k.Rates["BAD"]; ok {
		t.Fatal("non-numeric dropped")
	}
	if _, ok := k.Rates["ZERO"]; ok {
		t.Fatal("non-positive dropped")
	}
	if k.NextUpdate.IsZero() {
		t.Fatal("next update parsed")
	}
	o, err := Parse("application/json", []byte(open))
	if err != nil || o.Rates["EUR"] != 0.92 {
		t.Fatalf("open: %v %+v", err, o)
	}
	if _, err := Parse("application/json", []byte(errBody)); !errors.Is(err, ErrProvider) {
		t.Fatalf("error envelope: %v", err)
	}
	if _, err := Parse("text/html", []byte("<html>oops</html>")); err == nil {
		t.Fatal("html rejected")
	}
}

func TestLatestPicksEndpointByCredential(t *testing.T) {
	var path, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.URL.Path, r.Header.Get("Authorization")
		if auth != "" {
			_, _ = w.Write([]byte(keyed))
		} else {
			_, _ = w.Write([]byte(open))
		}
	}))
	defer srv.Close()
	a := New(providers.NewClient(time.Second, nil), providers.Credential{})
	a.OpenBase, a.KeyedBase = srv.URL+"/open", srv.URL+"/keyed"
	r, err := a.Latest(context.Background(), "usd")
	if err != nil || r.Source != "open" || path != "/open/latest/USD" || auth != "" {
		t.Fatalf("open: %v %s %s", err, path, auth)
	}
	t.Setenv("FX_KEY", "k1")
	cred, _ := providers.Resolve("env:FX_KEY")
	a.Cred = cred
	r, err = a.Latest(context.Background(), "USD")
	if err != nil || r.Source != "keyed" || path != "/keyed/latest/USD" || auth != "Bearer k1" {
		t.Fatalf("keyed: %v %s %s", err, path, auth)
	}
	if _, err := a.Latest(context.Background(), "usdollar"); err == nil {
		t.Fatal("bad base code")
	}
}

func TestLatestSurfacesProviderErrorOn4xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte(errBody))
	}))
	defer srv.Close()
	a := New(providers.NewClient(time.Second, nil), providers.Credential{})
	a.OpenBase = srv.URL
	if _, err := a.Latest(context.Background(), "USD"); !errors.Is(err, ErrProvider) {
		t.Fatalf("want provider error-type, got %v", err)
	}
}
