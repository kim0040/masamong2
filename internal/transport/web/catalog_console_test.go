//go:build !noweb

package web

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"masamong/internal/catalog"
	"masamong/internal/tools"
)

// consoleUnreachable lists executable catalog Actions the console deliberately does not call, with the
// reason. AGENTS.md says a feature that only exists in the CLI is unfinished; the route check in
// catalog_parity_test.go only proves a page exists for each Action, not that the page calls it, so a
// feature could ship GUI-less behind a valid route. This map is the explicit, reviewed exception list:
// adding an Action to the catalog without a console flow now fails TestConsoleCallsEveryExecutableAction
// until it is either wired into ui/src or listed here with a reason.
var consoleUnreachable = map[string]string{
	"config.validate":    "equivalent: the Settings page previews a change with config.diff, which validates the same document",
	"runtime.status":     "equivalent in practice: pause state, version, revision, uptime and the restart-needed flag reach Home and the TopBar through dashboard.overview; the raw started_at timestamp of runtime.status is not shown as such (uptime is)",
	"runtime.drain":      "equivalent: the TopBar pause control calls runtime.pause with scope all, which is what drain does",
	"tools.configure":    "equivalent: the Tools page edits tool settings by patching the config document (config.patch)",
	"jobs.get":           "equivalent: the Jobs page lists jobs with jobs.list, whose rows carry the same job fields; a single-job fetch is a CLI convenience",
	"jobs.watch":         "equivalent: jobs.watch is a one-shot read of the whole job list that the CLI polls repeatedly; the console reads the same list with jobs.list, refreshes it on the SSE job events and re-polls it every few seconds while jobs run",
	"usage.budgets":      "equivalent: the Usage page shows the month's spend, budget and per-kind spend (dashboard.overview month), each provider's calls today against its daily limit (providers.list, from the same registry statuses) and each tool's calls today (tools.list used_today); usage.budgets is those three joined for the CLI, while the page calls usage.reset and reads its active reset from dashboard.overview",
	"channels.view.show": "equivalent: the Room page lists a channel's attached blocks from memory.blocks.list (channels[].attached, the same RAGView.Blocks this Action returns) and Block shows the reverse",

	"channels.attachments.add":    "dormant: attachments are stored in config (RAGAttachments) but nothing in answering reads them yet, so a console control would look like it works while doing nothing",
	"channels.attachments.remove": "dormant: attachments are stored in config (RAGAttachments) but nothing in answering reads them yet, so a console control would look like it works while doing nothing",
}

// The guard counts an Action as called only when its id is the first argument of an Action call in code
// the app actually loads: call(...) and resource(...) (the two ways ui/src/lib reaches POST /api/action),
// a file-local wrapper around call such as People's run(act, ...), or a const the call reads (Memory picks
// between two search Actions that way). Ids in comments, i18n keys (t('x.y')), permission checks
// (can('x.y')), tool-id maps (toolmeta.ts) and modules nothing imports are not calls, so none of them can
// keep a GUI-less Action green. A call whose first argument it cannot resolve fails the test instead of
// being skipped: a new way of naming an Action has to teach this guard, not slip past it.
//
// Limits (accepted, review R3c): this is a regression net for an Action someone forgot to wire, not a
// parser. Text that spells a call outside any string or comment — plain Svelte markup such as
// <pre>call('x.y')</pre>, or a JS regex literal — still counts. The other direction is safe: a real call
// it cannot see (inside a template literal's ${...} or a quoted attribute value) makes the test fail.
var (
	actionIDShape = `[a-z][a-zA-Z_]*(?:\.[a-zA-Z_]+)+`
	// call( / resource( with optional generics; the lookbehind-by-hand [^.\w$] keeps `x.call(` out.
	actionCallHead = regexp.MustCompile(`(?:^|[^.\w$])(call|resource)\s*(?:<[^()]*?>)?\s*\(\s*`)
	literalArg     = regexp.MustCompile("^[\"'`](" + actionIDShape + ")[\"'`]")
	plainIdent     = regexp.MustCompile(`^[A-Za-z_$][\w$]*$`)
	typedParam     = regexp.MustCompile(`^[A-Za-z_$][\w$]*\s*\??\s*:`)
	actionShaped   = regexp.MustCompile("[\"'`](" + actionIDShape + ")[\"'`]")
	importSpec     = regexp.MustCompile(`(?:\bfrom|\bimport)\s*\(?\s*["'](\.[^"']+)["']`)
	htmlComment    = regexp.MustCompile(`(?s)<!--.*?-->`)
)

