package application_test

import (
	"strings"
	"testing"
	"time"

	"masamong/internal/application"
	"masamong/internal/config"
	"masamong/internal/memory/store"
)

// askIn is a turn of 지수 (303) in a channel of server g; it returns the person block of the answer prompt.
func (f reviewPerson) askIn(t *testing.T, channel, text string) string {
	t.Helper()
	turn(t, f.Service, "303", "지수", text, func(in *application.ChatInput) { in.ChannelID, in.Locale = channel, "ko" })
	return block(f.model.last("answer"), cardOpen, cardClose)
}

// explicitAnswer sets what the explicit classifier answers.
func (m *turnModel) explicitAnswer(answer string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.explicit = func(string) string { return answer }
}

// Astra 19: "remember this" on a single-valued topic follows the order of the messages, not of the
// requests' completion: an older message processed later does not remove the newer fact.
func TestExplicitSupersessionFollowsMessageOrder(t *testing.T) {
	t.Parallel()
	for _, order := range []string{"inOrder", "newerFirst"} {
		t.Run(order, func(t *testing.T) {
			t.Parallel()
			s, m, e := memorySvc(t)
			f := reviewPerson{s, m, e, time.Now().UTC().Add(-3 * time.Hour)}
			f.source(t, "g", "c", "job-old", "101", "민수", f.at)
			f.source(t, "g", "c", "job-new", "101", "민수", f.at.Add(time.Hour))
			remember := func(id, fact string) {
				m.explicitAnswer(`{"intent":"remember","about":"self","fact":"` + fact + `","key":"job","instruction":false}`)
				turn(t, s, "101", "민수", "기억해, 나 직업 바뀌었어", live(id))
			}
			old := func() { remember("job-old", "{p1}는 고등학교 교사로 일함") }
			newer := func() { remember("job-new", "{p1}가 이번에 종합병원 의사가 됨") }
			if order == "inOrder" {
				old()
				newer()
			} else {
				newer()
				old()
			}
			if n := personClaimCount(t, s, "source=? AND instr(text,'종합병원')>0", store.SourceExplicit); n != 1 {
				t.Errorf("the newer fact's claim: %d", n)
			}
			if got := f.askIn(t, "c", "<@101> 어떤 사람이야?"); !strings.Contains(got, "종합병원") || strings.Contains(got, "교사") {
				t.Errorf("the newer message's fact is shown, the older is not: %q", got)
			}
		})
	}
}

// Sol medium, Astra 20: on a no_hearsay server a told statement is stored and hidden when read; turning
// the option off shows it.
func TestToldStoredUnderNoHearsay(t *testing.T) {
	t.Parallel()
	s, m, e := memorySvc(t)
	strict(t, s, config.GuildPrivacy{NoHearsay: true})
	f := reviewPerson{s, m, e, time.Now().UTC().Add(-time.Hour)}
	b := store.PersonRef("202")
	f.source(t, "g", "c", "nh-label", "202", "영희", f.at)
	f.source(t, "g", "c", "nh-line", "101", "민수", f.at.Add(time.Minute))
	m.explicitAnswer(`{"intent":"remember","about":"other","fact":"영희는 nh-tea-sentinel을 좋아함","key":"drink","instruction":false}`)
	turn(t, s, "101", "민수", "기억해, 영희는 nh-tea-sentinel 좋아해", live("nh-line"))
	if n := personClaimCount(t, s, "source=? AND subject_ref=?", store.SourceExplicit, b); n != 1 {
		t.Fatalf("a told statement on a no_hearsay server is stored: %d", n)
	}
	if got := f.askIn(t, "c", "<@202> 어떤 사람이야?"); strings.Contains(got, "nh-tea-sentinel") {
		t.Errorf("a told statement is shown on a no_hearsay server: %q", got)
	}
	strict(t, s, config.GuildPrivacy{})
	if got := f.askIn(t, "c", "<@202> 어떤 사람이야?"); !strings.Contains(got, "nh-tea-sentinel") {
		t.Errorf("the told statement after no_hearsay is off: %q", got)
	}
}

// Astra 20: on an own_card_only server a told statement is stored too and hidden when read (only the
// caller's card is shown); turning the option off shows it.
func TestToldStoredUnderOwnCardOnly(t *testing.T) {
	t.Parallel()
	s, m, e := memorySvc(t)
	strict(t, s, config.GuildPrivacy{OwnCardOnly: true})
	f := reviewPerson{s, m, e, time.Now().UTC().Add(-time.Hour)}
	b := store.PersonRef("202")
	f.source(t, "g", "c", "oc-label", "202", "영희", f.at)
	f.source(t, "g", "c", "oc-line", "101", "민수", f.at.Add(time.Minute))
	m.explicitAnswer(`{"intent":"remember","about":"other","fact":"영희는 oc-tea-sentinel을 좋아함","key":"drink","instruction":false}`)
	turn(t, s, "101", "민수", "기억해, 영희는 oc-tea-sentinel 좋아해", live("oc-line"))
	if n := personClaimCount(t, s, "source=? AND subject_ref=?", store.SourceExplicit, b); n != 1 {
		t.Fatalf("a told statement on an own_card_only server is stored: %d", n)
	}
	if got := f.askIn(t, "c", "<@202> 어떤 사람이야?"); strings.Contains(got, "oc-tea-sentinel") {
		t.Errorf("another person's card on an own_card_only server: %q", got)
	}
	strict(t, s, config.GuildPrivacy{})
	if got := f.askIn(t, "c", "<@202> 어떤 사람이야?"); !strings.Contains(got, "oc-tea-sentinel") {
		t.Errorf("the told statement after own_card_only is off: %q", got)
	}
}

// Astra 21: forgetting everything from Discord with nothing stored yet still offers the confirmation and
// records the cutoff a later backfill checks.
func TestDiscordForgetAllWritesCutoff(t *testing.T) {
	t.Parallel()
	s, m, e := memorySvc(t)
	f := reviewPerson{s, m, e, time.Now().UTC().Add(-3 * time.Hour)}
	ref := store.PersonRef("505")
	ev := f.source(t, "g", "c", "cut-line", "505", "진우", f.at)
	prev := as(s, "memory.mine.forget", map[string]any{"scope": "all"}, inChannel("505"))
	if !prev.OK {
		t.Fatalf("preview: %+v", prev)
	}
	if c := card(t, prev); len(c.Buttons) != 1 {
		t.Fatalf("forget-all with only raw messages offers no confirmation: %+v", c)
	}
	if done := as(s, "memory.mine.forget", map[string]any{"scope": "all", "confirm": prev.Output["token"]}, inChannel("505")); !done.OK || done.Output["applied"] != true {
		t.Fatalf("confirm: %+v", done)
	}
	var n int
	if err := s.Store.DB.QueryRow(`SELECT COUNT(*) FROM person_memory_suppress WHERE guild_id='g' AND person_ref=? AND scope='all'`, ref).Scan(&n); err != nil || n != 2 {
		t.Errorf("cutoff markers (subject and speaker): %d (%v)", n, err)
	}
	later := store.PersonClaim{GuildID: "g", SubjectRef: ref, SpeakerRef: ref, Kind: "like", Topic: "음식", Text: "{@" + ref + "}는 국수를 좋아함",
		Evidence: []string{ev}, Source: "backfill:pb_test", Link: store.LinkSelf}
	if n, err := s.SavePersonClaimsForTest(t.Context(), later); err != nil || n != 0 {
		t.Errorf("a backfill wrote what the member forgot: %d (%v)", n, err)
	}
}
