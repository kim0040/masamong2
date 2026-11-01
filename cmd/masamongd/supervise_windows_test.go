//go:build windows

package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"masamong/internal/launcher"
	"masamong/internal/transport/web"
)

func TestWindowsStopWithoutAConsoleNeverUsesTheDefaultFolder(t *testing.T) {
	root, other := t.TempDir(), t.TempDir()
	t.Setenv("MASAMONG_DATA_ROOT", other)
	if _, err := web.EnsureToken(root); err != nil {
		t.Fatal(err)
	}
	if _, err := web.EnsureToken(other); err != nil {
		t.Fatal(err)
	}
	if err := launcher.WriteInfo(other, launcher.DaemonInfo{PID: os.Getpid(), Bind: "127.0.0.1:1"}); err != nil {
		t.Fatal(err)
	}
	p, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Release()
	if err := terminate(context.Background(), p, os.Interrupt, root); err == nil || !strings.Contains(err.Error(), "record unavailable") {
		t.Fatalf("missing child console: %v", err)
	}
	if _, ok := launcher.ReadInfo(other); !ok {
		t.Fatal("modified other instance record")
	}
	if web.TokenPath(root) == web.TokenPath(other) {
		t.Fatal("authentication paths collided")
	}
}

func TestWindowsSupervisorStopsItsChildThroughConsole(t *testing.T) {
	testWindowsSupervisorStop(t, "80", false, true, "", false)
}

func TestWindowsSupervisorKillsOnlyAfterGrace(t *testing.T) {
	testWindowsSupervisorStop(t, "79", false, false, "deadline reached", false)
}

func TestWindowsSupervisorFallsBackWhenAuthenticationIsMissing(t *testing.T) {
	testWindowsSupervisorStop(t, "79", true, false, "authentication file unavailable", false)
}

func TestWindowsSupervisorUsesTheSessionEndBudget(t *testing.T) {
	testWindowsSupervisorStop(t, "79", false, false, "deadline reached", true)
}

