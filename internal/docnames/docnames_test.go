package docnames

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

var testKnown = []string{"fortune-register", "fortune", "weather", "날씨", "天気"}

func init() {
	sort.Slice(testKnown, func(i, j int) bool { return len(testKnown[i]) > len(testKnown[j]) })
}

func TestMatch(t *testing.T) {
	for _, c := range []struct {
		word, want string
	}{
		{"weather", "weather"},
		{"날씨", "날씨"},
		{"날씨를", "날씨"},           // a CJK name runs into a particle
		{"天気です", "天気"},          // same for ja
		{"weather를", "weather"}, // an ASCII name may only run into another script
		{"weathers", ""},        // never more ASCII
		{"admins", ""},
		{"weather-station", ""},
		{"fortune-register", "fortune-register"}, // longest first, not "fortune"
		{"other", ""},
	} {
		if got := Match(c.word, testKnown); got != c.want {
			t.Errorf("Match(%q) = %q, want %q", c.word, got, c.want)
		}
	}
}

func TestAside(t *testing.T) {
	re := Aside("날씨", "weather")
	for _, ok := range []string{
		"/날씨(/weather)", "/날씨 (/weather)", "`/날씨`(`/weather`)", "`/날씨` (`/weather`)",
		"/날씨（`/weather`）", "`/날씨`（/weather）",
	} {
		if !re.MatchString(ok) {
			t.Errorf("aside should match %q", ok)
		}
	}
	for _, no := range []string{"/weather(/날씨)", "/날씨 /weather", "/날씨(날씨)"} {
		if re.MatchString(no) {
			t.Errorf("aside should not match %q", no)
		}
	}
}

// Scan finds command mentions outside code, paths, quotes and marked lines.
func TestScan(t *testing.T) {
	var found []string
	scan := func(text string, asides ...*regexp.Regexp) []string {
		found = nil
		Scan(text, asides, func(w string, _ int) { found = append(found, w) })
		return found
	}
	if got := scan("날씨는 /weather 라 부른다"); strings.Join(found, ",") != "weather" {
		t.Fatalf("plain mention: %v", got)
	}
	if got := scan("```bash\ncurl x/search /weather\n```\nand /날씨"); strings.Join(found, ",") != "날씨" {
		t.Fatalf("fenced code skipped: %v", got)
	}
	if got := scan("./weather ~/weather path=/weather \"/weather\" '/weather' /weather/x"); len(got) != 0 {
		t.Fatalf("path-like and quoted tokens skipped: %v", got)
	}
	if got := scan(`Finnhub의 /weather 경로 <!-- doccheck:not-command -->`); len(got) != 0 {
		t.Fatalf("ignore marker: %v", got)
	}
	if got := scan("/날씨 (`/weather`) 그리고 `/날씨`（/weather）", Aside("날씨", "weather")); len(got) != 0 {
		t.Fatalf("asides removed: %v", got)
	}
}

// A fenced block removed for the scan keeps the lines that follow it.
func TestScanKeepsLineNumbers(t *testing.T) {
	var line int
	Scan("a\n```\n/code\n/inside\n```\nb /weather", nil, func(_ string, l int) { line = l })
	if line != 6 {
		t.Fatalf("line = %d, want 6", line)
	}
}

func TestRegistryNames(t *testing.T) {
	src := `package discord

func Commands() []Command {
	cmds := []Command{
		{Name: "weather", Desc: "d", Action: "weather.query", Options: []*discordgo.ApplicationCommandOption{option(str, "region", "x", false)}},
		{
			Name: "subscribe", Desc: "d", Action: "fortune.subscribe", PrefixOnly: true,
		},
		{Name: "help", Desc: "d", Action: "help.version"},
	}
	return cmds
}
`
	got, err := RegistryNames([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "help,weather" {
		t.Fatalf("names: %v", got)
	}
	if _, err := RegistryNames([]byte("package discord\n")); err == nil {
		t.Fatal("no Commands: expected an error")
	}
}
