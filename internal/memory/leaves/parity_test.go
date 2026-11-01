package leaves

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"masamong/internal/memory/corpus"
)

// TestSandboxLeafParity rebuilds leaves from each sandbox index's messages.json with that index's
// recipe and requires byte-identical leaves. Set MASAMONG_SANDBOX_INDEXES to "dataDir=configPath,…".
func TestSandboxLeafParity(t *testing.T) {
	spec := os.Getenv("MASAMONG_SANDBOX_INDEXES")
	if spec == "" {
		t.Skip("MASAMONG_SANDBOX_INDEXES not set")
	}
	for _, pair := range strings.Split(spec, ",") {
		dataDir, cfgPath, _ := strings.Cut(pair, "=")
		t.Run(filepath.Base(dataDir), func(t *testing.T) {
			var cfg struct {
				LeafSize      int     `json:"leaf_size"`
				LeafOverlap   int     `json:"leaf_overlap"`
				LeafLimit     int     `json:"leaf_limit"`
				LeafSplit     string  `json:"leaf_split"`
				GapMinutes    float64 `json:"gap_minutes"`
				ContextPrefix *bool   `json:"context_prefix"`
			}
			mustJSON(t, cfgPath, &cfg)
			var msgs []corpus.Message
			var want []Leaf
			mustJSON(t, filepath.Join(dataDir, "messages.json"), &msgs)
			mustJSON(t, filepath.Join(dataDir, "leaves.json"), &want)
			r := Recipe{Size: cfg.LeafSize, Overlap: cfg.LeafOverlap, Split: cfg.LeafSplit, GapMinutes: cfg.GapMinutes, MinSize: 2, WithPrefix: cfg.ContextPrefix == nil || *cfg.ContextPrefix}
			if r.Split == "" {
				r.Split = SplitFixed
			}
			got := SlicePerRoom(msgs, r, cfg.LeafLimit, "leaf")
			if len(got) != len(want) {
				t.Fatalf("leaves %d want %d", len(got), len(want))
			}
			for i := range got {
				if !reflect.DeepEqual(got[i], want[i]) {
					t.Fatalf("leaf %d differs:\n got %+v\nwant %+v", i, got[i].DocID, want[i].DocID)
				}
			}
			t.Logf("%d leaves identical (%s)", len(got), r.Version())
		})
	}
}

func mustJSON(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatal(err)
	}
}
