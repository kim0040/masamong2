package application

import (
	"context"
	"encoding/json"
	"testing"

	"masamong/internal/memory/store"
)

// A thread starts with copies of its caller's turns. A user turn whose words came partly from another
// message (a picture's description) stays tied to that message in the copy too: deleting the picture
// message takes the original and the copy.
func TestAThreadCopyOfADescribedTurnFollowsThePicture(t *testing.T) {
	for _, change := range []string{"delete", "edit"} {
		t.Run(change, func(t *testing.T) {
			s := privacyService(t)
			e, err := s.MemoryEngine()
			if err != nil {
				t.Fatal(err)
			}
			if err := e.Store.AppendChatTurns("discord:g:c", []store.ChatTurn{
				{Role: "user", Speaker: "bob", Content: "what is this? [a picture of a canary]", NativeID: "q", SpeakerID: "bob", DerivedFrom: []string{"picture"}},
				{Role: "user", Speaker: "bob", Content: "unrelated", NativeID: "q2", SpeakerID: "bob"},
			}, 40); err != nil {
				t.Fatal(err)
			}
			if seed := s.seedThread("g", "c", "thread", "bob"); len(seed) != 2 {
				t.Fatalf("seeded %d turns, want 2", len(seed))
			}
			described := func(channel string) int {
				t.Helper()
				var n int
				if err := s.Store.DB.QueryRow(`SELECT COUNT(*) FROM chat_turns WHERE channel_key=? AND native_id='q'`, "discord:g:"+channel).Scan(&n); err != nil {
					t.Fatal(err)
				}
				return n
			}
			if described("c") != 1 || described("thread") != 1 {
				t.Fatalf("before the change: %d in the channel, %d in the thread", described("c"), described("thread"))
			}
			var changeErr error
			if change == "delete" {
				changeErr = s.MessagesDeleted("g", "c", []string{"picture"})
			} else {
				changeErr = s.MessageEdited("g", "c", "picture", "another caption", s.Clock.Now())
			}
			if changeErr != nil {
				t.Fatal(changeErr)
			}
			if err := s.DrainRedactions(context.Background()); err != nil {
				t.Fatal(err)
			}
			if described("c") != 0 {
				t.Fatal("the turn described from the picture stayed in the channel")
			}
			if described("thread") != 0 {
				t.Fatal("the thread's copy of the turn described from the picture stayed")
			}
			var unrelated int
			if err := s.Store.DB.QueryRow(`SELECT COUNT(*) FROM chat_turns WHERE native_id='q2'`).Scan(&unrelated); err != nil || unrelated != 2 {
				t.Fatalf("an unrelated turn and its copy: %d rows, %v", unrelated, err)
			}
		})
	}
}

// A copy made after the picture changed is refused like the original would be.
func TestAThreadSeedSkipsATurnWhosePictureIsGone(t *testing.T) {
	s := privacyService(t)
	e, err := s.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Store.AppendChatTurns("discord:g:c", []store.ChatTurn{
		{Role: "user", Speaker: "bob", Content: "what is this? [a picture of a canary]", NativeID: "q", SpeakerID: "bob", DerivedFrom: []string{"picture"}},
	}, 40); err != nil {
		t.Fatal(err)
	}
	s.redaction.running.Store(true) // the worker has not removed the original yet
	if err := s.MessagesDeleted("g", "c", []string{"picture"}); err != nil {
		t.Fatal(err)
	}
	s.seedThread("g", "c", "thread", "bob")
	var n int
	if err := s.Store.DB.QueryRow(`SELECT COUNT(*) FROM chat_turns WHERE channel_key='discord:g:thread'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	s.redaction.running.Store(false)
	if n != 0 {
		t.Fatalf("%d turns copied from a picture already deleted", n)
	}
}

// An edit whose job is finished while another change of the message waits behind it (committed after the
// worker took its batch) settles nothing: the words still to come are not the ones a turn would read now.
func TestSettlingLeavesAMessageWithAJobWaitingBehindIt(t *testing.T) {
	s := privacyService(t)
	db, err := s.adminDB()
	if err != nil {
		t.Fatal(err)
	}
	s.redaction.running.Store(true)
	defer s.redaction.running.Store(false)
	for _, message := range []string{"waiting", "alone", "waiting"} {
		if err := s.queueSourceChange("g", "c", []string{message}, "edit", "words", s.Clock.Now()); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := db.Reader().Query(`SELECT id,payload FROM redaction_jobs ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var jobs []redactionJob
	for rows.Next() {
		var j redactionJob
		var raw string
		if err := rows.Scan(&j.id, &raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(raw), &j); err != nil {
			t.Fatal(err)
		}
		jobs = append(jobs, j)
	}
	rows.Close()
	if len(jobs) != 3 {
		t.Fatalf("jobs: %d", len(jobs))
	}
	if err := s.settleEdits(context.Background(), db, jobs[:2], jobs[:2]); err != nil {
		t.Fatal(err)
	}
	settled := map[string]bool{}
	for _, id := range s.SettledSourcesForTest() {
		settled[id] = true
	}
	if !settled["alone"] {
		t.Fatalf("the message with nothing waiting is not settled: %v", settled)
	}
	if settled["waiting"] {
		t.Fatalf("the message with a job waiting is settled: %v", settled)
	}
}
