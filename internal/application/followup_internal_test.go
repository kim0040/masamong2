package application

import "testing"

func TestScoreFollow(t *testing.T) {
	cases := []struct {
		f    followFeatures
		want int
	}{
		{followFeatures{}, 2},
		{followFeatures{Asked: true}, 3},
		{followFeatures{NobodyElse: true}, 3},
		{followFeatures{Asked: true, NobodyElse: true, Cue: true}, 5},
		{followFeatures{NobodyElse: true, Elsewhere: true}, -1},
		{followFeatures{Asked: true, NameOther: true}, 0},
		{followFeatures{Empty: true, NobodyElse: true}, 1},
		{followFeatures{Busy: true}, 0},
	}
	for _, c := range cases {
		if got := scoreFollow(c.f); got != c.want {
			t.Errorf("%+v: %d, want %d", c.f, got, c.want)
		}
	}
}

func TestEndsWithQuestion(t *testing.T) {
	for s, want := range map[string]bool{"내일 갈 거야?": true, "どこ行く？": true, "really? 😄": true, "ok.": false, "응!": false, "what time? 🙂🙂 ": true, "": false} {
		if got := endsWithQuestion(s); got != want {
			t.Errorf("%q: %v", s, got)
		}
	}
}
