package application

import "testing"

func TestAcceptedLocale(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ header, want string }{
		{"", "en"},
		{"ko-KR,ko;q=0.9", "ko"},
		{"en-US,en;q=0.8", "en"},
		{"ja", "ja"},
		{"zh-Hans", "zh-Hans"},
		{"fr,ko;q=0.9", "ko"}, // the first language the console has wins, not the first offered
		{"zh-Hant", "en"},     // traditional Chinese is not bundled
		{"*", "en"},
		{"!!", "en"},
		{"ko;q=0.1,en;q=0.9", "ko"}, // the first usable tag wins even at low q
	} {
		if got := AcceptedLocale(tc.header); got != tc.want {
			t.Errorf("AcceptedLocale(%q) = %q, want %q", tc.header, got, tc.want)
		}
	}
}
