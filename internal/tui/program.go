package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/table"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/mattn/go-runewidth"

	"masamong/internal/action"
	"masamong/internal/application"
)

// Interactive screens (ADR-15 §CLI): the TUI never owns Actions — it calls the same
// Service.Execute as the plain CLI and re-runs on events from Service.Subscribe.

// Executor is shared by offline Services and authenticated daemon clients.
type Executor interface {
	Execute(context.Context, action.Request) action.Result
	Subscribe() (<-chan application.Event, func())
	Msg(string, string, ...any) string
	HasMessage(string) bool
}

type Mode string

const (
	ModeDoctor Mode = "doctor"
	ModeJobs   Mode = "jobs"
)

type eventMsg application.Event
type resultMsg action.Result

const (
	defaultWidth  = 80
	defaultHeight = 24
)

type model struct {
	ctx           context.Context
	svc           Executor
	mode          Mode
	locale        string
	res           action.Result
	table         table.Model
	doctor        viewport.Model
	width         int
	height        int
	notice        string
	events        <-chan application.Event
	cancel        func()
	quit          bool
	confirmCancel bool
}

// RunInteractive drives a doctor or jobs screen until the user presses q. Output must be a TTY;
// callers fall back to Render when it is not.
func RunInteractive(ctx context.Context, svc Executor, mode Mode, locale string, in io.Reader, out io.Writer) error {
	ch, cancel := svc.Subscribe()
	defer cancel()
	m := model{
		ctx:    ctx,
		svc:    svc,
		mode:   mode,
		locale: locale,
		width:  defaultWidth,
		height: defaultHeight,
		events: ch,
		cancel: cancel,
	}
	m.doctor = newDoctorViewport(defaultWidth, m.bodyHeight(defaultHeight))
	m.table = table.New(table.WithColumns(jobColumns(defaultWidth)), table.WithWidth(defaultWidth), table.WithFocused(true), table.WithHeight(m.bodyHeight(defaultHeight)))
	st := table.DefaultStyles()
	// Column widths are terminal-cell widths. Removing the default two-cell
	// horizontal padding keeps the table inside the width reported by Tea.
	st.Header = lipgloss.NewStyle().Bold(true).Faint(true)
	st.Cell = lipgloss.NewStyle()
	st.Selected = st.Selected.Bold(true).Foreground(lipgloss.Cyan)
	m.table.SetStyles(st)
	p := tea.NewProgram(m, tea.WithContext(ctx), tea.WithInput(in), tea.WithOutput(out))
	_, err := p.Run()
	return err
}

func (m model) Init() tea.Cmd { return tea.Batch(m.refresh(), m.waitEvent()) }

func (m model) refresh() tea.Cmd {
	return func() tea.Msg {
		act := "runtime.doctor"
		if m.mode == ModeJobs {
			act = "jobs.list"
		}
		return resultMsg(m.svc.Execute(m.ctx, action.Request{Action: act, Locale: m.locale, RequestID: "tui"}))
	}
}

