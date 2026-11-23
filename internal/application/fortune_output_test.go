package application_test

import (
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/clock"
	"masamong/internal/config"
)

// The action output of a day's reading carries the computed points, as the card does: the model's own
// SCORE and LUCKY_* lines are out of its text, and scores and lucky hold the computed values. The
// expected scores are the hand-derived ones of TestFortuneTagReadingBecomesACard (1995-06-15 on
// 2026-10-15: 65 60 65 65 59, lucky number 9, west).
func TestFortuneOutputHasTheComputedPoints(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	url := chatServer(t, "SCORE: 77\nSUMMARY: 좋은 흐름이에요.\nWORK: 집중\nLUCKY_COLOR: 파랑\nLUCKY_ITEM: 시계", "stop", &calls)
	svc := routedSvc(t, url, url)
	applyDoc(t, svc, func(d *config.Document) { d.Discord.Enabled = true; d.I18n.Timezone = "Asia/Seoul" })
	serveAnyServer(t, svc)
	seoul, _ := time.LoadLocation("Asia/Seoul")
	svc.Clock = clock.Fixed{T: time.Date(2026, 10, 15, 8, 0, 0, 0, seoul)}
	dm := person("u1", true)
	_ = as(svc, "privacy.consent", map[string]any{"scope": "fortune", "decision": "grant", "confirmed": true}, dm)
	_ = as(svc, "fortune.subscribe", map[string]any{"op": "register", "birth_date": "1995-06-15"}, dm)
	r := asIn(svc, "ko", "fortune.query", map[string]any{"what": "today"}, dm)
	text, _ := r.Output["text"].(string)
	if strings.Contains(text, "SCORE") || strings.Contains(text, "LUCKY_") || !strings.Contains(text, "SUMMARY: 좋은 흐름이에요.") || !strings.Contains(text, "WORK: 집중") {
		t.Fatalf("text: %q", text)
	}
	if got := fmt.Sprint(r.Output["scores"]); got != "map[health:59 love:65 money:60 overall:65 work:65]" {
		t.Fatalf("scores %s", got)
	}
	lucky, _ := r.Output["lucky"].(map[string]any)
	if lucky["number"] != "9" || lucky["direction"] != "서쪽" || lucky["color"] == "" || lucky["color"] == "파랑" || lucky["item"] == "시계" {
		t.Fatalf("lucky %v", lucky)
	}
	// a public summary has no computed points: its output is the text as written
	pub := asIn(svc, "ko", "fortune.query", map[string]any{"what": "today"}, person("u1", false))
	if _, has := pub.Output["scores"]; has || pub.Output["text"] != "SCORE: 77\nSUMMARY: 좋은 흐름이에요.\nWORK: 집중\nLUCKY_COLOR: 파랑\nLUCKY_ITEM: 시계" {
		t.Fatalf("public output %v", pub.Output)
	}
}

// The twelve-animal card is posted once a minute per public channel: a second ask within the minute gets
// a short note only the asker sees, with the seconds left; another channel, a private ask, or the same
// channel a minute later get the card.
func TestFortuneAnimalsChannelCooldown(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	svc := fortuneSvc(t, &calls)
	applyDoc(t, svc, func(d *config.Document) { d.I18n.Timezone = "Asia/Seoul" })
	serveAnyServer(t, svc)
	seoul, _ := time.LoadLocation("Asia/Seoul")
	at := time.Date(2026, 10, 15, 21, 0, 0, 0, seoul)
	svc.Clock = clock.Fixed{T: at}
	status := func(inv *action.Invoker) (string, action.Card) {
		r := asIn(svc, "ko", "fortune.query", map[string]any{"what": "animals"}, inv)
		c, _ := r.Output["card"].(action.Card)
		return fmt.Sprint(r.Output["status"]), c
	}
	if st, _ := status(person("a", false)); st != "ok" {
		t.Fatalf("first: %s", st)
	}
	svc.Clock = clock.Fixed{T: at.Add(20 * time.Second)}
	st, c := status(person("b", false))
	if st != "recent" || !c.Ephemeral || !strings.Contains(c.Text, "41초") || strings.Count(c.Text, "\n") > 1 {
		t.Fatalf("second within the minute: %s %+v", st, c)
	}
	other := person("b", false)
	other.ChannelID = "c2"
	if st, _ := status(other); st != "ok" {
		t.Fatalf("another channel: %s", st)
	}
	if st, _ := status(person("b", true)); st != "ok" {
		t.Fatalf("private: %s", st)
	}
	svc.Clock = clock.Fixed{T: at.Add(61 * time.Second)}
	if st, _ := status(person("b", false)); st != "ok" {
		t.Fatalf("a minute later: %s", st)
	}
	if calls.Load() != 0 {
		t.Fatalf("model calls %d", calls.Load())
	}
}

