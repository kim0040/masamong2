package application_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// This test pins which packages internal/application imports and which packages import it
// (E7 design 4.1, review row 0). It is the guard a split of the package is checked against: a
// subpackage must never reach back up into application, a connector or the web layer, and the
// Discord protocol leaf, if it ever exists, stays free of every masamong package and of discordgo.
//
// Every .go file is read, whatever its build tags, so a Windows-only file cannot add an edge the
// Linux build does not see. Only imports are parsed, so the test stays fast.

const (
	modulePath    = "masamong"
	applicationPk = modulePath + "/internal/application"
	discordgoPk   = "github.com/bwmarrin/discordgo"
)

// applicationImports is every masamong package the non-test files of internal/application import
// today. A new import is an architecture decision: add it here in the same change and say why.
var applicationImports = []string{
	"internal/action", "internal/agent", "internal/catalog", "internal/chat", "internal/clock", "internal/config",
	"internal/conversation", "internal/crash", "internal/currency", "internal/errclass", "internal/errcode",
	"internal/features/fortune", "internal/features/notices", "internal/features/participation", "internal/features/poll",
	"internal/features/privacy", "internal/features/reminders", "internal/fsutil", "internal/hostprofile", "internal/i18n",
	"internal/jobs", "internal/lang", "internal/launcher", "internal/memlimit", "internal/memory/blocks",
	"internal/memory/corpus", "internal/memory/engine", "internal/memory/episodes", "internal/memory/leaves",
	"internal/memory/lexical", "internal/memory/peoplebackfill", "internal/memory/retrieval", "internal/memory/search",
	"internal/memory/store",
	"internal/observability", "internal/providers", "internal/providers/decode", "internal/providers/finnhub",
	"internal/providers/fx", "internal/providers/kakao", "internal/providers/kma", "internal/providers/openai",
	"internal/providers/openmeteo", "internal/providers/practice", "internal/providers/websearch", "internal/relnotes",
	"internal/secrets", "internal/storage", "internal/tools", "internal/vision", "locales",
}

// applicationImporters is every package whose non-test files import internal/application. The exported
// surface they use stays the same through a split, so this list changes only by decision.
var applicationImporters = []string{
	"cmd/masamongd", "internal/cli", "internal/connectors/discord", "internal/transport/web", "internal/tui",
	"tools/evaluation/memory_dims/runner", "tools/loadtest",
}

// pkgInfo is what the import scan keeps of one directory.
type pkgInfo struct {
	prod     map[string]bool // imports of the non-test files
	hasTests bool
	testMain bool // some _test.go file declares TestMain
}

// scanModule reads the import clauses of every Go file under root. Keys are slash paths relative to root.
func scanModule(t *testing.T, root string) map[string]*pkgInfo {
	t.Helper()
	pkgs := map[string]*pkgInfo{}
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
		rel, err := filepath.Rel(root, filepath.Dir(p))
		if err != nil {
			return err
		}
		dir := path.Clean(filepath.ToSlash(rel))
		info := pkgs[dir]
		if info == nil {
			info = &pkgInfo{prod: map[string]bool{}}
			pkgs[dir] = info
		}
		isTest := strings.HasSuffix(name, "_test.go")
		mode := parser.ImportsOnly
		if isTest {
			mode = parser.SkipObjectResolution // the TestMain check needs declarations
		}
		f, err := parser.ParseFile(fset, p, nil, mode)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		if isTest {
			info.hasTests = true
			info.testMain = info.testMain || declaresTestMain(f)
			return nil
		}
		for _, imp := range f.Imports {
			info.prod[strings.Trim(imp.Path.Value, `"`)] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return pkgs
}

// layerProblems applies the layering rules to a scanned module. dir keys are relative to the module.
func layerProblems(pkgs map[string]*pkgInfo) []string {
	var out []string
	const appDir = "internal/application"
	for dir, info := range pkgs {
		inApp := strings.HasPrefix(dir, appDir+"/")
		for imp := range info.prod {
			switch {
			case inApp && (imp == applicationPk):
				out = append(out, fmt.Sprintf("%s imports %s: a package under internal/application never imports the package above it", dir, imp))
			case inApp && (strings.HasPrefix(imp, modulePath+"/internal/connectors") || strings.HasPrefix(imp, modulePath+"/internal/transport")):
				out = append(out, fmt.Sprintf("%s imports %s: application packages know no connector or transport", dir, imp))
			case dir == "internal/discordwire" && (strings.HasPrefix(imp, modulePath+"/") || imp == discordgoPk || strings.HasPrefix(imp, discordgoPk+"/")):
				out = append(out, fmt.Sprintf("%s imports %s: the Discord protocol leaf imports no masamong package and no discordgo", dir, imp))
			}
		}
		// the guard in main_test.go (offline mode, no keys, file secret store) covers one package only
		if inApp && info.hasTests && !info.testMain {
			out = append(out, fmt.Sprintf("%s has tests but no TestMain: copy the offline and key guard of internal/application/main_test.go", dir))
		}
	}
	sort.Strings(out)
	return out
}

func declaresTestMain(f *ast.File) bool {
	for _, decl := range f.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "TestMain" {
			return true
		}
	}
	return false
}

