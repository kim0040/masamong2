package application_test

import (
	"encoding/json"
	"strings"
	"testing"
)

func residencyRow(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	m, _ := out["features"].(map[string]any)["memory_residency"].(map[string]any)
	return m
}

// ADR-18 stage 0 diagnostics: the memory_residency row reports counts and sizes only, tells "prepared"
// (embedded in the database) from "resident" (loaded in the index search reads), and the blocks list
// carries a per-room estimate. Neither names a room.
func TestMemoryResidencyDoctorRowAndBlockEstimate(t *testing.T) {
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
			VALUES('o1','kakao','c','kakao:friends','kakao:friends','n1','A','this is a message long enough','2026-09-01T00:00:00Z','message','2026-09-01T00:00:00Z')`,
		`INSERT INTO memory_leaves(leaf_id, room, recipe, first_observation_id, observation_ids, n, body, body_hash, speakers, closed, summary, embed_space, leaf_vec, updated_at)
			VALUES('leaf-1','kakao:friends','` + recipe + `','o1','["o1"]',1,'A: this is a message long enough','h','["A"]',1,'s','` + eng.EmbedSpace() + `',x'3c000000','2026-09-01T00:00:00Z')`,
	} {
		if _, err := svc.Store.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	list := func() map[string]any {
		r := exec(svc, "memory.blocks.list", nil)
		if !r.OK {
			t.Fatal(r.Error)
		}
		for _, b := range r.Output["blocks"].([]map[string]any) {
			if b["source_room"] == "kakao:friends" {
				return b
			}
		}
		t.Fatal("block missing")
		return nil
	}

	// prepared, but nothing is loaded yet
	if eng.Index() != nil {
		t.Fatal("an index was loaded before the test asked for one")
	}
	row := residencyRow(t, exec(svc, "runtime.doctor", nil).Output)
	if row == nil || row["status"] != "pending" || row["reason"] != "memory_residency_none" || row["resident_rooms"] != 0 || row["indexed_rooms"] != 1 || row["total_rooms"] != 1 {
		t.Fatalf("before the first load: %v", row)
	}
	b := list()
	if b["resident"] != false || b["memory_estimate_bytes"].(int64) <= 0 {
		t.Fatalf("block before load: resident=%v estimate=%v", b["resident"], b["memory_estimate_bytes"])
	}

	if _, err := eng.Reload(); err != nil {
		t.Fatal(err)
	}
	doctor := exec(svc, "runtime.doctor", nil)
	row = residencyRow(t, doctor.Output)
	if row == nil || row["status"] != "ready" || row["resident_set"] != "all" || row["resident_rooms"] != 1 || row["indexed_rooms"] != 1 || row["total_rooms"] != 1 {
		t.Fatalf("after load: %v", row)
	}
	if n, _ := row["estimated_bytes"].(int64); n <= 0 {
		t.Fatalf("estimate: %v", row["estimated_bytes"])
	}
	if n, _ := row["fts_page_bytes"].(int64); n <= 0 || row["fts_source"] != "sqlite_page_count_x_page_size" {
		t.Fatalf("fts: %v %v", row["fts_page_bytes"], row["fts_source"])
	}
	if n, _ := row["sample_max_heap_bytes"].(uint64); n == 0 || row["sample_basis"] != "stage_boundary_samples" || row["last_rebuild"] != "published" {
		t.Fatalf("samples: %v", row)
	}
	if text, _ := row["reason_text"].(string); text == "" || strings.Contains(text, "{") || strings.Contains(text, "features.reason") {
		t.Fatalf("reason text: %q", text)
	}
	raw, _ := json.Marshal(row)
	if strings.Contains(string(raw), "friends") || strings.Contains(string(raw), "kakao") {
		t.Fatalf("the row names a room: %s", raw)
	}
	b = list()
	if b["resident"] != true || b["memory_estimate_bytes"].(int64) <= 0 {
		t.Fatalf("block after load: resident=%v estimate=%v", b["resident"], b["memory_estimate_bytes"])
	}
}
