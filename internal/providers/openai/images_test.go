package openai

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func TestParseImageTakesTheLastImage(t *testing.T) {
	a := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("first"))
	b := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("final"))
	body, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{
		"content": "여기 있어요", "images": []any{
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": a}},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": b}},
		}}}}, "usage": map[string]any{"cost": 0.04}})
	out, err := ParseImage("application/json", body)
	if err != nil || string(out.Data) != "final" || out.MIME != "image/png" || out.Text != "여기 있어요" || out.CostUSD == nil || *out.CostUSD != 0.04 {
		t.Fatalf("%+v %v", out, err)
	}
	// content parts form
	body, _ = json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{
		"content": []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": a}}}}}}})
	if out, err := ParseImage("application/json", body); err != nil || string(out.Data) != "first" {
		t.Fatalf("parts: %+v %v", out, err)
	}
	// a remote URL or an HTML "image" is not accepted
	body, _ = json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{
		"content": "", "images": []any{map[string]any{"image_url": map[string]any{"url": "https://example.com/x.png"}},
			map[string]any{"image_url": map[string]any{"url": "data:text/html;base64,PGh0bWw+"}}}}}}})
	if _, err := ParseImage("application/json", body); err == nil {
		t.Fatal("no usable image must be an error")
	}
}
