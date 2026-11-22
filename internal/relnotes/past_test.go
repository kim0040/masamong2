package relnotes

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The notes written for the releases made so far (v1.0.0 to v1.5.1; v1.5.0 never shipped and has none).
func TestPastReleasesAreCovered(t *testing.T) {
	var got []string
	for _, n := range All() {
		got = append(got, n.Version)
	}
	want := "v1.0.0 v1.1.0 v1.1.1 v1.2.0 v1.2.1 v1.2.2 v1.3.0 v1.4.0 v1.5.1"
	if !strings.HasPrefix(strings.Join(got, " "), want) {
		t.Errorf("embedded versions %v, want them to start with %s", got, want)
	}
	if _, ok := For("v1.5.0"); ok {
		t.Error("v1.5.0 was replaced by v1.5.1 and has no note")
	}
	between := func(after, upTo string) string {
		var out []string
		for _, n := range Between(after, upTo) {
			out = append(out, n.Version)
		}
		return strings.Join(out, " ")
	}
	if got := between("v1.4.0", "v1.5.1"); got != "v1.5.1" {
		t.Errorf("v1.4.0 to v1.5.1: %q", got)
	}
	if got := between("v1.1.1", "v1.4.0-5-gabc1234"); got != "v1.2.0 v1.2.1 v1.2.2 v1.3.0 v1.4.0" {
		t.Errorf("v1.1.1 to a build after v1.4.0: %q", got)
	}
	// what users feel: the FX fix and the person-memory and alert-level changes reach them, the first release does not
	if n, _ := For("v1.3.0"); !n.Has(AudienceUser) {
		t.Error("v1.3.0 fixed a command people use")
	}
	if n, _ := For("v1.0.0"); n.Has(AudienceUser) {
		t.Error("the first release has nothing to tell users")
	}
}

// The notes keep to the facts of the release and carry no names, IDs or server details.
func TestPastNotesNameNobody(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join("..", "..", "release-notes"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") && !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join("..", "..", "release-notes", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		for _, bad := range []string{"@", "pjins", "Hyeonmin", "광양", "L1051100", "/pull/", "ANTHROPIC_API_KEY"} {
			if strings.Contains(text, bad) {
				t.Errorf("%s mentions %q", e.Name(), bad)
			}
		}
	}
}
