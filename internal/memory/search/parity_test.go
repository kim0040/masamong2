package search

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"

	"masamong/internal/lang"
	"masamong/internal/memory/corpus"
	"masamong/internal/memory/leaves"
	"masamong/internal/memory/retrieval"
)

// Parity with the sandbox engine.retrieve on a real index. Needs a dump made by the sandbox
// (chat text — never committed), e.g. experimental/rag-leaf-parent/parity_dump.py (no API calls):
//
//	MASAMONG_SANDBOX_DATA=<test-sandbox/data> MASAMONG_SANDBOX_CONFIG=<config.json>
//	MASAMONG_PARITY_RETRIEVE=<retrieve_<gold>.json> go test ./internal/memory/search/ -run Parity -v

type mapEmbedder map[string][]float32

func (m mapEmbedder) EmbedQuery(_ context.Context, text string) ([]float32, error) {
	v, ok := m[text]
	if !ok {
		return nil, fmt.Errorf("uncached")
	}
	return append([]float32(nil), v...), nil
}

// fakeRerank mirrors the dump's deterministic reranker (score from document length).
type fakeRerank struct{ avail bool }

func (f fakeRerank) Available(float64) bool { return f.avail }
func (fakeRerank) Rerank(_ context.Context, _ string, docs []string, topN int) (RerankResult, error) {
	score := func(d string) float64 { return 0.3 + 0.6*float64((utf8.RuneCountInString(d)*7919)%1000)/1000.0 }
	idx := make([]int, len(docs))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return score(docs[idx[a]]) > score(docs[idx[b]]) })
	idx = idx[:min(topN, len(idx))]
	out := RerankResult{Model: "voyageai/rerank-2.5-lite", Indices: idx}
	for _, i := range idx {
		out.Scores = append(out.Scores, score(docs[i]))
	}
	return out, nil
}

// LoadSandboxIndex builds an Index from sandbox artifacts (parity/dev only).
func LoadSandboxIndex(t *testing.T, dataDir, cfgPath string, roomFilter ...[]string) *Index {
	t.Helper()
	cfg := Default()
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	var msgs []corpus.Message
	var lv []leaves.Leaf
	var sums []struct {
		DocID   string `json:"doc_id"`
		Summary string `json:"summary"`
	}
	readJSON(t, filepath.Join(dataDir, "messages.json"), &msgs)
	readJSON(t, filepath.Join(dataDir, "leaves.json"), &lv)
	vecs, err := LoadNPY(filepath.Join(dataDir, "leaf_vecs.npy"))
	if err != nil {
		t.Fatal(err)
	}
	var summaries []Summary
	var svecs Matrix
	if cfg.SummaryEnabled {
		readJSON(t, filepath.Join(dataDir, "summaries.json"), &sums)
		for _, s := range sums {
			summaries = append(summaries, Summary{DocID: s.DocID, Text: s.Summary})
		}
		if svecs, err = LoadNPY(filepath.Join(dataDir, "summary_vecs.npy")); err != nil {
			t.Fatal(err)
		}
	}
	sandboxLang := lang.MergeSearch([]string{"ko"}, []string{"마사몽"}) // the sandbox pronoun list names the bot
	cfg.Language = &sandboxLang
	var selected []string
	if len(roomFilter) > 0 {
		selected = roomFilter[0]
	} else if path := os.Getenv("MASAMONG_SANDBOX_ROOMS_FILE"); path != "" {
		readJSON(t, path, &selected)
		if selected == nil {
			selected = []string{}
		}
	}
	ix, err := buildSandboxWithRooms(cfg, msgs, lv, vecs, summaries, svecs, selected)
	if err != nil {
		t.Fatal(err)
	}
	return ix
}

