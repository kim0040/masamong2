package chat

import (
	"slices"
	"sync"
	"time"
)

// Span is one stretch of a turn's work, in milliseconds from the turn's start (Request.TurnStart). The
// steps that run at once (the planner and the tool loop, then the planner's tools and the memory
// search) overlap, so the spans show which step the turn waited for; Result.Timing only sums them.
// Names are fixed words and tool ids, never text of the conversation.
type Span struct {
	Name    string `json:"name"`
	StartMS int    `json:"start_ms"`
	EndMS   int    `json:"end_ms"`
}

// spanLog gathers the turn's spans. The steps that run at once add to it from their own goroutines.
type spanLog struct {
	origin time.Time
	mu     sync.Mutex
	list   []Span
}

// newSpanLog starts a log whose offsets count from origin (the zero time: from now).
func newSpanLog(origin time.Time) *spanLog {
	if origin.IsZero() {
		origin = time.Now()
	}
	return &spanLog{origin: origin}
}

func (l *spanLog) offset(t time.Time) int { return max(int(t.Sub(l.origin).Milliseconds()), 0) }

// add records a stretch that has ended.
func (l *spanLog) add(name string, start, end time.Time) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.list = append(l.list, Span{Name: name, StartMS: l.offset(start), EndMS: l.offset(end)})
	l.mu.Unlock()
}

// begin starts a stretch now; the function it returns ends it (defer sp.begin("memory")()).
func (l *spanLog) begin(name string) func() {
	start := time.Now()
	return func() { l.add(name, start, time.Now()) }
}

// spans are the recorded stretches in the order they started.
func (l *spanLog) spans() []Span {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := slices.Clone(l.list)
	slices.SortStableFunc(out, func(a, b Span) int { return a.StartMS - b.StartMS })
	return out
}

// joinLoop records the tool loop's stretch and says which side the turn waited for at the join: the
// "loop" when it was still running after the planner had answered, else the "planner". "" when the
// loop did not run (planner mode, or a loop that stopped before asking the model).
func (l *spanLog) joinLoop(loop *loopRun, started, plannerEnd time.Time) string {
	if l == nil || loop == nil || loop.done == nil || loop.trace == nil {
		return ""
	}
	loopEnd := started.Add(time.Duration(loop.trace.Ms) * time.Millisecond)
	l.add("tool_loop", started, loopEnd)
	if loopEnd.After(plannerEnd) {
		return "loop"
	}
	return "planner"
}
