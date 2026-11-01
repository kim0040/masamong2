package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDownloadClientFollowsOnlyAllowedRedirectsWithoutCredentials(t *testing.T) {
	var seen http.Header
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		_, _ = w.Write([]byte("file"))
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/f", http.StatusFound)
	}))
	defer origin.Close()
	headers := map[string]string{"Authorization": "Bearer x", "X-Api-Key": "k", "Accept": "application/octet-stream"}

	// the strict provider client never follows it
	strict := NewClient(5*time.Second, nil)
	strict.MaxRetries = 0
	if _, err := strict.Get(context.Background(), origin.URL, headers); err == nil || !strings.Contains(err.Error(), "redirect refused") {
		t.Fatalf("the provider client must stay strict: %v", err)
	}
	// a download client follows it only to a host the caller allows
	deny := NewDownloadClient(5*time.Second, nil, func(string) bool { return false })
	deny.MaxRetries = 0
	if _, err := deny.Get(context.Background(), origin.URL, headers); err == nil {
		t.Fatal("a host that is not allowed must be refused")
	}
	dl := NewDownloadClient(5*time.Second, nil, func(h string) bool { return h == "127.0.0.1" })
	dl.MaxRetries = 0
	res, err := dl.Get(context.Background(), origin.URL, headers)
	if err != nil || string(res.Body) != "file" {
		t.Fatalf("%v %v", res, err)
	}
	if seen.Get("Authorization") != "" || seen.Get("X-Api-Key") != "" {
		t.Fatalf("credentials were sent to the other origin: %v", seen)
	}
	if seen.Get("Accept") != "application/octet-stream" {
		t.Fatalf("Accept was dropped: %v", seen)
	}
}
