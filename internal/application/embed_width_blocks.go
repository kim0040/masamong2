package application

import (
	"fmt"
	"slices"
	"strings"
	"sync"

	"masamong/internal/memory/blocks"
	"masamong/internal/memory/engine"
	"masamong/internal/providers/openai"
)

// Width aliases preserve channel references on both models.embed_width and
// config.patch, including after restart. We resolve rather than rewrite IDs:
// changing a width must not create a second configuration revision or erase
// preparation records. Model and recipe identities remain strict, so switching
// models keeps the existing reindex behavior.
func blockSpaceModel(b blocks.Block) string {
	if parts := strings.Split(b.EmbeddingSpace, "|"); len(parts) == 4 {
		return parts[1]
	}
	return engine.SpaceModel(b.EmbeddingSpace)
}

func addBlockWidthAliases(aliases map[string]int, b blocks.Block, index, currentWidth int) {
	widths := []int{0, 256, 512, 768, 1024, 1536, 2048, 2560, 3072, 4096, currentWidth}
	if full, ok := openai.EmbedFullWidth(blockSpaceModel(b)); ok {
		widths = append(widths, full)
	}
	slices.Sort(widths)
	widths = slices.Compact(widths)
	parts := strings.Split(b.EmbeddingSpace, "|")
	model := blockSpaceModel(b)
	for _, dims := range widths {
		space := b.EmbeddingSpace
		if len(parts) == 4 {
			space = blocks.EmbeddingSpace(parts[0], parts[1], dims, parts[3])
		} else if strings.HasSuffix(space, ":l2") {
			space = fmt.Sprintf("%s:%d:l2", model, dims)
		} else {
			continue
		}
		id := blocks.BlockID(b.Collection, b.SourceRoom, b.SegmentationVersion, space)
		if _, exists := aliases[id]; !exists {
			aliases[id] = index
		}
	}
}

type embedWidthAliasCache struct {
	mu           sync.Mutex
	storedIDs    []string
	declaredIDs  []string
	currentWidth int
	aliases      map[string]int
}

// Cache only alias positions, not block data: status and labels come from each
// fresh dbBlocks result. Width/model/recipe, room additions/deletions and channel
// declarations all change the ID lists, making invalidation independent of
// configuration revision history.
func (s *Service) blockWidthAliasIndex(stored, declared []blocks.Block, id string) (int, bool) {
	c := &s.embedWidthAliases
	c.mu.Lock()
	defer c.mu.Unlock()
	currentWidth := s.Doc().Models["embed"].Dimensions
	if c.aliases == nil || c.currentWidth != currentWidth ||
		!blockIDsEqual(stored, c.storedIDs) || !blockIDsEqual(declared, c.declaredIDs) {
		c.aliases = make(map[string]int, (len(stored)+len(declared))*12)
		c.storedIDs = make([]string, len(stored))
		c.declaredIDs = make([]string, len(declared))
		c.currentWidth = currentWidth
		for i, b := range stored {
			c.storedIDs[i] = b.ID
			addBlockWidthAliases(c.aliases, b, i, currentWidth)
			if _, exists := c.aliases[b.ID]; !exists {
				c.aliases[b.ID] = i
			}
		}
		for i, b := range declared {
			c.declaredIDs[i] = b.ID
			addBlockWidthAliases(c.aliases, b, len(stored)+i, currentWidth)
			if _, exists := c.aliases[b.ID]; !exists {
				c.aliases[b.ID] = len(stored) + i
			}
		}
	}
	index, ok := c.aliases[id]
	return index, ok
}

func blockIDsEqual(items []blocks.Block, ids []string) bool {
	if len(items) != len(ids) {
		return false
	}
	for i, b := range items {
		if b.ID != ids[i] {
			return false
		}
	}
	return true
}

func validBlockID(id string) bool {
	if len(id) != 16 {
		return false
	}
	for _, c := range []byte(id) {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
			return false
		}
	}
	return true
}

func sameBlockPreparation(a, b blocks.Block) bool {
	if a.SourceRoom != b.SourceRoom || a.Collection != b.Collection || a.SegmentationVersion != b.SegmentationVersion {
		return false
	}
	if a.EmbeddingSpace == b.EmbeddingSpace {
		return true
	}
	// Prepared DB blocks use the engine space (models may contain colons).
	// Declared channels use provider|model|width|normalization instead.
	ap, bp := strings.Split(a.EmbeddingSpace, "|"), strings.Split(b.EmbeddingSpace, "|")
	if len(ap) == 4 && len(bp) == 4 {
		return ap[0] == bp[0] && ap[1] == bp[1] && ap[3] == bp[3]
	}
	return strings.HasSuffix(a.EmbeddingSpace, ":l2") && strings.HasSuffix(b.EmbeddingSpace, ":l2") &&
		engine.SpaceModel(a.EmbeddingSpace) == engine.SpaceModel(b.EmbeddingSpace)
}
