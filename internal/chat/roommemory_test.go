package chat

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"masamong/internal/memory/engine"
	"masamong/internal/memory/search"
)

func longEpisode(id string, n int) RoomEpisode {
	e := RoomEpisode{ID: id, Title: "토요일 등산 " + id, Status: "정해짐", Leaves: []string{"leaf-" + id}}
	for i := 0; i < n; i++ {
		e.Evidence = append(e.Evidence, RoomLine{Date: "10-04", Author: "철수", Text: strings.Repeat("등산 얘기 ", 30)})
	}
	return e
}

// Episodes are capped at three and the block's lines at the budget; each keeps at least one evidence
// line and at most four; an episode without live evidence is never shown.
func TestRoomEpisodesAreCapped(t *testing.T) {
	eps := []RoomEpisode{{ID: "none", Title: "근거 없음"}, longEpisode("a", 6), longEpisode("b", 6), longEpisode("c", 6), longEpisode("d", 6)}
	lines, shown := fitEpisodes(eps)
	if len(shown) == 0 || len(shown) > MaxEpisodes || shown[0].ID != "a" {
		t.Fatalf("shown: %+v", shown)
	}
	if n := sumRunes(lines); n > EpisodeBudget {
		t.Fatalf("block takes %d runes, budget %d", n, EpisodeBudget)
	}
	evidence := 0
	for _, l := range lines {
		if strings.HasPrefix(l, "  · ") {
			evidence++
			if utf8.RuneCountInString(l) > evidenceRunes+20 {
				t.Fatalf("evidence line not clipped: %q", l)
			}
		}
	}
	if evidence > MaxEvidenceLines*len(shown) || evidence < len(shown) {
		t.Fatalf("evidence lines %d for %d episodes", evidence, len(shown))
	}
	small := []RoomEpisode{{ID: "s", Title: "작은 일", Evidence: []RoomLine{{Author: "영희", Text: "1"}, {Author: "영희", Text: "2"}, {Author: "영희", Text: "3"}, {Author: "영희", Text: "4"}, {Author: "영희", Text: "5"}}}}
	if lines, _ := fitEpisodes(small); len(lines) != 1+MaxEvidenceLines {
		t.Fatalf("at most four evidence lines: %v", lines)
	}
}

// A memory hit on a leaf of a shown episode is dropped; the others stay; the trace names the episode.
func TestRoomMemoryDropsDuplicateHits(t *testing.T) {
	rm := &RoomMemory{Episodes: []RoomEpisode{{ID: "ep1", Title: "등산", Evidence: []RoomLine{{Author: "철수", Text: "등산 가자"}}, Leaves: []string{"L1"}}}}
	hits := []search.Hit{{Rank: 1, DocID: "L1", Excerpt: "철수: 등산 가자"}, {Rank: 2, DocID: "L2", MergedLeafIDs: []string{"L1"}}, {Rank: 3, DocID: "L3", Excerpt: "영희: 다른 얘기"}}
	block, kept, tr := renderRoomMemory(koVoice(), rm, hits)
	if len(kept) != 1 || kept[0].DocID != "L3" || tr.DroppedHits != 2 {
		t.Fatalf("kept %+v trace %+v", kept, tr)
	}
	if len(tr.Episodes) != 1 || tr.Episodes[0] != "ep1" || tr.EpisodeChars == 0 {
		t.Fatalf("trace: %+v", tr)
	}
	if !strings.Contains(block, koVoice().Pack.Memory.ReadHeader) || !strings.Contains(block, "등산 가자") {
		t.Fatalf("block: %s", block)
	}
}

// The blocks go before the member's line of the last user message; without room memory the messages
// are exactly the evaluated ones.
func TestRoomMemoryPlacementKeepsPromptWithout(t *testing.T) {
	in := AnswerInput{System: "SYS", RagState: "not_attempted", Speaker: "영희", Message: "등산 어떻게 됐지?"}
	plain := BuildAnswerMessages(koVoice(), in)
	in.RoomMemory = ""
	if again := BuildAnswerMessages(koVoice(), in); again[len(again)-1].Content != plain[len(plain)-1].Content {
		t.Fatal("an empty room memory changed the prompt")
	}
	in.RoomMemory = "<방 사건 기억 참고용>\nX\n</방 사건 기억>"
	got := BuildAnswerMessages(koVoice(), in)
	if got[0].Content != plain[0].Content {
		t.Fatal("the system prompt (cached part) changed")
	}
	user := got[len(got)-1].Content
	if !strings.HasSuffix(user, "\n\n영희: 등산 어떻게 됐지?") || !strings.Contains(user, "</기억>\n\n<방 사건 기억 참고용>") {
		t.Fatalf("placement:\n%s", user)
	}
	if strings.Replace(user, "\n\n"+in.RoomMemory, "", 1) != plain[len(plain)-1].Content {
		t.Fatal("room memory changed more than its own block")
	}
}

type fixedMemory struct{ hits []search.Hit }

func (f fixedMemory) Search(context.Context, engine.SearchRequest) (search.Result, error) {
	return search.Result{Hits: f.hits, State: "candidates"}, nil
}

type recordLLM struct{ msgs [][]Msg }

func (r *recordLLM) Chat(_ context.Context, model string, msgs []Msg, _ int, _ float64) (string, float64, error) {
	r.msgs = append(r.msgs, msgs)
	if model == "planner" {
		return `{"memory_requirement":"required","tool_plan":[]}`, 0, nil
	}
	return "응", 0, nil
}

// End to end in the runtime: all hits covered by the episode → the memory block says so, the episode
// block carries the lines, and the result records the episode.
func TestAnswerWithRoomMemory(t *testing.T) {
	llm := &recordLLM{}
	ts := Toolset{Known: map[string]bool{"memory.search": true}, Enabled: []ToolInfo{{ID: "memory.search"}}}
	rt := &Runtime{LLM: llm, Memory: fixedMemory{hits: []search.Hit{{Rank: 1, DocID: "L1", Room: "discord:g:c", Excerpt: "철수: 등산 가자"}}}}
	res, err := rt.Answer(context.Background(), Request{Message: "등산 어떻게 됐지?", Speaker: "영희", Voice: koVoice(), MemoryOn: true, Tools: ts,
		Rooms: []string{"discord:g:c"}, PlannerModel: "planner", ResponseModel: "response",
		RoomMemory: &RoomMemory{Episodes: []RoomEpisode{{ID: "ep1", Title: "토요일 등산", Evidence: []RoomLine{{Author: "철수", Text: "등산 가자"}}, Leaves: []string{"L1"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Room == nil || res.Room.DroppedHits != 1 || len(res.Hits) != 0 || res.RagState != RagCoveredByRoomMemory {
		t.Fatalf("result: %+v room=%+v", res.RagState, res.Room)
	}
	last := llm.msgs[len(llm.msgs)-1]
	user := last[len(last)-1].Content
	if !strings.Contains(user, koVoice().Pack.Chat.RagNotes[RagCoveredByRoomMemory]) || strings.Count(user, "등산 가자") != 1 {
		t.Fatalf("answer prompt:\n%s", user)
	}
}
