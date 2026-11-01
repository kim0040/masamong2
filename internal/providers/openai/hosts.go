package openai

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Host is one model provider the console offers to add (hosts.json).
type Host struct {
	API     API    `json:"api"`
	ID      string `json:"id"`   // the provider name config uses by default
	Name    string `json:"name"` // shown to the operator
	BaseURL string `json:"base_url"`
	KeyEnv  string `json:"key_env"` // environment variable a key may come from
	KeyURL  string `json:"key_url"` // where the operator makes a key
	// Rerank false: the host has no /rerank although its family may (CometAPI and NanoGPT list no
	// reranker); nil follows the family
	Rerank *bool `json:"rerank,omitempty"`
}

// RerankAt reports whether the provider at baseURL of family api has a /rerank endpoint.
func RerankAt(api API, baseURL string) bool {
	if h, ok := HostByURL(baseURL); ok && h.Rerank != nil {
		return *h.Rerank
	}
	return api.Rerank()
}

// PublishedPrice is a price from a host's pricing page (USD per token).
type PublishedPrice struct {
	In, Out float64
	// Cached is the price of input tokens the host served from its prompt cache (USD per token); equal
	// to In when the page lists none
	Cached  float64
	Checked string // YYYY-MM-DD the page was read
	Source  string
}

//go:embed hosts.json
var hostsJSON []byte

var hostsData = func() (d struct {
	Hosts     []Host `json:"hosts"`
	Reference struct {
		Source string `json:"source"`
		URL    string `json:"url"`
	} `json:"reference_prices"`
	Prices map[API]struct {
		Checked string               `json:"checked"`
		Source  string               `json:"source"`
		Models  map[string][]float64 `json:"models"` // [input, output] or [input, output, cached input] per million tokens
		// ImageInput names the chat models of the family that accept pictures; its list does not say
		ImageInput []string `json:"image_input"`
	} `json:"prices"`
	EmbedWidths map[string]int `json:"embed_widths"`
}) {
	if err := json.Unmarshal(hostsJSON, &d); err != nil {
		panic(fmt.Sprintf("openai: hosts.json: %v", err))
	}
	return d
}()

// Hosts lists the providers the console offers to add, in its order.
func Hosts() []Host { return append([]Host(nil), hostsData.Hosts...) }

// ReferencePrices is the public price list models without a price of their own are matched against
// by name (ReferenceName), and what to call it.
func ReferencePrices() (url, source string) {
	return hostsData.Reference.URL, hostsData.Reference.Source
}

// ReferenceName is the key a model is matched by across hosts: the part after the last '/', without a
// ':variant', lower case ("deepseek/deepseek-v4-flash:thinking" and "deepseek-v4-flash" meet).
func ReferenceName(id string) string {
	id = strings.ToLower(strings.TrimSpace(id))
	if i := strings.LastIndex(id, "/"); i >= 0 {
		id = id[i+1:]
	}
	id, _, _ = strings.Cut(id, ":")
	return id
}

// HostFor is the listed host of a family. Listed hosts of the compatible family (CometAPI, NanoGPT)
// are found by id or address instead: "compatible" alone means any OpenAI-compatible address.
func HostFor(api API) (Host, bool) {
	for _, h := range hostsData.Hosts {
		if h.API == api && api != APICompatible {
			return h, true
		}
	}
	return Host{}, false
}

// HostByID is a listed host by its id.
func HostByID(id string) (Host, bool) {
	for _, h := range hostsData.Hosts {
		if h.ID == id {
			return h, true
		}
	}
	return Host{}, false
}

// HostByURL is the listed host serving baseURL (the same address, ignoring a trailing slash).
func HostByURL(baseURL string) (Host, bool) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	for _, h := range hostsData.Hosts {
		if strings.EqualFold(strings.TrimRight(h.BaseURL, "/"), baseURL) {
			return h, true
		}
	}
	return Host{}, false
}

// Published returns the price a family's pricing page lists for model, if hosts.json has it.
func Published(api API, model string) (PublishedPrice, bool) {
	p, ok := hostsData.Prices[api]
	if !ok {
		return PublishedPrice{}, false
	}
	m, ok := p.Models[api.trimModelID(model)]
	if !ok {
		return PublishedPrice{}, false
	}
	if len(m) < 2 {
		return PublishedPrice{}, false
	}
	pub := PublishedPrice{In: m[0] / 1e6, Out: m[1] / 1e6, Cached: m[0] / 1e6, Checked: p.Checked, Source: p.Source}
	if len(m) > 2 {
		pub.Cached = m[2] / 1e6
	}
	return pub, true
}

// PublishedImageInput reports a model hosts.json names as taking pictures as input (the family's list
// of models carries no such field). false means "not known to", never "does not".
func PublishedImageInput(api API, model string) bool {
	return slices.Contains(hostsData.Prices[api].ImageInput, api.trimModelID(model))
}

