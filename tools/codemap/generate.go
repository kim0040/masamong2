package main

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/bwmarrin/discordgo"

	"masamong/internal/catalog"
	"masamong/internal/connectors/discord"
	"masamong/internal/lang"
	"masamong/internal/tools"
	"masamong/locales"
)

// generatedHeader is the note every index opens with.
const generatedHeader = "이 파일은 생성됩니다: go run ./tools/codemap (손으로 고치지 마세요)"

// codeRef points at a file · function (or file · package variable) inside internal/application.
// It carries no line number, so it stays valid while code above it moves.
type codeRef struct {
	file string
	name string
}

func (r codeRef) String() string {
	if r.name == "" {
		return r.file
	}
	return r.file + " · " + r.name
}

// pkgFile is one parsed source file of internal/application, with the names its imports use.
type pkgFile struct {
	name    string
	file    *ast.File
	imports map[string]bool
}

// funcDecl is one function or method declaration in the package.
type funcDecl struct {
	name string
	recv string // receiver base type name; "" for a plain function
	file *pkgFile
	decl *ast.FuncDecl
}

// pkgIndex is what handler resolution needs of internal/application: every file's imports and
// every declared function and method.
type pkgIndex struct {
	files   []*pkgFile
	plain   map[string]*funcDecl   // package-level functions by name
	methods map[string][]*funcDecl // methods by name, any receiver
	byDecl  map[*ast.FuncDecl]*funcDecl
}

// dispatchSite is one case clause or map entry in internal/application whose string literal is a
// catalog Action ID: the place that Action is dispatched.
type dispatchSite struct {
	id    string
	file  *pkgFile
	enc   string     // enclosing function or variable name
	encFn *funcDecl  // enclosing function declaration, nil for package-level entries
	body  []ast.Stmt // case-clause body; nil for a map entry
	value ast.Expr   // map-entry value; nil for a case clause
	pos   token.Pos
}

// loadPackage parses every non-test .go file in dir. overlay replaces a file's contents (tests
// use it to feed modified sources without touching the tree).
func loadPackage(dir string, overlay map[string]string) (*pkgIndex, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	idx := &pkgIndex{
		plain:   map[string]*funcDecl{},
		methods: map[string][]*funcDecl{},
		byDecl:  map[*ast.FuncDecl]*funcDecl{},
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		var src any
		if s, ok := overlay[name]; ok {
			src = s
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), src, 0)
		if err != nil {
			return nil, fmt.Errorf("internal/application/%s: %w", name, err)
		}
		pf := &pkgFile{name: name, file: f, imports: map[string]bool{}}
		for _, im := range f.Imports {
			p, _ := strconv.Unquote(im.Path.Value)
			if im.Name != nil {
				pf.imports[im.Name.Name] = true
			} else {
				pf.imports[filepath.Base(p)] = true
			}
		}
		idx.files = append(idx.files, pf)
	}
	for _, pf := range idx.files {
		for _, d := range pf.file.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			rec := &funcDecl{name: fd.Name.Name, file: pf, decl: fd, recv: receiverType(fd)}
			if fd.Recv == nil {
				idx.plain[rec.name] = rec
			} else {
				idx.methods[rec.name] = append(idx.methods[rec.name], rec)
			}
			idx.byDecl[fd] = rec
		}
	}
	for _, l := range idx.methods {
		sort.Slice(l, func(i, j int) bool {
			if l[i].file.name != l[j].file.name {
				return l[i].file.name < l[j].file.name
			}
			return l[i].recv < l[j].recv
		})
	}
	return idx, nil
}

// receiverType is the base type name of a method's receiver, "" for a plain function.
func receiverType(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return ""
	}
	return baseTypeName(fd.Recv.List[0].Type)
}

func baseTypeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return baseTypeName(t.X)
	case *ast.ParenExpr:
		return baseTypeName(t.X)
	case *ast.IndexExpr:
		return baseTypeName(t.X)
	case *ast.IndexListExpr:
		return baseTypeName(t.X)
	}
	return ""
}

// receiverName is the variable a method's receiver binds (s in func (s *Service) ...).
func receiverName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 || len(fd.Recv.List[0].Names) == 0 {
		return ""
	}
	return fd.Recv.List[0].Names[0].Name
}

