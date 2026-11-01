package config

import "testing"

func TestSchemaCarriesWhatTheFormNeeds(t *testing.T) {
	find := func(path ...string) *SchemaNode {
		n := Schema()
		for _, k := range path {
			var next *SchemaNode
			for _, f := range n.Fields {
				if f.Key == k {
					next = f.Node
				}
			}
			if next == nil {
				t.Fatalf("no field %v", path)
			}
			n = next
		}
		return n
	}
	if n := find("participation", "wait_sec"); n.Implied != 45 {
		t.Fatalf("wait_sec implied %v", n.Implied)
	}
	if n := find("participation", "mode"); len(n.Enum) != len(ParticipationModes) {
		t.Fatalf("participation.mode enum %v", n.Enum)
	}
	if n := find("access", "server_approval"); n.Implied != true || !n.Nullable {
		t.Fatalf("server_approval %+v", n)
	}
	if n := find("backup", "hour"); n.Format != "hour" || n.Min == nil || *n.Max != 23 {
		t.Fatalf("backup.hour %+v", n)
	}
	if !find("instance", "data_dir").ReadOnly || !find("memory", "chat_tools").Legacy {
		t.Fatal("data_dir read-only, chat_tools legacy")
	}
	if n := find("retention", "audit_days"); n.Implied != (Retention{}).Days()["audit"] {
		t.Fatalf("audit_days implied %v", n.Implied)
	}
	if n := find("memory", "index_workers"); n.Implied != 8 || *n.Min != 1 || *n.Max != MaxIndexWorkers {
		t.Fatalf("memory.index_workers %+v", n)
	}
}

// memory.index_workers: empty is the old fixed 8; values outside 1..MaxIndexWorkers are brought into it.
func TestIndexWorkers(t *testing.T) {
	for in, want := range map[int]int{0: 8, -3: 8, 1: 1, 16: 16, 32: 32, 100: 100, 256: 256, 1000: 256} {
		if got := (MemoryFlags{IndexWorkers: in}).Workers(); got != want {
			t.Errorf("index_workers %d: %d, want %d", in, got, want)
		}
	}
}
