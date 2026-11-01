package i18n

import (
	"strings"
	"testing"
	"testing/fstest"
)

func TestEmbeddedCatalogsLoad(t *testing.T) {
	c := New()
	if c.T("en", "setup.complete") != "Setup complete." {
		t.Fatalf("en missing: %q", c.T("en", "setup.complete"))
	}
	if c.T("ko", "setup.complete") != "초기 설정을 마쳤습니다." {
		t.Fatalf("ko missing: %q", c.T("ko", "setup.complete"))
	}
}

func TestOverrideDoesNotWipeBase(t *testing.T) {
	c := New()
	if err := c.LoadFS(fstest.MapFS{"en.json": {Data: []byte(`{"doctor.title":"X"}`)}}); err != nil {
		t.Fatal(err)
	}
	if c.T("en", "doctor.title") != "X" {
		t.Fatal("override")
	}
	if c.T("en", "setup.complete") == "An error occurred." {
		t.Fatal("partial file must not wipe embedded keys")
	}
	if c.T("fr", "setup.complete") != c.T("en", "setup.complete") {
		t.Fatal("unsupported locale must English-fallback")
	}
	if c.T("zh-TW", "setup.complete") != c.T("en", "setup.complete") {
		t.Fatal("zh-Hant must fall back to English, never zh-Hans")
	}
}

func TestPlaceholders(t *testing.T) {
	c := New()
	got := c.T("en", "provider.unconfigured", "provider", "kma")
	if got != "No credential is configured for kma. Add one in Settings to enable live queries." {
		t.Fatalf("fill: %q", got)
	}
	// Unresolved placeholders never reach the screen.
	if Fill("a {missing} b") != "a b" {
		t.Fatalf("strip: %q", Fill("a {missing} b"))
	}
	if c.T("en", "no.such.key") != "An error occurred." {
		t.Fatal("missing key must be generic English, not the raw key")
	}
}

func TestAdditionalCatalogKeepsEnglishAndFallsBack(t *testing.T) {
	c := New()
	if got := c.Resolve("fr-CA"); got != "en" {
		t.Fatalf("unloaded locale resolved as %q", got)
	}
	original := c.T("en", "setup.complete")
	if err := c.LoadFS(fstest.MapFS{"fr.json": {Data: []byte(`{"setup.complete":"Terminé.","provider.unconfigured":"Aucun identifiant pour {provider}."}`)}}); err != nil {
		t.Fatal(err)
	}
	if got := c.T("en", "setup.complete"); got != original {
		t.Fatalf("English overwritten: %q", got)
	}
	if got := c.T("fr-CA", "setup.complete"); got != "Terminé." {
		t.Fatalf("region lookup: %q", got)
	}
	if got := c.T("fr", "provider.unconfigured", "provider", "kma"); got != "Aucun identifiant pour kma." {
		t.Fatalf("placeholder: %q", got)
	}
	if got := c.T("fr", "doctor.title"); got != c.T("en", "doctor.title") {
		t.Fatalf("missing key did not fall back: %q", got)
	}
	if c.Resolve("fr-CA") != "fr" || c.Resolve("fr-u-nu-latn") != "fr" || c.Resolve("fr-u") != "en" || c.Resolve("de-DE") != "en" || c.Resolve("zh-TW") != "en" {
		t.Fatal("Resolve must select only loaded locales")
	}
	if err := c.LoadFS(fstest.MapFS{"fr-CA.json": {Data: []byte(`{"setup.complete":"Prêt."}`)}}); err != nil {
		t.Fatal(err)
	}
	if c.T("fr", "setup.complete") != "Prêt." || c.T("en", "setup.complete") != original {
		t.Fatal("a separate operator load must override only the matching locale")
	}
}

func TestCatalogTagContractAndFilenameValidation(t *testing.T) {
	for input, want := range map[string]string{
		"en_US": "en", "ko-KR": "ko", "ja-JP": "ja", "es-MX": "es", "fr-CA": "fr",
		"zh": "zh-Hans", "zh-CN": "zh-Hans", "zh-SG": "zh-Hans", "zh-Hans-CN": "zh-Hans",
		"zh-TW": "zh-Hant", "zh-Hant": "zh-Hant", "fr--CA": "",
		"fr-u-nu-latn": "fr", "fr-CA-u-nu-latn": "fr", "fr-x-private": "fr",
		"zh-Hant-u-nu-hanidec": "zh-Hant", "zh-u-nu-latn": "zh-Hans",
		"fr-u": "", "fr-u-n": "", "fr-u-nu-": "", "fr-u-nu-u-ca": "",
		"fr-x": "", "fr-u-nu-@": "", "zh-Hant-u": "",
	} {
		if got := Normalize(input); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", input, got, want)
		}
	}
	c := New()
	if err := c.LoadFS(fstest.MapFS{"zh-Hans.json": {Data: []byte(`{"setup.complete":"完成。"}`)}}); err != nil {
		t.Fatal(err)
	}
	if c.T("zh-CN", "setup.complete") != "完成。" || c.T("zh-Hans", "setup.complete") != "完成。" || c.T("zh-u-nu-latn", "setup.complete") != "完成。" || c.T("zh-Hant-u-nu-hanidec", "setup.complete") != c.T("en", "setup.complete") {
		t.Fatal("Chinese aliases or traditional fallback")
	}
	for _, fsys := range []fstest.MapFS{
		{"fr.json": {Data: []byte(`{}`)}, "fr-CA.json": {Data: []byte(`{}`)}},
		{"zh.json": {Data: []byte(`{}`)}, "zh-Hans.json": {Data: []byte(`{}`)}},
		{"fr--CA.json": {Data: []byte(`{}`)}},
		{"fr-u.json": {Data: []byte(`{}`)}},
		{".json": {Data: []byte(`{}`)}},
	} {
		if err := c.LoadFS(fsys); err == nil {
			t.Fatalf("accepted invalid or duplicate filenames: %v", fsys)
		}
	}
}

func TestCatalogRejectsPlaceholderChangesAtomically(t *testing.T) {
	c := New()
	original := c.T("en", "provider.unconfigured", "provider", "kma")
	for _, translated := range []string{"Bonjour {other}", "Bonjour {provider} {provider}", "Bonjour"} {
		fsys := fstest.MapFS{"fr.json": {Data: []byte(`{"setup.complete":"Terminé.","provider.unconfigured":` + `"` + translated + `"}`)}}
		if err := c.LoadFS(fsys); err == nil || !strings.Contains(err.Error(), "provider.unconfigured") {
			t.Fatalf("accepted placeholder mismatch %q: %v", translated, err)
		}
		if c.Resolve("fr") != "en" || c.T("en", "provider.unconfigured", "provider", "kma") != original {
			t.Fatal("failed load changed catalog")
		}
	}
	if SamePlaceholders(`{name} {name} {x}`, `{x} {name} {name}`) == false || SamePlaceholders(`{name} {name}`, `{name}`) {
		t.Fatal("placeholder multiplicity")
	}
	if !SamePlaceholders(`{"key":{...}} {name}`, `{"other":{...}} {name}`) {
		t.Fatal("schema braces are not placeholders")
	}
}

func TestKoreanCoversEnglish(t *testing.T) {
	c := New()
	if missing := c.MissingKeys("ko"); len(missing) != 0 {
		t.Fatalf("ko catalog missing keys: %v", missing)
	}
}
