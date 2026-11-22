package catalog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"masamong/internal/testutil"
)

func TestSandboxToolParityIDsInCatalog(t *testing.T) {
	root := testutil.RepoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "contracts", "sandbox", "tool-parity.json"))
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		AgentTools []struct {
			ID string `json:"id"`
		} `json:"agent_tools"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	if len(spec.AgentTools) == 0 {
		t.Fatal("tool-parity.json has no agent_tools")
	}
	for _, row := range spec.AgentTools {
		if _, ok := Lookup(row.ID); !ok {
			t.Fatalf("sandbox tool %s missing from product catalog", row.ID)
		}
	}
}
