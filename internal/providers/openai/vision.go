package openai

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"masamong/internal/errclass"
)

// Picture input. Every host family here takes pictures the Chat Completions way: a user message's
// content becomes a list of parts, {"type":"text"} then {"type":"image_url"} with a data: URL. Gemini's
// OpenAI-compatible endpoint (the one this adapter speaks to Gemini) takes the same parts and turns
// them into its own inline_data. A host or model that takes no pictures refuses the request; the
// refusal is classified here and remembered, like a model that takes no tools.

// ImagePart is one picture of a message: its media type and bytes (sent inline, never by address).
type ImagePart struct {
	MIME string
	Data []byte
}

// URL is the part's data: URL.
func (p ImagePart) URL() string {
	return "data:" + p.MIME + ";base64," + p.base64()
}

func (p ImagePart) base64() string { return base64.StdEncoding.EncodeToString(p.Data) }

type textPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type imagePart struct {
	Type     string `json:"type"`
	ImageURL struct {
		URL string `json:"url"`
	} `json:"image_url"`
}

func marshalWithImages(m Message) ([]byte, error) {
	parts := make([]any, 0, len(m.Images)+1)
	if m.Content != "" {
		parts = append(parts, textPart{Type: "text", Text: m.Content})
	}
	for _, img := range m.Images {
		if img.MIME == "" || len(img.Data) == 0 {
			return nil, fmt.Errorf("openai: image part without data")
		}
		p := imagePart{Type: "image_url"}
		p.ImageURL.URL = img.URL()
		parts = append(parts, p)
	}
	return json.Marshal(struct {
		Role    string `json:"role"`
		Content []any  `json:"content"`
		Name    string `json:"name,omitempty"`
	}{Role: m.Role, Content: parts, Name: m.Name})
}

// ErrVisionUnsupported is a model that takes no pictures: returned without a request for a model this
// adapter has seen refuse them, and wrapped around the host's refusal the first time.
var ErrVisionUnsupported error = classedError{msg: "openai: model takes no images", class: errclass.Unsupported}

// visionError is a host's refusal of pictures: ErrVisionUnsupported for errors.Is, the APIError for
// errors.As.
type visionError struct{ err error }

func (e *visionError) Error() string            { return ErrVisionUnsupported.Error() + ": " + e.err.Error() }
func (e *visionError) Unwrap() []error          { return []error{ErrVisionUnsupported, e.err} }
func (e *visionError) ErrClass() errclass.Class { return errclass.Unsupported }

// VisionUnsupported reports whether this adapter has seen the model refuse pictures in the last
// VisionRefusalTTL. The application keeps its own process-wide memory too (a config reload forgets it).
func (a *Adapter) VisionUnsupported(model string) bool {
	v, ok := a.visionOff.Load(model)
	if !ok {
		return false
	}
	if at, _ := v.(time.Time); now().Sub(at) < VisionRefusalTTL {
		return true
	}
	a.visionOff.CompareAndDelete(model, v)
	return false
}

func hasImages(req ChatRequest) bool {
	for _, m := range req.Messages {
		if len(m.Images) > 0 && len(m.Raw) == 0 {
			return true
		}
	}
	return false
}

var (
	imageTerms = []string{"image", "vision", "multimodal", "multi-modal", "modalit"}
	// the model or route takes no pictures at all
	imageNotTaken = []string{"support", "unknown variant", "invalid content type", "expected `text`", "expected text", "only text",
		"text-only", "text only", "no endpoints found", "not enabled", "not allowed", "not available", "does not accept", "doesn't accept",
		"cannot accept", "can't accept", "not a multimodal", "is not a vision"}
	// a complaint about this picture, not about pictures: the next one may work
	imageThisOne = []string{"too large", "too big", "exceeds", "format", "decode", "corrupt", "base64", "dimension", "size", "invalid image data"}
	// a host that takes only string content refuses the parts list without naming pictures
	stringOnly = []string{"expected a string", "expected string", "must be a string", "must be string", "should be a string"}
	// a safety or moderation block of this picture, or a limit: about this request, not the model
	// ("Your input image may contain content that is not allowed by our safety system")
	imageBlocked = []string{"safety", "moderation", "policy", "content_policy", "violat", "flagged", "rate limit", "rate_limit",
		"ratelimit", "too many requests", "quota"}
)

// VisionRefusalTTL is how long a model's refusal of pictures is remembered: a host can add vision to a
// model, or a refusal can be misread, so the model is asked again after it.
const VisionRefusalTTL = time.Hour

// now is the clock of the refusal memory (tests move it).
var now = time.Now

// visionRefused decides whether err is a host saying the model takes no pictures.
func visionRefused(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) || errors.Is(err, ErrAuth) {
		return false
	}
	switch ae.Status {
	case 0, 400, 404, 415, 422:
	default:
		return false
	}
	m := strings.ToLower(ae.Type + " " + ae.Code + " " + ae.Message)
	if hasAny(m, imageBlocked) {
		return false
	}
	if strings.Contains(m, "content") && hasAny(m, stringOnly) {
		return true
	}
	return hasAny(m, imageTerms) && hasAny(m, imageNotTaken) && !hasAny(m, imageThisOne)
}
