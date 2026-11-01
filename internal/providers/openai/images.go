package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"masamong/internal/providers"
	"masamong/internal/providers/decode"
)

// ImageResult is one generated image.
type ImageResult struct {
	Data          []byte
	MIME          string
	Text          string // any words the model said along with the picture
	CostUSD       *float64
	CostEstimated bool
	Elapsed       time.Duration
	// tokens the host reported for a chat image answer, priced when it reports no cost
	inTokens, outTokens int
}

// maxImageBytes bounds one decoded image (a 1K–2K PNG is a few MB).
const maxImageBytes = 12 << 20

// GenerateImage asks an image model for one picture. aspectRatio is "1:1", "16:9", … or "".
//
//	openrouter, compatible   /chat/completions with modalities ["image","text"] (image_config); a
//	                         compatible host's model that refuses it is asked on /images/generations
//	                         (NanoGPT) and remembered
//	openai                   /images/generations (gpt-image: base64 PNG, usage in tokens)
//	gemini                   native models/{model}:generateContent — its OpenAI-compatible
//	                         endpoints serve Imagen only and cannot return a Gemini image
func (a *Adapter) GenerateImage(ctx context.Context, model, prompt, aspectRatio string) (ImageResult, error) {
	if !a.Configured() {
		return ImageResult{}, providers.ErrNoCredential
	}
	if model == "" || strings.TrimSpace(prompt) == "" {
		return ImageResult{}, fmt.Errorf("openai: image model and prompt required")
	}
	if err := a.gate(); err != nil {
		return ImageResult{}, err
	}
	switch a.API {
	case APIOpenAI:
		return a.imageOpenAI(ctx, model, prompt, aspectRatio)
	case APIGemini:
		return a.imageGemini(ctx, model, prompt, aspectRatio)
	}
	if _, ok := a.imageAPI.Load(model); ok {
		return a.imageOpenAI(ctx, model, prompt, aspectRatio)
	}
	out, err := a.imageChat(ctx, model, prompt, aspectRatio)
	if a.API == APICompatible && refusedChatImage(err) {
		if out, err2 := a.imageOpenAI(ctx, model, prompt, aspectRatio); err2 == nil {
			a.imageAPI.Store(model, true)
			return out, nil
		}
	}
	return out, err
}

// refusedChatImage: the host says the model does not take chat requests (an image-only model such as
// qwen-image on NanoGPT), not that the key or the prompt is wrong.
func refusedChatImage(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) || (ae.Status != 400 && ae.Status != 404 && ae.Status != 422) {
		return false
	}
	m := strings.ToLower(ae.Message + " " + ae.Code + " " + ae.Type)
	return strings.Contains(m, "support") || strings.Contains(m, "not found") || strings.Contains(m, "chat")
}

func (a *Adapter) imageChat(ctx context.Context, model, prompt, aspectRatio string) (ImageResult, error) {
	payload := map[string]any{"model": model, "modalities": []string{"image", "text"},
		"messages": []Message{{Role: "user", Content: prompt}}}
	if aspectRatio != "" {
		payload["image_config"] = map[string]any{"aspect_ratio": aspectRatio}
	}
	body, _ := json.Marshal(payload)
	resp, err := a.Client.PostJSON(ctx, a.BaseURL+"/chat/completions", a.headers(), body)
	if err != nil {
		return ImageResult{}, mapErr(err, resp)
	}
	out, err := ParseImage(resp.ContentType, resp.Body)
	out.Elapsed = resp.Elapsed
	if err == nil && out.CostUSD == nil {
		var est ChatResult
		a.estimate(&est.CostUSD, &est.CostEstimated, model, out.inTokens, out.outTokens)
		out.CostUSD, out.CostEstimated = est.CostUSD, est.CostEstimated
	}
	return out, err
}

// markdownImage is an image a host writes into the message text: ![image](data:image/png;base64,…)
// (CometAPI's Gemini image models).
var markdownImage = regexp.MustCompile(`!\[[^\]]*\]\((data:image/[a-z]+;base64,[A-Za-z0-9+/=]+)\)`)

