package reminders

import (
	"testing"
	"time"

	"masamong/internal/lang"
)

func TestParseKorean(t *testing.T) {
	seoul, _ := time.LoadLocation("Asia/Seoul")
	// Monday 2026-09-28 10:00
	now := time.Date(2026, 9, 28, 10, 0, 0, 0, seoul)
	words := lang.For("ko", "ko").Time
	at := func(mo, d, h, m int) time.Time { return time.Date(2026, time.Month(mo), d, h, m, 0, 0, seoul) }
	cases := []struct {
		in   string
		want []time.Time
		rest string
	}{
		{"내일 오후 3시에 회의 알려줘", []time.Time{at(9, 29, 15, 0)}, "회의"},
		{"30분 뒤에 빨래 꺼내기", []time.Time{at(9, 28, 10, 30)}, "빨래 꺼내기"},
		{"2시간 후 약 먹기", []time.Time{at(9, 28, 12, 0)}, "약 먹기"},
		{"3시 반에 전화", []time.Time{at(9, 28, 15, 30), at(9, 29, 3, 30)}, "전화"},
		{"내일 3시 병원", []time.Time{at(9, 29, 3, 0), at(9, 29, 15, 0)}, "병원"},
		{"다음주 수요일 점심 약속", []time.Time{at(10, 7, 12, 0)}, "약속"},
		{"금요일 저녁 7시 동창회", []time.Time{at(10, 2, 19, 0)}, "동창회"},
		{"10월 3일 새벽 5시 등산", []time.Time{at(10, 3, 5, 0)}, "등산"},
		{"2026-10-05 14:00 발표", []time.Time{at(10, 5, 14, 0)}, "발표"},
		{"모레 아침에 택배", []time.Time{at(9, 30, 8, 0)}, "택배"},
		{"15일 월세 내기", []time.Time{at(10, 15, 9, 0)}, "월세 내기"},
		{"오늘 밤 9시 게임", []time.Time{at(9, 28, 21, 0)}, "게임"},
	}
	for _, c := range cases {
		w, err := Parse(c.in, now, seoul, words, Options{})
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if len(w.Candidates) != len(c.want) {
			t.Errorf("%q: %d candidates %v, want %v", c.in, len(w.Candidates), w.Candidates, c.want)
			continue
		}
		for i := range c.want {
			if !w.Candidates[i].At.Equal(c.want[i]) {
				t.Errorf("%q: candidate %d = %v, want %v", c.in, i, w.Candidates[i].At, c.want[i])
			}
		}
		if w.Rest != c.rest {
			t.Errorf("%q: rest %q, want %q", c.in, w.Rest, c.rest)
		}
	}
	if _, err := Parse("회의 알려줘", now, seoul, words, Options{}); err != ErrNoTime {
		t.Fatalf("no time: %v", err)
	}
	if _, err := Parse("오늘 오전 8시 회의", now, seoul, words, Options{}); err != ErrPast {
		t.Fatalf("past: %v", err)
	}
	if w, _ := Parse("다음주 월요일", now, seoul, words, Options{DefaultTime: "10:30"}); len(w.Candidates) != 1 || !w.Candidates[0].At.Equal(at(10, 5, 10, 30)) || w.Candidates[0].Precision != DayOnly {
		t.Fatalf("a day alone takes the default time: %+v", w)
	}
}

func TestParseEnglish(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	now := time.Date(2026, 9, 28, 10, 0, 0, 0, ny) // Monday
	words := lang.For("en", "en").Time
	at := func(mo, d, h, m int) time.Time { return time.Date(2026, time.Month(mo), d, h, m, 0, 0, ny) }
	cases := []struct {
		in   string
		want []time.Time
		rest string
	}{
		{"remind me tomorrow at 3pm to call mom", []time.Time{at(9, 29, 15, 0)}, "call mom"},
		{"in 45 minutes check the oven", []time.Time{at(9, 28, 10, 45)}, "check the oven"},
		{"next friday lunch with Sam", []time.Time{at(10, 9, 12, 0)}, "with Sam"}, // "lunch" is the time of day
		{"October 3 at 9:30 dentist", []time.Time{at(10, 3, 9, 30), at(10, 3, 21, 30)}, "dentist"},
		{"on 10/12 at 7pm dinner", []time.Time{at(10, 12, 19, 0)}, "dinner"},
	}
	for _, c := range cases {
		w, err := Parse(c.in, now, ny, words, Options{})
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if len(w.Candidates) != len(c.want) {
			t.Errorf("%q: %d candidates %v, want %v", c.in, len(w.Candidates), w.Candidates, c.want)
			continue
		}
		for i := range c.want {
			if !w.Candidates[i].At.Equal(c.want[i]) {
				t.Errorf("%q: candidate %d = %v, want %v", c.in, i, w.Candidates[i].At, c.want[i])
			}
		}
		if w.Rest != c.rest {
			t.Errorf("%q: rest %q, want %q", c.in, w.Rest, c.rest)
		}
	}
}

func TestPastRelativeTimesAreRefusedAndEnglishWorksWithKoreanDefault(t *testing.T) {
	seoul, _ := time.LoadLocation("Asia/Seoul")
	now := time.Date(2026, 9, 28, 10, 0, 0, 0, seoul)
	ko := lang.For("ko", "ko").Time
	for _, input := range []string{"어제 9시 회의", "yesterday 9am meeting"} {
		if _, err := Parse(input, now, seoul, ko, Options{}); err != ErrPast {
			t.Errorf("past reminder %q: got %v, want ErrPast", input, err)
		}
	}
	w, err := Parse("in 10 minutes drink water", now, seoul, ko, Options{})
	if err != nil || len(w.Candidates) != 1 || !w.Candidates[0].At.Equal(now.Add(10*time.Minute)) || w.Rest != "drink water" {
		t.Fatalf("English expression in a Korean server: %+v %v", w, err)
	}
}

