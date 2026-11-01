package engine

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"

	"masamong/internal/memory/retrieval"
	"masamong/internal/memory/search"
	"masamong/internal/memory/store"
)

// seed (engine_test.go) stores 13 messages in two rooms. Seven of them have at least six meaningful
// characters ("주말에 캠핑 가자", "좋아 토요일 출발", "고기는 내가 살게", "비 오면 어떡해", "그땐 취소하자",
// "또 마라탕이냐" and the other room's line); the rest are short chat and stay out of the message index.
const seededEligible = 7

func messageEngine(t *testing.T) (*Engine, *fakeProvider) {
	t.Helper()
	e, fp := newEngine(t, false)
	e.Settings.MessageVectors = true
	on := true
	e.Settings.Search.Retrieval.MessageDense.Enabled = &on
	return e, fp
}

func embedSpendCalls(t *testing.T, e *Engine) int {
	t.Helper()
	var n int
	if err := e.Store.DB.DB.QueryRow(`SELECT COALESCE(SUM(calls),0) FROM memory_spend WHERE kind='embed'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestMessageSpaceAndDimensions(t *testing.T) {
	for _, c := range []struct {
		dims        int
		truncatable bool
		want        int
	}{{0, false, 0}, {4096, false, 4096}, {1024, false, 1024}, {256, false, 256}, {4096, true, 512}, {2048, true, 512}, {1025, true, 512}, {1024, true, 512}, {768, true, 512}, {512, true, 512}, {256, true, 256}} {
		e := &Engine{Models: Models{Embed: "m/x:free", EmbedDims: c.dims, EmbedTruncatable: c.truncatable}}
		if e.messageDims() != c.want {
			t.Fatalf("EmbedDims %d truncatable=%v: message dims %d, want %d", c.dims, c.truncatable, e.messageDims(), c.want)
		}
		want := ""
		if c.want > 0 {
			want = "m/x:free:" + itoa(c.want) + ":l2"
		}
		if e.MessageSpace() != want {
			t.Fatalf("space %q, want %q", e.MessageSpace(), want)
		}
	}
	// The measurement override is accepted through 1024; an invalid value falls back to the MRL rule.
	for _, c := range []struct{ dims, override, want int }{{1024, 512, 512}, {0, 512, 512}, {4096, 1024, 1024}, {1024, 0, 512}, {1024, 2048, 512}, {768, -1, 512}} {
		e := &Engine{Models: Models{Embed: "m/x:free", EmbedDims: c.dims, EmbedTruncatable: true}, Settings: Settings{MessageDims: c.override}}
		if e.messageDims() != c.want || e.MessageSpace() != "m/x:free:"+itoa(c.want)+":l2" {
			t.Fatalf("EmbedDims %d override %d: message dims %d (%q), want %d", c.dims, c.override, e.messageDims(), e.MessageSpace(), c.want)
		}
	}
	// A non-MRL vector uses its full native width and is normalized without truncation.
	e := &Engine{Models: Models{Embed: "gemini-embedding-2", EmbedDims: 4096}}
	long := make([]float32, 4096)
	for i := range long {
		long[i] = 1
	}
	v := e.fitMessageVec(long)
	var n float64
	for _, x := range v {
		n += float64(x) * float64(x)
	}
	if len(v) != 4096 || math.Abs(n-1) > 1e-6 || e.MessageSpace() != "gemini-embedding-2:4096:l2" {
		t.Fatalf("len %d norm² %g", len(v), n)
	}
	// A known MRL model with an explicit 4096 full-width leaf still stores 512-dimensional messages.
	e.Models = Models{Embed: "m", EmbedDims: 4096, EmbedTruncatable: true}
	if got := e.fitMessageVec(long); len(got) != 512 {
		t.Fatalf("MRL full-width vector has %d message dimensions, want 512", len(got))
	}
	e.Models.EmbedDims = 4
	e.Models.EmbedTruncatable = true
	if v := e.fitMessageVec([]float32{3, 4, 0, 0, 9, 9}); len(v) != 4 || math.Abs(float64(v[0])-0.6) > 1e-6 || math.Abs(float64(v[1])-0.8) > 1e-6 {
		t.Fatalf("%v", v)
	}
	if v := e.fitMessageVec([]float32{3, 4}); len(v) != 2 {
		t.Fatalf("a vector shorter than the dimensionality must stay as it is: %v", v)
	}
}

func TestIndexRoomsEmbedsMessageVectors(t *testing.T) {
	e, fp := messageEngine(t)
	seed(t, e)
	ctx := context.Background()

	// the preview counts the messages, prices them with the leaves and calls nothing
	dry, err := e.IndexRooms(ctx, IndexOptions{DryRun: true}, nil)
	if err != nil || dry.Estimate == nil || fp.embedCalls.Load() != 0 {
		t.Fatalf("dry run: %+v %v calls=%d", dry, err, fp.embedCalls.Load())
	}
	if dry.Estimate.MessageEmbeds != seededEligible || dry.Estimate.MessageTokens <= 0 {
		t.Fatalf("estimate counts %d messages / %d tokens, want %d", dry.Estimate.MessageEmbeds, dry.Estimate.MessageTokens, seededEligible)
	}
	withMsgs := *dry.Estimate
	e.Settings.MessageVectors = false
	dryOff, err := e.IndexRooms(ctx, IndexOptions{DryRun: true}, nil)
	e.Settings.MessageVectors = true
	if err != nil {
		t.Fatal(err)
	}
	off := *dryOff.Estimate
	if off.MessageEmbeds != 0 || off.EmbedTokens != withMsgs.EmbedTokens || off.LeafEmbeds != withMsgs.LeafEmbeds {
		t.Fatalf("flag off: %+v vs %+v", off, withMsgs)
	}
	wantUSD := off.EmbedUSD + float64(withMsgs.MessageTokens)*fallbackEmbedPerTok
	if math.Abs(withMsgs.EmbedUSD-wantUSD) > 1e-12 || math.Abs(withMsgs.TotalUSD-(off.TotalUSD+float64(withMsgs.MessageTokens)*fallbackEmbedPerTok)) > 1e-12 {
		t.Fatalf("estimate does not include the message tokens: %+v vs %+v", withMsgs, off)
	}
	if withMsgs.Seconds <= off.Seconds {
		t.Fatalf("estimated time %d should grow with the message batches (was %d)", withMsgs.Seconds, off.Seconds)
	}

	rep, err := e.IndexRooms(ctx, IndexOptions{BudgetUSD: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.StoppedBy != "" || rep.MessageVectors != seededEligible {
		t.Fatalf("%+v", rep)
	}
	if rep.Reload["message_vectors"] != seededEligible || rep.Reload["message_stale"] != 0 {
		t.Fatalf("reload %+v", rep.Reload)
	}
	total, in, err := e.Store.MessageVectorCounts(e.MessageSpace())
	if err != nil || total != seededEligible || in != seededEligible {
		t.Fatalf("stored %d/%d %v", total, in, err)
	}
	ix := e.Index()
	if ix == nil || !ix.HasMessageVectors() || ix.MsgVecs.Rows() != seededEligible || ix.MsgVecs.Q == nil || ix.MsgVecs.Dims != 16 {
		t.Fatalf("loaded index: %+v", ix.MsgVecs.Dims)
	}
	// every embedding call was recorded as spend: leaves (one batch) + message batch(es)
	if got, want := embedSpendCalls(t, e), int(fp.embedCalls.Load()); got != want || want < 2 {
		t.Fatalf("recorded %d embed calls, provider saw %d", got, want)
	}

	// nothing pending: the second pass pays nothing and keeps the loaded index
	calls := fp.embedCalls.Load()
	rep2, err := e.IndexRooms(ctx, IndexOptions{BudgetUSD: 1}, nil)
	if err != nil || rep2.MessageVectors != 0 || fp.embedCalls.Load() != calls || rep2.Reload != nil {
		t.Fatalf("second pass must be free and keep the index: %+v %v", rep2, err)
	}

	// nothing of it is in the embedding cache: the message vector table is the only copy
	var cached int
	if err := e.Store.DB.DB.QueryRow(`SELECT COUNT(*) FROM memory_embed_cache WHERE kind='message'`).Scan(&cached); err != nil || cached != 0 {
		t.Fatalf("%d message rows in the embedding cache (%v)", cached, err)
	}
	// the vectors are lost (a table cleared by hand): they are embedded again
	if _, err := e.Store.DB.DB.Exec(`DELETE FROM memory_message_vectors`); err != nil {
		t.Fatal(err)
	}
	rep3, err := e.IndexRooms(ctx, IndexOptions{BudgetUSD: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep3.MessageVectors != seededEligible || fp.embedCalls.Load() <= calls || rep3.Reload == nil {
		t.Fatalf("refill: %+v calls %d→%d", rep3, calls, fp.embedCalls.Load())
	}
	if total, _, _ := e.Store.MessageVectorCounts(e.MessageSpace()); total != seededEligible {
		t.Fatalf("refilled %d", total)
	}
}

func TestMessageVectorBudgetStopsAndResumes(t *testing.T) {
	e, fp := messageEngine(t)
	e.Settings.EmbedBatch, e.Settings.Workers = 2, 1
	seed(t, e)
	ctx := context.Background()
	// the fake charges 0.000001 per call: the two leaf batches use 2e-6 of the 2.5e-6, so only the first
	// first message batch starts; the next one, waiting for the worker, is checked again once it holds
	// the slot and does not run
	rep, err := e.IndexRooms(ctx, IndexOptions{BudgetUSD: 0.0000025}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.StoppedBy != "budget" || rep.MessageVectors != 2 {
		t.Fatalf("budget stop: %+v", rep)
	}
	calls := fp.embedCalls.Load()
	rep2, err := e.IndexRooms(ctx, IndexOptions{BudgetUSD: 1}, nil)
	if err != nil || rep2.StoppedBy != "" || rep2.MessageVectors != seededEligible-rep.MessageVectors || fp.embedCalls.Load() == calls {
		t.Fatalf("resume: %+v %v", rep2, err)
	}
	if total, _, _ := e.Store.MessageVectorCounts(e.MessageSpace()); total != seededEligible {
		t.Fatalf("stored %d", total)
	}
}

func TestMessageVectorsOffIsInvisible(t *testing.T) {
	e, fp := newEngine(t, false)
	seed(t, e)
	rep, err := e.IndexRooms(context.Background(), IndexOptions{BudgetUSD: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(rep)
	if strings.Contains(string(raw), "message_") {
		t.Fatalf("the report mentions messages while the setting is off: %s", raw)
	}
	if _, has := rep.Reload["message_vectors"]; has || rep.Estimate.MessageEmbeds != 0 {
		t.Fatalf("%+v", rep)
	}
	if total, _, _ := e.Store.MessageVectorCounts(e.MessageSpace()); total != 0 {
		t.Fatalf("message vectors stored with the setting off: %d", total)
	}
	if e.Index().HasMessageVectors() {
		t.Fatal("message vectors loaded with the setting off")
	}
	// a leaf batch only: one /embeddings call for the 3 leaves
	if n := fp.embedCalls.Load(); n != 1 {
		t.Fatalf("%d embedding calls", n)
	}
	// even with stored message vectors around, an engine that has the setting off does not load them
	pend, _ := e.Store.PendingMessages(nil, e.MessageSpace(), 0)
	vecs := make([]store.MessageVector, len(pend))
	for i, p := range pend {
		vecs[i] = store.MessageVector{ObservationID: p.ObservationID, Space: e.MessageSpace(), TextHash: p.TextHash, Vec: search.EncodeQuantizedRow(vecFor(p.Text))}
	}
	if n, err := e.Store.SaveMessageVectors(vecs); err != nil || n != seededEligible {
		t.Fatalf("%d %v", n, err)
	}
	if _, err := e.Reload(); err != nil || e.Index().HasMessageVectors() {
		t.Fatalf("an engine with the setting off loaded message vectors (%v)", err)
	}
}

func TestSearchTracesMessageRunWhenOn(t *testing.T) {
	e, _ := messageEngine(t)
	seed(t, e)
	ctx := context.Background()
	if _, err := e.IndexRooms(ctx, IndexOptions{BudgetUSD: 1}, nil); err != nil {
		t.Fatal(err)
	}
	res, err := e.Search(ctx, SearchRequest{Query: "캠핑 어디로 가", Rooms: []string{"discord:1:10"}, Ranking: retrieval.Hybrid,
		Intent: search.Intent{Kind: "other", Source: "none"}})
	if err != nil || len(res.Hits) == 0 {
		t.Fatalf("%v %+v", err, res)
	}
	md, ok := res.Trace["message_dense"].(map[string]any)
	if !ok || md["leaves"].(int) == 0 || md["messages_scored"].(int) == 0 {
		t.Fatalf("trace %v", res.Trace["message_dense"])
	}
	for _, h := range res.Hits {
		if h.Room != "discord:1:10" {
			t.Fatalf("scope leak through message vectors: %s", h.Room)
		}
	}
	// the setting on the search side alone is not enough: an engine without message vectors stays plain
	plain, _ := newEngine(t, false)
	on := true
	plain.Settings.Search.Retrieval.MessageDense.Enabled = &on
	seed(t, plain)
	if _, err := plain.IndexRooms(ctx, IndexOptions{BudgetUSD: 1}, nil); err != nil {
		t.Fatal(err)
	}
	res, err = plain.Search(ctx, SearchRequest{Query: "캠핑 어디로 가", Rooms: []string{"discord:1:10"}, Ranking: retrieval.Hybrid,
		Intent: search.Intent{Kind: "other", Source: "none"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, has := res.Trace["message_dense"]; has {
		t.Fatal("trace has message_dense without message vectors")
	}
}

func TestMessageWidthChangeRebuildsWithoutMixingSpaces(t *testing.T) {
	old, fp := messageEngine(t)
	t.Cleanup(old.Close)
	old.Models.EmbedDims = 1024
	old.Models.EmbedTruncatable = true
	old.Settings.MessageDims = 1024 // emulate the previous default
	seed(t, old)
	ctx := context.Background()
	if _, err := old.IndexRooms(ctx, IndexOptions{BudgetUSD: 1}, nil); err != nil {
		t.Fatal(err)
	}
	if old.MessageSpace() != "fake-embed:1024:l2" {
		t.Fatalf("old space %q", old.MessageSpace())
	}

	settings := old.Settings
	settings.MessageDims = 0
	next := New(old.Store, old.LLM, old.Models, settings)
	t.Cleanup(next.Close)
	if next.MessageSpace() != "fake-embed:512:l2" {
		t.Fatalf("new space %q", next.MessageSpace())
	}
	info, err := next.Reload()
	if err != nil || info["message_vectors"] != 0 || next.Index().HasMessageVectors() {
		t.Fatalf("cold load mixed old vectors into the new space: info=%v err=%v", info, err)
	}
	res, err := next.Search(ctx, SearchRequest{Query: "캠핑 어디로 가", Rooms: []string{"discord:1:10"}, Ranking: retrieval.Hybrid,
		Intent: search.Intent{Kind: "other", Source: "none"}})
	if err != nil || len(res.Hits) == 0 {
		t.Fatalf("search during re-embedding lost leaf results: hits=%d err=%v", len(res.Hits), err)
	}
	if _, has := res.Trace["message_dense"]; has {
		t.Fatalf("old-space message candidates were used: %v", res.Trace["message_dense"])
	}

	before := fp.embedCalls.Load()
	rep, err := next.IndexRooms(ctx, IndexOptions{BudgetUSD: 1}, nil)
	if err != nil || rep.MessageVectors != seededEligible || fp.embedCalls.Load() <= before {
		t.Fatalf("re-index did not rebuild the new space: report=%+v err=%v", rep, err)
	}
	total, inSpace, err := next.Store.MessageVectorCounts(next.MessageSpace())
	if err != nil || total != seededEligible || inSpace != seededEligible {
		t.Fatalf("rows were not replaced by observation_id: total=%d in-space=%d err=%v", total, inSpace, err)
	}
	if next.Index().MsgVecs.Dims != 16 { // the fake provider's native width is 16; production MRL vectors are cut at 512
		t.Fatalf("reloaded fake vectors have %d dimensions", next.Index().MsgVecs.Dims)
	}
}

func TestInheritKeepsIndexOnlyWhenMessageSettingMatches(t *testing.T) {
	old, _ := messageEngine(t)
	seed(t, old)
	if _, err := old.IndexRooms(context.Background(), IndexOptions{BudgetUSD: 1}, nil); err != nil {
		t.Fatal(err)
	}
	same := New(old.Store, old.LLM, old.Models, old.Settings)
	same.Inherit(old)
	if same.Index() == nil {
		t.Fatal("an unchanged configuration drops the loaded index")
	}
	narrow := old.Settings
	narrow.MessageDims = 512
	other := New(old.Store, old.LLM, old.Models, narrow)
	other.Inherit(old)
	if other.Index() != nil {
		t.Fatal("an index with message vectors of another width was handed over")
	}
	off := old.Settings
	off.MessageVectors = false
	next := New(old.Store, old.LLM, old.Models, off)
	next.Inherit(old)
	if next.Index() != nil {
		t.Fatal("an index built with message vectors was handed to an engine without them (its search config would linger)")
	}
	// switching on, from an index that never had them
	plain, _ := newEngine(t, false)
	seed(t, plain)
	if _, err := plain.IndexRooms(context.Background(), IndexOptions{BudgetUSD: 1}, nil); err != nil {
		t.Fatal(err)
	}
	on := plain.Settings
	on.MessageVectors = true
	next = New(plain.Store, plain.LLM, plain.Models, on)
	next.Inherit(plain)
	if next.Index() != nil {
		t.Fatal("an index without message vectors was handed to an engine that wants them")
	}
}

// The console shows what turning message vectors on would cost before the setting is saved: the engine
// built with it off answers, and nothing is called.
func TestEstimateMessageVectorsBeforeTurningThemOn(t *testing.T) {
	e, fp := newEngine(t, false)
	e.Models.EmbedDims = 16
	e.Models.EmbedTruncatable = true
	seed(t, e)
	ctx := context.Background()
	if _, err := e.IndexRooms(ctx, IndexOptions{BudgetUSD: 1}, nil); err != nil {
		t.Fatal(err)
	}
	calls := fp.embedCalls.Load()
	est, err := e.EstimateMessageVectors(nil)
	if err != nil || est.Messages != seededEligible || est.Tokens <= 0 || est.USD <= 0 || est.Seconds <= 0 || est.Dimensions != 16 || est.DiskBytes != int64(seededEligible*20) {
		t.Fatalf("%+v %v", est, err)
	}
	if one, _ := e.EstimateMessageVectors([]string{"discord:1:99"}); one.Messages != 1 {
		t.Fatalf("one room: %+v", one)
	}
	if fp.embedCalls.Load() != calls {
		t.Fatal("the estimate called the provider")
	}
	// turned on and indexed: nothing is left to embed
	e.Settings.MessageVectors = true
	if _, err := e.IndexRooms(ctx, IndexOptions{BudgetUSD: 1}, nil); err != nil {
		t.Fatal(err)
	}
	if est, _ := e.EstimateMessageVectors(nil); est.Messages != 0 {
		t.Fatalf("after indexing: %+v", est)
	}
	if _, err := e.Store.DB.DB.Exec(`DELETE FROM memory_message_vectors`); err != nil {
		t.Fatal(err)
	}
	if est, _ := e.EstimateMessageVectors(nil); est.Messages != seededEligible || est.USD <= 0 {
		t.Fatalf("with the vectors gone every message is estimated again: %+v", est)
	}
}

// A renamed speaker changes the embedded text. The next pass counts the message again in its estimate,
// embeds it (the new text is not in the cache) and a rebuilt engine loads it with nothing stale.
func TestRenamedSpeakerIsEmbeddedAgainAndLoaded(t *testing.T) {
	e, fp := messageEngine(t)
	seed(t, e)
	ctx := context.Background()
	if rep, err := e.IndexRooms(ctx, IndexOptions{BudgetUSD: 1}, nil); err != nil || rep.MessageVectors != seededEligible {
		t.Fatalf("%+v %v", rep, err)
	}
	if _, err := e.Store.DB.DB.Exec(`UPDATE memory_observations SET actor_label='renamed' WHERE observation_id='discord:1:10#0'`); err != nil {
		t.Fatal(err)
	}
	var oldHash string
	if err := e.Store.DB.DB.QueryRow(`SELECT text_hash FROM memory_message_vectors WHERE observation_id='discord:1:10#0'`).Scan(&oldHash); err != nil {
		t.Fatal(err)
	}
	est, err := e.EstimateMessageVectors(nil)
	if err != nil || est.Messages != 1 {
		t.Fatalf("the estimate must list the renamed message: %+v %v", est, err)
	}
	dry, err := e.IndexRooms(ctx, IndexOptions{DryRun: true}, nil)
	if err != nil || dry.Estimate == nil || dry.Estimate.MessageEmbeds != 1 {
		t.Fatalf("dry run %+v %v", dry.Estimate, err)
	}
	before := fp.embedCalls.Load()
	rep, err := e.IndexRooms(ctx, IndexOptions{BudgetUSD: 1}, nil)
	// the label is part of the leaf's text too, so its leaf is cut and embedded again alongside
	if err != nil || rep.MessageVectors != 1 || fp.embedCalls.Load() != before+1+int32(rep.LeafVectors) {
		t.Fatalf("%+v %v (%d calls)", rep, err, fp.embedCalls.Load()-before)
	}
	var newHash string
	if err := e.Store.DB.DB.QueryRow(`SELECT text_hash FROM memory_message_vectors WHERE observation_id='discord:1:10#0'`).Scan(&newHash); err != nil || newHash == oldHash {
		t.Fatalf("the stored hash did not follow the new label (%v)", err)
	}
	if n, _ := e.Store.CountPendingMessages(nil, e.MessageSpace(), e.Settings.Recipe.Version()); n != 0 {
		t.Fatalf("still pending: %d", n)
	}
	fresh := New(e.Store, e.LLM, e.Models, e.Settings)
	info, err := fresh.Reload()
	if err != nil || info["message_vectors"] != seededEligible || info["message_stale"] != 0 {
		t.Fatalf("reload %+v %v", info, err)
	}
}
