package relnotes

import (
	"regexp"
	"strconv"
	"strings"
)

// Version is a parsed semantic version. A build made after a tag (git describe: v1.5.1-3-gabc1234,
// v1.5.1-dirty) counts as that tag, so After is set and the build is never older than the tag.
type Version struct {
	Major, Minor, Patch int
	Pre                 string // pre-release part without the dash: "rc1"; "" for a release
	After               bool   // a source build made after the tag
}

// describeSuffix is what `git describe --tags --dirty` adds to a tag: commits after it (-3-gabc1234)
// and uncommitted changes (-dirty).
var describeSuffix = regexp.MustCompile(`(-[0-9]+-g[0-9a-f]{7,40})?(-dirty)?$`)

var versionRE = regexp.MustCompile(`^v?([0-9]+)\.([0-9]+)\.([0-9]+)(?:-([0-9A-Za-z.-]+))?$`)

// ParseVersion reads "v1.5.1", "1.5.1", "v1.5.1-rc1" and source builds such as "v1.5.1-3-gabc1234".
// Build metadata after "+" is ignored. It reports false for anything else (for example "dev" or "").
func ParseVersion(s string) (Version, bool) {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	suffix := describeSuffix.FindString(s)
	s = strings.TrimSuffix(s, suffix)
	m := versionRE.FindStringSubmatch(s)
	if m == nil {
		return Version{}, false
	}
	v := Version{Pre: m[4], After: suffix != ""}
	var err error
	if v.Major, err = strconv.Atoi(m[1]); err != nil {
		return Version{}, false
	}
	if v.Minor, err = strconv.Atoi(m[2]); err != nil {
		return Version{}, false
	}
	if v.Patch, err = strconv.Atoi(m[3]); err != nil {
		return Version{}, false
	}
	return v, true
}

// Compare orders two versions by semantic version: -1, 0 or 1. A release is above a pre-release of the
// same numbers. A source build after a tag equals that tag.
func Compare(a, b string) int {
	va, ok1 := ParseVersion(a)
	vb, ok2 := ParseVersion(b)
	if !ok1 || !ok2 {
		return strings.Compare(a, b)
	}
	return va.compare(vb)
}

func (v Version) compare(o Version) int {
	for _, p := range [][2]int{{v.Major, o.Major}, {v.Minor, o.Minor}, {v.Patch, o.Patch}} {
		if p[0] != p[1] {
			if p[0] < p[1] {
				return -1
			}
			return 1
		}
	}
	return comparePre(v.Pre, o.Pre)
}

// comparePre follows semantic versioning: no pre-release is above any pre-release; identifiers
// compare numerically when both are numbers, otherwise as text, and a shorter list is lower.
func comparePre(a, b string) int {
	switch {
	case a == b:
		return 0
	case a == "":
		return 1
	case b == "":
		return -1
	}
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		na, ea := strconv.Atoi(pa[i])
		nb, eb := strconv.Atoi(pb[i])
		switch {
		case ea == nil && eb == nil && na != nb:
			if na < nb {
				return -1
			}
			return 1
		case ea == nil && eb != nil:
			return -1
		case ea != nil && eb == nil:
			return 1
		case ea != nil && eb != nil && pa[i] != pb[i]:
			return strings.Compare(pa[i], pb[i])
		}
	}
	switch {
	case len(pa) < len(pb):
		return -1
	case len(pa) > len(pb):
		return 1
	}
	return 0
}
