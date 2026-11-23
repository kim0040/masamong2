package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"path"
	"sort"
	"strings"
)

// Mode is the kind of commit being checked (E7 review F3).
type Mode string

const (
	// ModeRename: members are renamed or exported inside one package. Every declaration is the same
	// after the declared rename map is applied.
	ModeRename Mode = "R"
	// ModeRehome: receiver and parameter types change inside one package, and the only other body
	// change is s.X becoming s.<feature>.X. New declarations are allowed; forwarding files are checked.
	ModeRehome Mode = "A"
	// ModeMove: every top-level declaration that left the package is identical in the target.
	ModeMove Mode = "B"
)

// Options says what a commit is allowed to do.
type Options struct {
	Mode Mode
	// From is the package directory whose declarations are compared (slash path, not recursive).
	From string
	// To is the destination directory of a move (mode B).
	To string
	// Rename maps identifiers of the base side to the head side (modes R and B).
	Rename map[string]string
	// Feature is the Service field a re-homed feature lives in (mode A): s.X in a caller becomes s.<Feature>.X.
	Feature string
	// FromType and ToType are the receiver type before and after a re-home (mode A): Service and fortuneSvc.
	FromType, ToType string
	// Allow lists declarations whose change or removal is expected (keys as printed in a failure: "Name" or "Recv.Name").
	Allow map[string]bool
	// ShimGlobs name the files that hold only forwarders. Default: *_shims.go and hosts_*.go.
	ShimGlobs []string
	// IgnoreComments leaves comments out of the comparison.
	IgnoreComments bool
}

// Tree is a set of files by slash path. Contents are Go source for the files this tool reads.
type Tree map[string][]byte

// Result is what a check found.
type Result struct {
	Failures []string
	// Notes are facts a reviewer should see but that do not fail the check.
	Notes []string
}

// OK reports whether the commit passed.
func (r Result) OK() bool { return len(r.Failures) == 0 }

type decl struct {
	key  string // "Name" or "Recv.Name"; the identity used to pair base and head
	orig string // key before the A-mode type rename, kept to tell a shim from the original
	text string
	file string
	fd   *ast.FuncDecl
	fset *token.FileSet
}

var defaultShimGlobs = []string{"*_shims.go", "hosts_*.go"}

// Check compares base and head. Every .go file of a directory is parsed, whatever its build tags.
func Check(base, head Tree, o Options) Result {
	var r Result
	if o.From == "" {
		o.From = "internal/application"
	}
	if len(o.ShimGlobs) == 0 {
		o.ShimGlobs = defaultShimGlobs
	}
	switch o.Mode {
	case ModeRename, ModeRehome, ModeMove:
	default:
		r.Failures = append(r.Failures, fmt.Sprintf("unknown mode %q (want R, A or B)", o.Mode))
		return r
	}
	if o.Mode == ModeMove && o.To == "" {
		r.Failures = append(r.Failures, "mode B needs the destination directory (-to)")
		return r
	}
	if o.Mode == ModeRehome && (o.Feature == "" || o.FromType == "" || o.ToType == "") {
		r.Failures = append(r.Failures, "mode A needs -feature and -retype Old=New")
		return r
	}

	baseDecls, err := collect(base, o.From, func(n ast.Node) { applyRename(n, o.Rename) }, o)
	if err != nil {
		r.Failures = append(r.Failures, err.Error())
		return r
	}
	headXform := func(n ast.Node) {}
	if o.Mode == ModeRehome {
		headXform = func(n ast.Node) {
			collapseFeature(n, o.Feature)
			applyRename(n, map[string]string{o.ToType: o.FromType})
		}
	}
	headDecls, err := collect(head, o.From, headXform, o)
	if err != nil {
		r.Failures = append(r.Failures, err.Error())
		return r
	}
	var destDecls []decl
	if o.Mode == ModeMove {
		destDecls, err = collect(head, o.To, func(ast.Node) {}, o)
		if err != nil {
			r.Failures = append(r.Failures, err.Error())
			return r
		}
	}

	// A-mode keys follow the type rename so the re-homed method pairs with its original.
	if o.Mode == ModeRehome {
		for i := range headDecls {
			headDecls[i].key = renameKeyRecv(headDecls[i].key, o.ToType, o.FromType)
		}
	}

	missing, extraHead := pair(baseDecls, headDecls)
	switch o.Mode {
	case ModeRename:
		for _, d := range missing {
			if !o.Allow[d.key] {
				r.Failures = append(r.Failures, diagnose("changed or removed", d, headDecls))
			}
		}
		for _, d := range extraHead {
			if !o.Allow[d.key] {
				r.Failures = append(r.Failures, fmt.Sprintf("%s (%s): added; a rename commit adds nothing", d.key, d.file))
			}
		}
	case ModeRehome:
		for _, d := range missing {
			if o.Allow[d.key] {
				r.Notes = append(r.Notes, fmt.Sprintf("allowed change: %s (%s)", d.key, d.file))
				continue
			}
			r.Failures = append(r.Failures, diagnose("changed or removed (only the receiver/parameter type and s.X to s."+o.Feature+".X may change)", d, headDecls))
		}
		r.Notes = append(r.Notes, fmt.Sprintf("%d new declaration(s) in %s", len(extraHead), o.From))
	case ModeMove:
		stillMissing, extraDest := pair(missing, destDecls)
		// a declaration that stayed behind but changed is "changed in place", not "moved"
		for _, d := range stillMissing {
			if o.Allow[d.key] {
				r.Notes = append(r.Notes, fmt.Sprintf("allowed change: %s (%s)", d.key, d.file))
				continue
			}
			r.Failures = append(r.Failures, diagnose("left "+o.From+" but is not identical in "+o.To, d, destDecls, extraHead))
		}
		moved := len(missing) - len(stillMissing)
		r.Notes = append(r.Notes, fmt.Sprintf("%d declaration(s) moved identically to %s; %d new in %s; %d new in %s",
			moved, o.To, len(extraDest), o.To, len(extraHead), o.From))
	}

	// forwarding files hold only forwarders, in every mode
	dirs := []string{o.From}
	if o.Mode == ModeMove {
		dirs = append(dirs, o.To)
	}
	for _, dir := range dirs {
		shimDecls, err := collectShims(head, dir, o)
		if err != nil {
			r.Failures = append(r.Failures, err.Error())
			continue
		}
		r.Failures = append(r.Failures, shimDecls...)
	}
	sort.Strings(r.Failures)
	return r
}

