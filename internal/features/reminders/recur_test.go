package reminders

import (
	"reflect"
	"testing"
	"time"
)

func zone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestNextDailyKeepsTheWallClockAcrossDST(t *testing.T) {
	ny := zone(t, "America/New_York")
	r := Rule{Kind: RepeatDaily, Time: "08:00"}
	// the night before spring forward (2026-03-08) and the night before fall back (2026-11-01)
	for _, c := range []struct {
		after, want string
	}{
		{"2026-03-07T09:00:00-05:00", "2026-03-08T08:00:00-04:00"},
		{"2026-03-08T08:00:00-04:00", "2026-03-09T08:00:00-04:00"},
		{"2026-10-31T09:00:00-04:00", "2026-11-01T08:00:00-05:00"},
		{"2026-11-01T07:59:59-05:00", "2026-11-01T08:00:00-05:00"},
	} {
		after, _ := time.Parse(time.RFC3339, c.after)
		got, ok := r.Next(after, ny)
		if !ok || got.Format(time.RFC3339) != c.want {
			t.Fatalf("after %s: %s %v, want %s", c.after, got.Format(time.RFC3339), ok, c.want)
		}
	}
	// the day is 23 hours long on the spring-forward date: 24 hours later would be 09:00
	a, _ := r.Next(time.Date(2026, 3, 7, 9, 0, 0, 0, ny), ny)
	b, _ := r.Next(a, ny)
	if b.Sub(a) != 24*time.Hour || a.Sub(time.Date(2026, 3, 7, 8, 0, 0, 0, ny)) != 23*time.Hour {
		t.Fatalf("a=%v b=%v", a, b)
	}
}

func TestNextInADaylightSavingGapAndRepeat(t *testing.T) {
	ny := zone(t, "America/New_York")
	r := Rule{Kind: RepeatDaily, Time: "02:30"}
	// 02:30 does not exist on 2026-03-08: it still happens that day (once), and the next day is normal
	got, _ := r.Next(time.Date(2026, 3, 7, 12, 0, 0, 0, ny), ny)
	if got.In(ny).Format("2006-01-02") != "2026-03-08" {
		t.Fatalf("gap day: %v", got)
	}
	next, _ := r.Next(got, ny)
	if next.In(ny).Format("2006-01-02 15:04") != "2026-03-09 02:30" {
		t.Fatalf("after the gap: %v", next)
	}
	// 01:30 happens twice on 2026-11-01: one occurrence that day, not two
	r = Rule{Kind: RepeatDaily, Time: "01:30"}
	first, _ := r.Next(time.Date(2026, 10, 31, 12, 0, 0, 0, ny), ny)
	second, _ := r.Next(first, ny)
	if first.In(ny).Format("2006-01-02") != "2026-11-01" || second.In(ny).Format("2006-01-02") != "2026-11-02" {
		t.Fatalf("repeated hour: %v then %v", first, second)
	}
}

