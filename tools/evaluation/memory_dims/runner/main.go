// Memory evaluation runner (2026-10-11, dims/message-vector knobs and -msgdims 2026-10-13): drives the Go product (application.Service + memory engine)
// on a fresh data root. Never prints message bodies; outputs ids, ranks and aggregate counters only.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/memory/engine"
	"masamong/internal/memory/retrieval"
	"masamong/internal/memory/search"
)

var keyFile = func() string {
	if p := os.Getenv("EVAL_KEYFILE"); p != "" {
		return p
	}
	return ".private/test-keys/openrouter"
}()

func die(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...); os.Exit(1) }

func open(root string) *application.Service {
	key, err := os.ReadFile(keyFile)
	if err != nil {
		die("key: %v", err)
	}
	os.Setenv("MASAMONG_OPENROUTER_KEY", strings.TrimSpace(string(key)))
	if err := os.MkdirAll(root, 0o700); err != nil {
		die("mkdir: %v", err)
	}
	svc, err := application.Open(root, nil, false)
	if err != nil {
		die("open: %v", err)
	}
	return svc
}

func run(svc *application.Service, act string, in map[string]any) (map[string]any, error) {
	raw, _ := json.Marshal(in)
	r := svc.Execute(context.Background(), action.Request{Action: act, Input: raw, Locale: "ko", RequestID: "eval"})
	if !r.OK {
		b, _ := json.Marshal(r.Error)
		return nil, fmt.Errorf("%s: %s", act, b)
	}
	b, _ := json.Marshal(r.Output)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m, nil
}

func must(m map[string]any, err error) map[string]any {
	if err != nil {
		die("%v", err)
	}
	return m
}

type question struct {
	ID       string        `json:"question_id"`
	Query    string        `json:"query"`
	Type     string        `json:"question_type"`
	Abstain  bool          `json:"expected_abstention"`
	Want     []string      `json:"want_observation_ids"`
	Room     string        `json:"room"`
	Answer   string        `json:"reference_answer"`
	AnchorTS string        `json:"anchor_time"`
	Rooms    []string      `json:"rooms,omitempty"`
	Intent   search.Intent `json:"intent,omitempty"`
	All      bool          `json:"all,omitempty"`
}

func loadGold(path string) []question {
	raw, err := os.ReadFile(path)
	if err != nil {
		die("gold: %v", err)
	}
	var g struct {
		Questions []question `json:"questions"`
	}
	if err := json.Unmarshal(raw, &g); err != nil {
		die("gold: %v", err)
	}
	return g.Questions
}