// collectSites finds the package's dispatch sites for catalog Actions: the case clauses of a
// switch on the request's Action and the map entries keyed by an Action ID. A site counts only
// inside a function returning action.Result — the switches on req.Action in a guard, audit or
// predicate (gateDiscord, audit, readForm) never handle the request, and a package-level table
// like practiceBlocked is data, not dispatch.
func collectSites(idx *pkgIndex, ids map[string]bool) map[string][]*dispatchSite {
	out := map[string][]*dispatchSite{}
	match := func(e ast.Expr) (string, bool) {
		lit, ok := e.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return "", false
		}
		v, err := strconv.Unquote(lit.Value)
		if err != nil || !ids[v] {
			return "", false
		}
		return v, true
	}
	for _, pf := range idx.files {
		var stack []ast.Node
		ast.Inspect(pf.file, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			switch x := n.(type) {
			case *ast.CaseClause:
				// a case clause sits in its switch's body block: the switch is the grandparent.
				var sw *ast.SwitchStmt
				if len(stack) >= 2 {
					sw, _ = stack[len(stack)-2].(*ast.SwitchStmt)
				}
				enc, encFn := idx.enclosing(stack)
				if sw == nil || encFn == nil || !returnsResult(encFn) || !actionTag(sw.Tag, requestParams(encFn.decl)) {
					break
				}
				for _, expr := range x.List {
					if v, ok := match(expr); ok {
						out[v] = append(out[v], &dispatchSite{id: v, file: pf, enc: enc, encFn: encFn, body: x.Body, pos: expr.Pos()})
					}
				}
			case *ast.KeyValueExpr:
				if v, ok := match(x.Key); ok {
					if enc, encFn := idx.enclosing(stack); encFn != nil && returnsResult(encFn) {
						out[v] = append(out[v], &dispatchSite{id: v, file: pf, enc: enc, encFn: encFn, value: x.Value, pos: x.Key.Pos()})
					}
				}
			}
			stack = append(stack, n)
			return true
		})
	}
	for _, l := range out {
		sort.Slice(l, func(i, j int) bool {
			if l[i].file.name != l[j].file.name {
				return l[i].file.name < l[j].file.name
			}
			return l[i].pos < l[j].pos
		})
	}
	return out
}

// enclosing is the innermost declaration holding the node at the top of stack: the function it
// sits in, or the variable or type for package-level literals.
func (idx *pkgIndex) enclosing(stack []ast.Node) (string, *funcDecl) {
	for i := len(stack) - 1; i >= 0; i-- {
		switch d := stack[i].(type) {
		case *ast.FuncDecl:
			return d.Name.Name, idx.byDecl[d]
		case *ast.ValueSpec:
			return d.Names[0].Name, nil
		case *ast.TypeSpec:
			return d.Name.Name, nil
		}
	}
	return "", nil
}

// returnsResult reports whether fn yields an action.Result: only such a function can dispatch a
// request. A switch on req.Action in a void (audit), bool (readForm) or *action.Result
// (gateDiscord) function is a guard or predicate, not a handler.
func returnsResult(fn *funcDecl) bool {
	res := fn.decl.Type.Results
	if res == nil {
		return false
	}
	for _, f := range res.List {
		sel, ok := f.Type.(*ast.SelectorExpr)
		if ok && sel.Sel.Name == "Result" {
			if x, ok := sel.X.(*ast.Ident); ok && x.Name == "action" {
				return true
			}
		}
	}
	return false
}

// actionTag reports whether expr is `req.Action` for a request parameter name — the tag a
// dispatch switch has.
func actionTag(e ast.Expr, reqNames map[string]bool) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Action" {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	return ok && reqNames[x.Name]
}

// handler is the function a dispatch site hands the Action to. A site is a pure relay when its
// whole body is a single call that takes the request (return s.X(ctx, req, ...) or s.X(...)):
// the handler is then the callee, followed through further pure relays for the same Action, so
// a grouped dispatcher (channels, adminActions) names the function doing the work. Any other
// site handles the Action where it stands: the handler is the function containing the case
// clause, `if req.Action == "<id>"` or map entry.
func (idx *pkgIndex) handler(s *dispatchSite) codeRef {
	f := idx.relay(s, s.encFn)
	if f == nil {
		return codeRef{s.file.name, s.enc}
	}
	seen := map[*funcDecl]bool{f: true}
	for range 8 {
		inner := innerSites(f, s.id)
		if len(inner) == 0 {
			return codeRef{f.file.name, f.name}
		}
		g := idx.relay(inner[0], f)
		if g == nil || seen[g] {
			return codeRef{f.file.name, f.name}
		}
		seen[g] = true
		f = g
	}
	return codeRef{f.file.name, f.name}
}

