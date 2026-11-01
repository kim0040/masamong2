package launcher

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Starting at login registers the Masamong command with the operating system for the current user
// only (no administrator rights):
//
//	macOS    ~/Library/LaunchAgents/app.masamong.<id>.plist (launchd, RunAtLoad; the icon is restarted after a crash)
//	Linux    ~/.config/autostart/masamong-<id>.desktop on a desktop session,
//	         otherwise a systemd user unit ~/.config/systemd/user/masamong-<id>.service
//	Windows  HKCU\Software\Microsoft\Windows\CurrentVersion\Run value Masamong-<id>
//
// <id> comes from the data folder, so two instances on one computer do not collide. On a desktop the
// registered command is `masamong tray` (menu bar icon, which starts the service under its supervisor,
// masamongd --supervise, so a crashed service comes back on every system); without one it is the
// service itself, which systemd restarts.

// Autostart is the current registration.
type Autostart struct {
	Supported bool     `json:"supported"`
	Enabled   bool     `json:"enabled"`
	Method    string   `json:"method"` // launchd | xdg | systemd | registry
	Path      string   `json:"path,omitempty"`
	Command   []string `json:"command,omitempty"`
	Note      string   `json:"note,omitempty"`
}

func instanceID(dataRoot string) string {
	abs, err := filepath.Abs(dataRoot)
	if err != nil {
		abs = dataRoot
	}
	sum := sha256.Sum256([]byte(abs))
	return hex.EncodeToString(sum[:4])
}

// executable is os.Executable (tests point it at a fake install).
var executable = os.Executable

// Binaries returns the masamong CLI and masamongd paths installed next to the running program.
func Binaries() (cli, daemon string, err error) {
	self, err := executable()
	if err != nil {
		return "", "", err
	}
	if r, err := filepath.EvalSymlinks(self); err == nil {
		self = r
	}
	dir := filepath.Dir(self)
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	cli, daemon = filepath.Join(dir, "masamong"+ext), filepath.Join(dir, "masamongd"+ext)
	if _, err := os.Stat(cli); err != nil {
		return "", "", fmt.Errorf("masamong not found next to %s", self)
	}
	return cli, daemon, nil
}

func desktopSession() bool {
	return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != "" || os.Getenv("XDG_CURRENT_DESKTOP") != ""
}

// autostartCommand is what runs at login.
func autostartCommand(dataRoot string, desktop bool) ([]string, error) {
	cli, daemon, err := Binaries()
	if err != nil {
		return nil, err
	}
	if desktop {
		// `tray` shows the menu bar icon and starts the service; a build without the icon just starts it
		return []string{cli, "--data-root", dataRoot, "tray"}, nil
	}
	return []string{daemon, "--data-root", dataRoot, "--web"}, nil
}

func home() string {
	h, _ := os.UserHomeDir()
	return h
}

func autostartPath(dataRoot string) (string, string) {
	id := instanceID(dataRoot)
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home(), "Library", "LaunchAgents", "app.masamong."+id+".plist"), "launchd"
	case "windows":
		return `HKCU\Software\Microsoft\Windows\CurrentVersion\Run\Masamong-` + id, "registry"
	}
	if desktopSession() {
		cfg := os.Getenv("XDG_CONFIG_HOME")
		if cfg == "" {
			cfg = filepath.Join(home(), ".config")
		}
		return filepath.Join(cfg, "autostart", "masamong-"+id+".desktop"), "xdg"
	}
	if runtime.GOOS != "linux" {
		return "", "unsupported" // a headless BSD has no systemd; use rc.d or cron @reboot
	}
	cfg := os.Getenv("XDG_CONFIG_HOME")
	if cfg == "" {
		cfg = filepath.Join(home(), ".config")
	}
	return filepath.Join(cfg, "systemd", "user", "masamong-"+id+".service"), "systemd"
}

