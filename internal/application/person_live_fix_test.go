package application_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/config"
	"masamong/internal/memory/engine"
	"masamong/internal/memory/store"
)

// Round-2 fixes of live extraction (person memory v1.7.0). Synthetic names, ids and text only.

// In #c, 영희 says 민수 has a hair marker and 민수 denies it. A model entry that
// cites both lines and lists both authors must not become 민수's own (server-wide) statement, nor any
// claim at all: it is skipped and counted. Neither the answer nor the picture prompt in #other carries
// it. 민수's genuinely self-authored statement (the control) is saved and travels as before.
func TestLiveMixedEvidenceCannotBecomeSelfMemory(t *testing.T) {
	t.Parallel()
	f, host := pictureSvc(t)
	applyDoc(t, f.Service, func(d *config.Document) { d.Guilds["g"].Channels["other"] = config.ChannelPolicy{Capture: true} })
	f.model.extract = func(string) string {
		return `{"episodes":[],"people":[
			{"about":"p2","kind":"appearance","topic":"머리","text":"{p2}는 r2-hair-marker 머리를 함","by":["p1","p2"],"ev":["m1","m2"]},
			{"about":"p2","kind":"appearance","topic":"안경","text":"{p2}는 r2-glasses-control 안경을 씀","by":["p2"],"ev":["m3"]}]}`
	}
	sayIn(f.engine, f.at,
		said{"9201", "202", "영희", "민수 너 r2-hair-marker 머리 했다며"}, said{"9202", "101", "민수", "아니 그거 사실 아니야 누가 그래"},
		said{"9203", "101", "민수", "나는 r2-glasses-control 안경은 써"}, said{"9204", "202", "영희", "아 그렇구나 알았어"})
	f.source(t, "g", "c", "label-303", "303", "지수", f.at.Add(-time.Hour))
	rep := f.RunRoomMemory(context.Background())
	if rep.PeopleClaims() != 1 || rep.PeopleSkipped() != 1 {
		t.Fatalf("pass: %+v claims=%d skipped=%d", rep, rep.PeopleClaims(), rep.PeopleSkipped())
	}
	a := store.PersonRef("101")
	if n := personClaimCount(t, f.Service, "instr(text,'r2-hair-marker')>0"); n != 0 {
		t.Errorf("the denied fact was stored: %d row(s)", n)
	}
	if n := personClaimCount(t, f.Service, "subject_ref=? AND speaker_ref=? AND link='self' AND instr(text,'r2-glasses-control')>0", a, a); n != 1 {
		t.Fatalf("control: 민수's own statement was not saved as self: %d row(s)", n)
	}
	got := f.askIn(t, "other", "<@101> 어떤 사람이야?")
	if strings.Contains(got, "r2-hair-marker") {
		t.Errorf("the answer prompt in #other carries the denied fact: %q", got)
	}
	if !strings.Contains(got, "r2-glasses-control") {
		t.Errorf("control: the self statement is not read in #other: %q", got)
	}
	if r := as(f.Service, "images.generate", map[string]any{"prompt": "초상화", "subjects": []any{"<@101>"}},
		&action.Invoker{Via: "command", GuildID: "g", ChannelID: "other", UserID: "303"}); !r.OK {
		t.Fatalf("draw: %+v", r)
	}
	drawn := host.drawnPrompt(t)
	if strings.Contains(drawn, "r2-hair-marker") {
		t.Errorf("the picture prompt in #other carries the denied fact:\n%s", drawn)
	}
	if !strings.Contains(drawn, "r2-glasses-control") {
		t.Errorf("control: the picture prompt lacks the self statement:\n%s", drawn)
	}
}

