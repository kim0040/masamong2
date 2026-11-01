package main

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"masamong/internal/launcher"
	"masamong/internal/transport/web"
)

// The test binary doubles as the supervised daemon: with fakeDaemonEnv set it plays the next exit code
// from a script (one per start) and exits.
const fakeDaemonEnv = "MASAMONG_FAKE_DAEMON"

func TestMain(m *testing.M) {
	if dir := os.Getenv(fakeDaemonEnv); dir != "" {
		fakeDaemon(dir)
	}
	os.Exit(m.Run())
}

func fakeDaemon(dir string) {
	root := filepath.Join(dir, "data")
	got, ok := lastArg(os.Args, "--data-root")
	_ = os.WriteFile(filepath.Join(dir, "pinned-root"), []byte(got), 0o600)
	if !ok || got != root {
		os.Exit(97)
	}
	runs, _ := os.ReadFile(filepath.Join(dir, "runs"))
	n := len(runs)
	_ = os.WriteFile(filepath.Join(dir, "runs"), append(runs, 'x'), 0o600)
	_ = os.Remove(filepath.Join(dir, "data", "daemon.json"))
	if pid, _ := strconv.Atoi(os.Getenv(supervisedEnv)); pid != os.Getppid() {
		os.Exit(99)
	}
	if b := os.Getenv("_MASAMONG_RESTART_BIND"); b != "" {
		_ = os.WriteFile(filepath.Join(dir, "bind"), []byte(b), 0o600)
	}
	script := strings.Split(os.Getenv("MASAMONG_FAKE_SCRIPT"), ",")
	code, _ := strconv.Atoi(script[min(n, len(script)-1)])
	if code == 81 {
		_ = os.WriteFile(filepath.Join(dir, "ready-signal"), nil, 0o600)
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
		<-ch
		_ = os.WriteFile(filepath.Join(dir, "signaled"), nil, 0o600)
		time.Sleep(800 * time.Millisecond)
		os.Exit(0)
	}
	if code == 78 || code == 79 || code == 80 || code == 82 {
		fakeServingDaemon(dir, code)
		return
	}
	if code == 2 {
		fmt.Fprintln(os.Stderr, "panic: runtime error: index out of range [3] with length 3")
	}
	if code == 77 {
		// a daemon without a web address: daemon.json names it but has no bind
		_ = os.MkdirAll(filepath.Join(dir, "data"), 0o700)
		_ = launcher.WriteInfo(filepath.Join(dir, "data"), launcher.DaemonInfo{PID: os.Getpid()})
		time.Sleep(time.Second)
		os.Exit(0)
	}
	if code == 75 {
		// a daemon that served on an address before its operator restart (each run on the next port, as a
		// restart that found the old one taken would)
		_ = os.MkdirAll(filepath.Join(dir, "data"), 0o700)
		_ = launcher.WriteInfo(filepath.Join(dir, "data"), launcher.DaemonInfo{PID: os.Getpid(), Bind: "127.0.0.1:" + strconv.Itoa(9911+n)})
		delay := 1500 * time.Millisecond
		if ms, err := strconv.Atoi(os.Getenv("MASAMONG_FAKE_SLEEP_MS")); err == nil && ms > 0 {
			delay = time.Duration(ms) * time.Millisecond
		}
		time.Sleep(delay)
		_ = os.Remove(filepath.Join(dir, "data", "daemon.json")) // a daemon that ends takes its file along
	}
	os.Exit(code)
}

