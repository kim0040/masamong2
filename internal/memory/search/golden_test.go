package search

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"masamong/internal/lang"
	"masamong/internal/memory/corpus"
	"masamong/internal/memory/leaves"
	"masamong/internal/memory/lexical"
)

var updateGolden = flag.Bool("update", false, "rewrite testdata goldens")

// goldenKorean pins the Korean-specific behaviour (evidence markers, particle/pronoun handling,
// lexical words) so moving it into language packs keeps the sandbox-evaluated output byte-identical.
func goldenKorean(t *testing.T) map[string]any {
	var msgs []corpus.Message
	for i := 0; i < 14; i++ {
		at := time.Date(2026, 9, 1, 10, i, 0, 0, time.UTC).Format(time.RFC3339)
		body := fmt.Sprintf("메시지 %d ", i) + strings.Repeat("가평 캠핑 얘기 ", 3+i%4)
		msgs = append(msgs, corpus.Message{ObservationID: fmt.Sprintf("o%02d", i), Room: "r", RoomOriginal: "r",
			ActorLabel: []string{"철수", "영희"}[i%2], Content: body, ParsedTime: at})
	}
	lv := leaves.SlicePerRoom(msgs, leaves.Recipe{Size: 4, Split: leaves.SplitFixed, WithPrefix: true}, 1000, "leaf")
	out := map[string]any{}
	for _, limit := range []int{60, 150, 400, 2000} {
		for _, excl := range []bool{false, true} {
			scope := packScope{}
			if excl {
				scope.timeAllows = func(m corpus.Message) bool {
					return m.ObservationID != "o05" && m.ObservationID != "o02" && m.ObservationID != "o09"
				}
			}
			p := packSegment(msgs, lv[1], 3, 3, true, limit, scope, nil)
			out[fmt.Sprintf("pack_%d_%v", limit, excl)] = map[string]any{"excerpt": p.Excerpt, "status": p.Status, "spans": p.VisibleSpans}
		}
	}
	ko := lang.MergeSearch([]string{"ko"}, []string{"마사몽"})
	rules := newTextRules(&ko)
	out["clean_targets"] = rules.cleanTargets([]string{"철수가", "걔", "영희랑", "마사몽", "그 사람", "민수는요", "Kim", "봇", "가"})
	labels := map[string]bool{"철수": true}
	var tok []string
	for _, c := range [][2]string{{"철수가 왔어", "철수"}, {"김철수가", "철수"}, {"철수네 집", "철수"}, {"Kim said", "Kim"}} {
		tok = append(tok, fmt.Sprint(rules.tokenInText(c[0], c[1], labels)))
	}
	out["token_in_text"] = tok
	q := "철수가 어디 가자고 했지? 초코우유를 누가 샀어 camping 🎉"
	out["query_words"] = lexical.QueryWords(q)
	out["ngrams"] = lexical.ToNgrams(q, 2)
	out["match"] = lexical.MatchExpression(q, 2)
	return out
}

func TestKoreanSearchMatchesGolden(t *testing.T) {
	got, err := json.MarshalIndent(goldenKorean(t), "", " ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("testdata", "golden_ko.json")
	if *updateGolden {
		_ = os.MkdirAll("testdata", 0o755)
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != string(got) {
		var w, g map[string]json.RawMessage
		_ = json.Unmarshal(want, &w)
		_ = json.Unmarshal(got, &g)
		for k := range w {
			if string(w[k]) != string(g[k]) {
				t.Errorf("%s differs:\nwant %s\ngot  %s", k, w[k], g[k])
			}
		}
	}
}