// 민수 says something about 영희 and 영희 only reacts. The fact the model cites from both lines is not
// stored as 영희's own statement, is not read in #other, and nothing of it remains once 민수 deletes his
// line.
func TestLiveSplitMakesHearsayASelfStatement(t *testing.T) {
	t.Parallel()
	s, m, e := memorySvc(t)
	applyDoc(t, s, func(d *config.Document) { d.Guilds["g"].Channels["other"] = config.ChannelPolicy{Capture: true} })
	m.extract = func(string) string {
		return `{"episodes":[],"people":[{"about":"p2","kind":"status","topic":"job","text":"{p2}는 r2-job-sentinel 회사로 이직함","by":["p1","p2"],"ev":["m1","m2"]},
			{"about":"p1","kind":"like","topic":"소문","text":"{p1}는 소문 듣기를 좋아함","by":["p1"],"ev":["m3"]}]}`
	}
	at := time.Now().Add(-3 * time.Hour)
	sayIn(e, at,
		said{"9001", "101", "민수", "영희 너 r2-job-sentinel 회사로 이직했다며"}, said{"9002", "202", "영희", "뭐? 누가 그래 그런 말을"},
		said{"9003", "101", "민수", "아니 그냥 소문으로 들었어"}, said{"9004", "202", "영희", "아 그렇구나 알았어 고마워"})
	if rep := s.RunRoomMemory(context.Background()); rep.PeopleClaims() == 0 {
		t.Fatalf("control: the extraction saved nothing: %+v", rep)
	}
	b := store.PersonRef("202")
	if n := personClaimCount(t, s, "subject_ref=? AND speaker_ref=? AND instr(text,'r2-job-sentinel')>0", b, b); n != 0 {
		t.Errorf("a fact cited from two people's lines was stored as the subject's own statement: %d row(s)", n)
	}
	f := reviewPerson{s, m, e, at}
	if got := f.askIn(t, "other", "<@202> 어떤 사람이야?"); strings.Contains(got, "r2-job-sentinel") {
		t.Errorf("what 민수 said in #c is shown in #other as 영희's own statement: %q", got)
	}
	if err := s.MessagesDeleted("g", "c", []string{"9001"}); err != nil {
		t.Fatal(err)
	}
	_ = s.DrainRedactions(context.Background())
	if n := personClaimCount(t, s, "instr(text,'r2-job-sentinel')>0"); n != 0 {
		t.Errorf("after the line that said it was deleted, %d statement(s) remain", n)
	}
}

// With memory.people off, or the old memory.cards: off, live extraction asks
// nothing about people: no people-only call for a one-line statement made to the bot, no people section
// in the episode prompt, no claim and no live_since. The ordinary episode still saves.
func TestLiveExtractionHonorsPeopleOff(t *testing.T) {
	t.Parallel()
	for name, off := range map[string]func(*config.Document){
		"people off":       func(d *config.Document) { d.Memory.People = "off" },
		"legacy cards off": func(d *config.Document) { d.Memory.Cards = "off" },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			svc, m, eng := roomSvc(t)
			applyDoc(t, svc, off)
			m.extract = func(prompt string) string {
				if strings.Contains(prompt, `{"episodes":`) {
					return peopleAnswer(hikePeople)
				}
				return `{"people":[{"about":"p1","kind":"status","topic":"residence","text":"{p1}는 광양에 산다","by":["p1"],"ev":["m1"]}]}`
			}
			oneLine(t, svc, eng, "6001", "나 광양에 살아 요즘", time.Now().Add(-8*time.Hour))
			sayIn(eng, time.Now().Add(-2*time.Hour), hike...)
			rep := svc.RunRoomMemory(context.Background())
			if rep.PeopleCalls() != 0 || m.calls("episode") != 1 || rep.Episodes != 1 || rep.PeopleClaims() != 0 {
				t.Fatalf("pass: %+v people calls=%d model calls=%d", rep, rep.PeopleCalls(), m.calls("episode"))
			}
			prompt := m.last("episode")
			for _, bad := range []string{"{people_begin}", "relationship_status", `"people":[{`} {
				if strings.Contains(prompt, bad) {
					t.Errorf("the episode prompt carries the people section (%s):\n%s", bad, prompt)
				}
			}
			if !strings.Contains(prompt, "토요일에 등산 갈래?") {
				t.Errorf("the conversation was not asked about:\n%s", prompt)
			}
			if rows := peopleRows(t, svc); len(rows) != 0 {
				t.Errorf("claims written with person memory off: %+v", rows)
			}
			if liveSince(t, svc, "g") != "" {
				t.Error("live_since written with person memory off")
			}
			if l := episodeList(t, svc); len(l) != 1 {
				t.Errorf("the ordinary episode was not saved: %+v", l)
			}
		})
	}
}

// oneLine stores a line of 철수 (u1) in g/c that the bot answered (seen by the gateway only).
func oneLine(t *testing.T, svc *application.Service, eng *engine.Engine, id, text string, at time.Time) {
	t.Helper()
	if ok, err := eng.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", MessageID: id, AuthorID: "u1", AuthorName: "철수", Content: text, At: at}); err != nil || !ok {
		t.Fatal(id, ok, err)
	}
	svc.Observe(application.ObservedMessage{GuildID: "g", ChannelID: "c", MessageID: id + "r", AuthorID: "bot", AuthorName: "마사몽", Content: "그렇구나",
		FromBot: true, ReplyTo: id, At: at.Add(20 * time.Second)})
}

