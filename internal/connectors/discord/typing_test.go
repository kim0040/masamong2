package discord

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

type fakeTypingSession struct {
	mu      sync.Mutex
	calls   int
	results []error
}

func (s *fakeTypingSession) ChannelTyping(string, ...discordgo.RequestOption) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.calls <= len(s.results) {
		return s.results[s.calls-1]
	}
	return nil
}

func (s *fakeTypingSession) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func TestTypingKeepaliveStopsImmediatelyOnPermissionFailure(t *testing.T) {
	var logs bytes.Buffer
	session := &fakeTypingSession{results: []error{&discordgo.RESTError{Response: &http.Response{StatusCode: http.StatusForbidden}}}}
	k := newTypingKeepaliveInterval(session, "channel", slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})), time.Hour)
	k.start(context.Background())
	select {
	case <-k.doneCh:
	case <-time.After(time.Second):
		t.Fatal("permission failure did not seal typing")
	}
	k.start(context.Background())
	if got := session.count(); got != 1 {
		t.Fatalf("permission failure made %d typing calls", got)
	}
	if !strings.Contains(logs.String(), "consecutive_failures=1") || !strings.Contains(logs.String(), "class=permission") {
		t.Fatalf("failure class/count missing from diagnostic: %s", logs.String())
	}
}

func TestTypingKeepaliveStopsAfterThreeTransientFailures(t *testing.T) {
	var logs bytes.Buffer
	session := &fakeTypingSession{results: []error{errors.New("synthetic transient"), errors.New("synthetic transient"), errors.New("synthetic transient")}}
	k := newTypingKeepaliveInterval(session, "channel", slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})), time.Millisecond)
	k.start(context.Background())
	select {
	case <-k.doneCh:
	case <-time.After(time.Second):
		t.Fatal("three transient failures did not seal typing")
	}
	if got := session.count(); got != typingFailureLimit {
		t.Fatalf("transient failures made %d typing calls", got)
	}
	if !strings.Contains(logs.String(), "consecutive_failures=3") || !strings.Contains(logs.String(), "class=unknown") || strings.Contains(logs.String(), "synthetic transient") {
		t.Fatalf("diagnostic is not count/class only: %s", logs.String())
	}
}

func TestTypingKeepaliveEndsOnContextCancelAndCannotRestart(t *testing.T) {
	session := &fakeTypingSession{}
	ctx, cancel := context.WithCancel(context.Background())
	k := newTypingKeepaliveInterval(session, "channel", slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), time.Hour)
	k.start(ctx)
	if got := session.count(); got != 1 {
		t.Fatalf("initial typing calls = %d", got)
	}
	cancel()
	select {
	case <-k.doneCh:
	case <-time.After(time.Second):
		t.Fatal("context cancellation did not stop typing")
	}
	k.start(context.Background())
	if got := session.count(); got != 1 {
		t.Fatalf("typing restarted after cancellation: %d calls", got)
	}
}

func TestTypingStopSealsRacingLateStarts(t *testing.T) {
	for i := 0; i < 20; i++ {
		session := &fakeTypingSession{}
		k := newTypingKeepaliveInterval(session, "channel", slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), time.Hour)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); k.start(context.Background()) }()
		go func() { defer wg.Done(); k.stop() }()
		wg.Wait()
		calls := session.count()
		k.start(context.Background())
		if got := session.count(); got != calls {
			t.Fatalf("late start added a typing call: %d → %d", calls, got)
		}
	}
}

// blockingTypingSession lets the first holdFrom-1 calls through and holds every later one until release
// is closed (it ignores the call's context, like a rate-limit wait inside the REST client).
type blockingTypingSession struct {
	holdFrom int
	entered  chan struct{}
	release  chan struct{}
	mu       sync.Mutex
	calls    int
}

func (s *blockingTypingSession) ChannelTyping(string, ...discordgo.RequestOption) error {
	s.mu.Lock()
	s.calls++
	hold := s.calls >= s.holdFrom
	s.mu.Unlock()
	if !hold {
		return nil
	}
	s.entered <- struct{}{}
	<-s.release
	return errors.New("synthetic late failure")
}

func (s *blockingTypingSession) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// stop must not wait for a typing call that is stuck in the REST client, whether it is the first call
// or a later tick, and the stuck call's late result must not revive or count against the keepalive.
func TestTypingStopDoesNotWaitForACallInFlight(t *testing.T) {
	for _, holdFrom := range []int{1, 2} {
		t.Run(fmt.Sprintf("hold from call %d", holdFrom), func(t *testing.T) {
			session := &blockingTypingSession{holdFrom: holdFrom, entered: make(chan struct{}, 4), release: make(chan struct{})}
			var logs bytes.Buffer
			k := newTypingKeepaliveInterval(session, "channel", slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})), time.Millisecond)
			started := make(chan struct{})
			go func() { k.start(context.Background()); close(started) }()
			<-session.entered
			stopped := make(chan struct{})
			go func() { k.stop(); close(stopped) }()
			select {
			case <-stopped:
			case <-time.After(2 * time.Second):
				t.Fatal("stop waited for the typing call in flight")
			}
			select {
			case <-k.doneCh:
			default:
				t.Fatal("stop did not seal typing")
			}
			calls := session.count()
			close(session.release)
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("start did not return after the call was released")
			}
			time.Sleep(20 * time.Millisecond)
			if got := session.count(); got != calls {
				t.Fatalf("typing called again after stop: %d → %d", calls, got)
			}
			if strings.Contains(logs.String(), "consecutive_failures") {
				t.Fatalf("a result that arrived after stop was counted: %s", logs.String())
			}
		})
	}
}
