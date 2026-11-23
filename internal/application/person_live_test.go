package application_test

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"masamong/internal/application"
	"masamong/internal/config"
	"masamong/internal/memory/corpus"
	"masamong/internal/memory/engine"
	"masamong/internal/memory/store"
)

// The live extraction asks for "people" next to the episodes (phase 2): the prompt shows the rules and
// no template fence, the answer's claims go through the write gate in the episode's transaction (one
// claim per speaker, with that speaker's own lines), and a bad people list costs only the claims.

// peopleAnswer is the hike answer with a people list.
func peopleAnswer(people string) string {
	return strings.Replace(hikeAnswer, `"importance":3}]}`, `"importance":3}],"people":`+people+`}`, 1)
}

const hikePeople = `[
 {"about":"p1","kind":"like","topic":"등산","text":"{p1}는 등산을 좋아함","by":["p1"],"ev":["m1"]},
 {"about":"p2","kind":"trait","topic":"요리","text":"{p2}는 김밥을 잘 쌈","by":["p1","p2"],"ev":["m3","m4"]},
 {"about":"p2","kind":"status","topic":"Residence","text":"{p2}는 광양에 산다","by":["p2"],"ev":["m2"]}]`

// peopleRows reads the stored claims of a server: subject, speaker, kind, topic, source, link, evidence.
type peopleRow struct{ subject, speaker, kind, topic, source, link, state, room string }

