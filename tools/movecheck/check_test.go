package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const root = "internal/application"
const dest = "internal/application/fortunesvc"

// base is a small package as it is before a feature leaves: a type, a constant, a variable, methods
// that call each other, and a caller in another file.
var baseFiles = Tree{
	root + "/fortune.go": []byte(`package application

// zodiacLimit is how many signs a card shows.
const zodiacLimit = 12

var zodiacNames = []string{"a", "b"}

type fortuneSvc struct {
	Name string ` + "`json:\"name\"`" + `
}

// answers one request.
func (s *fortuneSvc) query(in string) string {
	if in == "" {
		return s.msg("empty")
	}
	return s.msg(in) + s.helper(in)
}

func (s *fortuneSvc) helper(in string) string { return in + "!" }

func newFortune() *fortuneSvc { return &fortuneSvc{} }
`),
	root + "/service.go": []byte(`package application

type Service struct{ n int }

func (s *Service) msg(k string) string { return k }

func (s *Service) tick() string { return s.f.query("x") }
`),
}

func clone(t Tree) Tree {
	c := Tree{}
	for k, v := range t {
		c[k] = append([]byte(nil), v...)
	}
	return c
}

func mustFail(t *testing.T, res Result, want ...string) {
	t.Helper()
	if res.OK() {
		t.Fatalf("expected a failure, got ok (notes %v)", res.Notes)
	}
	all := strings.Join(res.Failures, "\n")
	for _, w := range want {
		if !strings.Contains(all, w) {
			t.Errorf("failures %q lack %q", all, w)
		}
	}
}

func mustPass(t *testing.T, res Result) {
	t.Helper()
	if !res.OK() {
		t.Fatalf("unexpected failures:\n%s", strings.Join(res.Failures, "\n"))
	}
}

// moveHead is base after a pure move: the feature file is now in the destination package, with the
// package clause and the type name changed.
func moveHead() Tree {
	h := clone(baseFiles)
	src := string(h[root+"/fortune.go"])
	delete(h, root+"/fortune.go")
	src = strings.Replace(src, "package application", "package fortunesvc", 1)
	src = strings.ReplaceAll(src, "fortuneSvc", "Service")
	h[dest+"/fortune.go"] = []byte(src)
	return h
}

func moveOpts() Options {
	return Options{Mode: ModeMove, From: root, To: dest, Rename: map[string]string{"fortuneSvc": "Service"}}
}

func TestPureMovePasses(t *testing.T) {
	res := Check(baseFiles, moveHead(), moveOpts())
	mustPass(t, res)
	if !strings.Contains(strings.Join(res.Notes, "\n"), "6 declaration(s) moved") {
		t.Errorf("notes %v: every declaration of fortune.go should count as moved", res.Notes)
	}
}

func TestMoveWithoutRenamePasses(t *testing.T) {
	h := clone(baseFiles)
	src := string(h[root+"/fortune.go"])
	delete(h, root+"/fortune.go")
	h[dest+"/fortune.go"] = []byte(strings.Replace(src, "package application", "package fortunesvc", 1))
	o := moveOpts()
	o.Rename = nil
	mustPass(t, Check(baseFiles, h, o))
}

func TestMoveFailsOnChangedBody(t *testing.T) {
	h := moveHead()
	h[dest+"/fortune.go"] = []byte(strings.Replace(string(h[dest+"/fortune.go"]), `return in + "!"`, `return in + "?"`, 1))
	mustFail(t, Check(baseFiles, h, moveOpts()), "Service.helper", "differs")
}

func TestMoveFailsOnChangedConstTagAndVar(t *testing.T) {
	for _, tc := range []struct{ from, to, want string }{
		{"= 12", "= 13", "zodiacLimit"},
		{`json:"name"`, `json:"n"`, "Service"},
		{`{"a", "b"}`, `{"a", "c"}`, "zodiacNames"},
	} {
		h := moveHead()
		h[dest+"/fortune.go"] = []byte(strings.Replace(string(h[dest+"/fortune.go"]), tc.from, tc.to, 1))
		mustFail(t, Check(baseFiles, h, moveOpts()), tc.want)
	}
}

func TestMoveFailsOnRenamedFunction(t *testing.T) {
	h := moveHead()
	h[dest+"/fortune.go"] = []byte(strings.ReplaceAll(string(h[dest+"/fortune.go"]), "newFortune", "NewFortune"))
	mustFail(t, Check(baseFiles, h, moveOpts()), "newFortune", "no declaration of that name remains")
}

func TestMoveFailsWhenDeclarationIsDropped(t *testing.T) {
	h := moveHead()
	h[dest+"/fortune.go"] = []byte(strings.Replace(string(h[dest+"/fortune.go"]), `func (s *Service) helper(in string) string { return in + "!" }`, "", 1))
	mustFail(t, Check(baseFiles, h, moveOpts()), "Service.helper")
}

