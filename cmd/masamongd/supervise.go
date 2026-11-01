package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"masamong/internal/application"
	"masamong/internal/launcher"
	"path/filepath"
)

// With --supervise, masamongd does not serve itself: it runs the daemon as a child and starts it again
// when it ends abnormally (a Go runtime fatal error, running out of memory, a kill). The launcher, the
// menu bar icon and `masamong start` all start the daemon this way, so a crash does not leave the bot off
// on any operating system. A clean stop ends the supervisor too; an operator restart is a quick exit
// with launcher.ExitRestart that the supervisor answers at once.
//
// A daemon that keeps failing is not restarted forever: after crashLimit abnormal ends within
// crashWindow the supervisor stops and leaves the reason for the next start (launcher.CrashNote).

const (
	crashLimit  = 5
	crashWindow = 10 * time.Minute
	stableAfter = 10 * time.Minute // a run this long resets the wait between restarts
	maxBackoff  = time.Minute
	// how often daemon.json is read to learn the address this run serves on: it starts at watchMin and
	// doubles up to watchMax while the answer is not final (the daemon is still starting, or serves no web)
	watchMin = time.Second
	watchMax = 30 * time.Second
)

// supervisedEnv marks the child with the supervisor's PID: it exits with launcher.ExitRestart instead of
// re-executing itself, and records the supervisor in daemon.json.
const supervisedEnv = "_MASAMONG_SUPERVISED"

type supervisor struct {
	self      string
	args      []string
	dataRoot  string
	log       io.Writer
	now       func() time.Time
	sleep     func(time.Duration, <-chan os.Signal) bool // false = interrupted
	readInfo  func(string) (launcher.DaemonInfo, bool)
	stopGrace time.Duration // zero = childStopGrace; session end always uses sessionEndGrace
	// watch intervals; zero means watchMin / watchMax (tests shrink them)
	watchFrom, watchTo time.Duration
}

func runSupervisor(dataRoot string, args []string) int {
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "masamongd supervisor:", err)
		return 1
	}
	if real, err := filepath.EvalSymlinks(self); err == nil {
		self = real // the program-update journal lives beside the real files (application.InstallDir)
	}
	sv := &supervisor{self: self, args: args, dataRoot: dataRoot, log: os.Stderr, now: time.Now, sleep: sleepOrSignal}
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	finished := make(chan struct{})
	cleanup, err := supervisorNotifications(sigs, finished)
	if err != nil {
		sv.say("shutdown notifications unavailable: %v", err)
	}
	defer cleanup()
	defer close(finished) // release an ENDSESSION callback before joining its message loop
	return sv.run(sigs)
}

// sessionEndSignal asks for the shorter budget Windows allows at confirmed session end.
type sessionEndSignal struct{}

func (sessionEndSignal) Signal()        {}
func (sessionEndSignal) String() string { return "session ending" }

const sessionEndGrace = 4 * time.Second

// childStopGrace is the daemon's own shutdown budget (lifecycle delay + reply wait + job wait) plus room for svc.Close, so a normal Windows kill lands at 60s and starts only after the stop API succeeds.
func childStopGrace() time.Duration {
	budget := shutdownLifecycleDelay + shutdownReplyWait + shutdownJobWait
	const withClose = 60 * time.Second
	if budget+time.Second > withClose {
		return budget + time.Second
	}
	return withClose
}

func sleepOrSignal(d time.Duration, sigs <-chan os.Signal) bool {
	select {
	case <-time.After(d):
		return true
	case <-sigs:
		return false
	}
}

func (sv *supervisor) say(format string, args ...any) {
	fmt.Fprintf(sv.log, sv.now().UTC().Format(time.RFC3339)+" supervisor: "+format+"\n", args...)
}