func (m model) waitEvent() tea.Cmd {
	return func() tea.Msg {
		select {
		case ev, ok := <-m.events:
			if !ok {
				return nil
			}
			return eventMsg(ev)
		case <-m.ctx.Done():
			return tea.Quit()
		}
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = max(1, msg.Width), max(1, msg.Height)
		if m.mode == ModeDoctor {
			m.ensureDoctorViewport()
			m.doctor.SetWidth(m.width)
			m.doctor.SetHeight(m.bodyHeight(m.height))
			if m.res.Action != "" && m.res.OK {
				m.doctor.SetContent(doctorView(m.res, m.svc, m.locale))
			}
		} else {
			m.table.SetColumns(jobColumns(m.width))
			m.table.SetWidth(m.width)
			m.table.SetHeight(m.bodyHeight(m.height))
		}
		return m, nil
	case noticeMsg:
		m.notice = string(msg)
		return m, m.refresh()
	case resultMsg:
		m.res = action.Result(msg)
		if m.mode == ModeJobs {
			m.table.SetRows(jobRows(m.res))
		} else if m.res.OK {
			m.ensureDoctorViewport()
			m.doctor.SetContent(doctorView(m.res, m.svc, m.locale))
		}
		return m, nil
	case eventMsg:
		if (m.mode == ModeJobs && msg.Kind == "job") || (m.mode == ModeDoctor && (msg.Kind == "config" || msg.Kind == "provider" || msg.Kind == "discord")) {
			return m, tea.Batch(m.refresh(), m.waitEvent())
		}
		return m, m.waitEvent()
	case tea.KeyPressMsg:
		key := msg.String()
		// q remains an unconditional exit key, including while a destructive
		// action confirmation is on screen.
		if key == "q" || key == "ctrl+c" {
			m.quit = true
			return m, tea.Quit
		}
		if m.confirmCancel {
			switch strings.ToLower(key) {
			case "y", "enter":
				m.confirmCancel = false
				m.notice = ""
				return m, m.jobAction("jobs.cancel")
			case "n", "esc":
				m.confirmCancel = false
				m.notice = ""
				return m, nil
			default:
				return m, nil
			}
		}
		if m.mode == ModeDoctor {
			m.ensureDoctorViewport()
			switch key {
			case "home", "g":
				m.doctor.GotoTop()
				return m, nil
			case "end", "G", "shift+g":
				m.doctor.GotoBottom()
				return m, nil
			case "r":
				m.notice = ""
				return m, m.refresh()
			}
			var cmd tea.Cmd
			m.doctor, cmd = m.doctor.Update(msg)
			return m, cmd
		}
		switch key {
		case "q", "esc", "ctrl+c":
			m.quit = true
			return m, tea.Quit
		case "r":
			m.notice = ""
			return m, m.refresh()
		case "c":
			if m.mode == ModeJobs {
				if len(m.table.SelectedRow()) > 0 {
					m.confirmCancel = true
					m.notice = m.cancelPrompt()
				}
				return m, nil
			}
		case "t":
			if m.mode == ModeJobs {
				return m, m.jobAction("jobs.retry")
			}
		}
	}
	if m.mode == ModeJobs {
		var cmd tea.Cmd
		m.table, cmd = m.table.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m model) jobAction(act string) tea.Cmd {
	row := m.table.SelectedRow()
	if len(row) == 0 {
		return nil
	}
	id := row[0]
	return func() tea.Msg {
		raw, _ := json.Marshal(map[string]any{"id": id})
		res := m.svc.Execute(m.ctx, action.Request{Action: act, Locale: m.locale, Input: raw, RequestID: "tui"})
		if !res.OK && res.Error != nil {
			return noticeMsg(fmt.Sprintf("%s: %s", res.Error.Code, res.Error.Message))
		}
		return noticeMsg(fmt.Sprintf("%s → %s", act, id))
	}
}

type noticeMsg string

func (m model) View() tea.View {
	width := m.width
	if width <= 0 {
		width = defaultWidth
	}
	height := m.height
	if height <= 0 {
		height = defaultHeight
	}
	bodyHeight := m.bodyHeight(height)

	title := m.svc.Msg(m.locale, "doctor.title")
	if m.mode == ModeJobs {
		title = m.svc.Msg(m.locale, "jobs.title")
	}
	header := fitLine(title+"  "+time.Now().Format("15:04:05"), width)
	footer := m.svc.Msg(m.locale, "cli.doctor.hint")
	if m.mode == ModeJobs {
		footer = m.svc.Msg(m.locale, "cli.jobs.hint")
	} else {
		// Keep the key legend visible even when the doctor content fits. It is
		// deliberately language-neutral while the surrounding hint is localized.
		footer = "↑/↓ PgUp/PgDn  " + footer
	}
	footer = fitLine(footer, width)

	var body string
	if m.mode == ModeJobs {
		jobs := m.table
		jobs.SetWidth(width)
		jobs.SetHeight(bodyHeight)
		switch {
		case m.res.Action == "":
			body = viewportBlock(width, bodyHeight, styleMuted.Render(m.svc.Msg(m.locale, "state.loading")))
		case !m.res.OK && m.res.Error != nil:
			body = viewportBlock(width, bodyHeight, styleErr.Render(m.res.Error.Code)+" "+m.res.Error.Message)
		case len(jobs.Rows()) == 0:
			body = viewportBlock(width, bodyHeight, styleMuted.Render(m.svc.Msg(m.locale, "jobs.empty")))
		default:
			body = jobs.View()
		}
	} else {
		doctor := m.doctor
		if doctor.Width() == 0 || doctor.Height() == 0 {
			doctor = newDoctorViewport(width, bodyHeight)
		} else {
			doctor.SetWidth(width)
			doctor.SetHeight(bodyHeight)
		}
		switch {
		case m.res.Action == "":
			body = m.svc.Msg(m.locale, "state.loading")
		case !m.res.OK && m.res.Error != nil:
			body = m.res.Error.Code + " " + m.res.Error.Message
		default:
			body = doctorView(m.res, m.svc, m.locale)
		}
		doctor.SetContent(body)
		body = doctor.View()
	}

	parts := []string{styleTitle.Render(header), "", body}
	if m.notice != "" {
		parts = append(parts, styleWarn.Render(fitLine(m.notice, width)))
	}
	parts = append(parts, styleMuted.Render(footer))

	content := strings.Join(parts, "\n")
	v := tea.NewView(content)
	v.AltScreen = true
	return v
}

func doctorView(res action.Result, svc Executor, locale string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %v  %s %v  %s %v\n\n",
		styleKey.Render(svc.Msg(locale, "overview.instance")), res.Output["instance"],
		styleKey.Render(svc.Msg(locale, "overview.profile")), res.Output["profile"],
		styleKey.Render(svc.Msg(locale, "overview.revision")), res.Output["revision"])
	feats, _ := res.Output["features"].(map[string]any)
	names := make([]string, 0, len(feats))
	for k := range feats {
		names = append(names, k)
	}
	sort.Strings(names)
	type featureRow struct {
		name       string
		statusCode string
		status     string
		reason     string
	}
	rows := make([]featureRow, 0, len(names))
	nameWidth := 0
	for _, name := range names {
		m, _ := feats[name].(map[string]any)
		machineStatus, _ := m["status"].(string)
		machineReason, _ := m["reason"].(string)
		displayStatus := doctorDisplayStatus(machineStatus, machineReason)
		name = localizedCatalogValue(svc, locale, "feature.name."+name, name)
		reason := localizedCatalogValue(svc, locale, "features.reason."+machineReason, machineReason)
		if text, _ := m["reason_text"].(string); text != "" {
			reason = text
		}
		statusLabel := localizedCatalogValue(svc, locale, "state."+displayStatus, displayStatus)
		rows = append(rows, featureRow{name: name, statusCode: displayStatus, status: statusLabel, reason: reason})
		nameWidth = max(nameWidth, runewidth.StringWidth(name))
	}
	for _, row := range rows {
		line := fmt.Sprintf("  %s %s", statusGlyph(row.statusCode, row.status), pad(row.name, nameWidth))
		if row.reason != "" {
			line += "  " + styleMuted.Render(row.reason)
		}
		b.WriteString(line + "\n")
	}
	if att, ok := res.Output["attention"].([]map[string]any); ok && len(att) > 0 {
		b.WriteString("\n" + styleWarn.Render(svc.Msg(locale, "overview.attention")) + "\n")
		for _, a := range att {
			item := str(a["item"])
			item = localizedCatalogValue(svc, locale, "feature.name."+item, item)
			reason := localizedCatalogValue(svc, locale, "features.reason."+str(a["reason"]), str(a["reason"]))
			fmt.Fprintf(&b, "  • %s — %s\n", item, reason)
		}
	}
	return b.String()
}

