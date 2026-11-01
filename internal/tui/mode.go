// Package tui decides rich vs plain output (ADR-15). TUI never owns Actions.
package tui

// UseRich is the TTY decision. Callers fold NO_COLOR/CI into noColor so tests can pin the flag.
func UseRich(output string, stdoutTTY bool, noColor bool) bool {
	if output == "json" || noColor {
		return false
	}
	return stdoutTTY
}
