package lang

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func v160(t *testing.T, tag, key string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "v160", tag+"."+key+".txt"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(string(b), "\n")
}

// The v1.6.0 room-memory prompts stay as they were: the new person section of episode_extract only
// adds lines between its fences, every old sentence of every language is still there byte for byte,
// and removing the section gives the old text exactly. The room-state prompts did not change at all.
func TestEpisodeExtractKeepsTheV160Text(t *testing.T) {
	for _, tag := range Available() {
		p, _ := Get(tag)
		cur := p.Memory.EpisodeExtract
		old := v160(t, tag, "episode_extract")
		begin, end := strings.Index(cur, "\n{people_begin}\n"), strings.Index(cur, "\n{people_end}")
		if begin < 0 || end < begin || strings.Count(cur, "{people_begin}") != 1 || strings.Count(cur, "{people_end}") != 1 {
			t.Fatalf("%s: person section fences", tag)
		}
		if stripped := cur[:begin] + cur[end+len("\n{people_end}"):]; stripped != old {
			t.Errorf("%s: the old episode_extract text changed", tag)
		}
		have := map[string]bool{}
		for _, l := range strings.Split(cur, "\n") {
			have[l] = true
		}
		for _, l := range strings.Split(old, "\n") {
			if !have[l] {
				t.Errorf("%s: old line is gone: %q", tag, l)
			}
		}
		// The section comes before the output format, so the last line is still the episodes example.
		last := cur[strings.LastIndex(cur, "\n")+1:]
		if last != old[strings.LastIndex(old, "\n")+1:] || !strings.HasPrefix(last, `{"episodes":`) {
			t.Errorf("%s: last line is no longer the episodes example", tag)
		}
		section := cur[begin : end+1]
		if strings.Count(section, "\n") != 5 {
			t.Errorf("%s: person section is not three rules: %q", tag, section)
		}
		// the read side matches these exact keys for a status claim (single-valued topics)
		for _, key := range statusTopics {
			if !strings.Contains(section, key) || !strings.Contains(p.Memory.PersonExtract, key) {
				t.Errorf("%s: the status topic %q is not named in both person prompts", tag, key)
			}
		}
		for _, k := range []string{"episode_extract_system", "channel_state_system", "channel_state"} {
			var got string
			switch k {
			case "episode_extract_system":
				got = p.Memory.EpisodeExtractSystem
			case "channel_state_system":
				got = p.Memory.ChannelStateSystem
			case "channel_state":
				got = p.Memory.ChannelState
			}
			if got != v160(t, tag, k) {
				t.Errorf("%s: %s changed", tag, k)
			}
		}
		// The example of the people field is JSON, with the keys and kinds the parser reads.
		example := section[strings.Index(section, `"people":[{`):]
		example = strings.TrimSpace(example[:strings.Index(example, "\n")])
		checkPeopleExample(t, tag+" episode_extract", "{"+example+"}")
	}
}

// statusTopics are the topics of a status claim that hold one value at a time (application singleTopics).
var statusTopics = []string{"residence", "job", "school", "birthday", "relationship_status"}

func checkPeopleExample(t *testing.T, name, obj string) {
	t.Helper()
	var doc struct {
		People []map[string]json.RawMessage `json:"people"`
	}
	if err := json.Unmarshal([]byte(obj), &doc); err != nil || len(doc.People) != 1 {
		t.Errorf("%s: example is not a people list: %v %q", name, err, obj)
		return
	}
	for _, k := range []string{"about", "kind", "topic", "text", "by", "ev"} {
		if _, ok := doc.People[0][k]; !ok {
			t.Errorf("%s: example lacks %q", name, k)
		}
	}
}

func TestPersonExtractPromptsAreInEveryPack(t *testing.T) {
	for _, tag := range Available() {
		p, _ := Get(tag)
		m := p.Memory
		if !strings.Contains(m.PersonExtractSystem, "{bot}") {
			t.Errorf("%s: person_extract_system has no {bot}", tag)
		}
		for _, ph := range []string{"{now}", "{people}", "{lines}"} {
			if strings.Count(m.PersonExtract, ph) != 1 {
				t.Errorf("%s: person_extract needs %s once", tag, ph)
			}
		}
		if strings.Contains(m.PersonExtract, "{plans}") || strings.Contains(m.PersonExtract, "{people_begin}") {
			t.Errorf("%s: person_extract has episode-only placeholders", tag)
		}
		lines := strings.Split(m.PersonExtract, "\n")
		checkPeopleExample(t, tag+" person_extract", lines[len(lines)-1])
		for _, kind := range []string{"nickname", "like", "dislike", "trait", "status", "appearance", "relation", "other"} {
			if !strings.Contains(m.PersonExtract, kind) {
				t.Errorf("%s: person_extract does not list kind %s", tag, kind)
			}
		}
	}
}
