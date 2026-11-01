// Package practice is an offline, OpenAI-compatible model endpoint for the console's practice mode.
// It answers /models, /embeddings, /rerank and /chat/completions inside the process (an
// http.RoundTripper), so practice runs the real import → index → search → plan → answer pipeline with
// no network and no cost. Vectors are hashed character bigrams, reranking is bigram overlap, the
// planner always searches memory, and answers quote the best evidence with a practice notice. The
// notice text comes from the language pack (practice.*), never from code.
package practice

import (
	"bytes"
	"encoding/json"
	"hash/fnv"
	"io"
	"math"
	"net/http"
	"regexp"
	"strings"

	"masamong/internal/lang"
)

// BaseURL is the address practice providers are configured with; nothing ever connects to it.
const BaseURL = "https://practice.masamong.invalid/v1"

// Dims is the vector width.
const Dims = 256

// Models are the ids the practice endpoint lists (one per role).
var Models = []string{"practice/chat", "practice/embed", "practice/rerank"}

// Transport serves the endpoint. Locale picks the language pack for answers.
type Transport struct{ Locale string }

func (t Transport) RoundTrip(r *http.Request) (*http.Response, error) {
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(io.LimitReader(r.Body, 8<<20))
		_ = r.Body.Close()
	}
	var out any
	path := r.URL.Path
	switch {
	case strings.HasSuffix(path, "/models"):
		var data []map[string]any
		for _, m := range Models {
			kind := "chat"
			if strings.HasSuffix(m, "embed") {
				kind = "embedding"
			}
			data = append(data, map[string]any{"id": m, "name": m, "context_length": 32768, "kind": kind,
				"pricing": map[string]any{"prompt": "0", "completion": "0"}})
		}
		out = map[string]any{"data": data}
	case strings.HasSuffix(path, "/embeddings"):
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.Unmarshal(body, &req)
		data := make([]map[string]any, len(req.Input))
		for i, s := range req.Input {
			data[i] = map[string]any{"index": i, "embedding": Embed(s)}
		}
		out = map[string]any{"data": data, "usage": map[string]any{"cost": 0}}
	case strings.HasSuffix(path, "/rerank"):
		var req struct {
			Query     string   `json:"query"`
			Documents []string `json:"documents"`
			TopN      int      `json:"top_n"`
		}
		_ = json.Unmarshal(body, &req)
		type sc struct {
			i int
			s float64
		}
		scores := make([]sc, len(req.Documents))
		for i, d := range req.Documents {
			scores[i] = sc{i, overlap(req.Query, d)}
		}
		for i := 1; i < len(scores); i++ { // insertion sort: tiny lists
			for j := i; j > 0 && scores[j].s > scores[j-1].s; j-- {
				scores[j], scores[j-1] = scores[j-1], scores[j]
			}
		}
		n := req.TopN
		if n <= 0 || n > len(scores) {
			n = len(scores)
		}
		res := make([]map[string]any, n)
		for k := 0; k < n; k++ {
			// map overlap into the product's rerank score range so thresholds behave
			res[k] = map[string]any{"index": scores[k].i, "relevance_score": 0.5 + 0.5*scores[k].s}
		}
		out = map[string]any{"results": res, "model": "practice/rerank", "usage": map[string]any{"cost": 0}}
	case strings.HasSuffix(path, "/chat/completions"):
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		system, user := "", ""
		for _, m := range req.Messages {
			if m.Role == "system" && system == "" {
				system = m.Content
			}
			if m.Role == "user" {
				user = m.Content
			}
		}
		out = map[string]any{"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": t.reply(system, user)}}},
			"usage": map[string]any{"cost": 0, "prompt_tokens": 0, "completion_tokens": 0}}
	default:
		return respond(r, http.StatusNotFound, map[string]any{"error": map[string]any{"message": "practice endpoint: unknown path"}}), nil
	}
	return respond(r, http.StatusOK, out), nil
}

func respond(r *http.Request, status int, v any) *http.Response {
	raw, _ := json.Marshal(v)
	return &http.Response{StatusCode: status, Status: http.StatusText(status), Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(raw)),
		ContentLength: int64(len(raw)), Request: r}
}

// evidenceHead is the numbered line (chat.evidence_line, chat.summary_line) that opens each evidence item.
var evidenceHead = regexp.MustCompile(`^\[\d+\] `)

