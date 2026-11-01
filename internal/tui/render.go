package tui

import (
	"fmt"
	"io"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/mattn/go-runewidth"

	"masamong/internal/action"
	"masamong/internal/present"
)

// Palette uses the terminal's own 16-colour set so it respects user themes and stays legible on
// light and dark backgrounds; status is always paired with a word, never colour alone.
var (
	styleOK     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Green)
	styleErr    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Red)
	styleWarn   = lipgloss.NewStyle().Foreground(lipgloss.Yellow)
	styleMuted  = lipgloss.NewStyle().Faint(true)
	styleTitle  = lipgloss.NewStyle().Bold(true)
	styleHeader = lipgloss.NewStyle().Bold(true).Faint(true)
	styleKey    = lipgloss.NewStyle().Foreground(lipgloss.Cyan)
)

// Render prints a TTY-rich or plain view of the same Action result. JSON is not handled here.
func Render(w io.Writer, res action.Result, color bool) {
	v := present.Build(res)
	paint := func(st lipgloss.Style, s string) string {
		if !color {
			return s
		}
		return st.Render(s)
	}
	if v.OK {
		fmt.Fprintf(w, "%s %s\n", paint(styleOK, "✓ ok"), paint(styleTitle, v.Action))
	} else {
		fmt.Fprintf(w, "%s %s\n", paint(styleErr, "✗ error"), paint(styleTitle, v.Action))
		if v.Code != "" {
			fmt.Fprintf(w, "  %s %s\n", paint(styleErr, v.Code), v.Message)
		}
		if v.Raw != "" {
			fmt.Fprintln(w, indent(v.Raw, "  "))
		}
		return
	}
	if v.Message != "" {
		fmt.Fprintf(w, "  %s\n", v.Message)
	}
	if len(v.Fields) > 0 {
		width := 0
		for _, kv := range v.Fields {
			width = max(width, runewidth.StringWidth(kv.Key))
		}
		for _, kv := range v.Fields {
			fmt.Fprintf(w, "  %s  %s\n", paint(styleKey, pad(kv.Key, width)), statusWord(kv.Key, kv.Value, color, paint))
		}
	}
	for _, t := range v.Tables {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "  %s\n", paint(styleHeader, t.Key))
		writeTable(w, t, color, paint)
	}
}

func writeTable(w io.Writer, t present.Table, color bool, paint func(lipgloss.Style, string) string) {
	widths := make([]int, len(t.Columns))
	for i, c := range t.Columns {
		widths[i] = runewidth.StringWidth(c)
	}
	for _, row := range t.Rows {
		for i, cell := range row {
			if i < len(widths) {
				widths[i] = max(widths[i], min(runewidth.StringWidth(cell), 48))
			}
		}
	}
	var head []string
	for i, c := range t.Columns {
		head = append(head, pad(c, widths[i]))
	}
	fmt.Fprintf(w, "  %s\n", paint(styleHeader, strings.Join(head, "  ")))
	for _, row := range t.Rows {
		cells := make([]string, 0, len(row))
		for i, cell := range row {
			if i >= len(widths) {
				break
			}
			cell = runewidth.Truncate(cell, 48, "…")
			cells = append(cells, statusWord(t.Columns[i], pad(cell, widths[i]), color, paint))
		}
		fmt.Fprintf(w, "  %s\n", strings.Join(cells, "  "))
	}
}

// statusWord colours known status values but keeps the text, so plain output and screen readers
// carry the same information.
func statusWord(key, val string, color bool, paint func(lipgloss.Style, string) string) string {
	if !color || (key != "status" && !strings.HasSuffix(key, ".status")) {
		return val
	}
	switch strings.TrimSpace(val) {
	case "ready", "ok", "succeeded", "done", "open", "configured":
		return paint(styleOK, val)
	case "error", "failed", "rejected":
		return paint(styleErr, val)
	case "unconfigured", "pending", "leased", "running", "partial", "degraded", "stale", "retry_wait":
		return paint(styleWarn, val)
	}
	return paint(styleMuted, val)
}

func pad(s string, width int) string {
	return s + strings.Repeat(" ", max(0, width-runewidth.StringWidth(s)))
}

func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := range lines {
		lines[i] = prefix + lines[i]
	}
	return strings.Join(lines, "\n")
}
