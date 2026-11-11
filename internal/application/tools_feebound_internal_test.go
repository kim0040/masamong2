package application

import (
	"testing"

	"masamong/internal/lang"
	"masamong/internal/tools"
)

// The chat tool loop runs a round's calls of read-only lookups at once and holds a web search's fee bound
// by the depth it asks for; tools that write keep their order, and a tool that pays nothing holds nothing.
func TestToolInfoMarksParallelLookupsAndFeeBounds(t *testing.T) {
	p, _ := lang.Get("ko")
	infos := map[string]bool{}
	var search, quote, weather, images bool
	for _, r := range tools.Resolve(nil, nil) {
		info := toolInfo(r, p, nil)
		infos[r.ID] = info.Parallel
		if info.Parallel && (r.Writes || r.Once) {
			t.Errorf("%s writes or is once, yet runs beside itself", r.ID)
		}
		if !info.Pays && (info.MaxFeeUSD != 0 || info.FeeBound != nil) {
			t.Errorf("%s pays nothing, yet holds a fee", r.ID)
		}
		switch r.ID {
		case "search.web":
			search = info.FeeBound != nil
			for depth, want := range map[string]float64{"quick": 0.005, "": 0.01, "standard": 0.01, "STANDARD": 0.01, "bogus": 0.01, "deep": 0.06, " Deep ": 0.06} {
				if got := info.FeeBound(map[string]any{"depth": depth}); got != want {
					t.Errorf("search.web depth %q: bound %v, want %v", depth, got, want)
				}
			}
			if got := info.FeeBound(nil); got != 0.01 {
				t.Errorf("search.web without parameters: bound %v", got)
			}
			// the static bound is the dearest call's, so a caller without FeeBound never holds too little
			if info.MaxFeeUSD != max(searchFeeBounds["quick"], searchFeeBounds["standard"], searchFeeBounds["deep"]) {
				t.Errorf("search.web MaxFeeUSD %v is not its deep bound", info.MaxFeeUSD)
			}
		case "finance.quote":
			quote = info.Pays && info.MaxFeeUSD == searchFeeBounds["standard"] // one standard search when the quote service cannot answer
		case "weather.query":
			weather = !info.Pays
		case "images.generate":
			images = !info.Parallel
		}
	}
	for _, id := range []string{"search.web", "web.read", "places.search", "weather.query", "finance.quote", "finance.fx", "disasters.query"} {
		if !infos[id] {
			t.Errorf("%s does not run beside itself", id)
		}
	}
	for _, id := range []string{"reminders.propose", "polls.create", "images.generate", "summaries.channel"} {
		if infos[id] {
			t.Errorf("%s runs beside itself", id)
		}
	}
	if !search || !quote || !weather || !images {
		t.Fatalf("search %v quote %v weather %v images %v", search, quote, weather, images)
	}
}