func TestMoveFailsWhenACallerChanged(t *testing.T) {
	h := moveHead()
	h[root+"/service.go"] = []byte(strings.Replace(string(h[root+"/service.go"]), `s.f.query("x")`, `s.f.query("y")`, 1))
	mustFail(t, Check(baseFiles, h, moveOpts()), "Service.tick")
	// the same change is fine when the review allows it
	o := moveOpts()
	o.Allow = map[string]bool{"Service.tick": true}
	mustPass(t, Check(baseFiles, h, o))
}

func TestMoveParsesFilesWhateverTheirBuildTags(t *testing.T) {
	b := clone(baseFiles)
	b[root+"/fortune_windows.go"] = []byte("//go:build windows\n\npackage application\n\nfunc winOnly() int { return 1 }\n")
	h := moveHead()
	h[root+"/fortune_windows.go"] = []byte("//go:build windows\n\npackage application\n\nfunc winOnly() int { return 2 }\n")
	mustFail(t, Check(b, h, moveOpts()), "winOnly")
}

func TestMoveFailsWhenFileDoesNotParse(t *testing.T) {
	h := moveHead()
	h[dest+"/broken.go"] = []byte("package fortunesvc\n\nfunc (")
	mustFail(t, Check(baseFiles, h, moveOpts()), "broken.go")
}

func TestMoveIgnoresNewDeclarationsInBothPackages(t *testing.T) {
	h := moveHead()
	h[dest+"/aliases.go"] = []byte("package fortunesvc\n\ntype Alias = int\n")
	h[root+"/hosts_fortune.go"] = []byte("package application\n\ntype fortuneHost struct{ s *Service }\n\nfunc (h fortuneHost) Msg(k string) string { return h.s.msg(k) }\n")
	mustPass(t, Check(baseFiles, h, moveOpts()))
}

func TestMoveNeedsDestination(t *testing.T) {
	mustFail(t, Check(baseFiles, baseFiles, Options{Mode: ModeMove, From: root}), "-to")
}

// rehomeBase is a package before commit A: the feature methods are on Service.
func rehomeBase() Tree {
	return Tree{
		root + "/fortune.go": []byte(`package application

func (s *Service) query(in string) string {
	return s.msg(in) + s.helper(in)
}

func (s *Service) helper(in string) string { return in + "!" }
`),
		root + "/service.go": []byte(`package application

type Service struct{ n int }

func (s *Service) msg(k string) string { return k }

func (s *Service) tick() string { return s.query("x") }
`),
	}
}

// rehomeHead is rehomeBase after commit A: the feature methods take a feature receiver, a shim
// forwards the call they made on Service, and the caller reaches the feature through a field.
func rehomeHead() Tree {
	h := rehomeBase()
	h[root+"/fortune.go"] = []byte(strings.ReplaceAll(string(h[root+"/fortune.go"]), "*Service", "*fortuneSvc"))
	h[root+"/fortune_shims.go"] = []byte("package application\n\nfunc (s *fortuneSvc) msg(k string) string { return s.core.Msg(k) }\n")
	h[root+"/service.go"] = []byte(strings.Replace(string(h[root+"/service.go"]), `s.query("x")`, `s.fortune.query("x")`, 1))
	return h
}

func rehomeOpts() Options {
	return Options{Mode: ModeRehome, From: root, Feature: "fortune", FromType: "Service", ToType: "fortuneSvc"}
}

func TestRehomePasses(t *testing.T) {
	res := Check(rehomeBase(), rehomeHead(), rehomeOpts())
	mustPass(t, res)
	if !strings.Contains(strings.Join(res.Notes, "\n"), "1 new declaration") {
		t.Errorf("notes %v: the shim is the one new declaration", res.Notes)
	}
}

func TestRehomeFailsOnChangedBody(t *testing.T) {
	h := rehomeHead()
	h[root+"/fortune.go"] = []byte(strings.Replace(string(h[root+"/fortune.go"]), `in + "!"`, `in + "?"`, 1))
	mustFail(t, Check(rehomeBase(), h, rehomeOpts()), "Service.helper")
}

func TestRehomeFailsOnOtherCallerChange(t *testing.T) {
	h := rehomeHead()
	h[root+"/service.go"] = []byte(strings.Replace(string(h[root+"/service.go"]), `s.fortune.query("x")`, `s.fortune.query("y")`, 1))
	mustFail(t, Check(rehomeBase(), h, rehomeOpts()), "Service.tick")
}

func TestRehomeFailsWhenPathUsesAnotherField(t *testing.T) {
	// s.other.query is not the feature field, so it is a real change
	h := rehomeHead()
	h[root+"/service.go"] = []byte(strings.Replace(string(h[root+"/service.go"]), `s.fortune.query("x")`, `s.other.query("x")`, 1))
	mustFail(t, Check(rehomeBase(), h, rehomeOpts()), "Service.tick")
}

