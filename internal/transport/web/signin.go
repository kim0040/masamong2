//go:build !noweb

package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"masamong/internal/launcher"
)

// signinCommands are the terminal commands that sign a browser into this installation again: open
// (the launcher's one-time link) and admin token (prints the token to paste). They name this
// installation's program and data folder when those differ from the defaults, so a copied command
// reaches this instance and not another one (a test instance next to the real one, for example).
//
// On Windows the main command is written for PowerShell (the default terminal), where a quoted program
// path needs a leading &. open_cmd and token_cmd are the same commands for cmd.exe, which does not
// know &; they are left out when they read the same.
func signinCommands(dataRoot string) map[string]string {
	if runtime.GOOS == "windows" {
		ps, cmd := signinFor(dataRoot, shellPowerShell), signinFor(dataRoot, shellCmd)
		out := map[string]string{"open": ps["open"], "token": ps["token"]}
		if cmd["open"] != ps["open"] {
			out["open_cmd"], out["token_cmd"] = cmd["open"], cmd["token"]
		}
		return out
	}
	return signinFor(dataRoot, shellPosix)
}

func signinFor(dataRoot string, sh shell) map[string]string {
	cli := "masamong"
	if exe, err := os.Executable(); err == nil {
		name := "masamong"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		sibling := filepath.Join(filepath.Dir(exe), name)
		if st, err := os.Stat(sibling); err == nil && !st.IsDir() && !onPath(sibling) {
			cli = sh.program(sibling)
		}
	}
	if dataRoot != "" && !samePath(dataRoot, launcher.OSDataRoot()) {
		cli += " --data-root " + sh.quote(dataRoot)
	}
	return map[string]string{"open": cli + " open", "token": cli + " admin token"}
}

// onPath reports whether `masamong` on PATH is this file.
func onPath(file string) bool {
	found, err := exec.LookPath("masamong")
	return err == nil && samePath(found, file)
}

func samePath(a, b string) bool {
	ea, errA := filepath.EvalSymlinks(a)
	eb, errB := filepath.EvalSymlinks(b)
	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ea == eb
}

type shell int

const (
	shellPosix shell = iota
	shellPowerShell
	shellCmd
)

// program quotes the path of the program itself: PowerShell only runs a quoted path with & in front.
func (sh shell) program(path string) string {
	q := sh.quote(path)
	if sh == shellPowerShell && strings.HasPrefix(q, `"`) {
		return "& " + q
	}
	return q
}

func (sh shell) quote(s string) string {
	home, _ := os.UserHomeDir()
	return quoteFor(sh, s, home)
}

// quoteFor quotes a path for the shell, only when it needs it. A path in the home folder is written
// with the shell's own name for it ("$HOME/…", "$env:USERPROFILE\…", "%USERPROFILE%\…"): the command
// still works when pasted, and the sign-in screen (shown before signing in) does not reveal the
// account name or the folder layout above it.
func quoteFor(sh shell, s, home string) string {
	switch sh {
	case shellPosix:
		if home != "" && strings.HasPrefix(s, home+"/") {
			rest := strings.TrimPrefix(s, home)
			return `"$HOME` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`, "`", "\\`").Replace(rest) + `"`
		}
		if plain(s) {
			return s
		}
		return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
	case shellPowerShell:
		// inside "…" a backtick escapes ` $ and "; $env:USERPROFILE expands
		esc := strings.NewReplacer("`", "``", "$", "`$", `"`, "`\"")
		if rest, ok := underHome(s, home); ok {
			return `"$env:USERPROFILE` + esc.Replace(rest) + `"`
		}
		if plain(s) {
			return s
		}
		return `"` + esc.Replace(s) + `"`
	default: // cmd.exe: a Windows path cannot contain ", and %USERPROFILE% expands inside quotes
		if rest, ok := underHome(s, home); ok {
			return `"%USERPROFILE%` + rest + `"`
		}
		if plain(s) {
			return s
		}
		return `"` + s + `"`
	}
}

// underHome reports a path inside the home folder (compared without regard to case) and what follows
// the home folder, starting with the separator.
func underHome(s, home string) (string, bool) {
	home = strings.TrimRight(home, `\/`)
	if home == "" || len(s) <= len(home) || !strings.EqualFold(s[:len(home)], home) || (s[len(home)] != '\\' && s[len(home)] != '/') {
		return "", false
	}
	return s[len(home):], true
}

// plain paths need no quotes in any of the shells.
func plain(s string) bool {
	return s != "" && strings.Trim(s, `abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-:\`) == ""
}
