package chat

import (
	"strings"
	"unicode/utf8"

	"masamong/internal/lang"
	"masamong/internal/memory/search"
)

// Room memory in the answer prompt (docs/plan-memory-conversation.md §5.4.6, ADR-17).
//
// The application chooses what the room remembers that bears on this turn — a few related episodes
// (L2) with their evidence lines read live from the raw messages, the room's live topics (L1) linked to
// the caller's conversation, and person cards (L3) — and this file fits them to their budgets and
// renders them with the language pack. The blocks go into the last user message, before the member's
// line, and only when they have content: a turn without room memory sends exactly the prompt the
// sandbox evaluated, and the static system prompt (the cacheable part) never changes.
//
// A memory hit whose leaf belongs to an episode already shown is the same conversation told twice: the
// hit is dropped and the episode's evidence lines stay (they were read live and are short).

// RagCoveredByRoomMemory is the rag state of a turn whose memory hits were all already shown as room
// episodes (the pack's rag note points at the episodes block).
const RagCoveredByRoomMemory = "room_memory"

// Budgets of the room memory blocks, in runes of their lines (§5.4.6).
const (
	EpisodeBudget    = 1200
	MaxEpisodes      = 3
	MaxEvidenceLines = 4
	TopicBudget      = 500
	CallerCardBudget = 300
	OtherCardBudget  = 150
	MaxOtherCards    = 2
	evidenceRunes    = 110
)

// RoomMemory is the room's derived memory the application chose for one turn, best first.
type RoomMemory struct {
	Episodes []RoomEpisode
	Topics   []RoomTopic
	Cards    []RoomCard // the caller's first (Caller true), then up to MaxOtherCards others
	// Packs are the languages whose instruction cues drop a line that reads as an instruction to the bot
	// (the reply's and the memory's); empty = the voice's pack (cleanRoomMemory).
	Packs []*lang.Pack
}

// RoomEpisode is one episode with its live evidence. Status and Source are already words of the pack
// (empty when not shown); Leaves are the memory leaves it is linked to (episode_leaf).
type RoomEpisode struct {
	ID       string
	Title    string
	Status   string
	When     string
	Source   string
	Evidence []RoomLine
	Leaves   []string
}

// RoomLine is one evidence line as it stands now.
type RoomLine struct {
	Date, Author, Text string
}

// RoomTopic is one live topic of the room.
type RoomTopic struct {
	Label string
	Gist  string
	Who   []string
}

// RoomCard is what the bot keeps about one person of this server.
type RoomCard struct {
	Name   string
	Facts  []string
	Caller bool
}

// RoomTrace is the body-free record of the room memory a reply was given: episode IDs, how many topics
// and cards, their sizes, and how many memory hits were dropped as duplicates.
type RoomTrace struct {
	Episodes     []string `json:"episodes,omitempty"`
	EpisodeChars int      `json:"episode_chars,omitempty"`
	Topics       int      `json:"topics,omitempty"`
	TopicChars   int      `json:"topic_chars,omitempty"`
	Cards        int      `json:"cards,omitempty"`
	CardChars    int      `json:"card_chars,omitempty"`
	DroppedHits  int      `json:"dropped_hits,omitempty"`
}

// Empty reports whether nothing was given.
func (t RoomTrace) Empty() bool {
	return len(t.Episodes) == 0 && t.Topics == 0 && t.Cards == 0 && t.DroppedHits == 0
}

func runes(s string) int { return utf8.RuneCountInString(s) }

func clipRunes(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:max(1, n-1)]) + "…"
}

// episodeLines renders one episode with at most n evidence lines.
func episodeLines(e RoomEpisode, n int) []string {
	var meta []string
	for _, m := range []string{e.Status, e.When, e.Source} {
		if m = strings.TrimSpace(m); m != "" {
			meta = append(meta, m)
		}
	}
	head := "- " + clipRunes(e.Title, 120)
	if len(meta) > 0 {
		head += " (" + strings.Join(meta, ", ") + ")"
	}
	out := []string{head}
	for i, l := range e.Evidence {
		if i == n {
			break
		}
		line := "  · "
		if l.Date != "" {
			line += l.Date + " "
		}
		out = append(out, line+l.Author+": "+clipRunes(l.Text, evidenceRunes))
	}
	return out
}

func sumRunes(lines []string) int {
	n := 0
	for _, l := range lines {
		n += runes(l) + 1
	}
	return n
}

