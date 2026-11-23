package application_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"masamong/internal/application"
	"masamong/internal/config"
)

// roomRetentionSvc is a service with rooms whose logging.retain_days differ, and the global
// retention.turn_detail_days given (-1 = keep forever).
func roomRetentionSvc(t *testing.T, global int, rooms map[string]int) *application.Service {
	t.Helper()
	svc, _ := slowChatSvc(t, 0, func(d *config.Document) {
		d.Retention.TurnDetailDays = global
		chans := map[string]config.ChannelPolicy{}
		for id, days := range rooms {
			chans[id] = config.ChannelPolicy{Logging: config.ChannelLogging{Level: "ops", RetainDays: days}}
		}
		d.Guilds = map[string]config.GuildSettings{"g": {Channels: chans}}
	})
	return svc
}

func seedRoomTurn(t *testing.T, svc *application.Service, room string, ageDays int) string {
	t.Helper()
	id := fmt.Sprintf("%s-%d", room, ageDays)
	at := time.Now().UTC().AddDate(0, 0, -ageDays)
	if _, err := svc.Store.DB.Exec(`INSERT INTO turn_log(id, at, day, hour, guild_id, channel_id, outcome, tools, hits, cost_usd, latency_ms, lab, body)
		VALUES(?, ?, ?, 0, 'g', ?, 'answered', '', 0, 0, 0, 0, '{"message":"synthetic text"}')`,
		id, at.Format(time.RFC3339), at.Format("2006-01-02"), room); err != nil {
		t.Fatal(err)
	}
	return id
}

func turnState(t *testing.T, svc *application.Service, id string) (exists, textKept bool) {
	t.Helper()
	var body string
	err := svc.Store.DB.QueryRow(`SELECT body FROM turn_log WHERE id=?`, id).Scan(&body)
	if err != nil {
		return false, false
	}
	return true, body != "{}"
}

// Every config written before the setting was wired carries retain_days: 30. With the global setting
// at -1 (an install that keeps everything), that stored 30 and an unset room must strip nothing.
func TestRoomRetainDaysOldDefaultFollowsGlobalForever(t *testing.T) {
	t.Parallel()
	svc := roomRetentionSvc(t, -1, map[string]int{"legacy": 30, "unset": 0})
	ids := []string{seedRoomTurn(t, svc, "legacy", 200), seedRoomTurn(t, svc, "unset", 200), seedRoomTurn(t, svc, "unlisted", 200)}
	if _, err := svc.RunRetentionForTest(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if ok, kept := turnState(t, svc, id); !ok || !kept {
			t.Errorf("%s: row %v, text kept %v; want kept (global keeps forever)", id, ok, kept)
		}
	}
}

// A room value shortens the text of that room only; its rows and every other room stay.
func TestRoomRetainDaysShortensOnlyThatRoomsText(t *testing.T) {
	t.Parallel()
	svc := roomRetentionSvc(t, -1, map[string]int{"short": 7, "other": 0})
	oldShort, newShort := seedRoomTurn(t, svc, "short", 10), seedRoomTurn(t, svc, "short", 3)
	oldOther, unlisted := seedRoomTurn(t, svc, "other", 10), seedRoomTurn(t, svc, "unlisted", 10)
	removed, err := svc.RunRetentionForTest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if removed["room_turn_detail"] != 1 {
		t.Errorf("room_turn_detail = %d, want 1", removed["room_turn_detail"])
	}
	for id, wantKept := range map[string]bool{oldShort: false, newShort: true, oldOther: true, unlisted: true} {
		ok, kept := turnState(t, svc, id)
		if !ok {
			t.Errorf("%s: the row was deleted; a room setting only blanks text", id)
		} else if kept != wantKept {
			t.Errorf("%s: text kept = %v, want %v", id, kept, wantKept)
		}
	}
}

// A room never keeps text longer than the global setting: 200 days in the room, 90 globally.
func TestRoomRetainDaysNeverKeepsLongerThanGlobal(t *testing.T) {
	t.Parallel()
	svc := roomRetentionSvc(t, 90, map[string]int{"long": 200, "short": 20})
	long100, short30, short5 := seedRoomTurn(t, svc, "long", 100), seedRoomTurn(t, svc, "short", 30), seedRoomTurn(t, svc, "short", 5)
	long50 := seedRoomTurn(t, svc, "long", 50)
	if _, err := svc.RunRetentionForTest(context.Background()); err != nil {
		t.Fatal(err)
	}
	for id, wantKept := range map[string]bool{long100: false, long50: true, short30: false, short5: true} {
		if _, kept := turnState(t, svc, id); kept != wantKept {
			t.Errorf("%s: text kept = %v, want %v", id, kept, wantKept)
		}
	}
}

// A person's own deletion still wins: their text is gone before and after a room's pass, an old row
// stays blank, and the room pass never brings anything back.
func TestRoomRetainDaysDoesNotUndoAPersonsDeletion(t *testing.T) {
	t.Parallel()
	svc := roomRetentionSvc(t, -1, map[string]int{"c": 7})
	if _, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", Message: "synthetic question", Speaker: "u", SpeakerID: "u1"}); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := svc.Store.DB.QueryRow(`SELECT id FROM turn_log WHERE channel_id='c'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if r := asUser(svc, "u1", "privacy.consent", map[string]any{"scope": "memory", "decision": "withdraw"}); !r.OK {
		t.Fatalf("withdraw: %+v", r)
	}
	message := func() (string, bool) {
		var msg string
		var deleted bool
		var body string
		if err := svc.Store.DB.QueryRow(`SELECT body FROM turn_log WHERE id=?`, id).Scan(&body); err != nil {
			t.Fatal(err)
		}
		_ = svc.Store.DB.QueryRow(`SELECT COALESCE(json_extract(body,'$.message'),''), COALESCE(json_extract(body,'$.message_deleted'),0) FROM turn_log WHERE id=?`, id).Scan(&msg, &deleted)
		return msg, deleted
	}
	if msg, del := message(); msg != "" || !del {
		t.Fatalf("the person's deletion did not blank their text: %q deleted=%v", msg, del)
	}
	// recent row: the room pass leaves it alone, the person's deletion still holds
	if _, err := svc.RunRetentionForTest(context.Background()); err != nil {
		t.Fatal(err)
	}
	if msg, del := message(); msg != "" || !del {
		t.Fatalf("the room pass changed a deleted message: %q deleted=%v", msg, del)
	}
	// old row: blanked by the room pass, still one row, still no text
	old := time.Now().UTC().AddDate(0, 0, -10).Format(time.RFC3339)
	if _, err := svc.Store.DB.Exec(`UPDATE turn_log SET at=? WHERE id=?`, old, id); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RunRetentionForTest(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ok, kept := turnState(t, svc, id); !ok || kept {
		t.Fatalf("old row: exists %v, text kept %v; want an empty body", ok, kept)
	}
}