func (sv *supervisor) run(sigs <-chan os.Signal) int {
	var crashes []time.Time
	backoff := time.Second
	bind := ""
	for {
		if _, err := os.Stat(filepath.Join(filepath.Dir(sv.self), ".masamong-update.json")); !os.IsNotExist(err) {
			if err := application.RecoverUpdateAt(filepath.Dir(sv.self)); err != nil {
				// the daemon starts anyway: it finds the same journal, and its maintenance page tells the
				// operator what to do (no daemon at all would leave nobody to ask)
				sv.say("program update recovery failed: %v", err)
			}
		}
		// Pin the resolved folder even when the caller used an environment/default root.
		args := append(append([]string(nil), sv.args...), "--data-root", sv.dataRoot)
		cmd := exec.Command(sv.self, args...)
		cmd.Env = append(os.Environ(), supervisedEnv+"="+strconv.Itoa(os.Getpid()))
		if bind != "" {
			cmd.Env = append(cmd.Env, "_MASAMONG_RESTART_BIND="+bind)
		}
		tail := &tailBuffer{max: 8 << 10}
		cmd.Stdout = sv.log
		cmd.Stderr = io.MultiWriter(sv.log, tail)
		childAttr(cmd)
		started := sv.now()
		if err := cmd.Start(); err != nil {
			sv.say("cannot start the daemon: %v", err)
			return 1
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		// Every run is watched until daemon.json names THIS process with its address: a restarted daemon
		// that could not take the old port serves on another, and the next restart must follow it. A
		// run without a web address keeps asking, but ever more rarely.
		every, most := sv.watchFrom, sv.watchTo
		if every <= 0 {
			every = watchMin
		}
		if most <= 0 {
			most = watchMax
		}
		watch := time.NewTimer(every)
		watchC := watch.C
		stopping := false
		sessionEnding := false
		var activeGrace time.Duration
		var stopTimer *time.Timer
		var stopC <-chan time.Time
		var stopResult <-chan error
		var cancelStop context.CancelFunc
		var err error
		armKill := func(d time.Duration) {
			if stopTimer == nil {
				stopTimer = time.NewTimer(d)
			} else {
				stopTimer.Reset(d)
			}
			stopC = stopTimer.C
		}
	wait:
		for {
			select {
			case err = <-done:
				break wait
			case sig := <-sigs:
				_, ending := sig.(sessionEndSignal)
				if stopping {
					if ending && !sessionEnding {
						sessionEnding = true
						activeGrace = sessionEndGrace
						if fromSignal, _ := stopKillPlan(true); fromSignal {
							armKill(activeGrace)
						}
						sv.say("session ending; waiting for child (up to %s)", activeGrace)
					} else if resignalStop() {
						_ = terminate(context.Background(), cmd.Process, sig, sv.dataRoot)
					}
					continue
				}
				sessionEnding = ending
				activeGrace = sv.stopGrace
				if activeGrace <= 0 {
					activeGrace = childStopGrace()
				}
				if sessionEnding {
					activeGrace = sessionEndGrace
				}
				stopping = true
				if fromSignal, _ := stopKillPlan(sessionEnding); fromSignal {
					armKill(activeGrace)
				}
				ctx, cancel := context.WithTimeout(context.Background(), min(activeGrace, 3*time.Second))
				cancelStop = cancel
				result := make(chan error, 1)
				stopResult = result
				go func() { result <- terminate(ctx, cmd.Process, sig, sv.dataRoot) }()
			case stopErr := <-stopResult:
				stopResult = nil
				if stopErr != nil {
					sv.say("graceful stop unavailable; killing child: %v", stopErr)
					_ = cmd.Process.Kill()
					stopC = nil
					continue
				}
				fromSignal, afterSuccess := stopKillPlan(sessionEnding)
				if afterSuccess {
					armKill(activeGrace)
				}
				if fromSignal || afterSuccess || stopTimer != nil {
					sv.say("graceful stop requested; waiting for child (up to %s)", activeGrace)
				} else {
					sv.say("graceful stop requested; waiting for child")
				}
			case <-stopC:
				stopC = nil
				sv.say("graceful stop deadline reached; killing child")
				_ = cmd.Process.Kill()
			case <-watchC:
				readInfo := sv.readInfo
				if readInfo == nil {
					readInfo = launcher.ReadInfo
				}
				if info, ok := readInfo(sv.dataRoot); ok && info.PID == cmd.Process.Pid {
					bind = info.Bind // empty: this run serves no web address, so a restart should not ask for the old one
					if bind != "" {
						watchC = nil
						break
					}
				}
				every = min(every*2, most)
				watch.Reset(every)
			}
		}
		watch.Stop()
		if stopTimer != nil {
			stopTimer.Stop()
		}
		if cancelStop != nil {
			cancelStop()
		}
		code := exitCode(err)
		switch {
		case stopping || code == 0:
			return 0
		case code == launcher.ExitRestart:
			sv.say("restarting the daemon as asked")
			continue
		case code == launcher.ExitLocked:
			return code // another daemon owns the folder; starting again would only fail the same way
		}
		now := sv.now()
		ran := now.Sub(started)
		if ran >= stableAfter {
			backoff = time.Second
		}
		recent := crashes[:0]
		for _, at := range crashes {
			if now.Sub(at) < crashWindow {
				recent = append(recent, at)
			}
		}
		crashes = append(recent, now)
		note := launcher.CrashNote{At: now.UTC().Format(time.RFC3339), Reason: describeExit(err), Detail: tail.fatalLine(), Restarts: len(crashes)}
		if len(crashes) >= crashLimit {
			note.GaveUp = true
			_ = launcher.WriteCrashNote(sv.dataRoot, note)
			sv.say("the daemon ended abnormally %d times in %s (%s); not restarting it again", len(crashes), crashWindow, note.Reason)
			return 1
		}
		_ = launcher.WriteCrashNote(sv.dataRoot, note)
		sv.say("the daemon ended abnormally (%s) after %s; starting it again in %s", note.Reason, ran.Round(time.Second), backoff)
		if !sv.sleep(backoff, sigs) {
			return 0
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode() // -1 when a signal ended it
	}
	return -1
}

func describeExit(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ProcessState.String() // "exit status 2", "signal: killed"
	}
	if err != nil {
		return err.Error()
	}
	return "exit status 0"
}

// tailBuffer keeps the last bytes the daemon wrote to stderr, where Go prints a fatal error.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if over := len(t.b) - t.max; over > 0 {
		t.b = t.b[over:]
	}
	return len(p), nil
}

// fatalLine is the last "panic:" or "fatal error:" line, clipped. The daemon's own log lines are JSON and
// go to its log file; what reaches stderr outside them is the runtime's report.
func (t *tailBuffer) fatalLine() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	lines := strings.Split(string(t.b), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if strings.HasPrefix(l, "panic:") || strings.HasPrefix(l, "fatal error:") {
			if r := []rune(l); len(r) > 200 {
				l = string(r[:200]) + "…"
			}
			return l
		}
	}
	return ""
}
