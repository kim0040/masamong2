package discord

import "testing"

func TestFXDefaultCurrency(t *testing.T) {
	cmd, _ := ByName("fx")
	for _, tc := range []struct{ locale, base, want string }{
		{"ko", "", "KRW"},
		{"ko-KR", "", "KRW"},
		{"en-US", "", "USD"},
		{"ja", "", "USD"},
		{"ko", "USD", "USD"},
		{"en", "JPY", "JPY"},
	} {
		act, input := cmd.Build(map[string]string{"base": tc.base}, InvokeContext{Locale: tc.locale})
		if act != "finance.fx" || input["base"] != tc.want {
			t.Errorf("locale %q, base %q: got %s %v, want finance.fx %s", tc.locale, tc.base, act, input, tc.want)
		}
	}
}
