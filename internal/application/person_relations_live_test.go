package application_test

import (
	"testing"
	"time"

	"masamong/internal/application"
	"masamong/internal/memory/engine"
	"masamong/internal/memory/store"
)

// Astra 10, Sol medium (relations): the live relation counter leaves out a person who withdrew profile
// or forgot everything (checked when it writes), and the one-time fill counts a live line that is not
// stored yet once.
func TestPersonRelationsLiveChecks(t *testing.T) {
	t.Parallel()
	pairs := func(t *testing.T, s *application.Service, account string) (n int) {
		t.Helper()
		ref := store.PersonRef(account)
		if err := s.Store.DB.QueryRow(`SELECT COALESCE(SUM(mentions+replies),0) FROM person_relation WHERE guild_id='g' AND (a_ref=? OR b_ref=?)`, ref, ref).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	mention := func(s *application.Service, id, from, to string, at time.Time) {
		s.Observe(application.ObservedMessage{GuildID: "g", ChannelID: "c", MessageID: id, AuthorID: from, AuthorName: "n" + from,
			Content: "<@" + to + "> 안녕", At: at, Mentions: []string{to}, ToSomeone: true})
	}
	tests := map[string]func(*testing.T, *application.Service, *engine.Engine){
		"ProfileWithdrawn": func(t *testing.T, s *application.Service, _ *engine.Engine) {
			s.RunRoomMemory(t.Context()) // the one-time fill
			if r := as(s, "privacy.consent", map[string]any{"scope": "profile", "decision": "withdraw"}, inChannel("101")); !r.OK {
				t.Fatal(r.Error)
			}
			mention(s, "rel-1", "202", "101", time.Now())
			s.RunRoomMemory(t.Context())
			if n := pairs(t, s, "101"); n != 0 {
				t.Errorf("a live mention of a profile-withdrawn person was counted: %d", n)
			}
		},
		"ForgottenBeforeCount": func(t *testing.T, s *application.Service, _ *engine.Engine) {
			s.RunRoomMemory(t.Context())
			mention(s, "rel-2", "202", "404", time.Now().Add(-time.Minute))
			req := func(confirm any) map[string]any {
				return map[string]any{"guild_id": "g", "user_id": "404", "scope": "all", "confirm": confirm}
			}
			p := exec(s, "memory.mine.forget", req(""))
			if r := exec(s, "memory.mine.forget", req(p.Output["token"])); !r.OK || r.Output["applied"] != true {
				t.Fatalf("forget: %+v", r)
			}
			s.RunRoomMemory(t.Context())
			if n := pairs(t, s, "404"); n != 0 {
				t.Errorf("a live line from before the forget was counted after it: %d", n)
			}
		},
		"UnstoredLiveLine": func(t *testing.T, s *application.Service, _ *engine.Engine) {
			mention(s, "rel-3", "202", "101", time.Now())
			s.RunRoomMemory(t.Context()) // the fill moves the live cursor past the line
			s.RunRoomMemory(t.Context())
			if n := pairs(t, s, "101"); n != 1 {
				t.Errorf("a live line not stored when the fill ran is counted once: %d", n)
			}
		},
		"StoredLiveLine": func(t *testing.T, s *application.Service, e *engine.Engine) {
			at := time.Now()
			if _, err := e.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", MessageID: "rel-4", AuthorID: "202", AuthorName: "n202", Content: "<@101> 안녕", At: at}); err != nil {
				t.Fatal(err)
			}
			mention(s, "rel-4", "202", "101", at)
			s.RunRoomMemory(t.Context())
			s.RunRoomMemory(t.Context())
			if n := pairs(t, s, "101"); n != 1 {
				t.Errorf("a stored live line is counted once: %d", n)
			}
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, _, e := memorySvc(t)
			// a stored line makes #c a room of the pass
			if _, err := e.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", MessageID: "rel-0", AuthorID: "303", AuthorName: "n303", Content: "안녕", At: time.Now().Add(-time.Hour)}); err != nil {
				t.Fatal(err)
			}
			test(t, s, e)
		})
	}
}
