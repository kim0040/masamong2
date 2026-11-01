// Package cli is the masamong command surface. JSON schema is independent of TTY/locale (ADR-15).
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"unicode/utf16"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/catalog"
	"masamong/internal/hostprofile"
	"masamong/internal/i18n"
	"masamong/internal/launcher"
	"masamong/internal/transport/web"
	"masamong/internal/tui"
)

type Options struct {
	DataRoot      string
	Output        string // json|table
	NoColor       bool
	NoInteractive bool
	Interactive   bool // -i: live TUI for doctor / jobs watch when stdout is a TTY
	Locale        string
	WebBuild      bool
	Usage         bool // no command, `help` or --help: a terminal gets the usage text
}

func ParseArgs(args []string) (Options, string, json.RawMessage, error) {
	opt := Options{} // empty: DecideOutput picks rich on TTY+color, JSON on pipe/CI/NO_COLOR
	var positional []string
	kv := map[string]any{}
	inputFileUsed := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--data-root" && i+1 < len(args):
			i++
			opt.DataRoot = args[i]
		case strings.HasPrefix(a, "--data-root="):
			opt.DataRoot = strings.TrimPrefix(a, "--data-root=")
		case a == "--db-backend" && i+1 < len(args):
			i++
			kv["backend"] = args[i]
		case strings.HasPrefix(a, "--db-backend="):
			kv["backend"] = strings.TrimPrefix(a, "--db-backend=")
		case a == "--dsn-ref" && i+1 < len(args):
			i++
			kv["dsn_ref"] = args[i]
		case strings.HasPrefix(a, "--dsn-ref="):
			kv["dsn_ref"] = strings.TrimPrefix(a, "--dsn-ref=")
		case a == "--output" && i+1 < len(args):
			i++
			opt.Output = args[i]
		case strings.HasPrefix(a, "--output="):
			opt.Output = strings.TrimPrefix(a, "--output=")
		case a == "--input-file":
			if i+1 >= len(args) {
				return Options{}, "", nil, fmt.Errorf("--input-file requires a path or - for standard input")
			}
			i++
			input, err := readJSONInput(args[i], os.Stdin)
			if err != nil {
				return Options{}, "", nil, err
			}
			kv["input"], inputFileUsed = input, true
		case strings.HasPrefix(a, "--input-file="):
			input, err := readJSONInput(strings.TrimPrefix(a, "--input-file="), os.Stdin)
			if err != nil {
				return Options{}, "", nil, err
			}
			kv["input"], inputFileUsed = input, true
		case a == "--locale" && i+1 < len(args):
			i++
			opt.Locale = args[i]
		case a == "--no-color":
			opt.NoColor = true
		case a == "--no-interactive":
			opt.NoInteractive = true
		case a == "-i" || a == "--interactive":
			opt.NoInteractive = false
			opt.Interactive = true
		case a == "--help" || a == "-h":
			positional = []string{"help"}
		case strings.HasPrefix(a, "--") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "--"):
			key := strings.TrimPrefix(a, "--")
			i++
			kv[key] = coerce(args[i])
		case !strings.HasPrefix(a, "-"):
			positional = append(positional, a)
		case strings.HasPrefix(a, "--") && len(a) > 2:
			kv[strings.ReplaceAll(strings.TrimPrefix(a, "--"), "-", "_")] = true // bare switch: --no-browser
		}
	}
	if len(positional) > 1 && positional[0] == "connect" {
		kv["host"] = positional[1]
	}
	// `masamong autostart on|off` is the short form of `autostart set --enabled true|false`
	if len(positional) == 2 && positional[0] == "autostart" && (positional[1] == "on" || positional[1] == "off") {
		kv["enabled"] = positional[1] == "on"
		positional = []string{"autostart", "set"}
	}
	// ADR-15: NO_COLOR and CI force the JSON Action schema, same as a pipe.
	if os.Getenv("NO_COLOR") != "" || os.Getenv("CI") != "" {
		opt.NoColor = true
	}
	opt.Usage = len(positional) == 0 || positional[0] == "help"
	act := mapAction(positional)
	if inputFileUsed && act != "tools.test" {
		return Options{}, "", nil, fmt.Errorf("--input-file is read only by `masamong tools test`")
	}
	raw, _ := json.Marshal(kv)
	return opt, act, raw, nil
}