// The Japanese, Chinese and Spanish packs: particles glued to the time ("3時に会議"), a particle between
// "next week" and the weekday ("来週の水曜日") and function words around the time all stay out of the text.
func TestParseOtherLanguages(t *testing.T) {
	seoul, _ := time.LoadLocation("Asia/Seoul")
	now := time.Date(2026, 9, 28, 10, 0, 0, 0, seoul) // Monday
	at := func(mo, d, h, m int) time.Time { return time.Date(2026, time.Month(mo), d, h, m, 0, 0, seoul) }
	cases := map[string][]struct {
		in   string
		want []time.Time
		rest string
	}{
		"ja": {
			{"明日の午後3時に会議", []time.Time{at(9, 29, 15, 0)}, "会議"},
			{"30分後に洗濯物", []time.Time{at(9, 28, 10, 30)}, "洗濯物"},
			{"3時半に電話", []time.Time{at(9, 28, 15, 30), at(9, 29, 3, 30)}, "電話"},
			{"来週の水曜日の昼に約束", []time.Time{at(10, 7, 12, 0)}, "約束"},
			{"10月3日の朝5時に登山", []time.Time{at(10, 3, 5, 0)}, "登山"},
			{"今夜9時にゲーム", []time.Time{at(9, 28, 21, 0)}, "ゲーム"},
			{"今晩8時に映画", []time.Time{at(9, 28, 20, 0)}, "映画"},
		},
		"zh-Hans": {
			{"明天下午3点开会", []time.Time{at(9, 29, 15, 0)}, "开会"},
			{"30分钟后收衣服", []time.Time{at(9, 28, 10, 30)}, "收衣服"},
			{"下周三中午约会", []time.Time{at(10, 7, 12, 0)}, "约会"},
			{"下个星期五下午2点面试", []time.Time{at(10, 9, 14, 0)}, "面试"},
			{"15号交房租", []time.Time{at(10, 15, 9, 0)}, "交房租"},
			{"今晚8点看电影", []time.Time{at(9, 28, 20, 0)}, "看电影"},
		},
		"es": {
			{"mañana a las 3 pm reunión", []time.Time{at(9, 29, 15, 0)}, "reunión"},
			{"en 30 minutos sacar la ropa", []time.Time{at(9, 28, 10, 30)}, "sacar la ropa"},
			{"el 15 pagar el alquiler", []time.Time{at(10, 15, 9, 0)}, "pagar el alquiler"},
			{"el próximo miércoles al mediodía cita", []time.Time{at(10, 7, 12, 0)}, "cita"},
			{"pasado mañana por la mañana paquete", []time.Time{at(9, 30, 8, 0)}, "paquete"},
			{"mañana por la tarde ir al cine", []time.Time{at(9, 29, 15, 0)}, "ir al cine"},
		},
	}
	for tag, list := range cases {
		words := lang.For(tag, tag).Time
		for _, c := range list {
			w, err := Parse(c.in, now, seoul, words, Options{})
			if err != nil {
				t.Errorf("%s %q: %v", tag, c.in, err)
				continue
			}
			if len(w.Candidates) != len(c.want) {
				t.Errorf("%s %q: %d candidates %v, want %v", tag, c.in, len(w.Candidates), w.Candidates, c.want)
				continue
			}
			for i := range c.want {
				if !w.Candidates[i].At.Equal(c.want[i]) {
					t.Errorf("%s %q: candidate %d = %v, want %v", tag, c.in, i, w.Candidates[i].At, c.want[i])
				}
			}
			if w.Rest != c.rest {
				t.Errorf("%s %q: rest %q, want %q", tag, c.in, w.Rest, c.rest)
			}
		}
	}
}

// A wall-clock time on the day summer time starts that does not exist moves forward, never back.
func TestParseAcrossSummerTime(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	now := time.Date(2027, 3, 13, 12, 0, 0, 0, ny) // the day before the spring change (2027-03-14)
	w, err := Parse("tomorrow at 2:30am", now, ny, lang.For("en", "en").Time, Options{})
	if err != nil || len(w.Candidates) != 1 || w.Candidates[0].At.Before(now) {
		t.Fatalf("%+v %v", w, err)
	}
}

func TestParseMonthEndsAndRejectsInvalidExplicitDate(t *testing.T) {
	utc := time.UTC
	ko := lang.For("ko", "ko").Time
	now := time.Date(2026, 1, 31, 10, 0, 0, 0, utc)
	w, err := Parse("15일 15:30 회의", now, utc, ko, Options{})
	if err != nil || len(w.Candidates) != 1 || !w.Candidates[0].At.Equal(time.Date(2026, 2, 15, 15, 30, 0, 0, utc)) {
		t.Fatalf("month rollover skipped February: %+v %v", w, err)
	}
	now = time.Date(2026, 1, 31, 16, 0, 0, 0, utc)
	w, err = Parse("31일 15:30 월세", now, utc, ko, Options{})
	if err != nil || len(w.Candidates) != 1 || !w.Candidates[0].At.Equal(time.Date(2026, 2, 28, 15, 30, 0, 0, utc)) {
		t.Fatalf("month end should clamp to February's last day: %+v %v", w, err)
	}
	for _, input := range []string{"2026-02-30 at 15:30 call", "2/30 at 15:30 call"} {
		if _, err := Parse(input, now, utc, lang.For("en", "en").Time, Options{}); err != ErrInvalidDate {
			t.Errorf("%q: got %v, want ErrInvalidDate", input, err)
		}
	}
}
