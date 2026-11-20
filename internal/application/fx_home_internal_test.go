package application

import (
	"testing"

	"masamong/internal/currency"
)

// The home currency comes from the first tag that names a country with a currency: the person's saved
// language when it names one, else the tag Discord sent, else the reply language's likely region, else
// USD. A region that is not such a country (es-419) is skipped, and the reply language alone (the
// instance default when nothing is saved) never outranks Discord's tag.
func TestFXHomePrecedence(t *testing.T) {
	lex := currency.New(nil)
	for _, tc := range []struct {
		name, saved, reply, tag, want string
	}{
		{"saved en-US beats Discord en-GB", "en-US", "en-US", "en-GB", "USD"},
		{"saved en, Discord pt-BR", "en", "en", "pt-BR", "BRL"},
		{"saved en, guild en-GB", "en", "en", "en-GB", "GBP"},
		{"saved es, guild en-GB", "es", "es", "en-GB", "GBP"},
		{"saved es-419 is skipped for Discord pt-BR", "es-419", "es-419", "pt-BR", "BRL"},
		{"saved es, Discord es-419 falls to es's likely region", "es", "es", "es-419", "EUR"},
		{"instance default ko-KR does not beat guild en-GB", "", "ko-KR", "en-GB", "GBP"},
		{"nothing names a region: the reply language's likely region", "", "ja", "", "JPY"},
		{"no country anywhere: USD", "", "es-419", "es-419", "USD"},
	} {
		in := map[string]any{}
		if tc.tag != "" {
			in["locale_tag"] = tc.tag
		}
		if got := fxHome(lex, tc.saved, tc.reply, in); got != tc.want {
			t.Errorf("%s: home %s, want %s", tc.name, got, tc.want)
		}
	}
}