// The full loader and TestSandboxRetrieveParity body remain on the original
// Build path unless the measurement-only filter is explicitly supplied.
func buildSandboxWithRooms(cfg Config, msgs []corpus.Message, lv []leaves.Leaf, vecs Matrix, sums []Summary, svecs Matrix, selected []string) (*Index, error) {
	if selected == nil {
		return Build(cfg, msgs, lv, vecs, sums, svecs)
	}
	identity := NewIdentityInputs(msgs, lv)
	rooms := map[string]bool{}
	for _, room := range selected {
		rooms[room] = true
	}
	var residentMsgs []corpus.Message
	positions := map[int]int{}
	for i, m := range msgs {
		if rooms[m.Room] {
			positions[i] = len(residentMsgs)
			residentMsgs = append(residentMsgs, m)
		}
	}
	var residentLeaves []leaves.Leaf
	residentVecs := Matrix{Dims: vecs.Dims}
	kept := Set{}
	for i, l := range lv {
		if !rooms[l.Room] {
			continue
		}
		if !(l.Start >= 0 && l.Start < l.End && l.End <= len(msgs)) {
			return nil, fmt.Errorf("invalid sandbox leaf interval")
		}
		start, ok := positions[l.Start]
		end, lastOK := positions[l.End-1]
		if !ok || !lastOK || end-start+1 != l.End-l.Start {
			return nil, fmt.Errorf("sandbox leaf crosses resident rooms")
		}
		l.Start, l.End = start, end+1
		residentLeaves = append(residentLeaves, l)
		kept[l.DocID] = struct{}{}
		residentVecs.Data = append(residentVecs.Data, vecs.Row(i)...)
	}
	var residentSums []Summary
	residentSvecs := Matrix{Dims: svecs.Dims}
	for i, s := range sums {
		if kept.Has(s.DocID) {
			residentSums = append(residentSums, s)
			residentSvecs.Data = append(residentSvecs.Data, svecs.Row(i)...)
		}
	}
	return BuildWithIdentity(cfg, residentMsgs, residentLeaves, residentVecs, residentSums, residentSvecs, identity)
}

func TestSandboxPartialRetrieveParity(t *testing.T) {
	if os.Getenv("MASAMONG_SANDBOX_ROOMS_FILE") == "" {
		t.Skip("partial parity filter not set")
	}
	TestSandboxRetrieveParity(t)
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatal(err)
	}
}

type dumpHit struct {
	DocID   string   `json:"doc_id"`
	Excerpt string   `json:"excerpt"`
	Status  string   `json:"status"`
	Visible []string `json:"visible"`
	Merged  []string `json:"merged"`
	Summary string   `json:"summary"`
	Date    *string  `json:"date"` // dumps made before 2026-09-30 have none
	Score   *float64 `json:"score"`
}

type dumpMode struct {
	Error           string                 `json:"error"`
	Hits            []dumpHit              `json:"hits"`
	PackingVariants [][]dumpHit            `json:"packing_variants"`
	FusionIDs       []string               `json:"fusion_ids"`
	Scores          map[string]parityScore `json:"scores"`
	Tree            *parityTree            `json:"tree"`
	FinalScores     map[string]float64     `json:"final_scores"`
	PoolIDs         []string               `json:"pool_ids"`
	Auto            *struct {
		Level string `json:"level"`
	} `json:"auto"`
	Reason    string            `json:"reason"`
	Applied   string            `json:"applied"`
	Abstained bool              `json:"abstained"`
	Queries   map[string]string `json:"queries"`
	Recovery  bool              `json:"recovery"`
}