func readJSONInput(path string, stdin io.Reader) (string, error) {
	var raw []byte
	var err error
	if path == "-" {
		raw, err = io.ReadAll(stdin)
	} else if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("--input-file requires a path or - for standard input")
	} else {
		raw, err = os.ReadFile(path)
	}
	if err != nil {
		return "", fmt.Errorf("read JSON input: %w", err)
	}
	raw, err = decodeTextFile(raw)
	if err != nil {
		return "", fmt.Errorf("--input-file: %w", err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return "", fmt.Errorf("--input-file must contain a JSON object: %w", err)
	}
	if object == nil {
		return "", fmt.Errorf("--input-file must contain a JSON object, not null")
	}
	return string(bytes.TrimSpace(raw)), nil
}

// decodeTextFile turns what Windows PowerShell 5.1 writes into UTF-8: Set-Content -Encoding UTF8 adds
// a byte-order mark, and Out-File or > write UTF-16 (little endian, with a mark). Big-endian UTF-16
// with a mark is accepted too. Anything else is taken as UTF-8.
func decodeTextFile(raw []byte) ([]byte, error) {
	switch {
	case bytes.HasPrefix(raw, []byte{0xEF, 0xBB, 0xBF}):
		return raw[3:], nil
	case bytes.HasPrefix(raw, []byte{0xFF, 0xFE}), bytes.HasPrefix(raw, []byte{0xFE, 0xFF}):
		body := raw[2:]
		if len(body)%2 != 0 {
			return nil, fmt.Errorf("the UTF-16 file has an odd number of bytes")
		}
		units := make([]uint16, len(body)/2)
		for i := range units {
			if raw[0] == 0xFF {
				units[i] = uint16(body[2*i]) | uint16(body[2*i+1])<<8
			} else {
				units[i] = uint16(body[2*i])<<8 | uint16(body[2*i+1])
			}
		}
		return []byte(string(utf16.Decode(units))), nil
	}
	return raw, nil
}

func coerce(s string) any {
	if s == "true" {
		return true
	}
	if s == "false" {
		return false
	}
	// Discord IDs are 17–19 digit snowflakes; keep those as strings so they never round-trip
	// through float64. Short integers stay numeric for count/limit style flags.
	if len(s) > 15 {
		return s
	}
	var n json.Number
	if err := json.Unmarshal([]byte(s), &n); err == nil {
		if i, err := n.Int64(); err == nil {
			return int(i)
		}
	}
	return s
}

// catalogRoutes maps the catalog's CLI token sequences ("channels ops-log", "backup restore")
// to Action IDs so every catalog entry is reachable from the CLI (ADR-15 parity with the GUI).
// Flags in Descriptor.CLI (e.g. --dry-run) are not part of the key.
func catalogRoutes() map[string]string {
	out := map[string]string{}
	for _, d := range catalog.All() {
		var toks []string
		for _, w := range strings.Fields(d.CLI) {
			if !strings.HasPrefix(w, "-") {
				toks = append(toks, w)
			}
		}
		if len(toks) == 0 {
			continue
		}
		out[strings.Join(toks, " ")] = d.ID
	}
	return out
}

