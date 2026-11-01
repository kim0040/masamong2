package search

import (
	"math"
	"sort"
	"strings"

	"masamong/internal/memory/retrieval"
)

const rrfK = 60

// messageRun is the name of the message-vector run in the final fusion.
const messageRun = "message_dense"

// Candidate is one fused leaf with the retrievers that surfaced it (candidates.Candidate).
type Candidate struct {
	DocID   string             `json:"doc_id"`
	Score   float64            `json:"score"`
	Rank    int                `json:"rank"`
	Sources map[string]int     `json:"sources"` // retriever → 1-based rank in that run
	Raw     map[string]float64 `json:"-"`
	Flags   map[string]bool    `json:"flags,omitempty"`
}

// Families are the retriever families (lexical, dense, speaker, alias, …) behind the candidate.
func (c *Candidate) Families() map[string]bool {
	out := map[string]bool{}
	for s := range c.Sources {
		out[SourceFamily(s)] = true
	}
	return out
}

// SourceFamily maps `leaf_lexical` → lexical, `q1_dense` → dense, `speaker_dense` → speaker …
func SourceFamily(source string) string {
	if source == "message_dense" {
		// a dense retriever like the others: it must not make a leaf look confirmed by a second family
		return "dense"
	}
	name := source
	if name == "soft_event" || strings.HasPrefix(name, "soft_event_") {
		return "soft_event"
	}
	for _, p := range []string{"leaf_", "summary_", "q1_", "q2_"} {
		if strings.HasPrefix(name, p) {
			name = strings.TrimPrefix(name, p)
			break
		}
	}
	switch {
	case strings.HasPrefix(name, "speaker"):
		return "speaker"
	case strings.HasPrefix(name, "alias"):
		return "alias"
	case strings.HasPrefix(name, "time"):
		return "time"
	}
	return name
}

// Run is one named ranked list entering a fusion.
type Run struct {
	Name   string
	IDs    []string
	Weight float64
}

// Fusion is a weighted-RRF result with provenance (candidates.FusionResult).
type Fusion struct {
	Ordered []*Candidate
	Weights map[string]float64
	Runs    map[string][]string
	byID    map[string]*Candidate
}

// IDs is the fused order.
func (f *Fusion) IDs() []string {
	out := make([]string, len(f.Ordered))
	for i, c := range f.Ordered {
		out[i] = c.DocID
	}
	return out
}

// ByID finds a candidate.
func (f *Fusion) ByID(id string) *Candidate {
	if f.byID == nil {
		f.byID = make(map[string]*Candidate, len(f.Ordered))
		for _, c := range f.Ordered {
			f.byID[c.DocID] = c
		}
	}
	return f.byID[id]
}

// SourceCounts is run length per retriever.
func (f *Fusion) SourceCounts() map[string]int {
	out := map[string]int{}
	for k, v := range f.Runs {
		out[k] = len(v)
	}
	return out
}

