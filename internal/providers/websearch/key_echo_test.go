package websearch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/providers"
)

// A service that refuses a request often repeats the key it was sent. The error reaches the console and
// the logs, so it must not carry the key, whatever status or body shape the service uses.
const echoedKey = "synthetic-key-0123456789abcdef"

func keyEchoServer(status int, body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func echoClient() *providers.Client {
	c := providers.NewClient(5*time.Second, nil)
	c.MaxRetries = 0
	return c
}

func TestSearchErrorsDoNotCarryTheKey(t *testing.T) {
	bodies := map[string]string{
		"detail text":   `{"detail":"Invalid API key: ` + echoedKey + `"}`,
		"nested error":  `{"detail":{"error":"key ` + echoedKey + ` was refused"}}`,
		"message":       `{"message":"Bearer ` + echoedKey + ` is not allowed"}`,
		"error object":  `{"error":{"message":"bad credentials (` + echoedKey + `)"}}`,
		"query shaped":  `{"error":"GET /search?api_key=` + echoedKey + ` failed"}`,
		"cut by a clip": `{"error":"` + strings.Repeat("x", snippetRunes-10) + echoedKey + `"}`,
	}
	for _, kind := range []string{"tavily", "linkup", "exa", "serper"} {
		for name, body := range bodies {
			for _, status := range []int{400, 401, 403, 422} {
				srv := keyEchoServer(status, body)
				t.Setenv("ECHO_KEY", echoedKey)
				cred, _ := providers.Resolve("env:ECHO_KEY")
				s, err := New(kind, echoClient(), srv.URL, cred)
				if err != nil {
					t.Fatal(err)
				}
				_, err = s.Search(context.Background(), Query{Text: "q", Max: 3})
				srv.Close()
				if err == nil {
					t.Fatalf("%s/%s/%d: no error", kind, name, status)
				}
				if strings.Contains(err.Error(), echoedKey) || strings.Contains(err.Error(), echoedKey[:12]) {
					t.Errorf("%s/%s/%d: the error carries the key: %v", kind, name, status, err)
				}
			}
		}
	}
}

func TestSearchErrorKeepsTheServicesWords(t *testing.T) {
	srv := keyEchoServer(401, `{"detail":"Invalid API key `+echoedKey+`: check the dashboard"}`)
	defer srv.Close()
	t.Setenv("ECHO_KEY", echoedKey)
	cred, _ := providers.Resolve("env:ECHO_KEY")
	s, _ := New("tavily", echoClient(), srv.URL, cred)
	_, err := s.Search(context.Background(), Query{Text: "q", Max: 3})
	if err == nil || !strings.Contains(err.Error(), "check the dashboard") || !strings.Contains(err.Error(), "***") {
		t.Fatalf("the reason stays, the key goes: %v", err)
	}
}

func TestReaderErrorsDoNotCarryTheKey(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
	}{
		"refused key":         {401, `{"message":"Bearer ` + echoedKey + ` rejected"}`},
		"other status":        {422, `{"detail":"bad request from ` + echoedKey + `"}`},
		"ok status, bad code": {200, `{"code":402,"readableMessage":"balance used up for ` + echoedKey + `","message":"x"}`},
	} {
		srv := keyEchoServer(tc.status, tc.body)
		t.Setenv("ECHO_KEY", echoedKey)
		cred, _ := providers.Resolve("env:ECHO_KEY")
		_, err := NewReader(echoClient(), srv.URL, cred).Read(context.Background(), "https://news.example.com/a", 100)
		srv.Close()
		if err == nil {
			t.Fatalf("%s: no error", name)
		}
		if strings.Contains(err.Error(), echoedKey) {
			t.Errorf("%s: the error carries the key: %v", name, err)
		}
	}
}

// A refused key error is ErrAuth and keeps the HTTP status, so the console can say "refused (401)"
// without ever needing the body's text or the address.
func TestAuthErrorKeepsTheStatus(t *testing.T) {
	for _, kind := range []string{"tavily", "brave", "kakao_search"} {
		srv := keyEchoServer(401, `{"message":"service-says the key was refused"}`)
		t.Setenv("ECHO_KEY", echoedKey)
		cred, _ := providers.Resolve("env:ECHO_KEY")
		s, err := New(kind, echoClient(), srv.URL, cred)
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.Search(context.Background(), Query{Text: "q", Max: 3})
		srv.Close()
		var pe *providers.Error
		if !errors.Is(err, ErrAuth) || !errors.As(err, &pe) || pe.Status != 401 {
			t.Fatalf("%s: the refusal stays ErrAuth and keeps status 401, got %v", kind, err)
		}
	}
}

// A key shorter than minScrubbed cannot be cut back out of the service's answer, so it is refused
// before the wire: no request goes out, the error names the refusal and never the key.
func TestShortCredentialNeverLeavesTheProcess(t *testing.T) {
	const shortKey = "abcde" // under minScrubbed
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"message":"the key ` + shortKey + ` is wrong"}`))
	}))
	defer srv.Close()
	t.Setenv("SHORT_KEY", shortKey)
	cred, _ := providers.Resolve("env:SHORT_KEY")
	for _, kind := range []string{"tavily", "brave", "kakao_search"} {
		s, err := New(kind, echoClient(), srv.URL, cred)
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.Search(context.Background(), Query{Text: "q", Max: 3})
		if err == nil || !errors.Is(err, ErrCredentialTooShort) || !errors.Is(err, ErrAuth) || strings.Contains(err.Error(), shortKey) {
			t.Fatalf("%s: want a refusal naming no key, got %v", kind, err)
		}
	}
	if _, err := NewReader(echoClient(), srv.URL, cred).Read(context.Background(), "https://news.example.com/a", 100); err == nil || !errors.Is(err, ErrCredentialTooShort) || strings.Contains(err.Error(), shortKey) {
		t.Fatalf("reader: want a refusal naming no key, got %v", err)
	}
	if _, err := NewTinyFishFetcher(echoClient(), srv.URL, cred).Fetch(context.Background(), []string{"https://news.example.com/a"}, 100); err == nil || !errors.Is(err, ErrCredentialTooShort) || strings.Contains(err.Error(), shortKey) {
		t.Fatalf("tinyfish fetch: want a refusal naming no key, got %v", err)
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Fatalf("the short key was sent %d times", n)
	}
}

// Brave and TinyFish search send a GET with the key in a header; their errors must not carry it either.
func TestBraveAndTinyFishErrorsDoNotCarryTheKey(t *testing.T) {
	for _, kind := range []string{"brave", "tinyfish"} {
		for _, status := range []int{400, 401, 403, 422} {
			srv := keyEchoServer(status, `{"error":{"message":"token `+echoedKey+` refused"},"detail":"key `+echoedKey+`"}`)
			t.Setenv("ECHO_KEY", echoedKey)
			cred, _ := providers.Resolve("env:ECHO_KEY")
			s, err := New(kind, echoClient(), srv.URL, cred)
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.Search(context.Background(), Query{Text: "q", Max: 3})
			srv.Close()
			if err == nil {
				t.Fatalf("%s/%d: no error", kind, status)
			}
			if strings.Contains(err.Error(), echoedKey) {
				t.Errorf("%s/%d: the error carries the key: %v", kind, status, err)
			}
		}
	}
}
