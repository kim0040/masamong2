package application

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"masamong/internal/chat"
	"masamong/internal/providers"
	"masamong/internal/providers/openai"
)

// testModelAdapter is an adapter for a model whose host answers with rt, in process.
func testModelAdapter(t *testing.T, rt registryGateTransport) *openai.Adapter {
	t.Helper()
	t.Setenv("MASAMONG_A3_LOOP_TEST_KEY", "synthetic-test-credential")
	cred, err := providers.Resolve("env:MASAMONG_A3_LOOP_TEST_KEY")
	if err != nil {
		t.Fatal(err)
	}
	c := providers.NewClient(5*time.Second, nil)
	c.InProcess, c.MaxRetries = true, 0
	c.HTTP.Transport = rt
	return openai.New(c, "https://loop-deadline.test", cred)
}

// A tool loop round whose model hangs past its own limit is asked of the model's fallback: the round's
// deadline is the whole chain's (each model's limit), not the first model's alone.
func TestToolLoopRoundReachesTheFallbackModel(t *testing.T) {
	s, _ := deliveryService(t)
	const limit = 200 * time.Millisecond
	hang := registryGateTransport(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	answer := registryGateTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(
			`{"choices":[{"message":{"role":"assistant","content":"from the fallback"},"finish_reason":"stop"}]}`))}, nil
	})
	a := testModelAdapter(t, hang)
	a.Failover = &openai.Failover{Provider: "p0", Model: "m0", Timeout: limit,
		Fallbacks: []openai.Fallback{{A: testModelAdapter(t, answer), Provider: "p1", Model: "m1", Timeout: limit}}}
	model := chatLLM{s: s, routes: map[string]chatRoute{"planner": {model: "m0", provider: "p0", a: a, timeout: limit}}}
	out, err := model.ChatTools(context.Background(), "planner", []chat.LoopMsg{{Role: "user", Content: "q"}}, nil, 10)
	if err != nil || out.Text != "from the fallback" {
		t.Fatalf("reply %+v err %v", out, err)
	}
}