func mapAction(pos []string) string {
	if len(pos) == 0 {
		return "help.version"
	}
	if pos[0] == "admin" {
		// local helpers, not Actions: resolved in run()
		if len(pos) > 1 {
			return "admin." + pos[1]
		}
		return "admin.open"
	}
	if pos[0] == "version" {
		return "help.version"
	}
	switch pos[0] {
	case "open", "connect", "start", "stop", "restart", "status", "tray":
		return "launcher." + pos[0]
	}
	// Longest catalog match first: "channels attachments add" before "channels".
	routes := catalogRoutes()
	for n := min(len(pos), 3); n >= 1; n-- {
		if id, ok := routes[strings.Join(pos[:n], " ")]; ok {
			return id
		}
	}
	// Bare group verbs default to their list/show Action.
	switch pos[0] {
	case "autostart":
		return "runtime.autostart"
	case "admins", "schedules", "people":
		if len(pos) == 1 {
			return pos[0] + ".list"
		}
	case "eval":
		if len(pos) == 1 {
			return "eval.runs"
		}
	case "finance":
		return "finance.quote"
	case "config":
		if len(pos) > 1 {
			return "config." + pos[1]
		}
		return "config.show"
	case "jobs":
		if len(pos) > 1 {
			return "jobs." + pos[1]
		}
		return "jobs.list"
	case "channels", "personas", "tools", "stats", "runtime", "providers", "models", "polls", "notices", "backup":
		if len(pos) > 1 {
			return pos[0] + "." + pos[1]
		}
		switch pos[0] {
		case "polls":
			return "polls.result"
		case "notices":
			return "notices.status"
		case "backup":
			return "backup.create"
		}
		return pos[0] + ".list"
	}
	if len(pos) == 1 {
		return pos[0]
	}
	return pos[0] + "." + pos[1]
}

func stdoutIsTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}

// DecideOutput is the ADR-15 output rule: TTY+color → rich; --output json,
// pipe, NO_COLOR, or CI → the same JSON Action schema (no ANSI).
func DecideOutput(opt Options, stdoutTTY bool) string {
	if opt.Output == "json" {
		return "json"
	}
	if opt.NoColor || !stdoutTTY {
		return "json"
	}
	if tui.UseRich(opt.Output, stdoutTTY, opt.NoColor) {
		return "rich"
	}
	return "json"
}

func Run(ctx context.Context, args []string, stdout, stderr io.Writer, webBuild bool) int {
	return run(ctx, args, stdout, stderr, webBuild, stdoutIsTTY(stdout))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, webBuild, stdoutTTY bool) int {
	opt, act, input, err := ParseArgs(args)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if strings.HasPrefix(act, "launcher.") {
		return launchCommand(ctx, act, opt, input, stdout, stderr, stdoutTTY)
	}
	opt.Output = DecideOutput(opt, stdoutTTY)
	if opt.Output != "json" {
		if opt.Usage {
			fmt.Fprint(stdout, usageText())
			return 0
		}
		if !strings.HasPrefix(act, "admin.") && !knownAction(act) {
			unknownCommand(stderr, act)
			return 2
		}
	}
	if opt.DataRoot == "" {
		// the same folder `masamong open` uses, so every command reaches the instance the console shows
		opt.DataRoot = launcher.DefaultDataRoot()
		if _, err := os.Stat(opt.DataRoot); err != nil && act == "help.version" {
			return WriteResult(stdout, opt, action.Result{OK: true, Action: act, Output: map[string]any{"name": "masamong", "version": application.Version, "web_build": webBuild}})
		}
	}
	if strings.HasPrefix(act, "admin.") {
		return adminCommand(act, opt, stdout, stderr, webBuild)
	}
	input, err = commandPaths(act, input)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	// Taking the same lock as the daemon closes the startup race: an offline command and a daemon
	// can never both open the live database. A held lock always routes to the daemon, even if its
	// console is temporarily unavailable; falling back then would fork config and privacy state.
	if launcher.Running(opt.DataRoot, "daemon.lock") {
		return daemonCommand(ctx, act, input, opt, stdout, stderr)
	}
	lock, err := launcher.AcquireLock(opt.DataRoot)
	if err != nil {
		return daemonCommand(ctx, act, input, opt, stdout, stderr)
	}
	defer lock.Release()
	applyHostProfile()
	svc, err := application.Open(opt.DataRoot, nil, webBuild)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer svc.Close()
	if opt.Interactive && opt.Output == "rich" {
		if mode, ok := interactiveMode(act); ok {
			if err := tui.RunInteractive(ctx, svc, mode, opt.Locale, os.Stdin, stdout); err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
			return 0
		}
	}
	req := action.Request{Action: act, Input: input, Locale: opt.Locale, RequestID: "cli"}
	res := svc.Execute(ctx, req)
	if act == "runtime.status" && res.OK && res.Output["maintenance"] == true {
		res.Output["message"] = "masamong is stopped for maintenance"
		if opt.Output != "json" {
			fmt.Fprintln(stdout, res.Output["message"])
			return 0
		}
	}
	return WriteResult(stdout, opt, res)
}