// Nothing stale goes out: before the one-line people-only request is sent, a line that is no longer
// current is dropped (a committed deletion or edit intent whose observation is still stored, withdrawn
// memory or profile consent, a blocked account, a deleted thread), and with no line left no call is
// made. A current addressed line, in the channel or in a live thread, is still asked about (controls).
func TestLiveOneLineDoesNotSendTombstonedText(t *testing.T) {
	t.Parallel()
	const marker = "r3-stale-marker"
	now := func() string { return time.Now().UTC().Format(time.RFC3339Nano) }
	for name, tc := range map[string]struct {
		channel string
		stale   func(t *testing.T, svc *application.Service)
		asked   bool
	}{
		"current line": {"c", nil, true},
		"live thread":  {"t1", nil, true},
		"message tombstone": {"c", func(t *testing.T, svc *application.Service) {
			if _, err := svc.Store.DB.Exec(`INSERT INTO privacy_tombstones(kind,source_id,at) SELECT 'message',native_message_id,? FROM memory_observations WHERE instr(content,?)>0`, now(), marker); err != nil {
				t.Fatal(err)
			}
		}, false},
		"memory withdrawn": {"c", func(t *testing.T, svc *application.Service) {
			if _, err := svc.Store.DB.Exec(`INSERT INTO consent_state(user_id,scope,status,updated_at) VALUES('u1','memory','withdrawn',?)`, now()); err != nil {
				t.Fatal(err)
			}
		}, false},
		"profile withdrawn": {"c", func(t *testing.T, svc *application.Service) {
			if _, err := svc.Store.DB.Exec(`INSERT INTO consent_state(user_id,scope,status,updated_at) VALUES('u1','profile','withdrawn',?)`, now()); err != nil {
				t.Fatal(err)
			}
		}, false},
		"account tombstone": {"c", func(t *testing.T, svc *application.Service) {
			if _, err := svc.Store.DB.Exec(`INSERT INTO privacy_tombstones(kind,source_id,at) VALUES('source',?,?)`, store.PersonRef("u1"), now()); err != nil {
				t.Fatal(err)
			}
		}, false},
		"blocked account": {"c", func(t *testing.T, svc *application.Service) {
			applyDoc(t, svc, func(d *config.Document) { d.Access.BlockedUsers = []string{"u1"} })
		}, false},
		"deleted thread": {"t1", func(t *testing.T, svc *application.Service) { svc.ThreadDeleted("t1") }, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			svc, m, eng := roomSvc(t)
			m.extract = func(string) string {
				return `{"people":[{"about":"p1","kind":"status","topic":"residence","text":"{p1}는 광양에 산다","by":["p1"],"ev":["m1"]}]}`
			}
			at := time.Now().Add(-8 * time.Hour)
			if tc.channel != "c" {
				svc.NoteThread("g", tc.channel, "c", false)
			}
			if ok, err := eng.Capture(engine.LiveMessage{GuildID: "g", ChannelID: tc.channel, MessageID: "6301", AuthorID: "u1", AuthorName: "철수",
				Content: "나 " + marker + " 광양에 살아 요즘", At: at}); err != nil || !ok {
				t.Fatal(ok, err)
			}
			svc.Observe(application.ObservedMessage{GuildID: "g", ChannelID: tc.channel, MessageID: "6301r", AuthorID: "bot", AuthorName: "마사몽",
				Content: "그렇구나", FromBot: true, ReplyTo: "6301", At: at.Add(20 * time.Second)})
			if tc.stale != nil {
				tc.stale(t, svc)
			}
			var stored int
			if err := svc.Store.Reader().QueryRow(`SELECT COUNT(*) FROM memory_observations WHERE deleted_at='' AND instr(content,?)>0`, marker).Scan(&stored); err != nil || stored != 1 {
				t.Fatalf("the observation is not kept for the check: %d %v", stored, err)
			}
			rep := svc.RunRoomMemory(context.Background())
			sent := 0
			m.mu.Lock()
			for _, p := range m.prompts["episode"] {
				if strings.Contains(p, marker) {
					sent++
				}
			}
			m.mu.Unlock()
			if tc.asked {
				if sent != 1 || rep.PeopleCalls() != 1 || rep.PeopleClaims() != 1 {
					t.Fatalf("control: the current line was not asked about once: sent=%d %+v", sent, rep)
				}
				return
			}
			if sent != 0 || rep.PeopleCalls() != 0 || rep.PeopleClaims() != 0 {
				t.Fatalf("a line that is no longer current was sent: sent=%d calls=%d claims=%d", sent, rep.PeopleCalls(), rep.PeopleClaims())
			}
		})
	}
}