// ParseImage takes the last image of the first choice: message.images[].image_url.url (OpenRouter),
// image_url content parts, or a markdown image in the text, as a data: URL. Remote URLs are not fetched.
func ParseImage(contentType string, body []byte) (ImageResult, error) {
	obj, err := decode.Object(contentType, body)
	if err != nil {
		return ImageResult{}, err
	}
	if e, ok := obj["error"]; ok && string(e) != "null" {
		return ImageResult{}, apiError(e, 0)
	}
	type imagePart struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	var choices []struct {
		Message struct {
			Content json.RawMessage `json:"content"`
			Images  []imagePart     `json:"images"`
		} `json:"message"`
	}
	if err := json.Unmarshal(obj["choices"], &choices); err != nil || len(choices) == 0 {
		return ImageResult{}, fmt.Errorf("%w: choices missing", decode.ErrUnexpected)
	}
	msg := choices[0].Message
	var out ImageResult
	urls := []string{}
	for _, im := range msg.Images {
		urls = append(urls, im.ImageURL.URL)
	}
	var s string
	if json.Unmarshal(msg.Content, &s) == nil {
		for _, m := range markdownImage.FindAllStringSubmatch(s, -1) {
			urls = append(urls, m[1])
		}
		out.Text = strings.TrimSpace(markdownImage.ReplaceAllString(s, ""))
	} else {
		var parts []imagePart
		if json.Unmarshal(msg.Content, &parts) == nil {
			for _, p := range parts {
				switch p.Type {
				case "text":
					out.Text += p.Text
				case "image_url":
					urls = append(urls, p.ImageURL.URL)
				}
			}
		}
	}
	if u, ok := obj["usage"]; ok {
		var usage map[string]json.RawMessage
		if json.Unmarshal(u, &usage) == nil {
			out.CostUSD = usageCost(usage)
			if n, ok := decode.Number(usage["prompt_tokens"]); ok {
				out.inTokens = int(n)
			}
			if n, ok := decode.Number(usage["completion_tokens"]); ok {
				out.outTokens = int(n)
			}
		}
	}
	for i := len(urls) - 1; i >= 0; i-- {
		mime, data, err := decodeDataURL(urls[i])
		if err == nil {
			out.MIME, out.Data = mime, data
			return out, nil
		}
	}
	return out, fmt.Errorf("%w: no image in the response", decode.ErrUnexpected)
}

func decodeDataURL(u string) (string, []byte, error) {
	rest, ok := strings.CutPrefix(u, "data:")
	if !ok {
		return "", nil, fmt.Errorf("not a data URL")
	}
	meta, payload, ok := strings.Cut(rest, ",")
	if !ok || !strings.HasSuffix(meta, ";base64") {
		return "", nil, fmt.Errorf("not base64 data")
	}
	mime := strings.TrimSuffix(meta, ";base64")
	switch mime {
	case "image/png", "image/jpeg", "image/webp", "image/gif":
	default:
		return "", nil, fmt.Errorf("unsupported image type %q", mime)
	}
	if base64.StdEncoding.DecodedLen(len(payload)) > maxImageBytes {
		return "", nil, fmt.Errorf("image too large")
	}
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return "", nil, err
	}
	return mime, data, nil
}

// imageSize maps an aspect ratio to the sizes gpt-image models take.
func imageSize(aspectRatio string) string {
	w, h, ok := strings.Cut(aspectRatio, ":")
	if !ok {
		return "1024x1024"
	}
	var x, y float64
	if _, err := fmt.Sscan(w, &x); err != nil {
		return "1024x1024"
	}
	if _, err := fmt.Sscan(h, &y); err != nil || y == 0 {
		return "1024x1024"
	}
	switch r := x / y; {
	case r > 1.15:
		return "1536x1024"
	case r < 0.87:
		return "1024x1536"
	}
	return "1024x1024"
}

func (a *Adapter) imageOpenAI(ctx context.Context, model, prompt, aspectRatio string) (ImageResult, error) {
	req := map[string]any{"model": model, "prompt": prompt, "n": 1, "size": imageSize(aspectRatio)}
	if a.API != APIOpenAI && aspectRatio != "" && aspectRatio != "1:1" {
		// other hosts' image models take their own sizes (NanoGPT: 1024x768, 768x1024, …): say the
		// ratio and let the host pick the size
		delete(req, "size")
		req["aspect_ratio"] = aspectRatio
	}
	body, _ := json.Marshal(req)
	resp, err := a.Client.PostJSON(ctx, a.BaseURL+"/images/generations", a.headers(), body)
	if err != nil {
		return ImageResult{}, mapErr(err, resp)
	}
	out, in, outTok, err := ParseImageGeneration(resp.ContentType, resp.Body)
	out.Elapsed = resp.Elapsed
	if err == nil {
		a.estimate(&out.CostUSD, &out.CostEstimated, model, in, outTok)
	}
	return out, err
}

// ParseImageGeneration reads an /images/generations answer: data[0].b64_json and usage
// {input_tokens, output_tokens}. A URL instead of base64 (dall-e's default) is not fetched.
func ParseImageGeneration(contentType string, body []byte) (ImageResult, int, int, error) {
	obj, err := decode.Object(contentType, body)
	if err != nil {
		return ImageResult{}, 0, 0, err
	}
	if e, ok := obj["error"]; ok && string(e) != "null" {
		return ImageResult{}, 0, 0, apiError(e, 0)
	}
	var data []struct {
		B64           string `json:"b64_json"`
		RevisedPrompt string `json:"revised_prompt"`
	}
	if err := json.Unmarshal(obj["data"], &data); err != nil || len(data) == 0 || data[0].B64 == "" {
		return ImageResult{}, 0, 0, fmt.Errorf("%w: no image in the response", decode.ErrUnexpected)
	}
	raw, err := decodeBase64Image(data[0].B64)
	if err != nil {
		return ImageResult{}, 0, 0, err
	}
	out := ImageResult{Data: raw, MIME: sniffImage(raw), Text: data[0].RevisedPrompt}
	var usage struct {
		In  int `json:"input_tokens"`
		Out int `json:"output_tokens"`
	}
	_ = json.Unmarshal(obj["usage"], &usage)
	return out, usage.In, usage.Out, nil
}