func relImports(info *pkgInfo) []string {
	var out []string
	for imp := range info.prod {
		if rel, ok := strings.CutPrefix(imp, modulePath+"/"); ok {
			out = append(out, rel)
		}
	}
	sort.Strings(out)
	return out
}

func sameSet(t *testing.T, what string, got, want []string) {
	t.Helper()
	g, w := map[string]bool{}, map[string]bool{}
	for _, x := range got {
		g[x] = true
	}
	for _, x := range want {
		w[x] = true
	}
	var added, removed []string
	for x := range g {
		if !w[x] {
			added = append(added, x)
		}
	}
	for x := range w {
		if !g[x] {
			removed = append(removed, x)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	if len(added) > 0 || len(removed) > 0 {
		t.Errorf("%s changed.\n  new: %v\n  gone: %v\nIf this is intended, update the list in layers_test.go in the same change and say why.", what, added, removed)
	}
}

func TestApplicationPackageEdgesArePinned(t *testing.T) {
	pkgs := scanModule(t, moduleRoot(t))
	app := pkgs["internal/application"]
	if app == nil {
		t.Fatal("internal/application not found")
	}
	sameSet(t, "the masamong packages internal/application imports", relImports(app), applicationImports)

	var importers []string
	for dir, info := range pkgs {
		if dir != "internal/application" && info.prod[applicationPk] {
			importers = append(importers, dir)
		}
	}
	sameSet(t, "the packages that import internal/application", importers, applicationImporters)
}

func TestApplicationDoesNotImportItsImporters(t *testing.T) {
	pkgs := scanModule(t, moduleRoot(t))
	for imp := range pkgs["internal/application"].prod {
		rel, _ := strings.CutPrefix(imp, modulePath+"/")
		for _, importer := range applicationImporters {
			if rel == importer {
				t.Errorf("internal/application imports %s, which imports it back", imp)
			}
		}
	}
	for _, dir := range []string{"internal/connectors", "internal/transport"} {
		for imp := range pkgs["internal/application"].prod {
			if strings.HasPrefix(imp, modulePath+"/"+dir) {
				t.Errorf("internal/application imports %s: the connectors and the web layer call application, not the other way", imp)
			}
		}
	}
}

func TestLayerRulesHoldInTheModule(t *testing.T) {
	if problems := layerProblems(scanModule(t, moduleRoot(t))); len(problems) > 0 {
		t.Errorf("layer rules broken:\n  %s", strings.Join(problems, "\n  "))
	}
}

// TestLayerRulesCatchTheirCases runs the rules on made-up modules, since today's module has no
// subpackage and no discordwire to break them.
func TestLayerRulesCatchTheirCases(t *testing.T) {
	mk := func(dir string, hasTests, testMain bool, imports ...string) map[string]*pkgInfo {
		info := &pkgInfo{prod: map[string]bool{}, hasTests: hasTests, testMain: testMain}
		for _, i := range imports {
			info.prod[i] = true
		}
		return map[string]*pkgInfo{dir: info}
	}
	for name, tc := range map[string]struct {
		pkgs map[string]*pkgInfo
		want string // substring of the one expected problem; "" = none
	}{
		"subpackage imports application":    {mk("internal/application/fortunesvc", false, false, applicationPk), "never imports the package above"},
		"subpackage imports a connector":    {mk("internal/application/kernel", false, false, modulePath+"/internal/connectors/discord"), "no connector or transport"},
		"subpackage imports transport":      {mk("internal/application/kernel", false, false, modulePath+"/internal/transport/web"), "no connector or transport"},
		"subpackage imports a lower one":    {mk("internal/application/fortunesvc", false, false, modulePath+"/internal/application/kernel", modulePath+"/internal/config"), ""},
		"discordwire imports masamong":      {mk("internal/discordwire", false, false, modulePath+"/internal/config"), "imports no masamong package"},
		"discordwire imports discordgo":     {mk("internal/discordwire", false, false, discordgoPk), "no discordgo"},
		"discordwire stdlib only":           {mk("internal/discordwire", false, false, "strings", "net/http"), ""},
		"subpackage tests without guard":    {mk("internal/application/fortunesvc", true, false), "no TestMain"},
		"subpackage tests with guard":       {mk("internal/application/fortunesvc", true, true), ""},
		"the connector may use application": {mk("internal/connectors/discord", false, false, applicationPk), ""},
	} {
		got := layerProblems(tc.pkgs)
		switch {
		case tc.want == "" && len(got) > 0:
			t.Errorf("%s: unexpected problems %v", name, got)
		case tc.want != "" && (len(got) != 1 || !strings.Contains(got[0], tc.want)):
			t.Errorf("%s: problems %v, want one containing %q", name, got, tc.want)
		}
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}
