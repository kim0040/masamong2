// Package leaves cuts a room-ordered message stream into search units ("leaves") and renders their
// text. It ports experimental/rag-leaf-parent/segment.py: a leaf never crosses a room boundary, is at
// most `size` messages, and (time_gap split) closes early at an idle gap. The body format — a
// "[room · date · speakers]" prefix plus "label: content" lines — is what lexical search, embeddings
// and the reranker see, so it must stay byte-identical to the sandbox.
package leaves

import (
	"fmt"
	"strings"

	"masamong/internal/memory/corpus"
)

// Leaf is one search unit. Start/End index the room-ordered message slice ([Start, End)).
type Leaf struct {
	DocID          string   `json:"doc_id"`
	Kind           string   `json:"kind"`
	Start          int      `json:"start"`
	End            int      `json:"end"`
	Body           string   `json:"body"`
	Room           string   `json:"source_room_id"`
	ObservationIDs []string `json:"observation_ids"`
	Speakers       []string `json:"speakers"`
	N              int      `json:"n"`
}

// Split modes.
const (
	SplitFixed   = "fixed"    // every `size` messages (sandbox default recipe)
	SplitTimeGap = "time_gap" // `size` messages or an idle gap, whichever comes first
)

// Recipe is the segmentation recipe. It is part of the index identity: a different recipe is a
// different index, never mixed with existing leaves.
type Recipe struct {
	Size       int     `json:"size"`
	Overlap    int     `json:"overlap"`
	Split      string  `json:"split"`
	GapMinutes float64 `json:"gap_minutes"`
	MinSize    int     `json:"min_size"`
	WithPrefix bool    `json:"with_prefix"`
}

// DefaultRecipe: 8 messages or a 30-minute gap (docs/rag/README §5: holdout neutral-to-better than
// fixed 8, and the same rule works for live capture where a leaf must close on idle).
func DefaultRecipe() Recipe {
	return Recipe{Size: 8, Split: SplitTimeGap, GapMinutes: 30, MinSize: 2, WithPrefix: true}
}

// Version is a stable label for the recipe (stored with leaves and in block IDs).
func (r Recipe) Version() string {
	if r.Split == SplitTimeGap {
		return fmt.Sprintf("leaf-v2:%s:%d:gap%g:min%d", r.Split, r.Size, r.GapMinutes, r.MinSize)
	}
	return fmt.Sprintf("leaf-v2:%s:%d:ov%d", SplitFixed, r.Size, r.Overlap)
}

func discordRoomLabel(room string) string {
	parts := strings.Split(room, ":")
	if len(parts) >= 3 && parts[0] == "discord" {
		if parts[1] == "0" {
			return "discord DM channel=" + parts[2]
		}
		return "discord guild=" + parts[1] + " channel=" + parts[2]
	}
	return room
}

// FormatBody renders messages exactly like segment.format_body.
func FormatBody(msgs []corpus.Message, withPrefix bool) string {
	lines := make([]string, len(msgs))
	for i, m := range msgs {
		lines[i] = m.ActorLabel + ": " + m.Content
	}
	body := strings.Join(lines, "\n")
	if !withPrefix || len(msgs) == 0 {
		return body
	}
	room := msgs[0].Room
	if room == "" {
		room = "?"
	}
	prefix := "[" + discordRoomLabel(room)
	for _, m := range msgs {
		if m.ParsedTime != "" {
			prefix += " · " + firstRunes(m.ParsedTime, 10)
			break
		}
	}
	speakers := uniqueSpeakers(msgs)
	if len(speakers) > 0 {
		part := strings.Join(speakers[:min(8, len(speakers))], ", ")
		if len(speakers) > 8 {
			part += ", …"
		}
		prefix += " · " + part
	}
	return prefix + "]\n" + body
}

func firstRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func uniqueSpeakers(msgs []corpus.Message) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range msgs {
		label := m.ActorLabel
		if label == "" {
			label = "?"
		}
		if !seen[label] {
			seen[label] = true
			out = append(out, label)
		}
	}
	return out
}

func newLeaf(kind string, msgs []corpus.Message, start, end int, withPrefix bool) Leaf {
	chunk := msgs[start:end]
	ids := make([]string, len(chunk))
	for i, m := range chunk {
		ids[i] = m.ObservationID
	}
	return Leaf{
		DocID:          fmt.Sprintf("%s:%d-%d", kind, start, end-1),
		Kind:           kind,
		Start:          start,
		End:            end,
		Body:           FormatBody(chunk, withPrefix),
		Room:           chunk[0].Room,
		ObservationIDs: ids,
		Speakers:       uniqueSpeakers(chunk),
		N:              len(chunk),
	}
}

func sliceFixed(msgs []corpus.Message, size, overlap, limit int, kind string, withPrefix bool) []Leaf {
	if size <= 0 || overlap < 0 || overlap >= size || limit <= 0 {
		return nil
	}
	step := size - overlap
	var out []Leaf
	for start := 0; start < len(msgs) && len(out) < limit; start += step {
		end := min(len(msgs), start+size)
		if end <= start {
			break
		}
		if len(out) > 0 && end == out[len(out)-1].End && end-start <= overlap {
			break
		}
		out = append(out, newLeaf(kind, msgs, start, end, withPrefix))
		if end >= len(msgs) {
			break
		}
	}
	return out
}

func sliceTimeGap(msgs []corpus.Message, maxSize int, gapMinutes float64, minSize, limit int, kind string, withPrefix bool) []Leaf {
	if maxSize <= 0 || limit <= 0 {
		return nil
	}
	gapSec := max(0, gapMinutes) * 60
	minSize = max(1, minSize)
	var out []Leaf
	n := len(msgs)
	for start := 0; start < n && len(out) < limit; {
		end := start + 1
		prev, prevOK := corpus.Timestamp(msgs[start].ParsedTime)
		for end < n && end-start < maxSize {
			ts, ok := corpus.Timestamp(msgs[end].ParsedTime)
			if prevOK && ok && ts-prev >= gapSec && end-start >= minSize {
				break
			}
			if ok {
				prev, prevOK = ts, true
			}
			end++
		}
		out = append(out, newLeaf(kind, msgs, start, end, withPrefix))
		start = end
	}
	return out
}

// SlicePerRoom cuts leaves inside each room run of a room-ordered stream (segment.slice_segments_per_room).
// Start/End/DocID are positions in msgs. limit <= 0 returns nothing (sandbox semantics); pass a
// large limit for "all".
func SlicePerRoom(msgs []corpus.Message, r Recipe, limit int, kind string) []Leaf {
	if limit <= 0 {
		return nil
	}
	var out []Leaf
	for i := 0; i < len(msgs) && len(out) < limit; {
		room := msgs[i].Room
		j := i + 1
		for j < len(msgs) && msgs[j].Room == room {
			j++
		}
		var segs []Leaf
		if r.Split == SplitTimeGap {
			segs = sliceTimeGap(msgs[i:j], r.Size, r.GapMinutes, r.MinSize, limit-len(out), kind, r.WithPrefix)
		} else {
			segs = sliceFixed(msgs[i:j], r.Size, r.Overlap, limit-len(out), kind, r.WithPrefix)
		}
		for _, s := range segs {
			s.Start += i
			s.End += i
			s.DocID = fmt.Sprintf("%s:%d-%d", kind, s.Start, s.End-1)
			out = append(out, s)
		}
		i = j
	}
	return out
}