// fuse is candidates.fuse: sum of weight/(k+pos+1), stable on first appearance. A run whose name is
// in children stands for an already-fused list; its per-retriever ranks are copied into provenance
// and the aggregate name is replaced by the child's runs.
func fuse(runs []Run, children map[string]*Fusion, raw map[string]map[string]float64) *Fusion {
	scores := map[string]float64{}
	var order []string
	sources := map[string]map[string]int{}
	f := &Fusion{Weights: map[string]float64{}, Runs: map[string][]string{}}
	for _, r := range runs {
		f.Weights[r.Name] = r.Weight
		f.Runs[r.Name] = append([]string(nil), r.IDs...)
		child := children[r.Name]
		for pos, id := range r.IDs {
			if _, ok := scores[id]; !ok {
				order = append(order, id)
			}
			scores[id] += r.Weight / float64(rrfK+pos+1)
			src := sources[id]
			if src == nil {
				src = map[string]int{}
				sources[id] = src
			}
			if child != nil {
				if cc := child.ByID(id); cc != nil {
					for s, rk := range cc.Sources {
						if _, ok := src[s]; !ok {
							src[s] = rk
						}
					}
				}
			} else if _, ok := src[r.Name]; !ok {
				src[r.Name] = pos + 1
			}
		}
	}
	for name, child := range children {
		for s, ids := range child.Runs {
			if _, ok := f.Runs[s]; !ok {
				f.Runs[s] = ids
			}
		}
		delete(f.Runs, name)
	}
	sort.SliceStable(order, func(a, b int) bool { return scores[order[a]] > scores[order[b]] })
	f.Ordered = make([]*Candidate, len(order))
	for i, id := range order {
		c := &Candidate{DocID: id, Score: scores[id], Rank: i + 1, Sources: sources[id], Raw: map[string]float64{}, Flags: map[string]bool{}}
		for s := range c.Sources {
			if t, ok := raw[s]; ok {
				if v, ok := t[id]; ok {
					c.Raw[s] = v
				}
			}
		}
		f.Ordered[i] = c
	}
	return f
}

// guaranteePool is candidates.guarantee_pool: fused top-(pool − reserve) plus the top
// per_source_min of every run the cut would drop (round-robin across sorted run names).
// Returns (pool prefix + rest of fusion order, guaranteed ids, guaranteed by source).
func guaranteePool(f *Fusion, pool, perSourceMin int, reserveMaxFrac float64) ([]string, []string, map[string][]string) {
	ids := f.IDs()
	bySource := map[string][]string{}
	if pool <= 0 || perSourceMin <= 0 || len(f.Runs) == 0 {
		return ids, nil, bySource
	}
	reserveCap := max(0, int(float64(pool)*math.Max(0, math.Min(1, reserveMaxFrac))))
	baseN := min(max(0, pool-reserveCap), len(ids))
	base := ids[:baseN]
	inPool := setOf(base...)
	var extras []string
	names := make([]string, 0, len(f.Runs))
	for n := range f.Runs {
		names = append(names, n)
	}
	sort.Strings(names)
	// The message run claims its slots last, from what the other runs left of the reserve, so adding it
	// never displaces a leaf or lexical candidate that would have been guaranteed without it.
	late := false
	for i, n := range names {
		if n == messageRun {
			names = append(names[:i], names[i+1:]...)
			late = true
			break
		}
	}
	claim := func(name string) {
		for k := 0; k < perSourceMin && len(extras) < reserveCap; k++ {
			run := f.Runs[name]
			if k >= len(run) {
				break
			}
			id := run[k]
			if inPool.Has(id) {
				continue
			}
			extras = append(extras, id)
			inPool[id] = struct{}{}
			bySource[name] = append(bySource[name], id)
		}
	}
	for k := 0; k < perSourceMin && len(extras) < reserveCap; k++ {
		for _, name := range names {
			run := f.Runs[name]
			if k >= len(run) {
				continue
			}
			id := run[k]
			if inPool.Has(id) {
				continue
			}
			extras = append(extras, id)
			inPool[id] = struct{}{}
			bySource[name] = append(bySource[name], id)
			if len(extras) >= reserveCap {
				break
			}
		}
	}
	if late {
		claim(messageRun)
	}
	var fill []string
	for _, id := range ids[baseN:] {
		if !inPool.Has(id) {
			fill = append(fill, id)
		}
	}
	head := append(append([]string(nil), base...), extras...)
	room := pool - len(head)
	poolIDs := append(head, fill[:min(len(fill), max(0, room))]...)
	inFinal := setOf(poolIDs...)
	out := append([]string(nil), poolIDs...)
	for _, id := range ids {
		if !inFinal.Has(id) {
			out = append(out, id)
		}
	}
	return out, extras, bySource
}

