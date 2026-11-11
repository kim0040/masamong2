package chat

// Progress tells the connector how far a turn's web search has got, so a turn that searches long can
// say it is still looking (the interim line). It carries no text of the turn.
type Progress struct {
	Stage    string // ProgressSearch | ProgressDeep | ProgressMore
	Round    int    // the tool loop round (1 for the planner path)
	Searches int    // web searches started in that round
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
