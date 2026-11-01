package chat

import (
	"context"
	"errors"
	"strings"
	"testing"

	"masamong/internal/lang"
)

type truncatedLLM struct {
	calls  []Completion
	errs   []error
	limits []int
	msgs   [][]Msg
}

func (f *truncatedLLM) Chat(_ context.Context, model string, _ []Msg, _ int, _ float64) (string, float64, error) {
	if model != "planner" {
		return "", 0, nil
	}
	return `{"memory_requirement":"none","tool_plan":[]}`, 0, nil
}

func (f *truncatedLLM) ChatDetailed(_ context.Context, _ string, msgs []Msg, maxTokens int, _ float64) (Completion, error) {
	f.limits = append(f.limits, maxTokens)
	f.msgs = append(f.msgs, append([]Msg(nil), msgs...))
	out := f.calls[0]
	f.calls = f.calls[1:]
	var err error
	if len(f.errs) > 0 {
		err, f.errs = f.errs[0], f.errs[1:]
	}
	return out, err
}

func TestAnswerContinuesOnceWhenProviderStopsAtTokenLimit(t *testing.T) {
	p, _ := lang.Get("ko")
	llm := &truncatedLLM{calls: []Completion{
		{Text: "첫 문장. 다음 문장이 중간에 끊겼", FinishReason: "length", CostUSD: .01},
		{Text: "던 부분을 이어서 완성합니다.", FinishReason: "stop", CostUSD: .02},
	}}
	rt := &Runtime{LLM: llm}
	out, err := rt.Answer(context.Background(), Request{Message: "역사를 자세히 설명해줘", Voice: Voice{Pack: p}, PlannerModel: "planner", ResponseModel: "response"})
	if err != nil {
		t.Fatal(err)
	}
	if len(llm.limits) != 2 || llm.limits[0] < 500 || llm.limits[1] != llm.limits[0] {
		t.Fatalf("response token limits = %v", llm.limits)
	}
	if !strings.Contains(out.Answer, "첫 문장.") || !strings.Contains(out.Answer, "이어") || strings.Contains(out.Answer, p.Chat.IncompleteAnswer) {
		t.Fatalf("continued answer = %q", out.Answer)
	}
	if out.CostUSD != .03 {
		t.Fatalf("both calls must count toward cost: %v", out.CostUSD)
	}
	last := llm.msgs[1]
	if len(last) < 2 || last[len(last)-1].Content != p.Chat.ContinueAnswer {
		t.Fatalf("continuation prompt missing: %+v", last)
	}
}

func TestFinalLengthTruncationDoesNotEndMidSentenceSilently(t *testing.T) {
	p, _ := lang.Get("en")
	llm := &truncatedLLM{calls: []Completion{
		{Text: "A complete sentence. This clause stops", FinishReason: "length"},
		{Text: " before it is done", FinishReason: "length"},
	}}
	rt := &Runtime{LLM: llm}
	out, err := rt.Answer(context.Background(), Request{Message: "long essay in detail", Voice: Voice{Pack: p}, PlannerModel: "planner", ResponseModel: "response"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(out.Answer, p.Chat.IncompleteAnswer) || strings.Contains(out.Answer, "stops before") {
		t.Fatalf("incomplete answer needs a visible marker and sentence boundary: %q", out.Answer)
	}
}

func TestFailedContinuationKeepsCompleteSentencesAndMarksIncomplete(t *testing.T) {
	p, _ := lang.Get("en")
	llm := &truncatedLLM{
		calls: []Completion{{Text: "A complete sentence. This clause stops", FinishReason: "length"}, {}},
		errs:  []error{nil, errors.New("continuation unavailable")},
	}
	rt := &Runtime{LLM: llm}
	out, err := rt.Answer(context.Background(), Request{Message: "Explain this", Voice: Voice{Pack: p}, PlannerModel: "planner", ResponseModel: "response"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Answer != "A complete sentence. "+p.Chat.IncompleteAnswer {
		t.Fatalf("failed continuation must use the last sentence boundary: %q", out.Answer)
	}
}

func TestTruncatedAnswerWithoutSentenceBoundaryReturnsOnlyMarker(t *testing.T) {
	p, _ := lang.Get("en")
	llm := &truncatedLLM{
		calls: []Completion{{Text: "A cut off fragment", FinishReason: "length"}, {}},
		errs:  []error{nil, errors.New("continuation unavailable")},
	}
	rt := &Runtime{LLM: llm}
	out, err := rt.Answer(context.Background(), Request{Message: "Explain this", Voice: Voice{Pack: p}, PlannerModel: "planner", ResponseModel: "response"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Answer != p.Chat.IncompleteAnswer {
		t.Fatalf("must not return a fragment without a sentence boundary: %q", out.Answer)
	}
}
