package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"masamong/internal/application"
	"masamong/internal/memory/corpus"
	"masamong/internal/memory/engine"
	"masamong/internal/memory/leaves"
	"masamong/internal/memory/retrieval"
	"masamong/internal/memory/search"
	"masamong/internal/memory/store"
	"masamong/internal/providers/openai"
	"masamong/internal/storage"
)

// Gate files may contain private identifiers. Reject paths outside this repository's
// scratch directory, including existing symlinks/junctions pointing outside it.
func scratchPath(path string) error {
	root, err := repositoryRoot()
	if err != nil {
		return err
	}
	return scratchPathAt(root, path)
}

// repositoryRoot uses the root go.mod so relative scratch paths have the same
// meaning when the runner or its tests are launched from a package directory.
func repositoryRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	dir = filepath.Clean(dir)
	for {
		if info, e := os.Stat(filepath.Join(dir, "go.mod")); e == nil && !info.IsDir() {
			return dir, nil
		} else if e != nil && !os.IsNotExist(e) {
			return "", e
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("repository root unavailable")
		}
		dir = parent
	}
}

// scratchPathAt is separated from repositoryRoot so path validation can be
// tested with an isolated temporary repository.
func scratchPathAt(root, path string) error {
	if path == "" {
		return errors.New("scratch path required")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return errors.New("repository root unavailable")
	}
	base := filepath.Join(root, ".masamong-scratch")
	target := path
	if !filepath.IsAbs(target) {
		target = filepath.Join(root, target)
	}
	abs, err := filepath.Abs(target)
	if err != nil {
		return errors.New("invalid scratch path")
	}
	rel, err := filepath.Rel(base, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return errors.New("gate paths must stay under .masamong-scratch/")
	}

	// Lstat each existing component. EvalSymlinks misses dangling links and
	// Windows junctions are ModeIrregular on Go 1.23+.
	current := base
	components := []string{}
	if rel != "." {
		components = strings.Split(rel, string(os.PathSeparator))
	}
	for i := -1; i < len(components); i++ {
		if i >= 0 {
			current = filepath.Join(current, components[i])
		}
		info, e := os.Lstat(current)
		if os.IsNotExist(e) {
			break
		}
		if e != nil {
			return errors.New("cannot inspect gate path")
		}
		if info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			return errors.New("gate paths cannot contain symlinks or junctions")
		}
	}
	return nil
}

func writeGateJSON(path string, v any) error {
	if err := scratchPath(path); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0600)
}

func gateVector(text string) []float32 {
	v := make([]float32, 16)
	r := []rune(text)
	for i := 0; i+1 < len(r); i++ {
		h := fnv.New32a()
		_, _ = h.Write([]byte(string(r[i : i+2])))
		v[h.Sum32()%16]++
	}
	search.Normalize(v)
	return v
}

type gateEmbedder struct{}

func (gateEmbedder) EmbedQuery(_ context.Context, text string) ([]float32, error) {
	return gateVector(text), nil
}