func TestRehomeFailsWhenAMethodIsDropped(t *testing.T) {
	h := rehomeHead()
	h[root+"/fortune.go"] = []byte("package application\n\nfunc (s *fortuneSvc) query(in string) string {\n\treturn s.msg(in) + s.helper(in)\n}\n")
	mustFail(t, Check(rehomeBase(), h, rehomeOpts()), "Service.helper")
}

func TestRehomeAllowsNamedChangesOnly(t *testing.T) {
	h := rehomeHead()
	h[root+"/service.go"] = []byte(strings.Replace(string(h[root+"/service.go"]), "struct{ n int }", "struct {\n\tn       int\n\tfortune *fortuneSvc\n}", 1))
	mustFail(t, Check(rehomeBase(), h, rehomeOpts()), "Service ")
	o := rehomeOpts()
	o.Allow = map[string]bool{"Service": true}
	res := Check(rehomeBase(), h, o)
	mustPass(t, res)
}

func TestRehomeFailsOnShims(t *testing.T) {
	for name, shim := range map[string]string{
		"two statements":     "func (s *fortuneSvc) msg(k string) string { s.n++; return s.core.Msg(k) }",
		"another method":     "func (s *fortuneSvc) msg(k string) string { return s.core.Other(k) }",
		"parameters swapped": "func (s *fortuneSvc) msg(a, b string) string { return s.core.Msg(b, a) }",
		"extra argument":     `func (s *fortuneSvc) msg(k string) string { return s.core.Msg(k, "x") }`,
		"dropped argument":   "func (s *fortuneSvc) msg(a, b string) string { return s.core.Msg(a) }",
		"computed argument":  "func (s *fortuneSvc) msg(k string) string { return s.core.Msg(k + \"!\") }",
		"wrong root":         "func (s *fortuneSvc) msg(k string) string { return other.core.Msg(k) }",
		"not a call":         "func (s *fortuneSvc) msg(k string) string { return k }",
		"plain function":     "func msg(k string) string { return core.Msg(k) }",
		"no return":          "func (s *fortuneSvc) msg(k string) string { s.core.Msg(k) }",
		"variadic":           "func (s *fortuneSvc) msg(k string, a ...any) string { return s.core.Msg(k, a) }",
	} {
		h := rehomeHead()
		h[root+"/fortune_shims.go"] = []byte("package application\n\n" + shim + "\n")
		if res := Check(rehomeBase(), h, rehomeOpts()); res.OK() {
			t.Errorf("%s: a non-forwarding shim passed", name)
		}
	}
}

func TestShimRuleAcceptsForwarders(t *testing.T) {
	for name, shim := range map[string]string{
		"same name":       "func (s *fortuneSvc) msg(k string) string { return s.core.msg(k) }",
		"exported twin":   "func (s *fortuneSvc) msg(k string) string { return s.core.Msg(k) }",
		"lowercase twin":  "func (s *fortuneSvc) Msg(k string) string { return s.core.msg(k) }",
		"field path":      "func (s *fortuneSvc) msg(k string) string { return s.a.b.Msg(k) }",
		"no results":      "func (s *fortuneSvc) log(k string) { s.core.Log(k) }",
		"several results": "func (s *fortuneSvc) get(k string) (int, error) { return s.core.Get(k) }",
		"two params":      "func (s *fortuneSvc) put(a string, b int) { s.core.Put(a, b) }",
		"grouped params":  "func (s *fortuneSvc) put(a, b string) { s.core.Put(a, b) }",
		"variadic":        "func (s *fortuneSvc) msg(k string, a ...any) string { return s.core.Msg(k, a...) }",
		"no params":       "func (s *fortuneSvc) now() int { return s.core.Now() }",
	} {
		h := rehomeHead()
		h[root+"/fortune_shims.go"] = []byte("package application\n\n" + shim + "\n")
		res := Check(rehomeBase(), h, rehomeOpts())
		for _, f := range res.Failures {
			if strings.Contains(f, "not a forwarder") {
				t.Errorf("%s: %s", name, f)
			}
		}
	}
}

func TestHostsFileIsCheckedToo(t *testing.T) {
	h := rehomeHead()
	h[root+"/hosts_fortune.go"] = []byte("package application\n\ntype fortuneHost struct{ s *Service }\n\nfunc (h fortuneHost) Msg(k string) string { return h.s.tick() }\n")
	mustFail(t, Check(rehomeBase(), h, rehomeOpts()), "fortuneHost.Msg")
}

func TestShimFileHoldsNoVariablesOrFunctions(t *testing.T) {
	h := rehomeHead()
	h[root+"/fortune_shims.go"] = []byte("package application\n\nvar cache = map[string]int{}\n\nfunc helper() {}\n")
	mustFail(t, Check(rehomeBase(), h, rehomeOpts()), "cache", "helper")
}

