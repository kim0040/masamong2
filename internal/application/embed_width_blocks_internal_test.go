package application

import (
	"testing"

	"masamong/internal/memory/blocks"
)

func TestValidBlockID(t *testing.T) {
	for id, want := range map[string]bool{
		"0123456789abcdef":  true,
		"0123456789ABCDEF":  true,
		"fedcba9876543210":  true,
		"0123456789abcde":   false, // 15
		"0123456789abcdef0": false, // 17
		"0123456789abcdeg":  false, // not hex
		"":                  false,
		"0123456789abcde f": false,
	} {
		if got := validBlockID(id); got != want {
			t.Errorf("validBlockID(%q) = %v", id, got)
		}
	}
}

func TestBlockIDsEqual(t *testing.T) {
	items := []blocks.Block{{ID: "a"}, {ID: "b"}}
	if !blockIDsEqual(items, []string{"a", "b"}) {
		t.Fatal("same order, same ids")
	}
	if blockIDsEqual(items, []string{"b", "a"}) {
		t.Fatal("order matters")
	}
	if blockIDsEqual(items, []string{"a"}) {
		t.Fatal("length matters")
	}
	if !blockIDsEqual(nil, nil) {
		t.Fatal("two empty lists are equal")
	}
}

func TestBlockSpaceModel(t *testing.T) {
	if got := blockSpaceModel(blocks.Block{EmbeddingSpace: "fakeprov|fakemodel|512|l2"}); got != "fakemodel" {
		t.Fatalf("declared space model %q", got)
	}
	// Engine spaces are "model:dims:l2" and the model itself may hold colons.
	if got := blockSpaceModel(blocks.Block{EmbeddingSpace: "fakemodel:512:l2"}); got != "fakemodel" {
		t.Fatalf("engine space model %q", got)
	}
	if got := blockSpaceModel(blocks.Block{EmbeddingSpace: "ns:fake:512:l2"}); got != "ns:fake" {
		t.Fatalf("engine space model with a colon %q", got)
	}
}

func TestSameBlockPreparation(t *testing.T) {
	base := blocks.Block{
		SourceRoom:          "room1",
		Collection:          "col",
		SegmentationVersion: "v3",
		EmbeddingSpace:      "fakeprov|fakemodel|512|l2",
	}
	if !sameBlockPreparation(base, base) {
		t.Fatal("the same block is a different preparation")
	}
	// A declared space differs from a stored one only in width.
	other := base
	other.EmbeddingSpace = "fakeprov|fakemodel|1024|l2"
	if !sameBlockPreparation(base, other) {
		t.Fatal("a width change is not a new preparation")
	}
	other.EmbeddingSpace = "fakeprov|othermodel|512|l2"
	if sameBlockPreparation(base, other) {
		t.Fatal("a different model passes as the same preparation")
	}
	other.EmbeddingSpace = "fakeprov|fakemodel|512|cosine"
	if sameBlockPreparation(base, other) {
		t.Fatal("a different normalization passes as the same preparation")
	}
	other = base
	other.SourceRoom = "room2"
	if sameBlockPreparation(base, other) {
		t.Fatal("a different room passes as the same preparation")
	}
	// Engine-style spaces compare by model too.
	a := base
	a.EmbeddingSpace = "fakemodel:512:l2"
	b := base
	b.EmbeddingSpace = "fakemodel:1024:l2"
	if !sameBlockPreparation(a, b) {
		t.Fatal("engine spaces differing only in width are a new preparation")
	}
	b.EmbeddingSpace = "othermodel:512:l2"
	if sameBlockPreparation(a, b) {
		t.Fatal("engine spaces with different models pass as the same preparation")
	}
}

func TestAddBlockWidthAliases(t *testing.T) {
	b := blocks.Block{
		Collection:          "col",
		SourceRoom:          "room1",
		SegmentationVersion: "v3",
		EmbeddingSpace:      "fakeprov|fakemodel|512|l2",
	}
	aliases := map[string]int{}
	addBlockWidthAliases(aliases, b, 7, 512)
	// Every canonical width of the same model points at this block's index.
	for _, dims := range []int{0, 256, 512, 1024, 2048, 4096} {
		id := blocks.BlockID("col", "room1", "v3", blocks.EmbeddingSpace("fakeprov", "fakemodel", dims, "l2"))
		if got, ok := aliases[id]; !ok || got != 7 {
			t.Fatalf("width %d alias missing or wrong: %v %v", dims, got, ok)
		}
	}
	strange := blocks.BlockID("col", "room1", "v3", "fakeprov|fakemodel|333|l2")
	if _, ok := aliases[strange]; ok {
		t.Fatal("a non-canonical width got an alias")
	}
	// A block with no usable space adds nothing.
	aliases = map[string]int{}
	addBlockWidthAliases(aliases, blocks.Block{Collection: "c", SourceRoom: "r", SegmentationVersion: "v", EmbeddingSpace: "opaque"}, 3, 512)
	if len(aliases) != 0 {
		t.Fatalf("an opaque space got aliases: %v", aliases)
	}
}