func peopleRows(t *testing.T, s *application.Service) []peopleRow {
	t.Helper()
	rows, err := s.Store.Reader().Query(`SELECT subject_ref,speaker_ref,kind,topic,source,link,state,room_id FROM person_claim ORDER BY kind,speaker_ref,subject_ref`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []peopleRow
	for rows.Next() {
		var r peopleRow
		if err := rows.Scan(&r.subject, &r.speaker, &r.kind, &r.topic, &r.source, &r.link, &r.state, &r.room); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func liveSince(t *testing.T, s *application.Service, guild string) string {
	t.Helper()
	var raw string
	if err := s.Store.Reader().QueryRow(`SELECT v FROM admin_state WHERE k=?`, "people:live_since:"+guild).Scan(&raw); err != nil {
		return ""
	}
	var v string
	_ = json.Unmarshal([]byte(raw), &v)
	return v
}

func TestLiveExtractionSavesPeopleInTheEpisodeTransaction(t *testing.T) {
	t.Parallel()
	svc, m, eng := roomSvc(t)
	m.extract = func(string) string { return peopleAnswer(hikePeople) }
	sayIn(eng, time.Now().Add(-2*time.Hour), hike...)
	if liveSince(t, svc, "g") != "" {
		t.Fatal("live_since is not written before a claim is saved")
	}

	rep := svc.RunRoomMemory(context.Background())
	if rep.Segments != 1 || rep.Episodes != 1 || m.calls("episode") != 1 || rep.PeopleClaims() != 4 || rep.PeopleCalls() != 0 {
		t.Fatalf("pass: %+v calls=%d", rep, m.calls("episode"))
	}
	prompt := m.last("episode")
	for _, bad := range []string{"{people_begin}", "{people_end}"} {
		if strings.Contains(prompt, bad) {
			t.Fatalf("the template fence %s reached the model:\n%s", bad, prompt)
		}
	}
	for _, want := range []string{"relationship_status", `"people":[{"about":"p2"`} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("the prompt lacks %q:\n%s", want, prompt)
		}
	}
	u1, u2 := store.PersonRef("u1"), store.PersonRef("u2")
	src := "extract:" + corpus.DiscordLiveObservationID("discord:g:c", "3001")
	want := []peopleRow{
		{u1, u1, "like", "등산", src, store.LinkSelf, "active", "discord:g:c"},
		{u2, u1, "trait", "요리", src, "", "active", "discord:g:c"}, // hearsay: link is not self (checked below)
		{u2, u2, "status", "residence", src, store.LinkSelf, "active", "discord:g:c"},
		{u2, u2, "trait", "요리", src, store.LinkSelf, "active", "discord:g:c"},
	}
	var got []peopleRow
	for _, g := range peopleRows(t, svc) {
		if strings.HasPrefix(g.source, "extract:") { // the episode's own items are derived as well (source episode_item:)
			got = append(got, g)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("claims: %+v", got)
	}
	for _, w := range want {
		found := false
		for _, g := range got {
			if g.subject == w.subject && g.speaker == w.speaker && g.kind == w.kind {
				found = true
				if g.topic != w.topic || g.state != w.state || g.room != w.room || !strings.HasPrefix(g.source, "extract:") || w.link != "" && g.link != w.link || w.link == "" && g.link == store.LinkSelf {
					t.Errorf("claim %+v, want %+v", g, w)
				}
			}
		}
		if !found {
			t.Errorf("no claim like %+v in %+v", w, got)
		}
	}
	// the claim of the speaker u2 about themselves rests on their own line only (m4), not on u1's
	var ev string
	if err := svc.Store.Reader().QueryRow(`SELECT evidence FROM person_claim WHERE subject_ref=? AND speaker_ref=? AND kind='trait'`, u2, u2).Scan(&ev); err != nil ||
		ev != `["`+corpus.DiscordLiveObservationID("discord:g:c", "3004")+`"]` {
		t.Fatalf("evidence of a split claim: %s %v", ev, err)
	}
	if liveSince(t, svc, "g") == "" {
		t.Fatal("live_since is written on the first people save")
	}
	// the episode's own items are still saved with it
	if l := episodeList(t, svc); len(l) != 1 || l[0]["items"].(float64) != 3 {
		t.Fatalf("episodes: %+v", l)
	}
}

// live_since is written once: a later save never moves it.
func TestLiveSinceIsWrittenOnce(t *testing.T) {
	t.Parallel()
	svc, m, eng := roomSvc(t)
	m.extract = func(string) string { return peopleAnswer(hikePeople) }
	sayIn(eng, time.Now().Add(-5*time.Hour), hike...)
	if rep := svc.RunRoomMemory(context.Background()); rep.PeopleClaims() == 0 {
		t.Fatalf("first pass: %+v", rep)
	}
	first := liveSince(t, svc, "g")
	if _, err := time.Parse(time.RFC3339, first); err != nil || !strings.HasSuffix(first, "Z") {
		t.Fatalf("live_since is RFC 3339 UTC: %q", first)
	}
	if _, err := svc.Store.DB.Exec(`UPDATE admin_state SET v=? WHERE k=?`, `"2020-01-01T00:00:00Z"`, "people:live_since:g"); err != nil {
		t.Fatal(err)
	}
	later := []said{
		{"4001", "u1", "철수", "일요일에 영화 볼래?"}, {"4002", "u2", "영희", "좋아 영화 보자"},
		{"4003", "u1", "철수", "저녁 7시에 영화 확정"}, {"4004", "u2", "영희", "팝콘은 내가 살게"},
	}
	m.extract = func(string) string {
		return `{"episodes":[],"people":[{"about":"p1","kind":"like","topic":"영화","text":"{p1}는 영화를 좋아함","by":["p1"],"ev":["m1"]}]}`
	}
	sayIn(eng, time.Now().Add(-90*time.Minute), later...)
	if rep := svc.RunRoomMemory(context.Background()); rep.PeopleClaims() != 1 {
		t.Fatalf("second pass: %+v", rep)
	}
	if got := liveSince(t, svc, "g"); got != "2020-01-01T00:00:00Z" {
		t.Fatalf("live_since was overwritten: %q", got)
	}
}

// A bad or missing people section never fails, re-asks or regenerates the episode extraction.
func TestLivePeopleFailureKeepsTheEpisode(t *testing.T) {
	t.Parallel()
	for name, people := range map[string]string{
		"not a list":    `"oops"`,
		"unknown token": `[{"about":"p9","kind":"like","topic":"x","text":"{p9}는 등산","by":["p1"],"ev":["m1"]}]`,
		"bot evidence":  `[{"about":"p1","kind":"like","topic":"x","text":"{p1}는 등산","by":["p1"],"ev":["bot:1"]}]`,
		"wrong author":  `[{"about":"p1","kind":"like","topic":"x","text":"{p1}는 등산","by":["p1"],"ev":["m2"]}]`,
		"bad kind":      `[{"about":"p1","kind":"secret","topic":"x","text":"{p1}는 등산","by":["p1"],"ev":["m1"]}]`,
	} {
		t.Run(name, func(t *testing.T) {
			svc, m, eng := roomSvc(t)
			m.extract = func(string) string { return peopleAnswer(people) }
			sayIn(eng, time.Now().Add(-2*time.Hour), hike...)
			rep := svc.RunRoomMemory(context.Background())
			if rep.Episodes != 1 || rep.Failed != 0 || m.calls("episode") != 1 || rep.PeopleClaims() != 0 {
				t.Fatalf("pass: %+v calls=%d", rep, m.calls("episode"))
			}
			// the only claim is the one derived from the episode's own fact item
			if rows := peopleRows(t, svc); len(rows) != 1 || !strings.HasPrefix(rows[0].source, "episode_item:") {
				t.Fatalf("claims: %+v", rows)
			}
			if liveSince(t, svc, "g") != "" {
				t.Fatal("no live claim, no live_since")
			}
		})
	}
}

// Without a people key the answer is the one before person memory: nothing extra is saved or asked.
func TestLiveAnswerWithoutPeopleChangesNothing(t *testing.T) {
	t.Parallel()
	svc, m, eng := roomSvc(t)
	m.extract = func(string) string { return hikeAnswer }
	sayIn(eng, time.Now().Add(-2*time.Hour), hike...)
	rep := svc.RunRoomMemory(context.Background())
	if rep.Episodes != 1 || rep.PeopleClaims() != 0 || m.calls("episode") != 1 {
		t.Fatalf("pass: %+v", rep)
	}
}

// A mention in a line is shown by name with its token, and an exact alias match of a person who did not
// write in the conversation is listed; an ambiguous name is not.
func TestLivePromptShowsMentionedPeople(t *testing.T) {
	t.Parallel()
	svc, m, eng := roomSvc(t)
	for i, p := range []struct{ id, name string }{{"303", "민수"}, {"404", "새봄"}, {"505", "새봄"}, {"606", "지수"}} {
		if ok, err := eng.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", AuthorID: p.id, AuthorName: p.name, MessageID: "s" + p.id,
			At: time.Now().Add(-6*time.Hour + time.Duration(i)*time.Second), Content: "hello there"}); err != nil || !ok {
			t.Fatal(ok, err)
		}
	}
	lines := []said{
		{"5001", "u1", "철수", "<@303> 이번 주말에 등산 갈래?"},
		{"5002", "u2", "영희", "좋아 등산 가자 지수도 부를까?"},
		{"5003", "u1", "철수", "새봄도 같이 가면 좋겠다"},
		{"5004", "u2", "영희", "그럼 토요일 아침 9시 확정"},
	}
	m.extract = func(string) string { return `{"episodes":[]}` }
	sayIn(eng, time.Now().Add(-2*time.Hour), lines...)
	svc.RunRoomMemory(context.Background())
	p := ""
	for _, c := range m.prompts["episode"] {
		if strings.Contains(c, "이번 주말에 등산") {
			p = c
		}
	}
	if p == "" {
		t.Fatal("the conversation was not asked about")
	}
	if !strings.Contains(p, "[m1] 철수(p1): 민수(p2)") {
		t.Errorf("a <@id> mention is shown by name with its token:\n%s", p)
	}
	if strings.Contains(p, "<@303>") {
		t.Errorf("an account id is shown:\n%s", p)
	}
	if !strings.Contains(p, ": 지수\n") {
		t.Errorf("an exact single-account alias match is listed:\n%s", p)
	}
	if strings.Contains(p, "새봄(p") || regexp.MustCompile(`\np\d+: 새봄\n`).MatchString(p) {
		t.Errorf("an ambiguous name got a token:\n%s", p[:500])
	}
}

