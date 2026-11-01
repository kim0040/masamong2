package openai

import "testing"

// Only models hosts.json names are cut; the lookup ignores the vendor prefix, case and a ':variant'.
func TestEmbedFullWidth(t *testing.T) {
	for model, want := range map[string]int{
		"qwen/qwen3-embedding-8b": 4096, "Qwen3-Embedding-4B": 2560, "qwen3-embedding-0.6b": 1024,
		"openai/text-embedding-3-large": 3072, "text-embedding-3-small": 1536, "models/gemini-embedding-001": 3072,
	} {
		if got, ok := EmbedFullWidth(model); !ok || got != want {
			t.Errorf("%s = %d %v, want %d", model, got, ok, want)
		}
	}
	for _, model := range []string{"", "text-embedding-ada-002", "baai/bge-m3", "qwen/qwen3-embedding", "gemini-embedding-2"} {
		if got, ok := EmbedFullWidth(model); ok {
			t.Errorf("%q is not known to be cuttable, got %d", model, got)
		}
	}
}
