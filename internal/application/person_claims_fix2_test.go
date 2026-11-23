package application_test

import (
	"strings"
	"testing"
	"time"

	"masamong/internal/config"
	"masamong/internal/memory/store"
)

// Round-2 review fixes of person memory v1.7.0 (forget and correction paths).

// One message gives two statements about a person (where they live, what they drink). Marking the
// residence item wrong, or forgetting it, follows the line rule: every statement about the person resting
// on the corrected line goes, the drink statement too (known limit: recall only, the member can say it
// again), and nothing about them from that line is written again. A statement on another line stays. The
// same holds for a correction kept from v1.6 (a hidden item in an episode never made again): it refuses
// every statement about the person from its line, and passes one from another line.
func TestCorrectionTakesEveryStatementOfItsLines(t *testing.T) {
	t.Parallel()
	for _, how := range []string{"wrong", "forget", "v16"} {
		t.Run(how, func(t *testing.T) {
			t.Parallel()
			s, m, e := memorySvc(t)
			f := reviewPerson{s, m, e, time.Now().UTC().Add(-time.Hour)}
			a := store.PersonRef("101")
			line := f.source(t, "g", "c", "shared-line", "101", "민수", f.at)
			otherLine := f.source(t, "g", "c", "kept-line", "101", "민수", f.at.Add(time.Minute))
			residence := "{@" + a + "}는 shared-city-sentinel에 삶"
			tea := store.PersonClaim{GuildID: "g", SubjectRef: a, SpeakerRef: a, Kind: "like", Topic: "음료", Text: "{@" + a + "}는 shared-tea-sentinel을 좋아함",
				Evidence: []string{line}, Source: "extract:seg-shared", Link: store.LinkSelf}
			teaID := claimID("g", a, a, "like", "음료", line)
			noodle := tea
			noodle.Topic, noodle.Text, noodle.Evidence = "음식", "{@"+a+"}는 kept-noodle-sentinel을 좋아함", []string{otherLine}
			noodleID := claimID("g", a, a, "like", "음식", otherLine)
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
				if n, err := s.SavePersonClaimsForTest(t.Context(), tea); err != nil || n != 0 {
					t.Errorf("another statement about the person from the corrected line was written: %d (%v)", n, err)
				}
				if n, err := s.SavePersonClaimsForTest(t.Context(), noodle); err != nil || n != 1 {
					t.Errorf("control: a statement from another line was refused: %d (%v)", n, err)
				}
				return
			}
			if n := f.item(t, "c", "res-item", residence, []string{"101"}, line); n != 1 {
				t.Fatalf("the residence item gave %d claims", n)
			}
			for _, c := range []store.PersonClaim{tea, noodle} {
				if n, err := s.SavePersonClaimsForTest(t.Context(), c); err != nil || n != 1 {
					t.Fatalf("the %s statement: %d (%v)", c.Topic, n, err)
				}
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
			if n := personClaimCount(t, s, "id=? AND state=?", teaID, store.PersonActive); n != 0 {
				t.Errorf("another statement about the person on the corrected line stayed: %d", n)
			}
			if n := personClaimCount(t, s, "id=? AND state=?", noodleID, store.PersonActive); n != 1 {
				t.Errorf("control: a statement on another line went with the correction: %d", n)
			}
			again := tea // the corrected fact in other words, from a later writer
			again.Kind, again.Topic, again.Text, again.Source = "status", "residence", "{@"+a+"}는 shared-city-sentinel 거주", "backfill:pb_test"
			if n, err := s.SavePersonClaimsForTest(t.Context(), again); err != nil || n != 0 {
				t.Errorf("a later writer stated something about the person from the corrected line again: %d (%v)", n, err)
			}
		})
	}
}

