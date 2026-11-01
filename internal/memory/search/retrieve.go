package search

import (
	"context"
	"errors"
	"strings"
	"time"

	"masamong/internal/memory/corpus"
	"masamong/internal/memory/leaves"
	"masamong/internal/memory/lexical"
	"masamong/internal/memory/retrieval"
)

// QueryEmbedder turns query text into a normalized vector (query instruction applied by the caller's
// implementation). Used for the q1 semantic query; q0 arrives in the request.
type QueryEmbedder interface {
	EmbedQuery(ctx context.Context, text string) ([]float32, error)
}

// RerankResult is one reranker call. Indices/Scores are in relevance order.
type RerankResult struct {
	Indices []int
	Scores  []float64
	Model   string
	CostUSD float64
	Latency time.Duration
}

// Reranker is the R2 semantic reranker (voyageai/rerank-2.5-lite via OpenRouter in the product).
type Reranker interface {
	Available(remainingUSD float64) bool
	Rerank(ctx context.Context, query string, docs []string, topN int) (RerankResult, error)
}

// Scope is who may be searched. Rooms: the channel's own room plus attached blocks (evidence limited
// to those rooms' leaves). SourceRoom: the sandbox eval form (evidence rows checked by original room).
// Both empty with All=false means nothing may be searched.
type Scope struct {
	Rooms      []string
	SourceRoom string
	All        bool
	// Exclude removes leaves an operator excluded (memory.review). Empty in the sandbox parity path.
	Exclude Set
}

// Request is one memory search.
type Request struct {
	Query         string
	QueryVector   []float32
	Intent        Intent
	Ranking       retrieval.RankingMode // auto (default) | hybrid | rerank
	Scope         Scope
	TopK          int
	RemainingUSD  float64
	TimeReference *TimeReference
	// Nil keeps search and Trace unchanged; set before a measurement request.
	OnCandidatesForMeasurement func(CandidateMeasurement)
}

// Hit is one packed evidence document.
type Hit struct {
	Rank                  int            `json:"rank"`
	DocID                 string         `json:"doc_id"`
	Room                  string         `json:"source_room_id"`
	Platform              string         `json:"platform"`
	Date                  string         `json:"date"`
	Speakers              []string       `json:"speakers"`
	Excerpt               string         `json:"excerpt"`
	Summary               string         `json:"summary,omitempty"`
	Span                  string         `json:"span"`
	PackingStatus         string         `json:"packing_status"`
	ObservationIDs        []string       `json:"observation_ids"`
	VisibleObservationIDs []string       `json:"visible_observation_ids"`
	VisibleSpans          []Span         `json:"visible_spans"`
	LeafObservationIDs    []string       `json:"leaf_observation_ids"`
	MergedLeafIDs         []string       `json:"merged_leaf_ids"`
	Sources               map[string]int `json:"sources"`
	RerankScore           *float64       `json:"rerank_score,omitempty"`
	DenseCos              float64        `json:"dense_cos"`
}

// Result is hits plus the trace the sandbox records (debug_turns rank_trace subset).
type Result struct {
	Hits  []Hit           `json:"hits"`
	State retrieval.State `json:"rag_state"`
	Trace map[string]any  `json:"trace"`
}

func (ix *Index) scopeAllowed(s Scope) Set {
	allowed := ix.scopeRooms(s)
	if len(s.Exclude) == 0 {
		return allowed
	}
	if allowed == nil {
		allowed = Set{}
		for _, l := range ix.Leaves {
			allowed[l.DocID] = struct{}{}
		}
	}
	out := Set{}
	for id := range allowed {
		if !s.Exclude.Has(id) {
			out[id] = struct{}{}
		}
	}
	return out
}

func (ix *Index) scopeRooms(s Scope) Set {
	switch {
	case s.All:
		if s.SourceRoom != "" {
			return ix.roomLeaves(s.SourceRoom)
		}
		return nil
	case len(s.Rooms) > 0:
		allowed := ix.roomLeaves(s.Rooms...)
		if s.SourceRoom != "" {
			allowed = intersect(allowed, ix.roomLeaves(s.SourceRoom))
		}
		return allowed
	case s.SourceRoom != "":
		return ix.roomLeaves(s.SourceRoom)
	}
	return Set{}
}

