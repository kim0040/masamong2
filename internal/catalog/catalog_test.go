package catalog

import "testing"

func TestDeferredActionsHaveNoExecutePath(t *testing.T) {
	// memory.search / imports.start / memory.reindex were promoted on 2026-09-23 (docs/rag/README §3).
	// participation and reminders (work order 12) were built on 2026-09-28; participation starts off and
	// goes through shadow mode, so nothing is left deferred on RAG
	ragDeferred := []string{}
	implementationDeferred := []string{}
	operatorDisabled := []string{"storage.migrate"}
	want := make(map[string]bool, len(ragDeferred)+len(implementationDeferred)+len(operatorDisabled))
	for _, id := range operatorDisabled {
		want[id] = true
		if d, ok := Lookup(id); !ok || d.Availability != Deferred || d.DeferredReason != OperatorDisabled || IsExecutable(id) {
			t.Fatalf("%s must be disabled by the operator, got %+v", id, d)
		}
	}
	for _, id := range ragDeferred {
		want[id] = true
		d, ok := Lookup(id)
		if !ok {
			t.Fatalf("missing catalog entry %s", id)
		}
		if d.Availability != Deferred || d.DeferredReason != RAGDeferred {
			t.Fatalf("%s must be deferred rag_dependency_deferred, got %+v", id, d)
		}
		if IsExecutable(id) {
			t.Fatalf("%s must not be executable", id)
		}
	}
	for _, id := range implementationDeferred {
		want[id] = true
		d, ok := Lookup(id)
		if !ok || d.Availability != Deferred || d.DeferredReason != ImplementationDeferred {
			t.Fatalf("%s must be implementation-deferred, got %+v", id, d)
		}
	}
	for _, d := range All() {
		if d.Availability == Deferred && !want[d.ID] {
			t.Fatalf("unexpected deferred action %s; update product status documentation", d.ID)
		}
	}
}

func TestExecutableNonRAG(t *testing.T) {
	for _, id := range []string{"participation.mode", "reminders.propose", "reminders.update", "setup.resume", "config.apply", "weather.query", "polls.create", "runtime.doctor", "backup.create", "jobs.watch"} {
		if !IsExecutable(id) {
			t.Fatalf("%s should be executable", id)
		}
	}
}

func TestAllIDsUnique(t *testing.T) {
	seen := map[string]struct{}{}
	for _, d := range All() {
		if _, ok := seen[d.ID]; ok {
			t.Fatalf("duplicate action id %s", d.ID)
		}
		seen[d.ID] = struct{}{}
	}
}

func TestPromotedMemoryActionsExecutable(t *testing.T) {
	for _, id := range []string{"memory.blocks.prepare", "messages.search", "memory.search", "imports.start", "memory.reindex", "chat.respond", "memory.review", "people.merge", "eval.run"} {
		if !IsExecutable(id) {
			t.Fatalf("%s should be executable after the 2026-09-23 promotion", id)
		}
	}
}
