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

// handlerLocation is one place in internal/application where an Action ID appears in a case
// clause or a map entry.
type handlerLocation struct {
	file string
	line int
}

// handlerLocations maps each catalog Action ID to the file:line spots in internal/application
// whose source dispatches it: case-clause string literals and map-literal keys. Other shapes
// (call arguments, comparisons) are not dispatch and are not listed.
func handlerLocations(root string) (map[string][]handlerLocation, error) {
	ids := map[string]bool{}
	for _, d := range catalog.All() {
		ids[d.ID] = true
	}
	dir := filepath.Join(root, "internal", "application")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := map[string][]handlerLocation{}
	record := func(fset *token.FileSet, name string, lit *ast.BasicLit) {
		v, err := strconv.Unquote(lit.Value)
		if err != nil || !ids[v] {
			return
		}
		out[v] = append(out[v], handlerLocation{file: name, line: fset.Position(lit.Pos()).Line})
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			return nil, fmt.Errorf("internal/application/%s: %w", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CaseClause:
				for _, expr := range n.List {
					if lit, ok := expr.(*ast.BasicLit); ok && lit.Kind == token.STRING {
						record(fset, name, lit)
					}
				}
			case *ast.KeyValueExpr:
				if lit, ok := n.Key.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					record(fset, name, lit)
				}
			}
			return true
		})
	}
	for _, l := range out {
		sort.Slice(l, func(i, j int) bool {
			if l[i].file != l[j].file {
				return l[i].file < l[j].file
			}
			return l[i].line < l[j].line
		})
	}
	return out, nil
}

// links renders handler locations as clickable file:line links relative to docs/code-map/.
func links(locs []handlerLocation) string {
	if len(locs) == 0 {
		return "—"
	}
	parts := make([]string, len(locs))
	for i, l := range locs {
		parts[i] = fmt.Sprintf("[%s:%d](../../internal/application/%s)", l.file, l.line, l.file)
	}
	return strings.Join(parts, "<br>")
}

// actionsIndex lists every catalog Action with its handler spots in internal/application.
func actionsIndex(root string) (string, error) {
	locs, err := handlerLocations(root)
	if err != nil {
		return "", err
	}
	var rows [][]string
	for _, d := range catalog.All() {
		avail := d.Availability
		if d.DeferredReason != "" {
			avail += " (" + d.DeferredReason + ")"
		}
		rows = append(rows, []string{
			cell(d.ID), cell(avail), cell(d.Effects),
			"`" + cell(d.CLI) + "`", "`" + cell(d.GUIRoute) + "`", links(locs[d.ID]),
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i][0] < rows[j][0] })
	var b strings.Builder
	b.WriteString("# Action 색인\n\ninternal/catalog의 모든 Action: CLI 명령, 콘솔 경로와 internal/application에서의 처리 위치.\n\n" + generatedHeader + "\n\n원본: `internal/catalog/catalog.go`의 `catalog.All()` (ID·상태·효과·CLI·경로), `internal/application`의 `case` 절과 맵 항목 (go/parser로 읽은 처리 위치)\n\n")
	b.WriteString(table(
		[]string{"Action", "상태", "효과", "CLI 명령", "콘솔 경로", "처리 위치"},
		rows))
	return b.String(), nil
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
			n := 0
			for k := range all {
				if _, ok := bundles[loc][k]; !ok {
					n++
				}
			}
			if n > 0 {
				missing = append(missing, fmt.Sprintf("%s(-%d)", loc, n))
			}
		}
		rows = append(rows, []string{
			cell(p), strconv.Itoa(len(all)), orDash(strings.Join(missing, ", ")),
		})
	}
	var b strings.Builder
	b.WriteString("# 로케일 색인\n\n메시지 카탈로그 키 접두사(첫 마디): 전체 키 수와 그 접두사의 키가 일부 없는 로케일.\n\n" + generatedHeader + "\n\n원본: `locales/*.json` (내장 메시지 카탈로그)\n\n")
	b.WriteString(table(
		[]string{"접두사", "키 수", "누락 로케일"},
		rows))
	return b.String()
}
