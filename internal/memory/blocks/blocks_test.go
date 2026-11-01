package blocks_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"masamong/internal/memory/blocks"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
}

func TestFixtureBlockIDsMatchGo(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "fixtures", "memory", "blocks", "catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	var catalog struct {
		Blocks []struct {
			ID                  string `json:"block_id"`
			Collection          string `json:"collection"`
			SourceRoom          string `json:"source_room"`
			SegmentationVersion string `json:"segmentation_version"`
			EmbeddingSpace      string `json:"embedding_space"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal(raw, &catalog); err != nil {
		t.Fatal(err)
	}
	for _, b := range catalog.Blocks {
		got := blocks.BlockID(b.Collection, b.SourceRoom, b.SegmentationVersion, b.EmbeddingSpace)
		if got != b.ID {
			t.Fatalf("%s: got %s want %s", b.SourceRoom, got, b.ID)
		}
	}
}

func TestFixtureViewFingerprintsAndRejects(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "fixtures", "memory", "blocks", "views.json"))
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Views []blocks.View `json:"views"`
		Stale blocks.View   `json:"stale_after_revision"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	for _, v := range payload.Views {
		if err := blocks.AssertFingerprint(v); err != nil {
			t.Fatalf("%s: %v", v.ChannelKey, err)
		}
	}
	if err := blocks.AssertFingerprint(payload.Stale); err == nil {
		t.Fatal("expected stale_scope")
	}
	spaceA := blocks.EmbeddingSpace("openrouter", "qwen/qwen3-embedding-8b", 4096, "l2")
	spaceB := blocks.EmbeddingSpace("openrouter", "other-embed", 4096, "l2")
	bound := []blocks.Block{{ID: "a", EmbeddingSpace: spaceA}}
	add := blocks.Block{ID: "b", EmbeddingSpace: spaceB}
	if _, err := blocks.Attach(blocks.MakeView("ch", []string{"a"}, 1), add, bound); err == nil {
		t.Fatal("expected embedding_space mismatch")
	}
}