func TestNextWeeklyWeekdaySets(t *testing.T) {
	seoul := zone(t, "Asia/Seoul")
	r := Rule{Kind: RepeatWeekly, Time: "09:30", Weekdays: []int{3, 1, 1}} // Mon, Wed (given unsorted, duplicated)
	at := time.Date(2026, 10, 12, 9, 30, 0, 0, seoul)                      // a Monday, exactly on the time
	var got []string
	for i := 0; i < 4; i++ {
		var ok bool
		if at, ok = r.Next(at, seoul); !ok {
			t.Fatal("no next")
		}
		got = append(got, at.Format("2006-01-02 Mon 15:04"))
	}
	want := []string{"2026-10-14 Wed 09:30", "2026-10-19 Mon 09:30", "2026-10-21 Wed 09:30", "2026-10-26 Mon 09:30"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	// a single weekday later the same day, and the weekend set
	sun := Rule{Kind: RepeatWeekly, Time: "20:00", Weekdays: []int{0, 6}}
	n, _ := sun.Next(time.Date(2026, 10, 10, 19, 0, 0, 0, seoul), seoul) // Saturday 19:00
	if n.Format("2006-01-02 15:04") != "2026-10-10 20:00" {
		t.Fatalf("same day: %v", n)
	}
	n, _ = sun.Next(n, seoul)
	if n.Format("2006-01-02 Mon") != "2026-10-11 Sun" {
		t.Fatalf("sunday: %v", n)
	}
}

func TestNextMonthlyClampsToTheLastDay(t *testing.T) {
	seoul := zone(t, "Asia/Seoul")
	r := Rule{Kind: RepeatMonthly, Time: "10:00", Day: 31}
	at := time.Date(2026, 1, 15, 0, 0, 0, 0, seoul)
	var got []string
	for i := 0; i < 5; i++ {
		at, _ = r.Next(at, seoul)
		got = append(got, at.Format("2006-01-02"))
	}
	want := []string{"2026-01-31", "2026-02-28", "2026-03-31", "2026-04-30", "2026-05-31"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	// a leap year, and day 29/30 in February
	leap := Rule{Kind: RepeatMonthly, Time: "10:00", Day: 30}
	n, _ := leap.Next(time.Date(2028, 2, 1, 0, 0, 0, 0, seoul), seoul)
	if n.Format("2006-01-02") != "2028-02-29" {
		t.Fatalf("leap february: %v", n)
	}
	// the day has passed this month: next month; across a year end
	fifteen := Rule{Kind: RepeatMonthly, Time: "08:00", Day: 15}
	n, _ = fifteen.Next(time.Date(2026, 12, 20, 0, 0, 0, 0, seoul), seoul)
	if n.Format("2006-01-02") != "2027-01-15" {
		t.Fatalf("year end: %v", n)
	}
}

func TestNextEveryHoursIsOnAFixedGrid(t *testing.T) {
	ny := zone(t, "America/New_York")
	anchor := time.Date(2026, 3, 8, 0, 30, 0, 0, ny)
	r := Rule{Kind: RepeatHours, Every: 6, Anchor: anchor}
	n, _ := r.Next(time.Date(2026, 3, 7, 1, 0, 0, 0, ny), ny) // before the first
	if !n.Equal(anchor) {
		t.Fatalf("before the anchor: %v", n)
	}
	n, _ = r.Next(anchor, ny)
	if !n.Equal(anchor.Add(6 * time.Hour)) { // elapsed time, not wall clock: crosses the DST jump
		t.Fatalf("grid: %v", n)
	}
	// long after: the grid still lands on the anchor's phase
	late, _ := r.Next(anchor.Add(1000*time.Hour+time.Minute), ny)
	if d := late.Sub(anchor); d%(6*time.Hour) != 0 || d != 1002*time.Hour {
		t.Fatalf("late: %v", d)
	}
}

func TestLatestSkipsOlderMissedOccurrences(t *testing.T) {
	seoul := zone(t, "Asia/Seoul")
	daily := Rule{Kind: RepeatDaily, Time: "08:00"}
	grace := 30 * time.Minute
	// down for three days, back at 08:10: the 08:00 of today is the one to deliver
	now := time.Date(2026, 10, 14, 8, 10, 0, 0, seoul)
	o, ok := daily.Latest(now, grace, seoul)
	if !ok || !o.Equal(time.Date(2026, 10, 14, 8, 0, 0, 0, seoul)) {
		t.Fatalf("latest: %v %v", o, ok)
	}
	// back at 10:00: today's is past grace and yesterday's is older still
	if o, ok := daily.Latest(time.Date(2026, 10, 14, 10, 0, 0, 0, seoul), grace, seoul); ok {
		t.Fatalf("outside grace: %v", o)
	}
	// hourly with a long grace: one occurrence (the newest), not three
	hourly := Rule{Kind: RepeatHours, Every: 1, Anchor: time.Date(2026, 10, 14, 0, 0, 0, 0, seoul)}
	o, ok = hourly.Latest(time.Date(2026, 10, 14, 5, 20, 0, 0, seoul), 3*time.Hour, seoul)
	if !ok || !o.Equal(time.Date(2026, 10, 14, 5, 0, 0, 0, seoul)) {
		t.Fatalf("hourly latest: %v %v", o, ok)
	}
}

func TestRuleNormalizeAndWeekdayWords(t *testing.T) {
	for _, bad := range []Rule{
		{Kind: "yearly"}, {Kind: RepeatDaily, Time: "08:0"}, {Kind: RepeatDaily, Time: "24:00"}, {Kind: RepeatWeekly, Time: "08:00"},
		{Kind: RepeatWeekly, Time: "08:00", Weekdays: []int{7}}, {Kind: RepeatMonthly, Time: "08:00", Day: 0}, {Kind: RepeatMonthly, Time: "08:00", Day: 32},
		{Kind: RepeatHours, Every: 0, Anchor: time.Now()}, {Kind: RepeatHours, Every: MaxEveryHours + 1, Anchor: time.Now()}, {Kind: RepeatHours, Every: 2},
	} {
		if _, err := bad.Normalize(); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
		if _, ok := bad.Next(time.Now(), time.UTC); ok {
			t.Fatalf("computed %+v", bad)
		}
	}
	for in, want := range map[string][]int{"mon,wed": {1, 3}, "월 수 금": {1, 3, 5}, "Monday/FRI": {1, 5}, "weekdays": {1, 2, 3, 4, 5}, "토,일요일": {6, 0}, "月曜日": {1}, "0 6": {0, 6}, "lunes": {1}} {
		got, ok := ParseWeekdays(in)
		if !ok || !reflect.DeepEqual(got, want) {
			t.Fatalf("%q: %v %v want %v", in, got, ok, want)
		}
	}
	if _, ok := ParseWeekdays("funday"); ok {
		t.Fatal("accepted a word it does not know")
	}
}