func main() {
	if len(os.Args) < 2 {
		die("usage: runner setup|index|search|chat|status|keep|load|synthetic-root|dump-candidates|compare-candidates|measure-reload|measure-rooms ...")
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	root := fs.String("root", "", "data root")
	rawPath := fs.String("raw", "", "raw corpus file")
	rooms := fs.String("rooms", "", "comma rooms")
	budget := fs.String("budget", "0.30", "index budget usd")
	gold := fs.String("gold", "", "gold json")
	outPath := fs.String("out", "", "output jsonl")
	modes := fs.String("modes", "hybrid,rerank,auto", "ranking modes")
	workers := fs.Int("workers", 4, "concurrency")
	ids := fs.String("ids", "", "comma question ids (chat); empty = all")
	ranking := fs.String("ranking", "", "chat ranking override")
	dims := fs.Int("dims", 0, "set models.embed.dimensions first (0 = leave as is)")
	msgvec := fs.Bool("msgvec", false, "memory.message_vectors on (search uses stored message vectors; index builds them)")
	msgdims := fs.Int("msgdims", 0, "measurement-only message-vector width (1..1024; 0 = product rule: MRL cap 512, full native width otherwise)")
	resident := fs.String("resident", "", "measurement residency: all, none, or comma rooms; independent gate build only")
	residentFile := fs.String("resident-file", "", "scratch JSON array of resident rooms (keeps private IDs out of command lines)")
	synthetic := fs.Bool("synthetic", false, "use synthetic gate root and fake providers (no API keys)")
	tape := fs.String("tape", "", "rerank response tape under .masamong-scratch/")
	rrMode := fs.String("rerank-mode", "off", "off|record|replay; missing replay requests fail the gate")
	left := fs.String("left", "", "baseline candidate dump")
	right := fs.String("right", "", "comparison candidate dump")
	failOnRegress := fs.Bool("fail-on-regress", false, "exit non-zero when comparison finds regressions")
	profDir := fs.String("prof", "", "measure-reload: write allocation profiles of the hot rebuild under this scratch directory")
	settle := fs.Bool("settle", false, "measure-reload: wait out the replaced index's close delay and read the heap again")
	rank := fs.Int("rank", 1, "measure-rooms: target room's rank by message count (1 = largest)")
	restRanks := fs.String("rest-ranks", "", "measure-rooms: ranks of the rooms already resident (empty = every other room)")
	reps := fs.Int("reps", 3, "measure-rooms: repetitions")
	residentSeqFile := fs.String("resident-seq-file", "", "dump-candidates: scratch JSON array of resident-room arrays, run in order in one process")
	maxSpend := fs.Float64("max-spend", 0, "dump-candidates -rerank-mode record: stop recording after this much provider cost (USD)")
	_ = fs.Parse(os.Args[2:])
	setDims, setMsgvec, setMsgdims = *dims, *msgvec, *msgdims
	recordSpendCap = *maxSpend
	switch os.Args[1] {
	case "synthetic-root":
		if err := syntheticRoot(*root); err != nil {
			die("synthetic-root: %v", err)
		}
	case "dump-candidates":
		if *residentSeqFile != "" {
			if *residentFile != "" || *resident != "" {
				die("use resident-seq-file alone")
			}
			if err := scratchPath(*residentSeqFile); err != nil {
				die("resident-seq-file: %v", err)
			}
			raw, err := os.ReadFile(*residentSeqFile)
			if err != nil {
				die("resident-seq-file unavailable")
			}
			if err := json.Unmarshal(raw, &residentSequence); err != nil || len(residentSequence) == 0 {
				die("resident-seq-file must be a non-empty JSON array of arrays")
			}
		}
		if *residentFile != "" {
			if *resident != "" {
				die("use resident or resident-file")
			}
			if err := scratchPath(*residentFile); err != nil {
				die("resident-file: %v", err)
			}
			raw, err := os.ReadFile(*residentFile)
			if err != nil {
				die("resident-file unavailable")
			}
			var selected []string
			if err := json.Unmarshal(raw, &selected); err != nil {
				die("resident-file must be a JSON array")
			}
			*resident = strings.Join(selected, ",")
			if len(selected) == 0 {
				*resident = "none"
			} else if *resident == "" {
				*resident = ","
			}
		}
		if err := dumpCandidates(*root, *gold, *outPath, *resident, *tape, *rrMode, *synthetic, strings.Split(*modes, ",")); err != nil {
			die("dump-candidates: %v", err)
		}
	case "measure-reload":
		if err := measureReload(*root, *outPath, *profDir, *settle); err != nil {
			die("measure-reload: %v", err)
		}
	case "measure-rooms":
		if err := measureRooms(*root, *outPath, *rank, *restRanks, *reps); err != nil {
			die("measure-rooms: %v", err)
		}
	case "compare-candidates":
		if err := compareCandidates(*left, *right, *outPath, *failOnRegress); err != nil {
			die("compare-candidates: %v", err)
		}
	case "setup":
		setup(*root, *rawPath, *rooms, *budget)
	case "index":
		indexRoot(*root, *budget)
	case "search":
		searchEval(*root, *gold, *outPath, strings.Split(*modes, ","), *workers)
	case "chat":
		chatEval(*root, *gold, *outPath, *ids, *ranking, *workers)
	case "status":
		svc := open(*root)
		defer svc.Close()
		m := must(run(svc, "memory.blocks.list", nil))
		b, _ := json.MarshalIndent(m, "", " ")
		fmt.Println(string(b))
	case "keep":
		keepRooms(*root, strings.Split(*rooms, ","))
	case "load":
		loadOnly(*root)
	default:
		die("unknown command")
	}
}

// keepRooms deletes every block of a data root COPY except the given source rooms (memory.blocks.delete,
// the product's own path), so a search over the copy sees only those rooms' documents and statistics.
// Prints counts only.
func keepRooms(root string, keep []string) {
	svc := open(root)
	defer svc.Close()
	want := map[string]bool{}
	for _, r := range keep {
		want[r] = true
	}
	m := must(run(svc, "memory.blocks.list", nil))
	blocks, _ := m["blocks"].([]any)
	var deleted, removed, kept int
	for _, b := range blocks {
		bm, _ := b.(map[string]any)
		room := fmt.Sprint(bm["source_room"])
		if want[room] {
			kept++
			continue
		}
		out := must(run(svc, "memory.blocks.delete", map[string]any{"source_room": room, "confirm": fmt.Sprint(bm["label"])}))
		deleted++
		if n, ok := out["messages_removed"].(float64); ok {
			removed += int(n)
		}
	}
	fmt.Printf("kept=%d deleted=%d messages_removed=%d\n", kept, deleted, removed)
}

// loadOnly measures one cold index load (a fresh process): wall time of EnsureLoaded and the Go heap
// after it. Run it with GOMAXPROCS=1 to approximate a one-core box.
func loadOnly(root string) {
	svc := open(root)
	defer svc.Close()
	applyKnobs(svc)
	eng, err := svc.MemoryEngine()
	if err != nil {
		die("engine: %v", err)
	}
	t0 := time.Now()
	if err := eng.EnsureLoaded(); err != nil {
		die("load: %v", err)
	}
	took := time.Since(t0)
	ix := eng.Index()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	fmt.Printf("load rooms=%d leaves=%d messages=%d message_vectors=%d seconds=%.2f heap_mb=%.1f gomaxprocs=%d\n", len(ix.RoomSigs),
		len(ix.Leaves), len(ix.Messages), len(ix.MessageRows), took.Seconds(), float64(ms.HeapAlloc)/1e6, runtime.GOMAXPROCS(0))
}

func setup(root, rawPath, rooms, budget string) {
	svc := open(root)
	defer svc.Close()
	if svc.Revision() == 0 {
		must(run(svc, "setup.resume", map[string]any{"instance": "eval"}))
	}
	must(run(svc, "config.patch", map[string]any{"ops": []any{
		map[string]any{"path": []any{"memory", "summaries"}, "value": false},
		map[string]any{"path": []any{"memory", "index_budget_usd"}, "value": budget},
		map[string]any{"path": []any{"i18n", "default_locale"}, "value": "ko"},
	}}))
	if _, err := run(svc, "access.policy", map[string]any{"server_approval": false}); err != nil {
		fmt.Println("access.policy:", err)
	}
	t0 := time.Now()
	imp := must(run(svc, "imports.start", map[string]any{"source": "raw", "path": rawPath, "rooms": strings.Split(rooms, ",")}))
	b, _ := json.Marshal(imp["result"])
	fmt.Printf("import %s in %s\n", b, time.Since(t0).Round(time.Second))
	t0 = time.Now()
	rep := must(run(svc, "memory.reindex", map[string]any{"rooms": strings.Split(rooms, ","), "budget_usd": budget}))
	b, _ = json.Marshal(rep["report"])
	fmt.Printf("reindex %s in %s\n", b, time.Since(t0).Round(time.Second))
	eng, err := svc.MemoryEngine()
	if err != nil {
		die("engine: %v", err)
	}
	counts, err := eng.Reload()
	if err != nil {
		die("reload: %v", err)
	}
	fmt.Printf("reload %v\n", counts)
}

type modeRow struct {
	Rank        int      `json:"rank"`
	NHits       int      `json:"n_hits"`
	PoolHit     bool     `json:"pool_hit"`
	PoolN       int      `json:"pool_n"`
	FusionHit   bool     `json:"fusion_hit"`
	Applied     any      `json:"applied"`
	AutoReason  any      `json:"auto_reason"`
	Abstained   bool     `json:"abstained"`
	State       string   `json:"rag_state"`
	RerankCost  float64  `json:"rerank_cost_usd"`
	RerankErr   any      `json:"rerank_error,omitempty"`
	Err         string   `json:"error,omitempty"`
	MS          int64    `json:"ms"`
	TopDocs     []string `json:"top_docs"`
	VisibleHit  bool     `json:"visible_hit"`
	RecentApply any      `json:"recent_window_applied,omitempty"`
}

type row struct {
	ID       string             `json:"question_id"`
	Type     string             `json:"question_type"`
	Abstain  bool               `json:"expected_abstention"`
	Room     string             `json:"room"`
	WantLeaf []string           `json:"want_leaf"`
	Missing  bool               `json:"anchor_missing"`
	Modes    map[string]modeRow `json:"modes"`
}

func strs(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			out = append(out, fmt.Sprint(e))
		}
		return out
	}
	return nil
}

