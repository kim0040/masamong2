package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Rendered diagrams: a D2 source x.d2 is committed with its two renderings, x-light.svg and x-dark.svg
// (scripts/diagrams.sh, make diagrams). Rendering needs d2, which CI does not have, so freshness is
// checked from stamps instead: each SVG ends with the sha256 of the source it was rendered from and of
// the shared style file (_style-light.d2 or _style-dark.d2 beside the source). A source or style edited
// without a render, a missing SVG and an SVG whose source is gone are all reported here.
//
// Files whose name starts with "_" are parts of other diagrams, not diagrams.

var (
	diagramModes = []string{"light", "dark"}
	sourceStamp  = regexp.MustCompile(`<!-- d2-source-sha256: ([0-9a-f]{64}) -->`)
	styleStamp   = regexp.MustCompile(`<!-- d2-style-sha256: ([0-9a-f]{64}) -->`)
)

// stampOf is the hash a rendering carries of a file: its bytes without carriage returns, so a Windows
// checkout that turns line endings into CRLF still matches.
func stampOf(p string) (string, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(bytes.ReplaceAll(b, []byte("\r"), nil))
	return hex.EncodeToString(sum[:]), nil
}

// diagramFiles lists the maintained files with the given suffix, slash-separated and relative to root.
func diagramFiles(root, suffix string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			rel, _ := filepath.Rel(root, p)
			if p != root && (skipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".") || frozen[filepath.ToSlash(rel)]) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(p, suffix) {
			rel, _ := filepath.Rel(root, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}

// checkDiagrams reports D2 sources whose renderings are missing or stale, and renderings left without
// a source.
func checkDiagrams(root string) []string {
	sources, err := diagramFiles(root, ".d2")
	if err != nil {
		return []string{"diagrams: " + err.Error()}
	}
	var problems []string
	rendered := map[string]bool{}
	for _, src := range sources {
		name := strings.TrimSuffix(filepath.Base(src), ".d2")
		if strings.HasPrefix(name, "_") {
			continue
		}
		dir := filepath.ToSlash(filepath.Dir(src))
		want, err := stampOf(filepath.Join(root, filepath.FromSlash(src)))
		if err != nil {
			problems = append(problems, src+": "+err.Error())
			continue
		}
		for _, mode := range diagramModes {
			svg := dir + "/" + name + "-" + mode + ".svg"
			rendered[svg] = true
			body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(svg)))
			if err != nil {
				problems = append(problems, src+": no "+svg+" (run make diagrams)")
				continue
			}
			if m := sourceStamp.FindSubmatch(body); m == nil {
				problems = append(problems, svg+": no d2-source-sha256 stamp (run make diagrams)")
			} else if string(m[1]) != want {
				problems = append(problems, svg+": rendered from an older "+src+" (run make diagrams)")
			}
			style := dir + "/_style-" + mode + ".d2"
			wantStyle, err := stampOf(filepath.Join(root, filepath.FromSlash(style)))
			if err != nil {
				problems = append(problems, src+": no "+style)
				continue
			}
			if m := styleStamp.FindSubmatch(body); m == nil {
				problems = append(problems, svg+": no d2-style-sha256 stamp (run make diagrams)")
			} else if string(m[1]) != wantStyle {
				problems = append(problems, svg+": rendered with an older "+style+" (run make diagrams)")
			}
		}
	}
	svgs, err := diagramFiles(root, ".svg")
	if err != nil {
		return append(problems, "diagrams: "+err.Error())
	}
	for _, svg := range svgs {
		if rendered[svg] {
			continue
		}
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(svg)))
		if err == nil && sourceStamp.Match(body) {
			problems = append(problems, svg+": its .d2 source is gone (delete it, or restore the source)")
		}
	}
	return problems
}
