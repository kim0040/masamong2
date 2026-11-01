package config

import (
	"masamong/internal/tools"
	"testing"
)

func TestEndpointURLPolicy(t *testing.T) {
	for _, tc := range []struct {
		url     string
		allowed bool
	}{
		{"https://provider.example/v1", true}, {"http://localhost:9000/v1", true}, {"http://127.0.0.2:9000", true}, {"http://127.255.1.1", true}, {"http://[::1]:9000", true},
		{"http://localhost.attacker.example", false}, {"http://127.0.0.1.attacker.example", false}, {"http://127.0.0.10@attacker.example", false}, {"https://user:password@provider.example", false}, {"http://provider.example", false}, {"ftp://localhost", false}, {"file:///tmp/key", false}, {"https:provider.example", false}, {"//provider.example", false}, {"https://", false},
	} {
		t.Run(tc.url, func(t *testing.T) {
			d := Default("url-policy", t.TempDir())
			d.Providers["test"] = Provider{Kind: KindOpenAI, BaseURL: tc.url}
			raw, err := Marshal(d)
			if err != nil {
				t.Fatal(err)
			}
			_, err = Parse(raw)
			if (err == nil) != tc.allowed {
				t.Fatalf("provider: allowed=%v err=%v", tc.allowed, err)
			}
			d.Providers = map[string]Provider{}
			d.Models = map[string]Model{}
			d.Tools = map[string]tools.Settings{"custom.echo": {Description: "Fetch a synthetic response", HTTP: &tools.HTTP{URL: tc.url}}}
			raw, err = Marshal(d)
			if err != nil {
				t.Fatal(err)
			}
			_, err = Parse(raw)
			if (err == nil) != tc.allowed {
				t.Fatalf("custom tool: allowed=%v err=%v", tc.allowed, err)
			}
		})
	}
}
