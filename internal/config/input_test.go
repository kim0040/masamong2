package config

import (
	"strings"
	"testing"
)

// A stored_name names the staged file the attachment delete path joins to the channel RAG dir. Only
// the generated form — a sha256 hex name plus the original extension — is safe; anything containing
// a separator or a made-up name is rejected from a candidate and dropped from a committed document.
func TestStoredNameValidation(t *testing.T) {
	doc := func(name string) string {
		return minimalDoc + `guilds:
  g1:
    channels:
      c1:
        respond: always
        rag_attachments:
          - {id: a1, content_hash: h1, filename: note.txt, stored_name: "` + name + `"}
`
	}
	for _, bad := range []string{"../outside", "a/b", "a\\b", ".hidden", "madeup.txt", "ABCDEF0123456789abcdef0123456789abcdef0123456789abcdef0123456789"} {
		if _, err := Parse([]byte(doc(bad))); err == nil || !strings.Contains(err.Error(), "stored_name") {
			t.Fatalf("stored_name %q: a candidate must be rejected naming the key: %v", bad, err)
		}
		got, problems, err := ParseCommitted([]byte(doc(bad)))
		if err != nil {
			t.Fatalf("stored_name %q: a committed document must load: %v", bad, err)
		}
		if n := len(got.Guilds["g1"].Channels["c1"].RAGAttachments); n != 0 {
			t.Fatalf("stored_name %q: the attachment must be dropped, %d kept", bad, n)
		}
		if len(problems) == 0 || !strings.Contains(problems[0].Path, "stored_name") {
			t.Fatalf("stored_name %q: the drop must be reported: %v", bad, problems)
		}
	}
	for _, good := range []string{
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef.txt",
		"", // an inline attachment keeps no staged file
	} {
		if _, err := Parse([]byte(doc(good))); err != nil {
			t.Fatalf("stored_name %q must be accepted: %v", good, err)
		}
	}
}

// config diff/validate --file must take JSON the same as YAML: JSON text — including \uXXXX
// surrogate escapes for emoji — is valid input, not a YAML decode error.
func TestJSONInputAccepted(t *testing.T) {
	raw := []byte(`{"schema_version":1,"instance":{"id":"x"},` +
		`"memory":{"capture":false},"personas":{"p":{"label":"\ud83d\ude00"}}}`)
	d, err := Parse(raw)
	if err != nil {
		t.Fatalf("JSON input must be accepted: %v", err)
	}
	if d.Memory.Capture {
		t.Fatal("JSON input: explicit capture:false must be kept")
	}
	if d.Personas["p"].Label != "😀" {
		t.Fatalf("a surrogate-pair escape must decode, got %q", d.Personas["p"].Label)
	}
	got, problems, err := ParseCommitted(raw)
	if err != nil || len(problems) != 0 || got.Memory.Capture {
		t.Fatalf("committed JSON input: %v %v %+v", err, problems, got.Memory)
	}
	// something that only looks like JSON still gets a parse error, not a panic
	if _, err := Parse([]byte(`{"schema_version":`)); err == nil {
		t.Fatal("truncated JSON must be an error")
	}
}
