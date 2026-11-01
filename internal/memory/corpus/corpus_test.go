package corpus

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestClassify(t *testing.T) {
	cases := map[string]string{
		"안녕":            HumanText,
		"사진":            MediaMarker,
		"사진 3장":         MediaMarker,
		"메시지가 삭제되었습니다.": DeleteMarker,
		"철수님이 들어왔습니다.":  SystemMarker,
		"   ":           EmptyUnknown,
	}
	for in, want := range cases {
		if got := Classify(in, false, ""); got != want {
			t.Errorf("%q: %s want %s", in, got, want)
		}
	}
	if Classify("hi", true, "") != BotRecord || Classify("hi", false, BotRecord) != BotRecord {
		t.Fatal("bot flag must win")
	}
}

func TestParseTimeFrames(t *testing.T) {
	if _, aware, ok := ParseTime("2021-12-14T18:11:17"); !ok || aware {
		t.Fatal("naive kakao time")
	}
	if _, aware, ok := ParseTime("2026-08-29T10:54:44.123000+00:00"); !ok || !aware {
		t.Fatal("aware discord time")
	}
	if _, aware, ok := ParseTime("2026-08-29T10:54:44Z"); !ok || !aware {
		t.Fatal("Z suffix")
	}
	if _, _, ok := ParseTime("어제"); ok {
		t.Fatal("garbage parsed")
	}
}

func TestOrderByRoomKeepsRoomsApart(t *testing.T) {
	msgs := []Message{
		{ObservationID: "b2", Room: "discord:1:2", ParsedTime: "2026-01-02T00:00:00+00:00"},
		{ObservationID: "a1", Room: "discord:1:1", ParsedTime: "2026-01-03T00:00:00+00:00"},
		{ObservationID: "b1", Room: "discord:1:2", ParsedTime: "2026-01-01T00:00:00+00:00"},
	}
	got := OrderByRoom(msgs)
	if got[0].ObservationID != "a1" || got[1].ObservationID != "b1" || got[2].ObservationID != "b2" {
		t.Fatalf("%v", got)
	}
}

func TestValidateMergeRefusesDiscord(t *testing.T) {
	if err := ValidateMerge(map[string]string{"discord:1:1": "discord:1:2"}); err == nil {
		t.Fatal("discord channels must never merge")
	}
	if err := ValidateMerge(map[string]string{"kakao:a": "kakao:ab"}); err != nil {
		t.Fatal(err)
	}
}

func TestKakaoCSVAndLiveIDs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chat.csv")
	body := "\xef\xbb\xbfDate,User,Message\n2021-12-14 18:11:17,철수,안녕\n2021-12-14 18:12:00,영희,사진\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var got []Observation
	sha, n, err := ReadKakaoCSV(path, "friends", "kakao:friends", func(o Observation) error { got = append(got, o); return nil })
	if err != nil || n != 2 || len(sha) != 64 {
		t.Fatalf("%v %d %q", err, n, sha)
	}
	if got[0].ParsedTime != "2021-12-14T18:11:17" || !got[0].Eligible() || got[1].Eligible() {
		t.Fatalf("%+v", got)
	}
	if got[0].ObservationID != KakaoStableObservationID("kakao:friends", "2021-12-14 18:11:17", "철수", "안녕", 0) || got[0].LegacyID != KakaoObservationID("kakao:friends", sha, 1) || got[0].ActorAccount != "" {
		t.Fatal("kakao id / account")
	}
	if DiscordLiveObservationID("discord:1:2", "9") == DiscordLiveObservationID("discord:1:3", "9") {
		t.Fatal("live ids must be room-scoped")
	}
}

