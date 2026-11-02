package openai

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"masamong/internal/providers"
)

// Live smoke test of one host's batch API. It is skipped unless MASAMONG_LIVE_BATCH_HOST names a host
// (openrouter | anthropic | gemini | openai | nanogpt), and it spends a few tokens at half price:
//
//	MASAMONG_LIVE_BATCH_HOST      the host id from hosts.json
//	MASAMONG_LIVE_BATCH_KEY_FILE  a file holding the key (the key is never printed)
//	MASAMONG_LIVE_BATCH_MODEL     a cheap chat model of the host
//	MASAMONG_LIVE_BATCH_EMBED_MODEL  optional: an embedding model, to batch one embeddings request too
//	MASAMONG_LIVE_BATCH_WAIT      how long to wait for each batch (default 20m)
//
// It prints states, counts, finish reasons and costs only, and deletes what it left on the host.
func TestLiveBatch(t *testing.T) {
	id := os.Getenv("MASAMONG_LIVE_BATCH_HOST")
	if id == "" {
		t.Skip("MASAMONG_LIVE_BATCH_HOST not set")
	}
	h, ok := HostByID(id)
	if !ok {
		t.Fatalf("unknown host %q", id)
	}
	keyFile, model := os.Getenv("MASAMONG_LIVE_BATCH_KEY_FILE"), os.Getenv("MASAMONG_LIVE_BATCH_MODEL")
	if keyFile == "" || model == "" {
		t.Fatal("MASAMONG_LIVE_BATCH_KEY_FILE and MASAMONG_LIVE_BATCH_MODEL are required")
	}
	cred, err := providers.Resolve("file:" + keyFile)
	if err != nil || !cred.Present() {
		t.Fatalf("key file unreadable or empty (%v)", err)
	}
	wait := 20 * time.Minute
	if v := os.Getenv("MASAMONG_LIVE_BATCH_WAIT"); v != "" {
		if wait, err = time.ParseDuration(v); err != nil {
			t.Fatalf("MASAMONG_LIVE_BATCH_WAIT: %v", err)
		}
	}
	a := New(providers.NewClient(5*time.Minute, nil), h.BaseURL, cred)
	a.API = h.API
	t.Logf("host %s, style %q, chat=%v embed=%v, max items %d/%d", id, a.liveStyle(), a.BatchSupport(BatchChat), a.BatchSupport(BatchEmbed),
		a.BatchMaxItems(BatchChat), a.BatchMaxItems(BatchEmbed))

	chat := func(id string) BatchItem {
		return BatchItem{ID: id, Chat: &ChatRequest{Model: model, MaxTokens: 16, Messages: []Message{{Role: "user", Content: "Reply with the single word OK."}}}}
	}
	if !a.BatchSupport(BatchChat) {
		t.Fatalf("host %s reports no chat batch support", id)
	}
	liveRun(t, a, BatchChat, model, []BatchItem{chat("live-1"), chat("live-2")}, wait)
	if em := os.Getenv("MASAMONG_LIVE_BATCH_EMBED_MODEL"); em != "" {
		if !a.BatchSupport(BatchEmbed) {
			t.Logf("host %s has no embeddings batch; skipped", id)
			return
		}
		liveRun(t, a, BatchEmbed, em, []BatchItem{{ID: "live-e1", Input: []string{"hello batch"}}}, wait)
	}
}

func (a *Adapter) liveStyle() string {
	if s, ok := a.batchSpec(); ok {
		return s.Style
	}
	return ""
}

func liveRun(t *testing.T, a *Adapter, kind BatchKind, model string, items []BatchItem, wait time.Duration) {
	t.Helper()
	ctx := context.Background()
	ref, err := a.SubmitBatch(ctx, kind, model, items, "")
	if err != nil {
		t.Fatalf("%s submit: %v", kind, err)
	}
	t.Logf("%s batch submitted: %d items", kind, ref.Count)
	defer func() {
		if err := a.CleanupBatch(context.Background(), ref); err != nil {
			t.Logf("%s cleanup: %v", kind, err)
		} else {
			t.Logf("%s cleanup done", kind)
		}
	}()
	deadline := time.Now().Add(wait)
	var last BatchStatus
	for {
		st, err := a.PollBatch(ctx, ref)
		if err != nil {
			t.Fatalf("%s poll: %v", kind, err)
		}
		if st != last {
			t.Logf("%s state=%s total=%d completed=%d failed=%d", kind, st.State, st.Total, st.Completed, st.Failed)
			last = st
		}
		if st.State.Final() {
			if st.Error != "" {
				t.Logf("%s batch error: %s", kind, st.Error)
			}
			break
		}
		if time.Now().After(deadline) {
			_ = a.CancelBatch(context.Background(), ref)
			t.Fatalf("%s not finished after %s (state %s); cancel requested", kind, wait, st.State)
		}
		time.Sleep(20 * time.Second)
	}
	outs, err := a.BatchResults(ctx, ref)
	if err != nil {
		t.Fatalf("%s results: %v", kind, err)
	}
	if len(outs) != len(items) {
		t.Errorf("%s: %d outcomes for %d items", kind, len(outs), len(items))
	}
	for _, o := range outs {
		switch {
		case o.Err != nil:
			t.Errorf("%s item %s failed: %v", kind, o.ID, o.Err)
		case o.Chat != nil:
			t.Logf("%s item %s: finish=%q chars=%d tokens=%d+%d cost=%s", kind, o.ID, o.Chat.FinishReason, len(o.Chat.Content),
				o.Chat.PromptTokens, o.Chat.CompletionTokens, liveCost(o.Chat.CostUSD, o.Chat.CostEstimated))
		case o.Embed != nil:
			dim := 0
			if len(o.Embed.Vectors) > 0 {
				dim = len(o.Embed.Vectors[0])
			}
			t.Logf("%s item %s: vectors=%d dim=%d tokens=%d cost=%s", kind, o.ID, len(o.Embed.Vectors), dim, o.Embed.PromptTokens,
				liveCost(o.Embed.CostUSD, o.Embed.CostEstimated))
		default:
			t.Errorf("%s item %s: empty outcome", kind, o.ID)
		}
	}
}

func liveCost(c *float64, estimated bool) string {
	switch {
	case c == nil:
		return "unknown"
	case estimated:
		return "~" + formatUSD(*c)
	}
	return formatUSD(*c)
}

func formatUSD(v float64) string { return "$" + strconv.FormatFloat(v, 'g', 6, 64) }
