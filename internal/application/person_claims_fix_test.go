package application_test

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"masamong/internal/application"
	"masamong/internal/config"
	"masamong/internal/memory/engine"
	"masamong/internal/memory/store"
)

// Round-1 review fixes of person memory v1.7.0 (read gate, write gate, derivation, forget and hide,
// relations, explicit and told statements, strict options).

// claimID is the id the write gate gives a claim.
func claimID(guild, subject, speaker, kind, topic string, evidence ...string) string {
	return store.PersonClaimID(guild, subject, speaker, kind, topic, evidence)
}

// item saves an episode item of server g with its own text, speakers and evidence, and derives its
// claims as insertItems does; it returns how many claims were written.
func (f reviewPerson) item(t *testing.T, channel, id, text string, by []string, ev ...string) int {
	t.Helper()
	refs := make([]string, len(by))
	for i, b := range by {
		refs[i] = store.PersonRef(b)
	}
	rawEv, _ := json.Marshal(ev)
	rawBy, _ := json.Marshal(refs)
	if _, err := f.Store.DB.Exec(`INSERT INTO channel_episode(id,guild_id,channel_id,room,kind,title,status,source,first_seen,last_seen,updated_at)
		VALUES(?,'g',?,?,'event',?,'done','discord',?,?,?)`, id, channel, engine.RoomKey("g", channel), id, f.at, f.at, f.at); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Store.DB.Exec(`INSERT INTO episode_item(id,episode_id,kind,text,evidence,by_refs,created_at) VALUES(?,?,'fact',?,?,?,?)`,
		id, id, text, string(rawEv), string(rawBy), f.at.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	n, err := f.DerivePersonItemForTest(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// Sol H1, Astra 4, Fable R5: /내기억 shows a member what a chat turn in that channel could show about
// them: no private thread statement from elsewhere (with its speaker), nothing pending, no hidden topic.
// The console sees every statement, a pending one marked as pending.
func TestMineDoesNotExportPrivateThreadClaim(t *testing.T) {
	t.Parallel()
	s, m, e := memorySvc(t)
	f := reviewPerson{s, m, e, time.Now().UTC().Add(-3 * time.Hour)}
	ctx := t.Context()
	a, b, d := store.PersonRef("101"), store.PersonRef("202"), store.PersonRef("404")
	f.source(t, "g", "c", "mine-label", "101", "민수", f.at)
	thread := f.source(t, "g", "pthread", "mine-thread-line", "202", "영희", f.at)
	f.fact(t, "g", "pthread", "mine-private-sentinel", "101", "202", thread)
	public := f.source(t, "g", "c", "mine-public-line", "202", "영희", f.at)
	f.fact(t, "g", "c", "mine-public-control", "101", "202", public)
	pending := store.PersonClaim{GuildID: "g", SubjectRef: a, SpeakerRef: b, Kind: "trait", Topic: "성격", Text: "{@" + a + "} mine-pending-sentinel",
		Evidence: []string{f.source(t, "g", "c", "mine-pending-line", "202", "영희", f.at)}, Source: "extract:seg-p", Link: store.LinkMention, State: store.PersonPending}
	hiddenLine := f.source(t, "g", "c", "mine-hidden-line", "202", "영희", f.at)
	hidden := store.PersonClaim{GuildID: "g", SubjectRef: a, SpeakerRef: b, Kind: "like", Topic: "음식", Text: "{@" + a + "}는 mine-hidden-first",
		Evidence: []string{hiddenLine}, Source: "extract:seg-h", Link: store.LinkMention}
	if n, err := s.SavePersonClaimsForTest(ctx, pending, hidden); err != nil || n != 2 {
		t.Fatalf("seed: %d (%v)", n, err)
	}
	if r := exec(s, "memory.person.hide", map[string]any{"guild_id": "g", "user_id": "101", "claim_id": claimID("g", a, b, "like", "음식", hiddenLine)}); !r.OK {
		t.Fatalf("hide: %+v", r)
	}
	again := hidden
	again.SpeakerRef, again.Text, again.Source = d, "{@"+a+"}는 mine-topic-sentinel", "extract:seg-a"
	again.Evidence = []string{f.source(t, "g", "c", "mine-again-line", "404", "진우", time.Now().UTC().Add(time.Minute))} // after the hide
	if n, err := s.SavePersonClaimsForTest(ctx, again); err != nil || n != 1 {
		t.Fatalf("the hidden topic said again: %d (%v)", n, err)
	}
	r := as(s, "memory.mine.show", map[string]any{}, inChannel("101"))
	if !r.OK {
		t.Fatalf("mine: %+v", r.Error)
	}
	person, _ := json.Marshal(r.Output["person"])
	card, _ := json.Marshal(r.Output["person_card"])
	if !strings.Contains(string(person), "mine-public-control") {
		t.Fatalf("control: the channel's own statement is missing: %s", person)
	}
	for _, gone := range []string{"mine-private-sentinel", "mine-pending-sentinel", "mine-topic-sentinel", "mine-hidden-first"} {
		if strings.Contains(string(person), gone) || strings.Contains(string(card), gone) {
			t.Errorf("/내기억 in #c shows %s:\nperson=%s\nperson_card=%s", gone, person, card)
		}
	}
	op := exec(s, "memory.mine.show", map[string]any{"guild_id": "g", "user_id": "101"})
	if !op.OK {
		t.Fatalf("console: %+v", op.Error)
	}
	states := map[string]string{}
	for _, c := range jsonList(op.Output["person"]) {
		states[strings.Fields(c["text"].(string))[1]] = c["state"].(string)
	}
	if states["mine-pending-sentinel"] != store.PersonPending || states["mine-private-sentinel"] != store.PersonActive {
		t.Errorf("the console sees every statement, a pending one marked: %v", states)
	}
	if card, _ := json.Marshal(op.Output["person_card"]); strings.Contains(string(card), "mine-pending-sentinel") {
		t.Errorf("a pending statement is a card line: %s", card)
	}
}

// memory.mine.show names the room each statement was said in (room_id, a room key). A member sees only
// the rooms the text itself passes the scope filter for, so a private thread's room never shows up in
// /내기억; the console sees it next to its statement.
func TestMineShowsClaimRoomWithinScope(t *testing.T) {
	t.Parallel()
	s, m, e := memorySvc(t)
	f := reviewPerson{s, m, e, time.Now().UTC().Add(-3 * time.Hour)}
	f.source(t, "g", "c", "room-label", "101", "민수", f.at)
	thread := f.source(t, "g", "pthread", "room-thread-line", "202", "영희", f.at)
	f.fact(t, "g", "pthread", "room-private-sentinel", "101", "202", thread)
	public := f.source(t, "g", "c", "room-public-line", "202", "영희", f.at)
	f.fact(t, "g", "c", "room-public-control", "101", "202", public)
	pub, priv := engine.RoomKey("g", "c"), engine.RoomKey("g", "pthread")

	roomOf := func(out map[string]any, word string) (string, bool) {
		for _, c := range jsonList(out["person"]) {
			if strings.Contains(c["text"].(string), word) {
				id, present := c["room_id"].(string)
				return id, present
			}
		}
		return "", false
	}
	r := as(s, "memory.mine.show", map[string]any{}, inChannel("101"))
	if !r.OK {
		t.Fatalf("mine: %+v", r.Error)
	}
	if id, ok := roomOf(r.Output, "room-public-control"); !ok || id != pub {
		t.Fatalf("a member's statement carries its room: %q (present %v), want %q", id, ok, pub)
	}
	// "items" is the member's own remembered lines (with their channel), a separate field; the statements
	// about the person are the "person" field
	raw, _ := json.Marshal(r.Output["person"])
	if strings.Contains(string(raw), priv) || strings.Contains(string(raw), "pthread") || strings.Contains(string(raw), "room-private-sentinel") {
		t.Errorf("a member's person statements name a room outside the channel scope: %s", raw)
	}
	op := exec(s, "memory.mine.show", map[string]any{"guild_id": "g", "user_id": "101"})
	if !op.OK {
		t.Fatalf("console: %+v", op.Error)
	}
	if id, ok := roomOf(op.Output, "room-private-sentinel"); !ok || id != priv {
		t.Errorf("the console names the thread's room: %q (present %v), want %q", id, ok, priv)
	}
	if id, ok := roomOf(op.Output, "room-public-control"); !ok || id != pub {
		t.Errorf("the console names the channel's room: %q (present %v), want %q", id, ok, pub)
	}
}

// Sol H2, Astra 5: an item naming two people is not the speaker's own statement about themselves, and
// an item two speakers wrote is not one speaker's own: neither travels server-wide. An item only about
// its speaker still does.
func TestDerivedClaimDoesNotExportOtherSubjectHearsay(t *testing.T) {
	t.Parallel()
	s, m, e := memorySvc(t)
	applyDoc(t, s, func(d *config.Document) { d.Guilds["g"].Channels["other"] = config.ChannelPolicy{Capture: true} })
	f := reviewPerson{s, m, e, time.Now().UTC().Add(-3 * time.Hour)}
	a, b := store.PersonRef("101"), store.PersonRef("202")
	f.source(t, "g", "c", "mixed-label-a", "101", "민수", f.at)
	f.source(t, "g", "c", "mixed-label-b", "202", "영희", f.at)
	f.item(t, "other", "mixed-item", "{@"+a+"}가 전하길 {@"+b+"}는 mixed-job-sentinel", []string{"101"},
		f.source(t, "g", "other", "mixed-line", "101", "민수", f.at))
	f.item(t, "other", "multi-item", "{@"+a+"} multi-speaker-sentinel", []string{"101", "202"},
		f.source(t, "g", "other", "multi-line-a", "101", "민수", f.at), f.source(t, "g", "other", "multi-line-b", "202", "영희", f.at))
	f.item(t, "other", "own-item", "{@"+a+"} own-fact-control", []string{"101"}, f.source(t, "g", "other", "own-line", "101", "민수", f.at))
	got := f.askIn(t, "c", "<@101> 어떤 사람이야?")
	if !strings.Contains(got, "own-fact-control") {
		t.Fatalf("control: the person's own statement from #other: %q", got)
	}
	for _, gone := range []string{"mixed-job-sentinel", "multi-speaker-sentinel"} {
		if strings.Contains(got, gone) {
			t.Errorf("%s travelled to #c as 민수's own statement: %q", gone, got)
		}
	}
	if n := personClaimCount(t, s, "subject_ref=speaker_ref AND (instr(text,'mixed-job')>0 OR instr(text,'multi-speaker')>0)"); n != 0 {
		t.Errorf("self claims from a mixed item: %d", n)
	}
	if n := personClaimCount(t, s, "subject_ref=? AND speaker_ref=? AND instr(text,'mixed-job')>0", b, a); n != 1 {
		t.Errorf("the hearsay about 영희 is kept for #other: %d", n)
	}
}

// Sol H5 (explicit half): forgetting an explicit episode, or a room's memory, removes the explicit
// claims resting on it.
func TestEpisodeForgetRemovesExplicitClaims(t *testing.T) {
	t.Parallel()
	for _, by := range []string{"id", "room"} {
		t.Run(by, func(t *testing.T) {
			t.Parallel()
			s, m, e := memorySvc(t)
			f := reviewPerson{s, m, e, time.Now().UTC().Add(-time.Hour)}
			f.source(t, "g", "c", "ef-line", "101", "민수", f.at)
			m.explicitAnswer(`{"intent":"remember","about":"self","fact":"{p1}는 explicit-cat-sentinel을 키운다","key":"pet","instruction":false}`)
			turn(t, s, "101", "민수", "기억해, 나 고양이 키워", live("ef-line"))
			if n := personClaimCount(t, s, "source=?", store.SourceExplicit); n != 1 {
				t.Fatalf("explicit claim: %d", n)
			}
			ask := func() string { return f.askIn(t, "c", "<@101> 어떤 사람이야?") }
			if got := ask(); !strings.Contains(got, "explicit-cat-sentinel") {
				t.Fatalf("control: %q", got)
			}
			in := map[string]any{"guild_id": "g", "channel_id": "c"}
			if by == "id" {
				var ep string
				if err := s.Store.DB.QueryRow(`SELECT episode_id FROM memory_explicit`).Scan(&ep); err != nil {
					t.Fatal(err)
				}
				in = map[string]any{"id": ep}
			}
			if r := exec(s, "memory.episodes.forget", in); !r.OK {
				t.Fatalf("forget: %+v", r)
			}
			if n := personClaimCount(t, s, "source=?", store.SourceExplicit); n != 0 {
				t.Errorf("explicit claims left after the episode was forgotten: %d", n)
			}
			s.RunRoomMemory(context.Background())
			r := as(s, "memory.mine.show", map[string]any{}, inChannel("101"))
			if raw, _ := json.Marshal(r.Output); strings.Contains(string(raw), "explicit-cat-sentinel") {
				t.Errorf("/내기억 after the forget: %s", raw)
			}
			if got := ask(); strings.Contains(got, "explicit-cat-sentinel") {
				t.Errorf("chat after the forget: %q", got)
			}
		})
	}
}

// Astra 12: a correction a member made in v1.6 (an item marked wrong in an episode kept from
// regeneration) is not written again as a claim from the same lines.
func TestCorrectionFromV16NotResurrected(t *testing.T) {
	t.Parallel()
	s, m, e := memorySvc(t)
	f := reviewPerson{s, m, e, time.Now().UTC().Add(-3 * time.Hour)}
	a := store.PersonRef("101")
	wrong := f.source(t, "g", "c", "v16-wrong-line", "101", "민수", f.at)
	kept := f.source(t, "g", "c", "v16-kept-line", "101", "민수", f.at)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO channel_episode(id,guild_id,channel_id,room,kind,title,status,source,first_seen,last_seen,updated_at,regen)
			VALUES('v16-ep','g','c','discord:g:c','event','t','done','discord',?,?,?,3)`, []any{f.at, f.at, f.at}},
		{`INSERT INTO episode_item(id,episode_id,kind,text,evidence,by_refs,created_at,state) VALUES('v16-wrong','v16-ep','fact',?,?,?,?,'hidden')`,
			[]any{"{@" + a + "}는 부산에 삶", `["` + wrong + `"]`, `["` + a + `"]`, f.at.Format(time.RFC3339Nano)}},
		{`INSERT INTO episode_item(id,episode_id,kind,text,evidence,by_refs,created_at,state) VALUES('v16-kept','v16-ep','fact',?,?,?,?,'active')`,
			[]any{"{@" + a + "}는 국수를 좋아함", `["` + kept + `"]`, `["` + a + `"]`, f.at.Format(time.RFC3339Nano)}},
		{`INSERT INTO evidence_refs(source_id,holder_kind,holder_id) VALUES(?,'episode_item','v16-wrong'),(?,'episode_item','v16-kept')`, []any{wrong, kept}},
	} {
		if _, err := s.Store.DB.Exec(q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	corrected := store.PersonClaim{GuildID: "g", SubjectRef: a, SpeakerRef: a, Kind: "status", Topic: "residence", Text: "{@" + a + "}는 부산에 삶",
		Evidence: []string{wrong}, Source: "backfill:pb_test", Link: store.LinkSelf}
	if n, err := s.SavePersonClaimsForTest(t.Context(), corrected); err != nil || n != 0 {
		t.Errorf("a statement from a line the member corrected in v1.6 was written: %d (%v)", n, err)
	}
	control := corrected
	control.Kind, control.Topic, control.Text, control.Evidence = "like", "음식", "{@"+a+"}는 국수를 좋아함", []string{kept}
	if n, err := s.SavePersonClaimsForTest(t.Context(), control); err != nil || n != 1 {
		t.Errorf("control: a line of the same episode that was not corrected: %d (%v)", n, err)
	}
}

// Astra 13: "that's wrong" or "forget that" about a remembered fact on someone else (a told claim, the
// fact naming them by plain name) removes the told claim, and it is not written again.
func TestTargetedCorrectionRemovesToldClaim(t *testing.T) {
	t.Parallel()
	for _, how := range []string{"wrong", "forget"} {
		t.Run(how, func(t *testing.T) {
			t.Parallel()
			s, m, e := memorySvc(t)
			f := reviewPerson{s, m, e, time.Now().UTC().Add(-time.Hour)}
			b := store.PersonRef("202")
			f.source(t, "g", "c", "told-label", "202", "영희", f.at)
			f.source(t, "g", "c", "told-line", "101", "민수", f.at.Add(time.Minute))
			m.explicitAnswer(`{"intent":"remember","about":"other","fact":"영희는 told-tea-sentinel을 좋아함","key":"drink","instruction":false}`)
			turn(t, s, "101", "민수", "기억해, 영희는 told-tea-sentinel 좋아해", live("told-line"))
			told := store.PersonClaim{GuildID: "g", Source: store.SourceExplicit}
			var ev string
			if err := s.Store.DB.QueryRow(`SELECT subject_ref,speaker_ref,kind,topic,text,evidence,link FROM person_claim WHERE source=? AND subject_ref=?`, store.SourceExplicit, b).
				Scan(&told.SubjectRef, &told.SpeakerRef, &told.Kind, &told.Topic, &told.Text, &ev, &told.Link); err != nil {
				t.Fatalf("the told claim: %v", err)
			}
			_ = json.Unmarshal([]byte(ev), &told.Evidence)
			ask := func() string { return f.askIn(t, "c", "<@202> 어떤 사람이야?") }
			if got := ask(); !strings.Contains(got, "told-tea-sentinel") {
				t.Fatalf("control: %q", got)
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
			if n := personClaimCount(t, s, "subject_ref=? AND state<>?", b, store.PersonHidden); n != 0 { // wrong hides, forget deletes
				t.Errorf("the told claim is left: %d", n)
			}
			if got := ask(); strings.Contains(got, "told-tea-sentinel") {
				t.Errorf("chat still repeats it: %q", got)
			}
			if n, err := s.SavePersonClaimsForTest(t.Context(), told); err != nil || n != 0 {
				t.Errorf("written again: %d (%v)", n, err)
			}
		})
	}
}

// Sol medium (topic controls), Astra 18: a topic's synonyms are one key for hiding and supersession; a
// direct statement that is not usable does not reopen a hidden topic; a hidden derived statement stays
// hidden when another item says the same.
func TestPersonTopicControls(t *testing.T) {
	t.Parallel()
	a, b, d := store.PersonRef("101"), store.PersonRef("202"), store.PersonRef("404")
	// visible says what the person block shows about 민수, with a statement no control hides as the
	// control: the block is there, and a statement left out is left out by the control under test
	visible := func(t *testing.T, f reviewPerson) string {
		t.Helper()
		got := f.askIn(t, "c", "<@101> 어떤 사람이야?")
		if !strings.Contains(got, "주말마다 등산") {
			t.Fatalf("control: the person block about 민수: %q", got)
		}
		return got
	}
	hiker := func(t *testing.T, f reviewPerson, at time.Time) {
		t.Helper()
		c := store.PersonClaim{GuildID: "g", SubjectRef: a, SpeakerRef: b, Kind: "like", Topic: "취미", Text: "{@" + a + "}는 주말마다 등산을 다님",
			Evidence: []string{f.source(t, "g", "c", "hiker-line", "202", "영희", at)}, Source: "extract:seg-hiker", Link: store.LinkMention}
		if n, err := f.SavePersonClaimsForTest(t.Context(), c); err != nil || n != 1 {
			t.Fatalf("control statement: %d (%v)", n, err)
		}
	}
	tests := map[string]func(*testing.T, reviewPerson){
		"SynonymHide": func(t *testing.T, f reviewPerson) {
			f.source(t, "g", "c", "syn-label", "101", "민수", f.at)
			hiker(t, f, f.at)
			l1 := f.source(t, "g", "c", "syn-1", "202", "영희", f.at)
			first := store.PersonClaim{GuildID: "g", SubjectRef: a, SpeakerRef: b, Kind: "status", Topic: "job", Text: "{@" + a + "}는 syn-teacher-sentinel",
				Evidence: []string{l1}, Source: "extract:seg-1", Link: store.LinkMention}
			if n, err := f.SavePersonClaimsForTest(t.Context(), first); err != nil || n != 1 {
				t.Fatalf("seed: %d (%v)", n, err)
			}
			if r := exec(f.Service, "memory.person.hide", map[string]any{"guild_id": "g", "user_id": "101", "claim_id": claimID("g", a, b, "status", "job", l1)}); !r.OK {
				t.Fatalf("hide: %+v", r)
			}
			second := first
			second.SpeakerRef, second.Topic, second.Text, second.Source = d, "직업", "{@"+a+"}는 syn-doctor-sentinel", "extract:seg-2"
			second.Evidence = []string{f.source(t, "g", "c", "syn-2", "404", "진우", time.Now().UTC().Add(time.Minute))} // after the hide
			if n, err := f.SavePersonClaimsForTest(t.Context(), second); err != nil || n != 1 {
				t.Fatalf("later hearsay: %d (%v)", n, err)
			}
			if got := visible(t, f); strings.Contains(got, "syn-doctor-sentinel") {
				t.Errorf("a hidden topic came back under its synonym: %q", got)
			}
		},
		"SynonymSupersede": func(t *testing.T, f reviewPerson) {
			older := store.PersonClaim{GuildID: "g", SubjectRef: a, SpeakerRef: a, Kind: "status", Topic: "job", Text: "{@" + a + "}는 고등학교 교사로 일함",
				Evidence: []string{f.source(t, "g", "c", "sup-1", "101", "민수", f.at)}, Source: "extract:seg-1", Link: store.LinkSelf}
			newer := older
			newer.Topic, newer.Text, newer.Source = "직업", "{@"+a+"}가 이번에 종합병원 의사가 됨", "extract:seg-2"
			newer.Evidence = []string{f.source(t, "g", "c", "sup-2", "101", "민수", f.at.Add(time.Hour))}
			if n, err := f.SavePersonClaimsForTest(t.Context(), older, newer); err != nil || n != 2 {
				t.Fatalf("seed: %d (%v)", n, err)
			}
			if got := f.askIn(t, "c", "<@101> 어떤 사람이야?"); !strings.Contains(got, "종합병원") || strings.Contains(got, "교사") {
				t.Errorf("the newer statement under a synonym replaces the older: %q", got)
			}
		},
		"TombstonedReassertion": func(t *testing.T, f reviewPerson) {
			hiker(t, f, f.at)
			l1 := f.source(t, "g", "c", "tomb-gossip-1", "202", "영희", f.at)
			gossip := store.PersonClaim{GuildID: "g", SubjectRef: a, SpeakerRef: b, Kind: "like", Topic: "음식", Text: "{@" + a + "}는 tomb-first-sentinel",
				Evidence: []string{l1}, Source: "extract:seg-1", Link: store.LinkMention}
			if n, err := f.SavePersonClaimsForTest(t.Context(), gossip); err != nil || n != 1 {
				t.Fatalf("seed: %d (%v)", n, err)
			}
			if r := exec(f.Service, "memory.person.hide", map[string]any{"guild_id": "g", "user_id": "101", "claim_id": claimID("g", a, b, "like", "음식", l1)}); !r.OK {
				t.Fatalf("hide: %+v", r)
			}
			again := gossip
			again.SpeakerRef, again.Text, again.Source = d, "{@"+a+"}는 잔치국수를 좋아함", "extract:seg-2"
			later := time.Now().UTC().Add(time.Minute) // after the hide
			again.Evidence = []string{f.source(t, "g", "c", "tomb-gossip-2", "404", "진우", later)}
			own := store.PersonClaim{GuildID: "g", SubjectRef: a, SpeakerRef: a, Kind: "like", Topic: "음식", Text: "{@" + a + "}가 직접 밝힌 최애 메뉴는 냉면",
				Evidence: []string{f.source(t, "g", "c", "tomb-own", "101", "민수", later.Add(time.Minute))}, Source: "extract:seg-3", Link: store.LinkSelf}
			if n, err := f.SavePersonClaimsForTest(t.Context(), again, own); err != nil || n != 2 {
				t.Fatalf("later statements: %d (%v)", n, err)
			}
			f.source(t, "g", "c", "tomb-label", "101", "민수", later.Add(2*time.Minute)) // 민수's name stays readable
			if got := visible(t, f); !strings.Contains(got, "잔치국수") {
				t.Fatalf("control: the person's own statement reopens the topic: %q", got)
			}
			// the person's own line is deleted; the tombstone is committed, no invalidation pass ran yet
			if _, err := f.Store.DB.Exec(`INSERT INTO privacy_tombstones(kind,source_id,at) VALUES('message','tomb-own',?)`, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
				t.Fatal(err)
			}
			if got := visible(t, f); strings.Contains(got, "잔치국수") || strings.Contains(got, "냉면") {
				t.Errorf("a deleted statement of the person reopens a hidden topic: %q", got)
			}
		},
		"DerivedHearsay": func(t *testing.T, f reviewPerson) {
			f.source(t, "g", "c", "dh-label", "101", "민수", f.at)
			e1 := f.source(t, "g", "c", "dh-1", "202", "영희", f.at)
			f.item(t, "c", "dh-item-1", "{@"+a+"} dh-noodle-sentinel", []string{"202"}, e1)
			f.item(t, "c", "dh-item-2", "{@"+a+"} dh-noodle-sentinel", []string{"202"}, f.source(t, "g", "c", "dh-2", "202", "영희", f.at.Add(time.Minute)))
			if got := f.askIn(t, "c", "<@101> 어떤 사람이야?"); !strings.Contains(got, "dh-noodle-sentinel") {
				t.Fatalf("control: %q", got)
			}
			if r := exec(f.Service, "memory.person.hide", map[string]any{"guild_id": "g", "user_id": "101", "claim_id": claimID("g", a, b, "other", "", e1)}); !r.OK {
				t.Fatalf("hide: %+v", r)
			}
			if got := f.askIn(t, "c", "<@101> 어떤 사람이야?"); strings.Contains(got, "dh-noodle-sentinel") {
				t.Errorf("the hidden hearsay came back through another item: %q", got)
			}
			if n := f.item(t, "c", "dh-item-3", "{@"+a+"} dh-noodle-sentinel", []string{"202"}, f.source(t, "g", "c", "dh-3", "202", "영희", f.at.Add(2*time.Minute))); n != 0 {
				t.Errorf("the hidden hearsay was derived again from a new item: %d", n)
			}
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, m, e := memorySvc(t)
			test(t, reviewPerson{s, m, e, time.Now().UTC().Add(-3 * time.Hour)})
		})
	}
}

// Sol medium, Astra 17: on a no_sensitive server, an ordinary "remember this" about oneself is active at
// once (the explicit path classified it when it was asked for).
func TestNoSensitiveExplicitSelfIsActive(t *testing.T) {
	t.Parallel()
	s, m, e := memorySvc(t)
	strict(t, s, config.GuildPrivacy{NoSensitive: true})
	f := reviewPerson{s, m, e, time.Now().UTC().Add(-time.Hour)}
	f.source(t, "g", "c", "ns-line", "101", "민수", f.at)
	m.explicitAnswer(`{"intent":"remember","about":"self","fact":"{p1}는 ns-noodle-sentinel을 좋아함","key":"food","instruction":false}`)
	turn(t, s, "101", "민수", "기억해, 나 국수 좋아해", live("ns-line"))
	if a := m.last("answer"); !strings.Contains(a, `"saved":true`) {
		t.Fatalf("saved: %s", a)
	}
	if n := personClaimCount(t, s, "source=? AND state=?", store.SourceExplicit, store.PersonActive); n != 1 {
		t.Errorf("the explicit claim is active: %d (pending %d)", n, personClaimCount(t, s, "state=?", store.PersonPending))
	}
	turn(t, s, "101", "민수", "날 기억해?", func(in *application.ChatInput) { in.Locale = "ko" })
	if got := block(m.last("answer"), cardOpen, cardClose); !strings.Contains(got, "ns-noodle-sentinel") {
		t.Errorf("the caller's own remembered fact: %q", got)
	}
}

// Sol medium (claim identity), Fable R4: two statements of different kinds or topics resting on one line
// are two claims.
func TestPersonClaimKeepsDistinctFactsFromOneMessage(t *testing.T) {
	t.Parallel()
	s, m, e := memorySvc(t)
	f := reviewPerson{s, m, e, time.Now().UTC().Add(-3 * time.Hour)}
	a := store.PersonRef("101")
	ev := f.source(t, "g", "c", "r4-line", "101", "민수", f.at) // "나 부산 살고 국수 좋아해"
	home := store.PersonClaim{GuildID: "g", SubjectRef: a, SpeakerRef: a, Kind: "status", Topic: "residence", Text: "{@" + a + "}는 부산에 삼",
		Evidence: []string{ev}, Source: "backfill:pb_test", Link: store.LinkSelf}
	food := home
	food.Kind, food.Topic, food.Text = "like", "음식", "{@"+a+"}는 국수를 좋아함"
	n, err := s.SavePersonClaimsForTest(context.Background(), home, food)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 || personClaimCount(t, s, "subject_ref=?", a) != 2 {
		t.Errorf("two statements from one line: %d written, %d stored", n, personClaimCount(t, s, "subject_ref=?", a))
	}
	if n, err := s.SavePersonClaimsForTest(context.Background(), food, home); err != nil || n != 0 {
		t.Errorf("written again: %d (%v)", n, err)
	}
}

// Astra 23, Fable R3: the read takes the usable statements before its row limit: hearsay from another
// channel group does not crowd out the person's own older statement.
func TestEligibleClaimsBeforeLimit(t *testing.T) {
	t.Parallel()
	s, m, e := memorySvc(t)
	applyDoc(t, s, func(d *config.Document) { d.Guilds["g"].Channels["other"] = config.ChannelPolicy{Capture: true} })
	f := reviewPerson{s, m, e, time.Now().UTC().Add(-6 * time.Hour)}
	ctx := context.Background()
	a, b := store.PersonRef("101"), store.PersonRef("202")
	own := f.source(t, "g", "c", "r3-own", "101", "민수", f.at)
	self := store.PersonClaim{GuildID: "g", SubjectRef: a, SpeakerRef: a, Kind: "like", Topic: "음료", Text: "{@" + a + "}는 차를 좋아함",
		Evidence: []string{own}, Source: "backfill:pb_test", Link: store.LinkSelf}
	if n, err := s.SavePersonClaimsForTest(ctx, self); err != nil || n != 1 {
		t.Fatalf("own statement: %d (%v)", n, err)
	}
	ask := func() string { return block(f.ask(t, "<@101> 어떤 사람이야?"), cardOpen, cardClose) }
	if got := ask(); !strings.Contains(got, "차를 좋아함") {
		t.Fatalf("control: %q", got)
	}
	var gossip []store.PersonClaim
	for i := range 201 {
		ev := f.source(t, "g", "other", "r3-gossip-"+strconv.Itoa(i), "202", "영희", f.at.Add(time.Minute+time.Duration(i)*time.Second))
		gossip = append(gossip, store.PersonClaim{GuildID: "g", SubjectRef: a, SpeakerRef: b, Kind: "other", Topic: "잡담" + strconv.Itoa(i),
			Text: "{@" + a + "} 이야기 " + strconv.Itoa(i), Evidence: []string{ev}, Source: "backfill:pb_test", Link: store.LinkMention})
	}
	if n, err := s.SavePersonClaimsForTest(ctx, gossip...); err != nil || n != 201 {
		t.Fatalf("hearsay in another channel: %d (%v)", n, err)
	}
	if got := ask(); !strings.Contains(got, "차를 좋아함") {
		t.Errorf("the person's own statement is no longer read in #c once 201 newer statements exist in #other: %q", got)
	}
}

// Fable H1/R1: /잊어줘 (all) by a person a statement names in its text, neither its subject nor its
// speaker, removes the statement (the preview counts it), and it is not written again.
func TestForgetAllRemovesStatementsNamingThePerson(t *testing.T) {
	t.Parallel()
	s, m, e := memorySvc(t)
	f := reviewPerson{s, m, e, time.Now().UTC().Add(-3 * time.Hour)}
	ctx := context.Background()
	a, b := store.PersonRef("101"), store.PersonRef("202")
	f.source(t, "g", "c", "r1-label-b", "202", "영희", f.at)
	f.source(t, "g", "c", "r1-label-a", "101", "민수", f.at)
	// said by a third member about 민수, naming 영희 (a statement of one's own names only its subject)
	ev := f.source(t, "g", "c", "r1-said", "404", "준호", f.at)
	c := store.PersonClaim{GuildID: "g", SubjectRef: a, SpeakerRef: store.PersonRef("404"), Kind: "relation", Topic: "친구",
		Text: "{@" + a + "}는 {@" + b + "}와 매주 술을 마심", Evidence: []string{ev}, Source: "backfill:pb_test", Link: store.LinkMention}
	if n, err := s.SavePersonClaimsForTest(ctx, c); err != nil || n != 1 {
		t.Fatalf("control write: %d (%v)", n, err)
	}
	ask := func() string { return block(f.ask(t, "<@101> 어떤 사람이야?"), cardOpen, cardClose) }
	if got := ask(); !strings.Contains(got, "영희와 매주 술을 마심") {
		t.Fatalf("control read: %q", got)
	}
	req := func(confirm any) map[string]any {
		return map[string]any{"guild_id": "g", "user_id": "202", "scope": "all", "confirm": confirm}
	}
	p := exec(s, "memory.mine.forget", req(""))
	if !p.OK || p.Output["person_items"] != 1 {
		t.Errorf("the preview counts the statement naming 영희: %+v", p.Output)
	}
	if r := exec(s, "memory.mine.forget", req(p.Output["token"])); !r.OK || r.Output["applied"] != true {
		t.Fatalf("forget: %+v", r)
	}
	if n := personClaimCount(t, s, "instr(text, ?)>0", "{@"+b+"}"); n != 0 {
		t.Errorf("after 영희's forget-all, %d stored statement(s) still name her", n)
	}
	if got := ask(); strings.Contains(got, "영희") {
		t.Errorf("after 영희's forget-all the person block still names her: %q", got)
	}
	if _, err := s.Store.DB.Exec(`DELETE FROM person_claim`); err != nil {
		t.Fatal(err)
	}
	if n, err := s.SavePersonClaimsForTest(ctx, c); err != nil || n != 0 {
		t.Errorf("a statement naming a person who forgot everything was written again: %d (%v)", n, err)
	}
}

// Fable H2/R2: a hidden statement stays hidden when another writer cites another subset of its lines
// with another kind and topic.
func TestHiddenStatementStaysHiddenWithOtherLines(t *testing.T) {
	t.Parallel()
	s, m, e := memorySvc(t)
	f := reviewPerson{s, m, e, time.Now().UTC().Add(-3 * time.Hour)}
	ctx := context.Background()
	a, b := store.PersonRef("101"), store.PersonRef("202")
	f.source(t, "g", "c", "r2-label", "101", "민수", f.at)
	e1 := f.source(t, "g", "c", "r2-said-1", "202", "영희", f.at)
	e2 := f.source(t, "g", "c", "r2-said-2", "202", "영희", f.at.Add(time.Second))
	derived := store.PersonClaim{GuildID: "g", SubjectRef: a, SpeakerRef: b, Kind: "other", Text: "{@" + a + "}는 국수를 좋아함",
		Evidence: []string{e1, e2}, Source: "extract:seg-1", Link: store.LinkMention}
	if n, err := s.SavePersonClaimsForTest(ctx, derived); err != nil || n != 1 {
		t.Fatalf("control write: %d (%v)", n, err)
	}
	if r := exec(s, "memory.person.hide", map[string]any{"guild_id": "g", "user_id": "101", "claim_id": claimID("g", a, b, "other", "", e1, e2)}); !r.OK {
		t.Fatalf("hide: %+v", r)
	}
	ask := func() string { return block(f.ask(t, "<@101> 어떤 사람이야?"), cardOpen, cardClose) }
	if got := ask(); strings.Contains(got, "국수") {
		t.Fatalf("control: hidden statement shown: %q", got)
	}
	again := store.PersonClaim{GuildID: "g", SubjectRef: a, SpeakerRef: b, Kind: "like", Topic: "음식", Text: "{@" + a + "}는  국수를 좋아함",
		Evidence: []string{e1}, Source: "backfill:pb_test", Link: store.LinkMention}
	if n, err := s.SavePersonClaimsForTest(ctx, again); err != nil || n != 0 {
		t.Errorf("the hidden statement was written again from a subset of its lines: %d (%v)", n, err)
	}
	if got := ask(); strings.Contains(got, "국수") {
		t.Errorf("the hidden statement is shown again: %q", got)
	}
}

// Astra 22, Sol medium: a private thread the gateway knows has its own statements inside it, and only
// there.
func TestPersonClaimKnownPrivateThread(t *testing.T) {
	t.Parallel()
	s, m, e := memorySvc(t)
	f := reviewPerson{s, m, e, time.Now().UTC().Add(-3 * time.Hour)}
	f.source(t, "g", "c", "known-label", "101", "민수", f.at)
	ev := f.source(t, "g", "private", "known-line", "101", "민수", f.at)
	f.fact(t, "g", "private", "known-thread-marker", "101", "101", ev)
	if got := f.askIn(t, "private", "<@101> 어떤 사람이야?"); !strings.Contains(got, "known-thread-marker") {
		t.Errorf("inside the thread it was said in: %q", got)
	}
	for _, channel := range []string{"c", "other"} {
		if got := f.askIn(t, channel, "<@101> 어떤 사람이야?"); strings.Contains(got, "known-thread-marker") {
			t.Errorf("a private thread statement in #%s: %q", channel, got)
		}
	}
}

// Fable H1/H2 hook: the check the backfill's stored results go through before they are shown or
// written (forget-all of anyone named, hide of the same words, a hidden topic).
func TestPersonStatementSuppressed(t *testing.T) {
	t.Parallel()
	s, m, e := memorySvc(t)
	f := reviewPerson{s, m, e, time.Now().UTC().Add(-3 * time.Hour)}
	ctx := t.Context()
	a, b, d := store.PersonRef("101"), store.PersonRef("202"), store.PersonRef("404")
	l1 := f.source(t, "g", "c", "hook-1", "202", "영희", f.at)
	l2 := f.source(t, "g", "c", "hook-2", "202", "영희", f.at.Add(time.Minute))
	base := store.PersonClaim{GuildID: "g", SubjectRef: a, SpeakerRef: b, Kind: "like", Topic: "음식", Text: "{@" + a + "}는 국수를 좋아함",
		Evidence: []string{l1}, Source: "backfill:pb_test", Link: store.LinkMention}
	check := func(c store.PersonClaim) bool {
		t.Helper()
		hit, err := s.PersonStatementSuppressedForTest(ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		return hit
	}
	if check(base) {
		t.Fatal("control: nothing hidden or forgotten yet")
	}
	if n, err := s.SavePersonClaimsForTest(ctx, base); err != nil || n != 1 {
		t.Fatalf("seed: %d (%v)", n, err)
	}
	if r := exec(s, "memory.person.hide", map[string]any{"guild_id": "g", "user_id": "101", "claim_id": claimID("g", a, b, "like", "음식", l1)}); !r.OK {
		t.Fatalf("hide: %+v", r)
	}
	same := base
	same.Kind, same.Topic, same.Evidence = "other", "", []string{l2}
	if !check(same) {
		t.Error("the same words on another line after a hide")
	}
	topic := base
	topic.Text, topic.Evidence = "{@"+a+"}는 냉면을 좋아함", []string{l2}
	if !check(topic) {
		t.Error("another speaker's statement on a hidden topic")
	}
	naming := store.PersonClaim{GuildID: "g", SubjectRef: b, SpeakerRef: b, Kind: "relation", Topic: "친구", Text: "{@" + b + "}는 {@" + d + "}와 친함",
		Evidence: []string{l2}, Source: "backfill:pb_test", Link: store.LinkSelf}
	if check(naming) {
		t.Fatal("control: 진우 has not forgotten anything yet")
	}
	req := func(confirm any) map[string]any {
		return map[string]any{"guild_id": "g", "user_id": "404", "scope": "all", "confirm": confirm}
	}
	p := exec(s, "memory.mine.forget", req(""))
	if r := exec(s, "memory.mine.forget", req(p.Output["token"])); !r.OK || r.Output["applied"] != true {
		t.Fatalf("forget: %+v", r)
	}
	if !check(naming) {
		t.Error("a statement naming a person who forgot everything")
	}
	undated := naming
	undated.Evidence = nil
	if !check(undated) {
		t.Error("a statement with no evidence time fails closed")
	}
}
