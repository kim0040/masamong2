package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/cli"
	"masamong/internal/launcher"
	"masamong/internal/transport/web"
)

var quietLog = slog.New(slog.NewTextHandler(io.Discard, nil))

// A signal and an operator's restart request can arrive together. The answer is read once the watch
// has ended, so it is the decision the watch acted on and the read does not race its write (-race).
func TestLifecycleWatchSignalAndRequestTogether(t *testing.T) {
	for range 8 {
		requests := make(chan application.Lifecycle, 1)
		requests <- application.Lifecycle("restart")
		ctx, cancel := context.WithCancel(context.Background())
		kept := false
		w := watchLifecycle(ctx, cancel, requests, func() { kept = true }, quietLog)
		cancel() // the signal
		<-ctx.Done()
		restart := w.result()
		if restart != kept {
			t.Fatalf("restart %v, but sessions kept %v", restart, kept)
		}
	}
}

func TestLifecycleWatchRestartRequestEndsTheRun(t *testing.T) {
	requests := make(chan application.Lifecycle, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	kept := false
	w := watchLifecycle(ctx, cancel, requests, func() { kept = true }, quietLog)
	requests <- application.Lifecycle("restart")
	select {
	case <-ctx.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("a restart request did not end the run")
	}
	if !w.result() || !kept {
		t.Fatalf("restart %v, sessions kept %v", w.result(), kept)
	}
}

func TestLifecycleWatchStopRequestIsNotARestart(t *testing.T) {
	requests := make(chan application.Lifecycle, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := watchLifecycle(ctx, cancel, requests, func() { t.Error("a stop kept the restart sessions") }, quietLog)
	requests <- application.Lifecycle("stop")
	<-ctx.Done()
	if w.result() {
		t.Fatal("a stop request reported a restart")
	}
}

func TestLifecycleWatchSignalOnly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	w := watchLifecycle(ctx, cancel, make(chan application.Lifecycle), func() {}, quietLog)
	cancel()
	if w.result() {
		t.Fatal("a signal reported a restart")
	}
}

// A console that cannot listen used to os.Exit inside run, skipping every deferred step: running
// jobs and replies did not record their outcome and the database was not closed. run now returns the
// error after its deferred shutdown, and the failed run is not recorded as a clean stop.
func TestRunListenFailureReturnsAfterShutdown(t *testing.T) {
	if !web.Embedded() {
		t.Skip("built without the console: nothing listens")
	}
	dir := t.TempDir()
	var out, errb bytes.Buffer
	if code := cli.Run(context.Background(), []string{"--data-root", dir, "setup", "--instance", "gate"}, &out, &errb, false); code != 0 {
		t.Fatalf("setup %d: %s", code, errb.String())
	}
	done := make(chan error, 1)
	go func() {
		// a loopback address with an impossible port: the listener fails at once, with no other program
		// involved (a port in use falls back to a free one instead)
		_, _, err := run(dir, true, "127.0.0.1:99999", 0, quietLog)
		done <- err
	}()
	var err error
	select {
	case err = <-done:
	case <-time.After(2 * time.Minute):
		t.Fatal("run did not return after the listener failed")
	}
	if err == nil {
		t.Fatal("a listener failure was not reported")
	}
	// the deferred svc.Close ran: the data folder opens again and the history shows no clean stop
	svc, err := application.Open(dir, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	res := svc.Execute(context.Background(), action.Request{Action: "runtime.history", Input: json.RawMessage(`{}`), Locale: "en"})
	if !res.OK {
		t.Fatalf("history: %+v", res)
	}
	raw, _ := json.Marshal(res.Output["events"])
	var events []struct{ Kind string }
	if err := json.Unmarshal(raw, &events); err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Kind == "stop" || e.Kind == "restart" {
			t.Fatalf("a failed run was recorded as a clean %s: %s", e.Kind, raw)
		}
	}
}

// A record left by the previous owner of the folder (a crash leaves one) names a port that another
// program may hold by now: the daemon removes it as soon as it holds the lock, so no client sends the
// token there while this daemon starts.
func TestTakingTheDataRootRemovesTheStaleAddressRecord(t *testing.T) {
	root := t.TempDir()
	if err := launcher.WriteInfo(root, launcher.DaemonInfo{PID: 1, Bind: "127.0.0.1:1"}); err != nil {
		t.Fatal(err)
	}
	lock, err := takeDataRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	if info, ok := launcher.ReadInfo(root); ok {
		t.Fatalf("the stale record survived: %+v", info)
	}
}
