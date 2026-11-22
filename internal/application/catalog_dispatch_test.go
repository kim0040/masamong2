package application

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/catalog"
	"masamong/internal/errcode"
)

// The catalog (internal/catalog), the Execute switch (service.go) and the transports are three hand-kept
// lists of the same Actions. These tests keep the catalog and the switch together: a catalog ID that
// Execute does not know answers ACTION_UNKNOWN on every surface, and nothing else would notice.

// A catalog Action that can run on the practice instance (offline providers, its own data root) must
// reach a handler. An empty input is enough: a handler may refuse it (INVALID_INPUT, FEATURE_DISABLED,
// JOB_NOT_FOUND, ...), but not as an unknown Action.
func TestEveryCatalogActionReachesAHandler(t *testing.T) {
	p, err := OpenPractice(context.Background(), t.TempDir(), "en", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	ran := 0
	for _, d := range catalog.All() {
		if d.Availability != catalog.Executable || practiceBlocked[d.ID] {
			continue // checked against the source below: they never reach a handler on the practice instance
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		res := p.Execute(ctx, action.Request{Action: d.ID, Input: []byte(`{}`)})
		cancel()
		if res.Error != nil && res.Error.Code == errcode.ActionUnknown {
			t.Errorf("catalog Action %s answers %s: Service.execute has no case for it", d.ID, errcode.ActionUnknown)
		}
		ran++
	}
	if ran < 100 {
		t.Fatalf("only %d catalog Actions ran; the catalog or the filter changed", ran)
	}
}

// The Actions the practice instance refuses (practiceBlocked) and the ones switched off in the catalog
// stop before the switch, so running them proves nothing (and some would restart or reset a real
// instance). For those, the main `switch req.Action` of Service.execute must still list the ID.
func TestPracticeBlockedAndDeferredActionsAreInTheExecuteSwitch(t *testing.T) {
	cases := executeCases(t)
	check := map[string]string{}
	for _, d := range catalog.All() {
		if d.Availability != catalog.Executable {
			check[d.ID] = "deferred in the catalog"
		}
	}
	for id := range practiceBlocked {
		if _, ok := catalog.Lookup(id); !ok {
			t.Errorf("practiceBlocked names %q, which is not a catalog Action", id)
			continue
		}
		check[id] = "blocked in practice mode"
	}
	ids := make([]string, 0, len(check))
	for id := range check {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if !cases[id] {
			t.Errorf("%s (%s) has no case in Service.execute", id, check[id])
		}
	}
}

// executeCases returns the string constants of the case clauses of the main switch in Service.execute
// (the switch on req.Action that has a default clause).
func executeCases(t *testing.T) map[string]bool {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "service.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]bool{}
	found := false
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "execute" || fn.Recv == nil || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			sw, ok := n.(*ast.SwitchStmt)
			if !ok || !switchesOnRequestAction(sw) || !hasDefault(sw) {
				return true
			}
			found = true
			for _, stmt := range sw.Body.List {
				cc := stmt.(*ast.CaseClause)
				for _, e := range cc.List {
					if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
						if id, err := strconv.Unquote(lit.Value); err == nil {
							cases[id] = true
						}
					}
				}
			}
			return false
		})
	}
	if !found || len(cases) < 100 {
		t.Fatalf("found the main switch of Service.execute: %v, with %d cases; service.go changed shape", found, len(cases))
	}
	return cases
}

func switchesOnRequestAction(sw *ast.SwitchStmt) bool {
	sel, ok := sw.Tag.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Action" {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	return ok && x.Name == "req"
}

func hasDefault(sw *ast.SwitchStmt) bool {
	for _, stmt := range sw.Body.List {
		if cc, ok := stmt.(*ast.CaseClause); ok && cc.List == nil {
			return true
		}
	}
	return false
}