// AutostartStatus reports whether this instance starts at login.
func AutostartStatus(dataRoot string) Autostart {
	path, method := autostartPath(dataRoot)
	if method == "unsupported" {
		return Autostart{Method: method, Note: "start at login needs a desktop session or systemd on " + runtime.GOOS + "; run `masamong start` from rc.d or cron @reboot"}
	}
	st := Autostart{Supported: true, Method: method, Path: path}
	switch method {
	case "registry":
		key, name := filepath.Dir(path), filepath.Base(path)
		out, err := exec.Command("reg", "query", key, "/v", name).CombinedOutput()
		st.Enabled = err == nil && strings.Contains(string(out), name)
	default:
		_, err := os.Stat(path)
		st.Enabled = err == nil
	}
	if cmd, err := autostartCommand(dataRoot, method != "systemd"); err == nil {
		st.Command = cmd
	} else {
		st.Note = err.Error()
	}
	return st
}

// SetAutostart turns starting at login on or off.
func SetAutostart(dataRoot string, enabled bool) (Autostart, error) {
	path, method := autostartPath(dataRoot)
	if method == "unsupported" {
		st := AutostartStatus(dataRoot)
		if !enabled {
			return st, nil
		}
		return st, errors.New(st.Note)
	}
	if !enabled {
		switch method {
		case "registry":
			_ = exec.Command("reg", "delete", filepath.Dir(path), "/v", filepath.Base(path), "/f").Run()
		case "systemd":
			if os.Getenv("MASAMONG_AUTOSTART_NO_EXEC") == "" {
				_ = exec.Command("systemctl", "--user", "disable", filepath.Base(path)).Run()
			}
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return AutostartStatus(dataRoot), err
			}
		default:
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return AutostartStatus(dataRoot), err
			}
		}
		return AutostartStatus(dataRoot), nil
	}
	cmd, err := autostartCommand(dataRoot, method != "systemd")
	if err != nil {
		return AutostartStatus(dataRoot), err
	}
	var body string
	switch method {
	case "launchd":
		var args strings.Builder
		for _, a := range cmd {
			args.WriteString("    <string>" + xmlEscape(a) + "</string>\n")
		}
		body = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>app.masamong.` + instanceID(dataRoot) + `</string>
  <key>ProgramArguments</key>
  <array>
` + args.String() + `  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key>
  <dict>
    <key>SuccessfulExit</key><false/>
  </dict>
  <key>ProcessType</key><string>Interactive</string>
</dict>
</plist>
`
	case "xdg":
		body = "[Desktop Entry]\nType=Application\nName=Masamong\nComment=Start Masamong at login\nExec=" + desktopExec(cmd) +
			"\nIcon=masamong\nTerminal=false\nX-GNOME-Autostart-enabled=true\n"
	case "systemd":
		body = "[Unit]\nDescription=Masamong\nAfter=network-online.target\n\n[Service]\nExecStart=" + desktopExec(cmd) +
			"\nRestart=on-failure\nRestartSec=5\n\n[Install]\nWantedBy=default.target\n"
	case "registry":
		quoted := make([]string, len(cmd))
		for i, a := range cmd {
			quoted[i] = `"` + a + `"`
		}
		if out, err := exec.Command("reg", "add", filepath.Dir(path), "/v", filepath.Base(path), "/t", "REG_SZ", "/d", strings.Join(quoted, " "), "/f").CombinedOutput(); err != nil {
			return AutostartStatus(dataRoot), fmt.Errorf("reg add: %v: %s", err, strings.TrimSpace(string(out)))
		}
		return AutostartStatus(dataRoot), nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return AutostartStatus(dataRoot), err
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return AutostartStatus(dataRoot), err
	}
	if method == "systemd" && os.Getenv("MASAMONG_AUTOSTART_NO_EXEC") == "" {
		_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
		if out, err := exec.Command("systemctl", "--user", "enable", filepath.Base(path)).CombinedOutput(); err != nil {
			st := AutostartStatus(dataRoot)
			st.Note = "systemctl --user enable failed: " + strings.TrimSpace(string(out))
			return st, nil
		}
	}
	return AutostartStatus(dataRoot), nil
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}

// desktopExec quotes arguments for Exec= lines (desktop entries and systemd units).
func desktopExec(cmd []string) string {
	out := make([]string, len(cmd))
	for i, a := range cmd {
		if strings.ContainsAny(a, " \t\"'\\$") {
			a = `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`).Replace(a) + `"`
		}
		out[i] = a
	}
	return strings.Join(out, " ")
}
