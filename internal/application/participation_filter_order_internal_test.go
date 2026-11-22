package application

import (
	"context"
	"testing"

	"masamong/internal/action"
	"masamong/internal/config"
	"masamong/internal/features/privacy"
)

// When several reasons hold at once, Observe counts the first in a fixed order. The store-backed reasons
// (consent, pause) are read before part.mu since fix plan R03; this pins that the order did not change.
func TestObserveFilterReasonOrder(t *testing.T) {
	s, clk := banterLimitsService(t)
	_, drain := ManualParticipationForTest(s)
	doc := s.Doc()
	doc.Discord.Enabled = true
	doc.Memory.Capture = true
	doc.Participation = config.Participation{Enabled: true, Mode: "collect"}
	doc.Access.BlockedUsers = []string{"blocked-user"}
	pol := func(mode, nameCall string) config.ChannelPolicy {
		return config.ChannelPolicy{Respond: "mention", Participation: mode, NameCall: nameCall}
	}
	doc.Guilds = map[string]config.GuildSettings{"g": {Locale: "ko", Channels: map[string]config.ChannelPolicy{
		"c1": pol("conservative", ""), "c2": pol("conservative", ""), "c3": pol("conservative", ""),
		"c4": pol("collect", ""), "c5": pol("collect", ""), "c6": pol("collect", ""),
		"c7": pol("collect", "off"), "c8": pol("collect", "off"), "c9": pol("collect", "off"),
	}}}
	s.setDoc(doc, s.Revision())
	for _, w := range []struct{ user, scope string }{
		{"blocked-user", privacy.Participation}, {"no-part", privacy.Participation}, {"no-memory", privacy.Memory},
	} {
		if _, err := s.decideConsent(w.user, w.scope, privacy.Withdrawn, "test"); err != nil {
			t.Fatal(err)
		}
	}
	for _, ch := range []string{"c1", "c2", "c3", "c4", "c7"} {
		if r := s.Execute(context.Background(), action.Request{Action: "runtime.pause",
			Input: []byte(`{"scope":"channel","guild_id":"g","channel_id":"` + ch + `"}`)}); !r.OK {
			t.Fatal(r.Error)
		}
	}
	question := "누가 텐트 챙기기로 했지?"
	type observeCase struct {
		name, channel, author, content, botCall, want string
	}
	check := func(cases []observeCase) {
		t.Helper()
		for _, tc := range cases {
			s.Observe(ObservedMessage{GuildID: "g", ChannelID: tc.channel, MessageID: "m-" + tc.channel, AuthorID: tc.author,
				AuthorName: tc.author, Content: tc.content, At: clk.t, BotCall: tc.botCall})
			s.part.mu.Lock()
			got := map[string]int{}
			for k, v := range s.part.chans["g:"+tc.channel].filtered {
				got[k] = v
			}
			s.part.mu.Unlock()
			want := map[string]int{}
			if tc.want != "" {
				want[tc.want] = 1
			}
			if len(got) != len(want) || got[tc.want] != want[tc.want] {
				t.Errorf("%s: filtered %v, want %v", tc.name, got, want)
			}
		}
	}
	check([]observeCase{
		{"blocked first", "c1", "blocked-user", question, "", "blocked"},
		{"withdrawn before paused", "c2", "no-part", question, "", "opted_out"},
		{"paused", "c3", "someone", question, "", "paused"},
		{"memory withdrawn in collect", "c6", "no-memory", question, "", "opted_out"},
		// a name said in collect mode is checked by collectSkip, whose order puts consent before the pause
		{"name mention: withdrawn before paused", "c7", "no-memory", "마사몽", "name", "opted_out"},
		{"name mention recorded", "c8", "someone", "마사몽", "name", ""},
	})
	drain()
	if n := countWhere(t, s, `SELECT COUNT(*) FROM participation_log WHERE channel_id='c8' AND state='collected'`); n != 1 {
		t.Fatalf("the name mention in a collecting channel was not recorded: %d rows", n)
	}
	// capture off for the whole instance: collect mode records nothing anywhere
	doc = s.Doc()
	doc.Memory.Capture = false
	s.setDoc(doc, s.Revision())
	check([]observeCase{
		{"paused before capture off", "c4", "someone", question, "", "paused"},
		{"capture off before memory withdrawn", "c5", "no-memory", question, "", "capture_off"},
		{"name mention: capture off", "c9", "someone", "마사몽", "name", "capture_off"},
	})
	drain()
	if n := countWhere(t, s, `SELECT COUNT(*) FROM participation_log WHERE channel_id<>'c8'`); n != 0 {
		t.Fatalf("filtered messages were recorded: %d rows", n)
	}
}
