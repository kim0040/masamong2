package application

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"masamong/internal/action"
)

// These tests pin the operator alert queue: its cap and order, when it is emptied and when it is kept,
// and that nothing queued is lost or sent twice, so the change that took the store and Discord calls out
// of alertMu (fix plan R03) is checked to keep them.

// alertDM is a connected Discord stand-in that records direct messages. onSend, when set, runs inside
// SendDM before it returns; fail makes every send fail for a passing reason.
type alertDM struct {
	mu     sync.Mutex
	sent   []action.Card
	fail   bool
	onSend func()
}

func (d *alertDM) Connected() bool                          { return true }
func (d *alertDM) BotUser() (string, string)                { return "bot", "masamong" }
func (d *alertDM) Guilds() []GuildInfo                      { return nil }
func (d *alertDM) Channels(string) []ChannelInfo            { return nil }
func (d *alertDM) Permissions(string, string) (int64, bool) { return 0, false }
func (d *alertDM) SendDM(_ context.Context, _ string, c action.Card, _ string) error {
	d.mu.Lock()
	hook, fail := d.onSend, d.fail
	d.mu.Unlock()
	if hook != nil {
		hook()
	}
	if fail {
		return errors.New("discord: temporarily unavailable")
	}
	d.mu.Lock()
	d.sent = append(d.sent, c)
	d.mu.Unlock()
	return nil
}

// take returns the text of the messages sent since the last take.
func (d *alertDM) take() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for _, c := range d.sent {
		out = append(out, c.Text)
	}
	d.sent = nil
	return out
}

func alertQueueService(t *testing.T) (*Service, *alertDM) {
	t.Helper()
	s, _ := banterLimitsService(t)
	dm := &alertDM{}
	s.SetDiscord(dm)
	doc := s.Doc()
	doc.Access.Operators = []string{"op1"}
	doc.I18n.DefaultLocale = "en"
	doc.Alerts.MinSeverity = "danger"
	s.setDoc(doc, s.Revision())
	return s, dm
}

func queuedNs(t *testing.T, s *Service) []int {
	t.Helper()
	var out []int
	for _, e := range queuedAlerts(t, s) {
		n, _ := e.Args["n"].(float64)
		out = append(out, int(n))
	}
	return out
}

// The queue keeps the newest alertMaxQueued events, in the order they came.
func TestAlertQueueKeepsTheNewestInOrder(t *testing.T) {
	s, _ := alertQueueService(t)
	for i := range alertMaxQueued + 5 {
		s.queueAlert(alertMemoryScope, map[string]any{"n": i})
	}
	got := queuedNs(t, s)
	if len(got) != alertMaxQueued || got[0] != 5 || got[len(got)-1] != alertMaxQueued+4 {
		t.Fatalf("queue = %v, want 5..%d", got, alertMaxQueued+4)
	}
	for i := 1; i < len(got); i++ {
		if got[i] != got[i-1]+1 {
			t.Fatalf("queue out of order: %v", got)
		}
	}
}

// Events queued at the same time are all kept.
func TestAlertQueueConcurrentEventsAllKept(t *testing.T) {
	s, _ := alertQueueService(t)
	const n = alertMaxQueued
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() { defer wg.Done(); s.queueAlert(alertMemoryScope, map[string]any{"n": i}) }()
	}
	wg.Wait()
	seen := map[int]bool{}
	for _, v := range queuedNs(t, s) {
		seen[v] = true
	}
	if len(seen) != n {
		t.Fatalf("queued %d distinct events, want %d", len(seen), n)
	}
}

// Queued events go out in one message, in order, and leave the queue; a send that reaches nobody for a
// passing reason keeps them for the next tick; with alerts off the backlog is dropped.
func TestAlertTickSendsQueuedEventsOnce(t *testing.T) {
	s, dm := alertQueueService(t)
	ctx := context.Background()
	s.alertTick(ctx)
	dm.take() // what the dashboard lists now (no model key)

	s.queueAlert(alertMemoryScope, map[string]any{"n": 1})
	s.queueAlert(alertMemoryScope, map[string]any{"n": 2})
	dm.mu.Lock()
	dm.fail = true
	dm.mu.Unlock()
	s.alertTick(ctx)
	if got := queuedNs(t, s); len(got) != 2 {
		t.Fatalf("a failed send dropped the queue: %v", got)
	}
	dm.mu.Lock()
	dm.fail = false
	dm.mu.Unlock()
	s.alertTick(ctx)
	texts := dm.take()
	if len(texts) != 1 {
		t.Fatalf("messages = %d, want 1: %q", len(texts), texts)
	}
	first, second := strings.Index(texts[0], "(1)"), strings.Index(texts[0], "(2)")
	if first < 0 || second < 0 || first > second {
		t.Fatalf("queued events not in order: %q", texts[0])
	}
	if got := queuedNs(t, s); len(got) != 0 {
		t.Fatalf("sent events left in the queue: %v", got)
	}
	s.alertTick(ctx)
	if texts := dm.take(); len(texts) != 0 {
		t.Fatalf("queued events sent twice: %q", texts)
	}

	s.queueAlert(alertMemoryScope, map[string]any{"n": 3})
	doc := s.Doc()
	off := false
	doc.Alerts.Discord = &off
	s.setDoc(doc, s.Revision())
	s.alertTick(ctx)
	if got := queuedNs(t, s); len(got) != 0 {
		t.Fatalf("alerts off kept a backlog: %v", got)
	}
	if texts := dm.take(); len(texts) != 0 {
		t.Fatalf("alerts off but sent %q", texts)
	}
}