// A one-line statement made to the bot is asked with the people-only prompt: no episode, one call on the
// summary role, counted. A line too short, not addressed to the bot, or past the budget is not asked.
func TestLiveSelfStatementOfOneLine(t *testing.T) {
	t.Parallel()
	svc, m, eng := roomSvc(t)
	m.extract = func(prompt string) string {
		if strings.Contains(prompt, `{"episodes":`) {
			t.Errorf("a one-line statement was asked with the episode prompt:\n%s", prompt)
		}
		return `{"people":[{"about":"p1","kind":"status","topic":"residence","text":"{p1}는 광양에 산다","by":["p1"],"ev":["m1"]},
			{"about":"p1","kind":"trait","topic":"x","text":"{p1}는 친절함","by":["p1"],"ev":["m9"]}]}`
	}
	base := time.Now().Add(-8 * time.Hour)
	say := func(id, text string, at time.Time, answered bool) {
		t.Helper()
		if ok, err := eng.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", MessageID: id, AuthorID: "u1", AuthorName: "철수", Content: text, At: at}); err != nil || !ok {
			t.Fatal(id, ok, err)
		}
		if answered { // the bot's reply, seen by the gateway only
			svc.Observe(application.ObservedMessage{GuildID: "g", ChannelID: "c", MessageID: id + "r", AuthorID: "bot", AuthorName: "마사몽", Content: "그렇구나",
				FromBot: true, ReplyTo: id, At: at.Add(20 * time.Second)})
		}
	}
	say("6001", "나 광양에 살아 요즘", base, true)                        // addressed, long enough
	say("6101", "응 그래", base.Add(2*time.Hour), true)              // answered, too short
	say("6201", "나는 매운 음식을 정말 좋아해", base.Add(4*time.Hour), false) // long enough, nobody answered

	rep := svc.RunRoomMemory(context.Background())
	if rep.PeopleCalls() != 1 || m.calls("episode") != 1 || rep.Episodes != 0 || rep.PeopleClaims() != 1 {
		t.Fatalf("pass: %+v calls=%d", rep, m.calls("episode"))
	}
	if p := m.last("episode"); !strings.Contains(p, "나 광양에 살아 요즘") || !strings.Contains(p, "relationship_status") {
		t.Fatalf("the people-only prompt:\n%s", p)
	}
	rows := peopleRows(t, svc)
	if len(rows) != 1 || rows[0].kind != "status" || rows[0].topic != "residence" || rows[0].link != store.LinkSelf || !strings.HasPrefix(rows[0].source, "extract:") {
		t.Fatalf("claims: %+v", rows)
	}
	if l := episodeList(t, svc); len(l) != 0 {
		t.Fatalf("a one-line statement made an episode: %+v", l)
	}
	if liveSince(t, svc, "g") == "" {
		t.Fatal("live_since is written by a self statement too")
	}
	// the same segment is not asked again
	if rep := svc.RunRoomMemory(context.Background()); rep.PeopleCalls() != 0 || m.calls("episode") != 1 {
		t.Fatalf("second pass: %+v", rep)
	}
}

