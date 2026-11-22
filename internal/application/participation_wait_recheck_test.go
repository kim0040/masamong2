package application_test

import (
	"testing"

	"masamong/internal/application"
	"masamong/internal/config"
)

// The quiet wait is long enough for a person to withdraw or an operator to pause the channel. When it
// ends, a speaking mode checks again what it checked when the candidate was made, as collect mode does:
// nothing is judged or said for a candidate that is no longer allowed.
func TestParticipationRechecksAfterTheQuietWait(t *testing.T) {
	cases := []struct {
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
	for _, mode := range []string{"conservative", "shadow"} {
		for _, tc := range cases {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				svc, gw, js := partSvc(t, mode)
				finishWait, _ := application.ManualParticipationForTest(svc)
				say(svc, "m1", "u1", "누가 텐트 챙기기로 했지?")
				tc.change(t, svc)
				if !finishWait("g", "c") {
					t.Fatal("candidate missing")
				}
				judged, posts := int(js.judged.Load()), gw.postCount()
				if tc.name == "nothing changed" {
					if judged != 1 || (mode == "conservative") != (posts == 1) {
						t.Fatalf("an allowed candidate is judged (and said in conservative): judged %d, posted %d", judged, posts)
					}
					return
				}
				if judged != 0 || posts != 0 {
					t.Fatalf("judged %d, posted %d after the change", judged, posts)
				}
				if finishWait("g", "c") {
					t.Fatal("the candidate is still pending")
				}
			})
		}
	}
}
