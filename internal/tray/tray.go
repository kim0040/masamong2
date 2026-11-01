// Package tray is the menu bar (macOS) / notification area (Windows, Linux) icon: it shows whether the
// service runs, is paused or is down, and offers open console, pause/resume, restart, start at login,
// stop and quit. It talks to the daemon through the console API with the local bootstrap token, like
// `masamong stop`. Menu text comes from locales/ui (tray.*).
package tray

import (
	"encoding/json"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"masamong/locales"
)

// Options configure Run.
type Options struct {
	DataRoot string
	Lang     string                            // empty = the system language
	Token    func() (string, error)            // the console bootstrap token
	OnceURL  func(bind string) (string, error) // one-time sign-in link for "open console"
}

// strings loads tray.* for lang, falling back to English.
func texts(lang string) map[string]string {
	out := map[string]string{}
	for _, l := range []string{"en", lang} {
		raw, err := locales.UIFS.ReadFile("ui/" + l + ".json")
		if err != nil {
			continue
		}
		m := map[string]string{}
		if json.Unmarshal(raw, &m) == nil {
			for k, v := range m {
				if strings.HasPrefix(k, "tray.") {
					out[k] = v
				}
			}
		}
	}
	return out
}

// SystemLang guesses the desktop language (ko or en).
func SystemLang() string {
	for _, k := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := os.Getenv(k); v != "" {
			if strings.HasPrefix(strings.ToLower(v), "ko") {
				return "ko"
			}
			if !strings.HasPrefix(v, "C") {
				return "en"
			}
		}
	}
	if runtime.GOOS == "darwin" {
		if out, err := exec.Command("defaults", "read", "-g", "AppleLanguages").Output(); err == nil && strings.Contains(string(out), "ko") {
			first := strings.TrimSpace(strings.Trim(strings.TrimSpace(string(out)), "()"))
			if strings.HasPrefix(strings.Trim(first, "\" ,\n"), "ko") {
				return "ko"
			}
		}
	}
	return "en"
}
