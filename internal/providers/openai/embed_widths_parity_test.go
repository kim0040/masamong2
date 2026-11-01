package openai

import (
	"encoding/json"
	"os"
	"testing"
)

// Python's test_mrl_catalogue_matches_go_shared_table reads this same input and expected-output table.
func TestMRLCatalogueMatchesPythonSharedTable(t *testing.T) {
	raw, err := os.ReadFile("testdata/embed_widths_parity.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Model, Reference string
		Width            int
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.Model, func(t *testing.T) {
			width, mrl := EmbedFullWidth(c.Model)
			if ReferenceName(c.Model) != c.Reference || width != c.Width || mrl != (c.Width > 0) {
				t.Fatalf("reference=%q width=%d mrl=%v", ReferenceName(c.Model), width, mrl)
			}
		})
	}
}
