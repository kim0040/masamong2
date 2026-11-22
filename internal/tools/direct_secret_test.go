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
		{"custom token header", tool("https://api.example/v1", map[string]string{"X-Service-Token": direct}), "headers.X-Service-Token"},
		{"query key", tool("https://api.example/v1?q={q}&key="+direct, nil), "the key parameter"},
		{"query serviceKey", tool("https://api.example/v1?serviceKey="+direct+"&q={q}", nil), "the serviceKey parameter"},
		{"query access_token", tool("https://api.example/v1?access_token="+direct, nil), "the access_token parameter"},
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