// pair matches base and head declarations of the same key and exact text, one to one, and returns
// the base declarations with no partner and the head declarations with no partner.
func pair(base, head []decl) (missing, extra []decl) {
	byKey := map[string][]int{}
	for i, d := range head {
		byKey[d.key] = append(byKey[d.key], i)
	}
	used := make([]bool, len(head))
	for _, b := range base {
		found := false
		for _, i := range byKey[b.key] {
			if !used[i] && head[i].text == b.text {
				used[i], found = true, true
				break
			}
		}
		if !found {
			missing = append(missing, b)
		}
	}
	for i, h := range head {
		if !used[i] {
			extra = append(extra, h)
		}
	}
	return missing, extra
}

// diagnose names a base declaration that has no identical partner and says whether a declaration of
// the same key exists elsewhere (changed) or not (removed or renamed).
func diagnose(what string, d decl, pools ...[]decl) string {
	for _, pool := range pools {
		for _, h := range pool {
			if h.key == d.key {
				return fmt.Sprintf("%s (%s): %s: a declaration of that name exists but differs (%s)", d.key, d.file, what, firstDiff(d.text, h.text))
			}
		}
	}
	return fmt.Sprintf("%s (%s): %s: no declaration of that name remains (renamed or dropped?)", d.key, d.file, what)
}

// firstDiff describes the first line where two declaration texts differ.
func firstDiff(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < len(al) || i < len(bl); i++ {
		var x, y string
		if i < len(al) {
			x = al[i]
		}
		if i < len(bl) {
			y = bl[i]
		}
		if x != y {
			return fmt.Sprintf("line %d: %q -> %q", i+1, strings.TrimSpace(x), strings.TrimSpace(y))
		}
	}
	return "identical"
}

func renameKeyRecv(key, from, to string) string {
	if recv, name, ok := strings.Cut(key, "."); ok && recv == from {
		return to + "." + name
	}
	return key
}

// collect parses every .go file directly inside dir and returns its declarations, after xform.
func collect(t Tree, dir string, xform func(ast.Node), o Options) ([]decl, error) {
	var out []decl
	for _, name := range filesIn(t, dir) {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, name, t[name], parser.ParseComments)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		for _, d := range f.Decls {
			out = append(out, declsOf(fset, f, name, d, xform, o.IgnoreComments)...)
		}
	}
	return out, nil
}

func filesIn(t Tree, dir string) []string {
	var names []string
	for p := range t {
		if strings.HasSuffix(p, ".go") && path.Dir(p) == strings.TrimSuffix(dir, "/") {
			names = append(names, p)
		}
	}
	sort.Strings(names)
	return names
}

