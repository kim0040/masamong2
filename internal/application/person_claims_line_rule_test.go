package application_test

import (
	"strings"
	"testing"
	"time"

	"masamong/internal/config"
	"masamong/internal/memory/store"
)

// A member says an item about them is wrong, or forgets it. Live extraction saved the same fact from the
// same line in other words (the item's text and the people entry's text are two fields of one answer, so
// they rarely match). The correction takes that statement too, and the other fact of the same line (the
// line rule; known limit: recall only). A fact on another line stays, and the corrected fact is not read
// in another channel.
func TestCorrectionTakesTheSameFactInOtherWords(t *testing.T) {
	t.Parallel()
	for _, how := range []string{"wrong", "forget"} {
		t.Run(how, func(t *testing.T) {
			t.Parallel()
			s, m, e := memorySvc(t)
			applyDoc(t, s, func(d *config.Document) { d.Guilds["g"].Channels["other"] = config.ChannelPolicy{Capture: true} })
			f := reviewPerson{s, m, e, time.Now().UTC().Add(-time.Hour)}
			a := store.PersonRef("101")
			line := f.source(t, "g", "c", "lr-shared-line", "101", "민수", f.at)
			otherLine := f.source(t, "g", "c", "lr-other-line", "101", "민수", f.at.Add(time.Minute))
			// the episode item, as the extraction wrote it
			if n := f.item(t, "c", "res-item", "{@"+a+"}가 lr-city-sentinel로 이사함", []string{"101"}, line); n != 1 {
				t.Fatalf("the residence item gave %d claims", n)
			}
			// the people entry of the same answer: the same fact, other words
			same := store.PersonClaim{GuildID: "g", SubjectRef: a, SpeakerRef: a, Kind: "status", Topic: "residence",
				Text: "{@" + a + "}는 lr-city-sentinel에 산다", Evidence: []string{line}, Source: "extract:seg-lr", Link: store.LinkSelf}
			tea := same
			tea.Kind, tea.Topic, tea.Text = "like", "음료", "{@"+a+"}는 lr-tea-sentinel을 좋아함"
			noodle := tea
			noodle.Topic, noodle.Text, noodle.Evidence = "음식", "{@"+a+"}는 lr-noodle-sentinel을 좋아함", []string{otherLine}
			for _, c := range []store.PersonClaim{same, tea, noodle} {
				if n, err := s.SavePersonClaimsForTest(t.Context(), c); err != nil || n != 1 {
					t.Fatalf("live statement %q: %d (%v)", c.Topic, n, err)
				}
			}
			if how == "wrong" {
				m.explicitAnswer(`{"intent":"wrong","target":["i1"]}`)
				turn(t, s, "101", "민수", "그거 틀렸어", live(""))
				if ans := m.last("answer"); !strings.Contains(ans, `"hidden":1`) {
					t.Fatalf("correction: %s", ans)
				}
			} else {
				m.explicitAnswer(`{"intent":"forget","target":["i1"]}`)
				turn(t, s, "101", "민수", "아까 그거 잊어줘", live(""))
				prev := as(s, "memory.mine.forget", map[string]any{}, inChannel("101"))
				if !prev.OK || prev.Output["scope"] != "pending" {
					t.Fatalf("preview: %+v", prev)
				}
				if done := as(s, "memory.mine.forget", map[string]any{"scope": "pending", "confirm": prev.Output["token"]}, inChannel("101")); !done.OK || done.Output["applied"] != true {
					t.Fatalf("forget: %+v", done)
				}
			}
			if n := personClaimCount(t, s, "source=? AND state<>?", "episode_item:res-item", store.PersonHidden); n != 0 {
				t.Fatalf("the item's own statement is left: %d", n)
			}
			if n := personClaimCount(t, s, "instr(text,'lr-city-sentinel')>0 AND state=?", store.PersonActive); n != 0 {
				t.Errorf("after the member's %s, %d active statement(s) of the same fact remain (same subject, same line, other words)", how, n)
			}
			if n := personClaimCount(t, s, "instr(text,'lr-tea-sentinel')>0 AND state=?", store.PersonActive); n != 0 {
				t.Errorf("the line rule: another fact about the person on the corrected line stayed: %d", n)
			}
			if n := personClaimCount(t, s, "instr(text,'lr-noodle-sentinel')>0 AND state=?", store.PersonActive); n != 1 {
				t.Errorf("control: a fact on another line went with the correction: %d", n)
			}
			got := f.askIn(t, "other", "<@101> 어디 살아? 뭐 좋아해?")
			if strings.Contains(got, "lr-city-sentinel") {
				t.Errorf("the corrected fact is still read, in another channel: %q", got)
			}
			if !strings.Contains(got, "lr-noodle-sentinel") {
				t.Errorf("control: the fact on another line is not read in another channel: %q", got)
			}
		})
	}
}

// A correction kept from v1.6 (a hidden item in an episode never made again). A later writer (the paid
// backfill reads all of that history) states the same fact about the same person from the same line in
// other words, or with only a punctuation change: it is refused (the line rule). A fact from another line
// passes.
func TestV16CorrectionRefusesTheSameFactInOtherWords(t *testing.T) {
	t.Parallel()
	for name, text := range map[string]string{
		"other words": "는 lr-old-city-sentinel 거주",
		"punctuation": "는 lr-old-city-sentinel에 삶.",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, m, e := memorySvc(t)
			f := reviewPerson{s, m, e, time.Now().UTC().Add(-3 * time.Hour)}
			a := store.PersonRef("101")
			line := f.source(t, "g", "c", "lr-v16-line", "101", "민수", f.at)
			otherLine := f.source(t, "g", "c", "lr-v16-other", "101", "민수", f.at.Add(time.Minute))
			for _, q := range []struct {
				sql  string
				args []any
			}{
				{`INSERT INTO channel_episode(id,guild_id,channel_id,room,kind,title,status,source,first_seen,last_seen,updated_at,regen)
					VALUES('v16-ep','g','c','discord:g:c','event','t','done','discord',?,?,?,3)`, []any{f.at, f.at, f.at}},
				{`INSERT INTO episode_item(id,episode_id,kind,text,evidence,by_refs,created_at,state) VALUES('v16-wrong','v16-ep','fact',?,?,?,?,'hidden')`,
					[]any{"{@" + a + "}는 lr-old-city-sentinel에 삶", `["` + line + `"]`, `["` + a + `"]`, f.at.Format(time.RFC3339Nano)}},
				{`INSERT INTO evidence_refs(source_id,holder_kind,holder_id) VALUES(?,'episode_item','v16-wrong')`, []any{line}},
			} {
				if _, err := s.Store.DB.Exec(q.sql, q.args...); err != nil {
					t.Fatal(err)
				}
			}
			again := store.PersonClaim{GuildID: "g", SubjectRef: a, SpeakerRef: a, Kind: "status", Topic: "residence",
				Text: "{@" + a + "}" + text, Evidence: []string{line}, Source: "backfill:pb_test", Link: store.LinkSelf}
			if n, err := s.SavePersonClaimsForTest(t.Context(), again); err != nil || n != 0 {
				t.Errorf("the fact a member corrected in v1.6 was written again (%s): %d row(s) (%v)", name, n, err)
			}
			other := again
			other.Kind, other.Topic, other.Text, other.Evidence = "like", "음료", "{@"+a+"}는 lr-tea-sentinel을 좋아함", []string{otherLine}
			if n, err := s.SavePersonClaimsForTest(t.Context(), other); err != nil || n != 1 {
				t.Errorf("control: a fact from another line was refused: %d (%v)", n, err)
			}
		})
	}
}
