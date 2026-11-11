package chat

import (
	"slices"
	"strings"
	"sync"

	"masamong/internal/crash"
)

// Progress tells the connector how far a turn's web search has got, so a turn that searches long can
// say it is still looking (the interim line). It carries no text of the turn.
type Progress struct {
	Stage    string // ProgressSearch | ProgressDeep | ProgressMore
	Round    int    // the tool loop round (1 for the planner path)
	Searches int    // web searches the round asked for
}

// Progress stages.
const (
	// ProgressSearch: a web search started (an ordinary one; the connector may speak if it runs long).
	ProgressSearch = "search"
	// ProgressDeep: a deep search started (the connector may speak at once).
	ProgressDeep = "deep"
	// ProgressMore: a second round of searches started (the first did not find enough).
	ProgressMore = "more"
)

// webSearchTool is the tool whose calls a turn's Progress counts.
const webSearchTool = "search.web"

// progressOnce is a turn's OnProgress passing each stage on at most once. A listener that panics is
// logged, never felt by the goroutine of the loop or of the planner's steps that called it. nil for none.
func progressOnce(fn func(Progress)) func(Progress) {
	if fn == nil {
		return nil
	}
	var mu sync.Mutex
	sent := map[string]bool{}
	return func(p Progress) {
		mu.Lock()
		seen := sent[p.Stage]
		sent[p.Stage] = true
		mu.Unlock()
		if seen {
			return
		}
		defer func() {
			if r := recover(); r != nil {
				_ = crash.Error(nil, "chat progress", r)
			}
		}()
		fn(p)
	}
}

// searchProgress is the stage of a round that starts web searches (their parameters): deep when one of
// them asks for depth deep, more from the loop's second round on, else search.
func searchProgress(round int, searches []map[string]any) Progress {
	p := Progress{Stage: ProgressSearch, Round: round, Searches: len(searches)}
	switch {
	case slices.ContainsFunc(searches, deepParams):
		p.Stage = ProgressDeep
	case round >= 2:
		p.Stage = ProgressMore
	}
	return p
}

// deepParams reports whether a web search's parameters ask for depth deep.
func deepParams(params map[string]any) bool {
	depth, _ := params["depth"].(string)
	return strings.EqualFold(strings.TrimSpace(depth), "deep")
}

// roundProgress tells a round's stage as its web searches are let in (admitTool): a search the turn's
// limits refuse starts nothing, so it says nothing. The stage is searchProgress of the searches let in so
// far, each stage told once (progressOnce): deep only once a deep search is let in. A round whose
// ordinary search is let in before its deep one tells search, then deep.
type roundProgress struct {
	report   func(Progress) // nil: no listener
	round    int
	asked    int // the round's searches
	admitted []map[string]any
}

// admit notes that one of the round's searches (its parameters) was let in.
func (p *roundProgress) admit(params map[string]any) {
	if p == nil || p.report == nil {
		return
	}
	p.admitted = append(p.admitted, params)
	st := searchProgress(p.round, p.admitted)
	st.Searches = p.asked
	p.report(st)
}