// EmbedFullWidth is the full width of an embedding model hosts.json names as cuttable (trained so the
// leading part of a vector is a vector of its own: Matryoshka / MRL). ok is false for any other model,
// which means "not known to be cuttable": such a model always keeps its full width.
func EmbedFullWidth(model string) (width int, ok bool) {
	width, ok = hostsData.EmbedWidths[ReferenceName(model)]
	return width, ok && width > 0
}

type roleRule struct {
	Kind string   `json:"kind"`
	Has  []string `json:"has"`
	Not  []string `json:"not"`
}

var recommendRules = func() (r map[string]map[string]roleRule) {
	var d struct {
		Recommend map[string]map[string]roleRule `json:"recommend"`
	}
	if err := json.Unmarshal(hostsJSON, &d); err != nil {
		panic(fmt.Sprintf("openai: hosts.json: %v", err))
	}
	return d.Recommend
}()

// Recommend picks a model per role from a host's list (hosts.json "recommend"): the newest released
// model whose ID has every wanted part and none of the unwanted ones; a preview only when no release
// matches. The rules of the listed host at baseURL come first (CometAPI, NanoGPT), else the family's.
// Roles without a rule, or without a match, are absent.
func Recommend(api API, baseURL string, models []Model) map[string]string {
	out := map[string]string{}
	rules := recommendRules[string(api)]
	if h, ok := HostByURL(baseURL); ok && recommendRules[h.ID] != nil {
		rules = recommendRules[h.ID]
	}
	for role, rule := range rules {
		best, bestVer, bestPreview := "", []int(nil), true
		for _, m := range models {
			id := strings.ToLower(api.trimModelID(m.ID))
			if !ruleFitsKind(rule.Kind, m) || !idHasAll(id, rule.Has) || idHasAny(id, rule.Not) || strings.Contains(id, "latest") {
				continue
			}
			preview := strings.Contains(id, "preview") || strings.Contains(id, "exp")
			ver := modelVersion(id)
			if api == APIAnthropic {
				ver = dashedVersion(id)
			}
			switch {
			case best == "",
				bestPreview && !preview,
				preview == bestPreview && compareVersions(ver, bestVer) > 0,
				preview == bestPreview && compareVersions(ver, bestVer) == 0 && len(id) < len(best):
				best, bestVer, bestPreview = id, ver, preview
			}
		}
		if best != "" {
			out[role] = best
		}
	}
	return out
}

// ruleFitsKind says whether a model can fill a rule of this kind. A model that writes pictures is an
// image model whatever its list says: OpenRouter lists them as chat models with an image output
// modality, while name-based lists (OpenAI, Gemini) say "image". A chat rule never takes one.
func ruleFitsKind(kind string, m Model) bool {
	switch kind {
	case "image":
		return m.ImageOutput || m.Kind == "image"
	case "chat":
		return m.Kind == "chat" && !m.ImageOutput
	}
	return m.Kind == kind
}

func idParts(id string) []string {
	return strings.FieldsFunc(id, func(r rune) bool { return r == '-' || r == '/' || r == '_' || r == ':' })
}

func idHasAll(id string, parts []string) bool {
	have := idParts(id)
	for _, p := range parts {
		if !slices.Contains(have, p) {
			return false
		}
	}
	return true
}

func idHasAny(id string, parts []string) bool {
	have := idParts(id)
	for _, p := range parts {
		if slices.Contains(have, p) {
			return true
		}
	}
	return false
}

var versionRE = regexp.MustCompile(`\d+(?:\.\d+)*`)

// modelVersion is the first number in an ID ("gemini-3.8-flash" → 3.8, "gpt-5-mini" → 5,
// "gemini-embedding-001" → 1), as components so 3.10 sorts after 3.9.
func modelVersion(id string) []int {
	m := versionRE.FindString(id)
	var v []int
	for _, p := range strings.Split(m, ".") {
		n, err := strconv.Atoi(p)
		if err != nil {
			return v
		}
		v = append(v, n)
	}
	return v
}

// dashedVersion reads a version written with dashes, as Anthropic names its models: "claude-sonnet-5-5"
// is 5.5 and newer than "claude-sonnet-5"; "claude-haiku-4-5-20251001" is 4.5 (a date is no part).
func dashedVersion(id string) []int {
	var v []int
	for _, p := range strings.Split(dashedVersionRE.FindString(id), "-") {
		if n, err := strconv.Atoi(p); err == nil {
			v = append(v, n)
		}
	}
	return v
}

var dashedVersionRE = regexp.MustCompile(`\b\d{1,2}(?:-\d{1,2}\b)*`)

func compareVersions(a, b []int) int {
	for i := 0; i < max(len(a), len(b)); i++ {
		x, y := 0, 0
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			if x > y {
				return 1
			}
			return -1
		}
	}
	return 0
}