// fitEpisodes keeps the episodes that fit EpisodeBudget (at most MaxEpisodes), each with as many of its
// evidence lines (at most MaxEvidenceLines, at least one) as fit.
func fitEpisodes(eps []RoomEpisode) (lines []string, shown []RoomEpisode) {
	used := 0
	for _, e := range eps {
		if len(shown) == MaxEpisodes {
			break
		}
		if len(e.Evidence) == 0 {
			continue // an episode is shown with what it rests on, never alone
		}
		var fit []string
		for n := min(MaxEvidenceLines, len(e.Evidence)); n >= 1; n-- {
			if l := episodeLines(e, n); used+sumRunes(l) <= EpisodeBudget {
				fit = l
				break
			}
		}
		if fit == nil {
			break
		}
		used += sumRunes(fit)
		lines = append(lines, fit...)
		shown = append(shown, e)
	}
	return lines, shown
}

func fitTopics(topics []RoomTopic) []string {
	var lines []string
	used := 0
	for _, t := range topics {
		line := "- " + clipRunes(t.Label, 60)
		if len(t.Who) > 0 {
			line += " (" + strings.Join(t.Who, ", ") + ")"
		}
		if g := strings.TrimSpace(t.Gist); g != "" {
			line += ": " + clipRunes(g, 140)
		}
		if used+runes(line)+1 > TopicBudget {
			break
		}
		used += runes(line) + 1
		lines = append(lines, line)
	}
	return lines
}

func fitCards(cards []RoomCard) []string {
	var lines []string
	others := 0
	for _, c := range cards {
		budget := OtherCardBudget
		if c.Caller {
			budget = CallerCardBudget
		} else {
			if others == MaxOtherCards {
				continue
			}
			others++
		}
		line := "- " + c.Name + ":"
		n := 0
		for _, f := range c.Facts {
			f = strings.TrimSpace(f)
			sep := " "
			if n > 0 {
				sep = "; "
			}
			if f == "" || runes(line)+runes(sep)+runes(f) > budget {
				continue
			}
			line += sep + f
			n++
		}
		if n > 0 {
			lines = append(lines, line)
		}
	}
	return lines
}

// dropShownLeaves removes memory hits whose leaf (or a leaf merged into it) belongs to a shown episode.
func dropShownLeaves(hits []search.Hit, shown []RoomEpisode) ([]search.Hit, int) {
	leaves := map[string]bool{}
	for _, e := range shown {
		for _, l := range e.Leaves {
			leaves[l] = true
		}
	}
	if len(leaves) == 0 || len(hits) == 0 {
		return hits, 0
	}
	kept := hits[:0:0]
	for _, h := range hits {
		dup := leaves[h.DocID]
		for _, m := range h.MergedLeafIDs {
			dup = dup || leaves[m]
		}
		if !dup {
			kept = append(kept, h)
		}
	}
	return kept, len(hits) - len(kept)
}

// renderRoomMemory fits the room memory to its budgets, drops the memory hits it already covers and
// renders the blocks. It returns "" when there is nothing to add.
func renderRoomMemory(v Voice, rm *RoomMemory, hits []search.Hit) (string, []search.Hit, RoomTrace) {
	var tr RoomTrace
	if rm == nil {
		return "", hits, tr
	}
	rm = cleanRoomMemory(v, rm)
	m := v.pack().Memory
	var blocks []string
	if lines := fitCards(rm.Cards); len(lines) > 0 && m.CardBlock != "" {
		body := strings.Join(lines, "\n")
		tr.Cards, tr.CardChars = len(lines), runes(body)
		blocks = append(blocks, lang.Fill(m.CardBlock, "header", m.ReadHeader, "cards", body))
	}
	if lines, shown := fitEpisodes(rm.Episodes); len(lines) > 0 && m.EpisodesBlock != "" {
		body := strings.Join(lines, "\n")
		tr.EpisodeChars = runes(body)
		for _, e := range shown {
			tr.Episodes = append(tr.Episodes, e.ID)
		}
		hits, tr.DroppedHits = dropShownLeaves(hits, shown)
		blocks = append(blocks, lang.Fill(m.EpisodesBlock, "header", m.ReadHeader, "episodes", body))
	}
	if lines := fitTopics(rm.Topics); len(lines) > 0 && m.StateBlock != "" {
		body := strings.Join(lines, "\n")
		tr.Topics, tr.TopicChars = len(lines), runes(body)
		blocks = append(blocks, lang.Fill(m.StateBlock, "header", m.ReadHeader, "topics", body))
	}
	return strings.Join(blocks, "\n"), hits, tr
}

// withRoomMemory places the room memory blocks before the template's last paragraph (the member's line),
// leaving the template untouched when there are none.
func withRoomMemory(tpl, block string) string {
	if strings.TrimSpace(block) == "" {
		return tpl
	}
	if i := strings.LastIndex(tpl, "\n\n"); i >= 0 {
		return tpl[:i] + "\n\n{room_memory}" + tpl[i:]
	}
	return "{room_memory}\n\n" + tpl
}
