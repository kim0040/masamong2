package application_test

import (
	"reflect"
	"testing"

	"masamong/internal/config"
)

// A console change to one room (or any other typed config change) must keep every other server setting:
// the per-server blocked users, the privacy options and the log channel were once dropped from the saved
// file by every such change, on every server.
func TestConfigChangeKeepsEveryServerSetting(t *testing.T) {
	t.Parallel()
	svc := setupSvc(t)
	yes, no := true, false
	want := config.GuildSettings{
		Locale:       "ko",
		BlockedUsers: []string{"1100000000000000001"},
		Privacy:      config.GuildPrivacy{OwnCardOnly: true, NoHearsay: true, NoSensitive: true, CardChannelOnly: true},
		Log:          config.GuildLog{Channel: "1100000000000000002", Deletes: &yes, DeletedText: &no, Problems: &yes},
	}
	// every field of GuildSettings is set above: a new field must be added here, and kept by the clone
	v := reflect.ValueOf(want)
	for i := 0; i < v.NumField(); i++ {
		if name := v.Type().Field(i).Name; name != "Channels" && v.Field(i).IsZero() {
			t.Fatalf("set GuildSettings.%s in this test", name)
		}
	}
	applyDoc(t, svc, func(d *config.Document) { d.Guilds = map[string]config.GuildSettings{"g2": want} })

	for _, step := range []struct {
		act string
		in  map[string]any
	}{
		{"channels.configure", map[string]any{"guild_id": "g1", "id": "c1", "respond": "mention"}},
		{"channels.configure", map[string]any{"guild_id": "g2", "id": "c9", "respond": "mention"}},
	} {
		if r := exec(svc, step.act, step.in); !r.OK {
			t.Fatalf("%s: %+v", step.act, r.Error)
		}
		got := svc.Doc().Guilds["g2"]
		got.Channels = nil
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("after %s %v the server settings changed:\n got %+v\nwant %+v", step.act, step.in, got, want)
		}
	}
}