var (
	setDims    int
	setMsgvec  bool
	setMsgdims int
)

// applyKnobs sets the embedding width and the message-vector switch through the product's own
// config.patch (the data root keeps its config in the database; config.yaml is not read back).
// A lower width than the stored vectors is applied by the engine's adaptSpace (MRL truncation,
// no API call); a higher one would need a re-embedding.
func applyKnobs(svc *application.Service) {
	ops := []any{map[string]any{"path": []any{"memory", "message_vectors"}, "value": setMsgvec}}
	if setDims > 0 {
		ops = append(ops, map[string]any{"path": []any{"models", "embed", "dimensions"}, "value": setDims})
	}
	if _, err := run(svc, "config.patch", map[string]any{"ops": ops}); err != nil && !strings.Contains(err.Error(), "unchanged") {
		die("patch: %v", err)
	}
	// the message width is a measurement-only engine knob, not config: set it on the engine the actions
	// below use (the engine is rebuilt only by a config change, and none follows)
	eng, err := svc.MemoryEngine()
	if err != nil {
		die("engine: %v", err)
	}
	eng.Settings.MessageDims = setMsgdims
	fmt.Printf("dims=%d message_vectors=%v message_space=%s\n", svc.Doc().Models["embed"].Dimensions, setMsgvec, eng.MessageSpace())
}

