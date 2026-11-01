package application_test

import (
	"context"
	"encoding/json"
	"testing"

	"masamong/internal/action"
	"masamong/internal/application"
)

func setupIn(t *testing.T, locale string, in map[string]any) *application.Service {
	t.Helper()
	svc, err := application.Open(t.TempDir(), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	in["instance"] = "lang"
	raw, _ := json.Marshal(in)
	if res := svc.Execute(context.Background(), action.Request{Action: "setup.resume", Input: raw, Locale: locale}); !res.OK {
		t.Fatalf("setup: %+v", res.Error)
	}
	return svc
}

func defaultLocale(t *testing.T, svc *application.Service) string {
	t.Helper()
	doc := out(t, svc, "config.document", nil)["document"].(map[string]any)
	return doc["i18n"].(map[string]any)["default_locale"].(string)
}

// A console used in Korean must not create an English bot: new rooms would get the English persona.
func TestSetupFollowsTheConsoleLanguageAndTheWizardChoice(t *testing.T) {
	for locale, want := range map[string]string{"ko": "ko", "ja": "ja", "zh-Hans": "zh-Hans", "en": "en", "": "en", "xx": "en"} {
		if got := defaultLocale(t, setupIn(t, locale, map[string]any{})); got != want {
			t.Errorf("console in %q: default_locale %q, want %q", locale, got, want)
		}
	}
	if got := defaultLocale(t, setupIn(t, "ko", map[string]any{"locale": "es"})); got != "es" {
		t.Errorf("the wizard's bot language wins over the console language: %q", got)
	}
	if got := defaultLocale(t, setupIn(t, "ko", map[string]any{"locale": "tlh"})); got != "ko" {
		t.Errorf("an unknown bot language is ignored: %q", got)
	}
}

// A new instance takes the wizard browser's time zone and reads memory with the bot language's pack;
// nothing is fixed to Seoul or Korean unless that is what the browser and the choice say.
func TestSetupTakesTheBrowserTimeZoneAndTheBotLanguagesPack(t *testing.T) {
	doc := out(t, setupIn(t, "en", map[string]any{"locale": "en", "timezone": "America/New_York"}), "config.document", nil)["document"].(map[string]any)
	if tz := doc["i18n"].(map[string]any)["timezone"]; tz != "America/New_York" {
		t.Errorf("timezone %v", tz)
	}
	mem := doc["memory"].(map[string]any)
	if mem["naive_timezone"] != "America/New_York" || len(mem["languages"].([]any)) != 1 || mem["languages"].([]any)[0] != "en" {
		t.Errorf("memory %v %v", mem["naive_timezone"], mem["languages"])
	}
	doc = out(t, setupIn(t, "ko", map[string]any{"timezone": "Mars/Base"}), "config.document", nil)["document"].(map[string]any)
	if tz := doc["i18n"].(map[string]any)["timezone"]; tz != "Asia/Seoul" {
		t.Errorf("an unknown zone keeps the default: %v", tz)
	}
}

// Disaster alerts (KMA) and school notices (Korean universities) only work in Korea: a new instance
// outside it starts with them off (and on UTC when the browser sent no time zone); in Korea, by
// language or by time zone, they stay on.
func TestSetupSwitchesTheKoreaOnlyFeaturesOffOutsideKorea(t *testing.T) {
	for _, c := range []struct {
		name, console string
		in            map[string]any
		on            bool
		tz            string
	}{
		{"en + London", "en", map[string]any{"locale": "en", "timezone": "Europe/London"}, false, "Europe/London"},
		{"en + no zone", "en", map[string]any{"locale": "en"}, false, "UTC"},
		{"ko + no zone", "ko", map[string]any{"locale": "ko"}, true, "Asia/Seoul"},
		{"en + Seoul", "en", map[string]any{"locale": "en", "timezone": "Asia/Seoul"}, true, "Asia/Seoul"},
		{"ko + New York", "ko", map[string]any{"locale": "ko", "timezone": "America/New_York"}, true, "America/New_York"},
	} {
		doc := out(t, setupIn(t, c.console, c.in), "config.document", nil)["document"].(map[string]any)
		feats := doc["features"].(map[string]any)
		for _, f := range []string{"disasters", "notices"} {
			if feats[f] != c.on {
				t.Errorf("%s: feature %s = %v, want %v", c.name, f, feats[f], c.on)
			}
		}
		if feats["weather"] != true {
			t.Errorf("%s: weather works worldwide and stays on: %v", c.name, feats["weather"])
		}
		if tz := doc["i18n"].(map[string]any)["timezone"]; tz != c.tz {
			t.Errorf("%s: timezone %v, want %s", c.name, tz, c.tz)
		}
		if tz := doc["memory"].(map[string]any)["naive_timezone"]; tz != c.tz {
			t.Errorf("%s: naive_timezone %v, want %s", c.name, tz, c.tz)
		}
	}
}
