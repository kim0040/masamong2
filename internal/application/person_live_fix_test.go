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
