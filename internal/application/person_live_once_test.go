package application_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"masamong/internal/application"
	"masamong/internal/memory/engine"
)

// The one-line people request goes out once. Its author withdraws memory consent while the request is
// on its way, and the model answers with no text, cut at the output limit: the line is not sent a second
// time (without thinking, or with more room), and the empty answer is paid for, not asked again on the
// next pass. The control is a line whose author stays: one send too, and a later pass buys nothing.
// Synthetic names, ids and text only.
func TestLiveOneLineIsSentOnce(t *testing.T) {
	t.Parallel()
	const marker = "once-send-marker"
	for name, withdraw := range map[string]bool{"withdrawn during the send": true, "author stays": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			svc, m, eng := roomSvc(t)
			liveStarted(t, svc)
			m.mu.Lock()
			m.extract = func(prompt string) string {
				if strings.Contains(prompt, marker) && withdraw {
					if _, err := svc.Store.DB.Exec(`INSERT OR IGNORE INTO consent_state(user_id,scope,status,updated_at) VALUES('u1','memory','withdrawn',?)`,
						time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
						t.Error(err)
					}
				}
				return ""
			}
			m.finish = func(prompt string) string {
				if strings.Contains(prompt, marker) {
					return "length"
				}
				return "stop"
			}
			m.mu.Unlock()
			at := time.Now().Add(-8 * time.Hour)
			if ok, err := eng.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", MessageID: "6401", AuthorID: "u1", AuthorName: "철수",
				Content: "나 " + marker + " 요즘 광양에 살아", At: at}); err != nil || !ok {
				t.Fatal(ok, err)
			}
			svc.Observe(application.ObservedMessage{GuildID: "g", ChannelID: "c", MessageID: "6401r", AuthorID: "bot", AuthorName: "마사몽",
				Content: "그렇구나", FromBot: true, ReplyTo: "6401", At: at.Add(20 * time.Second)})
			sent := func() int {
				m.mu.Lock()
				defer m.mu.Unlock()
				n := 0
				for _, p := range m.prompts["episode"] {
					if strings.Contains(p, marker) {
						n++
					}
				}
				return n
			}
			rep := svc.RunRoomMemory(context.Background())
			if n := sent(); n != 1 {
				t.Fatalf("the one-line request went out %d times, want once", n)
			}
			if rep.PeopleCalls() != 1 || rep.PeopleClaims() != 0 {
				t.Fatalf("the empty answer is one paid call with no claims: %+v", rep)
			}
			svc.RunRoomMemory(context.Background())
			if n := sent(); n != 1 {
				t.Fatalf("the next pass asked about the line again: %d sends", n)
			}
		})
	}
}