// A live claim that names a third person literally (the model copied 찰리's name from the line instead of
// writing the token) is tied to 찰리's token when the name is 찰리's alone, so 찰리's forget-all reaches it:
// afterwards neither the stored row nor the recall prompt carries it. With the cutoff written first, the
// statement is never stored. (A name two people share drops the claim: TestLiveNamesTieLiteralNames.)
func TestLiveForgetCoversKnownLiteralNames(t *testing.T) {
	t.Parallel()
	const marker = "r4-literal-marker"
	charlie := store.PersonRef("505")
	conversation := []said{
		{"9301", "101", "민수", "영희는 요즘 찰리랑 " + marker + " 연습해"}, {"9302", "202", "영희", "응 맞아 매주 하고 있어"},
		{"9303", "101", "민수", "부럽다 나도 끼워줘"}, {"9304", "202", "영희", "좋아 다음에 같이 하자"},
	}
	answer := `{"episodes":[],"people":[{"about":"p2","kind":"other","topic":"연습","text":"{p2}는 찰리와 ` + marker + ` 연습을 함","by":["p1"],"ev":["m1"]}]}`
	setup := func(t *testing.T) reviewPerson {
		s, m, e := memorySvc(t)
		f := reviewPerson{s, m, e, time.Now().UTC().Add(-3 * time.Hour)}
		f.source(t, "g", "c", "r4-charlie", "505", "찰리", f.at.Add(-time.Hour))
		f.source(t, "g", "c", "r4-asker", "303", "지수", f.at.Add(-40*time.Minute))
		m.extract = func(prompt string) string {
			if !strings.Contains(prompt, marker) {
				return `{"episodes":[]}` // the lines that gave the names
			}
			return answer
		}
		return f
	}
	forgetAll := func(t *testing.T, f reviewPerson) {
		t.Helper()
		req := func(confirm any) map[string]any {
			return map[string]any{"guild_id": "g", "user_id": "505", "scope": "all", "confirm": confirm}
		}
		p := exec(f.Service, "memory.mine.forget", req(""))
		if !p.OK {
			t.Fatalf("preview: %+v", p)
		}
		if r := exec(f.Service, "memory.mine.forget", req(p.Output["token"])); !r.OK || r.Output["applied"] != true {
			t.Fatalf("forget: %+v", r)
		}
	}
	recall := func(t *testing.T, f reviewPerson) string {
		return block(f.ask(t, "<@202> 요즘 뭐 해?"), cardOpen, cardClose)
	}

	t.Run("forget after", func(t *testing.T) {
		t.Parallel()
		f := setup(t)
		sayIn(f.engine, f.at, conversation...)
		if rep := f.RunRoomMemory(context.Background()); rep.PeopleClaims() != 1 {
			t.Fatalf("pass: %+v claims=%d skipped=%d", rep, rep.PeopleClaims(), rep.PeopleSkipped())
		}
		if n := personClaimCount(t, f.Service, "instr(text,?)>0 AND instr(text,'찰리')=0", "{@"+charlie+"}"); n != 1 {
			t.Fatalf("the literal name was not tied to 찰리's token: %d row(s)", n)
		}
		if got := recall(t, f); !strings.Contains(got, marker) || !strings.Contains(got, "찰리") {
			t.Fatalf("control: the statement is read with 찰리's name: %q", got)
		}
		forgetAll(t, f)
		if n := personClaimCount(t, f.Service, "instr(text,?)>0", marker); n != 0 {
			t.Errorf("after 찰리's forget-all, %d stored statement(s) still name him", n)
		}
		if got := recall(t, f); strings.Contains(got, marker) {
			t.Errorf("after 찰리's forget-all the recall prompt carries the statement: %q", got)
		}
	})
	t.Run("cutoff first", func(t *testing.T) {
		t.Parallel()
		f := setup(t)
		sayIn(f.engine, f.at, conversation...)
		forgetAll(t, f)
		f.RunRoomMemory(context.Background())
		if n := personClaimCount(t, f.Service, "instr(text,?)>0", marker); n != 0 {
			t.Errorf("a statement naming 찰리 was stored after his forget-all: %d row(s)", n)
		}
		if got := recall(t, f); strings.Contains(got, marker) {
			t.Errorf("the recall prompt carries a statement naming 찰리 after his forget-all: %q", got)
		}
	})
}