func TestRenamePassesAndFailsOnOtherChanges(t *testing.T) {
	b := clone(baseFiles)
	h := clone(baseFiles)
	// export the member the rest of the package uses: query becomes Query
	h[root+"/fortune.go"] = []byte(strings.ReplaceAll(string(h[root+"/fortune.go"]), "query", "Query"))
	h[root+"/service.go"] = []byte(strings.ReplaceAll(string(h[root+"/service.go"]), "query", "Query"))
	o := Options{Mode: ModeRename, From: root, Rename: map[string]string{"query": "Query"}}
	mustPass(t, Check(b, h, o))

	h[root+"/fortune.go"] = []byte(strings.Replace(string(h[root+"/fortune.go"]), `s.msg("empty")`, `s.msg("none")`, 1))
	mustFail(t, Check(b, h, o), "fortuneSvc.Query")

	h2 := clone(h)
	h2[root+"/extra.go"] = []byte("package application\n\nfunc extra() {}\n")
	mustFail(t, Check(b, h2, o), "extra")
}

func TestRenameFailsWhenNotDeclared(t *testing.T) {
	h := clone(baseFiles)
	h[root+"/service.go"] = []byte(strings.ReplaceAll(string(h[root+"/service.go"]), "tick", "Tick"))
	mustFail(t, Check(baseFiles, h, Options{Mode: ModeRename, From: root}), "Service.tick")
}

func TestCommentsAreComparedUnlessIgnored(t *testing.T) {
	h := moveHead()
	h[dest+"/fortune.go"] = []byte(strings.Replace(string(h[dest+"/fortune.go"]), "// answers one request.", "// answers a request.", 1))
	mustFail(t, Check(baseFiles, h, moveOpts()), "Service.query")
	o := moveOpts()
	o.IgnoreComments = true
	mustPass(t, Check(baseFiles, h, o))
}

func TestUnknownMode(t *testing.T) {
	mustFail(t, Check(baseFiles, baseFiles, Options{Mode: "X"}), "unknown mode")
}

func TestInitFunctionsPairAsAMultiset(t *testing.T) {
	b := Tree{root + "/a.go": []byte("package application\n\nfunc init() { one() }\n\nfunc init() { two() }\n")}
	h := Tree{
		root + "/a.go": []byte("package application\n\nfunc init() { one() }\n"),
		dest + "/b.go": []byte("package fortunesvc\n\nfunc init() { two() }\n"),
	}
	o := Options{Mode: ModeMove, From: root, To: dest}
	mustPass(t, Check(b, h, o))
	h[dest+"/b.go"] = []byte("package fortunesvc\n\nfunc init() { three() }\n")
	mustFail(t, Check(b, h, o), "init")
}

// TestCommandOnAGitRepository runs the tool the way a reviewer does: two commits in a repository.
func TestCommandOnAGitRepository(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(tree Tree) {
		t.Helper()
		for p, b := range tree {
			full := filepath.Join(dir, filepath.FromSlash(p))
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	git("init", "-q")
	write(baseFiles)
	git("add", "-A")
	git("commit", "-q", "-m", "base")
	first := git("rev-parse", "HEAD")

	head := moveHead()
	if err := os.Remove(filepath.Join(dir, filepath.FromSlash(root+"/fortune.go"))); err != nil {
		t.Fatal(err)
	}
	write(head)
	git("add", "-A")
	git("commit", "-q", "-m", "move")
	t.Chdir(dir)

	args := []string{"-mode", "B", "-base", first, "-head", "HEAD", "-to", dest, "-rename", "fortuneSvc=Service"}
	var out, errb bytes.Buffer
	if code := run(args, &out, &errb); code != 0 {
		t.Fatalf("pure move: exit %d\n%s%s", code, out.String(), errb.String())
	}

	// a change outside the allowed paths fails even when the move is pure
	write(Tree{"cmd/other/main.go": []byte("package main\n")})
	git("add", "-A")
	git("commit", "-q", "-m", "stray")
	out.Reset()
	if code := run(args, &out, &errb); code != 1 || !strings.Contains(out.String(), "cmd/other/main.go") {
		t.Fatalf("stray file: exit %d\n%s", code, out.String())
	}

	// a changed body fails
	write(Tree{dest + "/fortune.go": []byte(strings.Replace(string(head[dest+"/fortune.go"]), `in + "!"`, `in + "?"`, 1))})
	git("add", "-A")
	git("commit", "-q", "-m", "edit")
	out.Reset()
	if code := run(append(args, "-only", ""), &out, &errb); code != 1 || !strings.Contains(out.String(), "Service.helper") {
		t.Fatalf("changed body: exit %d\n%s", code, out.String())
	}
}
