package relnotes

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"masamong/locales"
)

func note(version string) Note {
	return Note{
		Version:  version,
		Date:     "2026-11-22",
		User:     map[string][]string{"ko": {"사용자 변경"}, "en": {"A user change"}},
		Operator: map[string][]string{"ko": {"운영자 변경"}, "en": {"An operator change"}},
	}
}

func fsOf(versions ...string) fstest.MapFS {
	m := fstest.MapFS{}
	for _, v := range versions {
		b, _ := Marshal(note(v))
		m[v+".json"] = &fstest.MapFile{Data: b}
	}
	return m
}

// The committed notes are valid and every file is named after its version: a bad note never reaches a build.
func TestEmbeddedNotesAreValid(t *testing.T) {
	if err := EmbeddedError(); err != nil {
		t.Fatal(err)
	}
	all := All()
	if len(all) == 0 {
		t.Fatal("no embedded notes")
	}
	for i := 1; i < len(all); i++ {
		if Compare(all[i-1].Version, all[i].Version) >= 0 {
			t.Errorf("All() is not sorted oldest first: %s before %s", all[i-1].Version, all[i].Version)
		}
	}
	for _, n := range all {
		if err := n.Validate(); err != nil {
			t.Error(err)
		}
	}
}

// Languages is the console's language list: a console language added without a note language (or the
// reverse) is noticed here.
func TestLanguagesMatchConsoleLocales(t *testing.T) {
	entries, err := locales.UIFS.ReadDir("ui")
	if err != nil {
		t.Fatal(err)
	}
	var tags []string
	for _, e := range entries {
		tags = append(tags, strings.TrimSuffix(e.Name(), ".json"))
	}
	if len(tags) != len(Languages) {
		t.Fatalf("console languages %v, note languages %v", tags, Languages)
	}
	for _, tag := range tags {
		found := false
		for _, l := range Languages {
			found = found || l == tag
		}
		if !found {
			t.Errorf("console language %q is not in relnotes.Languages", tag)
		}
	}
}

func TestParseVersion(t *testing.T) {
	for _, tc := range []struct {
		in    string
		ok    bool
		after bool
		pre   string
	}{
		{"v1.5.1", true, false, ""},
		{"1.5.1", true, false, ""},
		{"v1.5.1-rc1", true, false, "rc1"},
		{"v1.5.1-3-gabc1234", true, true, ""},
		{"v1.5.1-dirty", true, true, ""},
		{"v1.5.1-rc1-2-gabc1234-dirty", true, true, "rc1"},
		{"v1.5.1+build5", true, false, ""},
		{"dev", false, false, ""},
		{"", false, false, ""},
		{"v1.5", false, false, ""},
		{"v1.5.x", false, false, ""},
	} {
		v, ok := ParseVersion(tc.in)
		if ok != tc.ok || (ok && (v.After != tc.after || v.Pre != tc.pre)) {
			t.Errorf("ParseVersion(%q) = %+v, %v", tc.in, v, ok)
		}
	}
}

func TestCompare(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"v1.2.0", "v1.10.0", -1}, // numbers, not text
		{"v1.5.1", "v1.5.1", 0},
		{"1.5.1", "v1.5.1", 0},
		{"v2.0.0", "v1.99.99", 1},
		{"v1.5.1-rc1", "v1.5.1", -1},
		{"v1.5.1-rc1", "v1.5.1-rc2", -1},
		{"v1.5.1-rc10", "v1.5.1-rc2", -1}, // rc10 vs rc2 is text order, as in semantic versioning
		{"v1.5.1-1", "v1.5.1-rc1", -1},    // numeric identifiers sit below text ones
		{"v1.5.1-3-gabc1234", "v1.5.1", 0},
		{"v1.5.1-3-gabc1234", "v1.5.2", -1},
	} {
		if got := Compare(tc.a, tc.b); got != tc.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
		if got := Compare(tc.b, tc.a); got != -tc.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", tc.b, tc.a, got, -tc.want)
		}
	}
}

func TestLoadSortsAndRejects(t *testing.T) {
	notes, err := Load(fsOf("v1.10.0", "v1.2.0", "v1.9.0"))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, n := range notes {
		got = append(got, n.Version)
	}
	if strings.Join(got, " ") != "v1.2.0 v1.9.0 v1.10.0" {
		t.Errorf("order %v", got)
	}

	m := fsOf("v1.2.0")
	b, _ := Marshal(note("v1.3.0"))
	m["v1.9.9.json"] = &fstest.MapFile{Data: b} // content says v1.3.0
	if _, err := Load(m); err == nil || !strings.Contains(err.Error(), "file name must be v1.3.0.json") {
		t.Errorf("a file named after another version must be refused, got %v", err)
	}
	m = fsOf("v1.2.0")
	m["v1.3.0.json"] = &fstest.MapFile{Data: []byte(`{"version":"v1.3.0"`)}
	if _, err := Load(m); err == nil {
		t.Error("broken JSON must be refused")
	}
}