func testWindowsSupervisorStop(t *testing.T, script string, removeToken, clean bool, logReason string, ending bool) {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "data")
	t.Setenv(fakeDaemonEnv, dir)
	t.Setenv("MASAMONG_FAKE_SCRIPT", script)
	t.Setenv("MASAMONG_DATA_ROOT", filepath.Join(t.TempDir(), "other"))
	self, _ := os.Executable()
	log := &lockedBuffer{}
	sv := &supervisor{self: self, dataRoot: root, log: log, now: time.Now, stopGrace: 700 * time.Millisecond}
	if ending {
		sv.stopGrace = 10 * time.Second
	}
	sigs := make(chan os.Signal, 2)
	finished := make(chan int, 1)
	go func() { finished <- sv.run(sigs) }()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, ok := launcher.ReadInfo(root); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake daemon did not become ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if removeToken {
		if err := os.Remove(web.TokenPath(root)); err != nil {
			t.Fatal(err)
		}
	}
	started := time.Now()
	var sig os.Signal = os.Interrupt
	if ending {
		sig = sessionEndSignal{}
	}
	sigs <- sig
	// A repeated ordinary stop must neither resend the mutation nor shorten the grace.
	sigs <- sig
	select {
	case code := <-finished:
		if code != 0 {
			t.Fatalf("code %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("supervisor did not finish")
	}
	grace := sv.stopGrace
	if ending {
		grace = sessionEndGrace
	}
	elapsed := time.Since(started)
	if !clean && !removeToken && elapsed < grace {
		t.Fatal("killed before grace elapsed")
	}
	if removeToken && (elapsed > 2*time.Second || strings.Contains(log.String(), "waiting for child")) {
		t.Fatal("failed request waited instead of killing")
	}
	if ending && elapsed > 7*time.Second {
		t.Fatal("session end used the ordinary 40-second budget")
	}
	if ending {
		text := log.String()
		if !strings.Contains(text, "up to "+sessionEndGrace.String()) || strings.Contains(text, "up to "+sv.stopGrace.String()) {
			t.Fatalf("log did not follow the session-end grace: %s", text)
		}
	}
	_, err := os.Stat(filepath.Join(dir, "clean"))
	if (err == nil) != clean {
		t.Fatalf("clean marker: %v, want %v", err, clean)
	}
	if logReason != "" && !strings.Contains(log.String(), logReason) {
		t.Fatalf("missing fallback reason %q", logReason)
	}
	if runs, _ := os.ReadFile(filepath.Join(dir, "runs")); len(runs) != 1 {
		t.Fatalf("restarted on stop: %d runs", len(runs))
	}
}

func TestSessionWindowQueryCancellationCloseAndConfirmedEnd(t *testing.T) {
	for _, confirmed := range []bool{false, true} {
		t.Run(map[bool]string{false: "close", true: "end-session"}[confirmed], func(t *testing.T) {
			sigs := make(chan os.Signal, 2)
			finished := make(chan struct{})
			hwnd, cleanup, err := startSessionWindow(sigs, finished)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			defer close(finished)
			send := sessionUser32.NewProc("SendMessageW")
			if r, _, _ := send.Call(hwnd, wmQueryEndSession, 0, 0); r != 1 {
				t.Fatal("QUERY must return TRUE")
			}
			send.Call(hwnd, wmEndSession, 0, 0)
			select {
			case <-sigs:
				t.Fatal("cancelled shutdown stopped supervisor")
			default:
			}
			if confirmed {
				// A full ordinary-signal buffer must not discard the shorter shutdown budget.
				sigs <- os.Interrupt
				sigs <- os.Interrupt
				postMessage.Call(hwnd, wmEndSession, 1, 0)
			} else {
				postMessage.Call(hwnd, wmClose, 0, 0)
			}
			deadline := time.After(time.Second)
			for {
				select {
				case sig := <-sigs:
					_, ending := sig.(sessionEndSignal)
					if ending == confirmed {
						return
					}
				case <-deadline:
					t.Fatal("window did not signal stop")
				}
			}
		})
	}
}

func TestWindowsStopKillPlan(t *testing.T) {
	from, after := stopKillPlan(false)
	if from || !after {
		t.Fatal("ordinary stop must kill only after the API succeeds")
	}
	from, after = stopKillPlan(true)
	if !from || after {
		t.Fatal("session end must keep its budget from the signal")
	}
	if resignalStop() {
		t.Fatal("a repeated ordinary stop must not send the API again")
	}
}

func TestWindowsKillClockStartsAfterTheStopSucceeds(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "data")
	t.Setenv(fakeDaemonEnv, dir)
	t.Setenv("MASAMONG_FAKE_SCRIPT", "79")
	t.Setenv("MASAMONG_FAKE_STOP_DELAY_MS", "500")
	t.Setenv("MASAMONG_DATA_ROOT", filepath.Join(t.TempDir(), "other"))
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	log := &lockedBuffer{}
	const grace = 1500 * time.Millisecond
	sv := &supervisor{self: self, dataRoot: root, log: log, now: time.Now, stopGrace: grace}
	sigs := make(chan os.Signal, 1)
	finished := make(chan int, 1)
	go func() { finished <- sv.run(sigs) }()
	waitForDaemon(t, root)
	sigs <- os.Interrupt
	var finishedAt time.Time
	select {
	case code := <-finished:
		finishedAt = time.Now()
		if code != 0 {
			t.Fatalf("code %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("supervisor did not finish")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "asked"))
	if err != nil {
		t.Fatal(err)
	}
	asked, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	waited := finishedAt.Sub(time.Unix(0, asked))
	if waited < grace-200*time.Millisecond {
		t.Fatalf("killed %s after the stop succeeded, want at least %s", waited, grace)
	}
	if !strings.Contains(log.String(), "deadline reached") {
		t.Fatalf("missing deadline: %s", log.String())
	}
}

func TestWindowsSuccessfulStopUsesTheDaemonBudget(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "data")
	t.Setenv(fakeDaemonEnv, dir)
	t.Setenv("MASAMONG_FAKE_SCRIPT", "82")
	t.Setenv("MASAMONG_DATA_ROOT", filepath.Join(t.TempDir(), "other"))
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	log := &lockedBuffer{}
	sv := &supervisor{self: self, dataRoot: root, log: log, now: time.Now}
	sigs := make(chan os.Signal, 1)
	finished := make(chan int, 1)
	go func() { finished <- sv.run(sigs) }()
	waitForDaemon(t, root)
	sigs <- os.Interrupt
	want := "up to " + childStopGrace().String()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(log.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("log never showed %s: %s", want, log.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(time.Second)
	if text := log.String(); strings.Contains(text, "deadline") || strings.Contains(text, "killing") || strings.Contains(text, launcher.StopGrace.String()) {
		t.Fatalf("killed inside the daemon budget or armed the CLI wait: %s", text)
	}
	if err := os.WriteFile(filepath.Join(dir, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-finished:
		if code != 0 {
			t.Fatalf("code %d, log %s", code, log.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("supervisor did not finish")
	}
	if text := log.String(); strings.Contains(text, "deadline") || strings.Contains(text, "killing") {
		t.Fatalf("killed a child that exited on its own: %s", text)
	}
}

func TestWindowsSessionEndShrinksTheLoggedGrace(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "data")
	t.Setenv(fakeDaemonEnv, dir)
	t.Setenv("MASAMONG_FAKE_SCRIPT", "79")
	t.Setenv("MASAMONG_DATA_ROOT", filepath.Join(t.TempDir(), "other"))
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	log := &lockedBuffer{}
	sv := &supervisor{self: self, dataRoot: root, log: log, now: time.Now, stopGrace: 10 * time.Second}
	sigs := make(chan os.Signal, 2)
	finished := make(chan int, 1)
	go func() { finished <- sv.run(sigs) }()
	waitForDaemon(t, root)
	sigs <- os.Interrupt
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(log.String(), "up to 10s") {
		if time.Now().After(deadline) {
			t.Fatalf("ordinary grace was not logged: %s", log.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	shrunk := time.Now()
	sigs <- sessionEndSignal{}
	select {
	case code := <-finished:
		if code != 0 {
			t.Fatalf("code %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("supervisor did not finish")
	}
	elapsed := time.Since(shrunk)
	if elapsed < sessionEndGrace || elapsed > 7*time.Second {
		t.Fatalf("session end took %s", elapsed)
	}
	text := log.String()
	ordinary := strings.LastIndex(text, "up to 10s")
	short := strings.LastIndex(text, "up to 4s")
	if short < 0 || short < ordinary {
		t.Fatalf("log did not follow the reduced grace: %s", text)
	}
}

func waitForDaemon(t *testing.T, root string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, ok := launcher.ReadInfo(root); ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("fake daemon did not become ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