func TestSandboxRetrieveParity(t *testing.T) {
	dataDir, cfgPath, dumpPath := os.Getenv("MASAMONG_SANDBOX_DATA"), os.Getenv("MASAMONG_SANDBOX_CONFIG"), os.Getenv("MASAMONG_PARITY_RETRIEVE")
	if dataDir == "" || cfgPath == "" || dumpPath == "" {
		t.Skip("parity env not set")
	}
	ix := LoadSandboxIndex(t, dataDir, cfgPath)
	defer ix.Close()
	var dump struct {
		Rows []struct {
			QuestionID string              `json:"question_id"`
			Query      string              `json:"query"`
			Room       string              `json:"room"`
			Intent     *Intent             `json:"intent"`
			Modes      map[string]dumpMode `json:"modes"`
		} `json:"rows"`
		Vectors map[string][]float32 `json:"vectors"`
	}
	readJSON(t, dumpPath, &dump)
	emb := mapEmbedder(dump.Vectors)
	stats := map[string]int{}
	var diagnostics []parityRunDiagnostic
	for rowNumber, row := range dump.Rows {
		it := Intent{Kind: "other", Source: "none"}
		if row.Intent != nil {
			it = *row.Intent
		}
		scope := Scope{SourceRoom: row.Room}
		if row.Room == "" {
			scope = Scope{All: true}
		}
		for _, mode := range []string{"hybrid", "auto", "rerank"} {
			want, exists := row.Modes[mode]
			if !exists {
				stats["missing_mode"]++
				continue
			}
			if want.Error != "" {
				stats["dump_error"]++
				continue
			}
			stats["runs"]++
			avail := mode == "rerank"
			res, err := ix.Retrieve(context.Background(), Request{Query: row.Query, QueryVector: dump.Vectors[row.Query], Intent: it,
				Ranking: retrieval.RankingMode(mode), Scope: scope, TopK: 5, RemainingUSD: 0.2}, emb, fakeRerank{avail: avail})
			if err != nil {
				t.Fatalf("%s %s: %v", row.QuestionID, mode, err)
			}
			tag := row.QuestionID + "/" + mode
			f := parityGenerate(ix, row.Query, dump.Vectors[row.Query], it, scope, emb, res)
			proof := parityProof(f, want)
			if want.Tree == nil || len(want.Scores) == 0 {
				t.Fatal("점수/트리가 없는 덤프입니다: parity_dump.py로 다시 만드세요")
			}
			if !proof {
				stats["score_proof_diff"]++
			}
			gotFusion := asStrings(res.Trace["fusion_ids"])
			recovered, _ := res.Trace["recovery_performed"].(bool)
			pool := ix.Cfg.poolSize(5, recovered)
			original, originalErr := parityReplay(want.Tree, f, false)
			if originalErr != nil {
				stats["score_proof_diff"]++
				proof = false
			} else {
				if !reflect.DeepEqual(nonEmpty(want.FusionIDs), nonEmpty(original.IDs()[:min(pool, len(original.Ordered))])) {
					stats["score_proof_diff"]++
					proof = false
				}
				originalPool, _, _ := guaranteePool(original, pool, ix.Cfg.perSourceMin(), ix.Cfg.reserveMaxFrac())
				if !reflect.DeepEqual(nonEmpty(want.PoolIDs), nonEmpty(originalPool[:min(pool, len(originalPool))])) {
					stats["score_proof_diff"]++
					proof = false
				}
			}
			if !reflect.DeepEqual(gotFusion, f.IDs()[:min(len(f.Ordered), pool)]) {
				stats["score_proof_diff"]++
				proof = false
			}
			poolOrder, _, _ := guaranteePool(f, pool, ix.Cfg.perSourceMin(), ix.Cfg.reserveMaxFrac())
			if !reflect.DeepEqual(asStrings(res.Trace["candidate_pool_ids"]), nonEmpty(poolOrder[:min(pool, len(poolOrder))])) {
				stats["score_proof_diff"]++
				proof = false
			}
			if os.Getenv("MASAMONG_PARITY_NUMERIC") != "" {
				diagnostics = append(diagnostics, parityNumbers(ix, rowNumber+1, mode, f, res, want, proof))
			}
			if got := asStrings(res.Trace["fusion_ids"]); !reflect.DeepEqual(got, nonEmpty(want.FusionIDs)) {
				stats["fusion_raw_diff"]++
				if !proof || !paritySameSet(got, want.FusionIDs) {
					stats["fusion_diff"]++
				} else {
					stats["fusion_epsilon"]++
				}
				t.Logf("%s fusion differs at %d", tag, firstDiff(got, want.FusionIDs))
				parityDiagnose(t, ix, row.Query, dump.Vectors[row.Query], it, scope, emb, res, want)
			}
			if got := asStrings(res.Trace["candidate_pool_ids"]); !reflect.DeepEqual(got, nonEmpty(want.PoolIDs)) {
				if !proof || !paritySameSet(got, want.PoolIDs) {
					stats["pool_diff"]++
				}
			}
			if want.Auto != nil {
				a := res.Trace["auto_confidence"].(Assessment)
				if string(a.Level) != want.Auto.Level {
					stats["auto_level_diff"]++
					t.Logf("%s auto level %s want %s", tag, a.Level, want.Auto.Level)
				}
				if res.Trace["ranking_auto_reason"] != want.Reason {
					stats["reason_diff"]++
				}
			}
			if res.Trace["applied"] != want.Applied || res.Trace["abstained"] != want.Abstained || res.Trace["recovery_performed"] != want.Recovery {
				stats["decision_diff"]++
			}
			var gotIDs, wantIDs []string
			finalScores := map[string]float64{}
			for _, h := range res.Hits {
				gotIDs = append(gotIDs, h.DocID)
				if h.RerankScore != nil {
					finalScores[h.DocID] = *h.RerankScore
				}
			}
			for _, h := range want.Hits {
				wantIDs = append(wantIDs, h.DocID)
			}
			if !reflect.DeepEqual(gotIDs, wantIDs) {
				stats["hits_raw_diff"]++
			}
			if !parityHitOrder(gotIDs, wantIDs, f, want, proof, finalScores) {
				stats["hits_diff"]++
				t.Logf("%s hits order or membership differs", tag)
				continue
			}
			if !reflect.DeepEqual(gotIDs, wantIDs) {
				stats["hits_epsilon"]++
			}
			expectedHits, ok := parityExpectedHits(gotIDs, want, proof)
			if !ok {
				stats["packing_variant_missing"]++
				continue
			}
			for i, h := range res.Hits {
				w := expectedHits[i]
				if w.Score == nil {
					stats["final_score_diff"]++
				} else if h.RerankScore != nil {
					if !parityNear(*h.RerankScore, *w.Score) {
						stats["final_score_diff"]++
					}
				} else if s, ok := want.Scores[h.DocID]; !ok || s.Score != *w.Score {
					stats["final_score_diff"]++
				}
				if h.Excerpt != w.Excerpt {
					stats["excerpt_diff"]++
					t.Logf("%s hit %d excerpt differs", tag, i)
				}
				if h.PackingStatus != w.Status || !reflect.DeepEqual(nonEmpty(h.VisibleObservationIDs), nonEmpty(w.Visible)) || !reflect.DeepEqual(nonEmpty(h.MergedLeafIDs), nonEmpty(w.Merged)) {
					stats["packing_diff"]++
				}
				if h.Summary != w.Summary {
					stats["summary_diff"]++
				}
				if w.Date != nil {
					if h.Date != *w.Date {
						stats["date_diff"]++
						t.Logf("%s hit %d date %q want %q", tag, i, h.Date, *w.Date)
					}
					if strings.Contains(h.Date, "~") || (len(h.VisibleObservationIDs) > 0 && !overlaps(h.VisibleObservationIDs, h.LeafObservationIDs)) {
						stats["context_only_hits"]++
					}
				}
				stats["hits_compared"]++
			}
		}
	}
	if path := os.Getenv("MASAMONG_PARITY_NUMERIC"); path != "" {
		data, err := json.MarshalIndent(diagnostics, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, k := range []string{"fusion_diff", "hits_diff", "pool_diff"} {
		stats[k] += 0
	}
	t.Logf("parity: %v", stats)
	for _, k := range []string{"fusion_diff", "pool_diff", "hits_diff", "excerpt_diff", "packing_diff", "packing_variant_missing", "summary_diff", "auto_level_diff", "reason_diff", "date_diff", "decision_diff", "score_proof_diff", "final_score_diff", "missing_mode", "dump_error"} {
		if stats[k] > 0 {
			t.Errorf("%s = %d", k, stats[k])
		}
	}
}

func overlaps(a, b []string) bool {
	in := setOf(a...)
	for _, x := range b {
		if in.Has(x) {
			return true
		}
	}
	return false
}

func asStrings(v any) []string {
	s, _ := v.([]string)
	return nonEmpty(s)
}

func nonEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func firstDiff(a, b []string) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}
