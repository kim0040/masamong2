package config

import "testing"

func TestRoomDetailDays(t *testing.T) {
	for _, c := range []struct{ room, global, want int }{
		{0, 30, 30}, {0, -1, -1}, // follows global
		{30, -1, -1}, {30, 90, 90}, {30, 14, 14}, // the old default follows global too
		{7, -1, 7}, {7, 30, 7}, {1, 30, 1}, // shorter than global (or global forever): the room wins
		{60, 30, 30}, {60, 60, 60}, {31, 30, 30}, // never longer than global
		{60, -1, 60}, {60, 90, 60},
		{-5, 30, 30}, // a negative room value is no setting
	} {
		if got := RoomDetailDays(c.room, c.global); got != c.want {
			t.Errorf("RoomDetailDays(%d, %d) = %d, want %d", c.room, c.global, got, c.want)
		}
	}
}

func TestNewRoomPolicyFollowsTheGlobalRetention(t *testing.T) {
	if got := NormalizeChannelPolicy(DefaultChannelPolicy()).Logging.RetainDays; got != 0 {
		t.Fatalf("a new room's retain_days = %d, want 0 (follow the global setting)", got)
	}
	d, err := Parse([]byte("schema_version: 1\ninstance: {id: x}\nguilds:\n  g:\n    channels:\n      c:\n        logging: {level: ops, retain_days: 30}\n      d: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Guilds["g"].Channels["c"].Logging.RetainDays; got != 30 {
		t.Errorf("a stored 30 is kept as written, got %d", got)
	}
	if got := d.Guilds["g"].Channels["d"].Logging.RetainDays; got != 0 {
		t.Errorf("a room without the key reads 0, got %d", got)
	}
}
