package chat

import (
	"strings"
	"unicode"

	"masamong/internal/lang"
)

// DetectLanguage returns a supported language when the message contains clear script or Spanish
// signals. Latin messages without Spanish markers default to English; text without letters leaves
// the caller's configured voice in place.
func DetectLanguage(message string) string {
	var latin, han bool
	for _, r := range message {
		switch {
		case unicode.Is(unicode.Hangul, r):
			return "ko"
		case unicode.Is(unicode.Hiragana, r), unicode.Is(unicode.Katakana, r):
			return "ja"
		case unicode.Is(unicode.Han, r):
			han = true
		case unicode.IsLetter(r) && r <= unicode.MaxASCII:
			latin = true
		case strings.ContainsRune("áéíóúüñ¿¡", unicode.ToLower(r)):
			return "es"
		}
	}
	if han {
		return "zh-Hans"
	}
	if latin {
		lower := " " + strings.ToLower(message) + " "
		for _, word := range []string{" hola ", " gracias ", " por favor ", " qué ", " para ", " tengo ", " quiero ", " mañana ", " buenos días ", " buenas tardes "} {
			if strings.Contains(lower, word) {
				return "es"
			}
		}
		return "en"
	}
	return ""
}

func answerTokenLimit(message, locale string) int {
	tag := lang.Normalize(locale)
	base, long := 1200, 2200
	if tag == "ko" || tag == "ja" || tag == "zh-Hans" {
		base, long = 1800, 3000
	}
	text := strings.ToLower(message)
	for _, cue := range []string{"in detail", "very detailed", "long answer", "long essay", "3000 characters", "자세히", "자세하게", "상세히", "길게", "3000자 이상", "詳しく", "詳細に", "長文", "3000文字", "详细说明", "详细介绍", "长篇", "detalladamente", "en detalle", "respuesta larga"} {
		if strings.Contains(text, cue) {
			return long
		}
	}
	return base
}