func declsOf(fset *token.FileSet, f *ast.File, file string, d ast.Decl, xform func(ast.Node), noComments bool) []decl {
	switch d := d.(type) {
	case *ast.FuncDecl:
		start := d.Pos()
		if d.Doc != nil {
			start = d.Doc.Pos()
		}
		if noComments {
			d.Doc = nil
		}
		xform(d)
		key := d.Name.Name
		if d.Recv != nil && len(d.Recv.List) == 1 {
			key = recvName(d.Recv.List[0].Type) + "." + key
		}
		return []decl{{key: key, orig: key, file: file, fd: d, fset: fset, text: render(fset, f, d, start, d.End(), noComments)}}
	case *ast.GenDecl:
		if d.Tok == token.IMPORT {
			return nil
		}
		whole := !d.Lparen.IsValid() || iotaGroup(d)
		if whole {
			start := d.Pos()
			if d.Doc != nil {
				start = d.Doc.Pos()
			}
			if noComments {
				d.Doc = nil
				for _, s := range d.Specs {
					stripSpecComments(s)
				}
			}
			xform(d)
			return []decl{{key: specNames(d.Specs), orig: specNames(d.Specs), file: file, fset: fset, text: render(fset, f, d, start, d.End(), noComments)}}
		}
		var out []decl
		for _, s := range d.Specs {
			start, end := s.Pos(), s.End()
			switch s := s.(type) {
			case *ast.ValueSpec:
				if s.Doc != nil {
					start = s.Doc.Pos()
				}
				if s.Comment != nil {
					end = s.Comment.End()
				}
			case *ast.TypeSpec:
				if s.Doc != nil {
					start = s.Doc.Pos()
				}
				if s.Comment != nil {
					end = s.Comment.End()
				}
			}
			if noComments {
				stripSpecComments(s)
			}
			xform(s)
			one := &ast.GenDecl{Tok: d.Tok, Specs: []ast.Spec{s}}
			out = append(out, decl{key: specNames([]ast.Spec{s}), orig: specNames([]ast.Spec{s}), file: file, fset: fset, text: render(fset, f, one, start, end, noComments)})
		}
		return out
	}
	return nil
}

func stripSpecComments(s ast.Spec) {
	switch s := s.(type) {
	case *ast.ValueSpec:
		s.Doc, s.Comment = nil, nil
	case *ast.TypeSpec:
		s.Doc, s.Comment = nil, nil
	}
}

// iotaGroup reports a const group whose specs depend on their neighbours (a spec with no value).
func iotaGroup(d *ast.GenDecl) bool {
	if d.Tok != token.CONST {
		return false
	}
	for _, s := range d.Specs {
		if vs, ok := s.(*ast.ValueSpec); ok && len(vs.Values) == 0 {
			return true
		}
	}
	return false
}

func specNames(specs []ast.Spec) string {
	var names []string
	for _, s := range specs {
		switch s := s.(type) {
		case *ast.ValueSpec:
			for _, n := range s.Names {
				names = append(names, n.Name)
			}
		case *ast.TypeSpec:
			names = append(names, s.Name.Name)
		}
	}
	return strings.Join(names, ",")
}

func recvName(e ast.Expr) string {
	for {
		switch t := e.(type) {
		case *ast.StarExpr:
			e = t.X
		case *ast.ParenExpr:
			e = t.X
		case *ast.IndexExpr:
			e = t.X
		case *ast.IndexListExpr:
			e = t.X
		case *ast.Ident:
			return t.Name
		default:
			return "?"
		}
	}
}

func render(fset *token.FileSet, f *ast.File, n ast.Node, start, end token.Pos, noComments bool) string {
	var target any = n
	if !noComments {
		var cgs []*ast.CommentGroup
		for _, cg := range f.Comments {
			if cg.Pos() >= start && cg.End() <= end {
				cgs = append(cgs, cg)
			}
		}
		target = &printer.CommentedNode{Node: n, Comments: cgs}
	}
	var buf bytes.Buffer
	cfg := printer.Config{Mode: printer.UseSpaces | printer.TabIndent, Tabwidth: 8}
	if err := cfg.Fprint(&buf, fset, target); err != nil {
		return "unprintable: " + err.Error()
	}
	return buf.String()
}

// applyRename renames identifiers in place.
func applyRename(n ast.Node, m map[string]string) {
	if len(m) == 0 {
		return
	}
	ast.Inspect(n, func(x ast.Node) bool {
		if id, ok := x.(*ast.Ident); ok {
			if to, ok := m[id.Name]; ok {
				id.Name = to
			}
		}
		return true
	})
}

