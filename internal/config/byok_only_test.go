package config

import (
	"strings"
	"testing"
)

func TestByokOnlyValidation(t *testing.T) {
	for routes, ok := range map[string]bool{"anthropic=anthropic": true, "google=google-vertex": true, "meta-llama=deepinfra/turbo": true,
		"anthropic=Anthropic": false, "=anthropic": false, "anthropic= ": false} {
		d := Default("a", "/tmp/x")
		p := d.Providers["openrouter"]
		family, upstream, _ := strings.Cut(routes, "=")
		p.ByokOnly = map[string]string{family: upstream}
		d.Providers["openrouter"] = p
		b, err := Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Parse(b)
		if (err == nil) != ok {
			t.Fatalf("%q: %v", routes, err)
		}
		if ok && got.Providers["openrouter"].ByokOnly[family] != upstream {
			t.Fatalf("%q did not round-trip: %v", routes, got.Providers["openrouter"].ByokOnly)
		}
	}
	d := Default("a", "/tmp/x")
	p := d.Providers["kakao"]
	p.ByokOnly = map[string]string{"anthropic": "anthropic"}
	d.Providers["kakao"] = p
	b, _ := Marshal(d)
	if _, err := Parse(b); err == nil {
		t.Fatal("byok_only is for model providers only")
	}
}
