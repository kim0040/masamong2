package episodes

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"masamong/internal/lang"
	"masamong/internal/memory/lexical"
)

var seoul = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Seoul")
	if err != nil {
		panic(err)
	}
	return loc
}()

// script builds messages from lines "ID YYYY-MM-DD HH:MM[:SS] author[>replyTo]: text" in Seoul time.
func script(t *testing.T, s string) []Message {
	t.Helper()
	var out []Message
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		line = strings.TrimSpace(line)
		head, text, ok := strings.Cut(line, ": ")
		if !ok {
			t.Fatalf("bad line %q", line)
		}
		f := strings.Fields(head)
		if len(f) != 4 {
			t.Fatalf("bad head %q", head)
		}
		layout := "2006-01-02 15:04"
		if strings.Count(f[2], ":") == 2 {
			layout = "2006-01-02 15:04:05"
		}
		at, err := time.ParseInLocation(layout, f[1]+" "+f[2], seoul)
		if err != nil {
			t.Fatal(err)
		}
		author, reply, _ := strings.Cut(f[3], ">")
		out = append(out, Message{ID: f[0], Account: "acct-" + author, Author: author, At: at, ReplyTo: reply, Bot: author == "bot", Text: text})
	}
	return out
}

func render(segs []Segment) string {
	var b strings.Builder
	for _, s := range segs {
		flags := ""
		if s.Closed {
			flags += " closed"
		}
		if s.Sealed {
			flags += " sealed"
		}
		fmt.Fprintf(&b, "b%d [%s]%s\n", s.Block, strings.Join(s.IDs, " "), flags)
	}
	return b.String()
}

func tokenizer(tag string) *lexical.Tokenizer {
	p, _ := lang.Get(tag)
	t := lexical.FromLanguage(p.Search)
	return &t
}

func TestSplitKoreanMultiDay(t *testing.T) {
	msgs := script(t, `
m1 2026-10-09 21:00 민수: 이번 주말 캠핑 갈 사람?
m2 2026-10-09 21:01 지영: 나 캠핑 좋아 토요일 가능
m3 2026-10-09 21:02 철수: 점심 뭐 먹었어? 나 라면 먹음
m4 2026-10-09 21:03 민수>m2: 그럼 토요일 캠핑 확정? 장소는 가평
m5 2026-10-09 21:03:30 민수: 텐트는 내가 챙길게
m6 2026-10-09 21:04 은비>m3: 나도 라면 먹었어 ㅋㅋ
m7 2026-10-09 21:05 지영>m4: 응 토요일 가평 캠핑 확정
m8 2026-10-09 23:50 민수: 아직 안 잔 사람 있어?
m9 2026-10-09 23:55 지영: 나 깨어 있음 영화 보는 중
m10 2026-10-10 00:02 민수: 무슨 영화 보는데
m11 2026-10-10 00:05 지영: 예전 공포 영화
m12 2026-10-10 04:50 지영: 결국 밤샘했다
m13 2026-10-10 05:05 민수: 나도 밤샘 아침 먹자
m14 2026-10-11 20:00 은비: 다들 캠핑 짐 쌌어?
m15 2026-10-11 20:01 민수: 거의 다 쌌어
`)
	now := time.Date(2026, 10, 11, 20, 5, 0, 0, seoul)
	got := render(Split(msgs, now, Options{Location: seoul, Tokenizer: tokenizer("ko")}))
	want := `b0 [m1 m2 m4 m5 m7] closed
b0 [m3 m6] closed
b1 [m8 m9 m10 m11] closed
b2 [m12] closed
b3 [m13] closed
b4 [m14 m15]
`
	if got != want {
		t.Fatalf("segments:\n%s\nwant:\n%s", got, want)
	}
}

func TestSplitEnglishInterleavedAndSoftCuts(t *testing.T) {
	msgs := script(t, `
e1 2026-10-09 19:00 Sam: anyone up for the lake trip next weekend?
e2 2026-10-09 19:01 Ana: lake trip sounds great, saturday works
e3 2026-10-09 19:02 Bob: did anyone watch the football game tonight
e4 2026-10-09 19:03 Joe: football game was insane, that last goal
e5 2026-10-09 19:04 Sam>e2: saturday lake trip then, I'll book the cabin
e6 2026-10-09 19:05 Bob: football refs were terrible though
e7 2026-10-09 19:06 Ana>e5: cabin booked works for me
e8 2026-10-09 19:40 Kim: totally different thing, my cat knocked over a plant
e9 2026-10-09 19:41 Kim: soil everywhere now
e10 2026-10-09 19:42 bot>e8: oh no, poor plant
`)
	now := time.Date(2026, 10, 9, 19, 45, 0, 0, seoul)
	got := render(Split(msgs, now, Options{Location: seoul, Tokenizer: tokenizer("en")}))
	want := `b0 [e1 e2 e5 e7] closed
b0 [e3 e4 e6] closed
b0 [e8 e9 e10]
`
	if got != want {
		t.Fatalf("segments:\n%s\nwant:\n%s", got, want)
	}
}

