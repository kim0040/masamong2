package application

import (
	"strings"
	"testing"
)

func TestCrashRestartAlertOmitsEmptyReason(t *testing.T) {
	for _, locale := range []string{"ko", "en", "ja", "zh-Hans", "es"} {
		got := alertText(locale, alertCrash, map[string]any{"reason": ""})
		if got == "" || strings.Contains(got, "()") || strings.Contains(got, "（ ）") {
			t.Errorf("%s alert still renders an empty reason: %q", locale, got)
		}
	}
}