// innerSites finds the spots inside f that dispatch id again: a case clause of an Action
// switch, a map entry, or a branch on req.Action == "id". Only sites inside f's own body count,
// so a relay function is followed exactly along the branch this Action takes.
func innerSites(f *funcDecl, id string) []*dispatchSite {
	var out []*dispatchSite
	reqNames := requestParams(f.decl)
	var stack []ast.Node
	ast.Inspect(f.decl.Body, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		switch x := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.CaseClause:
			var sw *ast.SwitchStmt
			if len(stack) >= 2 {
				sw, _ = stack[len(stack)-2].(*ast.SwitchStmt)
			}
			if sw == nil || !actionTag(sw.Tag, reqNames) {
				break
			}
			for _, expr := range x.List {
				lit, ok := expr.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				if v, err := strconv.Unquote(lit.Value); err == nil && v == id {
					out = append(out, &dispatchSite{id: id, file: f.file, enc: f.name, encFn: f, body: x.Body, pos: expr.Pos()})
					break
				}
			}
		case *ast.KeyValueExpr:
			lit, ok := x.Key.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				break
			}
			if v, err := strconv.Unquote(lit.Value); err == nil && v == id {
				out = append(out, &dispatchSite{id: id, file: f.file, enc: f.name, encFn: f, value: x.Value, pos: x.Key.Pos()})
			}
		case *ast.IfStmt:
			if condDispatches(x.Cond, id, reqNames) {
				out = append(out, &dispatchSite{id: id, file: f.file, enc: f.name, encFn: f, body: x.Body.List, pos: x.Pos()})
			}
		}
		stack = append(stack, n)
		return true
	})
	sort.Slice(out, func(i, j int) bool { return out[i].pos < out[j].pos })
	return out
}

// requestParams is the names of fn's parameters of type action.Request — the identifiers a
// requestAction comparison may be made on.
func requestParams(fd *ast.FuncDecl) map[string]bool {
	out := map[string]bool{}
	if fd.Type.Params == nil {
		return out
	}
	for _, p := range fd.Type.Params.List {
		sel, ok := p.Type.(*ast.SelectorExpr)
		if !ok {
			if st, isStar := p.Type.(*ast.StarExpr); isStar {
				sel, ok = st.X.(*ast.SelectorExpr)
			}
		}
		if !ok || sel.Sel.Name != "Request" {
			continue
		}
		if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "action" {
			continue
		}
		for _, n := range p.Names {
			out[n.Name] = true
		}
	}
	return out
}

// isActionCompare reports whether expr is `req.Action == "id"` (or the literal first), where req
// is one of the request parameters.
func isActionCompare(expr ast.Expr, id string, reqNames map[string]bool) bool {
	cmp, ok := expr.(*ast.BinaryExpr)
	if !ok || cmp.Op != token.EQL {
		return false
	}
	for _, side := range [][2]ast.Expr{{cmp.X, cmp.Y}, {cmp.Y, cmp.X}} {
		lit, ok := side[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			continue
		}
		if v, err := strconv.Unquote(lit.Value); err != nil || v != id {
			continue
		}
		if sel, ok := side[1].(*ast.SelectorExpr); ok && sel.Sel.Name == "Action" {
			if x, ok := sel.X.(*ast.Ident); ok && reqNames[x.Name] {
				return true
			}
		}
	}
	return false
}

// condDispatches reports whether an if condition contains a requestAction comparison for id.
func condDispatches(cond ast.Expr, id string, reqNames map[string]bool) bool {
	if len(reqNames) == 0 {
		return false
	}
	found := false
	ast.Inspect(cond, func(n ast.Node) bool {
		if found {
			return false
		}
		if e, ok := n.(ast.Expr); ok && isActionCompare(e, id, reqNames) {
			found = true
			return false
		}
		return true
	})
	return found
}

