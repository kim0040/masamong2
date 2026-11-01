package application

import (
	"testing"
	"time"

	"masamong/internal/config"
)

func TestQuietUntil(t *testing.T) {
	s := &Service{}
	s.cfg.Store(&configSnapshot{doc: config.Default("t", ""), rev: 1})
	seoul, _ := time.LoadLocation("Asia/Seoul")
	night := UserPrefs{QuietStart: "23:00", QuietEnd: "07:00", Timezone: "Asia/Seoul"}
	at := func(h, m int) time.Time { return time.Date(2026, 9, 28, h, m, 0, 0, seoul) }
	for _, c := range []struct {
		p        UserPrefs
		in, want time.Time
	}{
		{night, at(22, 59), at(22, 59)},
		{night, at(23, 30), at(7, 0).AddDate(0, 0, 1)},
		{night, at(3, 0), at(7, 0)},
		{night, at(7, 0), at(7, 0)},
		{UserPrefs{QuietStart: "13:00", QuietEnd: "14:00", Timezone: "Asia/Seoul"}, at(13, 30), at(14, 0)},
		{UserPrefs{}, at(3, 0), at(3, 0)},
	} {
		if got := s.quietUntil(c.p, c.in); !got.Equal(c.want) {
			t.Fatalf("%+v at %v: got %v want %v", c.p, c.in, got, c.want)
		}
	}
}
