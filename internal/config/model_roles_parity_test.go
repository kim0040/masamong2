package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestModelRolesMatchSandboxContract(t *testing.T) {
	dir, _ := os.Getwd()
	var root string
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			root = dir
			break
		}
		dir = filepath.Dir(dir)
	}
	raw, err := os.ReadFile(filepath.Join(root, "contracts", "sandbox", "tool-parity.json"))
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Models struct {
			Roles []string `json:"roles"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	if len(spec.Models.Roles) != len(ModelRoles) {
		t.Fatalf("model roles drift: contract=%v go=%v", spec.Models.Roles, ModelRoles)
	}
	for i, r := range spec.Models.Roles {
		if ModelRoles[i] != r {
			t.Fatalf("model roles drift at %d: contract=%s go=%s", i, r, ModelRoles[i])
		}
	}
	for _, r := range ModelRoles {
		if !ValidModelRole(r) {
			t.Fatal(r)
		}
	}
	if ValidModelRole("llm") {
		t.Fatal("llm is a sandbox legacy alias, not a product role")
	}
}