type genResult struct {
	fusion    *Fusion
	allowed   Set
	trace     map[string]any
	requested bool
	resolved  bool
}

func canonQuery(s string) string { return strings.Join(strings.Fields(s), " ") }

// q1Text is the semantic (dense) second query: planner semantic_query, else memory.search.query.
func q1Text(query string, it Intent, cfg MultiQueryConfig) string {
	if !boolOr(cfg.Enabled, true) || intOr(cfg.MaxQueries, 3) <= 1 {
		return ""
	}
	sem, rw := strings.TrimSpace(it.SemanticQuery), strings.TrimSpace(it.MemorySearchQuery)
	if sem != "" && canonQuery(sem) != canonQuery(query) {
		return sem
	}
	if rw != "" && canonQuery(rw) != canonQuery(query) {
		return rw
	}
	return ""
}

func (ix *Index) hybrid(key string, lex *lexical.Index, vecs Matrix, idList []string, query string, q []float32, w [2]float64, allowed Set, lexLim, denLim int, lexQuery string, trace map[string]any) *Fusion {
	lim := lexLim
	if allowed != nil {
		lim = max(lexLim, len(idList))
	}
	var lexical []Scored
	for _, h := range lex.Search(lexQuery, lim) {
		if allowed == nil || allowed.Has(h.DocID) {
			lexical = append(lexical, Scored{h.DocID, h.Score})
			if len(lexical) >= lexLim {
				break
			}
		}
	}
	dense := denseTop(vecs, idList, q, allowed, denLim)
	lexName, denName := key+"_lexical", key+"_dense"
	f := fuse([]Run{{lexName, ids(lexical), w[0]}, {denName, ids(dense), w[1]}}, nil,
		map[string]map[string]float64{lexName: scoreMap(lexical), denName: scoreMap(dense)})
	if trace != nil {
		trace[key+"_rrf"] = f.IDs()[:min(lexLim, len(f.Ordered))]
	}
	return f
}