func (a *Adapter) imageGemini(ctx context.Context, model, prompt, aspectRatio string) (ImageResult, error) {
	gen := map[string]any{"responseModalities": []string{"IMAGE", "TEXT"}}
	if aspectRatio != "" {
		gen["imageConfig"] = map[string]any{"aspectRatio": aspectRatio}
	}
	body, _ := json.Marshal(map[string]any{
		"contents":         []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": prompt}}}},
		"generationConfig": gen,
	})
	// the native API lives next to the OpenAI-compatible one: …/v1beta/openai → …/v1beta
	base := strings.TrimSuffix(strings.TrimRight(a.BaseURL, "/"), "/openai")
	url := base + "/models/" + a.API.trimModelID(model) + ":generateContent"
	resp, err := a.Client.PostJSON(ctx, url, map[string]string{"x-goog-api-key": a.Cred.Value()}, body)
	if err != nil {
		return ImageResult{}, mapErr(err, resp)
	}
	out, in, outTok, err := ParseGeminiImage(resp.ContentType, resp.Body)
	out.Elapsed = resp.Elapsed
	if err == nil {
		a.estimate(&out.CostUSD, &out.CostEstimated, model, in, outTok)
	}
	return out, err
}

// ParseGeminiImage reads a generateContent answer: the last inlineData part of the first candidate,
// any text parts, and usageMetadata (output counts thinking tokens too, priced as output).
func ParseGeminiImage(contentType string, body []byte) (ImageResult, int, int, error) {
	obj, err := decode.Object(contentType, body)
	if err != nil {
		return ImageResult{}, 0, 0, err
	}
	if e, ok := obj["error"]; ok && string(e) != "null" {
		return ImageResult{}, 0, 0, apiError(e, 0)
	}
	var cands []struct {
		Content struct {
			Parts []struct {
				Text       string `json:"text"`
				Thought    bool   `json:"thought"`
				InlineData *struct {
					MIME string `json:"mimeType"`
					Data string `json:"data"`
				} `json:"inlineData"`
			} `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	}
	_ = json.Unmarshal(obj["candidates"], &cands)
	var usage struct {
		Prompt   int `json:"promptTokenCount"`
		Output   int `json:"candidatesTokenCount"`
		Thoughts int `json:"thoughtsTokenCount"`
	}
	_ = json.Unmarshal(obj["usageMetadata"], &usage)
	if len(cands) == 0 {
		var fb struct {
			BlockReason string `json:"blockReason"`
		}
		_ = json.Unmarshal(obj["promptFeedback"], &fb)
		if fb.BlockReason != "" {
			return ImageResult{}, 0, 0, &APIError{Status: 400, Type: "blocked", Message: "the prompt was blocked: " + fb.BlockReason}
		}
		return ImageResult{}, 0, 0, fmt.Errorf("%w: no candidates", decode.ErrUnexpected)
	}
	var out ImageResult
	for _, p := range cands[0].Content.Parts {
		if p.Thought {
			continue
		}
		if p.InlineData != nil && p.InlineData.Data != "" {
			raw, err := decodeBase64Image(p.InlineData.Data)
			if err != nil {
				return ImageResult{}, 0, 0, err
			}
			out.Data, out.MIME = raw, sniffImage(raw)
		} else if p.Text != "" {
			out.Text += p.Text
		}
	}
	if out.Data == nil {
		reason := cands[0].FinishReason
		if reason != "" && reason != "STOP" {
			return ImageResult{}, 0, 0, &APIError{Status: 400, Type: "blocked", Message: "no image: " + reason}
		}
		return ImageResult{}, 0, 0, fmt.Errorf("%w: no image in the response", decode.ErrUnexpected)
	}
	return out, usage.Prompt, usage.Output + usage.Thoughts, nil
}

func decodeBase64Image(b64 string) ([]byte, error) {
	if base64.StdEncoding.DecodedLen(len(b64)) > maxImageBytes {
		return nil, fmt.Errorf("image too large")
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("%w: image data: %v", decode.ErrUnexpected, err)
	}
	if sniffImage(raw) == "" {
		return nil, fmt.Errorf("%w: not a PNG, JPEG, WebP or GIF image", decode.ErrUnexpected)
	}
	return raw, nil
}

// sniffImage names the image type from its first bytes ("" when it is none of the four).
func sniffImage(b []byte) string {
	switch {
	case len(b) > 8 && string(b[:8]) == "\x89PNG\r\n\x1a\n":
		return "image/png"
	case len(b) > 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "image/jpeg"
	case len(b) > 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "image/webp"
	case len(b) > 6 && (string(b[:6]) == "GIF87a" || string(b[:6]) == "GIF89a"):
		return "image/gif"
	}
	return ""
}
