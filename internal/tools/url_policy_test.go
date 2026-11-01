package tools

import (
	"strings"
	"testing"
)

// The message names the rule that failed (an https address with a user name is not "not https").
func TestValidateHTTPURLNamesTheRealReason(t *testing.T) {
	for raw, want := range map[string]string{
		"https://user:token@gateway/v1": "user name or password",
		"http://localhost.internal/v1":  "only looks like",
		"http://127.0.0.1.nip.io":       "only looks like",
		"http://localhost./v1":          "only looks like",
		"http://gateway.example/v1":     "not for",
		"ftp://gateway.example/v1":      "scheme",
		"https://":                      "not a valid web address",
		"https://gateway.example/v1":    "",
		"http://localhost:8080/v1":      "",
		"http://LOCALHOST/v1":           "",
		"http://127.0.0.2/v1":           "",
		"http://[::1]:11434/v1":         "",
		"https://localhost.example/v1":  "",
	} {
		err := ValidateHTTPURL(raw)
		switch {
		case want == "" && err != nil:
			t.Errorf("%s refused: %v", raw, err)
		case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
			t.Errorf("%s: %v, want a message with %q", raw, err, want)
		}
	}
}

func TestValidateLenientSeparatesTheAddressFromEverythingElse(t *testing.T) {
	good := Settings{Description: "d", HTTP: &HTTP{URL: "https://api.example/v1?q={q}"}, Params: []Param{{Name: "q"}}}
	if p, err := ValidateLenient("my.tool", good); p != nil || err != nil {
		t.Fatalf("%v %v", p, err)
	}
	bad := good
	bad.HTTP = &HTTP{URL: "https://user:pw@api.example/v1?q={q}"}
	p, err := ValidateLenient("my.tool", bad)
	if err != nil || p == nil || p.Path != "tools.my.tool.http.url" || !strings.Contains(p.Error(), "user name") {
		t.Fatalf("%v %v", p, err)
	}
	if err := Validate("my.tool", bad); err == nil {
		t.Fatal("a new change with that address must be refused")
	}
	off := false
	bad.Enabled = &off
	if err := Validate("my.tool", bad); err != nil {
		t.Fatalf("a tool that is switched off calls nothing: %v", err)
	}
	broken := good
	broken.Description = ""
	if _, err := ValidateLenient("my.tool", broken); err == nil {
		t.Fatal("anything else wrong is still an error")
	}
}
