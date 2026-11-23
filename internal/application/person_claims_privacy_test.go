package application_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"masamong/internal/application"
	"masamong/internal/memory/store"
)

func personClaimCount(t *testing.T, s *application.Service, where string, args ...any) int {
	t.Helper()
	var n int
	if err := s.Store.Reader().QueryRow(`SELECT COUNT(*) FROM person_claim WHERE `+where, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The single write gate (contract §2.1): every shape, evidence, consent and server check refuses a
// claim without failing the transaction; a write is idempotent.
func TestPersonClaimWriteGate(t *testing.T) {
	t.Parallel()
	s, m, e := memorySvc(t)
	f := reviewPerson{s, m, e, time.Now().UTC().Add(-3 * time.Hour)}
	ctx := context.Background()
	a, b := store.PersonRef("101"), store.PersonRef("202")
	ev := f.source(t, "g", "c", "gate-a", "101", "민수", f.at)
	elsewhere := f.source(t, "g", "other", "gate-b", "101", "민수", f.at)
	edited := f.source(t, "g", "c", "gate-d", "101", "민수", f.at)
	said := f.source(t, "g", "c", "gate-c", "202", "영희", f.at)
	good := store.PersonClaim{GuildID: "g", SubjectRef: a, SpeakerRef: a, Kind: "like", Topic: "음식", Text: "{@" + a + "}는 국수를 좋아함",
		Evidence: []string{ev}, Source: "extract:seg-1", Link: store.LinkSelf}
	if _, err := s.Store.DB.Exec(`UPDATE memory_observations SET edited_at=? WHERE observation_id=?`, time.Now().UTC().Format(time.RFC3339Nano), edited); err != nil {
		t.Fatal(err)
	}
	refused := map[string]func(*store.PersonClaim){
		"kakao subject":          func(c *store.PersonClaim) { c.SubjectRef, c.Text = "kakao:blk:민수", "{@kakao:blk:민수} 국수" },
		"label speaker":          func(c *store.PersonClaim) { c.SpeakerRef = "label:민수" },
		"speaker is not author":  func(c *store.PersonClaim) { c.SpeakerRef, c.Link = b, store.LinkMention },
		"evidence in two rooms":  func(c *store.PersonClaim) { c.Evidence = []string{ev, elsewhere} },
		"text changed since":     func(c *store.PersonClaim) { c.Lines = map[string]string{ev: "something else"} },
		"edited after read":      func(c *store.PersonClaim) { c.Evidence, c.ReadAt = []string{edited}, f.at.Format(time.RFC3339) },
		"no subject token":       func(c *store.PersonClaim) { c.Text = "국수를 좋아함" },
		"unknown kind":           func(c *store.PersonClaim) { c.Kind = "secret" },
		"self link on hearsay":   func(c *store.PersonClaim) { c.SpeakerRef, c.Evidence = b, []string{said} },
		"another server":         func(c *store.PersonClaim) { c.GuildID = "g2" },
		"no evidence":            func(c *store.PersonClaim) { c.Evidence = nil },
		"text over 120 runes":    func(c *store.PersonClaim) { c.Text = "{@" + a + "}" + strings.Repeat("가", 120) },
		"inactive episode item":  func(c *store.PersonClaim) { c.Source = "episode_item:missing" },
		"unknown state":          func(c *store.PersonClaim) { c.State = "draft" },
		"evidence not in store":  func(c *store.PersonClaim) { c.Evidence = []string{"discord:g:c#nope"} },
		"bot named in the text":  func(c *store.PersonClaim) { c.Text = "{@" + a + "}는 {@kakao:x:y}와 친함" },
		"a dm, not a server":     func(c *store.PersonClaim) { c.GuildID = "0" },
		"empty speaker":          func(c *store.PersonClaim) { c.SpeakerRef = "" },
		"empty source":           func(c *store.PersonClaim) { c.Source = "" },
		"withdrawn subject text": func(c *store.PersonClaim) { c.Text = "{@" + a + "}는 {@" + store.PersonRef("404") + "}와 친함" },
	}
	if _, err := s.Store.DB.Exec(`INSERT INTO consent_state(user_id,scope,status,updated_at) VALUES('404','memory','withdrawn',?)`, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	for name, change := range refused {
		c := good
		change(&c)
		if n, err := s.SavePersonClaimsForTest(ctx, c); err != nil || n != 0 {
			t.Errorf("%s: written %d (%v)", name, n, err)
		}
	}
	if n, err := s.SavePersonClaimsForTest(ctx, good); err != nil || n != 1 {
		t.Fatalf("a good claim: %d (%v)", n, err)
	}
	if n, err := s.SavePersonClaimsForTest(ctx, good); err != nil || n != 0 || personClaimCount(t, s, "1") != 1 {
		t.Fatalf("written twice: %d (%v)", n, err)
	}
	// a profile withdrawal of the subject, or a tombstone of the speaker, refuses what comes after it
	hearsay := store.PersonClaim{GuildID: "g", SubjectRef: a, SpeakerRef: b, Kind: "trait", Text: "{@" + a + "}는 친절함", Evidence: []string{said},
		Source: "extract:seg-2", Link: store.LinkName}
	if _, err := s.Store.DB.Exec(`INSERT INTO privacy_tombstones(kind,source_id,at) VALUES('source',?,?)`, b, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if n, err := s.SavePersonClaimsForTest(ctx, hearsay); err != nil || n != 0 {
		t.Fatalf("a tombstoned speaker's statement: %d (%v)", n, err)
	}
	if r := as(s, "privacy.consent", map[string]any{"scope": "profile", "decision": "withdraw"}, inChannel("101")); !r.OK {
		t.Fatal(r.Error)
	}
	drain(t, s)
	c := good
	c.Evidence = []string{f.source(t, "g", "c", "gate-e", "101", "민수", f.at.Add(time.Minute))}
	if n, err := s.SavePersonClaimsForTest(ctx, c); err != nil || n != 0 || personClaimCount(t, s, "subject_ref=?", a) != 0 {
		t.Fatalf("a profile-withdrawn subject: %d (%v)", n, err)
	}
}

// Hide (memory.person.hide): the claim is not shown and not written again; for hearsay with a topic,
// that topic about the person is not shown again until the person says it themselves. A member hides
// only what is about them.
func TestPersonClaimHideAndTopic(t *testing.T) {
	t.Parallel()
	s, m, e := memorySvc(t)
	f := reviewPerson{s, m, e, time.Now().UTC().Add(-3 * time.Hour)}
	ctx := context.Background()
	a, b := store.PersonRef("101"), store.PersonRef("202")
	f.source(t, "g", "c", "hide-label", "101", "민수", f.at)
	said := f.source(t, "g", "c", "hide-said", "202", "영희", f.at)
	gossip := store.PersonClaim{GuildID: "g", SubjectRef: a, SpeakerRef: b, Kind: "like", Topic: "음식", Text: "{@" + a + "}는 국수를 좋아함",
		Evidence: []string{said}, Source: "extract:seg-1", Link: store.LinkName}
	if n, err := s.SavePersonClaimsForTest(ctx, gossip); err != nil || n != 1 {
		t.Fatalf("hearsay: %d (%v)", n, err)
	}
	ask := func() string { return block(f.ask(t, "<@101> 어떤 사람이야?"), cardOpen, cardClose) }
	if got := ask(); !strings.Contains(got, "민수는 국수를 좋아함 (영희 말)") {
		t.Fatalf("hearsay with its source label: %q", got)
	}
	id := store.PersonClaimID("g", a, b, []string{said})
	if r := as(s, "memory.person.hide", map[string]any{"claim_id": id}, inChannel("202")); r.OK {
		t.Fatal("a member hid a statement about someone else")
	}
	if r := exec(s, "memory.person.hide", map[string]any{"guild_id": "g", "user_id": "101", "claim_id": id}); !r.OK || r.Output["hidden"] != true {
		t.Fatalf("hide: %+v", r)
	}
	if got := ask(); strings.Contains(got, "국수") {
		t.Fatalf("a hidden statement was shown: %q", got)
	}
	if _, err := s.Store.DB.Exec(`DELETE FROM person_claim WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if n, err := s.SavePersonClaimsForTest(ctx, gossip); err != nil || n != 0 {
		t.Fatalf("a hidden statement was written again: %d (%v)", n, err)
	}
	// the same topic said again by someone, after the hide: kept, but not shown
	later := time.Now().UTC().Add(time.Minute)
	again := gossip
	again.Text, again.Evidence = "{@"+a+"}는 잔치국수를 좋아함", []string{f.source(t, "g", "c", "hide-said-again", "202", "영희", later)}
	if n, err := s.SavePersonClaimsForTest(ctx, again); err != nil || n != 1 {
		t.Fatalf("a later statement: %d (%v)", n, err)
	}
	if got := ask(); strings.Contains(got, "국수") {
		t.Fatalf("a hidden topic came back through someone else: %q", got)
	}
	// once the person says it themselves, the topic is theirs again
	own := store.PersonClaim{GuildID: "g", SubjectRef: a, SpeakerRef: a, Kind: "like", Topic: "음식", Text: "{@" + a + "}가 직접 밝힌 최애 메뉴는 냉면",
		Evidence: []string{f.source(t, "g", "c", "hide-own", "101", "민수", later.Add(time.Minute))}, Source: "extract:seg-2", Link: store.LinkSelf}
	if n, err := s.SavePersonClaimsForTest(ctx, own); err != nil || n != 1 {
		t.Fatalf("own statement: %d (%v)", n, err)
	}
	if got := ask(); !strings.Contains(got, "냉면") || !strings.Contains(got, "잔치국수") {
		t.Fatalf("after the person's own statement: %q", got)
	}
}

// Fable review §4, phase 1: the privacy regressions of person memory.
func TestPersonClaimPrivacyRegressions(t *testing.T) {
	t.Parallel()
	a := store.PersonRef("101")
	tests := map[string]func(*testing.T, reviewPerson){
		// A statement made in a private thread is not used outside it; inside it there is no person memory.
		"PrivateThread": func(t *testing.T, f reviewPerson) {
			f.source(t, "g", "c", "private-label", "101", "민수", f.at)
			ev := f.source(t, "g", "pthread", "private-line", "101", "민수", f.at)
			f.fact(t, "g", "pthread", "private-thread-marker", "101", "101", ev)
			if n := personClaimCount(t, f.Service, "room_id='discord:g:pthread'"); n != 1 {
				t.Fatalf("private thread statements kept as their own room: %d", n)
			}
			if got := block(f.ask(t, "<@101> 어떤 사람이야?"), cardOpen, cardClose); strings.Contains(got, "private-thread-marker") {
				t.Fatalf("a private thread statement left the thread: %q", got)
			}
			turn(t, f.Service, "303", "지수", "<@101> 어떤 사람이야?", func(in *application.ChatInput) { in.ChannelID, in.Locale = "pthread", "ko" })
			if got := block(f.model.last("answer"), cardOpen, cardClose); got != "" {
				t.Fatalf("person memory inside a private thread: %q", got)
			}
		},
		// What a speaker said goes when the speaker withdraws memory consent.
		"SpeakerWithdraws": func(t *testing.T, f reviewPerson) {
			f.source(t, "g", "c", "withdraw-label", "101", "민수", f.at)
			ev := f.source(t, "g", "c", "withdraw-said", "202", "영희", f.at)
			f.fact(t, "g", "c", "speaker-withdraws-marker", "101", "202", ev)
			if got := block(f.ask(t, "<@101> 어떤 사람이야?"), cardOpen, cardClose); !strings.Contains(got, "speaker-withdraws-marker") {
				t.Fatalf("control: %q", got)
			}
			if r := as(f.Service, "privacy.consent", map[string]any{"scope": "memory", "decision": "withdraw"}, inChannel("202")); !r.OK {
				t.Fatal(r.Error)
			}
			drain(t, f.Service)
			if n := personClaimCount(t, f.Service, "speaker_ref=?", store.PersonRef("202")); n != 0 {
				t.Fatalf("statements of a withdrawn speaker: %d", n)
			}
			if got := block(f.ask(t, "<@101> 어떤 사람이야?"), cardOpen, cardClose); strings.Contains(got, "speaker-withdraws-marker") {
				t.Fatalf("a withdrawn speaker's statement was shown: %q", got)
			}
		},
		// A tombstone takes effect at once: the read gate checks it before any invalidation pass ran.
		"Tombstone": func(t *testing.T, f reviewPerson) {
			f.source(t, "g", "c", "tomb-label", "101", "민수", f.at)
			ev := f.source(t, "g", "c", "tomb-said", "202", "영희", f.at)
			f.fact(t, "g", "c", "tombstone-marker", "101", "202", ev)
			if got := block(f.ask(t, "<@101> 어떤 사람이야?"), cardOpen, cardClose); !strings.Contains(got, "tombstone-marker") {
				t.Fatalf("control: %q", got)
			}
			if _, err := f.Store.DB.Exec(`INSERT INTO privacy_tombstones(kind,source_id,at) VALUES('source',?,?)`, store.PersonRef("202"), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
				t.Fatal(err)
			}
			if got := block(f.ask(t, "<@101> 어떤 사람이야?"), cardOpen, cardClose); strings.Contains(got, "tombstone-marker") {
				t.Fatalf("a tombstoned speaker's statement was shown: %q", got)
			}
			ev2 := f.source(t, "g", "c", "tomb-said-2", "202", "영희", f.at)
			if _, err := f.Store.DB.Exec(`INSERT INTO channel_episode(id,guild_id,channel_id,room,kind,title,status,source,first_seen,last_seen,updated_at)
				VALUES('tomb-2','g','c','discord:g:c','event','t','done','discord',?,?,?)`, f.at, f.at, f.at); err != nil {
				t.Fatal(err)
			}
			if _, err := f.Store.DB.Exec(`INSERT INTO episode_item(id,episode_id,kind,text,evidence,by_refs,created_at) VALUES('tomb-2','tomb-2','fact',?,?,?,?)`,
				"{@"+a+"} again", `["`+ev2+`"]`, `["`+store.PersonRef("202")+`"]`, f.at.Format(time.RFC3339Nano)); err != nil {
				t.Fatal(err)
			}
			if n, err := f.DerivePersonItemForTest(t.Context(), "tomb-2"); err != nil || n != 0 {
				t.Fatalf("a tombstoned speaker's item derived %d (%v)", n, err)
			}
		},
		// A KakaoTalk speaker is never a person of a server and never merged with a Discord account.
		"KakaoNeverMerged": func(t *testing.T, f reviewPerson) {
			if _, err := f.Store.DB.Exec(`INSERT INTO channel_episode(id,guild_id,channel_id,room,kind,title,status,source,first_seen,last_seen,updated_at)
				VALUES('kakao-ep','g','c','kakao:blk','event','t','done','kakao:blk',?,?,?)`, f.at, f.at, f.at); err != nil {
				t.Fatal(err)
			}
			if _, err := f.Store.DB.Exec(`INSERT INTO episode_item(id,episode_id,kind,text,evidence,by_refs,created_at) VALUES('kakao-item','kakao-ep','fact',?,'["kakao:blk#1"]',?,?)`,
				"{@kakao:blk:민수}는 {@"+a+"}와 친함", `["kakao:blk:민수"]`, f.at.Format(time.RFC3339Nano)); err != nil {
				t.Fatal(err)
			}
			if n, err := f.DerivePersonItemForTest(t.Context(), "kakao-item"); err != nil || n != 0 {
				t.Fatalf("a KakaoTalk item derived %d (%v)", n, err)
			}
			f.PeoplePassForTest(t.Context())
			if n := personClaimCount(t, f.Service, "subject_ref LIKE 'kakao:%' OR speaker_ref LIKE 'kakao:%' OR source LIKE '%kakao%' OR text LIKE '%kakao:%'"); n != 0 {
				t.Fatalf("KakaoTalk in person memory: %d", n)
			}
		},
		// Person memory never crosses servers.
		"ServerIsolation": func(t *testing.T, f reviewPerson) {
			ev := f.source(t, "g", "c", "iso-line", "101", "민수", f.at)
			f.fact(t, "g", "c", "server-isolation-marker", "101", "101", ev)
			if got := block(f.ask(t, "<@101> 어떤 사람이야?"), cardOpen, cardClose); !strings.Contains(got, "server-isolation-marker") {
				t.Fatalf("control: %q", got)
			}
			f.source(t, "g2", "c2", "iso-label", "101", "민수", f.at)
			turn(t, f.Service, "303", "지수", "<@101> 어떤 사람이야?", func(in *application.ChatInput) { in.GuildID, in.ChannelID, in.Locale = "g2", "c2", "ko" })
			if strings.Contains(f.model.last("answer"), "server-isolation-marker") {
				t.Fatal("a statement crossed servers")
			}
		},
		// What a speaker forgot is never derived again, from the items that are left or new ones over the
		// same lines.
		"ForgetThenRederive": func(t *testing.T, f reviewPerson) {
			ev := f.source(t, "g", "c", "forget-said", "202", "영희", f.at)
			f.fact(t, "g", "c", "forget-marker", "101", "202", ev)
			req := func(confirm any) map[string]any {
				return map[string]any{"guild_id": "g", "user_id": "202", "scope": "all", "confirm": confirm}
			}
			p := exec(f.Service, "memory.mine.forget", req(""))
			if !p.OK || p.Output["person_items"] != 1 {
				t.Fatalf("preview: %+v", p)
			}
			if r := exec(f.Service, "memory.mine.forget", req(p.Output["token"])); !r.OK || r.Output["applied"] != true {
				t.Fatalf("forget: %+v", r)
			}
			f.fact(t, "g", "c", "forget-again", "101", "202", ev)
			f.PeoplePassForTest(t.Context())
			if n := personClaimCount(t, f.Service, "speaker_ref=? OR subject_ref=?", store.PersonRef("202"), store.PersonRef("202")); n != 0 {
				t.Fatalf("forgotten statements derived again: %d", n)
			}
		},
		// A DM reads no person memory, not even the caller's own.
		"DirectMessage": func(t *testing.T, f reviewPerson) {
			ev := f.source(t, "g", "c", "dm-line", "303", "지수", f.at)
			f.fact(t, "g", "c", "dm-marker", "303", "303", ev)
			if got := block(f.ask(t, "날 기억해?"), cardOpen, cardClose); !strings.Contains(got, "dm-marker") {
				t.Fatalf("control: %q", got)
			}
			turn(t, f.Service, "303", "지수", "날 기억해?", func(in *application.ChatInput) { in.GuildID, in.ChannelID, in.Locale = "", "dm-303", "ko" })
			if strings.Contains(f.model.last("answer"), "dm-marker") {
				t.Fatal("person memory in a DM")
			}
		},
		// A chime-in may use everything (owner decision 9): what others said about the caller too.
		"ChimeIn": func(t *testing.T, f reviewPerson) {
			f.source(t, "g", "c", "chime-label", "303", "지수", f.at)
			ev := f.source(t, "g", "c", "chime-said", "202", "영희", f.at)
			f.fact(t, "g", "c", "chime-in-marker", "303", "202", ev)
			if got := block(f.ask(t, "오늘 날씨 어때?"), cardOpen, cardClose); strings.Contains(got, "chime-in-marker") {
				t.Fatalf("hearsay about the caller on an ordinary turn: %q", got)
			}
			turn(t, f.Service, "303", "지수", "오늘 날씨 어때?", func(in *application.ChatInput) { in.Proactive, in.Locale = true, "ko" })
			if got := block(f.model.last("answer"), cardOpen, cardClose); !strings.Contains(got, "chime-in-marker (영희 말)") {
				t.Fatalf("a chime-in without what others said about the caller: %q", got)
			}
		},
		// A name two accounts answer to resolves to neither: "remember this" about it is no told statement.
		"NicknameImpersonation": func(t *testing.T, f reviewPerson) {
			f.source(t, "g", "c", "nick-1", "101", "동명", f.at)
			f.source(t, "g", "c", "nick-2", "202", "동명", f.at.Add(time.Minute))
			f.source(t, "g", "c", "nick-ask", "303", "지수", f.at.Add(2*time.Minute))
			f.model.explicit = func(string) string {
				return `{"intent":"remember","about":"other","fact":"동명은 고양이를 키움","key":"pet","instruction":false}`
			}
			turn(t, f.Service, "303", "지수", "기억해, 동명은 고양이를 키워", live("nick-ask"), func(in *application.ChatInput) { in.Locale = "ko" })
			if n := personClaimCount(t, f.Service, "source='explicit' AND subject_ref<>speaker_ref"); n != 0 {
				t.Fatalf("an ambiguous name got a told statement: %d", n)
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