// generate is engine._generate_candidates.
func (ix *Index) generate(ctx context.Context, emb QueryEmbedder, query string, q []float32, it Intent, scope Set, useSummary bool, pool, lexLim, denLim int, timeScale, limitScale float64, ref *TimeReference) genResult {
	cfg := ix.Cfg
	rc := cfg.Retrieval
	w := cfg.weights()
	trace := map[string]any{}
	allowed, tmeta := ix.narrowTimeIDs(scope, it, timeScale, ref)
	trace["time"] = tmeta
	lexQuery, extra := ix.lexicalQueryExpanded(query, it)
	if len(extra) > 0 {
		trace["lexical_query_expanded"] = true
		trace["alias_labels_added"] = extra[:min(24, len(extra))]
	}
	leafF := ix.hybrid("leaf", ix.leafLex, ix.LeafVecs, ix.leafIDs, query, q, w, allowed, lexLim, denLim, lexQuery, trace)
	outer := []Run{{"leaf", leafF.IDs(), 1.0}}
	children := map[string]*Fusion{"leaf": leafF}
	raw := map[string]map[string]float64{}
	used := []string{"leaf_lexical", "leaf_dense"}
	if useSummary && boolOr(rc.Summaries.Enabled, true) && ix.HasSummaries() {
		sumF := ix.hybrid("summary", ix.summaryLex, ix.SummaryVecs, ix.summaryIDs, query, q, w, allowed, lexLim, denLim, lexQuery, trace)
		outer = append(outer, Run{"summary", sumF.IDs(), cfg.summaryWeight()})
		children["summary"] = sumF
		used = append(used, "summary_lexical", "summary_dense")
	}
	speakerIDs, aliasIDs := Set{}, Set{}
	targets := ix.rules.cleanTargets(it.Targets)
	resolved := false
	personOn, aliasOn := boolOr(rc.Person.Enabled, true), boolOr(rc.Alias.Enabled, true)
	if len(targets) > 0 && (personOn || aliasOn) {
		personScope := allowed
		if !boolOr(rc.Person.ResolveInScope, true) {
			personScope = nil
		}
		useIdentity := cfg.personIdentity() != "config"
		res := ix.resolvePersons(targets, personScope, useIdentity)
		trace["persons"] = res.Trace()
		resolved = res.Resolved()
		if personOn && resolved {
			spF, uni := ix.speakerFusion(res, query, q, allowed, intOr(rc.Person.Limit, 60), w)
			if len(spF.Ordered) > 0 {
				outer = append(outer, Run{"speaker", spF.IDs(), floatOr(rc.Person.RRFWeight, 0.3)})
				children["speaker"] = spF
				used = append(used, "speaker_lexical", "speaker_dense")
			}
			speakerIDs = uni
			trace["speaker_pool_n"] = len(uni)
		}
		if aliasOn && len(res.Labels) > 0 {
			if al := ix.aliasRun(res, allowed, intOr(rc.Alias.Limit, 60)); len(al) > 0 {
				outer = append(outer, Run{"alias_lexical", ids(al), floatOr(rc.Alias.RRFWeight, 0.15)})
				raw["alias_lexical"] = scoreMap(al)
				aliasIDs = setOf(ids(al)...)
				used = append(used, "alias_lexical")
			}
		}
	}
	mq := rc.MultiQuery
	queries := map[string]string{"q0": query}
	maxQ := intOr(mq.MaxQueries, 3)
	if boolOr(mq.Enabled, true) && maxQ > 1 {
		if sem := q1Text(query, it, mq); sem != "" && sem != query && len(queries) < maxQ && emb != nil {
			if v, err := emb.EmbedQuery(ctx, sem); err != nil {
				trace["multi_query_error"] = "embed"
			} else {
				v = FitQuery(v, ix.LeafVecs.Dims)
				leafD := denseTop(ix.LeafVecs, ix.leafIDs, v, allowed, denLim)
				runs := []Run{{"q1_dense", ids(leafD), 1.0}}
				qraw := map[string]map[string]float64{"q1_dense": scoreMap(leafD)}
				if _, ok := children["summary"]; ok {
					sumD := denseTop(ix.SummaryVecs, ix.summaryIDs, v, allowed, denLim)
					runs = append(runs, Run{"q1_summary_dense", ids(sumD), cfg.summaryWeight()})
					qraw["q1_summary_dense"] = scoreMap(sumD)
				}
				q1F := fuse(runs, nil, qraw)
				if len(q1F.Ordered) > 0 {
					outer = append(outer, Run{"q1", q1F.IDs(), floatOr(mq.SemanticRRFWeight, 1.0)})
					children["q1"] = q1F
					for _, r := range runs {
						used = append(used, r.Name)
					}
				}
				queries["q1"] = sem
			}
		}
		var terms []string
		for _, t := range it.KeyTerms {
			if t = strings.TrimSpace(t); t != "" {
				terms = append(terms, t)
			}
		}
		if len(terms) > 0 && len(queries) < maxQ {
			q2 := strings.Join(terms[:min(12, len(terms))], " ")
			if q2 != query {
				lim := lexLim
				if allowed != nil {
					lim = max(lexLim, len(ix.leafIDs))
				}
				var lex2 []Scored
				for _, h := range ix.leafLex.Search(q2, lim) {
					if allowed == nil || allowed.Has(h.DocID) {
						lex2 = append(lex2, Scored{h.DocID, h.Score})
					}
				}
				lex2 = lex2[:min(lexLim, len(lex2))]
				if len(lex2) > 0 {
					outer = append(outer, Run{"q2_lexical", ids(lex2), floatOr(mq.KeyTermsRRFWeight, 0.5)})
					raw["q2_lexical"] = scoreMap(lex2)
					used = append(used, "q2_lexical")
				}
				queries["q2"] = q2
			}
		}
	}
	trace["queries_used"] = queries
	var fusion *Fusion
	if len(outer) == 1 {
		fusion = leafF
	} else {
		fusion = fuse(outer, children, raw)
	}
	if cfg.messageDenseOn() && ix.HasMessageVectors() {
		// Single messages find the leaf that holds a one-line fact an averaged leaf vector buries. The run
		// joins the finished fusion as one more weighted list, so every existing candidate keeps its place
		// in the fused order it came with. With the setting off none of this runs and nothing is traced.
		t0 := time.Now()
		msgIDs, msgScores, scored := ix.messageDense(q, allowed, int(float64(cfg.messageDenseLimit())*limitScale))
		trace["message_dense"] = map[string]any{"messages_scored": scored, "leaves": len(msgIDs), "ms": float64(time.Since(t0).Microseconds()) / 1000, "dims": ix.MsgVecs.Dims}
		if len(msgIDs) > 0 {
			raw[messageRun] = msgScores
			fusion = fuse([]Run{{"fused", fusion.IDs(), 1.0}, {messageRun, msgIDs, cfg.messageDenseWeight()}}, map[string]*Fusion{"fused": fusion}, raw)
			used = append(used, messageRun)
		}
	}
	for _, c := range fusion.Ordered {
		if speakerIDs.Has(c.DocID) {
			c.Flags["speaker_match"] = true
		}
		if aliasIDs.Has(c.DocID) {
			c.Flags["alias_match"] = true
		}
	}
	trace["retrievers_used"] = used
	trace["candidate_union_n"] = len(fusion.Ordered)
	trace["candidate_sources"] = fusion.SourceCounts()
	trace["candidate_limits"] = map[string]any{"lexical": lexLim, "dense": denLim, "pool": pool, "time_scale": timeScale}
	return genResult{fusion: fusion, allowed: allowed, trace: trace, requested: len(targets) > 0, resolved: resolved}
}

