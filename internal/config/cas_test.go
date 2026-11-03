package config

import (
	"bytes"
	"slices"
	"testing"

	"masamong/internal/lang"
)

func TestHashTreatsCRLFAsTheSameConfigContent(t *testing.T) {
	lf := sampleYAML("a")
	crlf := bytes.ReplaceAll(lf, []byte("\n"), []byte("\r\n"))
	if Hash(lf) != Hash(crlf) {
		t.Fatal("CRLF changed the normalized configuration hash")
	}
}

func sampleYAML(id string) []byte {
	d := Default(id, "/tmp/x")
	b, err := Marshal(d)
	if err != nil {
		panic(err)
	}
	return b
}

func TestBeginApplyRejectsStaleRevision(t *testing.T) {
	raw := sampleYAML("a")
	st := State{Current: &Revision{Revision: 3, Status: StatusCommitted, ContentHash: Hash(raw), Content: raw}}
	_, err := BeginApply(st, 2, raw)
	if err == nil || !isStale(err) {
		t.Fatalf("expected stale, got %v", err)
	}
}

func TestBeginApplyAcceptsMatchingBase(t *testing.T) {
	raw := sampleYAML("a")
	st := State{Current: &Revision{Revision: 1, Status: StatusCommitted, ContentHash: Hash(raw), Content: raw}}
	next, err := BeginApply(st, 1, raw)
	if err != nil {
		t.Fatal(err)
	}
	if next.Revision != 2 || next.Status != StatusPrepared {
		t.Fatalf("got %+v", next)
	}
}

func TestRecoverCommitsWhenFileMatchesPrepared(t *testing.T) {
	raw := sampleYAML("a")
	prep := Revision{Revision: 2, Status: StatusPrepared, ContentHash: Hash(raw), Content: raw}
	commit, discard := Recover(State{Prepared: &prep, FileHash: Hash(raw), FileBytes: raw})
	if discard != nil || commit == nil || commit.Status != StatusCommitted {
		t.Fatalf("commit=%v discard=%v", commit, discard)
	}
}

func TestRecoverDiscardsWhenFileDoesNotMatch(t *testing.T) {
	raw := sampleYAML("a")
	other := sampleYAML("b")
	prep := Revision{Revision: 2, Status: StatusPrepared, ContentHash: Hash(raw), Content: raw}
	commit, discard := Recover(State{Prepared: &prep, FileHash: Hash(other), FileBytes: other})
	if commit != nil || discard == nil || discard.Status != StatusDiscarded {
		t.Fatalf("commit=%v discard=%v", commit, discard)
	}
}

func TestDefaultBundledLocalesMatchAvailablePacks(t *testing.T) {
	if got, want := Default("test", t.TempDir()).I18n.Bundled, lang.Available(); !slices.Equal(got, want) {
		t.Fatalf("bundled locales %v do not match available packs %v", got, want)
	}
}

func TestUnknownYAMLFieldRejected(t *testing.T) {
	_, err := Parse([]byte("schema_version: 1\ninstance: {id: x}\nnot_a_field: 1\n"))
	if err == nil {
		t.Fatal("expected unknown field error")
	}
}

func isStale(err error) bool {
	return err != nil && (err == ErrStale || (err.Error() != "" && contains(err.Error(), "CONFIG_CONFLICT") || contains(err.Error(), "expected")))
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || (len(s) > 0 && (stringIndex(s, sub) >= 0)))
}

func stringIndex(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// A chat model's timeout is 0 (the default) or 5–600 seconds.
func TestModelTimeoutBounds(t *testing.T) {
	for secs, ok := range map[int]bool{0: true, 5: true, 120: true, 3: false, 601: false} {
		d := Default("x", t.TempDir())
		m := d.Models["planner"]
		m.TimeoutSeconds = secs
		d.Models["planner"] = m
		raw, err := Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Parse(raw); (err == nil) != ok {
			t.Fatalf("timeout_seconds %d: err=%v", secs, err)
		}
	}
}

// A chat model's effort is empty (low) or one of ModelEfforts, xhigh and max included; every chat role
// (FallbackRoles) takes one, the embedding, rerank and image roles none. A fallback may name its own.
func TestModelEffort(t *testing.T) {
	for _, c := range []struct {
		role, effort string
		ok           bool
	}{{"planner", "", true}, {"planner", "none", true}, {"response", "high", true}, {"planner", "max", true}, {"response", "xhigh", true},
		{"summary", "none", true}, {"summary", "medium", true}, {"vision", "low", true}, {"embed", "low", false}, {"rerank", "none", false},
		{"planner", "ultra", false}, {"response", "Low", false}} {
		d := Default("x", t.TempDir())
		m := d.Models[c.role]
		if c.role == "vision" {
			m = d.Models["response"]
		}
		m.Effort = c.effort
		d.Models[c.role] = m
		raw, err := Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Parse(raw); (err == nil) != c.ok {
			t.Fatalf("models.%s.effort %q: err=%v", c.role, c.effort, err)
		}
	}
	for effort, ok := range map[string]bool{"": true, "none": true, "max": true, "xhigh": true, "turbo": false} {
		d := Default("x", t.TempDir())
		m := d.Models["response"]
		m.Fallbacks = []ModelFallback{{Provider: m.Provider, Model: m.Model + "-other", Effort: effort}}
		d.Models["response"] = m
		raw, err := Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Parse(raw); (err == nil) != ok {
			t.Fatalf("fallbacks[0].effort %q: err=%v", effort, err)
		}
	}
}
