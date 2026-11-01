package application

import (
	"maps"
	"slices"
	"strconv"
	"strings"

	"masamong/internal/action"
	"masamong/internal/config"
	"masamong/internal/errcode"
	"masamong/internal/memory/engine"
	"masamong/internal/memory/store"
	"masamong/internal/providers/openai"
)

// Widths below 1024 also change the message-vector space and trigger embedding;
// the no-API narrowing contract therefore exposes only leaf widths >= 1024.
var embedNarrowWidths = []int{1024, 2048}

// effectiveEmbedWidth also respects MemoryEngine's legacy 4096 -> native-space
// mapping. A zero result means the native width is unknown, not an empty vector.
func effectiveEmbedWidth(m config.Model) int {
	if m.Dimensions == 0 || m.Dimensions == 4096 {
		full, _ := openai.EmbedFullWidth(m.Model)
		return full
	}
	return m.Dimensions
}

// embedWidthInfo is computed from one configuration snapshot, so the revision, the model, the stored
// width and the rerank judgment it reports all belong to the same committed document. The console
// takes its confirmation basis (and the base_revision it sends back) from this one object.
func (s *Service) embedWidthInfo() map[string]any {
	snap := s.snapshot()
	m := snap.doc.Models["embed"]
	full, known := openai.EmbedFullWidth(m.Model)
	widths := allowedEmbedWidths(full)
	recommended := 0
	if slices.Contains(widths, config.DefaultEmbedDimensions) {
		recommended = config.DefaultEmbedDimensions
	}
	return map[string]any{"revision": snap.rev, "provider": m.Provider, "model": m.Model,
		"dimensions": m.Dimensions, "effective": effectiveEmbedWidth(m),
		"full_width": full, "truncatable": known, "widths": widths, "recommended": recommended,
		"rerank_usable": rerankUsableIn(snap.reg, snap.doc)}
}

// rerankUsable judges the document's rerank role the way the runtime and the recommendations do
// (engine.RerankUsable): the provider must be configured and offer /rerank, and a model must be named.
func (s *Service) rerankUsable(doc config.Document) bool {
	return rerankUsableIn(s.Registry(), doc)
}

func rerankUsableIn(reg *Registry, doc config.Document) bool {
	if reg == nil {
		return false
	}
	rerank := doc.Models["rerank"]
	return engine.RerankUsable(reg.LLM[rerank.Provider], rerank.Model)
}

func allowedEmbedWidths(full int) []int {
	widths := []int{0}
	for _, width := range embedNarrowWidths {
		if width < full {
			widths = append(widths, width)
		}
	}
	return widths
}

// modelsEmbedWidth uses the ordinary configuration CAS/revision path. Holding
// cfgMu also covers the comparison and confirmation: a concurrent model change
// must not turn a confirmed narrowing into an unconfirmed widening.
func (s *Service) modelsEmbedWidth(req action.Request, in map[string]any) action.Result {
	dims, err := strconv.Atoi(strVal(in, "dimensions", ""))
	if err != nil {
		return action.Fail(req, errcode.InvalidInput, "models.embed_width_invalid", s.msg(req, "models.embed_width_invalid"), nil)
	}
	// direction is the optional "lower" | "raise" the operator was shown and approved; the server checks it against
	// its own judgment at request time. A caller that leaves it out (the CLI) keeps the plain behavior.
	direction := strings.TrimSpace(strVal(in, "direction", ""))
	if direction != "" && direction != "lower" && direction != "raise" {
		return action.Fail(req, errcode.InvalidInput, "models.embed_width_invalid", s.msg(req, "models.embed_width_invalid"), nil)
	}
	var indexed map[string]store.RoomText
	var model config.Model
	raising := false
	res := func() action.Result {
		s.cfgMu.Lock()
		defer s.cfgMu.Unlock()
		live := s.snapshot()
		model = live.doc.Models["embed"]
		full, known := openai.EmbedFullWidth(model.Model)
		if dims != 0 && !known {
			return action.Fail(req, errcode.InvalidInput, "models.embed_width_unsupported", s.msg(req, "models.embed_width_unsupported"), nil)
		}
		if !slices.Contains(allowedEmbedWidths(full), dims) {
			return action.Fail(req, errcode.InvalidInput, "models.embed_width_invalid", s.msg(req, "models.embed_width_invalid"), nil)
		}
		next := model
		next.Dimensions = dims
		previousEffective, nextEffective := effectiveEmbedWidth(model), effectiveEmbedWidth(next)
		out := map[string]any{"dimensions": model.Dimensions, "previous": model.Dimensions, "adapted": false, "reembed": false}
		if previousEffective == nextEffective {
			out["changed"] = false
			r := action.OK(req, out)
			r.Revision = live.rev
			return r
		}
		// Restoring the native width of an unknown legacy model is conservative:
		// its full width cannot be compared numerically, but may require embedding.
		// An oversized non-native space also cannot adapt to :0:l2: the engine's
		// native-width path skips shrinking, so this needs a fresh embedding.
		raising = nextEffective > previousEffective || (!known && dims == 0) ||
			(dims == 0 && model.Dimensions != 0 && model.Dimensions != 4096 && model.Dimensions > full)
		// What the operator approved is a direction. If the stored width moved meanwhile so that this request now goes
		// the other way (a confirmation for a cut would widen, or the reverse), nothing is changed: the console
		// reads again and asks again.
		if actual := map[bool]string{true: "raise", false: "lower"}[raising]; direction != "" && direction != actual {
			return action.Fail(req, errcode.ConfigConflict, "models.embed_width_direction_changed", s.msg(req, "models.embed_width_direction_changed"), map[string]any{"expected": direction, "actual": actual, "revision": live.rev})
		}
		if raising {
			indexed = s.embeddedRooms()
			if len(indexed) > 0 && !boolVal(in, "include_embed") {
				return embedReindexRequired(req, indexed)
			}
		}
		doc := live.doc
		doc.Models = maps.Clone(doc.Models)
		doc.Models["embed"] = next
		raw, err := config.Marshal(doc)
		if err != nil {
			return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
		}
		base := live.rev
		if req.BaseRevision > 0 {
			base = req.BaseRevision
		}
		r := s.applyCandidateLocked(req, raw, base)
		if r.OK {
			r.Output["dimensions"], r.Output["previous"] = dims, model.Dimensions
			r.Output["adapted"], r.Output["reembed"] = !raising, raising
		}
		return r
	}()
	if res.OK && raising && boolVal(in, "reindex") && len(indexed) > 0 {
		res.Output["reindex_jobs"] = s.reindexAll(indexed, model.Model, model.Provider)
	}
	return res
}
