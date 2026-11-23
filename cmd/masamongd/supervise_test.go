package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"masamong/internal/launcher"
	"masamong/internal/testutil"
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

// waitForRelease keeps the fake daemon alive until the test writes a "release" file — the
// deterministic form of "stay up a while" for a child process. A child left behind by a failed
// test still gives up on its own.
func waitForRelease(dir string) {
	release := filepath.Join(dir, "release")
	for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(release); err == nil {
			return
		}
		if time.Now().After(deadline) {
			os.Exit(4)
		}
	}
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
	_ = os.Remove(filepath.Join(dir, "release")) // this run is released only for its own reason
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
		waitForRelease(dir) // takes after its signal exactly as long as the test allows
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
		time.Sleep(time.Second) // the second of life the backoff count is measured against
		os.Exit(0)
	}
	if code == 75 {
		// a daemon that served on an address before its operator restart (each run on the next port, as a
		// restart that found the old one taken would); it ends once the test has seen the address
		_ = os.MkdirAll(filepath.Join(dir, "data"), 0o700)
		_ = launcher.WriteInfo(filepath.Join(dir, "data"), launcher.DaemonInfo{PID: os.Getpid(), Bind: "127.0.0.1:" + strconv.Itoa(9911+n)})
		waitForRelease(dir)
		_ = os.Remove(filepath.Join(dir, "data", "daemon.json")) // a daemon that ends takes its file along
	}
	os.Exit(code)
}

func fakeServingDaemon(dir string, code int) {
	root := filepath.Join(dir, "data")
	if code == 78 {
		waitForRelease(dir) // a daemon that stays up until the test lets it go
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
		// answers its stop API at once but stays up until the test releases it
		go func() {
			waitForRelease(dir)
			stop <- struct{}{}
		}()
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/health":
			// answers the launcher's challenge like the real console (from the address the request
			// arrived on), or the Windows stop request is refused before it is sent
			c := r.URL.Query().Get(launcher.ChallengeParam)
			addr, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
			if !launcher.ValidChallenge(c) || !ok {
				fmt.Fprint(w, `{"ok":true}`)
				return
			}
			fmt.Fprintf(w, `{"ok":true,"proof":%q}`, launcher.EndpointProof(token, addr.String(), c))
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

// The Windows supervisor stops its child through the console's stop API (terminate in
// supervise_windows.go), so the fake console must pass the launcher's checks, endpoint proof included.
// This runs on every system so a launcher change the fake does not follow fails here, not only on
// Windows CI.
func TestFakeConsoleTakesTheChildStopRequest(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "data")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, "--data-root", root) // the root the supervisor pins on its child
	cmd.Env = append(os.Environ(), fakeDaemonEnv+"="+dir, "MASAMONG_FAKE_SCRIPT=80",
		supervisedEnv+"="+strconv.Itoa(os.Getpid()))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-exited
	})
	waitForDaemon(t, root)
	token, err := web.ReadToken(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := launcher.RequestStopChild(context.Background(), root, cmd.Process.Pid, token); err != nil {
		t.Fatalf("the fake console refused the stop request: %v", err)
	}
	select {
	case err := <-exited:
		exited <- err // for the cleanup
		if err != nil {
			t.Fatalf("fake console exit: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the fake console did not stop")
	}
	if _, err := os.Stat(filepath.Join(dir, "clean")); err != nil {
		t.Fatalf("clean marker: %v", err)
	}
}

func waitForDaemon(t *testing.T, root string) {
	t.Helper()
	testutil.WaitFor(t, 30*time.Second, "the fake daemon to become ready", func() bool {
		_, ok := launcher.ReadInfo(root)
		return ok
	})
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
	dir := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(fakeDaemonEnv, dir)
	t.Setenv("MASAMONG_FAKE_SCRIPT", "75,0")
	var waits []time.Duration
	sv := &supervisor{self: self, dataRoot: filepath.Join(dir, "data"), log: &lockedBuffer{}, now: time.Now,
		sleep:     func(d time.Duration, _ <-chan os.Signal) bool { waits = append(waits, d); return true },
		watchFrom: 10 * time.Millisecond, watchTo: 40 * time.Millisecond,
		readInfo: func(root string) (launcher.DaemonInfo, bool) {
			info, ok := launcher.ReadInfo(root)
			if ok && info.Bind != "" {
				// the address is in hand: the release file lets this run end
				_ = os.WriteFile(filepath.Join(dir, "release"), nil, 0o600)
			}
			return info, ok
		},
	}
	if code := sv.run(make(chan os.Signal)); code != 0 {
		t.Fatalf("supervisor exit code %d", code)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "runs")); len(b) != 2 || len(waits) != 0 {
		t.Fatalf("runs %d, waits %v", len(b), waits)
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
	hits := 0 // reads that found an address
	sv := &supervisor{self: self, dataRoot: filepath.Join(dir, "data"), log: &lockedBuffer{}, now: time.Now,
		sleep:     func(time.Duration, <-chan os.Signal) bool { return true },
		watchFrom: 10 * time.Millisecond, watchTo: 40 * time.Millisecond,
		readInfo: func(root string) (launcher.DaemonInfo, bool) {
			info, ok := launcher.ReadInfo(root)
			if ok && info.Bind != "" {
				hits++
				// the run's address was seen: the release file lets it end
				_ = os.WriteFile(filepath.Join(dir, "release"), nil, 0o600)
			}
			return info, ok
		},
	}
	if code := sv.run(make(chan os.Signal)); code != 0 {
		t.Fatalf("supervisor exit code %d", code)
	}
	// each run stays up until its address is read once; a read finds it, then no more
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