func fakeServingDaemon(dir string, code int) {
	root := filepath.Join(dir, "data")
	if code == 78 {
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	token, err := web.EnsureToken(root)
	if err != nil {
		os.Exit(98)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		os.Exit(98)
	}
	stop := make(chan struct{}, 1)
	if code == 82 {
		go func() {
			release := filepath.Join(dir, "release")
			for {
				if _, err := os.Stat(release); err == nil {
					stop <- struct{}{}
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
		}()
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/health":
			fmt.Fprint(w, `{"ok":true}`)
		case "/api/login":
			// Exercise the actual token file and request path without logging its contents.
			b, _ := io.ReadAll(r.Body)
			if !bytes.Contains(b, []byte(token)) {
				w.WriteHeader(401)
				return
			}
			fmt.Fprint(w, `{"ok":true,"csrf":"test-csrf"}`)
		case "/api/session":
			fmt.Fprintf(w, `{"authenticated":true,"data_root":%q}`, filepath.ToSlash(root))
		case "/api/action":
			b, _ := io.ReadAll(r.Body)
			if !bytes.Contains(b, []byte("runtime.stop")) || r.Header.Get("X-CSRF-Token") != "test-csrf" {
				w.WriteHeader(403)
				return
			}
			if ms, err := strconv.Atoi(os.Getenv("MASAMONG_FAKE_STOP_DELAY_MS")); err == nil && ms > 0 {
				time.Sleep(time.Duration(ms) * time.Millisecond)
			}
			_ = os.WriteFile(filepath.Join(dir, "asked"), []byte(strconv.FormatInt(time.Now().UnixNano(), 10)), 0o600)
			fmt.Fprint(w, `{"ok":true}`)
			w.(http.Flusher).Flush()
			if code == 80 {
				stop <- struct{}{}
			}
		default:
			fmt.Fprint(w, `{"ok":true}`)
		}
	})}
	_ = launcher.WriteInfo(root, launcher.DaemonInfo{PID: os.Getpid(), Bind: l.Addr().String()})
	go srv.Serve(l)
	<-stop
	_ = srv.Close()
	_ = os.WriteFile(filepath.Join(dir, "clean"), nil, 0o600)
	os.Exit(0)
}

func runFake(t *testing.T, script string) (code int, runs int, waits []time.Duration, dir string) {
	t.Helper()
	dir = t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(fakeDaemonEnv, dir)
	t.Setenv("MASAMONG_FAKE_SCRIPT", script)
	// the daemon's stdout and stderr are copied by two goroutines when neither is a file, so the test's
	// buffer takes a lock (the real one is os.Stderr, which is safe to share)
	log := &lockedBuffer{}
	sv := &supervisor{self: self, dataRoot: filepath.Join(dir, "data"), log: log, now: time.Now,
		sleep: func(d time.Duration, _ <-chan os.Signal) bool { waits = append(waits, d); return true }}
	code = sv.run(make(chan os.Signal))
	b, _ := os.ReadFile(filepath.Join(dir, "runs"))
	return code, len(b), waits, dir
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func lastArg(args []string, name string) (string, bool) {
	var got string
	ok := false
	for i := 0; i+1 < len(args); i++ {
		if args[i] == name {
			got = args[i+1]
			ok = true
		}
	}
	return got, ok
}

func TestChildStopGraceExceedsDaemonBudget(t *testing.T) {
	budget := shutdownLifecycleDelay + shutdownReplyWait + shutdownJobWait
	grace := childStopGrace()
	if grace <= budget {
		t.Fatalf("grace %s does not cover daemon budget %s", grace, budget)
	}
	if grace < 55*time.Second || grace > 65*time.Second {
		t.Fatalf("grace %s, want about 60s", grace)
	}
}

func TestSupervisorPinsDataRootOnTheChildArgv(t *testing.T) {
	dir := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "data")
	t.Setenv(fakeDaemonEnv, dir)
	t.Setenv("MASAMONG_FAKE_SCRIPT", "0")
	sv := &supervisor{
		self: self, args: []string{"--data-root", filepath.Join(dir, "decoy")},
		dataRoot: root, log: &lockedBuffer{}, now: time.Now,
		sleep: func(time.Duration, <-chan os.Signal) bool { return true },
	}
	if code := sv.run(make(chan os.Signal)); code != 0 {
		t.Fatalf("code %d", code)
	}
	got, err := os.ReadFile(filepath.Join(dir, "pinned-root"))
	if err != nil || string(got) != root {
		t.Fatalf("pinned %q, err %v", got, err)
	}
}

func TestSupervisorRestartsACrashedDaemonAndStopsWithIt(t *testing.T) {
	code, runs, waits, dir := runFake(t, "2,2,0")
	if code != 0 || runs != 3 {
		t.Fatalf("code %d after %d runs, want 0 after 3", code, runs)
	}
	if len(waits) != 2 || waits[0] != time.Second || waits[1] != 2*time.Second {
		t.Fatalf("waits %v, want 1s then 2s", waits)
	}
	note, ok := launcher.TakeCrashNote(filepath.Join(dir, "data"))
	if !ok || note.Restarts != 2 || note.GaveUp || !strings.Contains(note.Reason, "exit status 2") || !strings.HasPrefix(note.Detail, "panic: runtime error") {
		t.Fatalf("crash note %+v", note)
	}
}

