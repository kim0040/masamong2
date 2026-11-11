package config

import (
	"fmt"
	"testing"
)

func intp(n int) *int { return &n }

// parsed runs a document through Marshal and Parse, the way a saved config is read back.
func parsed(t *testing.T, d Document) error {
	t.Helper()
	b, err := Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Parse(b)
	return err
}

func TestSearchDepthSettingsValidation(t *testing.T) {
	cases := []struct {
		name string
		set  func(*Document)
		ok   bool
	}{
		{"defaults", func(*Document) {}, true},
		{"read pages 0", func(d *Document) { d.WebSearch.ReadPages = intp(0) }, true},
		{"read pages 10", func(d *Document) { d.WebSearch.ReadPages = intp(10) }, true},
		{"read pages 11", func(d *Document) { d.WebSearch.ReadPages = intp(11) }, false},
		{"read pages -1", func(d *Document) { d.WebSearch.ReadPages = intp(-1) }, false},
		{"deep pages 10", func(d *Document) { d.WebSearch.DeepPages = intp(10) }, true},
		{"deep pages 11", func(d *Document) { d.WebSearch.DeepPages = intp(11) }, false},
		{"deep pages -1", func(d *Document) { d.WebSearch.DeepPages = intp(-1) }, false},
		{"fetch tinyfish", func(d *Document) { d.WebSearch.Fetch = "tinyfish" }, true},
		{"fetch jina", func(d *Document) { d.WebSearch.Fetch = "jina" }, true},
		{"fetch off", func(d *Document) { d.WebSearch.Fetch = "off" }, true},
		{"fetch other", func(d *Document) { d.WebSearch.Fetch = "browser" }, false},
		{"daily limit 5000", func(d *Document) { d.WebSearch.FetchDailyLimit = 5000 }, true},
		{"daily limit 5001", func(d *Document) { d.WebSearch.FetchDailyLimit = 5001 }, false},
		{"daily limit -1", func(d *Document) { d.WebSearch.FetchDailyLimit = -1 }, false},
		{"progress slow", func(d *Document) { d.Chat.Progress = "slow" }, true},
		{"progress deep", func(d *Document) { d.Chat.Progress = "deep" }, true},
		{"progress off", func(d *Document) { d.Chat.Progress = "off" }, true},
		{"progress other", func(d *Document) { d.Chat.Progress = "always" }, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := Default("a", "/tmp/x")
			c.set(&d)
			if err := parsed(t, d); (err == nil) != c.ok {
				t.Fatalf("accepted = %v, want %v (err %v)", err == nil, c.ok, err)
			}
		})
	}
}

func TestSearchDepthSchema(t *testing.T) {
	find := func(section, key string) *SchemaNode {
		for _, f := range Schema().Fields {
			if f.Key != section {
				continue
			}
			for _, g := range f.Node.Fields {
				if g.Key == key {
					return g.Node
				}
			}
		}
		return nil
	}
	if n := find("chat", "progress"); n == nil || len(n.Enum) != len(ProgressModes) || n.Implied != "slow" {
		t.Fatalf("chat.progress schema: %+v", n)
	}
	if n := find("web_search", "fetch"); n == nil || len(n.Enum) != len(FetchKinds) {
		t.Fatalf("web_search.fetch schema: %+v", n)
	}
	for key, want := range map[string]any{"read_pages": 3, "deep_pages": 6, "fetch_daily_limit": 800} {
		n := find("web_search", key)
		if n == nil || fmt.Sprint(n.Implied) != fmt.Sprint(want) || n.Min == nil || n.Max == nil {
			t.Fatalf("web_search.%s schema: %+v", key, n)
		}
	}
}
