package relnotes

import (
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"sync"

	releasenotes "masamong/release-notes"
)

// Load reads every vX.Y.Z.json of fsys, validates it and returns the notes oldest first. A file whose
// name differs from "<version>.json", a second note for the same version, or an invalid note is an error.
func Load(fsys fs.FS) ([]Note, error) {
	names, err := fs.Glob(fsys, "*.json")
	if err != nil {
		return nil, err
	}
	var notes []Note
	var bad []string
	seen := map[string]string{}
	for _, name := range names {
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			bad = append(bad, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		n, err := Parse(data)
		if err != nil {
			bad = append(bad, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		if name != n.Version+".json" {
			bad = append(bad, fmt.Sprintf("%s: file name must be %s.json", name, n.Version))
			continue
		}
		if prev, dup := seen[n.Version]; dup {
			bad = append(bad, fmt.Sprintf("%s: version %s is already in %s", name, n.Version, prev))
			continue
		}
		seen[n.Version] = name
		notes = append(notes, n)
	}
	sort.SliceStable(notes, func(i, j int) bool { return Compare(notes[i].Version, notes[j].Version) < 0 })
	if len(bad) > 0 {
		return notes, fmt.Errorf("release notes: %s", strings.Join(bad, "; "))
	}
	return notes, nil
}

var (
	embeddedOnce  sync.Once
	embeddedNotes []Note
	embeddedErr   error
)

func embedded() ([]Note, error) {
	embeddedOnce.Do(func() { embeddedNotes, embeddedErr = Load(releasenotes.FS) })
	return embeddedNotes, embeddedErr
}

// EmbeddedError reports why an embedded note was skipped (nil when all are valid). A test pins it to
// nil, so a bad file never reaches a build; at run time an invalid note is skipped, not fatal.
func EmbeddedError() error { _, err := embedded(); return err }

// All returns every embedded note, oldest first. The slice is a copy.
func All() []Note {
	notes, _ := embedded()
	return append([]Note(nil), notes...)
}

// For returns the embedded note of one version ("v1.5.1" or "1.5.1"). A source build such as
// "v1.5.1-3-gabc1234" has no note of its own, so it reports false.
func For(version string) (Note, bool) {
	v, ok := ParseVersion(version)
	if !ok || v.After {
		return Note{}, false
	}
	for _, n := range All() {
		if Compare(n.Version, version) == 0 {
			return n, true
		}
	}
	return Note{}, false
}

// Between returns the embedded notes of the versions above after and up to upTo (inclusive), oldest
// first. It works for skipped versions (after v1.2.0, up to v1.4.0 gives v1.2.1 … v1.4.0, whichever
// exist). A source build counts as the tag it was built after: upTo "v1.5.1-3-gabc1234" includes v1.5.1,
// and after "v1.5.1-3-gabc1234" excludes it. An empty after means no lower bound. Anything that is not
// a version (a dev build, "") gives nil.
func Between(after, upTo string) []Note {
	if _, ok := ParseVersion(upTo); !ok {
		return nil
	}
	if after != "" {
		if _, ok := ParseVersion(after); !ok {
			return nil
		}
	}
	var out []Note
	for _, n := range All() {
		if (after == "" || Compare(n.Version, after) > 0) && Compare(n.Version, upTo) <= 0 {
			out = append(out, n)
		}
	}
	return out
}