// Real exports contain odd messages: a stray quote must not reject the whole file (Python's csv
// module, which built the raw corpus, accepts it too), and a CP949 re-save from Excel still reads.
func TestKakaoCSVToleratesStrayQuotesAndCP949(t *testing.T) {
	dir := t.TempDir()
	stray := filepath.Join(dir, "stray.csv")
	body := "Date,User,Message\n2021-12-14 18:11:17,철수,그 영화 \"진짜 재밌더라\n2021-12-14 18:12:00,영희,\"인용\"\"안\"\"\"\n"
	if err := os.WriteFile(stray, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var got []Observation
	if _, n, err := ReadKakaoCSV(stray, "c", "kakao:c", func(o Observation) error { got = append(got, o); return nil }); err != nil || n != 2 {
		t.Fatalf("stray quote: %v %d", err, n)
	}
	if got[0].Content != `그 영화 "진짜 재밌더라` || got[1].Content != `인용"안"` {
		t.Fatalf("contents %q %q", got[0].Content, got[1].Content)
	}
	// "Date,User,Message\n2021-12-14 18:11:17,철수,안녕" in CP949
	cp949 := append([]byte("Date,User,Message\n2021-12-14 18:11:17,"), 0xC3, 0xB6, 0xBC, 0xF6, ',', 0xBE, 0xC8, 0xB3, 0xE7, '\n')
	p := filepath.Join(dir, "cp949.csv")
	if err := os.WriteFile(p, cp949, 0o600); err != nil {
		t.Fatal(err)
	}
	got = nil
	if _, n, err := ReadKakaoCSV(p, "c", "kakao:c", func(o Observation) error { got = append(got, o); return nil }); err != nil || n != 1 {
		t.Fatalf("cp949: %v %d", err, n)
	}
	if got[0].ActorLabel != "철수" || got[0].Content != "안녕" {
		t.Fatalf("cp949 decoded %q %q", got[0].ActorLabel, got[0].Content)
	}
	bin := filepath.Join(dir, "bin.csv")
	if err := os.WriteFile(bin, []byte{0x89, 'P', 'N', 'G', 0xff, 0xfe, 0x00, 0x81}, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadKakaoCSV(bin, "c", "kakao:c", func(Observation) error { return nil }); err == nil {
		t.Fatal("a binary file must be refused")
	}
}

// TestKakaoObservationIDMatchesRawCorpus recomputes IDs from any Kakao collection in the
// configured private raw corpus. It is skipped when no private corpus is available.
func TestKakaoObservationIDMatchesRawCorpus(t *testing.T) {
	root := os.Getenv("RAW_CORPUS_ROOT")
	if root == "" {
		t.Skip("RAW_CORPUS_ROOT not set")
	}
	paths, err := filepath.Glob(filepath.Join(root, "*", "dated.jsonl.gz"))
	if err != nil || len(paths) == 0 {
		t.Skip("no private raw corpus collections")
	}
	n := 0
	for _, path := range paths {
		f, openErr := os.Open(path)
		if openErr != nil {
			t.Fatal(openErr)
		}
		gz, gzipErr := gzip.NewReader(f)
		if gzipErr != nil {
			f.Close()
			t.Fatal(gzipErr)
		}
		sc := bufio.NewScanner(gz)
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		for sc.Scan() && n < 5000 {
			var rec struct {
				ObservationID string `json:"observation_id"`
				Platform      string `json:"platform"`
				SourceRoomID  string `json:"source_room_id"`
				Source        struct {
					Fingerprint string `json:"fingerprint"`
					Ordinal     int    `json:"record_ordinal"`
				} `json:"source"`
			}
			if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
				t.Fatal(err)
			}
			if rec.Platform != "kakao" {
				continue
			}
			if got := KakaoObservationID(rec.SourceRoomID, rec.Source.Fingerprint, rec.Source.Ordinal); got != rec.ObservationID {
				t.Fatalf("record %d: id %s want %s", n, got, rec.ObservationID)
			}
			n++
		}
		if err := sc.Err(); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			f.Close()
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		if n >= 5000 {
			break
		}
	}
	if n == 0 {
		t.Skip("private corpus has no Kakao observations")
	}
	t.Logf("%d kakao observation ids reproduced", n)
}
