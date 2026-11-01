package application

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Every top-level folder or file the product creates under a data root must be classified in
// dataRootEntries (erased by a reset, or kept on purpose). The sources are scanned for
// filepath.Join(<data root>, "<name>", ...), so a new folder that nobody thought about fails here
// instead of silently surviving "erase everything" (rag/ held the attached documents of channels).
func TestEveryDataRootEntryIsClassified(t *testing.T) {
	rootLike := map[string]bool{"root": true, "dataRoot": true, "DataRoot": true}
	found := map[string][]string{}
	fset := token.NewFileSet()
	for _, top := range []string{"../../internal", "../../cmd"} {
		_ = filepath.WalkDir(top, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) < 2 {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Join" {
					return true
				}
				var first string
				switch x := call.Args[0].(type) {
				case *ast.Ident:
					first = x.Name
				case *ast.SelectorExpr:
					first = x.Sel.Name
				}
				lit, ok := call.Args[1].(*ast.BasicLit)
				if !rootLike[first] || !ok || lit.Kind != token.STRING {
					return true
				}
				name, _ := strconv.Unquote(lit.Value)
				name = strings.Split(filepath.ToSlash(name), "/")[0]
				found[name] = append(found[name], fset.Position(call.Pos()).String())
				return true
			})
			return nil
		})
	}
	if len(found) == 0 {
		t.Fatal("scan found no data root entries: the test is broken")
	}
	var names []string
	for name := range found {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, ok := dataRootEntries[name]; !ok {
			t.Errorf("%q is created under the data root (%s) but is not classified in dataRootEntries: decide whether instance.reset erases it", name, found[name][0])
		}
	}
	// what the table says is erased must really be in the reset plan
	targets := map[string]bool{}
	for _, p := range resetTargets("/r", "/r/masamong.db", true) {
		targets[filepath.Base(p)] = true
	}
	for name, kind := range dataRootEntries {
		if strings.HasPrefix(kind, "erase") && !targets[name] {
			t.Errorf("%q is classified %q but resetTargets does not erase it", name, kind)
		}
		if kind == "keep" && targets[name] {
			t.Errorf("%q is classified keep but resetTargets erases it", name)
		}
	}
}

// A reset journal written before rag/ and notices/ joined the plan resumes and still erases them.
func TestResetJournalFromBeforeRagAndNoticesFinishesTheWholePlan(t *testing.T) {
	s := maintenanceService(t, true)
	root := s.DataRoot
	for _, name := range []string{"rag/channels/g/c", "notices", "imports"} {
		_ = os.MkdirAll(filepath.Join(root, name), 0o700)
		_ = os.WriteFile(filepath.Join(root, name, "owned"), []byte("owned"), 0o600)
	}
	maintenanceAction(t, s, "instance.reset", map[string]any{"confirm": "reset", "backup": false})
	_ = s.Close()
	path := filepath.Join(root, maintenancePending)
	j, err := readMaintenance(path, root)
	if err != nil {
		t.Fatal(err)
	}
	j.Targets = j.Targets[:len(j.Targets)-2] // what the previous release planned
	if err := saveMaintenance(path, j, "requested"); err != nil {
		t.Fatal(err)
	}
	if err := RecoverMaintenance(root, slog.Default()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"rag", "notices", "imports"} {
		if _, err := os.Lstat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Errorf("%s survived the reset", name)
		}
	}
}
