package application_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"masamong/internal/application"
	"masamong/internal/clock"
	"masamong/internal/config"
	"masamong/internal/launcher"
)

func alertSvc(t *testing.T) (*application.Service, *dmGateway, func(time.Duration)) {
	t.Helper()
	svc := setupSvc(t)
	gw := &dmGateway{}
	svc.Discord = gw
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	svc.Clock = clock.Fixed{T: now}
	applyDoc(t, svc, func(d *config.Document) { d.Access.Operators = []string{"op1", "op2"}; d.I18n.DefaultLocale = "ko" })
	advance := func(d time.Duration) { now = now.Add(d); svc.Clock = clock.Fixed{T: now} }
	return svc, gw, advance
}

func (d *dmGateway) take() ([]string, []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var texts []string
	for _, c := range d.sent {
		texts = append(texts, c.Title+"\n"+c.Text+"\n"+c.Footer)
	}
	to := d.to
	d.sent, d.to = nil, nil
	return texts, to
}

func TestAlertsTellOperatorsOnceAboutANewProblem(t *testing.T) {
	svc, gw, advance := alertSvc(t)
	ctx := context.Background()
	svc.AlertTickForTest(ctx)
	texts, to := gw.take()
	if len(texts) != 2 || strings.Join(to, ",") != "op1,op2" {
		t.Fatalf("one message per operator, got %d to %v", len(texts), to)
	}
	// setupSvc has no model key: the bot cannot answer, which is serious
	if !strings.Contains(texts[0], "대화 모델 API 키") || strings.Contains(texts[0], "{") {
		t.Fatalf("message: %q", texts[0])
	}
	svc.AlertTickForTest(ctx)
	if texts, _ := gw.take(); len(texts) != 0 {
		t.Fatalf("the same problem was sent again: %q", texts)
	}
	// a serious problem still open a day later is sent once more
	advance(25 * time.Hour)
	svc.AlertTickForTest(ctx)
	if texts, _ := gw.take(); len(texts) != 2 {
		t.Fatalf("a day-old serious problem: %d messages", len(texts))
	}
	st := out(t, svc, "alerts.status", nil)
	if st["ready"] != true || st["operators"] != float64(2) {
		t.Fatalf("status: %v", st)
	}
}

func TestAlertsSayTheServiceRestartedAfterACrash(t *testing.T) {
	svc, gw, _ := alertSvc(t)
	applyDoc(t, svc, func(d *config.Document) { d.Alerts.MinSeverity = "danger" })
	ctx := context.Background()
	svc.AlertTickForTest(ctx)
	gw.take()
	// a run that never recorded a stop, then the supervisor's note and a new start
	svc.NoteStartForTest()
	if err := launcher.WriteCrashNote(svc.DataRoot, launcher.CrashNote{At: "2026-09-30T11:59:00Z", Reason: "exit status 2", Detail: "panic: boom", Restarts: 1}); err != nil {
		t.Fatal(err)
	}
	svc.NoteStartForTest()
	svc.AlertTickForTest(ctx)
	texts, _ := gw.take()
	if len(texts) != 2 || !strings.Contains(texts[0], "비정상 종료") || !strings.Contains(texts[0], "exit status 2") || strings.Contains(texts[0], "panic: boom") {
		t.Fatalf("crash message: %q", texts)
	}
	hist := out(t, svc, "runtime.history", nil)
	found := false
	for _, e := range hist["events"].([]any) {
		m := e.(map[string]any)
		if m["kind"] == "crash" && strings.Contains(m["reason"].(string), "panic: boom") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the uptime history does not say why: %v", hist["events"])
	}
	svc.AlertTickForTest(ctx)
	if texts, _ := gw.take(); len(texts) != 0 {
		t.Fatalf("the crash was sent twice: %q", texts)
	}
}

func TestAlertsCanBeTurnedOffAndTested(t *testing.T) {
	svc, gw, _ := alertSvc(t)
	applyDoc(t, svc, func(d *config.Document) { off := false; d.Alerts.Discord = &off })
	svc.AlertTickForTest(context.Background())
	if texts, _ := gw.take(); len(texts) != 0 {
		t.Fatalf("alerts are off but sent %q", texts)
	}
	// the test message still checks that the operators can be reached
	if r := exec(svc, "alerts.test", nil); !r.OK {
		t.Fatal(r.Error)
	}
	texts, _ := gw.take()
	if len(texts) != 2 || !strings.Contains(texts[0], "테스트") {
		t.Fatalf("test message: %q", texts)
	}
	applyDoc(t, svc, func(d *config.Document) { d.Access.Operators = nil })
	if r := exec(svc, "alerts.test", nil); r.OK || r.Error.Args["reason"] != "no_operators" {
		t.Fatalf("no operators: %+v", r)
	}
}