// indexRoot runs one product index pass (memory.reindex) under a budget, e.g. to build message
// vectors after -msgvec. Prints the report counters only.
func indexRoot(root, budget string) {
	svc := open(root)
	defer svc.Close()
	applyKnobs(svc)
	out := must(run(svc, "memory.reindex", map[string]any{"budget_usd": budget}))
	b, _ := json.Marshal(out["report"])
	fmt.Printf("report %s\n", b)
}

func searchEval(root, goldPath, outPath string, modes []string, workers int) {
	svc := open(root)
	defer svc.Close()
	applyKnobs(svc)
	eng, err := svc.MemoryEngine()
	if err != nil {
		die("engine: %v", err)
	}
	if err := eng.EnsureLoaded(); err != nil {
		die("load: %v", err)
	}
	ix := eng.Index()
	leafObs := map[string][]string{}
	obsLeaf := map[string]string{}
	for _, l := range ix.Leaves {
		leafObs[l.DocID] = l.ObservationIDs
		for _, o := range l.ObservationIDs {
			if _, ok := obsLeaf[o]; !ok {
				obsLeaf[o] = l.DocID
			}
		}
	}
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	fmt.Printf("index leaves=%d messages=%d message_vectors=%d message_dims=%d message_space=%s heap_mb=%.1f\n", len(ix.Leaves), len(ix.Messages),
		len(ix.MessageRows), ix.MsgVecs.Dims, ix.MessageSpace, float64(ms.HeapAlloc)/1e6)
	qs := loadGold(goldPath)
	rows := make([]row, len(qs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, workers)
	for i, q := range qs {
		wg.Add(1)
		go func(i int, q question) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			want := map[string]bool{}
			wantLeaf := map[string]bool{}
			r := row{ID: q.ID, Type: q.Type, Abstain: q.Abstain, Room: q.Room, Modes: map[string]modeRow{}}
			for _, o := range q.Want {
				want[o] = true
				if l, ok := obsLeaf[o]; ok {
					wantLeaf[l] = true
				}
			}
			for l := range wantLeaf {
				r.WantLeaf = append(r.WantLeaf, l)
			}
			r.Missing = !q.Abstain && len(wantLeaf) == 0
			for _, m := range modes {
				mode, ok := retrieval.Normalize(m)
				if !ok {
					die("mode %s", m)
				}
				t0 := time.Now()
				res, err := eng.Search(context.Background(), engine.SearchRequest{Query: q.Query,
					Intent: search.Intent{Kind: "other", Source: "none"}, Rooms: []string{q.Room}, Ranking: mode,
					TopK: 5, RemainingUSD: 0.05})
				mr := modeRow{MS: time.Since(t0).Milliseconds()}
				if err != nil {
					mr.Err = err.Error()
					r.Modes[m] = mr
					continue
				}
				mr.NHits = len(res.Hits)
				mr.State = string(res.State)
				for k, h := range res.Hits {
					mr.TopDocs = append(mr.TopDocs, h.DocID)
					obs := map[string]bool{}
					for _, o := range h.LeafObservationIDs {
						obs[o] = true
					}
					if len(h.LeafObservationIDs) == 0 {
						for _, o := range h.ObservationIDs {
							obs[o] = true
						}
					}
					for _, ml := range h.MergedLeafIDs {
						for _, o := range leafObs[ml] {
							obs[o] = true
						}
					}
					if mr.Rank == 0 {
						for o := range want {
							if obs[o] {
								mr.Rank = k + 1
								break
							}
						}
					}
					for _, o := range h.VisibleObservationIDs {
						if want[o] {
							mr.VisibleHit = true
						}
					}
				}
				tr := res.Trace
				for _, id := range strs(tr["candidate_pool_ids"]) {
					mr.PoolN++
					if wantLeaf[id] {
						mr.PoolHit = true
					}
				}
				for _, id := range strs(tr["fusion_ids"]) {
					if wantLeaf[id] {
						mr.FusionHit = true
					}
				}
				mr.Applied = tr["applied"]
				mr.AutoReason = tr["ranking_auto_reason"]
				mr.Abstained, _ = tr["abstained"].(bool)
				if c, ok := tr["rerank_cost_usd"].(float64); ok {
					mr.RerankCost = c
				}
				if e := tr["rerank_error"]; e != nil && fmt.Sprint(e) != "" {
					mr.RerankErr = e
				}
				mr.RecentApply = tr["recent_window_applied"]
				r.Modes[m] = mr
			}
			rows[i] = r
		}(i, q)
	}
	wg.Wait()
	f, err := os.Create(outPath)
	if err != nil {
		die("out: %v", err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, r := range rows {
		_ = enc.Encode(r)
	}
	fmt.Printf("wrote %d rows\n", len(rows))
}

// chatEval runs chat.respond (dry run, detail) for questions. Discord rooms ask in their own channel;
// any other room (Kakao) is attached as a block to a scratch channel (guild 900, channel 901).
type chatRow struct {
	ID       string           `json:"question_id"`
	Abstain  bool             `json:"expected_abstention"`
	Rank     int              `json:"evidence_rank"`
	NEv      int              `json:"n_evidence"`
	RagState any              `json:"rag_state"`
	Ranking  any              `json:"retrieval"`
	Intent   any              `json:"intent"`
	Cost     any              `json:"cost_usd"`
	Answer   string           `json:"answer"` // stays in the scratch file only (judge input); never printed
	Evidence []map[string]any `json:"evidence"`
	Err      string           `json:"error,omitempty"`
	MS       int64            `json:"ms"`
}

func chatEval(root, goldPath, outPath, ids, ranking string, workers int) {
	svc := open(root)
	defer svc.Close()
	qs := loadGold(goldPath)
	pick := map[string]bool{}
	for _, id := range strings.Split(ids, ",") {
		if id != "" {
			pick[id] = true
		}
	}
	blocksOut := must(run(svc, "memory.blocks.list", nil))
	blockFor := map[string]string{}
	for _, b := range blocksOut["blocks"].([]any) {
		bm := b.(map[string]any)
		blockFor[fmt.Sprint(bm["source_room"])] = fmt.Sprint(bm["block_id"])
	}
	attached := map[string]bool{}
	var sel []question
	for _, q := range qs {
		if len(pick) > 0 && !pick[q.ID] {
			continue
		}
		sel = append(sel, q)
		if !strings.HasPrefix(q.Room, "discord:") && !attached[q.Room] {
			bid := blockFor[q.Room]
			if bid == "" {
				die("no block for room")
			}
			for k := 1; k <= 4; k++ {
				ch := fmt.Sprint(900 + k)
				if _, err := run(svc, "channels.configure", map[string]any{"guild_id": "900", "channel_id": ch, "memory_use": "on", "respond": "always"}); err != nil {
					fmt.Println("configure:", err)
				}
				if _, err := run(svc, "channels.view.attach", map[string]any{"guild_id": "900", "channel_id": ch, "block_id": bid}); err != nil && !strings.Contains(err.Error(), "already") {
					die("attach: %v", err)
				}
			}
			attached[q.Room] = true
		}
	}
	rows := make([]chatRow, len(sel))
	var wg sync.WaitGroup
	sem := make(chan struct{}, workers)
	for i, q := range sel {
		wg.Add(1)
		go func(i int, q question) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			g, c := "900", fmt.Sprint(901+i%4)
			if strings.HasPrefix(q.Room, "discord:") {
				p := strings.Split(q.Room, ":")
				g, c = p[1], p[2]
			}
			in := map[string]any{"guild_id": g, "channel_id": c, "message": q.Query, "detail": true, "speaker": "tester"}
			if ranking != "" {
				in["ranking"] = ranking
			}
			t0 := time.Now()
			out, err := run(svc, "chat.respond", in)
			for try := 0; try < 5 && err != nil && strings.Contains(err.Error(), "too many replies waiting"); try++ {
				time.Sleep(20 * time.Second)
				out, err = run(svc, "chat.respond", in)
			}
			r := chatRow{ID: q.ID, Abstain: q.Abstain, MS: time.Since(t0).Milliseconds()}
			if err != nil {
				r.Err = err.Error()
				rows[i] = r
				return
			}
			r.RagState, r.Ranking, r.Intent, r.Cost = out["rag_state"], out["retrieval"], out["intent"], out["cost_usd"]
			r.Answer = fmt.Sprint(out["answer"])
			want := map[string]bool{}
			for _, o := range q.Want {
				want[o] = true
			}
			if ev, ok := out["evidence"].([]any); ok {
				r.NEv = len(ev)
				for k, e := range ev {
					em := e.(map[string]any)
					obs := append(strs(em["leaf_observation_ids"]), strs(em["observation_ids"])...)
					hit := false
					for _, o := range obs {
						if want[o] {
							hit = true
						}
					}
					if hit && r.Rank == 0 {
						r.Rank = k + 1
					}
					r.Evidence = append(r.Evidence, map[string]any{"doc_id": em["doc_id"], "date": em["date"], "excerpt": em["excerpt"], "rerank_score": em["rerank_score"]})
				}
			}
			rows[i] = r
		}(i, q)
	}
	wg.Wait()
	f, err := os.Create(outPath)
	if err != nil {
		die("out: %v", err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	sort.SliceStable(rows, func(a, b int) bool { return rows[a].ID < rows[b].ID })
	for _, r := range rows {
		_ = enc.Encode(r)
	}
	fmt.Printf("wrote %d chat rows\n", len(rows))
}
