package engine

import (
	"context"
	"encoding/json"
	"errors"
	"hash/fnv"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/errclass"
	"masamong/internal/memory/corpus"
	"masamong/internal/memory/leaves"
	"masamong/internal/memory/retrieval"
	"masamong/internal/memory/search"
	"masamong/internal/memory/store"
	"masamong/internal/observability"
	"masamong/internal/providers"
	"masamong/internal/providers/openai"
	"masamong/internal/storage"
)

// fakeProvider serves /embeddings (deterministic bag-of-bigram vectors), /rerank and /chat/completions.
type fakeProvider struct {
	embedCalls, rerankCalls, chatCalls atomic.Int32
	embedDelay                         atomic.Int64 // nanoseconds every /embeddings call takes (0 = none)
	embedStatus                        atomic.Int32 // 0 = success; otherwise return this HTTP status
	chatStatus                         atomic.Int32 // 0 = success; otherwise /chat/completions returns this HTTP status
}

func vecFor(text string) []float32 {
	v := make([]float32, 16)
	r := []rune(text)
	for i := 0; i+1 < len(r); i++ {
		h := fnv.New32a()
		h.Write([]byte(string(r[i : i+2])))
		v[h.Sum32()%16] += 1
	}
	var n float64
	for _, x := range v {
		n += float64(x * x)
	}
	for i := range v {
		v[i] = float32(float64(v[i]) / (math.Sqrt(n) + 1e-9))
	}
	return v
}

