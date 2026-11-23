package observability

import (
	"strings"
	"testing"
)

func TestRedactDoesNotKeepToken(t *testing.T) {
	s := Redact("Authorization token=supersecretvalue")
	if strings.Contains(s, "supersecretvalue") {
		t.Fatalf("%s", s)
	}
}

func TestRedactSecrets(t *testing.T) {
	const discord = "MTIzNDU2Nzg5MDEyMzQ1Njc4OTA" + ".GabcDE." + "abcdefghijklmnopqrstuvwxyz0123456789" // fake; split so secret scanners do not flag it
	const google = "AIza" + "SyA1234567890" + "abcdefghijklmnopqrstuv"                                  // fake, split the same way
	for _, c := range []struct{ in, secret string }{
		{"password: hunter2", "hunter2"},
		{"call failed Bearer abcdef0123456789.xyz", "abcdef0123456789"},
		{"Authorization: Bearer abcdef0123456789", "abcdef0123456789"},
		{"authorization=Basic dXNlcjpwYXNz", "dXNlcjpwYXNz"},
		{"header Authorization: sometoken", "sometoken"},
		{"gateway said " + discord + " rejected", discord},
		{"Bot " + discord, discord},
		{"using sk-proj-abcdefghijklmnopqrstuv", "abcdefghijklmnopqrstuv"},
		{"key sk_live_abcdefghijklmnopqrst here", "abcdefghijklmnopqrst"},
		{"GET https://api.example.com/v1?key=AIzaSyFAKE&alt=json", "AIzaSyFAKE"},
		{"GET https://x.example/?q=1&api_key=abc123def", "abc123def"},
		{"https://x.example/p?access_token=tok123456&x=1", "tok123456"},
		// key names and key shapes the first rules missed (found by an audit probe)
		{"GET https://apihub.example/x?authKey=KMASECRET123&tm=1 failed", "KMASECRET123"},
		{"GET https://x.example/y?serviceKey=SVCSECRET123 failed", "SVCSECRET123"},
		{"tavily key tvly-dev-ABCDEFGHIJKLMNOPQRSTUV rejected", "ABCDEFGHIJKLMNOPQRSTUV"},
		{"gemini key " + google + " rejected", google[4:]},
		{"x-goog-api-key: " + google, google[4:]},
		{"serviceKey: SVCSECRET123 refused", "SVCSECRET123"},
		{"Authorization: Bearer abcdefghijklmnop12345", "abcdefghijklmnop12345"},
	} {
		got := Redact(c.in)
		if strings.Contains(got, c.secret) {
			t.Errorf("Redact(%q) = %q still has the secret", c.in, got)
		}
	}
}

func TestRedactKeepsSurroundingContext(t *testing.T) {
	got := Redact("GET https://x.example/?q=1&api_key=abc123def&alt=json")
	if !strings.Contains(got, "q=1") || !strings.Contains(got, "alt=json") || !strings.HasPrefix(got, "GET https://x.example/") {
		t.Fatalf("%s", got)
	}
}

func TestRedactLeavesOrdinaryTextAlone(t *testing.T) {
	for _, s := range []string{
		"",
		"안녕하세요, 오늘 점심 뭐 먹을까요? 비밀번호는 알려주지 않을게요.",
		"The Bot is ready and the Bot user joined",
		"Bot 상태가 정상입니다",
		"sk is a short word and so is skill-based routing",
		"토큰 비용이 많이 들었어요 (token budget exceeded)",
		"see https://example.com/docs/page?lang=ko&page=2 for details",
		"keyboard key pressed; monkey=happy",
		"internal/memory/retrieve.go:120 rank.fuse took 35ms",
		"version 1.2.3 built on 2026-10-11T09:00:00Z",
		// counts of tokens are numbers, not secrets
		"input_token=5 max_tokens=500",
		"output_token: 12 prompt_token=7 total_token=19",
		"a tvly- prefix alone and AIza are not keys",
	} {
		if got := Redact(s); got != s {
			t.Errorf("Redact(%q) = %q", s, got)
		}
	}
}

func TestHealthSeparatesLayers(t *testing.T) {
	h := Health(true, true, true)
	if h["readiness"] != true || h["feature_degraded"] != true {
		t.Fatalf("%+v", h)
	}
}

func TestRedactKeepsTheLineAroundNewKeyShapes(t *testing.T) {
	got := Redact("GET https://apihub.example/x?authKey=KMASECRET123&tm=1 failed")
	if got != "GET https://apihub.example/x?authKey=***&tm=1 failed" {
		t.Fatalf("%s", got)
	}
	if got := Redact("input_token=5 token=abcdef123456"); got != "input_token=5 token=***" {
		t.Fatalf("a count and a token on one line: %s", got)
	}
}

// The token-count exemption covers numbers only: input_token=5 is usage, input_token=text is a token
// with a counting prefix in its name, and it must still be masked.
func TestRedactMasksNonNumericTokenCounts(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"input_token=5 output_token: 12", "input_token=5 output_token: 12"},
		{"prompt_token=7 total_token=19 max_token=500", "prompt_token=7 total_token=19 max_token=500"},
		{"input_token=synthetic-fixture", "input_token=***"},
		{"output_token: asecretvalue", "output_token=***"},
		{"request=[prompt_token=token=tokvalue9]", "request=[prompt_token=***"},
	} {
		if got := Redact(c.in); got != c.want {
			t.Errorf("Redact(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