// collapseFeature turns recv.feature.X into recv.X, the one body change a re-home allows.
func collapseFeature(n ast.Node, feature string) {
	ast.Inspect(n, func(x ast.Node) bool {
		sel, ok := x.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if inner, ok := sel.X.(*ast.SelectorExpr); ok && inner.Sel.Name == feature {
			if _, ok := inner.X.(*ast.Ident); ok {
				sel.X = inner.X
			}
		}
		return true
	})
}

// collectShims checks every file of dir that matches a shim glob: only type declarations and
// methods that forward to the method of the same name (first letter in either case) with their own
// parameters in order.
func collectShims(t Tree, dir string, o Options) ([]string, error) {
	var fails []string
	for _, name := range filesIn(t, dir) {
		if !matchesAny(path.Base(name), o.ShimGlobs) {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, name, t[name], parser.ParseComments)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.GenDecl:
				if d.Tok != token.IMPORT && d.Tok != token.TYPE {
					fails = append(fails, fmt.Sprintf("%s (%s): a forwarding file holds only types and forwarders, not a %s", specNames(d.Specs), name, d.Tok))
				}
			case *ast.FuncDecl:
				if msg := shimProblem(d); msg != "" {
					fails = append(fails, fmt.Sprintf("%s (%s): not a forwarder: %s", funcKey(d), name, msg))
				}
			}
		}
	}
	return fails, nil
}

func funcKey(d *ast.FuncDecl) string {
	if d.Recv != nil && len(d.Recv.List) == 1 {
		return recvName(d.Recv.List[0].Type) + "." + d.Name.Name
	}
	return d.Name.Name
}

func matchesAny(base string, globs []string) bool {
	for _, g := range globs {
		if ok, _ := path.Match(g, base); ok {
			return true
		}
	}
	return false
}

// shimProblem returns why d is not a plain forwarder, or "".
func shimProblem(d *ast.FuncDecl) string {
	if d.Recv == nil || len(d.Recv.List) != 1 || len(d.Recv.List[0].Names) != 1 {
		return "only methods with a named receiver may live here"
	}
	recv := d.Recv.List[0].Names[0].Name
	if d.Body == nil || len(d.Body.List) != 1 {
		return "the body must be one statement"
	}
	var call *ast.CallExpr
	hasResult := d.Type.Results != nil && len(d.Type.Results.List) > 0
	switch s := d.Body.List[0].(type) {
	case *ast.ReturnStmt:
		if !hasResult || len(s.Results) != 1 {
			return "want `return x.y.Name(params...)`"
		}
		call, _ = s.Results[0].(*ast.CallExpr)
	case *ast.ExprStmt:
		if hasResult {
			return "a method with results must return the call"
		}
		call, _ = s.X.(*ast.CallExpr)
	}
	if call == nil {
		return "the statement is not a call"
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "the call is not a method call"
	}
	if sel.Sel.Name != d.Name.Name && sel.Sel.Name != toggleFirst(d.Name.Name) {
		return fmt.Sprintf("forwards to %s, not %s or %s", sel.Sel.Name, d.Name.Name, toggleFirst(d.Name.Name))
	}
	root := sel.X
	for {
		switch x := root.(type) {
		case *ast.SelectorExpr:
			root = x.X
			continue
		case *ast.Ident:
			if x.Name != recv {
				return fmt.Sprintf("the call starts at %s, not the receiver %s", x.Name, recv)
			}
		default:
			return "the call target is not a field path of the receiver"
		}
		break
	}
	var params []*ast.Field
	for _, f := range d.Type.Params.List {
		if len(f.Names) == 0 {
			return "a parameter has no name, so it cannot be forwarded"
		}
		params = append(params, f)
	}
	var want []string
	variadic := false
	for _, f := range params {
		for _, n := range f.Names {
			if n.Name == "_" {
				return "a parameter is `_`, so it cannot be forwarded"
			}
			want = append(want, n.Name)
		}
		if _, ok := f.Type.(*ast.Ellipsis); ok {
			variadic = true
		}
	}
	if len(call.Args) != len(want) {
		return fmt.Sprintf("passes %d argument(s), has %d parameter(s)", len(call.Args), len(want))
	}
	for i, a := range call.Args {
		id, ok := a.(*ast.Ident)
		if !ok || id.Name != want[i] {
			return fmt.Sprintf("argument %d is not the parameter %s, passed in order", i+1, want[i])
		}
	}
	if variadic != call.Ellipsis.IsValid() {
		return "a variadic parameter must be passed as name..."
	}
	return ""
}

func toggleFirst(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	if r[0] >= 'a' && r[0] <= 'z' {
		r[0] -= 'a' - 'A'
	} else if r[0] >= 'A' && r[0] <= 'Z' {
		r[0] += 'a' - 'A'
	}
	return string(r)
}