// relay resolves the function a pure-relay site hands the request to: a site whose whole body
// is a single call taking the request, or a map entry naming one. nil when the site's body does
// anything else — the work is then inline in the containing function — or when the call names
// nothing in the package (an external helper like action.OK produces the result, so the
// containing function still handles it).
func (idx *pkgIndex) relay(s *dispatchSite, owner *funcDecl) *funcDecl {
	if owner == nil {
		return nil
	}
	if s.value != nil {
		switch v := s.value.(type) {
		case *ast.CallExpr:
			return idx.relayCall(s.file, owner, v)
		case *ast.FuncLit:
			return idx.relay(&dispatchSite{file: s.file, body: v.Body.List}, owner)
		default:
			return idx.exprFunc(s.file, owner, v)
		}
	}
	if len(s.body) != 1 {
		return nil
	}
	var c *ast.CallExpr
	switch st := s.body[0].(type) {
	case *ast.ReturnStmt:
		if len(st.Results) != 1 {
			return nil
		}
		c, _ = st.Results[0].(*ast.CallExpr)
	case *ast.ExprStmt:
		c, _ = st.X.(*ast.CallExpr)
	}
	if c == nil {
		return nil
	}
	return idx.relayCall(s.file, owner, c)
}

// relayCall resolves a call that hands the request on, or nil when it is not a pure relay: the
// request must be a direct argument (a renamed or rebuilt request is the callee's own work),
// and a call taking a function literal (mutateConfig, applyCandidate) does its work through
// that closure rather than handing the request to a handler.
func (idx *pkgIndex) relayCall(pf *pkgFile, owner *funcDecl, c *ast.CallExpr) *funcDecl {
	if !passesRequest(c, requestParams(owner.decl)) {
		return nil
	}
	for _, a := range c.Args {
		if _, ok := a.(*ast.FuncLit); ok {
			return nil
		}
	}
	return idx.callFunc(pf, owner, c)
}

// passesRequest reports whether the call hands the request on directly.
func passesRequest(c *ast.CallExpr, reqNames map[string]bool) bool {
	for _, a := range c.Args {
		if id, ok := a.(*ast.Ident); ok && reqNames[id.Name] {
			return true
		}
	}
	return false
}

// exprFunc resolves an expression that names a function: a call, a plain function name, or a
// method reference like (*Service).checkFXInput.
func (idx *pkgIndex) exprFunc(pf *pkgFile, encFn *funcDecl, e ast.Expr) *funcDecl {
	switch x := e.(type) {
	case *ast.CallExpr:
		return idx.callFunc(pf, encFn, x)
	case *ast.Ident:
		return idx.plain[x.Name]
	case *ast.SelectorExpr:
		return idx.methodRef(pf, encFn, x)
	}
	return nil
}

// callFunc resolves s.X(...) or X(...) to its declaration in the package, or nil when the call
// is to another package, a builtin or a variable holding a function.
func (idx *pkgIndex) callFunc(pf *pkgFile, encFn *funcDecl, c *ast.CallExpr) *funcDecl {
	switch fn := c.Fun.(type) {
	case *ast.Ident:
		return idx.plain[fn.Name]
	case *ast.SelectorExpr:
		return idx.methodRef(pf, encFn, fn)
	}
	return nil
}

// methodRef resolves a selector s.Name or (*T).Name to a method declared in the package. A
// selector on an imported package is external; a selector on the enclosing function's receiver
// prefers a method of the same type.
func (idx *pkgIndex) methodRef(pf *pkgFile, encFn *funcDecl, sel *ast.SelectorExpr) *funcDecl {
	switch x := sel.X.(type) {
	case *ast.Ident:
		if pf.imports[x.Name] {
			return nil
		}
		cands := idx.methods[sel.Sel.Name]
		if len(cands) == 0 {
			return nil
		}
		if encFn != nil && receiverName(encFn.decl) == x.Name {
			if m := pickMethod(cands, encFn.recv); m != nil {
				return m
			}
		}
		if len(cands) == 1 {
			return cands[0]
		}
		if encFn != nil {
			if m := pickMethod(cands, encFn.recv); m != nil {
				return m
			}
		}
		return nil // same name on several receivers: not reliably resolvable
	case *ast.ParenExpr:
		return pickMethod(idx.methods[sel.Sel.Name], baseTypeName(x.X))
	}
	return nil
}

func pickMethod(cands []*funcDecl, recv string) *funcDecl {
	for _, c := range cands {
		if c.recv == recv {
			return c
		}
	}
	return nil
}

