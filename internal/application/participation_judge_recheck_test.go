package application_test

import (
	"testing"
	"time"

	"masamong/internal/application"
	"masamong/internal/config"
)

// A judge (and the reply turn after it) takes seconds, long enough for the person to withdraw, an admin
// to block them or an operator to pause the channel. The final check before the bot speaks reads those
// again: neither the reply nor a plan's offer is posted, and no offer is saved. Nothing changed is the
// positive control.
func TestParticipationRechecksAfterTheJudge(t *testing.T) {
	changes := []struct {
		name   string
		change func(t *testing.T, svc *application.Service)
	}{
		{"nothing changed", func(*testing.T, *application.Service) {}},
		{"participation consent withdrawn", func(t *testing.T, svc *application.Service) {
			if r := as(svc, "privacy.consent", map[string]any{"scope": "participation", "decision": "withdraw"}, inChannel("u1")); !r.OK {
				t.Fatal(r.Error)
			}
		}},
		{"blocked", func(t *testing.T, svc *application.Service) {
			applyDoc(t, svc, func(d *config.Document) {
				g := d.Guilds["g"]
				g.BlockedUsers = []string{"u1"}
				d.Guilds["g"] = g
			})
		}},
		{"paused", func(t *testing.T, svc *application.Service) {
			if r := exec(svc, "runtime.pause", map[string]any{"scope": "channel", "guild_id": "g", "channel_id": "c", "paused": true}); !r.OK {
				t.Fatal(r.Error)
			}
		}},
	}
	judges := map[string]string{
		"reply": `{"speak": true, "kind": "answer", "score": 0.9, "reason": "unanswered question"}`,
		"offer": `{"speak": true, "kind": "event", "score": 0.9, "reason": "plan", "when": "내일 저녁 7시", "what": "강남 모임"}`,
	}
	for kind, judge := range judges {
		for _, tc := range changes {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				svc, gw, js := partSvc(t, "conservative")
				js.set(judge)
				js.mu.Lock()
				js.hold, js.entered, js.release = true, make(chan struct{}, 1), make(chan struct{})
				js.mu.Unlock()
				t.Cleanup(js.releaseHold)
				finishWait, _ := application.ManualParticipationForTest(svc)
				say(svc, "m1", "u1", "내일 저녁 7시에 강남에서 보자")
				done := make(chan bool, 1)
				go func() { done <- finishWait("g", "c") }()
				select {
				case <-js.entered:
				case <-time.After(20 * time.Second):
					t.Fatal("the judge was never asked")
				}
				tc.change(t, svc)
				js.releaseHold()
				select {
				case ok := <-done:
					if !ok {
						t.Fatal("candidate missing")
					}
				case <-time.After(20 * time.Second):
					t.Fatal("the candidate never finished")
				}
				var offers int
				if err := svc.Store.DB.QueryRow(`SELECT COUNT(*) FROM reminder_offers`).Scan(&offers); err != nil {
					t.Fatal(err)
				}
				posts := gw.postCount()
				if tc.name == "nothing changed" {
					if posts != 1 || (kind == "offer") != (offers == 1) {
						t.Fatalf("an allowed candidate is said: posted %d, offers %d", posts, offers)
					}
					return
				}
				if posts != 0 || offers != 0 {
					t.Fatalf("posted %d, saved %d offers after the change during the judge", posts, offers)
				}
			})
		}
	}
}