func TestSupervisorGivesUpOnADaemonThatKeepsFailing(t *testing.T) {
	code, runs, _, dir := runFake(t, "1")
	if code != 1 || runs != crashLimit {
		t.Fatalf("code %d after %d runs, want 1 after %d", code, runs, crashLimit)
	}
	note, ok := launcher.TakeCrashNote(filepath.Join(dir, "data"))
	if !ok || !note.GaveUp || note.Restarts != crashLimit {
		t.Fatalf("crash note %+v", note)
	}
}

func TestSupervisorAnswersARestartOnTheSameAddress(t *testing.T) {
	code, runs, waits, dir := runFake(t, "75,0")
	if code != 0 || runs != 2 || len(waits) != 0 {
		t.Fatalf("code %d, runs %d, waits %v", code, runs, waits)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "bind")); string(b) != "127.0.0.1:9911" {
		t.Fatalf("restarted on %q, want the previous address", b)
	}
	if _, ok := launcher.TakeCrashNote(filepath.Join(dir, "data")); ok {
		t.Fatal("an operator restart is not a crash")
	}
}

// Each run is watched until daemon.json names that run's process with its address, then daemon.json is
// left alone; the next run is watched again, so a restart that had to take another port is followed.
func TestSupervisorFollowsTheAddressOfEveryRunAndStopsAskingOnceKnown(t *testing.T) {
	dir := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(fakeDaemonEnv, dir)
	t.Setenv("MASAMONG_FAKE_SCRIPT", "75,75,0")
	t.Setenv("MASAMONG_FAKE_SLEEP_MS", "400")
	hits := 0 // reads that found an address
	sv := &supervisor{self: self, dataRoot: filepath.Join(dir, "data"), log: &lockedBuffer{}, now: time.Now,
		sleep:     func(time.Duration, <-chan os.Signal) bool { return true },
		watchFrom: 10 * time.Millisecond, watchTo: 40 * time.Millisecond,
		readInfo: func(root string) (launcher.DaemonInfo, bool) {
			info, ok := launcher.ReadInfo(root)
			if ok && info.Bind != "" {
				hits++
			}
			return info, ok
		},
	}
	if code := sv.run(make(chan os.Signal)); code != 0 {
		t.Fatalf("supervisor exit code %d", code)
	}
	// each of the two runs lives 400 ms and writes its address at once; one read finds it, then no more
	if hits != 2 {
		t.Fatalf("daemon.json was read %d times with an address in it, want once per run (2)", hits)
	}
	// the third run is started on the address the SECOND run reported (a fallback port), not the first's
	if b, _ := os.ReadFile(filepath.Join(dir, "bind")); string(b) != "127.0.0.1:9912" {
		t.Fatalf("restarted on %q, want the latest observed address 127.0.0.1:9912", b)
	}
}

// A daemon without a web address never shows a bind: the supervisor keeps asking, but backs off.
func TestSupervisorBacksOffWhenTheDaemonHasNoWebAddress(t *testing.T) {
	dir := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(fakeDaemonEnv, dir)
	t.Setenv("MASAMONG_FAKE_SCRIPT", "77")
	reads := 0
	sv := &supervisor{self: self, dataRoot: filepath.Join(dir, "data"), log: &lockedBuffer{}, now: time.Now,
		sleep:     func(time.Duration, <-chan os.Signal) bool { return true },
		watchFrom: 10 * time.Millisecond, watchTo: 200 * time.Millisecond,
		readInfo: func(root string) (launcher.DaemonInfo, bool) {
			reads++
			return launcher.ReadInfo(root)
		},
	}
	if code := sv.run(make(chan os.Signal)); code != 0 {
		t.Fatalf("supervisor exit code %d", code)
	}
	// a fixed 10 ms poll over the daemon's second of life would read ~100 times; with doubling up to
	// 200 ms: 10, 20, 40, 80, 160, 200, 200, ... a dozen at most
	if reads > 20 || reads < 3 {
		t.Fatalf("%d reads of daemon.json in one second, want a backed-off handful", reads)
	}
}

func TestSupervisorLeavesAFolderAnotherDaemonOwns(t *testing.T) {
	if code, runs, _, _ := runFake(t, "3"); code != launcher.ExitLocked || runs != 1 {
		t.Fatalf("code %d after %d runs", code, runs)
	}
}