// The one-line path spends from the derived budget: once it is used up, no further line is asked.
func TestLiveSelfStatementStopsAtTheBudget(t *testing.T) {
	t.Parallel()
	svc, m, eng := roomSvc(t)
	applyDoc(t, svc, func(d *config.Document) { d.Memory.DerivedBudgetUSD = "0.000012" })
	m.extract = func(string) string { return `{"people":[]}` }
	base := time.Now().Add(-8 * time.Hour)
	for i := 0; i < 3; i++ {
		at := base.Add(time.Duration(i) * 2 * time.Hour)
		id := "7" + string(rune('a'+i))
		if ok, err := eng.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", MessageID: id + "1", AuthorID: "u1", AuthorName: "철수", Content: "나는 고양이를 키우고 있어 " + id, At: at}); err != nil || !ok {
			t.Fatal(ok, err)
		}
		svc.Observe(application.ObservedMessage{GuildID: "g", ChannelID: "c", MessageID: id + "2", AuthorID: "bot", AuthorName: "마사몽", Content: "귀엽겠다",
			FromBot: true, ReplyTo: id + "1", At: at.Add(20 * time.Second)})
	}
	rep := svc.RunRoomMemory(context.Background())
	if rep.StoppedBy != "budget" || rep.PeopleCalls() == 0 || rep.PeopleCalls() >= 3 || m.calls("episode") != rep.PeopleCalls() {
		t.Fatalf("the budget did not stop the one-line path: %+v calls=%d", rep, m.calls("episode"))
	}
}

