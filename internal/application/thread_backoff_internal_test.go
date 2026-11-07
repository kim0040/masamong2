package application

import (
	"testing"
	"time"
)

// The rest grows with each offer passed over and stops at the cap.
func TestThreadBackoffRestGrowsAndStopsAtTheCap(t *testing.T) {
	for streak, want := range map[int]time.Duration{
		0: 0, 1: 0, // two passed-over offers cost nothing
		2: 6 * time.Hour, 3: 12 * time.Hour, 4: 24 * time.Hour, 5: 48 * time.Hour, 6: 96 * time.Hour,
		7: 7 * 24 * time.Hour, 8: 7 * 24 * time.Hour, 500: 7 * 24 * time.Hour,
	} {
		if got := threadBackoffRest(streak); got != want {
			t.Errorf("rest after %d passed-over offers = %v, want %v", streak, got, want)
		}
	}
}

// The count reads the log newest first: an opened thread starts it over (except one the bot opened by
// itself), an offer that never showed or is too young to have been answered is not passed over.
func TestThreadBackoffCountsOnlyPassedOverOffers(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	row := func(decision string, d time.Duration) backoffRow { return backoffRow{decision: decision, at: ago(d)} }
	for _, tc := range []struct {
		name string
		rows []backoffRow // newest first
		want bool
	}{
		{"no history", nil, false},
		{"one offer ignored", []backoffRow{row("offer", time.Hour)}, false},
		{"two ignored, inside the rest", []backoffRow{row("offer", time.Hour), row("offer", 3*time.Hour)}, true},
		{"two ignored, rest over", []backoffRow{row("offer", 7*time.Hour), row("offer", 9*time.Hour)}, false},
		{"three ignored, 7h is inside the 12h rest", []backoffRow{row("offer", 7*time.Hour), row("offer", 9*time.Hour), row("offer", 20*time.Hour)}, true},
		{"offers before an opened thread do not count", []backoffRow{row("offer", time.Hour), row("opened", 3*time.Hour), row("offer", 4*time.Hour), row("offer", 5*time.Hour)}, false},
		{"two ignored after an opened thread count", []backoffRow{row("offer", time.Hour), row("offer", 2*time.Hour), row("opened", 3*time.Hour), row("offer", 4*time.Hour)}, true},
		{"opened after the two ignored ones", []backoffRow{row("opened", 30*time.Minute), row("offer", time.Hour), row("offer", 2*time.Hour)}, false},
		{"a thread the bot opened by itself is not a yes", []backoffRow{row("opened", 30*time.Minute), row("auto", 31*time.Minute), row("offer", time.Hour), row("offer", 2*time.Hour)}, true},
		{"an offer never shown is not passed over", []backoffRow{row("offer", time.Hour), {decision: "offer", outcome: threadOutcomeNotShown, at: ago(2 * time.Hour)}}, false},
		{"an offer too young to be answered is not counted yet", []backoffRow{row("offer", 5*time.Minute), row("offer", 3*time.Hour), row("offer", 5*time.Hour)}, true},
		{"two old ones and one too young: still two", []backoffRow{row("offer", 5*time.Minute), row("offer", 7*time.Hour), row("offer", 9*time.Hour)}, true},
	} {
		if got := threadBackoffAt(tc.rows, now); got != tc.want {
			t.Errorf("%s: backed off = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// hasCue matches Latin cues as words and other scripts anywhere.
func TestHasCueMatchesLatinCuesAsWords(t *testing.T) {
	for _, tc := range []struct {
		text string
		cues []string
		want bool
	}{
		{"lol that was funny", []string{"lol"}, true},
		{"a lollipop", []string{"lol"}, false},
		{"thanks!", []string{"thanks"}, true},
		{"no thanksgiving", []string{"thanks"}, false},
		{"고마워요 마사몽", []string{"고마워"}, true},
		{"ㅋㅋㅋㅋ", []string{"ㅋㅋ"}, true},
		{"thank you so much", []string{"thx", "thank you"}, true},
		{"anything", nil, false},
	} {
		if got := hasCue(tc.text, tc.cues); got != tc.want {
			t.Errorf("hasCue(%q, %v) = %v, want %v", tc.text, tc.cues, got, tc.want)
		}
	}
}
