package websearch

import (
	"errors"
	"strings"

	"masamong/internal/providers"
	"masamong/internal/providers/decode"
)

// Fee is the list price, in US dollars, of one request a search service is sent at a depth: what a
// turn holds before the search starts and what is recorded as spent when the service reports no cost
// of its own (Linkup, Tavily, Brave, Serper). Exa reports its cost in the answer, which is recorded
// instead. Prices are a little above the providers' published ones so a hold is never below the fee.
// A kind not listed costs nothing (TinyFish, Kakao); an unknown kind is priced like the dearest
// standard search, so a new adapter that forgets to add itself here is over-held rather than free.
func Fee(kind, depth string) float64 {
	switch kind {
	case "exa":
		return pick(depth, 0.005, 0.008, 0.015) // instant, auto, deep (published 0.004 / 0.007 / 0.012)
	case "linkup":
		return pick(depth, 0.0055, 0.0055, 0.055) // flash and standard 0.005, deep 0.05
	case "tavily":
		return pick(depth, 0.008, 0.008, 0.016) // one credit, two for advanced
	case "brave":
		return 0.005
	case "serper":
		return 0.001
	case "tinyfish", "kakao_search":
		return 0
	}
	return 0.01
}

// ReportsCost is whether the service's answer carries what the request cost (Response.CostUSD).
func ReportsCost(kind string) bool { return kind == "exa" }

// MaxExtra is how many of a deep search's reworded queries (Query.Extra) a service runs.
const MaxExtra = 2

// Requests is how many requests one search sends to the service at a depth, given how many reworded
// queries it was handed. Exa takes them in one request (additionalQueries) and Linkup's deep search
// plans its own queries, so both send one; the others run each reworded query (up to MaxExtra) as a
// request of its own beside the main one. Kakao's deep search also asks its blog search for the main
// query. Reworded queries are only used by deep searches.
func Requests(kind, depth string, extra int) int {
	if depth != DepthDeep {
		return 1
	}
	extra = max(0, min(extra, MaxExtra))
	switch kind {
	case "exa", "linkup":
		return 1
	case "kakao_search":
		return 2 + extra
	}
	return 1 + extra
}

// SearchFee is Fee times Requests: the most one search costs at that service.
func SearchFee(kind, depth string, extra int) float64 {
	depth = strings.ToLower(strings.TrimSpace(depth))
	return Fee(kind, depth) * float64(Requests(kind, depth, extra))
}

// MaybeCharged reports whether a failed search request may still have been charged: it timed out, lost
// its connection or was cancelled (the turn ended) after it may have been sent, or the service answered with something that could not be
// read (an answer the service gave, so one it may bill). An HTTP error answer (whatever became of its body), a request never sent, and
// anything that is not the service's (a limit of ours) were not.
func MaybeCharged(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, decode.ErrUnexpected) {
		return true
	}
	var pe *providers.Error
	if !errors.As(err, &pe) {
		return false
	}
	if pe.Status >= 400 {
		return false // an HTTP error answer, even one whose body was too large or cut off
	}
	switch pe.Kind {
	case "timeout", "connection", "cancelled":
		return !pe.NotSent
	case "too_large":
		return true
	}
	return false
}
