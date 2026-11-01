package config

import (
	"slices"
	"testing"
)

// models.vision is a product role (the test sandbox has none): config accepts it with a configured
// provider and refuses one that is not.
func TestVisionIsAProductModelRole(t *testing.T) {
	if !ValidModelRole("vision") || !slices.Contains(AllModelRoles(), "vision") {
		t.Fatal("vision is a model role")
	}
	if slices.Contains(ModelRoles, "vision") {
		t.Fatal("vision is a product-only role, not part of the sandbox contract")
	}
	doc := Default("test", t.TempDir())
	doc.Providers["lens"] = Provider{Kind: KindOpenAI, BaseURL: "https://example.com/v1"}
	doc.Models["vision"] = Model{Provider: "lens", Model: "vendor/eyes-1"}
	raw, err := Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(raw)
	if err != nil || got.Models["vision"].Model != "vendor/eyes-1" || got.Models["vision"].Provider != "lens" {
		t.Fatalf("models.vision: %v %+v", err, got.Models["vision"])
	}
	doc.Models["vision"] = Model{Provider: "nowhere", Model: "x"}
	raw, err = Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(raw); err == nil {
		t.Fatal("models.vision on an unknown provider must be refused")
	}
}