// reply answers the two prompts the chat runtime sends: the planner (JSON) and the answer.
func (t Transport) reply(system, user string) string {
	if strings.Contains(system, "tool_plan") {
		plan := map[string]any{"memory_requirement": "required", "time_scope": "any", "question_kind": "other",
			"person_targets": []string{}, "tool_plan": []map[string]any{{"tool_name": "memory.search", "parameters": map[string]any{}}}}
		raw, _ := json.Marshal(plan)
		return string(raw)
	}
	pack := lang.For(t.Locale)
	if ev := pickEvidence(user, 3); ev != "" {
		return lang.Fill(pack.Practice.Answer, "evidence", ev)
	}
	if strings.Contains(system, "{body}") || strings.Contains(user, "summary") {
		return firstLine(user)
	}
	return pack.Practice.NoMemory
}

// pickEvidence quotes the evidence lines that share the most with the question (the prompt's last
// line, "{who}: {message}"), in their original order. Excerpt headers such as "[room · date · names]"
// and the numbered item lines are skipped; a closing tag ("</memory>") ends the evidence.
func pickEvidence(prompt string, n int) string {
	lines := strings.Split(prompt, "\n")
	question := ""
	for i := len(lines) - 1; i >= 0; i-- {
		if s := strings.TrimSpace(lines[i]); s != "" {
			question = s
			break
		}
	}
	if _, after, ok := strings.Cut(question, ": "); ok {
		question = after
	}
	type cand struct {
		text  string
		score float64
	}
	var cands []cand
	in := false
	for _, l := range lines {
		s := strings.TrimSpace(l)
		switch {
		case evidenceHead.MatchString(s):
			in = true
			continue
		case !in || s == "":
			continue
		case strings.HasPrefix(s, "<"):
			in = false
			continue
		case strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]"):
			continue
		}
		cands = append(cands, cand{s, overlap(question, s)})
	}
	if len(cands) == 0 {
		return ""
	}
	// the n best-scoring lines; with no overlap at all, the first lines of the top evidence
	best := make([]bool, len(cands))
	for k := 0; k < min(n, len(cands)); k++ {
		pick := -1
		for i, c := range cands {
			if !best[i] && c.score > 0 && (pick < 0 || c.score > cands[pick].score) {
				pick = i
			}
		}
		if pick < 0 {
			break
		}
		best[pick] = true
	}
	var out []string
	for i, c := range cands {
		if best[i] {
			out = append(out, c.text)
		}
	}
	if len(out) == 0 {
		for _, c := range cands[:min(2, len(cands))] {
			out = append(out, c.text)
		}
	}
	return strings.Join(out, "\n")
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i > 0 {
		s = s[:i]
	}
	r := []rune(s)
	return string(r[:min(len(r), 80)])
}

func bigrams(s string) map[string]bool {
	r := []rune(strings.ToLower(s))
	out := map[string]bool{}
	for i := 0; i+1 < len(r); i++ {
		if r[i] == ' ' || r[i+1] == ' ' {
			continue
		}
		out[string(r[i:i+2])] = true
	}
	return out
}

func overlap(q, d string) float64 {
	a, b := bigrams(q), bigrams(d)
	if len(a) == 0 {
		return 0
	}
	n := 0
	for k := range a {
		if b[k] {
			n++
		}
	}
	return float64(n) / float64(len(a))
}

// Embed is a deterministic unit vector of hashed character unigrams and bigrams.
func Embed(text string) []float32 {
	v := make([]float32, Dims)
	r := []rune(strings.ToLower(text))
	add := func(s string, w float32) {
		h := fnv.New32a()
		_, _ = h.Write([]byte(s))
		v[h.Sum32()%Dims] += w
	}
	for i := range r {
		if r[i] != ' ' && r[i] != '\n' {
			add(string(r[i]), 0.3)
		}
		if i+1 < len(r) && r[i] != ' ' && r[i+1] != ' ' {
			add(string(r[i:i+2]), 1)
		}
	}
	var n float64
	for _, x := range v {
		n += float64(x * x)
	}
	if n == 0 {
		v[0] = 1
		return v
	}
	for i := range v {
		v[i] = float32(float64(v[i]) / math.Sqrt(n))
	}
	return v
}
