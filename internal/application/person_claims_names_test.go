package application_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"masamong/internal/memory/store"
)

// The write gate refuses a claim whose text writes out, outside its {@ref} tokens, a known name of anyone
// but its subject: every label the server saw a person under, a renamed account's old label included, as
// a whole word or with a particle or honorific, in any case. That person's forget finds claims by
// reference only, so such a claim would survive it. The controls: a plain claim, a claim naming only its
// subject, a name inside a longer word and a person written as a token are stored.
func TestWriteGateRefusesLiteralKnownNames(t *testing.T) {
	t.Parallel()
	s, m, e := memorySvc(t)
	f := reviewPerson{s, m, e, time.Now().UTC().Add(-3 * time.Hour)}
	a, c := store.PersonRef("101"), store.PersonRef("505")
	f.source(t, "g", "c", "kn-old-name", "505", "Charlie", f.at.Add(-2*time.Hour))
	f.source(t, "g", "c", "kn-new-name", "505", "Charles", f.at.Add(-time.Hour))
	line := f.source(t, "g", "c", "kn-line", "101", "민수", f.at)
	save := func(topic, text string) int {
		t.Helper()
		n, err := s.SavePersonClaimsForTest(t.Context(), store.PersonClaim{GuildID: "g", SubjectRef: a, SpeakerRef: a, Kind: "other", Topic: topic,
			Text: text, Evidence: []string{line}, Source: "extract:seg-kn", Link: store.LinkSelf})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	for topic, text := range map[string]string{
		"old":       "{@" + a + "} plays kn-chess with Charlie",
		"new":       "{@" + a + "} plays kn-chess with Charles",
		"case":      "{@" + a + "} plays kn-chess with charlie",
		"honorific": "{@" + a + "}는 Charlie님과 kn-chess를 둠",
		"particle":  "{@" + a + "}는 Charles랑 kn-chess를 둠",
	} {
		if n := save(topic, text); n != 0 {
			t.Errorf("%s: a claim writing out another person's known name was stored: %q", topic, text)
		}
	}
	for topic, text := range map[string]string{
		"plain":   "{@" + a + "} plays kn-chess every week",
		"own":     "{@" + a + "}(민수) plays kn-chess",
		"longer":  "{@" + a + "} plays kn-chess at Charliexyz",
		"pronoun": "{@" + a + "} plays kn-chess with friends",
	} {
		if n := save(topic, text); n != 1 {
			t.Errorf("control %s: the claim was refused: %q", topic, text)
		}
	}
	// hearsay naming the other person by token passes: their forget reaches it
	told := store.PersonClaim{GuildID: "g", SubjectRef: c, SpeakerRef: a, Kind: "other", Topic: "token",
		Text: "{@" + c + "} plays kn-chess with {@" + a + "}", Evidence: []string{line}, Source: "extract:seg-kn", Link: store.LinkMention}
	if n, err := s.SavePersonClaimsForTest(t.Context(), told); err != nil || n != 1 {
		t.Errorf("control: hearsay naming people by token was refused: %d (%v)", n, err)
	}
}

// An account seen as 찰리 and later renamed 찰스. A conversation names 찰리; the extraction prompt lists the
// account under its newest label, so the live tie does not know the old one and the model's text keeps
// 찰리 written out. The gate refuses that claim (찰리 is a known name of the account), so it is never
// stored, before or after 찰리's forget-all. The control: a plain statement of the same answer is stored.
func TestRenamedAccountsOldLabelIsRefused(t *testing.T) {
	t.Parallel()
	const marker = "kn-rename-marker"
	conversation := []said{
		{"9401", "101", "민수", "영희는 요즘 찰리랑 " + marker + " 연습해"}, {"9402", "202", "영희", "응 맞아 매주 하고 있어"},
		{"9403", "101", "민수", "부럽다 나는 kn-plain-sentinel 좋아해"}, {"9404", "202", "영희", "좋아 다음에 같이 하자"},
	}
	answer := `{"episodes":[],"people":[
	 {"about":"p2","kind":"other","topic":"연습","text":"{p2}는 찰리와 ` + marker + ` 연습을 함","by":["p1"],"ev":["m1"]},
	 {"about":"p1","kind":"like","topic":"취미","text":"{p1}는 kn-plain-sentinel을 좋아함","by":["p1"],"ev":["m3"]}]}`
	for _, order := range []string{"forget after", "cutoff first"} {
		t.Run(order, func(t *testing.T) {
			t.Parallel()
			s, m, e := memorySvc(t)
			f := reviewPerson{s, m, e, time.Now().UTC().Add(-3 * time.Hour)}
			f.source(t, "g", "c", "kn-charlie", "505", "찰리", f.at.Add(-2*time.Hour))
			f.source(t, "g", "c", "kn-charles", "505", "찰스", f.at.Add(-time.Hour))
			f.source(t, "g", "c", "kn-asker", "303", "지수", f.at.Add(-40*time.Minute))
			m.extract = func(prompt string) string {
				if !strings.Contains(prompt, marker) {
					return `{"episodes":[]}`
				}
				return answer
			}
			forgetAll := func() {
				t.Helper()
				req := func(confirm any) map[string]any {
					return map[string]any{"guild_id": "g", "user_id": "505", "scope": "all", "confirm": confirm}
				}
				p := exec(s, "memory.mine.forget", req(""))
				if !p.OK {
					t.Fatalf("preview: %+v", p)
				}
				if r := exec(s, "memory.mine.forget", req(p.Output["token"])); !r.OK || r.Output["applied"] != true {
					t.Fatalf("forget: %+v", r)
				}
			}
			sayIn(e, f.at, conversation...)
			if order == "cutoff first" {
				forgetAll()
			}
			s.RunRoomMemory(context.Background())
			if n := personClaimCount(t, s, "instr(text,?)>0", marker); n != 0 {
				t.Errorf("a statement writing out the renamed account's old label was stored: %d row(s)", n)
			}
			if n := personClaimCount(t, s, "instr(text,'kn-plain-sentinel')>0"); n != 1 {
				t.Errorf("control: the plain statement of the same answer was not stored: %d", n)
			}
			if order == "forget after" {
				forgetAll()
			}
			if n := personClaimCount(t, s, "instr(text,?)>0", marker); n != 0 {
				t.Errorf("after 찰리's forget-all, %d statement(s) still name him", n)
			}
			if got := block(f.ask(t, "<@202> 요즘 뭐 해?"), cardOpen, cardClose); strings.Contains(got, marker) {
				t.Errorf("the recall prompt carries a statement naming 찰리: %q", got)
			}
		})
	}
}
