package observability

import (
	"regexp"
	"strings"
)

var (
	secretish = regexp.MustCompile(`(?i)(api[_-]?key|token|secret|password)\s*[:=]\s*\S+`)

	// An Authorization header value is "Scheme credentials"; the whole value goes, not just the scheme.
	authHeader = regexp.MustCompile(`(?i)(authorization\s*[:=]\s*)(?:(?:bearer|bot|basic)\s+)?\S+`)
	bearer     = regexp.MustCompile(`(?i)\b(bearer\s+)[A-Za-z0-9._~+/=-]{8,}`)
	// "Bot " alone is ordinary text; only a token-sized word after it is a credential.
	botToken = regexp.MustCompile(`\b(Bot\s+)[A-Za-z0-9._-]{20,}`)
	// A Discord bot token is three base64url segments: user id, timestamp, HMAC.
	discordToken = regexp.MustCompile(`[\w-]{23,28}\.[\w-]{6,7}\.[\w-]{27,}`)
	// sk-... / sk_... style API keys (OpenAI, Anthropic, and lookalikes).
	skKey = regexp.MustCompile(`\bsk[-_][A-Za-z0-9_-]{17,}`)
	// key=, api_key=, token= and friends inside a URL query string.
	urlSecret = regexp.MustCompile(`(?i)([?&](?:api[_-]?key|key|token|access[_-]?token|secret|password|auth)=)[^&\s"'<>]+`)
)

// Redact removes credentials that slipped into a log line or error text. It keeps the surrounding words
// so the line stays useful for diagnosis.
func Redact(s string) string {
	if s == "" {
		return s
	}
	s = authHeader.ReplaceAllString(s, "${1}***")
	s = bearer.ReplaceAllString(s, "${1}***")
	s = botToken.ReplaceAllString(s, "${1}***")
	s = urlSecret.ReplaceAllString(s, "${1}***")
	s = secretish.ReplaceAllStringFunc(s, func(m string) string {
		// a value an earlier rule already masked keeps the rest of its line (the next query parameter)
		if i := strings.IndexAny(m, ":="); i >= 0 && strings.HasPrefix(strings.TrimLeft(m[i+1:], " \t"), "***") {
			return m
		}
		return m[:strings.IndexAny(m, ":=")] + "=***"
	})
	s = discordToken.ReplaceAllString(s, "***")
	s = skKey.ReplaceAllString(s, "sk-***")
	return s
}

func Health(liveness, dbOK, featureDegraded bool) map[string]any {
	ready := liveness && dbOK
	return map[string]any{
		"liveness":               liveness,
		"readiness":              ready,
		"feature_degraded":       featureDegraded,
		"not_a_single_ready_bit": true,
	}
}
