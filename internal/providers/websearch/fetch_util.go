package websearch

import (
	"context"
	"time"
)

type softKey struct{}

type softRule struct {
	done  int
	after time.Duration
}

// WithSoftDeadline asks the fetchers called with ctx to stop waiting for the pages still missing once
// `after` has passed since `done` pages were read (done <= 0: since the call began). The pages cut
// off carry Err "timeout". ctx's own deadline stays the hard limit.
func WithSoftDeadline(ctx context.Context, done int, after time.Duration) context.Context {
	return context.WithValue(ctx, softKey{}, softRule{done: done, after: after})
}

// gather runs one(i) for every i in [0, n) at once and returns when all are in, the soft deadline of
// ctx has passed (WithSoftDeadline) or ctx has ended. A page still missing then is Err "timeout" and
// its call is cancelled. Results are in index order.
func gather(ctx context.Context, n int, one func(ctx context.Context, i int) Fetched) []Fetched {
	out := make([]Fetched, n)
	if n == 0 {
		return out
	}
	sub, cancel := context.WithCancel(ctx)
	defer cancel()
	type msg struct {
		i int
		f Fetched
	}
	ch := make(chan msg, n)
	for i := range n {
		go func() { ch <- msg{i, one(sub, i)} }()
	}
	have := make([]bool, n)
	rule, soft := ctx.Value(softKey{}).(softRule)
	soft = soft && rule.after > 0
	var timer *time.Timer
	var softC <-chan time.Time
	arm := func() {
		if soft && timer == nil {
			timer = time.NewTimer(rule.after)
			softC = timer.C
		}
	}
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	if soft && rule.done <= 0 {
		arm()
	}
	got, ok := 0, 0
	take := func(m msg) {
		out[m.i], have[m.i] = m.f, true
		got++
		if m.f.Err == "" {
			ok++
			if soft && ok >= rule.done {
				arm()
			}
		}
	}
loop:
	for got < n {
		select {
		case m := <-ch:
			take(m)
		case <-softC:
			break loop
		case <-ctx.Done():
			break loop
		}
	}
	for { // what finished at the same moment as the cut-off still counts
		select {
		case m := <-ch:
			take(m)
			continue
		default:
		}
		break
	}
	for i := range out {
		if !have[i] {
			out[i] = Fetched{Err: "timeout"}
		}
	}
	return out
}
