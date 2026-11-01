package catalog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSandboxToolParityIDsInCatalog(t *testing.T) {
	root := findRepoRoot(t)
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

func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("go.mod not found")
	return ""
}
