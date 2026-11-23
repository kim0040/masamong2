package application_test

import (
	"strings"
	"testing"
	"time"

	"masamong/internal/memory/store"
)

// Round-2 review fixes of person memory v1.7.0 (forget and correction paths).

// One message gives two statements about a person (where they live, what they drink).
// Marking the residence item wrong, or forgetting it, removes the residence statement only: the other
// statement resting on the same line stays. The same holds for a correction kept from v1.6 (a hidden item
// in an episode never made again): it refuses its own statement, not every statement of that line.
func TestCorrectionKeepsOtherClaimsFromSharedEvidence(t *testing.T) {
	t.Parallel()
	for _, how := range []string{"wrong", "forget", "v16"} {
		t.Run(how, func(t *testing.T) {
			t.Parallel()
			s, m, e := memorySvc(t)
			f := reviewPerson{s, m, e, time.Now().UTC().Add(-time.Hour)}
			a := store.PersonRef("101")
			line := f.source(t, "g", "c", "shared-line", "101", "민수", f.at)
			residence := "{@" + a + "}는 shared-city-sentinel에 삶"
			tea := store.PersonClaim{GuildID: "g", SubjectRef: a, SpeakerRef: a, Kind: "like", Topic: "음료", Text: "{@" + a + "}는 shared-tea-sentinel을 좋아함",
				Evidence: []string{line}, Source: "extract:seg-shared", Link: store.LinkSelf}
			teaID := claimID("g", a, a, "like", "음료", line)
			if how == "v16" {
				for _, q := range []struct {
					sql  string
					args []any
				}{
					{`INSERT INTO channel_episode(id,guild_id,channel_id,room,kind,title,status,source,first_seen,last_seen,updated_at,regen)
						VALUES('v16-ep','g','c','discord:g:c','event','t','done','discord',?,?,?,3)`, []any{f.at, f.at, f.at}},
					{`INSERT INTO episode_item(id,episode_id,kind,text,evidence,by_refs,created_at,state) VALUES('v16-wrong','v16-ep','fact',?,?,?,?,'hidden')`,
						[]any{residence, `["` + line + `"]`, `["` + a + `"]`, f.at.Format(time.RFC3339Nano)}},
					{`INSERT INTO evidence_refs(source_id,holder_kind,holder_id) VALUES(?,'episode_item','v16-wrong')`, []any{line}},
				} {
					if _, err := s.Store.DB.Exec(q.sql, q.args...); err != nil {
						t.Fatal(err)
					}
				}
				corrected := tea
				corrected.Kind, corrected.Topic, corrected.Text = "status", "residence", residence
				if n, err := s.SavePersonClaimsForTest(t.Context(), corrected); err != nil || n != 0 {
					t.Errorf("the statement corrected in v1.6 was written again: %d (%v)", n, err)
				}
				if n, err := s.SavePersonClaimsForTest(t.Context(), tea); err != nil || n != 1 {
					t.Errorf("another statement of the corrected line was refused: %d (%v)", n, err)
				}
				return
			}
			if n := f.item(t, "c", "res-item", residence, []string{"101"}, line); n != 1 {
				t.Fatalf("the residence item gave %d claims", n)
			}
			if n, err := s.SavePersonClaimsForTest(t.Context(), tea); err != nil || n != 1 {
				t.Fatalf("the tea statement: %d (%v)", n, err)
			}
			if how == "wrong" {
				m.explicitAnswer(`{"intent":"wrong","target":["i1"]}`)
				turn(t, s, "101", "민수", "그거 틀렸어", live(""))
				if a := m.last("answer"); !strings.Contains(a, `"hidden":1`) {
					t.Fatalf("correction: %s", a)
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
			if n := personClaimCount(t, s, "source=? AND state<>?", "episode_item:res-item", store.PersonHidden); n != 0 { // wrong hides, forget deletes
				t.Errorf("the corrected statement is left: %d", n)
			}
			if n := personClaimCount(t, s, "id=? AND state=?", teaID, store.PersonActive); n != 1 {
				t.Errorf("the other statement of the same line went with the correction: %d", n)
			}
		})
	}
}
