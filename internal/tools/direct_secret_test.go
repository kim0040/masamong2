package tools

import (
	"errors"
	"strings"
	"testing"
)

// A key written straight into an operator tool's headers or web address is refused when the
// configuration changes (Validate) and names the field; a configuration an earlier release accepted still
// loads (ValidateLenient). References ({credential}) and declared placeholders pass.
func TestDirectSecretsAreRefusedOnChangeOnly(t *testing.T) {
	tool := func(url string, headers map[string]string) Settings {
		return Settings{Description: "d", HTTP: &HTTP{URL: url, Headers: headers, CredentialRef: "env:K"}, Params: []Param{{Name: "q"}, {Name: "key"}}}
	}
	// synthetic values only
	direct := "synthetic" + "-direct-value"
	refused := []struct {
		name  string
		s     Settings
		field string
	}{
		{"authorization", tool("https://api.example/v1", map[string]string{"Authorization": "Bearer " + direct}), "headers.Authorization"},
		{"x-api-key", tool("https://api.example/v1", map[string]string{"X-API-Key": direct}), "headers.X-API-Key"},
		{"x-auth-key", tool("https://api.example/v1", map[string]string{"X-Auth-Key": direct}), "headers.X-Auth-Key"},
		{"authKey header", tool("https://api.example/v1", map[string]string{"authKey": direct}), "headers.authKey"},
		{"custom token header", tool("https://api.example/v1", map[string]string{"X-Service-Token": direct}), "headers.X-Service-Token"},
		{"cookie with a second literal", tool("https://api.example/v1", map[string]string{"Cookie": "session={credential}; fallback=" + direct}), "headers.Cookie"},
		{"query key", tool("https://api.example/v1?q={q}&key="+direct, nil), "the key parameter"},
		{"query authKey", tool("https://api.example/v1?authKey="+direct, nil), "the authKey parameter"},
		{"query serviceKey", tool("https://api.example/v1?serviceKey="+direct+"&q={q}", nil), "the serviceKey parameter"},
		{"query access_token", tool("https://api.example/v1?access_token="+direct, nil), "the access_token parameter"},
		{"query passwd", tool("https://api.example/v1?passwd="+direct, nil), "the passwd parameter"},
		{"query percent-encoded", tool("https://api.example/v1?api%5Fkey="+direct, nil), "the api_key parameter"},
	}
	for _, c := range refused {
		err := Validate("ops.lookup", c.s)
		if err == nil || !strings.Contains(err.Error(), "tools.ops.lookup") || !strings.Contains(err.Error(), c.field) || !strings.Contains(err.Error(), "credential_ref") {
			t.Fatalf("%s: %v", c.name, err)
		}
		if strings.Contains(err.Error(), direct) {
			t.Fatalf("%s: the error repeats the value: %v", c.name, err)
		}
		if bad, err := ValidateLenient("ops.lookup", c.s); bad != nil || err != nil {
			t.Fatalf("%s: a stored configuration must still load: %v %v", c.name, bad, err)
		}
	}
	allowed := []Settings{
		tool("https://api.example/v1?key={credential}&q={q}", map[string]string{"Authorization": "Bearer {credential}", "Accept-Language": "ko"}),
		tool("https://api.example/v1?key={key}", map[string]string{"X-API-Key": "{credential}"}),
		tool("https://api.example/v1?q={q}&page=2", map[string]string{"User-Agent": "masamong"}),
		// a credential behind a scheme word or a cookie's name is the reference alone
		tool("https://api.example/v1", map[string]string{"Authorization": "Bearer {credential}", "Cookie": "session={credential}"}),
	}
	for i, s := range allowed {
		if err := Validate("ops.lookup", s); err != nil {
			t.Fatalf("allowed %d: %v", i, err)
		}
	}
}

// The refusal is a typed error, so the caller can say it in the person's language from the tool and the
// field without parsing the English text.
func TestDirectSecretErrorNamesToolAndField(t *testing.T) {
	var direct *DirectSecretError
	err := DirectSecret("ops.lookup", &HTTP{URL: "https://api.example/v1?serviceKey=synthetic-value", CredentialRef: "env:K"})
	if !errors.As(err, &direct) || direct.Tool != "ops.lookup" || direct.Where() != "url: serviceKey" {
		t.Fatalf("query: %#v", err)
	}
	err = DirectSecret("ops.lookup", &HTTP{URL: "https://api.example/v1", Headers: map[string]string{"X-API-Key": "synthetic-value"}})
	if !errors.As(err, &direct) || direct.Where() != "headers.X-API-Key" {
		t.Fatalf("header: %#v", err)
	}
}

// A cookie whose value is the reference alone inside quotes (session="{credential}") is still the
// reference, as it is without the quotes. A literal beside the reference — in the quotes or after them —
// keeps a key in the configuration and is refused.
func TestQuotedCredentialAloneIsAllowed(t *testing.T) {
	tool := func(cookie string) Settings {
		return Settings{Description: "d", HTTP: &HTTP{URL: "https://api.example/v1", Headers: map[string]string{"Cookie": cookie}, CredentialRef: "env:K"}}
	}
	literal := "synthetic" + "-direct-value"
	for _, ok := range []string{`session="{credential}"`, `session='{credential}'`, `session="{credential}"; theme="{credential}"`} {
		if err := Validate("ops.lookup", tool(ok)); err != nil {
			t.Fatalf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{
		`session="{credential}"; fallback=` + literal,
		`session="{credential}"; fallback="` + literal + `"`,
		`session="` + literal + `{credential}"`,
		`session="{credential}` + literal + `"`,
	} {
		if err := Validate("ops.lookup", tool(bad)); err == nil || !strings.Contains(err.Error(), "headers.Cookie") {
			t.Fatalf("%s: %v", bad, err)
		}
	}
}