// doctorDisplayStatus keeps the action result's machine status intact while
// making a not_implemented capability read as unavailable to a person.
func doctorDisplayStatus(status, reason string) string {
	if status == "not_implemented" || reason == "not_implemented" {
		return "unavailable"
	}
	if status == "" {
		return "unknown"
	}
	return status
}

func localizedCatalogValue(svc Executor, locale, key, fallback string) string {
	if fallback == "" {
		return ""
	}
	if svc == nil || !svc.HasMessage(key) {
		return fallback
	}
	return svc.Msg(locale, key)
}

func newDoctorViewport(width, height int) viewport.Model {
	v := viewport.New(viewport.WithWidth(max(1, width)), viewport.WithHeight(max(1, height)))
	v.SoftWrap = true
	v.FillHeight = true
	return v
}

func viewportBlock(width, height int, content string) string {
	v := newDoctorViewport(width, height)
	v.SetContent(content)
	return v.View()
}

func (m *model) ensureDoctorViewport() {
	if m.doctor.Width() == 0 || m.doctor.Height() == 0 {
		width := m.width
		if width <= 0 {
			width = 80
		}
		height := m.bodyHeight(m.height)
		m.doctor = newDoctorViewport(width, height)
	}
	m.doctor.SoftWrap = true
	m.doctor.FillHeight = true
}