// rerankDocument is engine._rerank_document (summary_body by default).
func (ix *Index) rerankDocument(id, format string) string {
	l, _ := ix.Leaf(id)
	summary := ix.summaryText[id]
	var parts []string
	speakers := l.Speakers[:min(8, len(l.Speakers))]
	if (format == "speaker_body" || format == "full") && len(speakers) > 0 {
		parts = append(parts, "[Speaker]\n"+strings.Join(speakers, ", "))
	}
	if format == "time_body" || format == "full" {
		if ts, ok := ix.leafEndTS[id]; ok {
			parts = append(parts, "[Time]\n"+time.Unix(int64(ts), 0).UTC().Format("2006-01-02 15:04"))
		}
	}
	if (format == "summary_body" || format == "full") && summary != "" {
		if format == "summary_body" {
			parts = append(parts, summary)
		} else {
			parts = append(parts, "[Summary]\n"+summary)
		}
	}
	if format == "full" {
		parts = append(parts, "[Content]\n"+l.Body)
	} else {
		parts = append(parts, l.Body)
	}
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	blob := strings.TrimSpace(strings.Join(kept, "\n"))
	if blob == "" {
		blob = l.Body
	}
	r := []rune(blob)
	return string(r[:min(2000, len(r))])
}

// ErrEmptyQuery is returned for a blank query.
var ErrEmptyQuery = errors.New("search: empty query")