// blankKeepLines replaces every byte of s but line breaks with a space, so offsets and lines stay put.
func blankKeepLines(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c != '\n' {
			b[i] = ' '
		}
	}
	return string(b)
}

// maskStrings blanks the contents of string literals (not their quotes), with the same quote rules as
// stripComments. A call written inside a string ("call('x.y')" in a t() argument or a message) is text,
// not a call: call heads are found on the masked copy, while arguments are read from the unmasked code
// at the same offsets.
func maskStrings(src string) string {
	b := []byte(src)
	var quote byte
	for i := 0; i < len(b); i++ {
		c := b[i]
		if quote != 0 {
			switch {
			case c == '\\':
				b[i] = ' '
				if i+1 < len(b) && b[i+1] != '\n' {
					b[i+1] = ' '
				}
				i++
			case c == quote, c == '\n' && quote != '`':
				quote = 0
			default:
				if c != '\n' {
					b[i] = ' '
				}
			}
			continue
		}
		if c == '"' || c == '\'' || c == '`' {
			quote = c
		}
	}
	return string(b)
}

// stripComments blanks JS/TS line and block comments and, for Svelte, HTML comments, keeping string
// contents and line breaks. A quote ends at its line break (except a template literal), so an
// apostrophe in markup text cannot swallow the rest of the file.
func stripComments(src string, html bool) string {
	if html {
		// HTML comments first, ignoring quotes: markup text is not code, so an apostrophe before a comment
		// on the same line ("don't <!-- call('x.y') -->") must not hide the comment from the scan below.
		src = htmlComment.ReplaceAllStringFunc(src, blankKeepLines)
	}
	b := []byte(src)
	n := len(b)
	blank := func(from, to int) {
		for k := from; k < to && k < n; k++ {
			if b[k] != '\n' {
				b[k] = ' '
			}
		}
	}
	var quote byte
	for i := 0; i < n; {
		c := b[i]
		switch {
		case quote != 0:
			switch {
			case c == '\\':
				i += 2
				continue
			case c == quote, c == '\n' && quote != '`':
				quote = 0
			}
			i++
		case c == '"', c == '\'', c == '`':
			quote = c
			i++
		case c == '/' && i+1 < n && b[i+1] == '/':
			j := i
			for j < n && b[j] != '\n' {
				j++
			}
			blank(i, j)
			i = j
		case c == '/' && i+1 < n && b[i+1] == '*':
			j := strings.Index(string(b[i+2:]), "*/")
			end := n
			if j >= 0 {
				end = i + 2 + j + 2
			}
			blank(i, end)
			i = end
		case html && c == '<' && strings.HasPrefix(string(b[i:min(i+4, n)]), "<!--"):
			j := strings.Index(string(b[i:]), "-->")
			end := n
			if j >= 0 {
				end = i + j + 3
			}
			blank(i, end)
			i = end
		default:
			i++
		}
	}
	return string(b)
}