func TestBetween(t *testing.T) {
	notes, _ := Load(fsOf("v1.0.0", "v1.1.0", "v1.2.0", "v1.4.0", "v1.5.1"))
	between := func(after, upTo string) string {
		var out []string
		for _, n := range betweenOf(notes, after, upTo) {
			out = append(out, n.Version)
		}
		return strings.Join(out, " ")
	}
	for _, tc := range []struct{ after, upTo, want string }{
		{"v1.1.0", "v1.4.0", "v1.2.0 v1.4.0"},          // after exclusive, upTo inclusive
		{"v1.2.0", "v1.5.1", "v1.4.0 v1.5.1"},          // skipped versions (1.3.0 never had a note)
		{"v1.1.0", "v1.3.0", "v1.2.0"},                 // upTo with no note of its own
		{"v1.4.0", "v1.5.1-3-gabc1234", "v1.5.1"},      // a build after v1.5.1 has seen v1.5.1
		{"v1.5.1-3-gabc1234", "v1.5.1-9-gdef5678", ""}, // both builds after v1.5.1: nothing new
		{"v1.4.0", "v1.5.1-rc1", ""},                   // a pre-release is before its release
		{"", "v1.1.0", "v1.0.0 v1.1.0"},                // no lower bound
		{"v1.5.1", "v1.5.1", ""},                       // nothing between a version and itself
		{"v1.5.1", "v1.1.0", ""},                       // upside down
		{"v1.4.0", "dev", ""},                          // not a version
		{"dev", "v1.5.1", ""},
		{"v1.0.0", "v9.0.0", "v1.1.0 v1.2.0 v1.4.0 v1.5.1"}, // running version newer than every note
	} {
		if got := between(tc.after, tc.upTo); got != tc.want {
			t.Errorf("Between(%q, %q) = %q, want %q", tc.after, tc.upTo, got, tc.want)
		}
	}
}

// betweenOf is Between over a given set (the exported one reads the embedded notes).
func betweenOf(notes []Note, after, upTo string) []Note {
	embedded() // load the real set first so it can be put back
	saved := embeddedNotes
	embeddedNotes = notes
	defer func() { embeddedNotes = saved }()
	return Between(after, upTo)
}

func TestForAndBetweenOnEmbeddedSet(t *testing.T) {
	first := All()[0]
	if n, ok := For(first.Version); !ok || n.Version != first.Version {
		t.Errorf("For(%s) = %v, %v", first.Version, n.Version, ok)
	}
	if _, ok := For(strings.TrimPrefix(first.Version, "v")); !ok {
		t.Error("For must take a version without the v")
	}
	if _, ok := For(first.Version + "-3-gabc1234"); ok {
		t.Error("a source build has no note of its own")
	}
	if _, ok := For("v99.0.0"); ok {
		t.Error("For(v99.0.0) must be false")
	}
	all := All()
	last := all[len(all)-1].Version
	if got := Between("", last); len(got) != len(all) {
		t.Errorf("Between(\"\", %s) = %d notes, want %d", last, len(got), len(all))
	}
	// All() is a copy
	all[0].Version = "changed"
	if All()[0].Version == "changed" {
		t.Error("All() must return a copy")
	}
}