// Retrieve is engine.retrieve (leaf_parent mode).
func (ix *Index) Retrieve(ctx context.Context, req Request, emb QueryEmbedder, rr Reranker) (Result, error) {
	if strings.TrimSpace(req.Query) == "" {
		return Result{}, ErrEmptyQuery
	}
	cfg := ix.Cfg
	rc := cfg.Retrieval
	topK := req.TopK
	if topK <= 0 {
		topK = cfg.topK()
	}
	wanted := req.Ranking
	if wanted == "" {
		wanted = retrieval.Auto
	}
	if wanted == retrieval.Heuristic {
		wanted = retrieval.Hybrid // R1 is sandbox ablation only; the product never runs it
	}
	auto := wanted == retrieval.Auto
	it := req.Intent
	q := FitQuery(req.QueryVector, ix.LeafVecs.Dims)
	pool := cfg.poolSize(topK, false)
	lexLim, denLim := cfg.searchLimits()
	scope := ix.scopeAllowed(req.Scope)
	gen := ix.generate(ctx, emb, req.Query, q, it, scope, true, pool, lexLim, denLim, 1.0, 1.0, req.TimeReference)
	trace := gen.trace
	ix.measureCandidates(req, gen, pool, "initial")
	mode := wanted
	reason := ""
	recovered := false
	if auto {
		th := cfg.thresholds()
		rerankOK := boolOr(rc.Rerank.Enabled, true) && rr != nil && rr.Available(req.RemainingUSD)
		a := assess(gen.fusion, th, gen.requested, gen.resolved)
		trace["auto_confidence"] = a
		dec := retrieval.DefaultAutoPolicy.Decide(a.Level, rerankOK, req.RemainingUSD, false)
		if dec.Recover && boolOr(rc.Recovery.Enabled, true) {
			scale := floatOr(rc.Recovery.LimitScale, 1.5)
			pool = cfg.poolSize(topK, true)
			gen2 := ix.generate(ctx, emb, req.Query, q, it, scope, true, pool,
				int(float64(lexLim)*scale), int(float64(denLim)*scale), floatOr(rc.Recovery.TimeWindowScale, 2.0), scale, req.TimeReference)
			before := a
			a = assess(gen2.fusion, th, gen2.requested, gen2.resolved)
			for k, v := range gen2.trace {
				trace[k] = v
			}
			trace["recovery"] = map[string]any{"before": before, "after": a, "candidate_union_n_after": len(gen2.fusion.Ordered)}
			trace["auto_confidence"] = a
			ix.measureCandidates(req, gen2, pool, "recovery")
			gen = gen2
			recovered = true
			dec = retrieval.DefaultAutoPolicy.Decide(a.Level, rerankOK, req.RemainingUSD, true)
		} else if dec.Recover {
			dec = retrieval.DefaultAutoPolicy.Decide(a.Level, rerankOK, req.RemainingUSD, true)
			if dec.Mode == retrieval.Rerank {
				dec.Reason = retrieval.ReasonRerankLowConfidence
			} else {
				dec.Reason = retrieval.ReasonNoRerank
			}
		}
		mode, reason = dec.Mode, dec.Reason
	}
	fusionIDs := gen.fusion.IDs()
	poolOrder, guaranteed, bySource := guaranteePool(gen.fusion, pool, cfg.perSourceMin(), cfg.reserveMaxFrac())
	poolIDs := poolOrder[:min(pool, len(poolOrder))]
	trace["fusion_ids"] = fusionIDs[:min(pool, len(fusionIDs))]
	trace["candidate_pool_ids"] = poolIDs
	trace["pool_guaranteed_ids"] = guaranteed
	trace["pool_guaranteed_by_source"] = bySource
	trace["recovery_performed"] = recovered
	if auto {
		trace["ranking_auto_reason"] = reason
	}
	var ranked []string
	abstained := false
	scoreOf := map[string]float64{}
	applied := string(mode)
	if mode == retrieval.Rerank {
		docs := make([]string, len(poolIDs))
		for i, id := range poolIDs {
			docs[i] = ix.rerankDocument(id, cfg.docFormat())
		}
		var res RerankResult
		var err error
		if len(docs) == 0 {
			err = errors.New("no candidates")
		} else {
			res, err = rr.Rerank(ctx, req.Query, docs, min(len(docs), max(topK, min(pool, len(docs)))))
		}
		if err != nil {
			fb, fbReason := retrieval.DefaultAutoPolicy.Fallback()
			if !auto {
				fb, fbReason = retrieval.Hybrid, retrieval.ReasonRerankFailed
			}
			applied = string(fb) + "_fallback"
			trace["rerank_error"] = errorKind(err)
			trace["ranking_auto_reason"] = fbReason
			ranked = fusionIDs[:min(topK, len(fusionIDs))]
		} else {
			floor := cfg.rerankMinScore(res.Model)
			seen := Set{}
			var order []string
			var scores []float64
			for i, idx := range res.Indices {
				if idx < 0 || idx >= len(poolIDs) {
					continue
				}
				order = append(order, poolIDs[idx])
				scores = append(scores, res.Scores[i])
				scoreOf[poolIDs[idx]] = res.Scores[i]
				if res.Scores[i] < floor || seen.Has(poolIDs[idx]) {
					continue
				}
				seen[poolIDs[idx]] = struct{}{}
				ranked = append(ranked, poolIDs[idx])
			}
			abstained = len(res.Indices) > 0 && len(ranked) == 0
			ranked = ranked[:min(topK, len(ranked))]
			trace["rerank_model"] = res.Model
			trace["rerank_min_score"] = floor
			trace["rerank_kept_n"] = len(seen)
			trace["rerank_order"] = order
			trace["rerank_scores"] = scores
			trace["rerank_cost_usd"] = res.CostUSD
			trace["rerank_latency_ms"] = float64(res.Latency.Microseconds()) / 1000
			trace["rerank_performed"] = true
		}
	} else {
		ranked = fusionIDs[:min(len(fusionIDs), max(topK*4, pool))]
		if cfg.HybridMinCos > 0 {
			var kept []string
			for _, id := range ranked {
				if i, ok := ix.leafPos[id]; ok && ix.LeafVecs.Dot(i, q) >= cfg.HybridMinCos {
					kept = append(kept, id)
				}
			}
			abstained = len(ranked) > 0 && len(kept) == 0
			ranked = kept
		}
	}
	trace["applied"] = applied
	trace["ranking"] = string(mode)
	trace["ranking_requested"] = string(wanted)
	trace["abstained"] = abstained
	final, mergeOf := ix.finalizeRanked(ranked, topK, it.Recent)
	trace["merged_leaf_ids"] = mergeOf
	// evidence scope
	ps := packScope{marks: ix.rules.search.Markers}
	switch {
	case req.Scope.SourceRoom != "" && len(req.Scope.Rooms) == 0:
		ps.allowedRooms = setOf(req.Scope.SourceRoom)
	case !req.Scope.All:
		ps.allowedObs = Set{}
		for id := range scope {
			if l, ok := ix.Leaf(id); ok {
				for _, o := range l.ObservationIDs {
					ps.allowedObs[o] = struct{}{}
				}
			}
		}
	}
	ts := ResolveTimeScope(it.Time, req.TimeReference, it.Recent)
	if ts.Hard() && (ts.Lo != nil || ts.Hi != nil) {
		ps.timeAllows = func(m corpus.Message) bool { return ts.ObservationStatus(m.ParsedTime) == "in" }
	}
	segs := make([]leaves.Leaf, len(final))
	for i, id := range final {
		if ex := mergeOf[id]; len(ex) > 0 {
			segs[i] = ix.mergedSpan(id, ex)
		} else {
			segs[i], _ = ix.Leaf(id)
		}
	}
	total := cfg.EvidenceTotalCharLimit
	if total == 0 {
		total = cfg.charLimit() * max(1, topK)
	}
	packed, spans, visible := packSet(ix.Messages, segs, cfg.parentBefore(), cfg.parentAfter(), boolOr(cfg.ContextPrefix, true), cfg.charLimit(), total, ps)
	var hits []Hit
	for i, id := range final {
		l, _ := ix.Leaf(id)
		p := packed[i]
		h := Hit{Rank: i + 1, DocID: id, Room: l.Room, Platform: corpus.PlatformOfRoom(l.Room), Speakers: l.Speakers,
			Excerpt: p.Excerpt, Summary: ix.summaryText[id], PackingStatus: p.Status, ObservationIDs: p.ObservationIDs,
			VisibleObservationIDs: p.VisibleObservations, VisibleSpans: p.VisibleSpans, LeafObservationIDs: l.ObservationIDs,
			MergedLeafIDs: mergeOf[id], Span: spanLabel(p, l)}
		if h.PackingStatus == "" {
			h.PackingStatus = statusInsuff
		}
		if c := gen.fusion.ByID(id); c != nil {
			h.Sources = c.Sources
		}
		if s, ok := scoreOf[id]; ok {
			v := s
			h.RerankScore = &v
		}
		if pos, ok := ix.leafPos[id]; ok {
			h.DenseCos = ix.LeafVecs.Dot(pos, q)
		}
		h.Date = leafDate(ix, l)
		if d := contextOnlyDate(ix, p, l); d != "" {
			h.Date = d
		}
		hits = append(hits, h)
	}
	trace["visible_spans"] = spans
	trace["visible_observation_ids"] = visible
	state := retrieval.StateFor(true, len(hits), abstained)
	if len(hits) == 0 {
		if abstained {
			trace["abstain_reason"] = "rerank_floor"
		} else {
			trace["abstain_reason"] = "empty_in_allowed_scope"
		}
	}
	return Result{Hits: hits, State: state, Trace: trace}, nil
}

