package chat

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// The answer prompt's cacheable prefix is everything before the new turn's own content: the system
// message and the earlier history messages. Between two turns of the same conversation those bytes
// must not change, or the provider's prompt cache never hits (production, 2026-11: about 1.5% of the
// answer's input tokens were served from cache while the planner and tool loop hit half).
func TestAnswerPrefixIsStableAcrossTurns(t *testing.T) {
	v := koVoice()
	loc := time.FixedZone("KST", 9*3600)
	now := time.Date(2026, 10, 31, 15, 4, 0, 0, loc)
	history := []Turn{
		{Role: "user", Speaker: "철수", Content: "점심 뭐 먹지", At: now.Add(-11 * time.Minute)},
		{Role: "assistant", Content: "나는 김치찌개 먹었어", At: now.Add(-10 * time.Minute)},
		// four minutes old at the first turn, seven at the second: it crosses the age line
		{Role: "user", Speaker: "영희", Content: "나는 국수", At: now.Add(-4 * time.Minute)},
		{Role: "user", Speaker: "철수", Content: "국수 좋다", At: now.Add(-2 * time.Minute)},
	}
	in := AnswerInput{System: "you are masamong", Speaker: "영희", Message: "그럼 국수로 가자", History: history, Now: now}
	first := BuildAnswerMessages(v, in)
	later := now.Add(3 * time.Minute)
	grown := append(append([]Turn(nil), history...),
		// the first turn's own exchange, kept in the stored conversation
		Turn{Role: "user", Speaker: "영희", Content: "그럼 국수로 가자", At: now},
		Turn{Role: "assistant", Content: "거기 진짜 맛있어", At: now.Add(time.Minute)})
	second := BuildAnswerMessages(v, AnswerInput{System: in.System, Speaker: "철수", Message: "거기 예약됐어", History: grown, Now: later})
	if len(second) != len(first)+2 {
		t.Fatalf("the second turn adds the call and its answer to the history: %d vs %d", len(second), len(first))
	}
	// as sent: the wire object of a message is its role and content
	sent := func(m Msg) string {
		raw, _ := json.Marshal(m)
		return string(raw)
	}
	prefix := 0
	for i := 0; i < len(first)-1; i++ {
		if sent(second[i]) != sent(first[i]) {
			t.Fatalf("message %d changed between turns:\n%s\n%s", i, sent(first[i]), sent(second[i]))
		}
		prefix += len(sent(first[i]))
	}
	total := 0
	for _, m := range first {
		total += len(sent(m))
	}
	t.Logf("stable prefix: %d of %d bytes (%.0f%%)", prefix, total, 100*float64(prefix)/float64(total))
}

// The answer's line cap keeps the newest MaxHistory rows: one hundred short rows under a cap of
// eighty show from row twenty.
func TestAnswerLineCapKeepsTheNewestRows(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	rows := make([]Turn, 130)
	for i := range rows {
		rows[i] = Turn{Role: "user", Speaker: "A", Content: fmt.Sprintf("line-%03d", i), At: now.Add(time.Duration(i) * time.Minute)}
	}
	first := func(n, max int) string {
		msgs := BuildAnswerMessages(koVoice(), AnswerInput{System: "S", Message: "next", History: rows[:n], MaxHistory: max, Now: now})
		return msgs[1].Content // the system message, then the first history line shown
	}
	if got := first(100, 80); !strings.Contains(got, "line-020") {
		t.Fatalf("100 rows under a cap of 80: first shown %q, want line-020", got)
	}
	if got := first(26, 24); !strings.Contains(got, "line-002") {
		t.Fatalf("26 rows under a cap of 24: first shown %q, want line-002", got)
	}
}
