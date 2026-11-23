package application

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
)

// gateState blocks the first WithAttrs a test's handler sees until released, so a test can hold
// OpenPractice at a known point in its opening work.
type gateState struct {
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

// gatedHandler wraps a slog handler; the first WithAttrs call waits on the shared gate.
type gatedHandler struct {
	slog.Handler
	g *gateState
}

func (h gatedHandler) WithAttrs(as []slog.Attr) slog.Handler {
	h.g.once.Do(func() { close(h.g.entered); <-h.g.release })
	return gatedHandler{Handler: h.Handler.WithAttrs(as), g: h.g}
}

func (h gatedHandler) WithGroup(name string) slog.Handler {
	return gatedHandler{Handler: h.Handler.WithGroup(name), g: h.g}
}

// Two services may be open on one practice folder while the newer one closes (a console reopen
// overlapping an earlier instance): the earlier still-open service must stay findable, and a
// closing one is skipped.
func TestPracticeRegistryKeepsEachOpenService(t *testing.T) {
	root := filepath.Join(t.TempDir(), "practice")
	first := &Service{DataRoot: root, practice: true}
	second := &Service{DataRoot: root, practice: true}
	openPractices.add(first)
	openPractices.add(second)
	t.Cleanup(func() {
		openPractices.remove(first)
		openPractices.remove(second)
	})
	if got := openPractices.find(root); got != second {
		t.Fatalf("the newest open practice service is not found: %p", got)
	}
	// a service still registered but already closing is not "the open one": the earlier service is
	second.lifeMu.Lock()
	second.closing = true
	second.lifeMu.Unlock()
	if got := openPractices.find(root); got != first {
		t.Fatalf("a closing service shadowed the still-open one: %p", got)
	}
	// and once the newer service's Close removes it, the earlier open service is found again
	second.closing = false
	openPractices.remove(second)
	if got := openPractices.find(root); got != first {
		t.Fatalf("the still-open earlier service was lost under the closed one: %p", got)
	}
	openPractices.remove(first)
	if got := openPractices.find(root); got != nil {
		t.Fatal("an empty practice root still finds a service")
	}
}

// A privacy change that runs while the practice service is still opening must reach the existing
// fallback (the practice DB opened directly), not the half-open service, and once the service is
// ready it is the registered one.
func TestOpenPracticeRegistersAfterItIsReady(t *testing.T) {
	s := privacyService(t)
	g := &gateState{entered: make(chan struct{}), release: make(chan struct{})}
	type opened struct {
		p   *Service
		err error
	}
	done := make(chan opened, 1)
	go func() {
		p, err := OpenPractice(context.Background(), s.DataRoot, "en", slog.New(gatedHandler{Handler: slog.DiscardHandler, g: g}))
		done <- opened{p, err}
	}()
	<-g.entered // the service is mid-open: its store is up, its initialization is not finished
	root := PracticeRoot(s.DataRoot)
	if got := openPractices.find(root); got != nil {
		close(g.release)
		t.Fatalf("a half-open practice service is registered: %p", got)
	}
	var seen *Service
	if err := s.applyPracticePrivacy(func(p *Service) error { seen = p; return nil }); err != nil {
		close(g.release)
		t.Fatal(err)
	}
	if seen == nil {
		close(g.release)
		t.Fatal("the direct-DB fallback did not run for the opening practice service")
	}
	close(g.release)
	out := <-done
	if out.err != nil {
		t.Fatal(out.err)
	}
	t.Cleanup(func() { _ = out.p.Close() })
	if seen == out.p {
		t.Fatal("a privacy change reached a half-open practice service")
	}
	if got := openPractices.find(root); got != out.p {
		t.Fatal("a fully opened practice service is not registered")
	}
}
