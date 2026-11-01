package openai

import (
	"cmp"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// BudgetRoles are the roles Budget ranks candidates for, in the order the console shows them.
var BudgetRoles = []string{"response", "planner", "summary", "vision", "embed", "rerank", "image"}

// BudgetMinContext is the context a chat role needs (tokens) when the list says how big it is. A list
// that does not say (0) is not held against a model.
const BudgetMinContext = 32000

// BudgetTop is how many alternatives a role gets.
const BudgetTop = 3

// BudgetPick is one cost-effective alternative for a role: the provider's list entry with its price
// per million tokens and what the list says it can do.
type BudgetPick struct {
	ID             string
	Name           string
	InPerMTok      float64
	OutPerMTok     float64
	CachedPerMTok  float64 // 0 when the list does not give one
	BlendedPerMTok float64 // (3×in + out) / 4: a chat turn reads far more than it writes
	ContextLength  int     // 0 when unknown
	SupportsTools  *bool   // nil when the list does not say
	ImageInput     *bool   // nil when the list does not say
	ImageOutput    bool
	// Known is true when the model belongs to a family the curated recommend rules name for this host
	// (hosts.json "recommend"): reputable models come before unknown ones regardless of price.
	Known bool
	// PriceSource is "list" (the host's own list) or "published" (the family's pricing page, for name-based
	// lists like OpenAI's and Gemini's).
	PriceSource string
}

// Blended is the price used to rank: (3×input + output) / 4 per million tokens.
func Blended(in, out float64) float64 { return (3*in + out) / 4 }

// ModelPrice is a model's price per million tokens and where it came from.
type ModelPrice struct {
	In, Out, Cached float64
	// OutKnown is false when the list gives no output price (Out is then 0 and means "unknown").
	OutKnown bool
	// Source is "list" (the host's own list), "published" (the family's pricing page) or "reference"
	// (the public reference list matched by name).
	Source string
}

// PriceFunc resolves a model's price; ok is false when none is known. Budget takes it so the console's
// model list and the ranking read prices the same way (list, then published, then reference).
type PriceFunc func(Model) (ModelPrice, bool)

// ListPrice is the price a model list carries itself, or for name-based lists (OpenAI, Gemini) the
// family's published one. The application passes a fuller resolver (with the reference list).
func ListPrice(api API) PriceFunc {
	return func(m Model) (ModelPrice, bool) {
		in, out, cached, src, ok := budgetPrice(api, m)
		return ModelPrice{In: in, Out: out, Cached: cached, OutKnown: ok && (src == "published" || out > 0 || m.CompletionPrice != ""), Source: src}, ok
	}
}

// Budget ranks, per role, the cheapest models of a provider's list that fit the role, at most BudgetTop
// each. A model fits when its list says so:
//
//   - response, planner, summary: a chat model (not one that writes pictures) with at least BudgetMinContext
//     tokens of context when known; response and planner also need tool support unless the list says nothing
//     about it (nil), because the tool loop cannot run without it;
//   - vision: a chat model whose list says it takes pictures, or that hosts.json names as taking them
//     (OpenAI's and Gemini's lists give names only); any other model is skipped;
//   - embed, rerank: that kind of model;
//   - image: a model that writes pictures.
//
// Models without a known price are skipped (never treated as free), and so are variants a bot cannot run
// on (budgetVariant): ":free" ones (rate limited), ":batch" ones (the batch API only answers hours later),
// suffix variants of a model whose base is listed too, and aliases that move ("~…", "…-latest"). A chat
// model listed with no output price is not one to answer with (an embedding or classifier model listed as
// chat).
//
// Candidates of a family the curated recommend rules name for the host and the role come first (chat roles
// use the response, planner and summary rules; embed, rerank and image their own), then the rest. Each
// group goes by blended price, but a family appears once, with the cheapest model of its newest
// generation, so the top three are three families where the list has them; slots left over are filled
// from the remaining models in the same order. price may be nil (ListPrice).
func Budget(api API, baseURL string, models []Model, price PriceFunc) map[string][]BudgetPick {
	if price == nil {
		price = ListPrice(api)
	}
	listed := map[string]bool{}
	for _, m := range models {
		listed[m.ID] = true
	}
	out := map[string][]BudgetPick{}
	for _, role := range BudgetRoles {
		families := knownFamilies(api, baseURL, role)
		var picks []BudgetPick
		versions := map[string][]int{}
		famOf := map[string]string{}
		seen := map[string]bool{}
		for _, m := range models {
			if m.ImageInput == nil && PublishedImageInput(api, m.ID) { // a name-only list (OpenAI, Gemini) says nothing about pictures
				yes := true
				m.ImageInput = &yes
			}
			if seen[m.ID] || !budgetFits(role, m) || budgetVariant(m.ID) || budgetSuffixOfListed(m.ID, listed) {
				continue
			}
			pr, ok := price(m)
			if !ok || (budgetChatRole(role) && (!pr.OutKnown || pr.Out <= 0)) {
				continue
			}
			seen[m.ID] = true
			id := strings.ToLower(api.trimModelID(m.ID))
			fam, known := budgetFamily(id, families)
			famOf[m.ID] = fam
			if v := budgetMajor(id); compareVersions(v, versions[fam]) > 0 {
				versions[fam] = v
			}
			picks = append(picks, BudgetPick{ID: m.ID, Name: m.Name, InPerMTok: pr.In, OutPerMTok: pr.Out, CachedPerMTok: pr.Cached, BlendedPerMTok: Blended(pr.In, pr.Out),
				ContextLength: m.ContextLength, SupportsTools: m.SupportsTools, ImageInput: m.ImageInput, ImageOutput: m.ImageOutput,
				Known: known, PriceSource: pr.Source})
		}
		slices.SortStableFunc(picks, func(a, b BudgetPick) int {
			if a.Known != b.Known {
				if a.Known {
					return -1
				}
				return 1
			}
			if c := cmp.Compare(a.BlendedPerMTok, b.BlendedPerMTok); c != 0 {
				return c
			}
			if c := cmp.Compare(b.ContextLength, a.ContextLength); c != 0 { // a bigger context at the same price
				return c
			}
			return cmp.Compare(a.ID, b.ID)
		})
		// one model per family: the cheapest of its newest generation (picks is already in rank order). A
		// generation is the major version: gpt-6 and gpt-6.1 are one, so a dear point release does not push
		// the family's cheap model out.
		var chosen, rest []BudgetPick
		taken := map[string]bool{}
		for _, p := range picks {
			fam := famOf[p.ID]
			if !taken[fam] && compareVersions(budgetMajor(strings.ToLower(api.trimModelID(p.ID))), versions[fam]) == 0 {
				taken[fam] = true
				chosen = append(chosen, p)
				continue
			}
			rest = append(rest, p)
		}
		picks = append(chosen, rest...)
		if len(picks) > BudgetTop {
			picks = picks[:BudgetTop]
		}
		if len(picks) > 0 {
			out[role] = picks
		}
	}
	return out
}

// budgetVariant reports a model id a bot should not be pointed at: a rate-limited, batch-only or routed
// variant, or an alias whose model changes under it.
func budgetVariant(id string) bool {
	id = strings.ToLower(id)
	if strings.HasPrefix(id, "~") || strings.Contains(id, "latest") {
		return true
	}
	// a preview or experimental model can change or vanish without notice: not one to run a bot on
	if parts := idParts(id); slices.Contains(parts, "preview") || slices.Contains(parts, "exp") {
		return true
	}
	for _, suffix := range []string{":free", ":batch", ":online", ":nitro", ":thinking", ":extended"} {
		if strings.HasSuffix(id, suffix) {
			return true
		}
	}
	return false
}

// budgetSuffixOfListed is a ":suffix" variant whose base model is listed as well: the base is the one to keep.
func budgetSuffixOfListed(id string, listed map[string]bool) bool {
	i := strings.LastIndex(id, ":")
	return i > 0 && listed[id[:i]]
}

// budgetChatRole is a role that writes text, so a candidate needs an output price.
func budgetChatRole(role string) bool {
	return role == "response" || role == "planner" || role == "summary" || role == "vision"
}

// budgetFamily is the family of a model id and whether the curated rules know it: the first wanted word
// of a rule the id has, else the vendor ("vendor/model") or the first word of the name.
func budgetFamily(id string, families []string) (string, bool) {
	parts := idParts(id)
	for _, f := range families {
		if slices.Contains(parts, f) {
			return f, true
		}
	}
	if i := strings.Index(id, "/"); i > 0 {
		return id[:i], false
	}
	if len(parts) > 0 {
		return parts[0], false
	}
	return id, false
}

var sizeRE = regexp.MustCompile(`^\d+(\.\d+)?[bkmt]$|^\d+x\d+[bkmt]$`)

// budgetVersion is the generation in a model id: the first number of a word that is not a parameter count
// ("deepseek-v4.1-flash" → 4.1, "gpt-6-luna" → 6, "gpt-oss-20b" → none).
// budgetMajor is the generation of a model id: the major part of budgetVersion, nil without one.
func budgetMajor(id string) []int {
	if v := budgetVersion(id); len(v) > 0 {
		return v[:1]
	}
	return nil
}

func budgetVersion(id string) []int {
	if i := strings.LastIndex(id, "/"); i >= 0 {
		id = id[i+1:]
	}
	for _, p := range idParts(id) {
		if sizeRE.MatchString(p) {
			continue
		}
		if m := versionRE.FindString(p); m != "" {
			return modelVersion(m)
		}
	}
	return nil
}

func budgetFits(role string, m Model) bool {
	chat := m.Kind == "chat" && !m.ImageOutput
	big := m.ContextLength == 0 || m.ContextLength >= BudgetMinContext
	switch role {
	case "response", "planner":
		return chat && big && (m.SupportsTools == nil || *m.SupportsTools)
	case "summary":
		return chat && big
	case "vision":
		return chat && big && m.ImageInput != nil && *m.ImageInput
	case "embed":
		return m.Kind == "embedding"
	case "rerank":
		return m.Kind == "rerank"
	case "image":
		return m.ImageOutput || m.Kind == "image"
	}
	return false
}

// budgetPrice is a model's price per million tokens (input, output, cached input) and where it came from.
// A price below zero (OpenRouter's routers are listed at -1) or 0/0 is unknown, not free.
func budgetPrice(api API, m Model) (in, out, cached float64, src string, ok bool) {
	if v, err := strconv.ParseFloat(m.PromptPrice, 64); err == nil && v >= 0 {
		o, err := strconv.ParseFloat(m.CompletionPrice, 64)
		if err != nil || o < 0 {
			o = 0
		}
		if v > 0 || o > 0 {
			in, out, src, ok = v*1e6, o*1e6, "list", true
			if c, err := strconv.ParseFloat(m.CachedPrice, 64); err == nil && c > 0 && c <= v {
				cached = c * 1e6
			}
			return
		}
	}
	if pub, found := Published(api, m.ID); found && (pub.In > 0 || pub.Out > 0) {
		cached = 0
		if pub.Cached > 0 && pub.Cached < pub.In {
			cached = pub.Cached * 1e6
		}
		return pub.In * 1e6, pub.Out * 1e6, cached, "published", true
	}
	return 0, 0, 0, "", false
}

// knownFamilies are the families the curated recommend rules of the host name for a role: the first
// wanted word of the rules that role is served by ("deepseek", "gpt", "qwen3", …). Chat roles (response,
// planner, summary, vision) use the chat-kind rules; embed, rerank and image use their own rule.
func knownFamilies(api API, baseURL, role string) []string {
	rules := recommendRules[string(api)]
	if h, ok := HostByURL(baseURL); ok && recommendRules[h.ID] != nil {
		rules = recommendRules[h.ID]
	}
	var out []string
	for name, r := range rules {
		own := name == role
		if budgetChatRole(role) {
			own = r.Kind == "chat"
		}
		if own && len(r.Has) > 0 && !slices.Contains(out, r.Has[0]) {
			out = append(out, r.Has[0])
		}
	}
	slices.Sort(out)
	return out
}
