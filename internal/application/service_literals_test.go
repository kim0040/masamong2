package application_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Service is built in three places in production code, and the split of this package (E7 review
// F4) needs every one of them to build the same way: when the shared state becomes a pointer that
// every feature holds, a Service built by hand in a fourth place would have a nil pointer and a
// guard such as `if s.Store == nil` would panic instead of returning. This test counts every
// composite literal (and new) of Service in the module and compares it with the list below. A new
// construction site fails the test. Adding it to the list is a decision: say why in the line.
//
// The count is per file, exact in both directions: a site that is removed must leave the list too,
// so the list never describes code that is gone.

type literalSites struct {
	n   int
	why string
}

var serviceLiterals = map[string]literalSites{
	// production
	"internal/application/service.go":  {1, "openService: the real service, built once by Open and openHooked"},
	"internal/application/practice.go": {1, "OpenPractice: the sandbox service with its own store and registry"},
	"internal/application/privacy.go":  {1, "applyPracticePrivacy: a short-lived practice service over the practice data root, closed when the callback returns"},
	// other packages
	"internal/tui/program_test.go": {1, "testService of the tray program tests: I18n only (test-only, outside the package)"},
	// tests inside the package: each builds a bare service for a unit that reads two or three fields
	"internal/application/channels_staging_internal_test.go":  {1, "staged file hashed and copied from the checked handle: DataRoot only"},
	"internal/application/embed_width_internal_test.go":       {1, "block lookup rejects labels before reading storage: no fields"},
	"internal/application/event_seq_internal_test.go":         {2, "two services count their own event ids: Clock and watchers"},
	"internal/application/fail_actions_test.go":               {1, "provider refusals as catalog sentences: I18n only"},
	"internal/application/fortune_flight_internal_test.go":    {1, "capacity of the zodiac slots: no fields"},
	"internal/application/idle_state_internal_test.go":        {6, "idle sweeps of room state: Clock only"},
	"internal/application/logring_lifetime_test.go":           {1, "a captured logger keeps only the ring after Close: Log only"},
	"internal/application/practice_registry_internal_test.go": {4, "practice registry lookups and slot clearing: DataRoot and practice only"},
	"internal/application/ledger_tool_loop_internal_test.go":  {1, "tool-loop params in the ledger: no fields"},
	"internal/application/log_level_test.go":                  {1, "log level change: Log only"},
	"internal/application/logring_redact_internal_test.go":    {2, "log ring redaction: no fields"},
	"internal/application/preferences_internal_test.go":       {1, "quietUntil arithmetic: no fields"},
	"internal/application/pricing_override_test.go":           {1, "price list stale at the listed price end: Clock only"},
	"internal/application/privacy_security_test.go":           {1, "HTTP tool credential scrubbing: Log only"},
	"internal/application/provider_result_internal_test.go":   {1, "provider result shows class and status only: I18n only"},
	"internal/application/scrubber_test.go":                   {2, "short credential is not sent: Log and I18n"},
	"internal/application/selfupdate_stream_test.go":          {1, "downloadService: release downloads from a test server, Log only"},
	"internal/application/threads_internal_test.go":           {1, "thread state stays bounded: Clock only"},
	"internal/application/ttl_cache_internal_test.go":         {1, "geocode and cooldown cache caps: no fields"},
	"internal/application/turn_flights_internal_test.go":      {5, "turn flight bookkeeping: no fields"},
}

func TestServiceIsBuiltOnlyWhereTheListSays(t *testing.T) {
	root := moduleRoot(t)
	got := map[string]int{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if p != root && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "node_modules" || name == "testdata" || name == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		inApp := filepath.ToSlash(filepath.Dir(rel)) == "internal/application"
		// outside the package only a file that imports it can name its Service
		imports, err := parser.ParseFile(fset, p, nil, parser.ImportsOnly)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		local := ""
		for _, imp := range imports.Imports {
			if strings.Trim(imp.Path.Value, `"`) == applicationPk {
				local = "application"
				if imp.Name != nil {
					local = imp.Name.Name
				}
			}
		}
		if !inApp && local == "" {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		isService := func(e ast.Expr) bool {
			switch t := e.(type) {
			case *ast.Ident:
				return inApp && t.Name == "Service"
			case *ast.SelectorExpr:
				x, ok := t.X.(*ast.Ident)
				return ok && local != "" && x.Name == local && t.Sel.Name == "Service"
			}
			return false
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CompositeLit:
				if n.Type != nil && isService(n.Type) {
					got[rel]++
				}
			case *ast.CallExpr:
				if fn, ok := n.Fun.(*ast.Ident); ok && fn.Name == "new" && len(n.Args) == 1 && isService(n.Args[0]) {
					got[rel]++
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	var problems []string
	for file, n := range got {
		want, ok := serviceLiterals[file]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("%s builds Service by hand %d time(s), and is not in the list: build it with Open, openHooked or OpenPractice, or add it to serviceLiterals with the reason", file, n))
		case n != want.n:
			problems = append(problems, fmt.Sprintf("%s builds Service %d time(s), the list says %d: update serviceLiterals", file, n, want.n))
		}
	}
	for file, want := range serviceLiterals {
		if got[file] == 0 {
			problems = append(problems, fmt.Sprintf("%s is in serviceLiterals (%d, %s) but no longer builds Service: remove the line", file, want.n, want.why))
		}
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		t.Errorf("Service construction sites changed:\n  %s", strings.Join(problems, "\n  "))
	}
}

// TestServiceLiteralListHasReasons keeps every allowlist line explained.
func TestServiceLiteralListHasReasons(t *testing.T) {
	total := 0
	for file, s := range serviceLiterals {
		if strings.TrimSpace(s.why) == "" || s.n < 1 {
			t.Errorf("%s: an allowlist line needs a count and a one-line reason", file)
		}
		total += s.n
	}
	// 3 production sites, 1 in the tray tests, 35 in this package's tests (E7 review F4: 30 at the
	// start of E7, then 5 in the log-ring and practice registry tests of the final review round)
	if total != 39 {
		t.Errorf("the list holds %d sites; the pinned count is 39: change it here with the reason", total)
	}
}