// actionCalls returns the Action ids one source file calls, and the calls whose first argument it could
// not resolve (reported as "name(identifier)").
func actionCalls(src string, html bool) (ids map[string]bool, unresolved []string) {
	code := stripComments(src, html)
	masked := maskStrings(code)
	ids = map[string]bool{}
	// wrappers[name] is true for file-local functions whose first parameter is passed on to call()
	wrappers := map[string]bool{}
	for _, m := range actionCallHead.FindAllStringSubmatchIndex(masked, -1) {
		arg := firstArgument(code[m[1]:])
		if arg == "" || typedParam.MatchString(arg) {
			continue // call() or a declaration such as `function call<T>(action: string`
		}
		if lit := literalArg.FindStringSubmatch(arg); lit != nil {
			ids[lit[1]] = true
			continue
		}
		name := ""
		if plainIdent.MatchString(arg) {
			name = arg
			// a const the call reads: collect the Action ids in its initializer
			if init := constInitializer(code, name); init != "" {
				found := false
				for _, lit := range actionShaped.FindAllStringSubmatch(init, -1) {
					ids[lit[1]] = true
					found = true
				}
				if found {
					continue
				}
			}
			// a parameter of an enclosing file-local function: its call sites name the Action
			if fn := enclosingParamOf(code, m[0], name); fn != "" {
				if fn == "call" || fn == "resource" {
					continue // the primitive itself (lib/api.svelte.ts, lib/resource.svelte.ts)
				}
				wrappers[fn] = true
				continue
			}
		} else if lits := actionShaped.FindAllStringSubmatch(arg, -1); len(lits) > 0 {
			// an inline choice such as `cond ? 'a.b' : 'c.d'`
			for _, lit := range lits {
				ids[lit[1]] = true
			}
			continue
		}
		unresolved = append(unresolved, code[m[2]:m[3]]+"("+arg+")")
	}
	for w := range wrappers {
		re := regexp.MustCompile(`(?:^|[^.\w$])` + regexp.QuoteMeta(w) + `\s*\(\s*["'` + "`" + `](` + actionIDShape + `)["'` + "`" + `]`)
		for _, m := range re.FindAllStringSubmatchIndex(code, -1) {
			// the literal's opening quote survives masking only when the call is code, not string text
			if masked[m[2]-1] == code[m[2]-1] {
				ids[code[m[2]:m[3]]] = true
			}
		}
	}
	return ids, unresolved
}

// firstArgument returns the text of the first argument of a call whose "(" was just consumed: up to the
// first comma or closing parenthesis outside any nested brackets and strings.
func firstArgument(rest string) string {
	depth := 0
	var quote byte
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		if quote != 0 {
			if c == '\\' {
				i++
			} else if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '"', '\'', '`':
			quote = c
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if depth == 0 {
				return strings.TrimSpace(rest[:i])
			}
			depth--
		case ',':
			if depth == 0 {
				return strings.TrimSpace(rest[:i])
			}
		}
	}
	return strings.TrimSpace(rest)
}

// constInitializer returns the rest of the line after `const name =` (also let/var), or "".
func constInitializer(code, name string) string {
	re := regexp.MustCompile(`\b(?:const|let|var)\s+` + regexp.QuoteMeta(name) + `\b[^=\n]*=\s*([^\n;]*)`)
	if m := re.FindStringSubmatch(code); m != nil {
		return m[1]
	}
	return ""
}

// enclosingParamOf finds the nearest `function f(name` declared before pos and returns f.
func enclosingParamOf(code string, pos int, name string) string {
	re := regexp.MustCompile(`\bfunction\s+([A-Za-z_$][\w$]*)\s*(?:<[^()]*?>)?\s*\(\s*` + regexp.QuoteMeta(name) + `\b`)
	var fn string
	for _, m := range re.FindAllStringSubmatchIndex(code[:pos], -1) {
		fn = code[m[2]:m[3]]
	}
	return fn
}