func spanLabel(p Packed, l leaves.Leaf) string {
	start, end := p.Start, p.End
	if end == 0 {
		start, end = l.Start, l.End
	}
	return itoa(start) + "-" + itoa(max(end, 1)-1)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

// leafDate is the date of the leaf's last message in the source's own frame (naive Kakao times are
// not shifted by a host zone).
func leafDate(ix *Index, l leaves.Leaf) string {
	for _, i := range []int{l.End - 1, l.Start} {
		if i >= 0 && i < len(ix.Messages) {
			if t, _, ok := corpus.ParseTime(ix.Messages[i].ParsedTime); ok {
				return t.Format("2006-01-02")
			}
		}
	}
	return ""
}

// contextOnlyDate dates a hit whose own messages were all shown by an earlier hit, so its excerpt holds
// only the messages around them: the label then covers what the model actually reads ("2026-08-11" or
// "2026-08-11~2026-09-06") instead of a matched message it does not see here. Other hits keep the
// leaf's date ("" = unchanged). engine.py _context_only_date is the same rule.
func contextOnlyDate(ix *Index, p Packed, l leaves.Leaf) string {
	if len(p.VisibleObservations) == 0 {
		return ""
	}
	visible := setOf(p.VisibleObservations...)
	for _, o := range l.ObservationIDs {
		if visible.Has(o) {
			return ""
		}
	}
	first, last := "", ""
	for i := p.Start; i < p.End && i < len(ix.Messages); i++ {
		m := ix.Messages[i]
		if !visible.Has(m.ObservationID) {
			continue
		}
		if t, _, ok := corpus.ParseTime(m.ParsedTime); ok {
			d := t.Format("2006-01-02")
			if first == "" {
				first = d
			}
			last = d
		}
	}
	if first == "" || first == last {
		return first
	}
	return first + "~" + last
}

func errorKind(err error) string {
	var k interface{ Kind() string }
	if errors.As(err, &k) {
		return k.Kind()
	}
	return "rerank_error"
}

// CandidateMeasurement is a detached snapshot immediately after generate, before
// reranking. Runs carry each independent RRF rank; Pool is the guaranteed pool.
// Final hits belong to the returned Result, never to the product Trace schema.
type CandidateMeasurement struct {
	Stage      string              `json:"stage"`
	Candidates []Candidate         `json:"candidates"`
	Runs       map[string][]string `json:"runs"`
	Weights    map[string]float64  `json:"weights"`
	Pool       []string            `json:"pool"`
	Confidence *Assessment         `json:"auto_confidence,omitempty"`
}

func (ix *Index) measureCandidates(req Request, gen genResult, pool int, stage string) {
	if req.OnCandidatesForMeasurement == nil {
		return
	}
	m := CandidateMeasurement{Stage: stage, Runs: map[string][]string{}, Weights: map[string]float64{}}
	for _, c := range gen.fusion.Ordered {
		cp := *c
		cp.Sources = map[string]int{}
		for k, v := range c.Sources {
			cp.Sources[k] = v
		}
		cp.Flags = map[string]bool{}
		for k, v := range c.Flags {
			cp.Flags[k] = v
		}
		cp.Raw = nil
		m.Candidates = append(m.Candidates, cp)
	}
	for k, v := range gen.fusion.Runs {
		m.Runs[k] = append([]string(nil), v...)
	}
	for k, v := range gen.fusion.Weights {
		m.Weights[k] = v
	}
	order, _, _ := guaranteePool(gen.fusion, pool, ix.Cfg.perSourceMin(), ix.Cfg.reserveMaxFrac())
	m.Pool = append([]string(nil), order[:min(pool, len(order))]...)
	if req.Ranking == "" || req.Ranking == retrieval.Auto {
		a := assess(gen.fusion, ix.Cfg.thresholds(), gen.requested, gen.resolved)
		m.Confidence = &a
	}
	req.OnCandidatesForMeasurement(m)
}
