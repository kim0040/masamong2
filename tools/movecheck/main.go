// movecheck proves that a commit which moves Go code only moves it (E7 design, review F3 and F9).
//
// A split of internal/application into packages is reviewable only if the reviewer can trust that a
// move changes nothing else. The commits come in three kinds, each with its own mode:
//
//	R  rename: members are exported or renamed inside the package. Every declaration is the same
//	   after the declared rename map (-rename old=new,...) is applied.
//	A  re-home: receiver and parameter types change (-retype Service=fortuneSvc); in other
//	   declarations the only body change is s.X becoming s.<feature>.X (-feature fortune). New
//	   declarations are listed, and the forwarding files (*_shims.go, hosts_*.go) must hold only
//	   methods that forward to the method of the same name with their own parameters in order.
//	B  move: every top-level declaration that left the package (-from, default internal/application)
//	   is identical in the destination (-to): functions, methods, types with their fields and tags,
//	   constants and variables. -rename maps a type renamed by the move (fortuneSvc=Service).
//
// Every .go file is parsed whatever its build tags, so a windows-only file cannot hide a change.
// A declaration whose change is expected is named with -allow (for example Service).
//
//	go run ./tools/movecheck -mode B -base HEAD~1 -head HEAD -to internal/application/fortunesvc \
//	    -rename fortuneSvc=Service
package main

import (
	"archive/tar"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"strings"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("movecheck", flag.ContinueOnError)
	fs.SetOutput(stderr)
	mode := fs.String("mode", "", "R (rename), A (re-home) or B (move)")
	base := fs.String("base", "", "the commit before the change (a git revision)")
	head := fs.String("head", "HEAD", "the commit after the change")
	from := fs.String("from", "internal/application", "the package directory the code leaves or changes in")
	to := fs.String("to", "", "mode B: the directory the code moves to")
	rename := fs.String("rename", "", "old=new,old2=new2: identifiers renamed by the commit")
	retype := fs.String("retype", "", "mode A: Old=New, the receiver type before and after (Service=fortuneSvc)")
	feature := fs.String("feature", "", "mode A: the Service field of the feature (s.X becomes s.<feature>.X)")
	allow := fs.String("allow", "", "comma list of declarations whose change is expected (Name or Recv.Name)")
	noComments := fs.Bool("ignore-comments", false, "leave comments out of the comparison")
	outside := fs.String("only", "internal/application,tools/codemap,docs/code-map,tools/movecheck", "comma list of paths the commit may touch; empty skips the check")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *base == "" || *mode == "" {
		fmt.Fprintln(stderr, "movecheck: -mode and -base are required")
		fs.Usage()
		return 2
	}
	o := Options{Mode: Mode(*mode), From: *from, To: *to, Feature: *feature, IgnoreComments: *noComments,
		Rename: parsePairs(*rename), Allow: map[string]bool{}}
	for k, v := range parsePairs(*retype) {
		o.FromType, o.ToType = k, v
	}
	for _, a := range strings.Split(*allow, ",") {
		if a = strings.TrimSpace(a); a != "" {
			o.Allow[a] = true
		}
	}
	dirs := []string{*from}
	if *to != "" {
		dirs = append(dirs, *to)
	}
	baseTree, err := gitTree(*base, dirs)
	if err != nil {
		fmt.Fprintln(stderr, "movecheck:", err)
		return 2
	}
	headTree, err := gitTree(*head, dirs)
	if err != nil {
		fmt.Fprintln(stderr, "movecheck:", err)
		return 2
	}
	res := Check(baseTree, headTree, o)
	if *outside != "" {
		res.Failures = append(res.Failures, outsideChanges(*base, *head, strings.Split(*outside, ","))...)
	}
	for _, n := range res.Notes {
		fmt.Fprintln(stdout, "note:", n)
	}
	for _, f := range res.Failures {
		fmt.Fprintln(stdout, "FAIL:", f)
	}
	if !res.OK() {
		fmt.Fprintf(stdout, "movecheck: %d problem(s)\n", len(res.Failures))
		return 1
	}
	fmt.Fprintln(stdout, "movecheck: ok")
	return 0
}

func parsePairs(s string) map[string]string {
	m := map[string]string{}
	for _, p := range strings.Split(s, ",") {
		if k, v, ok := strings.Cut(strings.TrimSpace(p), "="); ok && k != "" && v != "" {
			m[k] = v
		}
	}
	return m
}

// gitTree reads the .go files directly inside each dir at a revision. A directory that does not exist
// there is empty (the destination of a move does not exist before it).
func gitTree(rev string, dirs []string) (Tree, error) {
	t := Tree{}
	for _, dir := range dirs {
		cmd := exec.Command("git", "archive", "--format=tar", rev, "--", dir)
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			if strings.Contains(errb.String(), "did not match") {
				continue
			}
			return nil, fmt.Errorf("git archive %s %s: %v: %s", rev, dir, err, strings.TrimSpace(errb.String()))
		}
		tr := tar.NewReader(&out)
		for {
			h, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, err
			}
			if h.Typeflag != tar.TypeReg || !strings.HasSuffix(h.Name, ".go") || path.Dir(h.Name) != strings.TrimSuffix(dir, "/") {
				continue
			}
			b, err := io.ReadAll(tr)
			if err != nil {
				return nil, err
			}
			t[h.Name] = b
		}
	}
	return t, nil
}

// outsideChanges lists files the commit range changed outside the allowed paths.
func outsideChanges(base, head string, allowed []string) []string {
	out, err := exec.Command("git", "diff", "--name-only", base, head).Output()
	if err != nil {
		return []string{"git diff --name-only: " + err.Error()}
	}
	var fails []string
	for _, f := range strings.Fields(string(out)) {
		ok := false
		for _, a := range allowed {
			if a = strings.TrimSpace(a); a != "" && (f == a || strings.HasPrefix(f, strings.TrimSuffix(a, "/")+"/")) {
				ok = true
			}
		}
		if !ok {
			fails = append(fails, fmt.Sprintf("%s: changed outside the paths a move may touch", f))
		}
	}
	return fails
}
