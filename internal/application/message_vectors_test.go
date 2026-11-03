package application_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/config"
	"masamong/internal/memory/corpus"
	"masamong/internal/memory/engine"
	"masamong/internal/memory/search"
	"masamong/internal/providers"
	"masamong/internal/providers/openai"
)

func TestMessageVectorDoctorShowsMixedAndShortProviderWidthWaits(t *testing.T) {
	svc := setupSvc(t)
	t.Setenv("MASAMONG_OPENROUTER_KEY", "synthetic-test-key")
	applyDoc(t, svc, func(d *config.Document) {
		m := d.Models["embed"]
		m.Model, m.Dimensions = "baai/bge-m3", 1024
		d.Models["embed"] = m
	})
	eng, err := svc.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	var width atomic.Int32
	width.Store(1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		data := make([]map[string]any, len(req.Input))
		for i := range req.Input {
			v := make([]float32, int(width.Load()))
			v[0] = 1
			data[i] = map[string]any{"index": i, "embedding": v}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("MESSAGE_VECTOR_FAKE_KEY", "synthetic")
	cred, err := providers.Resolve("env:MESSAGE_VECTOR_FAKE_KEY")
	if err != nil {
		t.Fatal(err)
	}
	eng.LLM = openai.New(providers.NewClient(5*time.Second, nil), srv.URL, cred)
	eng.Settings.Summaries = false
	for _, room := range []string{"synthetic:a", "synthetic:z"} {
		if _, err := eng.Store.InsertObservations([]corpus.Observation{{ObservationID: room + "#0", Platform: "discord", Collection: "synthetic", SourceRoom: room, ActorLabel: "synthetic", Content: "synthetic message for width regression", ParsedTime: time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339), RecordKind: corpus.HumanText}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	if _, err := eng.IndexRooms(ctx, engine.IndexOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	checkWait := func(want string) {
		t.Helper()
		feat, _ := exec(svc, "runtime.doctor", nil).Output["features"].(map[string]any)["message_vectors"].(map[string]any)
		if feat == nil || feat["status"] != "pending_index" || !strings.Contains(feat["reason"].(string), "waiting:"+want) {
			t.Fatalf("width wait is missing from message_vectors diagnostics: %v", feat)
		}
	}
	if _, err := svc.Store.DB.Exec(`UPDATE memory_leaves SET leaf_vec=? WHERE room='synthetic:z'`, search.EncodeFloat16(make([]float32, 768))); err != nil {
		t.Fatal(err)
	}
	checkWait("leaf_width_mixed")
	if _, err := svc.Store.DB.Exec(`UPDATE memory_leaves SET leaf_vec=?`, search.EncodeFloat16(make([]float32, 1024))); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Store.DB.Exec(`DELETE FROM memory_message_vectors`); err != nil {
		t.Fatal(err)
	}
	width.Store(768)
	if rep, err := eng.IndexRooms(ctx, engine.IndexOptions{}, nil); err != nil || rep.StoppedBy != "message_width_wait" {
		t.Fatalf("expected short-width wait: %+v %v", rep, err)
	}
	checkWait("provider_width_short:768/1024")
	if rep, err := eng.IndexRooms(ctx, engine.IndexOptions{}, nil); err != nil || rep.LeafVectors != 0 || rep.MessageVectors != 0 || rep.StoppedBy != "message_width_wait" {
		t.Fatalf("short messages must not repair uniform leaves: %+v %v", rep, err)
	}
	if _, err := svc.Store.DB.Exec(`UPDATE memory_leaves SET leaf_vec=?`, search.EncodeFloat16(make([]float32, 768))); err != nil {
		t.Fatal(err)
	}
	_ = exec(svc, "runtime.doctor", nil)
	if _, err := svc.Store.DB.Exec(`UPDATE memory_leaves SET leaf_vec=?`, search.EncodeFloat16(make([]float32, 1024))); err != nil {
		t.Fatal(err)
	}
	checkWait("provider_width_short:768/1024")
	if _, err := svc.Store.DB.Exec(`UPDATE memory_leaves SET leaf_vec=?`, search.EncodeFloat16(make([]float32, 768))); err != nil {
		t.Fatal(err)
	}
	if rep, err := eng.IndexRooms(ctx, engine.IndexOptions{}, nil); err != nil || rep.MessageVectors != 2 {
		t.Fatalf("changed leaf width must release messages at 768 dimensions: %+v %v", rep, err)
	}
	feat, _ := exec(svc, "runtime.doctor", nil).Output["features"].(map[string]any)["message_vectors"].(map[string]any)
	if feat == nil || feat["status"] != "ready" || !strings.Contains(feat["reason"].(string), "message_dims:768") {
		t.Fatalf("diagnostics did not clear the wait after leaf convergence: %v", feat)
	}
}

func memorySection(t *testing.T, svc interface{}, out map[string]any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(out["memory"])
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return m
}

// memory.message_vectors is on unless it is set to false. It reaches the engine (the embedding step) and the
// search config (the candidate source) together, and a change of it builds a new engine. While it is
// explicitly off nothing about it shows in the memory status or the doctor.
func TestMessageVectorsSettingIsWiredToEngineStatusAndDoctor(t *testing.T) {
	svc, _ := slowChatSvc(t, 0, nil)
	def, err := svc.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	if svc.Doc().Memory.MessageVectors != nil {
		t.Fatal("the test service sets the field itself")
	}
	if !def.Settings.MessageVectors || def.Settings.Search.Retrieval.MessageDense.Enabled == nil || !*def.Settings.Search.Retrieval.MessageDense.Enabled {
		t.Fatalf("an unset memory.message_vectors is not on: %+v", def.Settings.Search.Retrieval.MessageDense)
	}
	status, ok := memorySection(t, svc, exec(svc, "memory.blocks.list", nil).Output)["message_vectors"].(map[string]any)
	if !ok || status["enabled"] != true || status["pending"] == nil || status["stored"] == nil || status["dimensions"] == nil {
		t.Fatalf("default status %v", status)
	}
	if _, has := status["estimate"]; has {
		t.Fatal("the estimate is only added when asked for")
	}

	// an explicit false turns it off, and then nothing about it shows
	off := false
	applyDoc(t, svc, func(d *config.Document) { d.Memory.MessageVectors = &off })
	disabled, err := svc.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	if disabled == def {
		t.Fatal("memory.message_vectors changed but the engine was kept")
	}
	if disabled.Settings.MessageVectors || disabled.Settings.Search.Retrieval.MessageDense.Enabled != nil {
		t.Fatalf("explicit false: %+v", disabled.Settings.Search.Retrieval.MessageDense)
	}
	list := exec(svc, "memory.blocks.list", nil)
	if !list.OK {
		t.Fatal(list.Error)
	}
	if _, has := memorySection(t, svc, list.Output)["message_vectors"]; has {
		t.Fatal("the memory status mentions message vectors while the setting is off")
	}
	if _, has := memorySection(t, svc, exec(svc, "memory.blocks.list", map[string]any{"message_vectors_estimate": false}).Output)["message_vectors"]; has {
		t.Fatal("an explicit false asked for the section")
	}
	doctor := exec(svc, "runtime.doctor", nil)
	if !doctor.OK {
		t.Fatal(doctor.Error)
	}
	if _, has := doctor.Output["features"].(map[string]any)["message_vectors"]; has {
		t.Fatal("the doctor lists message vectors while the setting is off")
	}

	// the console asks for the estimate before the operator turns it back on
	pre := memorySection(t, svc, exec(svc, "memory.blocks.list", map[string]any{"message_vectors_estimate": true}).Output)
	mv, ok := pre["message_vectors"].(map[string]any)
	if !ok || mv["enabled"] != false || mv["estimate"] == nil || mv["space"] == "" {
		t.Fatalf("estimate before turning on: %v", pre["message_vectors"])
	}
	if _, has := mv["pending"]; has {
		t.Fatal("a pending count while the setting is off")
	}
	// messages outside any prepared leaf are reported apart, so the console never shows them as free
	if est, _ := mv["estimate"].(map[string]any); est == nil || est["unprepared"] == nil || est["dimensions"] == nil || est["disk_bytes"] == nil {
		t.Fatalf("the estimate does not carry the unprepared messages: %v", mv["estimate"])
	}

	on := true
	applyDoc(t, svc, func(d *config.Document) { d.Memory.MessageVectors = &on })
	again, err := svc.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	if again == disabled || !again.Settings.MessageVectors || again.Settings.Search.Retrieval.MessageDense.Enabled == nil || !*again.Settings.Search.Retrieval.MessageDense.Enabled {
		t.Fatalf("an explicit true does not reach the engine: %+v", again.Settings.Search.Retrieval.MessageDense)
	}
}

func TestMessageVectorTruncationUsesTheMRLCatalog(t *testing.T) {
	svc := setupSvc(t)
	applyDoc(t, svc, func(d *config.Document) {
		m := d.Models["embed"]
		m.Model, m.Dimensions = "qwen/qwen3-embedding-8b", 4096
		d.Models["embed"] = m
	})
	mrl, err := svc.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	if !mrl.Models.EmbedTruncatable || mrl.MessageVectorDims() != 512 {
		t.Fatalf("catalogued MRL model was not cut to 512: models=%+v dims=%d", mrl.Models, mrl.MessageVectorDims())
	}
	applyDoc(t, svc, func(d *config.Document) {
		m := d.Models["embed"]
		m.Model, m.Dimensions = "baai/bge-m3", 0
		d.Models["embed"] = m
	})
	nonMRL, err := svc.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	if nonMRL.Models.EmbedTruncatable || nonMRL.MessageVectorDims() != 0 || nonMRL.MessageSpace() != "" {
		t.Fatalf("uncatalogued model must keep its full native width: models=%+v dims=%d space=%s", nonMRL.Models, nonMRL.MessageVectorDims(), nonMRL.MessageSpace())
	}
}

func TestMessageVectorNonMRLExplicitDimensionsAreWired(t *testing.T) {
	svc := setupSvc(t)
	for _, dims := range []int{256, 1024, 4096} {
		applyDoc(t, svc, func(d *config.Document) {
			m := d.Models["embed"]
			m.Model, m.Dimensions = "baai/bge-m3", dims
			d.Models["embed"] = m
		})
		eng, err := svc.MemoryEngine()
		if err != nil {
			t.Fatal(err)
		}
		want := dims
		if dims == 4096 {
			want = 0 // preserve the existing full-width leaf space; messages wait for actual leaf metadata
		}
		if eng.Models.EmbedTruncatable || eng.Models.EmbedDims != want || eng.MessageVectorDims() != want {
			t.Fatalf("non-MRL leaf and message dimensions must agree: want=%d leaf=%d message=%d", want, eng.Models.EmbedDims, eng.MessageVectorDims())
		}
		if dims == 4096 && (eng.EmbedSpace() != "baai/bge-m3:0:l2" || eng.MessageSpace() != "") {
			t.Fatal("explicit 4096 must preserve the legacy full-width leaf space and wait for actual message width")
		}
	}
}

// An install that updates with no memory.message_vectors field has it on, and the messages of its closed
// leaves that have no vector yet show in the doctor as pending_messages:<n>; an explicit false hides the
// line again.
func TestMessageVectorsDoctorShowsPendingMessagesAfterUpgrade(t *testing.T) {
	svc := setupSvc(t)
	t.Setenv("MASAMONG_OPENROUTER_KEY", "or-key") // the embedding model's provider has its key
	eng, err := svc.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO memory_rooms(room,collection,platform,updated_at,active_recipe)
			VALUES('kakao:friends','c','kakao','2026-09-01T00:00:00Z','` + eng.Settings.Recipe.Version() + `')`,
		`INSERT INTO memory_observations(observation_id, platform, collection, source_room, room, native_message_id, actor_label, content, parsed_time, record_kind, imported_at)
			VALUES('o1','kakao','c','kakao:friends','kakao:friends','n1','A','this is a message long enough','2026-09-01T00:00:00Z','message','2026-09-01T00:00:00Z')`,
		`INSERT INTO memory_leaves(leaf_id, room, recipe, first_observation_id, observation_ids, n, body, body_hash, speakers, closed, summary, embed_space, leaf_vec, updated_at)
			VALUES('leaf-1','kakao:friends','` + eng.Settings.Recipe.Version() + `','o1','["o1"]',1,'A: this is a message long enough','h','["A"]',1,'s','` + eng.EmbedSpace() + `',x'00','2026-09-01T00:00:00Z')`,
	} {
		if _, err := svc.Store.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	feat := func() map[string]any {
		m, _ := exec(svc, "runtime.doctor", nil).Output["features"].(map[string]any)["message_vectors"].(map[string]any)
		return m
	}
	if m := feat(); m == nil || m["status"] != "pending_index" || !strings.HasPrefix(m["reason"].(string), "pending_messages:1;message_dims:") {
		t.Fatalf("an unset setting should list the pending message: %v", m)
	}
	off := false
	applyDoc(t, svc, func(d *config.Document) { d.Memory.MessageVectors = &off })
	if m := feat(); m != nil {
		t.Fatalf("an explicit false still lists message vectors: %v", m)
	}
}

// config.patch keeps an explicit false for memory.message_vectors, and unsetting it returns to the default.
func TestMessageVectorsConfigPatch(t *testing.T) {
	svc := setupSvc(t)
	if !svc.Doc().Memory.MessageVectorsOn() {
		t.Fatal("not on by default")
	}
	patch := func(op map[string]any) {
		t.Helper()
		op["path"] = []any{"memory", "message_vectors"}
		if r := exec(svc, "config.patch", map[string]any{"ops": []any{op}}); !r.OK {
			t.Fatalf("patch: %+v", r.Error)
		}
	}
	patch(map[string]any{"value": false})
	if mv := svc.Doc().Memory.MessageVectors; mv == nil || *mv || svc.Doc().Memory.MessageVectorsOn() {
		t.Fatalf("false not kept: %v", svc.Doc().Memory.MessageVectors)
	}
	patch(map[string]any{"op": "unset"})
	if svc.Doc().Memory.MessageVectors != nil || !svc.Doc().Memory.MessageVectorsOn() {
		t.Fatalf("unset not back to on: %v", svc.Doc().Memory.MessageVectors)
	}
}

// The console's pending count walks every message of every closed leaf, so memory.blocks.list counts only
// once and then answers with the last count at once; when the stored vectors change it counts again in
// the background and a later list shows the new number.
func TestMessageVectorsPendingCountIsCachedAndRefreshedInTheBackground(t *testing.T) {
	svc := setupSvc(t)
	t.Setenv("MASAMONG_OPENROUTER_KEY", "or-key")
	eng, err := svc.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	recipe := eng.Settings.Recipe.Version()
	for _, q := range []string{
		`INSERT INTO memory_rooms(room,collection,platform,updated_at,active_recipe)
			VALUES('kakao:friends','c','kakao','2026-09-01T00:00:00Z','` + recipe + `')`,
		`INSERT INTO memory_observations(observation_id, platform, collection, source_room, room, native_message_id, actor_label, content, parsed_time, record_kind, imported_at)
			VALUES('o1','kakao','c','kakao:friends','kakao:friends','n1','A','this is a message long enough','2026-09-01T00:00:00Z','message','2026-09-01T00:00:00Z'),
			('o2','kakao','c','kakao:friends','kakao:friends','n2','B','and here is another one of them','2026-09-01T00:01:00Z','message','2026-09-01T00:00:00Z')`,
		`INSERT INTO memory_leaves(leaf_id, room, recipe, first_observation_id, observation_ids, n, body, body_hash, speakers, closed, summary, embed_space, leaf_vec, updated_at)
			VALUES('leaf-1','kakao:friends','` + recipe + `','o1','["o1","o2"]',2,'A: x','h','["A","B"]',1,'s','` + eng.EmbedSpace() + `',x'00','2026-09-01T00:00:00Z')`,
	} {
		if _, err := svc.Store.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	svc.SetPendingRecountGapForTest(300 * time.Millisecond)
	pending := func() any {
		status, _ := memorySection(t, svc, exec(svc, "memory.blocks.list", nil).Output)["message_vectors"].(map[string]any)
		return status["pending"]
	}
	if got := pending(); got != float64(2) {
		t.Fatalf("first count = %v, want 2", got)
	}
	if _, err := svc.Store.DB.Exec(`INSERT INTO memory_message_vectors(observation_id, room, embed_space, text_hash, label, vec, updated_at)
		VALUES('o1','kakao:friends',?,'h','A',x'00','2026-09-01T00:00:00Z')`, eng.MessageSpace()); err != nil {
		t.Fatal(err)
	}
	events, stop := svc.Subscribe()
	defer stop()
	if got := pending(); got != float64(2) {
		t.Fatalf("the list after a change = %v, want the last count (2) while it is counted again", got)
	}
	// within the gap after the last count nothing is counted; when it ends the console is asked to list
	// again (as an open one does on a memory event), that list counts, and the new number is announced
	timeout := time.After(10 * time.Second)
	for announced := false; !announced; {
		select {
		case ev := <-events:
			if ev.Kind == "memory" && ev.Ref == "pending_stale" {
				pending()
			}
			announced = ev.Kind == "memory" && ev.Ref == "pending_counted"
		case <-timeout:
			t.Fatal("the background count was not announced")
		}
	}
	if got := pending(); got != float64(1) {
		t.Fatalf("after the announced recount = %v, want 1", got)
	}
	// deleting a waiting message (it has no vector, so the stored count stays) is counted again too
	if _, err := svc.Store.DB.Exec(`UPDATE memory_observations SET deleted_at='2026-09-02T00:00:00Z' WHERE observation_id='o2'`); err != nil {
		t.Fatal(err)
	}
	pending()
	deadline := time.Now().Add(10 * time.Second)
	for pending() != float64(0) {
		if time.Now().After(deadline) {
			t.Fatal("a deleted waiting message stayed in the count")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
