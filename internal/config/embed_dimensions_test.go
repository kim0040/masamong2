package config

import "testing"

func TestDefaultEmbeddingDimensions(t *testing.T) {
	d := Default("test", t.TempDir())
	if DefaultEmbedDimensions != 1024 {
		t.Fatalf("default embedding dimension constant = %d, want 1024", DefaultEmbedDimensions)
	}
	if got := d.Models["embed"].Dimensions; got != DefaultEmbedDimensions {
		t.Fatalf("default embedding dimensions = %d, want %d", got, DefaultEmbedDimensions)
	}
}
