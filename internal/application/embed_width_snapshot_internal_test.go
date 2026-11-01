package application

import (
	"sync"
	"testing"

	"masamong/internal/action"
	"masamong/internal/config"
)

// The console builds its confirmation basis (revision, model, provider, width) from providers.list's
// embed_width alone, so that object must describe one committed document.
func TestProvidersListEmbedWidthNamesItsRevisionModelAndProvider(t *testing.T) {
	t.Setenv("MASAMONG_OPENROUTER_KEY", "fixture-only")
	s := privacyService(t)
	widthConfig(t, s, "qwen/qwen3-embedding-8b", 1024)
	r := widthExecute(s, "providers.list", nil)
	info, ok := r.Output["embed_width"].(map[string]any)
	if !r.OK || !ok {
		t.Fatal("providers.list must carry embed_width")
	}
	doc := s.Doc()
	if info["revision"] != s.Revision() || info["revision"].(int) <= 0 {
		t.Fatalf("revision = %v, want the committed revision %d", info["revision"], s.Revision())
	}
	if info["provider"] != doc.Models["embed"].Provider || info["model"] != "qwen/qwen3-embedding-8b" || info["dimensions"] != 1024 {
		t.Fatalf("provider/model/dimensions = %v/%v/%v", info["provider"], info["model"], info["dimensions"])
	}
	before := info["revision"].(int)
	widthConfig(t, s, "openai/text-embedding-3-large", 0)
	info = widthExecute(s, "providers.list", nil).Output["embed_width"].(map[string]any)
	if info["revision"].(int) <= before || info["model"] != "openai/text-embedding-3-large" || info["full_width"] != 3072 {
		t.Fatalf("a later save must show its own revision with its own model: %v", info)
	}
}

// Two documents that differ in embed model, width and rerank usability are swapped while the info is read.
// Every read must be one of them whole, never the model of one with the rerank judgment of the other, and one
// revision must never name two different documents.
func TestEmbedWidthInfoIsOneSnapshotWhileConfigChanges(t *testing.T) {
	t.Setenv("MASAMONG_OPENROUTER_KEY", "fixture-only")
	s := privacyService(t)
	const modelA, modelB = "qwen/qwen3-embedding-8b", "openai/text-embedding-3-large"
	usable := map[string]bool{modelA: true, modelB: false}
	dims := map[string]int{modelA: 1024, modelB: 0}
	apply := func(model string) {
		r := s.mutateConfig(action.Request{}, func(d *config.Document) error {
			e, rr := d.Models["embed"], d.Models["rerank"]
			e.Model, e.Dimensions = model, dims[model]
			rr.Model = map[bool]string{true: "voyageai/rerank-2.5-lite", false: ""}[usable[model]]
			d.Models["embed"], d.Models["rerank"] = e, rr
			return nil
		})
		if !r.OK {
			t.Error(r.Error)
		}
	}
	apply(modelA)
	var wg sync.WaitGroup
	done := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(done)
		for i := 0; i < 30; i++ {
			apply([]string{modelB, modelA}[i%2])
		}
	}()
	byRevision := map[int]string{}
	reads := 0
	for stop := false; !stop; {
		select {
		case <-done:
			stop = true
		default:
		}
		info := s.embedWidthInfo()
		model, _ := info["model"].(string)
		if info["rerank_usable"] != usable[model] || info["dimensions"] != dims[model] {
			t.Fatalf("mixed snapshot: %v", info)
		}
		rev := info["revision"].(int)
		if prev, seen := byRevision[rev]; seen && prev != model {
			t.Fatalf("revision %d named both %s and %s", rev, prev, model)
		}
		byRevision[rev] = model
		reads++
	}
	wg.Wait()
	if reads == 0 {
		t.Fatal("no read happened")
	}
}