// A statement filed under its own speaker that names another person ("{p1}가 전하길 {p2}는 …") is
// hearsay about that person. As a direct claim it would be read in every channel of the server, also on a
// no_hearsay server, as the speaker's own words, so the write gate refuses it. The control: the same
// speaker's plain statement about themselves from the same answer is stored and read in another channel.
func TestSelfStatementNamingAnotherPersonDoesNotTravel(t *testing.T) {
	t.Parallel()
	s, m, e := memorySvc(t)
	applyDoc(t, s, func(d *config.Document) { d.Guilds["g"].Channels["other"] = config.ChannelPolicy{Capture: true} })
	strict(t, s, config.GuildPrivacy{NoHearsay: true})
	m.extract = func(string) string {
		return `{"episodes":[],"people":[
		 {"about":"p1","kind":"other","topic":"소문","text":"{p1}가 전하길 {p2}는 debt-sentinel 빚이 있음","by":["p1"],"ev":["m1"]},
		 {"about":"p1","kind":"like","topic":"음료","text":"{p1}는 own-tea-sentinel을 좋아함","by":["p1"],"ev":["m3"]}]}`
	}
	at := time.Now().Add(-3 * time.Hour)
	sayIn(e, at,
		said{"9101", "101", "민수", "영희 빚 있다던데 진짜야"}, said{"9102", "202", "영희", "그런 얘기 하지 마 제발"},
		said{"9103", "101", "민수", "알았어 미안 나는 차나 마실래"}, said{"9104", "202", "영희", "응 고마워 부탁할게"})
	if rep := s.RunRoomMemory(t.Context()); rep.PeopleClaims() == 0 {
		t.Fatalf("control: the extraction saved nothing: %+v", rep)
	}
	if n := personClaimCount(t, s, "instr(text,'debt-sentinel')>0"); n != 0 {
		t.Errorf("a self statement naming another person was stored: %d row(s)", n)
	}
	if n := personClaimCount(t, s, "instr(text,'own-tea-sentinel')>0 AND subject_ref=speaker_ref"); n != 1 {
		t.Fatalf("control: the plain self statement was not stored: %d", n)
	}
	f := reviewPerson{s, m, e, at}
	got := f.askIn(t, "other", "<@101> 어떤 사람이야?")
	if strings.Contains(got, "debt-sentinel") {
		t.Errorf("hearsay about another person travelled to #other on a no_hearsay server, as the speaker's own statement: %q", got)
	}
	if !strings.Contains(got, "own-tea-sentinel") {
		t.Errorf("control: the speaker's own statement is not read in #other: %q", got)
	}
}

