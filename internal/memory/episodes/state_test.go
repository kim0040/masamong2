package episodes

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseStateValidatesAndCarries(t *testing.T) {
	p := tripPrompt(t)
	prev := State{Threads: []Thread{{Label: "{@ref-은비} 이사", Who: []string{"ref-은비"}, Status: "active", Last: "old1", LastAt: "2026-10-09T11:00:00Z", Ev: []string{"old1"}}},
		OpenQuestions: []Question{{Q: "누가 차 가져와?", Who: []string{"ref-민수"}, LastAt: "2026-10-09T11:00:00Z", Ev: []string{"old2"}}}}
	rendered := p.Previous(prev)
	if rendered != "[t1] {p4} 이사 (p4; active)\n[q1] ? 누가 차 가져와? (p1)" {
		t.Fatalf("previous: %q", rendered)
	}
	st, problems, err := p.ParseState(`{"threads":[
		{"from":null,"label":"가평 캠핑","gist":"{p1}와 {p2}가 27일로 정함","who":["p1","p2"],"status":"active","ev":["m2","m4"]},
		{"from":"t1","label":"{p4} 이사","who":[],"status":"cooling","ev":[]},
		{"label":"봇 칭찬","who":[],"ev":["m3"]},
		{"label":"근거 없음","ev":[]}],
		"open_questions":[{"from":"q1","q":"누가 차 가져와?","who":["p1"],"ev":[]}],
		"mood":{"text":"신남","ev":["m4"]},"banter":{"text":"","ev":[]}}`)
	if err != nil || len(problems) != 2 {
		t.Fatalf("%v %v", err, problems)
	}
	if len(st.Threads) != 2 || st.Threads[0].Gist != "{@ref-민수}와 {@ref-지영}가 27일로 정함" || st.Threads[0].Last != "t4" || !slices.Equal(st.Threads[0].Who, []string{"ref-민수", "ref-지영"}) {
		t.Fatalf("thread 1: %+v", st.Threads)
	}
	if c := st.Threads[1]; c.Status != "cooling" || !slices.Equal(c.Ev, []string{"old1"}) || c.LastAt != "2026-10-09T11:00:00Z" || !slices.Equal(c.Who, []string{"ref-은비"}) {
		t.Fatalf("carried thread: %+v", c)
	}
	if len(st.OpenQuestions) != 1 || !slices.Equal(st.OpenQuestions[0].Ev, []string{"old2"}) || st.Mood == nil || st.Banter != nil {
		t.Fatalf("questions/notes: %+v", st)
	}
	if !slices.Equal(st.Evidence(), []string{"t2", "t4", "old1", "old2"}) {
		t.Fatalf("evidence: %v", st.Evidence())
	}
	if !slices.Equal(st.Refs(), []string{"ref-민수", "ref-지영", "ref-은비"}) {
		t.Fatalf("refs: %v", st.Refs())
	}
	// at most four topics, labels fit
	var many []string
	for i := 0; i < 6; i++ {
		many = append(many, `{"label":"화제","ev":["m1"]}`)
	}
	st, problems, _ = p.ParseState(`{"threads":[` + strings.Join(many, ",") + `]}`)
	if len(st.Threads) != MaxThreads || len(problems) != 2 {
		t.Fatalf("cap: %d %v", len(st.Threads), problems)
	}
	if _, _, err := p.ParseState(`{"mood":"x"}`); err != ErrParse {
		t.Fatal("threads key is required")
	}
	if _, problems, _ := p.ParseState(`{"threads":[{"label":"` + strings.Repeat("가", 41) + `","ev":["m1"]}]}`); len(problems) != 1 {
		t.Fatal("long label")
	}
}

func TestStateAges(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }
	st := State{Threads: []Thread{{Label: "a", Status: "active", LastAt: at(5 * time.Minute)}, {Label: "b", Status: "active", LastAt: at(40 * time.Minute)}, {Label: "c", Status: "active", LastAt: at(4 * time.Hour)}},
		OpenQuestions: []Question{{Q: "q", LastAt: at(4 * time.Hour)}}, Mood: &Note{Text: "m", LastAt: at(time.Hour)}, Banter: &Note{Text: "b", LastAt: at(5 * time.Hour)}}
	st.Age(now)
	if len(st.Threads) != 2 || st.Threads[0].Status != "active" || st.Threads[1].Status != "cooling" || len(st.OpenQuestions) != 0 || st.Mood == nil || st.Banter != nil {
		t.Fatalf("%+v", st)
	}
}

func TestScheduleDebounces(t *testing.T) {
	var s Schedule
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	if s.Due(4, now.Add(-time.Hour), now) || !s.Due(5, now.Add(-3*time.Minute), now) || s.Due(10, now.Add(-time.Minute), now) || !s.Due(40, now, now) {
		t.Fatal("debounce")
	}
	if !s.Full(0) || s.Full(1) || !s.Full(6) {
		t.Fatal("full every 6 updates")
	}
}