// An event queued while a tick is sending is neither lost nor sent twice: it goes out with the next tick.
func TestAlertQueuedDuringASendGoesOutNextTick(t *testing.T) {
	s, dm := alertQueueService(t)
	ctx := context.Background()
	s.alertTick(ctx)
	dm.take()
	s.queueAlert(alertMemoryScope, map[string]any{"n": 1})
	queued := make(chan struct{})
	var once sync.Once
	dm.mu.Lock()
	dm.onSend = func() {
		once.Do(func() {
			go func() { s.queueAlert(alertMemoryScope, map[string]any{"n": 2}); close(queued) }()
		})
	}
	dm.mu.Unlock()
	s.alertTick(ctx)
	<-queued
	dm.mu.Lock()
	dm.onSend = nil
	dm.mu.Unlock()
	if texts := dm.take(); len(texts) != 1 || !strings.Contains(texts[0], "(1)") || strings.Contains(texts[0], "(2)") {
		t.Fatalf("first tick: %q", texts)
	}
	if got := queuedNs(t, s); len(got) != 1 || got[0] != 2 {
		t.Fatalf("queue after the first tick = %v, want [2]", got)
	}
	s.alertTick(ctx)
	texts := dm.take()
	if len(texts) != 1 || !strings.Contains(texts[0], "(2)") || strings.Contains(texts[0], "(1)") {
		t.Fatalf("second tick: %q", texts)
	}
	if got := queuedNs(t, s); len(got) != 0 {
		t.Fatalf("queue after the second tick = %v", got)
	}
}

// queueAlert (called from replies) and alerts.test do not wait while a tick sends (fix plan R03).
func TestAlertQueueDoesNotWaitForAnAlertSend(t *testing.T) {
	s, dm := alertQueueService(t)
	ctx := context.Background()
	s.alertTick(ctx)
	dm.take()
	s.queueAlert(alertMemoryScope, map[string]any{"n": 1})
	entered, free := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	dm.mu.Lock()
	dm.onSend = func() {
		if first.CompareAndSwap(false, true) { // only the tick's send waits
			close(entered)
			<-free
		}
	}
	dm.mu.Unlock()
	ticked := make(chan struct{})
	go func() { defer close(ticked); s.alertTick(ctx) }()
	<-entered
	queued := returnsWhileWaiting(t, "queueAlert", func() { s.queueAlert(alertMemoryScope, map[string]any{"n": 2}) })
	tested := returnsWhileWaiting(t, "alerts.test", func() {
		if r := s.Execute(ctx, action.Request{Action: "alerts.test"}); !r.OK {
			t.Errorf("alerts.test: %+v", r.Error)
		}
	})
	close(free)
	<-ticked
	<-queued
	<-tested
	if got := queuedNs(t, s); len(got) != 1 || got[0] != 2 {
		t.Fatalf("queue after the tick = %v, want [2]", got)
	}
}

// A send removes from the queue only the events it carried. When the cap pushes a sent event out while it
// sends and an identical one (same key, arguments and second) is queued, that one stays for the next
// message (Sol review of E8, finding 1).
func TestAlertSendKeepsAnIdenticalEventQueuedDuringIt(t *testing.T) {
	s, dm := alertQueueService(t)
	ctx := context.Background()
	s.alertTick(ctx)
	dm.take()
	same := map[string]any{"n": -1}
	s.queueAlert(alertMemoryScope, same)
	entered, free := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	dm.mu.Lock()
	dm.onSend = func() {
		if first.CompareAndSwap(false, true) {
			close(entered)
			<-free
		}
	}
	dm.mu.Unlock()
	ticked := make(chan struct{})
	go func() { defer close(ticked); s.alertTick(ctx) }()
	<-entered
	s.queueAlert(alertMemoryScope, same) // the clock has not moved: equal to the one being sent
	for i := range alertMaxQueued - 1 {
		s.queueAlert(alertMemoryScope, map[string]any{"n": i})
	}
	close(free)
	<-ticked
	if texts := dm.take(); len(texts) != 1 || !strings.Contains(texts[0], "(-1)") {
		t.Fatalf("first message: %q", texts)
	}
	got := queuedNs(t, s)
	if len(got) != alertMaxQueued || got[0] != -1 {
		t.Fatalf("queue after the send = %v, want the second -1 and 0..%d", got, alertMaxQueued-2)
	}
	s.alertTick(ctx)
	texts := dm.take()
	if len(texts) != 1 || !strings.Contains(texts[0], "(-1)") {
		t.Fatalf("the event queued during the send did not go out next: %q", texts)
	}
	if got := queuedNs(t, s); len(got) != 0 {
		t.Fatalf("queue after the second send = %v", got)
	}
}
