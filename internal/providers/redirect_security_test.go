package providers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRedirectPolicyRejectsDowngradeAndOriginChanges(t *testing.T) {
	c := NewClient(time.Second, nil)
	first, _ := http.NewRequest("GET", "https://Provider.example/path?key=synthetic", nil)
	first.Header.Set("Authorization", "Bearer synthetic")
	for _, tc := range []struct {
		to      string
		allowed bool
	}{
		{"https://provider.example:443/next", true}, {"http://provider.example/next", false}, {"https://provider.example:8443/next", false}, {"https://elsewhere.example/next", false}, {"https://user:synthetic@provider.example/next", false}, {"ftp://provider.example/next", false},
	} {
		r, _ := http.NewRequest("GET", tc.to, nil)
		err := c.HTTP.CheckRedirect(r, []*http.Request{first})
		if (err == nil) != tc.allowed {
			t.Fatalf("%s: %v", tc.to, err)
		}
		if !tc.allowed && !errors.Is(err, ErrCrossHostRedirect) {
			t.Fatal(err)
		}
	}
}

func TestHTTPSRedirectDoesNotSendCredentialsToHTTP(t *testing.T) {
	var leaked atomic.Bool
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil {
			leaked.Store(true)
			return
		}
		http.Redirect(w, r, "http://"+r.Host+"/next", http.StatusFound)
	}))
	defer srv.Close()
	c := NewClient(time.Second, nil)
	c.HTTP.Transport = srv.Client().Transport
	_, err := c.Get(context.Background(), srv.URL, map[string]string{"Authorization": "Bearer synthetic"})
	var pe *Error
	if !errors.As(err, &pe) || pe.Kind != "redirect" || pe.Attempts != 1 || leaked.Load() {
		t.Fatalf("downgrade: %v leak=%v", err, leaked.Load())
	}
	// The policy normalizes default ports and host casing, rather than comparing URL strings.
	u, _ := url.Parse(strings.ToUpper("https") + "://Provider.example:443")
	if redirectOrigin(u) != "https://provider.example:443" {
		t.Fatal(redirectOrigin(u))
	}
}
