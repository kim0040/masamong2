package config

import (
	"strings"
	"testing"
)

// memory.message_vectors is on while the field is absent; only an explicit false turns it off, and that
// false is written out and read back (an omitted key would turn the setting back on).
func TestMessageVectorsDefaultOnExplicitOffSurvives(t *testing.T) {
	if !(MemoryFlags{}).MessageVectorsOn() {
		t.Fatal("an unset memory.message_vectors is not on")
	}
	d := Default("a", "/tmp/x")
	if !d.Memory.MessageVectorsOn() {
		t.Fatal("the default document does not have message vectors on")
	}
	b, err := Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "message_vectors") {
		t.Fatalf("an unset field is written:\n%s", b)
	}
	got, err := Parse(b)
	if err != nil || got.Memory.MessageVectors != nil || !got.Memory.MessageVectorsOn() {
		t.Fatalf("unset round trip: %v %v", err, got.Memory.MessageVectors)
	}

	for _, want := range []bool{false, true} {
		v := want
		d.Memory.MessageVectors = &v
		b, err := Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "message_vectors: "+map[bool]string{false: "false", true: "true"}[want]) {
			t.Fatalf("explicit %v is not written:\n%s", want, b)
		}
		got, err := Parse(b)
		if err != nil || got.Memory.MessageVectors == nil || got.Memory.MessageVectorsOn() != want {
			t.Fatalf("explicit %v round trip: %v %v", want, err, got.Memory.MessageVectors)
		}
	}

	// the same through the tree the console and config.patch edit
	d.Memory.MessageVectors = nil
	raw, err := Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := Tree(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := TreeSet(tree, []string{"memory", "message_vectors"}, false); err != nil {
		t.Fatal(err)
	}
	patched, err := FromTree(tree)
	if err != nil {
		t.Fatal(err)
	}
	if doc, err := Parse(patched); err != nil || doc.Memory.MessageVectors == nil || doc.Memory.MessageVectorsOn() {
		t.Fatalf("a patched false is not off: %v %v", err, doc.Memory.MessageVectors)
	}
	TreeUnset(tree, []string{"memory", "message_vectors"})
	patched, err = FromTree(tree)
	if err != nil {
		t.Fatal(err)
	}
	if doc, err := Parse(patched); err != nil || doc.Memory.MessageVectors != nil || !doc.Memory.MessageVectorsOn() {
		t.Fatalf("an unset field is not back to on: %v %v", err, doc.Memory.MessageVectors)
	}
}

// The console form shows the switch as it applies: on when the field is absent.
func TestMessageVectorsSchemaImpliesOn(t *testing.T) {
	n := Schema()
	for _, k := range []string{"memory", "message_vectors"} {
		var next *SchemaNode
		for _, f := range n.Fields {
			if f.Key == k {
				next = f.Node
			}
		}
		if next == nil {
			t.Fatalf("no field %s", k)
		}
		n = next
	}
	if n.Implied != true || !n.Nullable {
		t.Fatalf("memory.message_vectors schema: %+v", n)
	}
}