func (f *fakeProvider) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/embeddings", func(w http.ResponseWriter, r *http.Request) {
		f.embedCalls.Add(1)
		if status := f.embedStatus.Load(); status != 0 {
			http.Error(w, `{"error":{"message":"synthetic failure"}}`, int(status))
			return
		}
		if d := f.embedDelay.Load(); d > 0 {
			time.Sleep(time.Duration(d))
		}
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var data []map[string]any
		for i, t := range req.Input {
			data = append(data, map[string]any{"index": i, "embedding": vecFor(t)})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "usage": map[string]any{"prompt_tokens": 7, "cost": 0.000001}})
	})
	mux.HandleFunc("/rerank", func(w http.ResponseWriter, r *http.Request) {
		f.rerankCalls.Add(1)
		var req struct {
			Documents []string `json:"documents"`
			TopN      int      `json:"top_n"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var res []map[string]any
		for i := 0; i < req.TopN; i++ {
			res = append(res, map[string]any{"index": i, "relevance_score": 0.9 - 0.1*float64(i)})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": res, "model": "fake-rerank", "usage": map[string]any{"total_tokens": 9, "cost": 0.0001}})
	})
	mux.HandleFunc("/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		f.chatCalls.Add(1)
		if status := f.chatStatus.Load(); status != 0 {
			http.Error(w, `{"error":{"message":"synthetic failure"}}`, int(status))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]any{"content": "요약: 약속 잡은 대화"}}},
			"usage": map[string]any{"prompt_tokens": 21, "completion_tokens": 4, "cost": 0.00005,
				"prompt_tokens_details": map[string]any{"cached_tokens": 2}}})
	})
	return mux
}

func newEngine(t *testing.T, summaries bool) (*Engine, *fakeProvider) {
	t.Helper()
	db, err := storage.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	fp := &fakeProvider{}
	srv := httptest.NewServer(fp.handler())
	t.Cleanup(srv.Close)
	t.Setenv("FAKE_KEY", "k")
	cred, _ := providers.Resolve("env:FAKE_KEY")
	a := openai.New(providers.NewClient(5*time.Second, nil), srv.URL, cred)
	e := New(st, a, Models{Embed: "fake-embed", Rerank: "fake-rerank", Summary: "fake-summary"},
		Settings{Search: search.Default(), Recipe: leaves.DefaultRecipe(), Summaries: summaries, RefTZ: time.UTC, HedgeAfter: time.Second})
	return e, fp
}

func obs(room string, i int, who, text string, at time.Time) corpus.Observation {
	return corpus.Observation{ObservationID: room + "#" + itoa(i), Platform: corpus.PlatformOfRoom(room), Collection: "t",
		SourceRoom: room, ActorLabel: who, ActorAccount: "acct-" + who, Content: text, ParsedTime: at.UTC().Format(time.RFC3339),
		RecordKind: corpus.HumanText}
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

func seed(t *testing.T, e *Engine) {
	t.Helper()
	base := time.Now().Add(-48 * time.Hour)
	var batch []corpus.Observation
	lines := []string{"주말에 캠핑 가자", "어디로 갈까", "가평 어때", "좋아 토요일 출발", "고기는 내가 살게", "텐트 챙겨", "비 오면 어떡해", "그땐 취소하자",
		"점심 뭐 먹지", "마라탕", "또 마라탕이냐", "ㅋㅋㅋ"}
	for i, l := range lines {
		who := []string{"철수", "영희"}[i%2]
		batch = append(batch, obs("discord:1:10", i, who, l, base.Add(time.Duration(i)*time.Minute)))
	}
	batch = append(batch, obs("discord:1:99", 0, "민수", "다른 방 캠핑 얘기", base))
	if _, err := e.Store.InsertObservations(batch, nil); err != nil {
		t.Fatal(err)
	}
}

func TestIndexSearchAndScope(t *testing.T) {
	e, fp := newEngine(t, true)
	seed(t, e)
	ctx := context.Background()
	dry, err := e.IndexRooms(ctx, IndexOptions{DryRun: true}, nil)
	if err != nil || dry.Estimate == nil || dry.Estimate.LeafEmbeds == 0 || fp.embedCalls.Load() != 0 {
		t.Fatalf("dry run must not call the provider: %+v %v calls=%d", dry, err, fp.embedCalls.Load())
	}
	rep, err := e.IndexRooms(ctx, IndexOptions{BudgetUSD: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.LeafVectors == 0 || rep.Summaries == 0 || rep.Reload["leaves"] != rep.LeafVectors || rep.SpentUSD <= 0 {
		t.Fatalf("%+v", rep)
	}
	res, err := e.Search(ctx, SearchRequest{Query: "캠핑 어디로 가", Rooms: []string{"discord:1:10"}, Ranking: retrieval.Hybrid,
		Intent: search.Intent{Kind: "other", Source: "none"}})
	if err != nil || len(res.Hits) == 0 {
		t.Fatalf("%v %+v", err, res)
	}
	for _, h := range res.Hits {
		if h.Room != "discord:1:10" {
			t.Fatalf("scope leak: %s", h.Room)
		}
	}
	// second pass: nothing pending, nothing paid
	calls := fp.embedCalls.Load()
	rep2, _ := e.IndexRooms(ctx, IndexOptions{BudgetUSD: 1}, nil)
	if rep2.Pending != 0 || fp.embedCalls.Load() != calls {
		t.Fatalf("re-index must be free: %+v", rep2)
	}
	// ...and it keeps the loaded index instead of rebuilding the whole history
	if rep2.Reload != nil {
		t.Fatalf("a pass that changed nothing rebuilt the index: %+v", rep2.Reload)
	}
	loaded := e.Index()
	// a new message that closes a leaf does change the index: it is rebuilt
	base := time.Now().Add(-47 * time.Hour)
	var more []corpus.Observation
	for i := 0; i < 8; i++ {
		more = append(more, obs("discord:1:99", 100+i, "민수", "새 캠핑 계획 얘기", base.Add(time.Duration(i)*time.Minute)))
	}
	if _, err := e.Store.InsertObservations(more, nil); err != nil {
		t.Fatal(err)
	}
	rep3, err := e.IndexRooms(ctx, IndexOptions{BudgetUSD: 1}, nil)
	if err != nil || rep3.Reload == nil || e.Index() == loaded {
		t.Fatalf("new leaves must rebuild the index: %v %+v", err, rep3)
	}
}

func TestQueryEmbedCachedAndRerankUsed(t *testing.T) {
	e, fp := newEngine(t, false)
	seed(t, e)
	ctx := context.Background()
	if _, err := e.IndexRooms(ctx, IndexOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	before := fp.embedCalls.Load()
	for i := 0; i < 2; i++ {
		if _, err := e.Search(ctx, SearchRequest{Query: "마라탕 누가 먹재", Rooms: []string{"discord:1:10"}, Ranking: retrieval.Rerank}); err != nil {
			t.Fatal(err)
		}
	}
	if fp.embedCalls.Load()-before != 1 {
		t.Fatalf("query embedding must be cached: %d calls", fp.embedCalls.Load()-before)
	}
	if fp.rerankCalls.Load() != 2 {
		t.Fatalf("rerank calls %d", fp.rerankCalls.Load())
	}
	_, total, _ := e.Store.SpendSince("2000-01-01")
	if total <= 0 {
		t.Fatal("spend not recorded")
	}
}

func TestCallObserverRecordsOnlyProviderRequestsAndPreservesContext(t *testing.T) {
	e, fp := newEngine(t, true)
	e.Settings.MessageVectors = true
	seed(t, e)
	var mu sync.Mutex
	type observed struct {
		ctx  context.Context
		call CallInfo
	}
	var calls []observed
	e.CallProviders = CallProviders{Embed: "embed-host", Rerank: "rerank-host", Summary: "summary-host"}
	e.OnCall = func(ctx context.Context, c CallInfo) {
		mu.Lock()
		calls = append(calls, observed{ctx: ctx, call: c})
		mu.Unlock()
	}
	jobCtx := observability.WithJob(context.Background(), "job-memory-index")
	rep, err := e.IndexRooms(jobCtx, IndexOptions{BudgetUSD: 1}, nil)
	if err != nil || rep.LeafVectors == 0 {
		t.Fatalf("index: %+v %v", rep, err)
	}
	mu.Lock()
	indexed := append([]observed(nil), calls...)
	mu.Unlock()
	if len(indexed) != int(fp.embedCalls.Load()+fp.chatCalls.Load()) {
		t.Fatalf("observed %d calls for %d embed + %d summary provider requests", len(indexed), fp.embedCalls.Load(), fp.chatCalls.Load())
	}
	var embedCalls, summaryCalls int
	for _, item := range indexed {
		if observability.JobID(item.ctx) != "job-memory-index" || observability.TurnID(item.ctx) != "" {
			t.Fatalf("index call lost its job context: %+v", item.call)
		}
		switch item.call.Role {
		case "embed":
			embedCalls++
			if item.call.Provider != "embed-host" || item.call.PromptTokens != 7 || item.call.CostUSD != 0.000001 || item.call.Class != errclass.None {
				t.Fatalf("embedding call: %+v", item.call)
			}
		case "summary":
			summaryCalls++
			if item.call.Provider != "summary-host" || item.call.PromptTokens != 21 || item.call.CompletionTokens != 4 || item.call.CachedTokens != 2 || item.call.CostUSD != 0.00005 || item.call.Class != errclass.None {
				t.Fatalf("summary call: %+v", item.call)
			}
		default:
			t.Fatalf("unexpected memory call role %q", item.call.Role)
		}
	}
	if embedCalls != int(fp.embedCalls.Load()) || summaryCalls != int(fp.chatCalls.Load()) {
		t.Fatalf("roles embed=%d/%d summary=%d/%d", embedCalls, fp.embedCalls.Load(), summaryCalls, fp.chatCalls.Load())
	}
	_, spent, err := e.Store.SpendSince("2000-01-01")
	if err != nil || math.Abs(spent-rep.SpentUSD) > 1e-9 {
		t.Fatalf("call observation changed memory_spend: spent=%f report=%f err=%v", spent, rep.SpentUSD, err)
	}

	mu.Lock()
	calls = nil
	mu.Unlock()
	turnCtx := observability.WithTurn(context.Background(), "turn-memory-search")
	request := SearchRequest{Query: "캠핑 어디로 가", Rooms: []string{"discord:1:10"}, Ranking: retrieval.Rerank,
		Intent: search.Intent{Kind: "other", Source: "none"}}
	if _, err := e.Search(turnCtx, request); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	firstSearchCalls := append([]observed(nil), calls...)
	firstSearchCount := len(calls)
	mu.Unlock()
	if _, err := e.Search(turnCtx, request); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	cacheHitCalls := append([]observed(nil), calls[firstSearchCount:]...)
	failureStart := len(calls)
	mu.Unlock()
	cacheEmbeds, cacheReranks := 0, 0
	for _, item := range cacheHitCalls {
		if item.call.Role == "embed" {
			cacheEmbeds++
		}
		if item.call.Role == "rerank" {
			cacheReranks++
		}
	}
	if cacheEmbeds != 0 || cacheReranks != 1 {
		t.Fatalf("cache-hit search made %d embeddings and %d reranks", cacheEmbeds, cacheReranks)
	}
	firstEmbeds := 0
	for _, item := range firstSearchCalls {
		if observability.TurnID(item.ctx) != "turn-memory-search" || observability.JobID(item.ctx) != "" {
			t.Fatalf("first search call lost its turn context: %+v", item.call)
		}
		if item.call.Role == "embed" {
			firstEmbeds++
		}
	}
	if firstEmbeds < 1 {
		t.Fatalf("first search recorded no embedding request: %+v", firstSearchCalls)
	}
	fp.embedStatus.Store(http.StatusServiceUnavailable)
	if _, err := e.EmbedQuery(turnCtx, "uncached synthetic failure query"); err == nil {
		t.Fatal("expected the synthetic provider failure")
	}
	mu.Lock()
	failureCalls := append([]observed(nil), calls[failureStart:]...)
	mu.Unlock()
	failedEmbeds := 0
	for _, item := range failureCalls {
		if observability.TurnID(item.ctx) != "turn-memory-search" || observability.JobID(item.ctx) != "" {
			t.Fatalf("search call lost its turn context: %+v", item.call)
		}
		if item.call.Role == "embed" {
			if item.call.Class != errclass.Overloaded {
				t.Fatalf("failed provider request has class %q", item.call.Class)
			}
			failedEmbeds++
			if item.call.CostUSD != 0.00001 {
				t.Fatalf("failed query cost fallback %f", item.call.CostUSD)
			}
		}
	}
	if failedEmbeds < 1 {
		t.Fatalf("no failed embedding request was recorded: %+v", failureCalls)
	}
}

func TestCaptureIsIdempotentAndMarksRoom(t *testing.T) {
	e, _ := newEngine(t, false)
	m := LiveMessage{GuildID: "1", ChannelID: "10", MessageID: "555", AuthorID: "u", AuthorName: "철수", Content: "내일 봐", At: time.Now()}
	if ok, err := e.Capture(m); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if ok, _ := e.Capture(m); ok {
		t.Fatal("duplicate capture stored twice")
	}
	if rooms := e.DirtyRooms(); len(rooms) != 1 || rooms[0] != "discord:1:10" {
		t.Fatalf("%v", rooms)
	}
	bot := m
	bot.MessageID, bot.Bot = "556", true
	if ok, _ := e.Capture(bot); ok {
		t.Fatal("bot messages are not memory")
	}
}

func TestOpenTailNotEmbeddedUntilIdle(t *testing.T) {
	e, _ := newEngine(t, false)
	now := time.Now()
	var batch []corpus.Observation
	for i := 0; i < 3; i++ {
		batch = append(batch, obs("discord:1:20", i, "철수", "방금 한 얘기", now.Add(-time.Duration(3-i)*time.Minute)))
	}
	if _, err := e.Store.InsertObservations(batch, nil); err != nil {
		t.Fatal(err)
	}
	rep, err := e.IndexRooms(context.Background(), IndexOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Rooms[0].Leaves != 1 || rep.Rooms[0].Closed != 0 || rep.LeafVectors != 0 {
		t.Fatalf("open tail must wait: %+v", rep.Rooms[0])
	}
}

// openLexicalDBs counts the in-memory SQLite databases behind loaded indexes (one connectionOpener
// goroutine each, until closed).
func openLexicalDBs() int {
	var b strings.Builder
	_ = pprof.Lookup("goroutine").WriteTo(&b, 1)
	n := 0
	for _, line := range strings.Split(b.String(), "\n") {
		if strings.Contains(line, "database/sql.(*DB).connectionOpener") && strings.HasPrefix(strings.TrimSpace(line), "#") {
			n++
		}
	}
	return n
}

// A config change replaces the engine while a rebuild may still run on the old one. Neither the old
// engine's index nor a rebuild finishing on it may stay open: each is an in-memory SQLite database.
// (The soak test found 2 leaked per config change before Retire.)
func TestReplacedEnginesCloseTheirIndexes(t *testing.T) {
	defer func(d time.Duration) { closeDelay = d }(closeDelay)
	closeDelay = 10 * time.Millisecond
	e, _ := newEngine(t, true)
	seed(t, e)
	if _, err := e.IndexRooms(context.Background(), IndexOptions{BudgetUSD: 1}, nil); err != nil {
		t.Fatal(err)
	}
	settleDBs := func() int {
		deadline := time.Now().Add(2 * time.Second)
		for {
			runtime.GC()
			n := openLexicalDBs()
			if time.Now().After(deadline) {
				return n
			}
			time.Sleep(50 * time.Millisecond)
			if m := openLexicalDBs(); m == n {
				return n
			}
		}
	}
	base := settleDBs()
	cur := e
	for i := 0; i < 5; i++ {
		next := New(cur.Store, cur.LLM, cur.Models, cur.Settings)
		next.Inherit(cur)
		cur.Retire(next)
		// the old engine's rebuild comes back after the switch: it must not install or leak an index
		if _, err := cur.Reload(); !errors.Is(err, errRetired) {
			t.Fatalf("retired engine rebuilt: %v", err)
		}
		if _, err := next.Reload(); err != nil {
			t.Fatal(err)
		}
		cur = next
	}
	time.Sleep(100 * time.Millisecond)
	if got := settleDBs(); got > base {
		t.Fatalf("in-memory index databases: %d before, %d after 5 engine replacements", base, got)
	}
	res, err := cur.Search(context.Background(), SearchRequest{Query: "캠핑", Rooms: []string{"discord:1:10"}, Ranking: retrieval.Hybrid,
		Intent: search.Intent{Kind: "other", Source: "none"}})
	if err != nil || len(res.Hits) == 0 {
		t.Fatalf("current engine must still search: %v %+v", err, res)
	}
}

func TestCaptureReportsNewlyQueuedRoomOnce(t *testing.T) {
	e, _ := newEngine(t, false)
	var calls []string
	e.OnDirty = func(room string) { calls = append(calls, room) }
	for i := 0; i < 3; i++ {
		m := LiveMessage{GuildID: "1", ChannelID: "10", MessageID: itoa(600 + i), AuthorID: "u", AuthorName: "철수", Content: "안녕", At: time.Now()}
		if _, err := e.Capture(m); err != nil {
			t.Fatal(err)
		}
	}
	if len(calls) != 1 || calls[0] != "discord:1:10" {
		t.Fatalf("OnDirty calls %v", calls)
	}
	if q := e.QueuedRooms(); len(q) != 1 {
		t.Fatalf("queued %v", q)
	}
	e.DirtyRooms()
	e.MarkDirty("discord:1:10", "discord:1:11")
	if q := e.QueuedRooms(); len(q) != 2 || len(calls) != 1 {
		t.Fatalf("MarkDirty: queued %v, OnDirty %v", q, calls)
	}
}

// A summary call that fails used to end the pass as if it were complete; it must say it stopped.
func TestSummaryProviderErrorStopsPass(t *testing.T) {
	e, _ := newEngine(t, true)
	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat/completions" {
			http.Error(w, `{"error":{"message":"rejected"}}`, http.StatusBadRequest)
			return
		}
		(&fakeProvider{}).handler().ServeHTTP(w, r)
	}))
	t.Cleanup(fail.Close)
	cred, _ := providers.Resolve("env:FAKE_KEY")
	e.LLM = openai.New(providers.NewClient(5*time.Second, nil), fail.URL, cred)
	seed(t, e)
	rep, err := e.IndexRooms(context.Background(), IndexOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.StoppedBy != "provider_error" {
		t.Fatalf("stopped_by %q, want provider_error (summaries %d)", rep.StoppedBy, rep.Summaries)
	}
}

// Summaries ask for no thinking. When the model still returns no text, nothing stands in for the summary:
// the leaf stays unsummarized for the next pass, nothing is cached, and a pass where every reply was empty
// says it stopped.
func TestEmptySummaryIsLeftForTheNextPass(t *testing.T) {
	e, _ := newEngine(t, true)
	var efforts []string
	var mu sync.Mutex
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat/completions" {
			var req struct {
				Reasoning struct {
					Effort string `json:"effort"`
				} `json:"reasoning"`
				Effort string `json:"reasoning_effort"` // the host family's field (api.go)
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			efforts = append(efforts, req.Reasoning.Effort+req.Effort)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"finish_reason": "length", "message": map[string]any{"content": ""}}},
				"usage": map[string]any{"prompt_tokens": 21, "completion_tokens": 120, "cost": 0.00005}})
			return
		}
		(&fakeProvider{}).handler().ServeHTTP(w, r)
	}))
	t.Cleanup(empty.Close)
	cred, _ := providers.Resolve("env:FAKE_KEY")
	e.LLM = openai.New(providers.NewClient(5*time.Second, nil), empty.URL, cred)
	seed(t, e)
	rep, err := e.IndexRooms(context.Background(), IndexOptions{BudgetUSD: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var summarized, waiting, cached int
	_ = e.Store.DB.DB.QueryRow(`SELECT count(*) FROM memory_leaves WHERE summary<>''`).Scan(&summarized)
	_ = e.Store.DB.DB.QueryRow(`SELECT count(*) FROM memory_leaves WHERE closed=1 AND summary=''`).Scan(&waiting)
	_ = e.Store.DB.DB.QueryRow(`SELECT count(*) FROM memory_embed_cache WHERE kind='summary_text'`).Scan(&cached)
	if summarized != 0 || waiting == 0 || cached != 0 || rep.StoppedBy != "provider_error" {
		t.Fatalf("summarized %d, waiting %d, cached %d, stopped_by %q", summarized, waiting, cached, rep.StoppedBy)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(efforts) == 0 || slices.ContainsFunc(efforts, func(s string) bool { return s != "none" }) {
		t.Fatalf("summary efforts: %v", efforts)
	}
}

// A deleted message leaves search at once, before any rebuild, and the resync that follows re-cuts its
// leaf without calling a model.
func TestDeletedMessageLeavesSearchAtOnce(t *testing.T) {
	e, fp := newEngine(t, false)
	base := time.Now().Add(-48 * time.Hour)
	var batch []corpus.Observation
	for i, l := range []string{"비밀번호는 고양이123", "알겠어", "내일 보자", "응"} {
		o := obs("discord:1:10", i, "철수", l, base.Add(time.Duration(i)*time.Minute))
		o.NativeMessageID = "n" + itoa(i)
		batch = append(batch, o)
	}
	if _, err := e.Store.InsertObservations(batch, nil); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := e.IndexRooms(ctx, IndexOptions{BudgetUSD: 1}, nil); err != nil {
		t.Fatal(err)
	}
	find := func() []search.Hit {
		t.Helper()
		res, err := e.Search(ctx, SearchRequest{Query: "비밀번호 고양이", Rooms: []string{"discord:1:10"}, Ranking: retrieval.Hybrid,
			Intent: search.Intent{Kind: "other", Source: "none"}})
		if err != nil {
			t.Fatal(err)
		}
		return res.Hits
	}
	if len(find()) == 0 {
		t.Fatal("the message must be found before it is deleted")
	}
	var revised []string
	e.OnRevised = func(rooms []string) { revised = rooms }
	if n, err := e.Delete([]string{"n0"}, time.Now()); err != nil || n != 1 {
		t.Fatalf("delete: %d %v", n, err)
	}
	if len(revised) != 1 || revised[0] != "discord:1:10" {
		t.Fatalf("OnRevised: %v", revised)
	}
	for _, h := range find() {
		if strings.Contains(h.Excerpt, "고양이123") {
			t.Fatalf("deleted text still shown before the rebuild: %q", h.Excerpt)
		}
	}
	calls := fp.embedCalls.Load()
	if err := e.Resync([]string{"discord:1:10"}); err != nil {
		t.Fatal(err)
	}
	if fp.embedCalls.Load() != calls {
		t.Fatal("a resync must not call the provider")
	}
	for _, h := range find() {
		if strings.Contains(h.Excerpt, "고양이123") {
			t.Fatalf("deleted text in the rebuilt index: %q", h.Excerpt)
		}
	}
	if _, ok := e.revised.Load("discord:1:10"); !ok {
		t.Fatal("the rebuild must retain the guard for searches holding an older index")
	}
	if n, _ := e.Delete([]string{"n0"}, time.Now()); n != 0 {
		t.Fatal("deleting twice changes nothing")
	}
	// an edit is applied the same way
	if ok, err := e.Edit("n1", "생각해 보니 싫어", time.Now()); err != nil || !ok {
		t.Fatalf("edit: %v %v", ok, err)
	}
	msgs, _ := e.Store.Messages([]string{"discord:1:10"})
	if msgs[0].Content != "생각해 보니 싫어" {
		t.Fatalf("edit not stored: %+v", msgs[0])
	}
}
