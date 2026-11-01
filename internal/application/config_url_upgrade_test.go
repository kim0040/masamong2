package application

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"masamong/internal/action"
	"masamong/internal/config"
)

// committedWithProviderURL stores a committed revision holding a provider address as an earlier
// release accepted it (it only checked the https:// prefix), closes the service and returns its root.
func committedWithProviderURL(t *testing.T, baseURL string) string {
	t.Helper()
	s := privacyService(t)
	root := s.DataRoot
	var content string
	if err := s.Store.DB.QueryRow(`SELECT content FROM config_revisions WHERE status='committed' ORDER BY revision DESC LIMIT 1`).Scan(&content); err != nil {
		t.Fatal(err)
	}
	doc, err := config.Parse([]byte(content))
	if err != nil {
		t.Fatal(err)
	}
	doc.Providers["proxy"] = config.Provider{Kind: config.KindOpenAI, BaseURL: baseURL}
	raw, _ := config.Marshal(doc)
	rev := config.Revision{Revision: 2, Status: config.StatusCommitted, ContentHash: config.Hash(raw), Content: raw, BaseRevision: 1}
	if err := persistRevision(s.Store, rev); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return root
}

// An address the stricter policy refuses must not stop a daemon that ran with it before the upgrade:
// the provider is switched off, the rest of the bot runs, and doctor says what to change.
func TestUpgradeKeepsRunningWithAnAddressThePolicyNowRefuses(t *testing.T) {
	for _, tc := range []struct{ url, reason string }{
		{"https://user:secret@gateway.example/v1", "user name or password"},
		{"http://localhost.internal/v1", "only looks like"},
		{"http://127.0.0.1.nip.io/v1", "only looks like"},
		{"http://localhost./v1", "only looks like"},
		{"http://gateway.example/v1", "use https"},
	} {
		t.Run(tc.url, func(t *testing.T) {
			root := committedWithProviderURL(t, tc.url)
			s, err := Open(root, nil, false)
			if err != nil {
				t.Fatalf("an upgrade must not stop the daemon over a committed address: %v", err)
			}
			defer s.Close()
			if !s.Ready() || s.Revision() != 2 {
				t.Fatalf("the committed revision is not live: ready=%v rev=%d", s.Ready(), s.Revision())
			}
			if p := s.Doc().Providers["proxy"]; !p.Disabled {
				t.Fatal("the provider with the refused address must be switched off")
			}
			if st, ok := s.Registry().Status("proxy"); !ok || !st.Disabled {
				t.Fatalf("the registry still offers the provider: %+v", st)
			}
			doctor := s.Execute(context.Background(), action.Request{Action: "runtime.doctor"})
			raw, _ := json.Marshal(doctor.Output)
			if !strings.Contains(string(raw), "providers.proxy.base_url") || !strings.Contains(string(raw), tc.reason) {
				t.Fatalf("doctor does not name the setting and the real reason (%q): %s", tc.reason, raw)
			}
			if strings.Contains(string(raw), "secret") {
				t.Fatalf("doctor leaks the credential from the address: %s", raw)
			}
		})
	}
}

// New changes are validated strictly, and the message names the rule that failed.
func TestNewConfigChangesStillRefuseThoseAddresses(t *testing.T) {
	s := privacyService(t)
	for _, tc := range []struct{ url, reason string }{
		{"https://user:secret@gateway.example/v1", "user name or password"},
		{"http://127.0.0.1.nip.io/v1", "only looks like"},
	} {
		doc := s.Doc()
		doc.Providers = map[string]config.Provider{"p2": {Kind: config.KindOpenAI, BaseURL: tc.url}}
		raw, _ := config.Marshal(doc)
		r := s.Execute(context.Background(), action.Request{Action: "config.apply", Input: mustJSON(map[string]any{"yaml": string(raw), "base_revision": s.Revision()})})
		if r.OK || !strings.Contains(r.Error.Message, tc.reason) || strings.Contains(r.Error.Message, "must use https") {
			t.Fatalf("%s: %+v", tc.url, r.Error)
		}
	}
}

// A provider that is already switched off sends nothing, so an unrelated change can be applied while
// its address is still wrong; turning it on again fails with the reason.
func TestDisabledProviderWithRefusedAddressDoesNotBlockOtherChanges(t *testing.T) {
	root := committedWithProviderURL(t, "https://user:secret@gateway.example/v1")
	s, err := Open(root, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	doc := s.Doc() // the provider is switched off in memory
	raw, _ := config.Marshal(doc)
	apply := func(yaml []byte) action.Result {
		return s.Execute(context.Background(), action.Request{Action: "config.apply", Input: mustJSON(map[string]any{"yaml": string(yaml), "base_revision": s.Revision()})})
	}
	if r := apply(raw); !r.OK {
		t.Fatalf("an unrelated change must apply: %+v", r.Error)
	}
	doc = s.Doc()
	p := doc.Providers["proxy"]
	p.Disabled = false
	doc.Providers["proxy"] = p
	raw, _ = config.Marshal(doc)
	if r := apply(raw); r.OK || !strings.Contains(r.Error.Message, "user name or password") {
		t.Fatalf("enabling it again must fail with the reason: %+v", r)
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