// reachableFiles returns the entries of files (slash path -> source) that the app loads: everything
// imported, statically or lazily, from entry through relative specifiers.
func reachableFiles(files map[string]string, entry string) []string {
	seen := map[string]bool{}
	var walk func(p string)
	walk = func(p string) {
		if seen[p] {
			return
		}
		seen[p] = true
		src := stripComments(files[p], strings.HasSuffix(p, ".svelte"))
		for _, m := range importSpec.FindAllStringSubmatch(src, -1) {
			base := path.Join(path.Dir(p), m[1])
			for _, cand := range []string{base, base + ".ts", base + ".svelte", base + ".js", base + "/index.ts"} {
				if _, ok := files[cand]; ok {
					walk(cand)
					break
				}
			}
		}
	}
	if _, ok := files[entry]; ok {
		walk(entry)
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// consoleActionCalls returns every Action id called from the code the console loads, and the calls it
// could not resolve.
func consoleActionCalls(t *testing.T) (map[string]bool, []string) {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir("ui/src", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if ext := filepath.Ext(p); ext != ".svelte" && ext != ".ts" {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(p)] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := files["ui/src/main.ts"]; !ok {
		t.Fatal("ui/src/main.ts (the console entry the guard walks from) is missing")
	}
	found := map[string]bool{}
	var unresolved []string
	for _, p := range reachableFiles(files, "ui/src/main.ts") {
		ids, un := actionCalls(files[p], strings.HasSuffix(p, ".svelte"))
		for id := range ids {
			found[id] = true
		}
		for _, u := range un {
			unresolved = append(unresolved, p+": "+u)
		}
	}
	return found, unresolved
}

// Every executable catalog Action is either called by the console or explicitly excused with a reason,
// and the excuse list cannot go stale (an excused Action that the console now calls, or one the catalog
// no longer has or no longer executes, fails too).
func TestConsoleCallsEveryExecutableAction(t *testing.T) {
	used, unresolved := consoleActionCalls(t)
	for _, u := range unresolved {
		t.Errorf("console Action call the guard cannot resolve: %s\n"+
			"pass the id as a string literal, or teach actionCalls the new pattern", u)
	}

	// A built-in tool is an Action too, but the console runs it by id: the Tool page tests any tool with
	// tools.test (tools.Find gates the id) and Tools/Usage/Room list and configure them from tools.list, so
	// the tool Actions (weather.query, search.web, ...) never appear as a call literal of their own. They
	// count as called while those two calls exist. chat.respond is the tool tools.test refuses, so it needs
	// a direct call (the Lab chat tab).
	toolsRunnable := used["tools.test"] && used["tools.list"]

	var missing []string
	for _, d := range catalog.All() {
		if d.Availability != catalog.Executable {
			continue
		}
		if _, excused := consoleUnreachable[d.ID]; excused {
			continue
		}
		if _, isTool := tools.Lookup(d.ID); isTool && d.ID != "chat.respond" && toolsRunnable {
			continue
		}
		if !used[d.ID] {
			missing = append(missing, d.ID)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("executable Actions with no console call and no entry in consoleUnreachable: %s\n"+
			"AGENTS.md: GUI is the default, so add a console flow, or list the Action with a reason", strings.Join(missing, ", "))
	}

	for id, reason := range consoleUnreachable {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("consoleUnreachable[%q] needs a reason", id)
		}
		d, ok := catalog.Lookup(id)
		if !ok {
			t.Errorf("consoleUnreachable lists %q, which is not in the catalog", id)
			continue
		}
		if d.Availability != catalog.Executable {
			t.Errorf("consoleUnreachable lists %q, which is not executable; remove the entry", id)
		}
		if used[id] {
			t.Errorf("consoleUnreachable lists %q but the console calls it; remove the stale entry", id)
		}
	}
}

// The guard has to count real calls and only those. Each case below is a way an id can sit in the source
// without the console calling it (the false-green R3 found: a comment kept TestConsoleCallsEveryExecutableAction
// passing after the real memory.reindex calls were gone), plus the call shapes that must count.
func TestConsoleActionCallDetection(t *testing.T) {
	type tc struct {
		name, ext, src string
		want           []string
		unresolved     int
	}
	cases := []tc{
		{name: "line comment only", ext: ".ts", src: "// await call('memory.reindex', {})\nconst a = 1"},
		{name: "block comment only", ext: ".ts", src: "/* call('memory.reindex') */ const a = 1"},
		{name: "html comment only", ext: ".svelte", src: "<!-- call('memory.reindex') -->\n<p>x</p>"},
		{name: "html comment spanning lines", ext: ".svelte", src: "<!--\n resource('memory.reindex')\n-->"},
		{name: "i18n key only", ext: ".svelte", src: "<p>{t('memory.reindex')}</p>\n<p>{tp('memory.reindex', 2)}</p>"},
		{name: "map key only", ext: ".ts", src: "const kinds = { 'memory.reindex': 'talk', 'memory.search': 'talk' }"},
		{name: "can() only", ext: ".svelte", src: "<Button disabled={!can('memory.reindex')}>x</Button>"},
		{name: "method named call", ext: ".ts", src: "fn.call('memory.reindex', 1)"},
		{name: "html comment after an apostrophe on the same line", ext: ".svelte", src: "<p>don't <!-- call('memory.reindex') --></p>"},
		{name: "html comment after a double quote on the same line", ext: ".svelte", src: "<p>say \"hi <!-- resource('memory.reindex') --></p>"},
		{name: "call spelled inside a t() string", ext: ".svelte", src: "<p>{t(\"call('memory.reindex')\")}</p>"},
		{name: "call spelled inside a can() string", ext: ".ts", src: "const ok = can(\"call('memory.reindex')\")"},
		{name: "wrapper call spelled inside a string", ext: ".svelte",
			src: "async function run(act: string) { return call(act, {}) }\nconst a = () => run('privacy.consent')\nconst msg = \"run('jobs.list')\"", want: []string{"privacy.consent"}},
		{name: "real call", ext: ".svelte", src: "<script>\n const r = await call('memory.reindex', { rooms: [] })\n</script>", want: []string{"memory.reindex"}},
		{name: "real call with generics", ext: ".svelte", src: "const r = await call<{ ok: boolean }>('memory.reindex', {})", want: []string{"memory.reindex"}},
		{name: "resource with nested generics", ext: ".svelte",
			src: "const s = resource<{ reasons: Record<string, number> }>('feedback.summary', () => ({}), { live: ['turn'] })", want: []string{"feedback.summary"}},
		{name: "real call after a comment on the same line", ext: ".ts", src: "/* x */ call('memory.reindex', {}) // call('jobs.list')", want: []string{"memory.reindex"}},
		{name: "url in a string does not start a comment", ext: ".ts", src: "post('http://x//y', { a: 1 }); call('memory.reindex')", want: []string{"memory.reindex"}},
		{name: "apostrophe in markup does not swallow the next line", ext: ".svelte", src: "<p>don't</p>\n<script>call('memory.reindex')</script>", want: []string{"memory.reindex"}},
		{name: "const the call reads", ext: ".svelte",
			src: "const action = mode === 'messages' ? 'messages.search' : 'memory.search'\nconst r = await call(action, {})", want: []string{"messages.search", "memory.search"}},
		{name: "file-local wrapper", ext: ".svelte",
			src:  "async function run(act: string, input: object) { return call(act, input) }\nconst a = () => run('privacy.consent', {})\n// run('jobs.list')",
			want: []string{"privacy.consent"}},
		{name: "primitive wrapper is skipped", ext: ".ts", src: "export async function call<T = any>(action: string) {}\nasync function load() { await call<T>(action, inp) }\nexport function resource(action: string) {}"},
		{name: "unresolvable id", ext: ".ts", src: "function go(list) { for (const x of list) call(x.action, {}) }", unresolved: 1},
	}
	for _, c := range cases {
		ids, un := actionCalls(c.src, c.ext == ".svelte")
		var got []string
		for id := range ids {
			got = append(got, id)
		}
		sort.Strings(got)
		want := append([]string(nil), c.want...)
		sort.Strings(want)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: calls = %v, want %v", c.name, got, want)
		}
		if len(un) != c.unresolved {
			t.Errorf("%s: unresolved = %v, want %d", c.name, un, c.unresolved)
		}
	}
}

// A module nothing imports is not part of the app, so an Action id called only there does not count.
func TestConsoleReachabilitySkipsUnusedModules(t *testing.T) {
	files := map[string]string{
		"ui/src/main.ts":                 "import App from './App.svelte'\nimport './styles/global.css'",
		"ui/src/App.svelte":              "<script>\n import { load } from './lib/load.svelte'\n const lazy = () => import('./pages/Home.svelte')\n</script>",
		"ui/src/lib/load.svelte.ts":      "import { x } from './util'\nexport const a = 1",
		"ui/src/lib/util.ts":             "export const x = 1",
		"ui/src/pages/Home.svelte":       "<script>call('jobs.list')</script>",
		"ui/src/components/Dead.svelte":  "<script>call('memory.reindex')</script>",
		"ui/src/lib/commented.svelte.ts": "export const b = 1",
		// only a comment imports it
		"ui/src/lib/ghost.ts": "call('memory.reindex')",
	}
	files["ui/src/lib/util.ts"] += "\n// import { g } from './ghost'"
	got := strings.Join(reachableFiles(files, "ui/src/main.ts"), ",")
	want := "ui/src/App.svelte,ui/src/lib/load.svelte.ts,ui/src/lib/util.ts,ui/src/main.ts,ui/src/pages/Home.svelte"
	if got != want {
		t.Errorf("reachable = %s\nwant       %s", got, want)
	}
}