// Synthetic fixtures are intentionally invented and generated into scratch. No
// application import/index action or network provider is needed to build them.
func syntheticRoot(root string) error {
	if err := scratchPath(root); err != nil {
		return err
	}
	dbPath := filepath.Join(root, "masamong.db")
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		return errors.New("synthetic root must be new")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	db, err := storage.Open(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	st, err := store.Open(db)
	if err != nil {
		return err
	}
	rooms := []string{"discord:gate:A", "discord:gate:B", "discord:gate:C"}
	base := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	var batch []corpus.Observation
	for ri, room := range rooms {
		for i := 0; i < 16; i++ {
			label, account := "합성화자", "synthetic-account"
			if ri == 2 {
				label = "합성별칭"
			}
			text := []string{"합성 캠핑 준비 텐트", "합성 도서관 책 반납", "합성 기차 일정 지도"}[ri]
			batch = append(batch, corpus.Observation{ObservationID: fmt.Sprintf("gate-%d-%02d", ri, i), Platform: "discord", Collection: "synthetic-gate", SourceRoom: room,
				ActorLabel: label, ActorAccount: account, Content: text, ParsedTime: base.Add(time.Duration(ri*48)*time.Hour + time.Duration(i)*time.Minute).Format(time.RFC3339), RecordKind: corpus.HumanText})
		}
	}
	if _, err := st.InsertObservations(batch, nil); err != nil {
		return err
	}
	r := leaves.DefaultRecipe()
	for _, room := range rooms {
		if _, err := st.SyncLeaves(room, r, base.AddDate(1, 0, 0), time.UTC); err != nil {
			return err
		}
	}
	space := "synthetic-embed:16:l2"
	rows, err := st.DB.DB.Query(`SELECT leaf_id, body FROM memory_leaves WHERE closed=1 ORDER BY leaf_id`)
	if err != nil {
		return err
	}
	type fixtureLeaf struct{ id, body string }
	var lv []fixtureLeaf
	for rows.Next() {
		var l fixtureLeaf
		if err := rows.Scan(&l.id, &l.body); err != nil {
			rows.Close()
			return err
		}
		lv = append(lv, l)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, l := range lv {
		if err := st.SaveLeafVector(l.id, space, search.EncodeFloat16(gateVector(l.body))); err != nil {
			return err
		}
	}
	qs := []question{
		{ID: "gate-pair", Query: "합성 캠핑", Rooms: rooms[:2], Want: []string{"gate-0-00"}},
		{ID: "gate-multi", Query: "합성 도서관", Rooms: rooms[:2], Want: []string{"gate-1-00"}},
		{ID: "gate-sequence", Query: "합성 캠핑", Rooms: rooms[:1], Want: []string{"gate-0-00"}},
		{ID: "gate-alias", Query: "합성별칭 캠핑", Rooms: rooms[:1], Intent: search.Intent{Kind: "person", Targets: []string{"합성별칭"}, Source: "planner"}, Want: []string{"gate-0-00"}},
		{ID: "gate-time", Query: "합성 캠핑", Rooms: rooms, Intent: search.Intent{Time: &search.TimeSpec{Kind: "before", To: "2025-01-02"}}, Want: []string{"gate-0-00"}},
		{ID: "gate-negative", Query: "합성 존재하지 않는 기록", Rooms: rooms, Intent: search.Intent{Time: &search.TimeSpec{Kind: "before", To: "2024-01-01"}}, Abstain: true},
		{ID: "gate-nonexistent-room", Query: "합성 캠핑", Rooms: []string{"discord:gate:missing"}, Abstain: true},
		{ID: "gate-empty-scope", Query: "합성 캠핑", Rooms: []string{}, Abstain: true},
		{ID: "gate-auto", Query: "합성 캠핑", Rooms: rooms[:1], Want: []string{"gate-0-00"}},
		{ID: "gate-partial", Query: "합성 기차", Rooms: rooms[2:], Want: []string{"gate-2-00"}},
	}
	if err := writeGateJSON(filepath.Join(root, "gold.json"), map[string]any{"questions": qs}); err != nil {
		return err
	}
	fmt.Printf("synthetic rooms=%d messages=%d leaves=%d questions=%d\n", len(rooms), len(batch), len(lv), len(qs))
	return nil
}

type deterministicRerank struct{}

type disabledRerank struct{}

func (disabledRerank) Available(float64) bool { return false }
func (disabledRerank) Rerank(context.Context, string, []string, int) (search.RerankResult, error) {
	return search.RerankResult{}, errors.New("measurement rerank disabled")
}

func (deterministicRerank) Available(float64) bool { return true }
func (deterministicRerank) Rerank(_ context.Context, _ string, docs []string, topN int) (search.RerankResult, error) {
	res := search.RerankResult{Model: "synthetic-rerank"}
	for i := 0; i < min(len(docs), topN); i++ {
		res.Indices = append(res.Indices, i)
		res.Scores = append(res.Scores, 0.9-float64(i)*0.01)
	}
	return res, nil
}

type providerRerank struct {
	adapter *openai.Adapter
	model   string
}

func (p providerRerank) Available(float64) bool { return p.adapter != nil && p.adapter.Configured() }
func (p providerRerank) Rerank(ctx context.Context, query string, docs []string, n int) (search.RerankResult, error) {
	if !p.Available(0) {
		return search.RerankResult{}, errors.New("rerank provider unavailable")
	}
	r, err := p.adapter.Rerank(ctx, p.model, query, docs, n)
	if err != nil {
		return search.RerankResult{}, errors.New("rerank provider failed")
	}
	if r.Model == "" {
		r.Model = p.model
	}
	res := search.RerankResult{Indices: r.Indices, Scores: r.Scores, Model: r.Model}
	if r.CostUSD != nil {
		res.CostUSD = *r.CostUSD
	}
	return res, nil
}

type rerankTape struct {
	Mode     string
	Entries  map[string]search.RerankResult
	Provider search.Reranker
	Missing  int
	Calls    int
	Failures int
	// Spent is the provider cost of the calls recorded by this run; SpendCap > 0 stops recording once it
	// is reached (the failure is reported, never hidden), so a real-data record run has a hard ceiling.
	Spent    float64
	SpendCap float64
}

func (t *rerankTape) Available(b float64) bool {
	return t.Mode == "replay" || (t.Provider != nil && t.Provider.Available(b))
}
func (t *rerankTape) Rerank(ctx context.Context, q string, docs []string, n int) (search.RerankResult, error) {
	raw, _ := json.Marshal(struct {
		Query string
		Docs  []string
		TopN  int
	}{q, docs, n})
	key := fmt.Sprintf("%x", sha256.Sum256(raw))
	t.Calls++
	if r, ok := t.Entries[key]; ok {
		if !validTapeResponse(r) {
			t.Failures++
			return search.RerankResult{}, errors.New("invalid rerank response")
		}
		return r, nil
	}
	if t.Mode == "replay" {
		t.Missing++
		return search.RerankResult{}, errors.New("rerank tape miss")
	}
	if t.Provider == nil {
		t.Failures++
		return search.RerankResult{}, errors.New("rerank provider unavailable")
	}
	if t.SpendCap > 0 && t.Spent >= t.SpendCap {
		t.Failures++
		return search.RerankResult{}, errors.New("rerank spend cap reached")
	}
	r, err := t.Provider.Rerank(ctx, q, docs, n)
	t.Spent += r.CostUSD
	if err == nil && !validTapeResponse(r) {
		err = errors.New("invalid rerank response")
	}
	if err == nil {
		r.Latency = 0
		r.CostUSD = 0
		t.Entries[key] = r
	} else {
		t.Failures++
	}
	return r, err
}

func validTapeResponse(r search.RerankResult) bool {
	if len(r.Indices) != len(r.Scores) {
		return false
	}
	for _, score := range r.Scores {
		if math.IsNaN(score) || math.IsInf(score, 0) {
			return false
		}
	}
	return true
}

type gateHit struct {
	Rank           int            `json:"rank"`
	DocID          string         `json:"doc_id"`
	ObservationIDs []string       `json:"observation_ids"`
	VisibleIDs     []string       `json:"visible_observation_ids"`
	MergedIDs      []string       `json:"merged_leaf_ids"`
	LeafIDs        []string       `json:"leaf_observation_ids"`
	Sources        map[string]int `json:"sources"`
	RerankScore    *float64       `json:"rerank_score,omitempty"`
	DenseCos       float64        `json:"dense_cos"`
}
type candidateRow struct {
	ID          string                        `json:"question_id"`
	Mode        string                        `json:"mode"`
	Correct     bool                          `json:"correct"`
	ScopeKind   string                        `json:"scope_kind"`
	Stages      []search.CandidateMeasurement `json:"stages"`
	Recovery    bool                          `json:"recovery"`
	Confidence  any                           `json:"auto_confidence,omitempty"`
	AutoReason  any                           `json:"auto_reason,omitempty"`
	Applied     any                           `json:"applied"`
	RerankError any                           `json:"rerank_error,omitempty"`
	Hits        []gateHit                     `json:"hits"`
	State       retrieval.State               `json:"state"`
}

func independentGateEngine(root string, synthetic bool) (*engine.Engine, func(), search.QueryEmbedder, search.Reranker, error) {
	if synthetic {
		db, err := storage.Open(filepath.Join(root, "masamong.db"))
		if err != nil {
			return nil, nil, nil, nil, err
		}
		st, err := store.Open(db)
		if err != nil {
			db.Close()
			return nil, nil, nil, nil, err
		}
		e := engine.New(st, nil, engine.Models{Embed: "synthetic-embed", EmbedDims: 16}, engine.Settings{Search: search.Default(), Recipe: leaves.DefaultRecipe()})
		return e, func() { e.Close(); db.Close() }, gateEmbedder{}, deterministicRerank{}, nil
	}
	// Open an offline scratch copy, then construct a separate engine. Never mutate
	// the Service engine/config, and never enable a query embedding provider here.
	svc, err := application.Open(root, nil, false)
	if err != nil {
		return nil, nil, nil, nil, errors.New("open scratch root failed")
	}
	product, err := svc.MemoryEngine()
	if err != nil {
		svc.Close()
		return nil, nil, nil, nil, errors.New("memory engine unavailable")
	}
	e := engine.New(product.Store, nil, product.Models, product.Settings)
	e.Settings.MessageDims = setMsgdims
	if setDims > 0 {
		e.Models.EmbedDims = setDims
	}
	if setMsgvec {
		e.Settings.MessageVectors = true
	}
	adapter := product.RerankLLM
	if adapter == nil {
		adapter = product.LLM
	}
	return e, func() { e.Close(); svc.Close() }, e, providerRerank{adapter, product.Models.Rerank}, nil
}

// recordSpendCap is the -max-spend ceiling of a record run (USD, 0 = none).
var recordSpendCap float64

// residentSequence, when set, makes dump-candidates run the same questions after each resident set in turn
// (-resident-seq-file); output goes to <out>.stepN.jsonl.
var residentSequence [][]string

// residentStepPath names the output of step i of a resident sequence. The name is derived from -out, so
// it is checked again: -out .masamong-scratch would otherwise put it next to the folder.
func residentStepPath(out string, i int) (string, error) {
	step := fmt.Sprintf("%s.step%d.jsonl", strings.TrimSuffix(out, ".jsonl"), i+1)
	return step, scratchPath(step)
}

func dumpCandidates(root, gold, out, resident, tapePath, rrMode string, synthetic bool, modes []string) error {
	for _, p := range []string{root, gold, out} {
		if err := scratchPath(p); err != nil {
			return err
		}
	}
	if rrMode != "off" && rrMode != "record" && rrMode != "replay" {
		return errors.New("invalid rerank-mode")
	}
	e, closeFn, emb, provider, err := independentGateEngine(root, synthetic)
	if err != nil {
		return err
	}
	defer closeFn()
	rooms, err := e.Store.Rooms()
	if err != nil {
		return err
	}
	roomSet := map[string]bool{}
	for _, r := range rooms {
		roomSet[r.Room.Room] = true
	}
	if resident == "none" {
		e.Settings.ResidentRoomsForMeasurement = []string{}
	} else if resident != "" && resident != "all" {
		selected := strings.Split(resident, ",")
		if err := validateResidentRooms(selected, roomSet); err != nil {
			return err
		}
		e.Settings.ResidentRoomsForMeasurement = selected
	}
	tape := &rerankTape{Mode: rrMode, Entries: map[string]search.RerankResult{}, Provider: provider, SpendCap: recordSpendCap}
	var rr search.Reranker = disabledRerank{}
	if rrMode != "off" {
		if rrMode == "record" && (provider == nil || !provider.Available(0.05)) {
			return errors.New("record requires a configured rerank provider")
		}
		if err := scratchPath(tapePath); err != nil {
			return err
		}
		if raw, err := os.ReadFile(tapePath); err == nil {
			if err := json.Unmarshal(raw, &tape.Entries); err != nil {
				return errors.New("invalid tape")
			}
			if tape.Entries == nil {
				return errors.New("invalid tape")
			}
		} else if rrMode == "replay" || !os.IsNotExist(err) {
			return errors.New("tape unavailable")
		}
		rr = tape
		if rrMode == "record" {
			// a failed or capped run still keeps what it paid for
			defer func() {
				if len(tape.Entries) > 0 {
					_ = writeGateJSON(tapePath, tape.Entries)
				}
			}()
		}
	}
	pass := func(outPath string) error {
		counts, err := e.Reload()
		if err != nil {
			return errors.New("measurement reload failed")
		}
		ix := e.Index()
		qs := loadGold(gold)
		var rows []candidateRow
		for _, q := range qs {
			qv, err := emb.EmbedQuery(context.Background(), q.Query)
			if err != nil {
				return fmt.Errorf("question %s: cached query vector unavailable", q.ID)
			}
			scopeRooms := q.Rooms
			if scopeRooms == nil && q.Room != "" {
				scopeRooms = []string{q.Room}
			}
			for _, name := range modes {
				mode, ok := retrieval.Normalize(name)
				if !ok {
					return errors.New("invalid ranking mode")
				}
				row := candidateRow{ID: q.ID, Mode: name, ScopeKind: "rooms"}
				if q.All {
					row.ScopeKind = "all"
				} else if len(scopeRooms) == 0 {
					row.ScopeKind = "empty"
				} else {
					found := false
					for _, r := range scopeRooms {
						found = found || roomSet[r]
					}
					if !found {
						row.ScopeKind = "nonexistent"
					}
				}
				ref := &search.TimeReference{Mode: "eval", Now: search.Moment{T: time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC), Aware: true}, TZ: time.UTC}
				if q.AnchorTS != "" {
					t, err := time.Parse(time.RFC3339, q.AnchorTS)
					if err != nil {
						return fmt.Errorf("question %s: invalid anchor", q.ID)
					}
					ref.Now.T = t
				}
				res, err := ix.Retrieve(context.Background(), search.Request{Query: q.Query, QueryVector: qv, Intent: q.Intent, Scope: search.Scope{Rooms: scopeRooms, All: q.All}, Ranking: mode, TopK: 5, RemainingUSD: 0.05, TimeReference: ref,
					OnCandidatesForMeasurement: func(m search.CandidateMeasurement) { row.Stages = append(row.Stages, m) }}, emb, rr)
				if err != nil {
					return fmt.Errorf("question %s: retrieval failed", q.ID)
				}
				if tape.Missing > 0 {
					return fmt.Errorf("question %s: rerank tape miss; record the union of paired requests first", q.ID)
				}
				if tape.Failures > 0 {
					return fmt.Errorf("question %s: rerank recording/replay failed", q.ID)
				}
				row.State = res.State
				row.Recovery, _ = res.Trace["recovery_performed"].(bool)
				row.Confidence = res.Trace["auto_confidence"]
				row.AutoReason = res.Trace["ranking_auto_reason"]
				row.Applied = res.Trace["applied"]
				row.RerankError = res.Trace["rerank_error"]
				want := map[string]bool{}
				for _, id := range q.Want {
					want[id] = true
				}
				for _, h := range res.Hits {
					row.Hits = append(row.Hits, gateHit{h.Rank, h.DocID, h.ObservationIDs, h.VisibleObservationIDs, h.MergedLeafIDs, h.LeafObservationIDs, h.Sources, h.RerankScore, h.DenseCos})
					ids := h.LeafObservationIDs
					if len(ids) == 0 {
						ids = h.ObservationIDs
					}
					for _, id := range ids {
						row.Correct = row.Correct || want[id]
					}
					for _, merged := range h.MergedLeafIDs {
						if leaf, ok := ix.Leaf(merged); ok {
							for _, id := range leaf.ObservationIDs {
								row.Correct = row.Correct || want[id]
							}
						}
					}
				}
				if q.Abstain {
					row.Correct = len(res.Hits) == 0
				}
				rows = append(rows, row)
			}
		}
		if rrMode == "record" {
			if err := writeGateJSON(tapePath, tape.Entries); err != nil {
				return err
			}
		}
		if err := os.MkdirAll(filepath.Dir(outPath), 0700); err != nil {
			return err
		}
		f, err := os.OpenFile(outPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		enc := json.NewEncoder(f)
		for _, row := range rows {
			if err := enc.Encode(row); err != nil {
				f.Close()
				return err
			}
		}
		if err := f.Close(); err != nil {
			return err
		}
		fmt.Printf("dump rows=%d rooms=%d leaves=%d messages=%d identity_rows=%d identity_prepare_us=%d rerank_calls=%d tape_entries=%d rerank_spent_usd=%.4f\n", len(rows), counts["rooms"], counts["leaves"], counts["messages"], counts["identity_input_rows"], counts["identity_prepare_us"], tape.Calls, len(tape.Entries), tape.Spent)
		return nil
	}
	if len(residentSequence) == 0 {
		return pass(out)
	}
	// an in-process sequence: each step rebuilds from the previous one, the way the product reuses rows
	for i, set := range residentSequence {
		if err := validateResidentRooms(set, roomSet); err != nil {
			return err
		}
		e.Settings.ResidentRoomsForMeasurement = set
		step, err := residentStepPath(out, i)
		if err != nil {
			return err
		}
		if err := pass(step); err != nil {
			return err
		}
	}
	return nil
}

func validateResidentRooms(selected []string, existing map[string]bool) error {
	missing := map[string]bool{}
	for _, room := range selected {
		if !existing[room] {
			missing[room] = true
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%d resident room(s) not found", len(missing))
	}
	return nil
}

type candidateChange struct {
	ID            string            `json:"question_id"`
	Mode          string            `json:"mode"`
	BeforeCorrect bool              `json:"before_correct"`
	AfterCorrect  bool              `json:"after_correct"`
	Entered       int               `json:"entered"`
	Exited        int               `json:"exited"`
	RankChanges   map[string][2]int `json:"rank_changes"`
	Changed       bool              `json:"changed"`
	Stages        []stageChange     `json:"stages"`
	Before        candidateRow      `json:"before"`
	After         candidateRow      `json:"after"`
}

type stageChange struct {
	Stage           string                       `json:"stage"`
	Entered         int                          `json:"entered"`
	Exited          int                          `json:"exited"`
	RankChanges     map[string][2]int            `json:"rank_changes"`
	PoolRankChanges map[string][2]int            `json:"pool_rank_changes"`
	RunRankChanges  map[string]map[string][2]int `json:"run_rank_changes"`
}

func rankDifferences(before, after []string) (map[string][2]int, int, int) {
	a, b := map[string]int{}, map[string]int{}
	for i, id := range before {
		a[id] = i + 1
	}
	for i, id := range after {
		b[id] = i + 1
	}
	out := map[string][2]int{}
	entered, exited := 0, 0
	for id, rank := range a {
		if rank != b[id] {
			out[id] = [2]int{rank, b[id]}
		}
		if b[id] == 0 {
			exited++
		}
	}
	for id, rank := range b {
		if a[id] == 0 {
			out[id] = [2]int{0, rank}
			entered++
		}
	}
	return out, entered, exited
}

func compareStages(a, b candidateRow) []stageChange {
	left, right := map[string]search.CandidateMeasurement{}, map[string]search.CandidateMeasurement{}
	for _, s := range a.Stages {
		left[s.Stage] = s
	}
	for _, s := range b.Stages {
		right[s.Stage] = s
	}
	var out []stageChange
	for _, name := range []string{"initial", "recovery"} {
		x, xok := left[name]
		y, yok := right[name]
		if !xok && !yok {
			continue
		}
		var xi, yi []string
		for _, c := range x.Candidates {
			xi = append(xi, c.DocID)
		}
		for _, c := range y.Candidates {
			yi = append(yi, c.DocID)
		}
		ranks, entered, exited := rankDifferences(xi, yi)
		pools, _, _ := rankDifferences(x.Pool, y.Pool)
		runs := map[string]map[string][2]int{}
		for k, ids := range x.Runs {
			diffs, _, _ := rankDifferences(ids, y.Runs[k])
			if len(diffs) > 0 {
				runs[k] = diffs
			}
		}
		for k, ids := range y.Runs {
			if _, ok := x.Runs[k]; !ok {
				diffs, _, _ := rankDifferences(nil, ids)
				runs[k] = diffs
			}
		}
		out = append(out, stageChange{name, entered, exited, ranks, pools, runs})
	}
	return out
}

func readCandidateRows(path string) (map[string]candidateRow, error) {
	if err := scratchPath(path); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]candidateRow{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for sc.Scan() {
		var r candidateRow
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			return nil, errors.New("invalid candidate dump")
		}
		key := r.ID + "/" + r.Mode
		if _, ok := out[key]; ok {
			return nil, errors.New("duplicate question/mode")
		}
		out[key] = r
	}
	return out, sc.Err()
}

func compareCandidates(left, right, out string, failOnRegress bool) error {
	l, err := readCandidateRows(left)
	if err != nil {
		return err
	}
	r, err := readCandidateRows(right)
	if err != nil {
		return err
	}
	if len(l) != len(r) {
		return errors.New("question/mode coverage differs")
	}
	keys := make([]string, 0, len(l))
	for k := range l {
		if _, ok := r[k]; !ok {
			return errors.New("question/mode coverage differs")
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var changes []candidateChange
	changed, regressed, improved := 0, 0, 0
	ranks := func(row candidateRow) map[string]int {
		m := map[string]int{}
		if len(row.Stages) > 0 {
			for i, c := range row.Stages[len(row.Stages)-1].Candidates {
				m[c.DocID] = i + 1
			}
		}
		return m
	}
	for _, key := range keys {
		a, b := l[key], r[key]
		ar, br := ranks(a), ranks(b)
		c := candidateChange{ID: a.ID, Mode: a.Mode, BeforeCorrect: a.Correct, AfterCorrect: b.Correct, RankChanges: map[string][2]int{}, Changed: !reflect.DeepEqual(a, b)}
		c.Stages = compareStages(a, b)
		c.Before = a
		c.After = b
		for id, rank := range ar {
			if nr, ok := br[id]; !ok {
				c.Exited++
			} else if rank != nr {
				c.RankChanges[id] = [2]int{rank, nr}
			}
		}
		for id := range br {
			if _, ok := ar[id]; !ok {
				c.Entered++
			}
		}
		if c.Changed {
			changed++
			fmt.Printf("question=%s mode=%s correct=%t->%t entered=%d exited=%d rank_changes=%d\n", c.ID, c.Mode, c.BeforeCorrect, c.AfterCorrect, c.Entered, c.Exited, len(c.RankChanges))
		}
		if a.Correct && !b.Correct {
			regressed++
		}
		if !a.Correct && b.Correct {
			improved++
		}
		changes = append(changes, c)
	}
	if out != "" {
		if err := writeGateJSON(out, changes); err != nil {
			return err
		}
	}
	fmt.Printf("compare rows=%d changed=%d regressed=%d improved=%d\n", len(keys), changed, regressed, improved)
	if failOnRegress && regressed > 0 {
		return errors.New("regressions detected")
	}
	return nil
}