// links renders file · function references as clickable links relative to docs/code-map/.
func links(refs []codeRef) string {
	if len(refs) == 0 {
		return "—"
	}
	parts := make([]string, len(refs))
	for i, r := range refs {
		parts[i] = fmt.Sprintf("[%s](../../internal/application/%s)", r.String(), r.file)
	}
	return strings.Join(parts, "<br>")
}

// actionsIndex lists every catalog Action with the functions handling it and the spots that
// dispatch it in internal/application.
func actionsIndex(root string) (string, error) {
	idx, err := loadPackage(filepath.Join(root, "internal", "application"), nil)
	if err != nil {
		return "", err
	}
	ids := map[string]bool{}
	for _, d := range catalog.All() {
		ids[d.ID] = true
	}
	sites := collectSites(idx, ids)
	handlers := map[string][]codeRef{}
	for id, ss := range sites {
		for _, s := range ss {
			handlers[id] = appendUniqueRef(handlers[id], idx.handler(s))
		}
	}
	sortRefs := func(l []codeRef) {
		sort.Slice(l, func(i, j int) bool {
			if l[i].file != l[j].file {
				return l[i].file < l[j].file
			}
			return l[i].name < l[j].name
		})
	}
	for id := range handlers {
		sortRefs(handlers[id])
	}
	var rows [][]string
	for _, d := range catalog.All() {
		avail := d.Availability
		if d.DeferredReason != "" {
			avail += " (" + d.DeferredReason + ")"
		}
		rows = append(rows, []string{
			cell(d.ID), cell(avail), cell(d.Effects),
			"`" + cell(d.CLI) + "`", "`" + cell(d.GUIRoute) + "`", links(handlers[d.ID]),
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i][0] < rows[j][0] })
	var b strings.Builder
	b.WriteString("# Action 색인\n\ninternal/catalog의 모든 Action: CLI 명령, 콘솔 경로와 internal/application에서의 처리 함수.\n\n" + generatedHeader + "\n\n원본: `internal/catalog/catalog.go`의 `catalog.All()` (ID·상태·효과·CLI·경로), `internal/application`의 `switch req.Action`/`if req.Action ==` 분기 (처리: 본문이 요청을 넘기는 한 호출뿐인 순수 중계를 따라간 도착 함수, 아니면 그 분기를 품은 함수) — go/parser로 읽은 위치는 줄 번호를 쓰지 않아 관련 없는 편집에 지도가 낡지 않음\n\n")
	b.WriteString(table(
		[]string{"Action", "상태", "효과", "CLI 명령", "콘솔 경로", "처리"},
		rows))
	return b.String(), nil
}

func appendUniqueRef(l []codeRef, r codeRef) []codeRef {
	for _, e := range l {
		if e == r {
			return l
		}
	}
	return append(l, r)
}

// commandsIndex lists the Discord application commands the gateway registers, with the Action
// each slash command invokes and its options.
func commandsIndex() string {
	actionByName := map[string]string{}
	for _, c := range discord.Commands() {
		actionByName[c.Name] = c.Action
	}
	cmds := discord.ApplicationCommands()
	sort.Slice(cmds, func(i, j int) bool { return cmds[i].Name < cmds[j].Name })
	var rows [][]string
	for _, ac := range cmds {
		kind := "슬래시"
		if ac.Type == discordgo.MessageApplicationCommand {
			kind = "메시지 메뉴"
		}
		var ko string
		if ac.NameLocalizations != nil {
			ko = (*ac.NameLocalizations)[discordgo.Korean]
		}
		var opts []string
		for _, o := range ac.Options {
			okName := o.NameLocalizations[discordgo.Korean]
			req := "선택"
			if o.Required {
				req = "필수"
			}
			opts = append(opts, fmt.Sprintf("%s (%s, %s, %s)", cell(o.Name), cell(orDash(okName)), strings.ToLower(o.Type.String()), req))
		}
		rows = append(rows, []string{
			"`" + cell(ac.Name) + "`", orDash(ko), cell(kind),
			orDash(actionByName[ac.Name]), orDash(strings.Join(opts, "<br>")),
		})
	}
	var b strings.Builder
	b.WriteString("# Discord 명령 색인\n\nDiscord에 등록하는 애플리케이션 명령: 한국어 이름, 호출하는 Action과 옵션.\n\n" + generatedHeader + "\n\n원본: `internal/connectors/discord`의 `ApplicationCommands()` (명령·옵션), `locales/*.json`의 `discord.command.*` (한국어 이름)\n\n")
	b.WriteString(table(
		[]string{"명령", "한국어 이름", "종류", "Action", "옵션 (이름, 한국어, 타입, 필수)"},
		rows))
	return b.String()
}

