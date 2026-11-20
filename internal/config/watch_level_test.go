package config

import (
	"strings"
	"testing"
)

func TestWatchLevelConfig(t *testing.T) {
	d := Default("test", "/data")
	d.DisasterAlerts.WarningRegions = []string{"서울"}
	d.Guilds = map[string]GuildSettings{"g": {Channels: map[string]ChannelPolicy{
		"room": {WatchLevel: WatchLevels{"L1052100": 3}},
	}}}
	raw, err := Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(raw)
	if err != nil || got.Guilds["g"].Channels["room"].WatchLevel["L1052100"] != 3 {
		t.Fatalf("load area levels alongside legacy warning_regions: %v", err)
	}
	for _, val := range []string{"0", "6", "1.5", "null", "\"3\""} {
		bad := strings.Replace(string(raw), "L1052100: 3", "L1052100: "+val, 1)
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("accepted invalid YAML watch_level %s", val)
		}
	}
}
