package chat

import (
	"context"
	"testing"

	"masamong/internal/lang"
)

// The turn's pictures go with the answer's last message only; a turn without pictures sends none.
func TestPicturesGoWithTheAnswersLastMessage(t *testing.T) {
	p, _ := lang.Get("ko")
	llm := &truncatedLLM{calls: []Completion{{Text: "고양이네.", FinishReason: "stop"}, {Text: "안녕.", FinishReason: "stop"}}}
	rt := &Runtime{LLM: llm}
	img := Image{MIME: "image/png", Data: []byte{1}}
	if _, err := rt.Answer(context.Background(), Request{Message: "이거 뭐야? [사진]", Voice: Voice{Pack: p}, PlannerModel: "planner", ResponseModel: "response", Images: []Image{img}}); err != nil {
		t.Fatal(err)
	}
	msgs := llm.msgs[0]
	for i, m := range msgs {
		if want := i == len(msgs)-1; (len(m.Images) > 0) != want {
			t.Fatalf("message %d (%s) images=%d", i, m.Role, len(m.Images))
		}
	}
	if _, err := rt.Answer(context.Background(), Request{Message: "안녕", Voice: Voice{Pack: p}, PlannerModel: "planner", ResponseModel: "response"}); err != nil {
		t.Fatal(err)
	}
	for _, m := range llm.msgs[1] {
		if len(m.Images) > 0 {
			t.Fatal("a turn without pictures sent one")
		}
	}
}