// Signals are candidates_assess.signals_for (rounded to 4 decimals like the sandbox before the
// shared level rule runs).
type Signals struct {
	CandidateCount      int      `json:"candidate_count"`
	Agreement           float64  `json:"agreement"`
	LexicalDenseOverlap float64  `json:"lexical_dense_overlap"`
	TopMargin           float64  `json:"top_margin"`
	TargetCoverage      *float64 `json:"target_coverage"`
	TargetsRequested    bool     `json:"targets_requested"`
	TargetsResolved     bool     `json:"targets_resolved"`
	SourceDiversity     int      `json:"source_diversity"`
}

func round4(x float64) float64 { return math.RoundToEven(x*1e4) / 1e4 }

func signalsFor(f *Fusion, topN int, requested, resolved bool) Signals {
	topN = max(1, topN)
	top := f.Ordered[:min(topN, len(f.Ordered))]
	n := len(top)
	fam := make([]map[string]bool, n)
	for i, c := range top {
		fam[i] = c.Families()
		delete(fam[i], "soft_event")
	}
	agreement := 0.0
	if n > 0 {
		k := 0
		for _, fs := range fam {
			if len(fs) >= 2 {
				k++
			}
		}
		agreement = float64(k) / float64(n)
	}
	lex := setOf(f.Runs["leaf_lexical"][:min(topN, len(f.Runs["leaf_lexical"]))]...)
	den := setOf(f.Runs["leaf_dense"][:min(topN, len(f.Runs["leaf_dense"]))]...)
	overlap := 0.0
	if len(lex) > 0 || len(den) > 0 {
		overlap = float64(len(intersect(lex, den))) / float64(max(1, topN))
	}
	margin := 0.0
	switch {
	case n >= 2 && top[0].Score > 0:
		margin = (top[0].Score - top[1].Score) / top[0].Score
	case n == 1:
		margin = 1.0
	}
	var coverage *float64
	if requested && resolved {
		v := 0.0
		if n > 0 {
			k := 0
			for _, c := range top {
				if c.Flags["speaker_match"] || c.Flags["alias_match"] {
					k++
				}
			}
			v = round4(float64(k) / float64(n))
		}
		coverage = &v
	}
	families := map[string]bool{}
	for _, fs := range fam {
		for x := range fs {
			families[x] = true
		}
	}
	return Signals{
		CandidateCount: len(f.Ordered), Agreement: round4(agreement), LexicalDenseOverlap: round4(overlap),
		TopMargin: round4(margin), TargetCoverage: coverage, TargetsRequested: requested, TargetsResolved: resolved,
		SourceDiversity: len(families),
	}
}

// Assessment is the confidence verdict over the candidate pool.
type Assessment struct {
	Level   retrieval.Confidence `json:"level"`
	Reasons []string             `json:"reasons"`
	Signals Signals              `json:"signals"`
}

func (c Config) thresholds() retrieval.Thresholds {
	a := c.Retrieval.Auto
	d := retrieval.DefaultThresholds
	return retrieval.Thresholds{
		AssessTopN: intOr(a.AssessTopN, d.AssessTopN), HighAgreement: floatOr(a.HighAgreement, d.HighAgreement),
		HighMargin: floatOr(a.HighMargin, d.HighMargin), LowAgreement: floatOr(a.LowAgreement, d.LowAgreement),
		MinCandidates: intOr(a.MinCandidates, d.MinCandidates), MinTargetCoverage: floatOr(a.MinTargetCoverage, d.MinTargetCoverage),
	}
}

func assess(f *Fusion, th retrieval.Thresholds, requested, resolved bool) Assessment {
	sig := signalsFor(f, th.AssessTopN, requested, resolved)
	level, reason := retrieval.Assess(retrieval.Signals{
		CandidateCount: sig.CandidateCount, Agreement: sig.Agreement, TopMargin: sig.TopMargin,
		TargetCoverage: sig.TargetCoverage, TargetsRequested: requested, TargetsResolved: resolved,
	}, th)
	return Assessment{Level: level, Reasons: []string{reason}, Signals: sig}
}