// toolsIndex lists the built-in chat tools and the language-pack keys that describe each.
func toolsIndex() string {
	ko, _ := lang.Get(lang.Default)
	var rows [][]string
	for _, t := range tools.Builtins() {
		var keys []string
		if tool, ok := toolText(ko, t.ID); ok {
			if tool.Description != "" {
				keys = append(keys, "description")
			}
			if tool.Route != "" {
				keys = append(keys, "route")
			}
			if tool.Purpose != "" {
				keys = append(keys, "purpose")
			}
			if tool.When != "" {
				keys = append(keys, "when")
			}
			if tool.Skip != "" {
				keys = append(keys, "skip")
			}
			var params []string
			for p := range tool.Params {
				params = append(params, "params."+p)
			}
			sort.Strings(params)
			keys = append(keys, params...)
		}
		rows = append(rows, []string{
			cell(t.ID), cell(t.ID), orDash(strings.Join(keys, " · ")),
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i][0] < rows[j][0] })
	var b strings.Builder
	b.WriteString("# 채팅 도구 색인\n\n모델이 보는 내장 도구 목록: 호출하는 Action과 그 도구를 설명하는 언어팩 키.\n\n" + generatedHeader + "\n\n원본: `internal/tools/tools.go`의 `Builtins()` (도구 ID = catalog Action ID), `internal/lang/packs/" + lang.Default + ".json`의 `tools.<id>.*` (다른 팩은 부분 집합을 가질 수 있음)\n\n")
	b.WriteString(table(
		[]string{"도구 ID", "Action", "언어팩 키 (tools.<id>.아래)"},
		rows))
	return b.String()
}

// toolText is the tool's planner text in a pack, or false when the pack or the entry is absent.
func toolText(p *lang.Pack, id string) (lang.Tool, bool) {
	if p == nil {
		return lang.Tool{}, false
	}
	t, ok := p.Tools[id]
	return t, ok
}

// localesIndex lists message-catalog key prefixes with the locales missing keys of each.
func localesIndex() string {
	bundles := map[string]map[string]string{}
	dir, err := fs.ReadDir(locales.FS, ".")
	if err != nil {
		panic(err)
	}
	var names []string
	for _, e := range dir {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := fs.ReadFile(locales.FS, e.Name())
		if err != nil {
			panic(err)
		}
		m := map[string]string{}
		if err := json.Unmarshal(raw, &m); err != nil {
			panic(err)
		}
		bundles[strings.TrimSuffix(e.Name(), ".json")] = m
		names = append(names, strings.TrimSuffix(e.Name(), ".json"))
	}
	sort.Strings(names)
	prefixAll := map[string]map[string]bool{} // prefix -> key set (union over locales)
	for _, m := range bundles {
		for k := range m {
			prefix, _, _ := strings.Cut(k, ".")
			if prefixAll[prefix] == nil {
				prefixAll[prefix] = map[string]bool{}
			}
			prefixAll[prefix][k] = true
		}
	}
	var prefixes []string
	for p := range prefixAll {
		prefixes = append(prefixes, p)
	}
	sort.Strings(prefixes)
	var rows [][]string
	for _, p := range prefixes {
		all := prefixAll[p]
		var missing []string
		for _, loc := range names {
			for k := range all {
				if _, ok := bundles[loc][k]; !ok {
					missing = append(missing, loc)
					break
				}
			}
		}
		rows = append(rows, []string{
			cell(p), orDash(strings.Join(missing, ", ")),
		})
	}
	var b strings.Builder
	b.WriteString("# 로케일 색인\n\n메시지 카탈로그 키 접두사(첫 마디)와 그 접두사의 키가 일부 없는 로케일.\n\n" + generatedHeader + "\n\n원본: `locales/*.json` (내장 메시지 카탈로그)\n\n")
	b.WriteString(table(
		[]string{"접두사", "누락 로케일"},
		rows))
	return b.String()
}
