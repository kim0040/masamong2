package episodes

import (
	"slices"
	"strings"
	"testing"
	"time"

	"masamong/internal/lang"
)

func refOf(m Message) string { return "ref-" + m.Author }

func tripPrompt(t *testing.T, plans ...OpenPlan) Prompt {
	t.Helper()
	msgs := script(t, `
t1 2026-10-09 21:00 민수: 이번 주말에 가평 캠핑 갈래? 27일 어때
t2 2026-10-09 21:01 지영: 좋아 27일 가능
t3 2026-10-09 21:02 bot>t1: 캠핑 좋겠다!
t4 2026-10-09 21:03 민수>t2: 그럼 27일 가평 확정
t5 2026-10-09 21:04 철수: 나는 텐트 빌려올게
`)
	return Build(Input{Lines: msgs, BotName: "마사몽", Now: msgs[len(msgs)-1].At, Location: seoul, Plans: plans, RefOf: refOf,
		LabelOf: func(ref string) string { return strings.TrimPrefix(ref, "ref-") }})
}

func TestBuildNumbersLinesAndPeople(t *testing.T) {
	p := tripPrompt(t, OpenPlan{ID: "ep1", Title: "{@ref-은비} 생일 파티", Status: Open, Due: "2026-10-30", Revision: 2})
	for _, want := range []string{"[m1] 민수(p1): 이번 주말에", "[m3] 마사몽 ↩m1: 캠핑 좋겠다!", "[m4] 민수(p1) ↩m2:", "[m5] 철수(p3):"} {
		if !strings.Contains(p.Lines, want) {
			t.Fatalf("lines lack %q:\n%s", want, p.Lines)
		}
	}
	if p.Plans != "[e1] {p4} 생일 파티 (open, 2026-10-30)" {
		t.Fatalf("plans: %q", p.Plans)
	}
	if p.People() != "p1: 민수\np2: 지영\np3: 철수\np4: 은비" {
		t.Fatalf("people: %q", p.People())
	}
	if strings.Contains(p.Lines, "ref-") || strings.Contains(p.Plans, "ref-") {
		t.Fatal("stored references reach the prompt")
	}
	if !strings.HasPrefix(p.Now, "2026-10-09 21:04 (Fri)") {
		t.Fatalf("now: %q", p.Now)
	}
}

func TestParseKeepsValidItemsAndMapsPeople(t *testing.T) {
	p := tripPrompt(t)
	res, err := p.Parse("```json\n" + `{"episodes":[{"op":"create","link_to":null,"kind":"plan","title":"가평 캠핑","items":[
		{"kind":"proposal","text":"{p1}가 27일 가평 캠핑 제안","by":["p1"],"ev":["m1"]},
		{"kind":"decision","text":"27일 가평 캠핑 확정","by":["p1","p2"],"ev":["m2","m4"]},
		{"kind":"fact","text":"{p3}가 텐트 담당","by":"p3","ev":["m5"]}],
		"status":"decided","due":"2026-10-27","when_text":"27일","importance":4}]}` + "\n```")
	if err != nil || !res.Clean() || len(res.Ops) != 1 {
		t.Fatalf("%v %+v", err, res)
	}
	op := res.Ops[0]
	if op.Status != Decided || op.Due != "2026-10-27" || op.WhenText != "27일" || op.Importance != 4 || op.Kind != "plan" {
		t.Fatalf("op: %+v", op)
	}
	if op.Items[0].Text != "{@ref-민수}가 27일 가평 캠핑 제안" || !slices.Equal(op.Items[1].Evidence, []string{"t2", "t4"}) || !slices.Equal(op.Items[2].By, []string{"ref-철수"}) {
		t.Fatalf("items: %+v", op.Items)
	}
	if !slices.Equal(op.Participants, []string{"ref-민수", "ref-지영", "ref-철수"}) || len(op.Evidence) != 4 {
		t.Fatalf("participants %v evidence %v", op.Participants, op.Evidence)
	}
}

