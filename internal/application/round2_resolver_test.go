package application

import (
	"database/sql"
	"slices"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/memory/engine"
)

// A plain name resolves only when no other account was captured under it in
// the eligible rooms, whether or not the alias index lists that account.
func TestRound2AliasCompleteness(t *testing.T) {
	t.Parallel()
	const label = "동명이인"
	for _, tc := range []struct {
		mode, guild, other, ask string
		want                    []string
	}{
		{"profile-withdrawn", "g", label, "동명이인은 어떤 사람이야?", nil},
		{"optional-write-rollback", "g", label, "동명이인은 어떤 사람이야?", nil},
		{"blocked", "g", label, "동명이인은 어떤 사람이야?", nil},
		{"profile-withdrawn", "g", "　" + label + " ", "동명이인은 어떤 사람이야?", nil},
		{"profile-withdrawn", "g", label + " 아빠", "동명이인 아빠는 어떤 사람이야?", nil},
		{"profile-withdrawn", "g", label, "<@101> 동명이인은 어떤 사람이야?", []string{"101"}},
		{"profile-withdrawn", "g2", label, "동명이인은 어떤 사람이야?", []string{"101"}},
		{"optional-write-rollback", "g", "Other", "동명이인은 어떤 사람이야?", []string{"101"}},
	} {
		t.Run(tc.mode+"/"+tc.guild+"/"+tc.other+"/"+tc.ask, func(t *testing.T) {
			s := resolverService(t)
			e, err := s.MemoryEngine()
			if err != nil {
				t.Fatal(err)
			}
			capture := func(guild, account, name, message string) {
				t.Helper()
				ok, err := e.Capture(engine.LiveMessage{GuildID: guild, ChannelID: "c", AuthorID: account,
					AuthorName: name, MessageID: message, Content: "fixture", At: time.Now()})
				if !ok || err != nil {
					t.Fatalf("capture %s: %t %v", account, ok, err)
				}
			}
			resolve := func() []string {
				var refs []string
				for _, p := range s.targetPeople(s.Store, ChatInput{GuildID: "g", ChannelID: "c", SpeakerID: "303", Message: tc.ask}, nil) {
					refs = append(refs, p.ref)
				}
				return refs
			}
			capture("g", "101", label, "old-account")
			if got := resolve(); !slices.Equal(got, []string{personRef("101")}) {
				t.Fatalf("unique control: %v", got)
			}
			switch tc.mode {
			case "profile-withdrawn":
				if r := s.Execute(t.Context(), action.Request{Action: "privacy.consent", Input: []byte(`{"scope":"profile","user_id":"202","decision":"withdraw"}`)}); !r.OK {
					t.Fatal(r.Error)
				}
			case "optional-write-rollback":
				err := s.Store.WithWriter(func(tx *sql.Tx) error {
					_, err := tx.Exec(`CREATE TRIGGER capture_name_fault BEFORE INSERT ON person_alias
						BEGIN SELECT RAISE(ROLLBACK,'synthetic alias failure'); END`)
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
			case "blocked":
				d := s.Doc()
				d.Access.BlockedUsers = []string{"202"}
				s.setDoc(d, s.Revision())
			}
			capture(tc.guild, "202", tc.other, "new-account")
			var indexed int
			if err := s.Store.Reader().QueryRow(`SELECT count(DISTINCT person_ref) FROM person_alias WHERE guild_id='g'`).Scan(&indexed); err != nil {
				t.Fatal(err)
			}
			if tc.mode != "blocked" && indexed != 1 {
				t.Fatalf("fixture: %d indexed accounts", indexed)
			}
			var want []string
			for _, id := range tc.want {
				want = append(want, personRef(id))
			}
			if got := resolve(); !slices.Equal(got, want) {
				t.Fatalf("refs %v, want %v", got, want)
			}
		})
	}
}