// interactiveMode maps the Actions that have a live screen; everything else ignores -i.
func interactiveMode(act string) (tui.Mode, bool) {
	switch act {
	case "runtime.doctor":
		return tui.ModeDoctor, true
	case "jobs.watch", "jobs.list":
		return tui.ModeJobs, true
	}
	return "", false
}

// adminCommand prints the console bootstrap token or login link. The token is a local secret:
// it is only ever written to the operator's stdout, never to logs.
func adminCommand(act string, opt Options, stdout, stderr io.Writer, webBuild bool) int {
	var bind string
	enabled := true
	if launcher.Running(opt.DataRoot, "daemon.lock") {
		info, ok := launcher.ReadInfo(opt.DataRoot)
		if !ok {
			fmt.Fprintln(stderr, i18n.New().T(opt.Locale, "features.reason.unavailable"))
			return 1
		}
		bind = info.Bind
	} else {
		lock, err := launcher.AcquireLock(opt.DataRoot)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		defer lock.Release()
		applyHostProfile()
		svc, err := application.Open(opt.DataRoot, nil, webBuild)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		bind, enabled = svc.Doc().Admin.Web.Bind, svc.Doc().Admin.Web.Enabled
		_ = svc.Close()
	}
	tok, err := web.EnsureToken(opt.DataRoot)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	out := map[string]any{"bind": bind, "web_enabled": enabled, "web_build": webBuild}
	switch act {
	case "admin.token":
		out["token"] = tok
	case "admin.open", "admin.url":
		out["url"] = web.LoginURL(bind, tok)
		if !enabled {
			out["note"] = "admin.web.enabled is false; set it to true in config and restart masamongd"
		}
	default:
		fmt.Fprintln(stderr, "masamong admin: use `admin token` or `admin open`")
		return 2
	}
	res := action.Result{OK: true, Action: act, Output: out}
	if opt.Output == "json" {
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(res)
		return 0
	}
	if v, ok := out["url"]; ok {
		fmt.Fprintf(stdout, "%v\n", v)
	} else {
		fmt.Fprintf(stdout, "%v\n", out["token"])
	}
	if note, ok := out["note"]; ok {
		fmt.Fprintf(stderr, "note: %v\n", note)
	}
	return 0
}

func WriteResult(w io.Writer, opt Options, res action.Result) int {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if opt.Output == "json" {
		_ = enc.Encode(res)
		if !res.OK {
			return 1
		}
		return 0
	}
	if opt.Output == "rich" {
		tui.Render(w, res, true)
		if !res.OK {
			return 1
		}
		return 0
	}
	// table/plain: no ANSI
	tui.Render(w, res, false)
	if !res.OK {
		return 1
	}
	return 0
}

func JSONEqualSchema(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	ab, _ := json.Marshal(x)
	bb, _ := json.Marshal(y)
	return bytes.Equal(ab, bb)
}

var hostProfileOnce sync.Once

// applyHostProfile installs the machine's tier before the CLI opens the instance itself (no daemon
// running): an offline command on a 1-core, 512 MB box then uses the small limits and honors
// MASAMONG_PERF like masamongd does, and its doctor shows the detected tier instead of the medium
// default. It runs only on these in-process paths; a command routed to the daemon uses the daemon's.
func applyHostProfile() {
	hostProfileOnce.Do(func() { hostprofile.Apply(hostprofile.Detect()) })
}