func TestValidate(t *testing.T) {
	mut := func(f func(*Note)) Note { n := note("v1.0.0"); f(&n); return n }
	long := strings.Repeat("가", maxItemRunes+1)
	for name, tc := range map[string]struct {
		n    Note
		want string // "" = valid
	}{
		"valid":              {note("v1.0.0"), ""},
		"prerelease":         {note("v1.0.0-rc1"), ""},
		"bad version":        {note("1.0.0"), "not vMAJOR"},
		"bad date":           {mut(func(n *Note) { n.Date = "2026-13-40" }), "date"},
		"no ko":              {mut(func(n *Note) { delete(n.User, "ko") }), `no "ko"`},
		"no en operator":     {mut(func(n *Note) { delete(n.Operator, "en") }), `no "en"`},
		"unknown language":   {mut(func(n *Note) { n.User["fr"] = []string{"x"} }), `"fr" is not a console language`},
		"fallback language":  {mut(func(n *Note) { n.User["ja"] = []string{"ユーザー向け"} }), ""},
		"count differs":      {mut(func(n *Note) { n.User["ko"] = []string{"a", "b"} }), "same items, same order"},
		"too many":           {mut(func(n *Note) { n.User["en"] = make([]string, 13); n.User["ko"] = make([]string, 13) }), "at most"},
		"all empty":          {mut(func(n *Note) { n.User["en"], n.User["ko"], n.Operator["en"], n.Operator["ko"] = nil, nil, nil, nil }), "no items at all"},
		"empty item":         {mut(func(n *Note) { n.User["en"] = []string{" "}; n.User["ko"] = []string{"가"} }), "empty item"},
		"too long":           {mut(func(n *Note) { n.User["ko"] = []string{long} }), "longer than"},
		"line break":         {mut(func(n *Note) { n.User["en"] = []string{"a\nb"} }), "line break"},
		"list marker":        {mut(func(n *Note) { n.User["en"] = []string{"- a"} }), "list or heading marker"},
		"bold marker":        {mut(func(n *Note) { n.User["en"] = []string{"**Bold** start"} }), "list or heading marker"},
		"numbered":           {mut(func(n *Note) { n.User["en"] = []string{"1. first"} }), "list or heading marker"},
		"draft line":         {mut(func(n *Note) { n.Operator["en"] = []string{"TODO: fix typo"} }), "unedited draft"},
		"url":                {mut(func(n *Note) { n.User["en"] = []string{"See https://example.com/x for more"} }), "only " + RepoURL},
		"www":                {mut(func(n *Note) { n.User["en"] = []string{"See www.example.com"} }), "only " + RepoURL},
		"repo url":           {mut(func(n *Note) { n.User["en"] = []string{"See " + RepoURL + "/wiki/Installation."} }), ""},
		"repo url lookalike": {mut(func(n *Note) { n.User["en"] = []string{"See " + RepoURL + "-evil/x"} }), "only " + RepoURL},
		"other github":       {mut(func(n *Note) { n.User["en"] = []string{"See https://github.com/other/repo"} }), "only " + RepoURL},
		"mention":            {mut(func(n *Note) { n.User["en"] = []string{"Thanks <@123>"} }), "mention"},
		"everyone":           {mut(func(n *Note) { n.User["en"] = []string{"ping @everyone"} }), "mention"},
		"id":                 {mut(func(n *Note) { n.User["en"] = []string{"Room 123456789012345678 fixed"} }), "ID-sized"},
		"small number":       {mut(func(n *Note) { n.User["en"] = []string{"Rooms up to 23919 rows"} }), ""},
	} {
		err := tc.n.Validate()
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: unexpected error %v", name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: error %v, want it to contain %q", name, err, tc.want)
		}
	}
}

func TestParseStrict(t *testing.T) {
	good, _ := Marshal(note("v1.0.0"))
	if _, err := Parse(good); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(good, &m)
	m["extra"] = 1
	withExtra, _ := json.Marshal(m)
	if _, err := Parse(withExtra); err == nil {
		t.Error("an unknown field must be refused")
	}
	if _, err := Parse(append(good, []byte(`{}`)...)); err == nil {
		t.Error("data after the object must be refused")
	}
}

func TestMarshalRoundTrip(t *testing.T) {
	n := note("v1.0.0")
	n.User["en"] = []string{"Use a <b> & c", "second"}
	n.User["ko"] = []string{"하나 <b> & c", "둘"}
	n.Operator = map[string][]string{"ko": nil, "en": nil}
	b, err := Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "<b> & c") || strings.Contains(string(b), string(rune(92))+"u00") {
		t.Errorf("Marshal must not HTML-escape:\n%s", b)
	}
	if !strings.Contains(string(b), `"en": []`) || strings.Contains(string(b), "null") {
		t.Errorf("empty lists are written as []:\n%s", b)
	}
	back, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(back.User) != fmt.Sprint(n.User) {
		t.Errorf("round trip changed the note: %v vs %v", back.User, n.User)
	}
	b2, _ := Marshal(back)
	if string(b) != string(b2) {
		t.Error("Marshal is not stable")
	}
}

