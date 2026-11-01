package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"masamong/internal/catalog"
)

// usageText is what `masamong`, `masamong help` and `--help` print on a terminal: the few commands a
// person needs first, then how every console Action is reached. Scripts (JSON output) still get the
// help.version result.
func usageText() string {
	var groups []string
	seen := map[string]bool{}
	for _, d := range catalog.All() {
		f := strings.Fields(d.CLI)
		if len(f) == 0 || strings.HasPrefix(f[0], "-") || seen[f[0]] {
			continue
		}
		seen[f[0]] = true
		groups = append(groups, f[0])
	}
	sort.Strings(groups)
	var b strings.Builder
	b.WriteString(`masamong — a Discord bot with memory, run from a browser console

Start and stop
  masamong open              start the service if needed and open the console, signed in
  masamong start             start the service without a browser
  masamong stop              stop it (replies in progress finish first)
  masamong restart           restart it
  masamong status            is it running, where, paused?
  masamong connect <host>    manage Masamong on another computer over SSH

Sign in and check
  masamong admin token       print the console sign-in token
  masamong doctor            what works and what still needs setup

Everything the console does
  masamong <group> [verb] [--name value]    e.g. masamong channels list, masamong backup create
`)
	b.WriteString(wrap("  groups: ", strings.Join(groups, ", "), 100))
	b.WriteString(`
Options
  --data-root DIR    another instance's folder     --output json    machine-readable output
  --locale ko|en     language of messages           -i               live screen (doctor, jobs)
  --input-file FILE  for "tools test" only: the tool input as a JSON object from a file (UTF-8 or UTF-16,
                     as PowerShell writes it); use - for standard input
`)
	return b.String()
}

func wrap(prefix, text string, width int) string {
	var b strings.Builder
	line := prefix
	indent := strings.Repeat(" ", len(prefix))
	for _, w := range strings.Fields(text) {
		if len(line)+len(w)+1 > width && strings.TrimSpace(line) != strings.TrimSpace(prefix) {
			b.WriteString(strings.TrimRight(line, " ") + "\n")
			line = indent
		}
		line += w + " "
	}
	b.WriteString(strings.TrimRight(line, " ") + "\n")
	return b.String()
}

// knownAction reports whether id is an Action the service knows (every one is in the catalog).
func knownAction(id string) bool {
	for _, d := range catalog.All() {
		if d.ID == id {
			return true
		}
	}
	return false
}

// suggest finds the command closest to what was typed ("staus" → "status"), or "".
func suggest(typed string) string {
	cands := []string{"open", "start", "stop", "restart", "status", "connect", "tray", "doctor", "version", "help", "admin token"}
	for k := range catalogRoutes() {
		cands = append(cands, k)
	}
	best, bestD := "", 1<<30
	for _, c := range cands {
		if d := editDistance(typed, c); d < bestD || (d == bestD && c < best) {
			best, bestD = c, d
		}
	}
	if bestD > max(2, len([]rune(typed))/3) {
		return ""
	}
	return best
}

func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur := make([]int, len(rb)+1)
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(rb)]
}

// unknownCommand explains a command that maps to no Action, with the nearest real one.
func unknownCommand(w io.Writer, act string) {
	typed := strings.ReplaceAll(act, ".", " ")
	fmt.Fprintf(w, "masamong: unknown command %q\n", typed)
	if s := suggest(typed); s != "" {
		fmt.Fprintf(w, "  did you mean: masamong %s\n", s)
	}
	fmt.Fprintln(w, "  see all commands: masamong help")
}
