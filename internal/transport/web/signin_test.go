//go:build !noweb

package web

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"masamong/internal/launcher"
)

func TestSigninCommandsNameThisInstallation(t *testing.T) {
	def := signinCommands(launcher.OSDataRoot())
	if strings.Contains(def["open"], "--data-root") || !strings.HasSuffix(def["open"], " open") || !strings.HasSuffix(def["token"], " admin token") {
		t.Fatalf("default install: %v", def)
	}
	other := signinCommands(filepath.Join(t.TempDir(), "try root"))
	if !strings.Contains(other["open"], "--data-root") || !strings.Contains(other["token"], "--data-root") {
		t.Fatalf("other data root must be named: %v", other)
	}
}

// Each shell gets a command it can run: PowerShell needs & before a quoted program path and cmd.exe
// does not know it; the home folder is named by the shell's variable so a pasted command works and the
// sign-in screen does not print the account's folder.
func TestQuotingPerShell(t *testing.T) {
	const home = `C:\Users\홍길동`
	for _, c := range []struct {
		name string
		sh   shell
		path string
		home string
		want string
	}{
		{"posix home", shellPosix, "/home/me/.masamong-try", "/home/me", `"$HOME/.masamong-try"`},
		{"posix dollar", shellPosix, "/home/me/a b/$x", "/home/me", `"$HOME/a b/\$x"`},
		{"posix space", shellPosix, "/opt/my app", "/home/me", `'/opt/my app'`},
		{"posix plain", shellPosix, "/opt/masamong", "/home/me", `/opt/masamong`},
		{"ps plain", shellPowerShell, `C:\tools\masamong.exe`, home, `C:\tools\masamong.exe`},
		{"ps space", shellPowerShell, `C:\Program Files\Masamong\masamong.exe`, home, `"C:\Program Files\Masamong\masamong.exe"`},
		{"ps home", shellPowerShell, home + `\AppData\Roaming\Masamong`, home, `"$env:USERPROFILE\AppData\Roaming\Masamong"`},
		{"ps home in other case", shellPowerShell, strings.ToUpper(home) + `\x y`, home, `"$env:USERPROFILE\x y"`},
		{"ps dollar", shellPowerShell, `D:\a$b\c d`, home, "\"D:\\a`$b\\c d\""},
		{"cmd home", shellCmd, home + `\AppData\Roaming\Masamong`, home, `"%USERPROFILE%\AppData\Roaming\Masamong"`},
		{"cmd space", shellCmd, `C:\Program Files\Masamong`, home, `"C:\Program Files\Masamong"`},
		{"cmd folder next to home", shellCmd, home + `x\y z`, home, `"` + home + `x\y z"`},
	} {
		if got := quoteFor(c.sh, c.path, c.home); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	if got := shellPowerShell.program(`C:\Program Files\Masamong\masamong.exe`); got != `& "C:\Program Files\Masamong\masamong.exe"` {
		t.Errorf("PowerShell runs a quoted program with &: %q", got)
	}
	if got := shellCmd.program(`C:\Program Files\Masamong\masamong.exe`); strings.HasPrefix(got, "&") {
		t.Errorf("cmd has no &: %q", got)
	}
	if got := shellPowerShell.program(`C:\tools\masamong.exe`); strings.HasPrefix(got, "&") {
		t.Errorf("an unquoted path needs no &: %q", got)
	}
}

func TestSigninOffersTheCmdFormOnlyOnWindows(t *testing.T) {
	got := signinCommands(filepath.Join(t.TempDir(), "try root"))
	_, hasCmd := got["open_cmd"]
	switch {
	case runtime.GOOS != "windows" && hasCmd:
		t.Fatalf("only Windows has a cmd form: %v", got)
	// on Windows the cmd form is offered only when it differs from the PowerShell one (a quoted program
	// path needs & in PowerShell); with a plain "masamong" both read the same and open_cmd is left out
	case runtime.GOOS == "windows" && (strings.Contains(got["open_cmd"], "&") || !strings.Contains(got["open"], "--data-root")):
		t.Fatalf("PowerShell form in open, cmd form in open_cmd: %v", got)
	case runtime.GOOS == "windows" && hasCmd && got["open_cmd"] == got["open"]:
		t.Fatalf("an identical cmd form is not offered: %v", got)
	}
}

func TestSigninHelpIsOnTheSignInScreen(t *testing.T) {
	_, ts := testServer(t)
	_, out, _ := do(t, client(), "GET", ts.URL+"/api/session", nil, nil)
	sg, ok := out["signin"].(map[string]any)
	if !ok || sg["open"] == nil || sg["token"] == nil {
		t.Fatalf("anonymous session needs the sign-in commands: %v", out)
	}
	home, _ := os.UserHomeDir()
	for _, k := range []string{"open", "token", "open_cmd", "token_cmd"} {
		cmd, _ := sg[k].(string)
		if home != "" && cmd != "" && strings.Contains(strings.ToLower(cmd), strings.ToLower(home)) {
			t.Fatalf("sign-in command %s reveals the home folder: %v", k, sg)
		}
	}
	if out["platform"] != runtime.GOOS {
		t.Fatalf("the session names the platform the service runs on: %v", out["platform"])
	}
}
