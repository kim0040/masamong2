package application_test

import (
	"context"
	"testing"
	"time"

	"masamong/internal/memory/engine"
)

// A store from before the upgrade holds a line said to the bot (<@999>) that no pass sealed yet, with no
// room cursor or with a legacy one reading from before it. Restarted with person memory on and no new
// event, the first pass never buys a one-line people request for it: live people extraction starts at that
// pass (people:live_boundary), and only lines captured after it are asked about one at a time. The control:
// a line said to the bot after that pass is asked about once.
func TestUpgradeDoesNotBuyStoredOneLine(t *testing.T) {
	t.Parallel()
	for _, cursor := range []string{"no cursor", "legacy cursor"} {
		t.Run(cursor, func(t *testing.T) {
			t.Parallel()
			svc, m, eng := roomSvc(t)
			m.extract = func(string) string {
				return `{"people":[{"about":"p1","kind":"status","topic":"residence","text":"{p1}는 광양에 산다","by":["p1"],"ev":["m1"]}]}`
			}
			base := time.Now().Add(-8 * time.Hour)
			say := func(eng *engine.Engine, id, text string, at time.Time) {
				t.Helper()
				if ok, err := eng.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", MessageID: id, AuthorID: "u1", AuthorName: "철수", Content: text, At: at}); err != nil || !ok {
					t.Fatal(id, ok, err)
				}
			}
			// before the upgrade: the line is stored, no pass of this version ran
			say(eng, "6601", "<@999> 나 광양에 살아 요즘", base)
			if cursor == "legacy cursor" {
				if _, err := svc.Store.DB.Exec(`INSERT INTO admin_state(k,v,updated_at) VALUES('episodes:cursor:discord:g:c',?,?)`,
					`{"block_start":"`+base.Add(-time.Hour).UTC().Format("2006-01-02T15:04:05+00:00")+`"}`, time.Now().UTC().Format(time.RFC3339)); err != nil {
					t.Fatal(err)
				}
			}
			svc = restartServiceForTest(t, svc)
			svc.SetDiscord(&botGateway{})
			if rep := svc.RunRoomMemory(context.Background()); rep.PeopleCalls() != 0 || m.calls("episode") != 0 || rep.PeopleClaims() != 0 {
				t.Fatalf("the first pass after the upgrade bought a stored line: %+v calls=%d", rep, m.calls("episode"))
			}
			var err error
			if eng, err = svc.MemoryEngine(); err != nil {
				t.Fatal(err)
			}
			say(eng, "6602", "<@999> 나 요즘 고양이 키우고 있어", base.Add(3*time.Hour))
			if rep := svc.RunRoomMemory(context.Background()); rep.PeopleCalls() != 1 || m.calls("episode") != 1 {
				t.Fatalf("control: a line said to the bot after the upgrade was not asked about once: %+v calls=%d", rep, m.calls("episode"))
			}
		})
	}
}
