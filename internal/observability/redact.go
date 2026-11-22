package observability

import (
	"regexp"
	"strings"
)

var (
	// The optional first group is a counting word ("input_token=5" is a number of tokens, not a token).
	secretish = regexp.MustCompile(`(?i)((?:input|output|prompt|completion|total|cached|cache|reasoning|max|min)[_-])?((?:api|auth|service|access|private)[_-]?key|token|secret|password)\s*[:=]\s*\S+`)

	// An Authorization header value is "Scheme credentials"; the whole value goes, not just the scheme.
	authHeader = regexp.MustCompile(`(?i)(authorization\s*[:=]\s*)(?:(?:bearer|bot|basic)\s+)?\S+`)
	bearer     = regexp.MustCompile(`(?i)\b(bearer\s+)[A-Za-z0-9._~+/=-]{8,}`)
	// "Bot " alone is ordinary text; only a token-sized word after it is a credential.
	botToken = regexp.MustCompile(`\b(Bot\s+)[A-Za-z0-9._-]{20,}`)
	// A Discord bot token is three base64url segments: user id, timestamp, HMAC.
	discordToken = regexp.MustCompile(`[\w-]{23,28}\.[\w-]{6,7}\.[\w-]{27,}`)
	// sk-... / sk_... style API keys (OpenAI, Anthropic, and lookalikes).
	skKey = regexp.MustCompile(`\bsk[-_][A-Za-z0-9_-]{17,}`)
	// Tavily keys ("tvly-dev-…") and Google API keys ("AIza…", also Gemini's).
	tavilyKey = regexp.MustCompile(`\btvly-[A-Za-z0-9_-]{16,}`)
	googleKey = regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{20,}`)
	// key=, api_key=, token= and friends inside a URL query string (authKey and serviceKey are the
	// Korean public-data portals' names for it).
	urlSecret = regexp.MustCompile(`(?i)([?&](?:api[_-]?key|auth[_-]?key|service[_-]?key|key|token|access[_-]?token|secret|password|auth)=)[^&\s"'<>]+`)
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
		i := strings.IndexAny(m, ":=")
		// a value an earlier rule already masked keeps the rest of its line (the next query parameter)
		if i >= 0 && strings.HasPrefix(strings.TrimLeft(m[i+1:], " \t"), "***") {
			return m
		}
		if sub := secretish.FindStringSubmatch(m); sub[1] != "" && strings.EqualFold(sub[2], "token") && numericValue(m[i+1:]) {
			return m // a count of tokens ("input_token=5"), not a token
		}
		return m[:strings.IndexAny(m, ":=")] + "=***"
	})
	s = discordToken.ReplaceAllString(s, "***")
	s = skKey.ReplaceAllString(s, "sk-***")
	s = tavilyKey.ReplaceAllString(s, "tvly-***")
	s = googleKey.ReplaceAllString(s, "AIza***")
	return s
}

// numericValue reports whether a key:value fragment's value is a plain number, which makes a
// counting prefix ("input_token") mean a count rather than the token itself.
func numericValue(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return false
	}
	for _, r := range v {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
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
