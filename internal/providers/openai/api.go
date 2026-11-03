package openai

import (
	"net/url"
	"regexp"
	"strings"
)

// API is the family of host an adapter talks to. Hosts that speak /chat/completions still disagree on
// a handful of things, and each difference is decided here once instead of by URL checks scattered
// through the adapter. The anthropic family speaks Anthropic's Messages format instead (anthropic.go):
//
//	                 reasoning field          token limit field       key check   rerank  images
//	openrouter       reasoning:{effort}       max_tokens              GET /key    /rerank chat modalities
//	openai           reasoning_effort         max_completion_tokens   GET /models  –      /images/generations
//	gemini           reasoning_effort         max_tokens              GET /models  –      native generateContent
//	compatible       reasoning_effort         max_tokens              GET /models /rerank chat modalities
//	anthropic        thinking+output_config   max_tokens              GET /models  –      –
//
// Per-model differences (which reasoning levels a model accepts, whether it takes a temperature) are
// read from the model list where it says them (caps.go) and otherwise not tabulated: a request a model
// refuses for an optional field is retried once without it, and the adapter remembers the shape that
// worked (see shape.go).
type API string

const (
	APIOpenRouter API = "openrouter"
	APIOpenAI     API = "openai"
	APIGemini     API = "gemini"
	APICompatible API = "compatible"
	// APIAnthropic is Anthropic's native Messages API at any address: Anthropic's own
	// (https://api.anthropic.com/v1) or another company's Anthropic-format endpoint (…/anthropic or
	// …/anthropic/v1; /v1 is added when missing, a pasted …/v1/messages is trimmed). Requests go to
	// {base}/messages with x-api-key and anthropic-version, plus a bearer token off Anthropic's host
	// (anthropic.go). Thinking is set with "thinking" and output_config.effort from the model's
	// capabilities (GET /models/{id}); a host without that list works with typed model IDs.
	APIAnthropic API = "anthropic"
)

// APIs lists the families in the order the console offers them.
var APIs = []API{APIOpenRouter, APIOpenAI, APIGemini, APIAnthropic, APICompatible}

// ValidAPI reports whether s names a family ("" means detect from the address).
func ValidAPI(s string) bool {
	if s == "" {
		return true
	}
	for _, a := range APIs {
		if string(a) == s {
			return true
		}
	}
	return false
}

// DetectAPI picks the family from the base URL's host. Anything unknown is a generic
// OpenAI-compatible host.
func DetectAPI(baseURL string) API {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return APICompatible
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case host == "openrouter.ai" || strings.HasSuffix(host, ".openrouter.ai"):
		return APIOpenRouter
	case host == "api.openai.com":
		return APIOpenAI
	case host == "generativelanguage.googleapis.com":
		return APIGemini
	case host == "api.anthropic.com":
		return APIAnthropic
	}
	return APICompatible
}

// ResolveAPI is the configured family, or the detected one when config leaves it empty.
func ResolveAPI(configured, baseURL string) API {
	if configured != "" && ValidAPI(configured) {
		return API(configured)
	}
	return DetectAPI(baseURL)
}

// Rerank reports whether the family has a /rerank endpoint. OpenAI and Gemini have none; memory search
// then ranks without one (hybrid fusion) instead of calling an endpoint that cannot answer.
func (a API) Rerank() bool { return a == APIOpenRouter || a == APICompatible }

// PricesInList reports whether the family's model list may carry prices: OpenRouter's always does, a
// compatible host's may (NanoGPT's detailed list); a list without prices leaves them unknown.
func (a API) PricesInList() bool { return a == APIOpenRouter || a == APICompatible }

// ModelKind classifies a model from its ID for lists that do not say what a model does (OpenAI and
// Gemini list only names). "other" covers speech, audio, video, music, live and moderation models
// that cannot serve any role here.
func ModelKind(id string) (kind string, imageOutput bool) {
	l := strings.ToLower(id)
	switch {
	case strings.Contains(l, "embed"):
		return "embedding", false
	case strings.Contains(l, "rerank"):
		return "rerank", false
	case otherModelRE.MatchString(l):
		return "other", false
	case imageModelRE.MatchString(l):
		return "image", true
	}
	return "chat", false
}

var (
	otherModelRE = regexp.MustCompile(`(^|[-/_.])(tts|audio|transcribe|whisper|realtime|live|native-audio|moderation|veo|sora|lyria|robotics|computer-use|aqa|search-preview|translate|deep-research|antigravity)([-/_.]|$)|^(models/)?(veo|lyria|sora|tts|whisper|omni-moderation|text-moderation|babbage|davinci)`)
	imageModelRE = regexp.MustCompile(`(^|[-/_.])(image|imagen|dall-e|gpt-image|nano-banana)([-/_.]|$)`)
)

// trimModelID drops the "models/" prefix Gemini lists IDs with; its chat endpoint takes either.
func (a API) trimModelID(id string) string {
	if a == APIGemini {
		return strings.TrimPrefix(id, "models/")
	}
	return id
}