func TestParseRejectsWhatTheModelInvented(t *testing.T) {
	p := tripPrompt(t)
	cases := map[string]string{
		"evidence not in the input":     `{"kind":"fact","text":"x","ev":["m9"]}`,
		"no evidence":                   `{"kind":"fact","text":"x","ev":[]}`,
		"evidence is the bot's line":    `{"kind":"moment","text":"봇이 응원함","ev":["m3"]}`,
		"by is not an author":           `{"kind":"proposal","text":"{p3}가 제안","by":["p3"],"ev":["m1"]}`,
		"unknown person token":          `{"kind":"fact","text":"{p9}가 옴","ev":["m1"]}`,
		"unknown person in by":          `{"kind":"fact","text":"누가 옴","by":["p9"],"ev":["m1"]}`,
		"decision without two people":   `{"kind":"decision","text":"캠핑 확정","by":["p1"],"ev":["m2"]}`,
		"decision by one person, no by": `{"kind":"decision","text":"캠핑 확정","ev":["m4"]}`,
		"text too long":                 `{"kind":"fact","text":"` + strings.Repeat("가", 121) + `","ev":["m1"]}`,
		"unknown kind":                  `{"kind":"rumor","text":"x","ev":["m1"]}`,
		"stored reference in text":      `{"kind":"fact","text":"{@abc} 옴","ev":["m1"]}`,
	}
	for name, item := range cases {
		res, err := p.Parse(`{"episodes":[{"op":"create","kind":"event","title":"t","items":[` + item + `,{"kind":"fact","text":"남는 항목","ev":["m2"]}]}]}`)
		if err != nil || res.Clean() || len(res.Ops) != 1 || len(res.Ops[0].Items) != 1 || res.Ops[0].Items[0].Text != "남는 항목" {
			t.Errorf("%s: %v %+v", name, err, res)
		}
	}
	// one person confirming their own plan is a decision
	res, _ := p.Parse(`{"episodes":[{"op":"create","kind":"decision","title":"t","items":[{"kind":"decision","text":"{p3}가 텐트 빌려오기로 함","by":["p3"],"ev":["m5"]}]}]}`)
	if !res.Clean() {
		t.Fatalf("own confirmation: %+v", res)
	}
	// operations
	for name, raw := range map[string]string{
		"not json":            `none`,
		"no episodes key":     `{"items":[]}`,
		"episodes not a list": `{"episodes":{}}`,
	} {
		if _, err := p.Parse(raw); err != ErrParse {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, raw := range map[string]string{
		"unknown op":           `{"episodes":[{"op":"merge","kind":"plan","title":"t","items":[{"kind":"fact","text":"x","ev":["m1"]}]}]}`,
		"create with link":     `{"episodes":[{"op":"create","link_to":"e1","kind":"plan","title":"t","items":[{"kind":"fact","text":"x","ev":["m1"]}]}]}`,
		"update unknown plan":  `{"episodes":[{"op":"update","link_to":"e1","items":[{"kind":"fact","text":"x","ev":["m1"]}]}]}`,
		"create without items": `{"episodes":[{"op":"create","kind":"plan","title":"t","items":[]}]}`,
		"create without title": `{"episodes":[{"op":"create","kind":"plan","items":[{"kind":"fact","text":"x","ev":["m1"]}]}]}`,
		"long title":           `{"episodes":[{"op":"create","kind":"plan","title":"` + strings.Repeat("a", 121) + `","items":[{"kind":"fact","text":"x","ev":["m1"]}]}]}`,
	} {
		res, err := p.Parse(raw)
		if err != nil || len(res.Ops) != 0 || res.Clean() {
			t.Errorf("%s: %v %+v", name, err, res)
		}
	}
	if res, err := p.Parse(`{"episodes":[]}`); err != nil || !res.Clean() || len(res.Ops) != 0 {
		t.Fatalf("nothing to keep: %v %+v", err, res)
	}
}

func TestParseUpdatesAndClosesOfferedPlans(t *testing.T) {
	p := tripPrompt(t, OpenPlan{ID: "ep-camp", Title: "가평 캠핑", Status: Open, Revision: 3})
	res, err := p.Parse(`{"episodes":[{"op":"update","link_to":"e1","items":[{"kind":"decision","text":"27일로 확정","ev":["m2","m4"]}],"status":"decided","due":"2026-10-27T10:00"}]}`)
	if err != nil || !res.Clean() || res.Ops[0].LinkTo != "ep-camp" || res.Ops[0].LinkRevision != 3 || res.Ops[0].Due != "2026-10-27T10:00" {
		t.Fatalf("update: %v %+v", err, res)
	}
	res, _ = p.Parse(`{"episodes":[{"op":"close","link_to":"e1","status":"dropped","items":[]}]}`)
	if len(res.Ops) != 0 {
		t.Fatalf("a cancellation needs the cancelling line: %+v", res)
	}
	res, _ = p.Parse(`{"episodes":[{"op":"close","link_to":"e1","status":"dropped","items":[{"kind":"fact","text":"취소함","ev":["m5"]}]},{"op":"update","link_to":"e1","items":[{"kind":"fact","text":"x","ev":["m1"]}]}]}`)
	if len(res.Ops) != 1 || res.Ops[0].Status != Dropped || res.Clean() {
		t.Fatalf("close, then a second op on the same plan: %+v", res)
	}
}

func TestStoredTextAndRefs(t *testing.T) {
	text := "{@a}가 {@b}에게 제안"
	if got := StoredText(text, func(r string) string { return map[string]string{"a": "민수"}[r] }, "누군가"); got != "민수가 누군가에게 제안" {
		t.Fatal(got)
	}
	if !slices.Equal(TextRefs(text), []string{"a", "b"}) {
		t.Fatal(TextRefs(text))
	}
}

func TestNormalizeDue(t *testing.T) {
	for in, want := range map[string]string{"2026-10-27": "2026-10-27", "2026-10-27T10:00": "2026-10-27T10:00", "2026-10-27 10:00": "2026-10-27T10:00",
		"2026-10-27T10:00:00+09:00": "2026-10-27T10:00", "next week": "", "": ""} {
		if got := NormalizeDue(in); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
	if d, ok := DueTime("2026-10-27", seoul); !ok || d.Hour() != 23 {
		t.Fatal(d)
	}
}

func TestStatusMachine(t *testing.T) {
	due := time.Date(2026, 10, 27, 23, 59, 59, 0, seoul)
	after := due.Add(time.Hour)
	cases := []struct {
		cur  string
		c    Change
		want string
	}{
		{Open, Change{Kind: "plan", Want: Decided, HasDecision: true}, Decided},
		{Open, Change{Kind: "plan", Want: Decided}, Open},
		{Decided, Change{Kind: "plan", Want: Done, Due: due, EvidenceAt: after}, Done},
		{Decided, Change{Kind: "plan", Want: Done, Due: due, EvidenceAt: due.Add(-time.Hour)}, Decided},
		{Decided, Change{Kind: "plan", Want: Done}, Decided},
		{Open, Change{Kind: "event", Want: Done}, Done},
		{Open, Change{Kind: "plan", Op: "update", Want: Dropped}, Open},
		{Decided, Change{Kind: "plan", Op: "close", Want: Dropped}, Dropped},
		{Decided, Change{Kind: "plan", Want: Open}, Decided},
		{Dropped, Change{Kind: "plan", Want: Decided, HasDecision: true}, Dropped},
		{"", Change{Kind: "plan"}, Open},
	}
	for i, c := range cases {
		if got := Next(c.cur, c.c); got != c.want {
			t.Errorf("%d: %s -> %s, want %s", i, c.cur, got, c.want)
		}
	}
	seen := time.Date(2026, 10, 1, 12, 0, 0, 0, seoul)
	if Expire("plan", Open, time.Time{}, seen, seen.Add(13*24*time.Hour)) != Open || Expire("plan", Open, time.Time{}, seen, seen.Add(14*24*time.Hour)) != Expired {
		t.Fatal("undecided proposals expire after 14 days")
	}
	if Expire("plan", Decided, time.Time{}, seen, seen.Add(29*24*time.Hour)) != Decided || Expire("plan", Decided, time.Time{}, seen, seen.Add(30*24*time.Hour)) != Expired {
		t.Fatal("30 days since the last mention")
	}
	if Expire("plan", Decided, due, seen, due.Add(6*24*time.Hour)) != Decided || Expire("plan", Open, due, seen, due.Add(7*24*time.Hour)) != Expired {
		t.Fatal("due + 7 days")
	}
	if Expire("joke", Open, time.Time{}, seen, seen.Add(400*24*time.Hour)) != Open || Expire("plan", Done, time.Time{}, seen, seen.Add(400*24*time.Hour)) != Done {
		t.Fatal("jokes and finished plans do not expire")
	}
	if !NeedsRegeneration(5, 2, false) || NeedsRegeneration(5, 1, false) || !NeedsRegeneration(5, 1, true) || NeedsRegeneration(0, 0, true) {
		t.Fatal("regeneration rule")
	}
}

// Every language pack fills the room-memory prompts completely, and their output examples are JSON the
// parsers accept as a shape.
func TestPackPromptsFill(t *testing.T) {
	p := tripPrompt(t)
	for _, tag := range lang.Available() {
		pk, _ := lang.Get(tag)
		m := pk.Memory
		ep := lang.Fill(m.EpisodeExtract, "now", p.Now, "people", p.People(), "plans", p.Plans, "lines", p.Lines)
		st := lang.Fill(m.ChannelState, "now", p.Now, "people", p.People(), "previous", "-", "lines", p.Lines)
		for name, filled := range map[string]string{"episode": ep, "state": st,
			"episode system": lang.Fill(m.EpisodeExtractSystem, "bot", "마사몽"), "state system": lang.Fill(m.ChannelStateSystem, "bot", "마사몽")} {
			for _, ph := range []string{"{now}", "{people}", "{plans}", "{previous}", "{lines}", "{bot}"} {
				if strings.Contains(filled, ph) {
					t.Errorf("%s %s: %s left", tag, name, ph)
				}
			}
			if !strings.Contains(filled, "마사몽") && strings.HasSuffix(name, "system") {
				t.Errorf("%s %s: no bot name", tag, name)
			}
		}
		lines := strings.Split(ep, "\n")
		if _, err := p.Parse(lines[len(lines)-1]); err != nil {
			t.Errorf("%s episode example: %v", tag, err)
		}
		lines = strings.Split(st, "\n")
		if _, _, err := p.ParseState(lines[len(lines)-1]); err != nil {
			t.Errorf("%s state example: %v", tag, err)
		}
	}
}
