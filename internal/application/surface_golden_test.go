package application_test

import (
	"bytes"
	"fmt"
	"go/importer"
	"go/token"
	"go/types"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"masamong/internal/application"
)

// The exported surface of internal/application is what cmd/masamongd, the Discord connector, the web
// layer, the CLI, the tray and two tools compile against (E7 review F13). A split moves code behind
// type aliases and one-line forwarders; this golden file is the check that it all still looks the
// same from outside: every exported name, its kind, its signature, the exported fields of a struct
// (promoted ones included) and the whole method set of every exported type. An alias to a type
// that gained exported methods widens what the connector can call, and shows up here as new lines.
//
// The file is read with go/types from the compiler's export data, so only the exported API is in it,
// aliases are looked through (a type moved behind an alias prints the same), and packages under
// internal/application print without a qualifier for the same reason. Rewrite it, after reading the
// diff, with: go test ./internal/application -run TestExportedSurfaceIsPinned -update

const surfaceGolden = "testdata/exported_surface.golden"
const applicationPrefix = "masamong/internal/application"

func TestExportedSurfaceIsPinned(t *testing.T) {
	got := exportedSurface(t)
	if application.UpdateGoldens() {
		if err := os.WriteFile(surfaceGolden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(surfaceGolden)
	if err != nil {
		t.Fatalf("%v (create it with: go test ./internal/application -run TestExportedSurfaceIsPinned -update)", err)
	}
	if got != string(want) {
		t.Errorf("the exported surface of internal/application changed:\n%s\nIf this is intended, read the diff above and run: go test ./internal/application -run TestExportedSurfaceIsPinned -update", lineDiff(string(want), got))
	}
}

// TestSurfaceRendererSeesPromotedMembersAndAliases checks the renderer on a made-up package, so the
// golden file is not the only evidence that embedded members and aliases are handled.
func TestSurfaceRendererSeesPromotedMembersAndAliases(t *testing.T) {
	pkg := types.NewPackage(applicationPrefix+"/kernel", "kernel")
	str := types.Typ[types.String]
	base := types.NewTypeName(token.NoPos, pkg, "Base", nil)
	baseNamed := types.NewNamed(base, types.NewStruct([]*types.Var{types.NewField(token.NoPos, pkg, "Clock", str, false)}, nil), nil)
	recv := types.NewVar(token.NoPos, pkg, "b", types.NewPointer(baseNamed))
	baseNamed.AddMethod(types.NewFunc(token.NoPos, pkg, "Now", types.NewSignatureType(recv, nil, nil, nil, types.NewTuple(types.NewVar(token.NoPos, pkg, "", str)), false)))
	svc := types.NewTypeName(token.NoPos, pkg, "Service", nil)
	svcNamed := types.NewNamed(svc, types.NewStruct([]*types.Var{
		types.NewField(token.NoPos, pkg, "Base", types.NewPointer(baseNamed), true),
		types.NewField(token.NoPos, pkg, "hidden", str, false),
	}, nil), nil)
	pkg.Scope().Insert(svc)
	pkg.Scope().Insert(base)
	// a package-level alias of Service
	alias := types.NewAlias(types.NewTypeName(token.NoPos, pkg, "Svc", nil), svcNamed)
	pkg.Scope().Insert(alias.Obj())

	got := renderSurface(pkg)
	for _, want := range []string{
		"type Service struct",
		"  field Base *Base",
		"  field Clock string",
		"  method (Service) Now() string", // promoted through the embedded *Base: in both method sets
		"type Svc struct",
		"  field Clock string",
		"  method (Svc) Now() string",
	} {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("surface lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "hidden") {
		t.Errorf("an unexported field leaked into the surface:\n%s", got)
	}
}

// exportedSurface type-checks nothing itself: it loads the compiled package through the export data
// the go command prepares for the test binary's own dependencies.
func exportedSurface(t *testing.T) string {
	t.Helper()
	goBin := filepath.Join(runtime.GOROOT(), "bin", "go")
	out, err := osexec.Command(goBin, "list", "-export", "-deps", "-f", "{{.ImportPath}}\t{{.Export}}", applicationPrefix).Output()
	if err != nil {
		t.Fatalf("go list -export: %v", err)
	}
	files := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if path, file, ok := strings.Cut(line, "\t"); ok && file != "" {
			files[path] = file
		}
	}
	fset := token.NewFileSet()
	imp := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		file, ok := files[path]
		if !ok {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(file)
	})
	pkg, err := imp.Import(applicationPrefix)
	if err != nil {
		t.Fatalf("import %s: %v", applicationPrefix, err)
	}
	return renderSurface(pkg)
}

func qualifier(p *types.Package) string {
	if p.Path() == applicationPrefix || strings.HasPrefix(p.Path(), applicationPrefix+"/") {
		return ""
	}
	return p.Path()
}

func renderSurface(pkg *types.Package) string {
	var b bytes.Buffer
	scope := pkg.Scope()
	for _, name := range scope.Names() {
		obj := scope.Lookup(name)
		if !obj.Exported() {
			continue
		}
		switch obj := obj.(type) {
		case *types.Const:
			fmt.Fprintf(&b, "const %s %s = %s\n", name, types.TypeString(obj.Type(), qualifier), obj.Val().ExactString())
		case *types.Var:
			fmt.Fprintf(&b, "var %s %s\n", name, types.TypeString(obj.Type(), qualifier))
		case *types.Func:
			fmt.Fprintf(&b, "func %s%s\n", name, strings.TrimPrefix(types.TypeString(obj.Type(), qualifier), "func"))
		case *types.TypeName:
			renderType(&b, obj)
		}
	}
	return b.String()
}

func renderType(b *bytes.Buffer, obj *types.TypeName) {
	name := obj.Name()
	typ := types.Unalias(obj.Type())
	switch u := typ.Underlying().(type) {
	case *types.Struct:
		fmt.Fprintf(b, "type %s struct\n", name)
		for _, f := range exportedFields(u, map[*types.Struct]bool{}) {
			fmt.Fprintf(b, "  field %s\n", f)
		}
	case *types.Interface:
		fmt.Fprintf(b, "type %s interface\n", name)
	default:
		fmt.Fprintf(b, "type %s %s\n", name, types.TypeString(u, qualifier))
	}
	if _, isIface := typ.Underlying().(*types.Interface); isIface {
		set := types.NewMethodSet(typ)
		renderMethods(b, set)
		return
	}
	value := types.NewMethodSet(typ)
	ptr := types.NewMethodSet(types.NewPointer(typ))
	var lines []string
	for i := 0; i < ptr.Len(); i++ {
		sel := ptr.At(i)
		if !sel.Obj().Exported() {
			continue
		}
		recv := "(*" + name + ")"
		if value.Lookup(sel.Obj().Pkg(), sel.Obj().Name()) != nil {
			recv = "(" + name + ")"
		}
		lines = append(lines, fmt.Sprintf("  method %s %s%s", recv, sel.Obj().Name(), strings.TrimPrefix(types.TypeString(sel.Type(), qualifier), "func")))
	}
	sort.Strings(lines)
	for _, l := range lines {
		b.WriteString(l + "\n")
	}
}

func renderMethods(b *bytes.Buffer, set *types.MethodSet) {
	var lines []string
	for i := 0; i < set.Len(); i++ {
		sel := set.At(i)
		if sel.Obj().Exported() {
			lines = append(lines, fmt.Sprintf("  method %s%s", sel.Obj().Name(), strings.TrimPrefix(types.TypeString(sel.Type(), qualifier), "func")))
		}
	}
	sort.Strings(lines)
	for _, l := range lines {
		b.WriteString(l + "\n")
	}
}

// exportedFields lists the exported fields of a struct, promoted ones included; a field of a shallower
// level hides one of the same name below it, as in Go.
func exportedFields(s *types.Struct, seen map[*types.Struct]bool) []string {
	depth := map[string]int{}
	text := map[string]string{}
	var walk func(s *types.Struct, level int)
	walk = func(s *types.Struct, level int) {
		if seen[s] && level > 0 {
			return
		}
		seen[s] = true
		for i := 0; i < s.NumFields(); i++ {
			f := s.Field(i)
			if f.Exported() {
				if d, ok := depth[f.Name()]; !ok || level < d {
					depth[f.Name()] = level
					text[f.Name()] = f.Name() + " " + types.TypeString(f.Type(), qualifier)
				}
			}
			if f.Embedded() {
				t := f.Type()
				if p, ok := t.(*types.Pointer); ok {
					t = p.Elem()
				}
				if inner, ok := types.Unalias(t).Underlying().(*types.Struct); ok {
					walk(inner, level+1)
				}
			}
		}
	}
	walk(s, 0)
	out := make([]string, 0, len(text))
	for _, v := range text {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// lineDiff reports the lines only one side has, which is what a reviewer reads.
func lineDiff(want, got string) string {
	w, g := map[string]bool{}, map[string]bool{}
	for _, l := range strings.Split(want, "\n") {
		w[l] = true
	}
	for _, l := range strings.Split(got, "\n") {
		g[l] = true
	}
	var out []string
	for _, l := range strings.Split(want, "\n") {
		if !g[l] {
			out = append(out, "- "+l)
		}
	}
	for _, l := range strings.Split(got, "\n") {
		if !w[l] {
			out = append(out, "+ "+l)
		}
	}
	if len(out) > 80 {
		out = append(out[:80], fmt.Sprintf("... and %d more", len(out)-80))
	}
	return strings.Join(out, "\n")
}
