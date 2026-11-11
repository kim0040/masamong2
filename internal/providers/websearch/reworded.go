package websearch

import (
	"context"
	"errors"
	"strings"
	"sync"
)

// searchFn is one service's search for one query.
type searchFn func(ctx context.Context, q Query) (Response, error)

// RewordedQueries are the reworded queries a search may use: those of a deep search only, cleaned (white
// space collapsed), without the main query again or a repeat, at most three (Exa's additionalQueries).
func RewordedQueries(q Query) []string {
	if q.Depth != DepthDeep {
		return nil
	}
	seen := map[string]bool{strings.ToLower(strings.Join(strings.Fields(q.Text), " ")): true}
	var out []string
	for _, x := range cleanExtra(q.Extra) {
		if key := strings.ToLower(x); !seen[key] {
			seen[key] = true
			out = append(out, x)
		}
	}
	return out
}

// rewordedFor is how many of the reworded queries a service that sends mainRequests requests for the
// main query runs of its own: MaxExtra at most, fewer when the caller reserved fewer requests.
func (q Query) rewordedFor(mainRequests int) []string {
	extra := RewordedQueries(q)
	n := min(len(extra), MaxExtra)
	if q.Requests > 0 {
		n = min(n, max(0, q.Requests-mainRequests))
	}
	return extra[:n]
}

// errPanic is what a request that panicked (a parser meeting an answer nobody expected) becomes: that
// request's error, not the end of the process.
var errPanic = errors.New("websearch: the request failed unexpectedly")

// safely calls fn and turns a panic into errPanic.
func safely(ctx context.Context, q Query, fn searchFn) (res Response, err error) {
	defer func() {
		if recover() != nil {
			res, err = Response{}, errPanic
		}
	}()
	return fn(ctx, q)
}

// fanOut is the search of a service that takes no reworded queries of its own: the main query, and a
// deep search's reworded ones (up to MaxExtra, fewer when fewer requests were reserved) at once at the
// same service.
func fanOut(ctx context.Context, q Query, one searchFn) (Response, error) {
	return fanOutWith(ctx, q, 1, one, one)
}

// fanOutWith runs main (the main query, which sends mainRequests requests) beside one request per
// reworded query. The answers are merged with the main query's first (MergeResults) and cut to the size
// asked. A reworded query that fails is dropped. When the main query fails, what the reworded queries found
// is the answer; the search fails only when they found nothing either (the error still says how many
// requests were answered, Response.Requests, for the fee). Response.Requests counts every request answered,
// Response.Lost every other one that may still have been charged (MaybeCharged).
func fanOutWith(ctx context.Context, q Query, mainRequests int, main, one searchFn) (Response, error) {
	extras := q.rewordedFor(mainRequests)
	if len(extras) == 0 {
		res, err := safely(ctx, q, main)
		if err == nil && res.Requests == 0 {
			res.Requests = 1
		}
		return res, err
	}
	type answer struct {
		res Response
		err error
	}
	got := make([]answer, len(extras))
	var wg sync.WaitGroup
	for i, text := range extras {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sub := q
			sub.Text, sub.Extra, sub.Requests = text, nil, 1
			res, err := safely(ctx, sub, one)
			got[i] = answer{res, err}
		}()
	}
	res, err := safely(ctx, q, main)
	wg.Wait()
	answered, lost := 0, 0
	for _, a := range got {
		switch {
		case a.err == nil:
			answered++
		case MaybeCharged(a.err):
			lost++
		}
	}
	if err != nil {
		// the main query failed: the reworded ones ask about the same thing, so what they found (already
		// paid for) answers the search; only when none found anything does the search fail
		var lists [][]Result
		for _, a := range got {
			if a.err == nil && len(a.res.Results) > 0 {
				lists = append(lists, a.res.Results)
			}
		}
		if len(lists) == 0 {
			return Response{Requests: answered + res.Requests, Lost: res.Lost + lost}, err
		}
		merged := MergeResults(lists...)
		if n := size(q.Max); len(merged) > n {
			merged = merged[:n]
		}
		if MaybeCharged(err) {
			lost++ // the main query's answer was lost, and the search did not fail with its error
		}
		return Response{Results: merged, Requests: answered + res.Requests, Lost: res.Lost + lost}, nil
	}
	lists := [][]Result{res.Results}
	for _, a := range got {
		if a.err == nil {
			lists = append(lists, a.res.Results)
		}
	}
	res.Results = MergeResults(lists...)
	if n := size(q.Max); len(res.Results) > n {
		res.Results = res.Results[:n]
	}
	res.Requests = max(res.Requests, 1) + answered
	res.Lost += lost
	return res, nil
}