const (
	headerLines = 2 // title + the breathing room below it
	footerLines = 1
)

func (m model) bodyHeight(height int) int {
	reserved := headerLines + footerLines
	if m.notice != "" {
		reserved++
	}
	return max(1, height-reserved)
}

func (m model) cancelPrompt() string {
	if m.svc == nil {
		return "Cancel job? [y/enter] [n/esc]"
	}
	return fmt.Sprintf("%s? [y/enter] [n/esc]", m.svc.Msg(m.locale, "jobs.cancel"))
}

func fitLine(s string, width int) string {
	s = strings.NewReplacer("\n", " ", "\r", " ").Replace(s)
	return runewidth.Truncate(s, max(1, width), "…")
}

// statusGlyph pairs a shape with the localized word so colour is never the only signal.
func statusGlyph(st, label string) string {
	switch st {
	case "ready":
		return styleOK.Render("● " + pad(label, 14))
	case "error":
		return styleErr.Render("✗ " + pad(label, 14))
	case "disabled", "unavailable":
		return styleMuted.Render("○ " + pad(label, 14))
	default:
		return styleWarn.Render("◐ " + pad(label, 14))
	}
}

func jobColumns(width int) []table.Column {
	preferred := []int{18, 14, 10, 6, 28}
	minimum := []int{4, 4, 5, 1, 8}
	shrinkOrder := []int{4, 0, 1, 3, 2}
	if width < len(preferred) {
		widths := make([]int, len(preferred))
		for i := 0; i < width; i++ {
			widths[i] = 1
		}
		return jobColumnsWithWidths(widths)
	}
	widths := append([]int(nil), preferred...)
	total := sum(widths)
	for total > width {
		changed := false
		for _, i := range shrinkOrder {
			if widths[i] <= minimum[i] {
				continue
			}
			widths[i]--
			total--
			changed = true
			if total <= width {
				break
			}
		}
		if !changed {
			// Very narrow terminals cannot show every column at its preferred
			// minimum. Shrink in the same stable order down to one cell.
			for _, i := range shrinkOrder {
				if widths[i] <= 1 {
					continue
				}
				widths[i]--
				total--
				changed = true
				if total <= width {
					break
				}
			}
		}
		if !changed {
			break
		}
	}
	// Use spare room for the columns that were shortened, up to their
	// preferred widths. This keeps normal 80-column tables readable.
	for total < width {
		changed := false
		for _, i := range []int{0, 1, 4, 2, 3} {
			if widths[i] >= preferred[i] {
				continue
			}
			widths[i]++
			total++
			changed = true
			if total == width {
				break
			}
		}
		if !changed {
			break
		}
	}
	return jobColumnsWithWidths(widths)
}

func jobColumnsWithWidths(widths []int) []table.Column {
	titles := []string{"id", "type", "status", "attempt", "progress"}
	cols := make([]table.Column, len(titles))
	for i := range titles {
		cols[i] = table.Column{Title: titles[i], Width: widths[i]}
	}
	return cols
}

func sum(values []int) int {
	total := 0
	for _, value := range values {
		total += value
	}
	return total
}

func jobRows(res action.Result) []table.Row {
	list, _ := res.Output["jobs"].([]map[string]any)
	rows := make([]table.Row, 0, len(list))
	for _, j := range list {
		progress := str(j["progress_label"])
		if progress == "" {
			progress = str(j["updated_at"])
		}
		rows = append(rows, table.Row{str(j["id"]), str(j["type"]), str(j["status"]), str(j["attempt"]), progress})
	}
	return rows
}

func str(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}