// Kakao rooms have no people section and anything the model answers for it is discarded.
func TestKakaoExtractionDiscardsPeople(t *testing.T) {
	t.Parallel()
	svc, m, _ := roomSvc(t)
	csv := writeKakao(t, svc.DataRoot, "club2.csv", 40)
	if r := exec(svc, "imports.start", map[string]any{"source": "kakao_csv", "path": csv, "room": "kakao:club2", "label": "동아리"}); !r.OK {
		t.Fatalf("import %+v", r.Error)
	}
	m.extract = func(string) string {
		return `{"episodes":[{"op":"create","kind":"event","title":"동아리 회의","items":[{"kind":"moment","text":"{p1}가 회의를 시작함","by":["p1"],"ev":["m1"]}]}],
			"people":[{"about":"p1","kind":"like","topic":"x","text":"{p1}는 회의를 좋아함","by":["p1"],"ev":["m1"]}]}`
	}
	r := exec(svc, "memory.episodes.extract", map[string]any{"source_room": "kakao:club2", "run_now": true, "limit": 50, "budget_usd": "1"})
	if !r.OK || m.calls("episode") == 0 {
		t.Fatalf("extract: %+v calls=%d", r, m.calls("episode"))
	}
	for _, p := range m.prompts["episode"] {
		if strings.Contains(p, "{people_begin}") || strings.Contains(p, "relationship_status") || strings.Contains(p, `"people":[{`) {
			t.Fatalf("a Kakao prompt carries the people section:\n%s", p)
		}
	}
	if rows := peopleRows(t, svc); len(rows) != 0 {
		t.Fatalf("Kakao claims: %+v", rows)
	}
}

// A status claim of a single-valued topic replaces the older one when read, end to end: extracted from
// two conversations, asked about in a chat turn.
func TestLiveStatusTopicSupersedesWhenRead(t *testing.T) {
	t.Parallel()
	s, m, e := memorySvc(t)
	m.extract = func(prompt string) string {
		city := "서울"
		if strings.Contains(prompt, "광양") {
			city = "광양"
		}
		return `{"episodes":[],"people":[{"about":"p1","kind":"status","topic":"residence","text":"{p1}는 ` + city + `에 산다","by":["p1"],"ev":["m1"]}]}`
	}
	old := time.Now().Add(-10 * time.Hour)
	sayIn(e, old,
		said{"8001", "101", "민수", "나 서울에 살아 지금"}, said{"8002", "202", "영희", "그렇구나 멀다"},
		said{"8003", "101", "민수", "응 서울 생활이 길어"}, said{"8004", "202", "영희", "부럽다 서울"})
	sayIn(e, old.Add(5*time.Hour),
		said{"8101", "101", "민수", "나 광양으로 이사했어"}, said{"8102", "202", "영희", "정말? 광양이면 멀다"},
		said{"8103", "101", "민수", "응 광양 생활 시작했어"}, said{"8104", "202", "영희", "축하해 광양 생활"})
	if rep := s.RunRoomMemory(context.Background()); rep.PeopleClaims() != 2 {
		t.Fatalf("extraction: %+v", rep)
	}
	f := reviewPerson{s, m, e, old}
	got := block(f.ask(t, "<@101> 어디 살아?"), cardOpen, cardClose)
	if !strings.Contains(got, "광양에 산다") || strings.Contains(got, "서울에 산다") {
		t.Fatalf("the newer residence replaces the older one:\n%s", got)
	}
}