// The people list follows the episodes under the same output cap. An answer cut inside it keeps its whole
// episodes: they are saved after one call, the people part is counted as failed, and nothing is asked
// again. The same holds for a regeneration. A cut inside the episodes is still a bad answer (asked again).
func TestTruncatedPeopleTailKeepsCompleteEpisode(t *testing.T) {
	t.Parallel()
	cutPeople := func(answer string) string {
		full := strings.Replace(answer, `}]}`, `}],"people":`+hikePeople+`}`, 1)
		return full[:strings.Index(full, `"people":`)+len(`"people":`)+60] // the cap falls inside the first entry
	}
	t.Run("extraction", func(t *testing.T) {
		t.Parallel()
		svc, m, eng := roomSvc(t)
		m.extract = func(string) string { return cutPeople(hikeAnswer) }
		sayIn(eng, time.Now().Add(-2*time.Hour), hike...)
		rep := svc.RunRoomMemory(context.Background())
		if rep.Episodes != 1 || rep.Failed != 0 || m.calls("episode") != 1 || rep.PeopleFailed() != 1 || rep.PeopleClaims() != 0 {
			t.Fatalf("an answer cut inside its people list: episodes=%d failed=%d calls=%d people failed=%d claims=%d (want 1 0 1 1 0)",
				rep.Episodes, rep.Failed, m.calls("episode"), rep.PeopleFailed(), rep.PeopleClaims())
		}
		if l := episodeList(t, svc); len(l) != 1 || l[0]["items"].(float64) != 3 {
			t.Fatalf("the whole episode is saved: %+v", l)
		}
		if rep := svc.RunRoomMemory(context.Background()); m.calls("episode") != 1 || rep.Segments != 0 {
			t.Fatalf("the segment is done: a second pass asked again (%d calls, %+v)", m.calls("episode"), rep)
		}
	})
	t.Run("regeneration", func(t *testing.T) {
		t.Parallel()
		svc, m, eng := roomSvc(t)
		m.extract = func(string) string { return hikeAnswer }
		sayIn(eng, time.Now().Add(-2*time.Hour), hike...)
		svc.RunRoomMemory(context.Background())
		old := episodeList(t, svc)[0]["id"].(string)
		if err := svc.MessagesDeleted("g", "c", []string{"3003"}); err != nil {
			t.Fatal(err)
		}
		drain(t, svc)
		before := m.calls("episode")
		m.extract = func(string) string {
			return cutPeople(`{"episodes":[{"op":"create","kind":"plan","title":"토요일 등산","items":[
			{"kind":"proposal","text":"{p1}가 토요일 등산 제안","by":["p1"],"ev":["m1"]},
			{"kind":"fact","text":"{p2}가 김밥 담당","by":["p2"],"ev":["m3"]}],"status":"open"}]}`)
		}
		rep := svc.RunRoomMemory(context.Background())
		if rep.Regenerated != 1 || m.calls("episode") != before+1 || rep.PeopleFailed() != 1 {
			t.Fatalf("a regeneration cut inside its people list: regenerated=%d calls=%d people failed=%d (want 1 1 1)",
				rep.Regenerated, m.calls("episode")-before, rep.PeopleFailed())
		}
		if l := episodeList(t, svc); len(l) != 1 || l[0]["id"] == old || l[0]["items"].(float64) != 2 {
			t.Fatalf("the regenerated episode replaces the old one: %+v", l)
		}
	})
	t.Run("cut inside the episodes", func(t *testing.T) {
		t.Parallel()
		svc, m, eng := roomSvc(t)
		m.extract = func(string) string { return hikeAnswer[:len(hikeAnswer)/2] }
		sayIn(eng, time.Now().Add(-2*time.Hour), hike...)
		if rep := svc.RunRoomMemory(context.Background()); rep.Episodes != 0 || rep.Failed != 1 || m.calls("episode") != 2 || rep.PeopleFailed() != 0 {
			t.Fatalf("an answer cut inside its episodes: episodes=%d failed=%d calls=%d people failed=%d (want 0 1 2 0)",
				rep.Episodes, rep.Failed, m.calls("episode"), rep.PeopleFailed())
		}
	})
}