// Overnight talk that crosses midnight stays one conversation; the same gap across 05:00 cuts.
func TestSplitDayBoundary(t *testing.T) {
	msgs := script(t, `
n1 2026-10-09 23:58 A: still awake watching the movie
n2 2026-10-10 00:03 B: same movie here, great movie
n3 2026-10-10 04:58 A: movie marathon over
n4 2026-10-10 05:03 B: movie marathon over for me too
`)
	segs := Split(msgs, time.Date(2026, 10, 11, 0, 0, 0, 0, seoul), Options{Location: seoul, Tokenizer: tokenizer("en")})
	if render(segs) != "b0 [n1 n2] closed\nb1 [n3] closed\nb2 [n4] closed\n" {
		t.Fatalf("segments:\n%s", render(segs))
	}
	// with a midnight boundary the first pair is cut too
	segs = Split(msgs, time.Date(2026, 10, 11, 0, 0, 0, 0, seoul), Options{Location: seoul, DayBoundary: "00:00", Tokenizer: tokenizer("en")})
	if segs[0].Block == segs[1].Block {
		t.Fatalf("midnight boundary keeps them together:\n%s", render(segs))
	}
}

func TestSplitClosesLongSegmentsAndCapsLines(t *testing.T) {
	base := time.Date(2026, 10, 9, 20, 0, 0, 0, seoul)
	var msgs []Message
	for i := 0; i < 90; i++ {
		who := []string{"A", "B"}[i%2]
		msgs = append(msgs, Message{ID: fmt.Sprintf("x%d", i), Account: who, Author: who, At: base.Add(time.Duration(i) * 20 * time.Second), Text: "game plan for the raid tonight"})
	}
	segs := Split(msgs, base.Add(30*time.Minute), Options{Location: seoul, Tokenizer: tokenizer("en")})
	if len(segs) != 2 || segs[0].Lines != 80 || !segs[0].Closed || segs[1].Closed || segs[1].Lines != 10 {
		t.Fatalf("cap/close: %s", render(segs))
	}
	segs = Split(msgs[:61], base.Add(21*time.Minute), Options{Location: seoul, Tokenizer: tokenizer("en")})
	if len(segs) != 1 || !segs[0].Closed {
		t.Fatalf("60 lines close a segment: %s", render(segs))
	}
}

// Assignments depend only on earlier messages; a sealed segment takes nothing written after it was sealed.
func TestSplitIsOnlineAndRespectsSealed(t *testing.T) {
	msgs := script(t, `
s1 2026-10-09 21:00 민수: 이번 주말 캠핑 갈 사람?
s2 2026-10-09 21:01 지영: 나 캠핑 좋아 토요일 가능
s3 2026-10-09 21:02 철수: 점심 뭐 먹었어? 나 라면 먹음
s4 2026-10-09 21:20 민수>s2: 그럼 토요일 캠핑 확정
`)
	opt := Options{Location: seoul, Tokenizer: tokenizer("ko")}
	first := Split(msgs[:3], time.Date(2026, 10, 9, 21, 15, 0, 0, seoul), opt)
	if render(first) != "b0 [s1 s2] closed\nb0 [s3] closed\n" {
		t.Fatalf("prefix:\n%s", render(first))
	}
	full := Split(msgs, time.Date(2026, 10, 9, 21, 21, 0, 0, seoul), opt)
	if render(full) != "b0 [s1 s2 s4]\nb0 [s3] closed\n" {
		t.Fatalf("full:\n%s", render(full))
	}
	opt.Sealed = []Sealed{{IDs: first[0].IDs, At: time.Date(2026, 10, 9, 21, 15, 0, 0, seoul)}}
	sealed := Split(msgs, time.Date(2026, 10, 9, 21, 21, 0, 0, seoul), opt)
	if render(sealed) != "b0 [s1 s2] closed sealed\nb0 [s3] closed\nb0 [s4]\n" {
		t.Fatalf("sealed:\n%s", render(sealed))
	}
	if !slices.Equal(sealed[0].Authors, []string{"acct-민수", "acct-지영"}) {
		t.Fatalf("authors: %v", sealed[0].Authors)
	}
}

// Kakao rows have no account: the label is the speaker.
func TestSplitWithoutAccounts(t *testing.T) {
	msgs := script(t, `
k1 2026-10-09 21:00 민수: 캠핑 가자
k2 2026-10-09 21:01 지영: 캠핑 좋아
`)
	for i := range msgs {
		msgs[i].Account = ""
	}
	segs := Split(msgs, time.Date(2026, 10, 10, 0, 0, 0, 0, seoul), Options{Location: seoul})
	if len(segs) != 1 || !slices.Equal(segs[0].Authors, []string{"label:민수", "label:지영"}) {
		t.Fatalf("%s %v", render(segs), segs[0].Authors)
	}
}