// An operator's forget of one episode, or of a room's whole memory, also deletes the statements live
// extraction saved from the same lines (source extract:), which follow no item. The controls: a live
// statement on a line outside the forgotten episode's segment survives the episode forget, and one in
// another room survives both.
func TestEpisodeForgetRemovesLiveClaims(t *testing.T) {
	t.Parallel()
	for _, by := range []string{"id", "room"} {
		t.Run(by, func(t *testing.T) {
			t.Parallel()
			s, m, e := memorySvc(t)
			applyDoc(t, s, func(d *config.Document) { d.Guilds["g"].Channels["c2"] = config.ChannelPolicy{Capture: true} })
			m.extract = func(string) string {
				return `{"episodes":[{"op":"create","link_to":null,"kind":"event","title":"이사","items":[
				 {"kind":"fact","text":"{p1}가 move-sentinel로 이사함","by":["p1"],"ev":["m1"]}],"status":"done","importance":3}],
				 "people":[{"about":"p1","kind":"status","topic":"residence","text":"{p1}는 live-sentinel에 산다","by":["p1"],"ev":["m1"]}]}`
			}
			at := time.Now().Add(-3 * time.Hour)
			sayIn(e, at,
				said{"9201", "101", "민수", "나 새 동네로 이사했어 지난주에"}, said{"9202", "202", "영희", "오 정말? 축하해 이사"},
				said{"9203", "101", "민수", "응 고마워 집들이 할게"}, said{"9204", "202", "영희", "좋아 꼭 불러줘 집들이"})
			if rep := s.RunRoomMemory(t.Context()); rep.Episodes != 1 || rep.PeopleClaims() == 0 {
				t.Fatalf("control: %+v", rep)
			}
			f := reviewPerson{s, m, e, at.Add(time.Hour)}
			a := store.PersonRef("101")
			keep := func(channel, id, sentinel string) {
				t.Helper()
				line := f.source(t, "g", channel, id, "101", "민수", f.at)
				c := store.PersonClaim{GuildID: "g", SubjectRef: a, SpeakerRef: a, Kind: "like", Topic: sentinel, Text: "{@" + a + "}는 " + sentinel + "을 좋아함",
					Evidence: []string{line}, Source: "extract:" + line, Link: store.LinkSelf}
				if n, err := s.SavePersonClaimsForTest(t.Context(), c); err != nil || n != 1 {
					t.Fatalf("control write %s: %d (%v)", sentinel, n, err)
				}
			}
			keep("c", "9301", "same-room-sentinel")
			keep("c2", "9302", "other-room-sentinel")
			in := map[string]any{"guild_id": "g", "channel_id": "c"}
			if by == "id" {
				var ep string
				if err := s.Store.DB.QueryRow(`SELECT id FROM channel_episode`).Scan(&ep); err != nil {
					t.Fatal(err)
				}
				in = map[string]any{"id": ep}
			}
			if r := exec(s, "memory.episodes.forget", in); !r.OK {
				t.Fatalf("forget: %+v", r)
			}
			if n := personClaimCount(t, s, "instr(text,'move-sentinel')>0"); n != 0 {
				t.Fatalf("control: the derived claim went with its item: %d", n)
			}
			if n := personClaimCount(t, s, "instr(text,'live-sentinel')>0"); n != 0 {
				t.Errorf("after the operator's forget, %d live-extracted statement(s) of the same lines remain", n)
			}
			if got := f.askIn(t, "c", "<@101> 어떤 사람이야?"); strings.Contains(got, "live-sentinel") {
				t.Errorf("the forgotten conversation is still read: %q", got)
			}
			if n := personClaimCount(t, s, "instr(text,'other-room-sentinel')>0"); n != 1 {
				t.Errorf("a live statement of another room went with the forget: %d", n)
			}
			want := 1 // outside the episode's segment
			if by == "room" {
				want = 0 // the whole room's memory
			}
			if n := personClaimCount(t, s, "instr(text,'same-room-sentinel')>0"); n != want {
				t.Errorf("a live statement of the same room outside the episode: %d, want %d", n, want)
			}
		})
	}
	// One answer creates two episodes from one segment: A rests on m1, B on m3, and each line has a live
	// statement. Forgetting A by its id takes A's live statement and keeps B with its own. Without item
	// evidence (no items) A falls back to the segment, minus the lines B's items cite: the statement on m2,
	// which no episode cites, goes then, and B's stays.
	for _, items := range []bool{true, false} {
		t.Run(map[bool]string{true: "id with a sibling", false: "id with a sibling, no items"}[items], func(t *testing.T) {
			t.Parallel()
			s, m, e := memorySvc(t)
			m.extract = func(string) string {
				return `{"episodes":[
				 {"op":"create","link_to":null,"kind":"event","title":"이사","items":[{"kind":"fact","text":"{p1}가 move-sentinel로 이사함","by":["p1"],"ev":["m1"]}],"status":"done","importance":3},
				 {"op":"create","link_to":null,"kind":"event","title":"집들이","items":[{"kind":"fact","text":"{p1}가 party-sentinel 집들이를 함","by":["p1"],"ev":["m3"]}],"status":"done","importance":3}],
				 "people":[{"about":"p1","kind":"status","topic":"residence","text":"{p1}는 live-a-sentinel에 산다","by":["p1"],"ev":["m1"]},
				  {"about":"p2","kind":"like","topic":"congrats","text":"{p2}는 live-c-sentinel 축하를 좋아함","by":["p2"],"ev":["m2"]},
				  {"about":"p1","kind":"like","topic":"party","text":"{p1}는 live-b-sentinel 파티를 좋아함","by":["p1"],"ev":["m3"]}]}`
			}
			at := time.Now().Add(-3 * time.Hour)
			sayIn(e, at,
				said{"9211", "101", "민수", "나 새 동네로 이사했어 지난주에"}, said{"9212", "202", "영희", "오 정말? 축하해 이사"},
				said{"9213", "101", "민수", "응 고마워 집들이 할게"}, said{"9214", "202", "영희", "좋아 꼭 불러줘 집들이"})
			if rep := s.RunRoomMemory(t.Context()); rep.Episodes != 2 || rep.PeopleClaims() != 3 {
				t.Fatalf("control: two episodes and three live statements: %+v", rep)
			}
			var a, b string
			if err := s.Store.DB.QueryRow(`SELECT id FROM channel_episode WHERE title='이사'`).Scan(&a); err != nil {
				t.Fatal(err)
			}
			if err := s.Store.DB.QueryRow(`SELECT id FROM channel_episode WHERE title='집들이'`).Scan(&b); err != nil {
				t.Fatal(err)
			}
			if !items {
				if _, err := s.Store.DB.Exec(`DELETE FROM episode_item WHERE episode_id=?`, a); err != nil {
					t.Fatal(err)
				}
			}
			if r := exec(s, "memory.episodes.forget", map[string]any{"id": a}); !r.OK {
				t.Fatalf("forget: %+v", r)
			}
			if n := personClaimCount(t, s, "instr(text,'live-a-sentinel')>0"); n != 0 {
				t.Errorf("the forgotten episode's live statement remains: %d", n)
			}
			var left int
			if err := s.Store.DB.QueryRow(`SELECT COUNT(*) FROM channel_episode WHERE id=?`, b).Scan(&left); err != nil || left != 1 {
				t.Fatalf("the sibling episode went with it: %d %v", left, err)
			}
			if n := personClaimCount(t, s, "instr(text,'party-sentinel')>0"); n != 1 {
				t.Errorf("the sibling's derived statement: %d, want 1", n)
			}
			if n := personClaimCount(t, s, "instr(text,'live-b-sentinel')>0"); n != 1 {
				t.Errorf("the sibling's live statement went with the forget: %d", n)
			}
			want := 1 // a line no episode cites is not the forgotten episode's own
			if !items {
				want = 0 // the segment fallback: a line no sibling cites goes
			}
			if n := personClaimCount(t, s, "instr(text,'live-c-sentinel')>0"); n != want {
				t.Errorf("the live statement on a line no episode cites: %d, want %d", n, want)
			}
		})
	}
}
