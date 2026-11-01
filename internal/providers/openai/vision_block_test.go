package openai

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestBlockedPictureIsNotAVisionRefusal(t *testing.T) {
	for _, body := range []string{
		`{"error":{"message":"Your input image may contain content that is not allowed by our safety system.","type":"invalid_request_error","code":"content_policy_violation"}}`,
		`{"error":{"message":"Image input is not allowed: flagged by moderation"}}`,
		`{"error":{"message":"Image input not supported right now: rate limit reached for vision requests"}}`,
		`{"error":{"message":"The image_url content violates our usage policy and is not allowed"}}`,
	} {
		srv := &rawServer{replies: []reply{{400, body}}}
		a := testAdapter(t, APICompatible, srv.handler)
		_, err := a.Chat(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "x", Images: []ImagePart{{MIME: "image/png", Data: []byte{1}}}}}})
		if err == nil || errors.Is(err, ErrVisionUnsupported) || a.VisionUnsupported("m") {
			t.Fatalf("a blocked picture must not mark the model blind: %s → %v", body, err)
		}
	}
}

func TestVisionRefusalIsForgottenAfterItsTTL(t *testing.T) {
	at := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	now = func() time.Time { return at }
	t.Cleanup(func() { now = time.Now })
	srv := &rawServer{replies: []reply{{400, `{"error":{"message":"No endpoints found that support image input","code":404}}`}, {200, okReply}}}
	a := testAdapter(t, APICompatible, srv.handler)
	req := ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "x", Images: []ImagePart{{MIME: "image/png", Data: []byte{1}}}}}}
	if _, err := a.Chat(context.Background(), req); !errors.Is(err, ErrVisionUnsupported) {
		t.Fatalf("refusal: %v", err)
	}
	at = at.Add(VisionRefusalTTL - time.Minute)
	if !a.VisionUnsupported("m") {
		t.Fatal("forgotten before the TTL")
	}
	at = at.Add(2 * time.Minute)
	if a.VisionUnsupported("m") {
		t.Fatal("still remembered after the TTL")
	}
	if _, err := a.Chat(context.Background(), req); err != nil || len(srv.bodies) != 2 {
		t.Fatalf("the model is not asked again: %v, %d requests", err, len(srv.bodies))
	}
}
