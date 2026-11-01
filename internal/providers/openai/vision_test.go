package openai

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"masamong/internal/errclass"
)

const okReply = `{"id":"x","model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"a cat"}}],"usage":{"prompt_tokens":30,"completion_tokens":3}}`

func TestImagePartsInTheUserMessage(t *testing.T) {
	for _, api := range []API{APICompatible, APIOpenRouter, APIGemini} {
		srv := &rawServer{replies: []reply{{200, okReply}}}
		a := testAdapter(t, api, srv.handler)
		_, err := a.Chat(context.Background(), ChatRequest{Model: "m", Messages: []Message{
			{Role: "system", Content: "be brief"},
			{Role: "user", Content: "what is this?", Images: []ImagePart{{MIME: "image/png", Data: []byte{1, 2, 3}}, {MIME: "image/jpeg", Data: []byte{4}}}},
		}})
		if err != nil {
			t.Fatalf("%s: %v", api, err)
		}
		msgs := srv.json(t, 0)["messages"].([]any)
		var want any
		_ = json.Unmarshal([]byte(`[{"role":"system","content":"be brief"},{"role":"user","content":[`+
			`{"type":"text","text":"what is this?"},`+
			`{"type":"image_url","image_url":{"url":"data:image/png;base64,AQID"}},`+
			`{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,BA=="}}]}]`), &want)
		if !reflect.DeepEqual(any(msgs), want) {
			t.Fatalf("%s: messages %v", api, msgs)
		}
	}
}

func TestMessageWithoutImagesIsUnchanged(t *testing.T) {
	plain, err := json.Marshal(Message{Role: "user", Content: "hi"})
	if err != nil || string(plain) != `{"role":"user","content":"hi"}` {
		t.Fatalf("plain message: %s %v", plain, err)
	}
	empty, _ := json.Marshal(Message{Role: "user", Content: "hi", Images: []ImagePart{}})
	if string(empty) != string(plain) {
		t.Fatalf("an empty image list changed the message: %s", empty)
	}
}

func TestVisionRefusalIsRememberedPerModel(t *testing.T) {
	for _, refusal := range []string{
		`{"error":{"message":"Invalid content type. image_url is only supported by certain models.","type":"invalid_request_error"}}`,
		`{"error":{"message":"No endpoints found that support image input","code":404}}`,
		`{"error":{"message":"Failed to deserialize the JSON body into the target type: messages[1]: unknown variant ` + "`image_url`" + `, expected ` + "`text`" + `"}}`,
		`{"error":{"message":"Invalid type for 'messages[1].content': expected a string, but got an array instead."}}`,
	} {
		srv := &rawServer{replies: []reply{{400, refusal}}}
		a := testAdapter(t, APICompatible, srv.handler)
		req := ChatRequest{Model: "text-only", Messages: []Message{{Role: "user", Content: "x", Images: []ImagePart{{MIME: "image/png", Data: []byte{1}}}}}}
		_, err := a.Chat(context.Background(), req)
		if !errors.Is(err, ErrVisionUnsupported) || errclass.Of(err) != errclass.Unsupported {
			t.Fatalf("refusal %s: got %v (%s)", refusal, err, errclass.Of(err))
		}
		if len(srv.bodies) != 1 {
			t.Fatalf("a picture refusal must not be retried without optional fields: %d requests", len(srv.bodies))
		}
		if !a.VisionUnsupported("text-only") {
			t.Fatal("refusal not remembered")
		}
		// the next picture request for that model fails without a request; text still goes
		if _, err := a.Chat(context.Background(), req); !errors.Is(err, ErrVisionUnsupported) || len(srv.bodies) != 1 {
			t.Fatalf("remembered refusal: %v, %d requests", err, len(srv.bodies))
		}
	}
}

func TestImageComplaintIsNotAVisionRefusal(t *testing.T) {
	srv := &rawServer{replies: []reply{{400, `{"error":{"message":"Unsupported image format; image too large"}}`}}}
	a := testAdapter(t, APICompatible, srv.handler)
	_, err := a.Chat(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "x", Images: []ImagePart{{MIME: "image/png", Data: []byte{1}}}}}})
	if err == nil || errors.Is(err, ErrVisionUnsupported) || a.VisionUnsupported("m") {
		t.Fatalf("a complaint about one picture must not mark the model blind: %v", err)
	}
}