func TestLang(t *testing.T) {
	for in, want := range map[string]string{
		"ko": "ko", "ko-KR": "ko", "KO": "ko", "en-US": "en", "es-419": "es", "ja": "ja",
		"zh": "zh-Hans", "zh-CN": "zh-Hans", "zh_CN": "zh-Hans", "zh-Hans": "zh-Hans", "zh-Hans-CN": "zh-Hans",
		"zh-TW": "en", "zh-Hant": "en", "fr": "en", "": "en",
	} {
		if got := Lang(in); got != want {
			t.Errorf("Lang(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRender(t *testing.T) {
	n := note("v1.5.1")
	n.User["ja"] = []string{"ユーザー向けの変更"}
	n.Operator["ja"] = []string{"運用者向けの変更"}

	if got := Render(n, AudienceUser, "en"); got != "## v1.5.1 (2026-11-22)\n\n### What's new\n\n- A user change\n" {
		t.Errorf("user en:\n%s", got)
	}
	if got := Render(n, AudienceOperator, "ko"); got != "## v1.5.1 (2026-11-22)\n\n### 운영자용\n\n- 운영자 변경\n" {
		t.Errorf("operator ko:\n%s", got)
	}
	all := Render(n, AudienceAll, "ja")
	if !strings.Contains(all, "### 新機能と修正\n\n- ユーザー向けの変更\n") || !strings.Contains(all, "### 運用者向け\n\n- 運用者向けの変更\n") {
		t.Errorf("all ja:\n%s", all)
	}
	if strings.Index(all, "新機能") > strings.Index(all, "運用者") {
		t.Error("user items come first")
	}
	// a language the note lacks falls back to en, and an unknown tag too
	if got := Render(n, AudienceUser, "es"); !strings.Contains(got, "- A user change") || !strings.Contains(got, "Novedades") {
		t.Errorf("es falls back to the en items under the es heading:\n%s", got)
	}
	if got := Render(n, AudienceUser, "fr"); !strings.Contains(got, "What's new") {
		t.Errorf("fr:\n%s", got)
	}
	// nothing to show for the audience
	n.User = map[string][]string{"ko": nil, "en": nil}
	if got := Render(n, AudienceUser, "en"); got != "" {
		t.Errorf("an audience without items renders nothing, got %q", got)
	}
	if got := Card(n, AudienceUser, "en"); got != "" {
		t.Errorf("card %q", got)
	}
	if n.Has(AudienceUser) || !n.Has(AudienceOperator) || !n.Has(AudienceAll) {
		t.Error("Has")
	}
}

func TestCard(t *testing.T) {
	n := note("v1.5.1")
	if got := Card(n, AudienceUser, "ko"); got != "**v1.5.1** · 2026-11-22\n• 사용자 변경" {
		t.Errorf("card: %q", got)
	}
	both := Card(n, AudienceAll, "en")
	if both != "**v1.5.1** · 2026-11-22\n• A user change\n**For operators**\n• An operator change" {
		t.Errorf("card all: %q", both)
	}

	// a long list is cut inside the limit and the cut is counted
	var items []string
	for i := 0; i < 12; i++ {
		items = append(items, fmt.Sprintf("%02d %s", i, strings.Repeat("x", 190)))
	}
	n.User = map[string][]string{"ko": items, "en": items}
	card := Card(n, AudienceUser, "en")
	if r := len([]rune(card)); r > 1600 {
		t.Errorf("card has %d characters", r)
	}
	if !strings.Contains(card, "• 00 ") || !strings.HasSuffix(card, "\n… (+5)") {
		t.Errorf("card cut:\n%s", card)
	}
}

func TestRenderRelease(t *testing.T) {
	n := note("v1.5.1")
	want := "## What's new\n\n- A user change\n\n## For operators\n\n- An operator change\n\n" +
		"## 새 기능과 수정\n\n- 사용자 변경\n\n## 운영자용\n\n- 운영자 변경\n" +
		"\n**Full Changelog**: " + RepoURL + "/compare/v1.4.0...v1.5.1\n"
	if got := RenderRelease(n, "v1.4.0"); got != want {
		t.Errorf("RenderRelease:\n%s\nwant:\n%s", got, want)
	}
	if got := RenderRelease(n, ""); strings.Contains(got, "Full Changelog") {
		t.Error("no link without a previous tag")
	}
	n.User = map[string][]string{"ko": nil, "en": nil}
	if got := RenderRelease(n, ""); strings.Contains(got, "What's new") || strings.Contains(got, "새 기능") {
		t.Errorf("empty sections are left out:\n%s", got)
	}
}

// The notes of the repository render and validate under the same rules the tool uses.
func TestRepositoryNotesHaveNoStrayFiles(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join("..", "..", "release-notes"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		ok := name == "embed.go" || strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".md")
		if !ok {
			t.Errorf("release-notes/%s: only vX.Y.Z.json, vX.Y.Z.md and embed.go belong here", name)
		}
	}
}
