package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/i18n"
)

func testService() *application.Service {
	return &application.Service{I18n: i18n.New()}
}

func doctorResult(featureCount int) action.Result {
	features := make(map[string]any, featureCount)
	for i := 0; i < featureCount; i++ {
		features[fmt.Sprintf("feature-%02d", i)] = map[string]any{
			"status": "ready",
		}
	}
	return action.OK(action.Request{Action: "runtime.doctor"}, map[string]any{
		"instance": "test",
		"profile":  "local",
		"revision": 1,
		"features": features,
	})
}

func keyPress(text string, code rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Text: text, Code: code}
}

func TestDoctorViewUsesViewportAndKeepsFooterAtTerminalHeight(t *testing.T) {
	res := doctorResult(32)
	m := model{
		svc:    testService(),
		mode:   ModeDoctor,
		locale: "en",
		res:    res,
		width:  80,
		height: 24,
		doctor: newDoctorViewport(80, 21),
	}
	m.doctor.SetContent(doctorView(res, m.svc, m.locale))

	top := m.View().Content
	if got := lipgloss.Height(top); got != 24 {
		t.Fatalf("top view height = %d, want 24", got)
	}
	if !strings.Contains(top, "Press q to quit") {
		t.Fatalf("footer is missing from top view: %q", top)
	}
	if !strings.Contains(top, "feature-00") {
		t.Fatalf("top of doctor content is missing: %q", top)
	}

	next, _ := m.Update(keyPress("end", tea.KeyEnd))
	updated := next.(model)
	if updated.doctor.YOffset() == 0 {
		t.Fatal("end key did not scroll the doctor viewport")
	}
	bottom := updated.View().Content
	if got := lipgloss.Height(bottom); got != 24 {
		t.Fatalf("scrolled view height = %d, want 24", got)
	}
	if !strings.Contains(bottom, "Press q to quit") {
		t.Fatalf("footer disappeared after scrolling: %q", bottom)
	}
	if strings.Contains(bottom, "feature-00") {
		t.Fatal("scrolled view still starts at the first feature")
	}
}

func TestDoctorViewFitsNarrowHeightWithNotice(t *testing.T) {
	res := doctorResult(12)
	m := model{
		svc:    testService(),
		mode:   ModeDoctor,
		locale: "en",
		res:    res,
		width:  40,
		height: 10,
		notice: "a very long notice that must remain a single line in the footer area",
		doctor: newDoctorViewport(40, 6),
	}
	m.doctor.SetContent(doctorView(res, m.svc, m.locale))

	content := m.View().Content
	if got := lipgloss.Height(content); got != 10 {
		t.Fatalf("narrow view height = %d, want 10", got)
	}
	if got := lipgloss.Width(strings.Split(content, "\n")[0]); got > 40 {
		t.Fatalf("header width = %d, want <= 40", got)
	}
	if !strings.Contains(content, "Press q to quit") {
		t.Fatalf("narrow footer is missing: %q", content)
	}
}

func TestDoctorViewUsesMachineStatusBeforeLocalizing(t *testing.T) {
	res := action.OK(action.Request{Action: "runtime.doctor"}, map[string]any{
		"features": map[string]any{
			"fortune": map[string]any{
				"status": "unconfigured",
				"reason": "not_implemented",
			},
		},
	})

	svc := testService()
	view := doctorView(res, svc, "ko")
	if !strings.Contains(view, "사용 불가") {
		t.Fatalf("not_implemented feature should be shown as unavailable: %q", view)
	}
	if want := svc.I18n.T("ko", "features.reason.not_implemented"); want == "features.reason.not_implemented" || !strings.Contains(view, want) {
		t.Fatalf("machine reason was not localized: %q", view)
	}
}

func TestDoctorKeysKeepQuitAndRefreshAvailable(t *testing.T) {
	m := model{svc: testService(), mode: ModeDoctor, locale: "en", width: 80, height: 24, doctor: newDoctorViewport(80, 21)}

	next, cmd := m.Update(keyPress("q", 'q'))
	if !next.(model).quit || cmd == nil {
		t.Fatal("q should quit the program")
	}

	m.quit = false
	next, cmd = m.Update(keyPress("r", 'r'))
	if next.(model).notice != "" || cmd == nil {
		t.Fatal("r should clear notices and trigger a refresh")
	}
}