// askBarrier makes the callers of one round wait for each other: the asks are lined up at the entrance of
// the cooldown check, where a check-then-set gap would let more than one through. A closed channel
// releases them together, so it needs no spare CPU (a spin would) and no skip on a one-CPU machine.
type askBarrier struct {
	mu      sync.Mutex
	want    int
	arrived int
	open    chan struct{}
}

// next starts a round of want callers. Call it only while no ask is running.
func (b *askBarrier) next(want int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.want, b.arrived, b.open = want, 0, make(chan struct{})
}

func (b *askBarrier) arrive() {
	b.mu.Lock()
	b.arrived++
	open := b.open
	if b.arrived == b.want {
		close(open)
	}
	b.mu.Unlock()
	select {
	case <-open:
	case <-time.After(10 * time.Second): // an ask that never arrives fails the round's count, not a hang
	}
}

// barrierClock is a fixed clock whose Now, when called from the animals card, waits at the barrier. It is
// installed before the service starts its workers and never replaced: they read Service.Clock.
type barrierClock struct {
	t       time.Time
	barrier *askBarrier
}

func (c barrierClock) Now() time.Time {
	pcs := make([]uintptr, 12)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
	for {
		f, more := frames.Next()
		if strings.HasSuffix(f.Function, "(*Service).animals") {
			c.barrier.arrive()
			break
		}
		if !more {
			break
		}
	}
	return c.t
}

// The cooldown check and its set are one step: N asks that reach the check together in a fresh channel
// post exactly one card, however the goroutines interleave (repeated, so a check-then-set gap shows).
func TestFortuneAnimalsCooldownIsAtomic(t *testing.T) {
	t.Parallel()
	seoul, err := time.LoadLocation("Asia/Seoul")
	if err != nil {
		t.Fatal(err)
	}
	const asks, rounds = 8, 300
	barrier := &askBarrier{}
	// the clock goes in before setup starts the service's workers (setupSvcLog runs this first), so no
	// worker reads Service.Clock while the test replaces it
	svc := setupSvcLog(t, func(s *application.Service) *slog.Logger {
		s.Clock = barrierClock{t: time.Date(2026, 10, 15, 21, 0, 0, 0, seoul), barrier: barrier}
		return s.Log
	})
	applyDoc(t, svc, func(d *config.Document) { d.Discord.Enabled = true; d.I18n.Timezone = "Asia/Seoul" })
	serveAnyServer(t, svc)
	for round := 0; round < rounds; round++ {
		barrier.next(asks)
		var wg sync.WaitGroup
		var cards, notes atomic.Int32
		for i := 0; i < asks; i++ {
			inv := person(fmt.Sprintf("u%d", i), false)
			inv.ChannelID = fmt.Sprintf("c%d", round)
			wg.Add(1)
			go func() {
				defer wg.Done()
				r := asIn(svc, "ko", "fortune.query", map[string]any{"what": "animals"}, inv)
				switch r.Output["status"] {
				case "ok":
					cards.Add(1)
				case "recent":
					notes.Add(1)
				}
			}()
		}
		wg.Wait()
		if cards.Load() != 1 || notes.Load() != int32(asks)-1 {
			t.Fatalf("round %d: %d cards and %d notes from %d simultaneous asks", round, cards.Load(), notes.Load(), asks)
		}
	}
}
