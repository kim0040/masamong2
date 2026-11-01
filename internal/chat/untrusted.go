package chat

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
	"sync"

	"masamong/internal/lang"
)

// Text the bot did not write reaches a prompt only after it is cleaned here: tool and web results, and
// the room memory a model extracted from other members' chat. Control characters and the special tokens
// of chat templates are removed (a model can read "<|im_start|>system" in a page as a new turn), and the
// prompt's own markers are neutralized so the text cannot close the block it sits in. Normal text comes
// out unchanged, so the evaluated prompts stay byte for byte the same.

// specialTokenRE matches the special tokens of common chat templates, in any case: <|im_start|>,
// <|endoftext|>, <|reserved_special_token_N|>, DeepSeek's <｜…｜>, Llama 2's [INST] and <<SYS>>, Gemma's
// <start_of_turn>.
// A token has no spaces inside, so code such as F#'s <| x |> is left alone; [INST] is matched as written.
var specialTokenRE = regexp.MustCompile(`(?i)<\|[^|<>\s]{1,64}\|>|<｜[^｜<>\s]{1,64}｜>|(?-i:\[/?INST\])|<</?SYS>>|<(?:start|end)_of_turn>`)

// stripSpecialTokens removes special tokens, again until none is left (removing one must not join the
// halves of another).
func stripSpecialTokens(s string) string {
	for range 8 {
		if !strings.ContainsAny(s, "<[") {
			return s
		}
		out := specialTokenRE.ReplaceAllString(s, "")
		if out == s {
			return s
		}
		s = out
	}
	return s
}

// cleanUntrusted drops control characters and special tokens.
func cleanUntrusted(s string) string { return stripSpecialTokens(ctrlRE.ReplaceAllString(s, "")) }

// toolFenceRE matches the start of a tool result fence ([[tool_result …]] or [[/tool_result …]]) inside
// a result's text.
var toolFenceRE = regexp.MustCompile(`(?i)\[\[\s*/?\s*tool_result`)

// cleanToolText is one text of a tool result as a model reads it: cleaned, and a fence marker in it no
// longer one.
func cleanToolText(s string) string {
	return toolFenceRE.ReplaceAllStringFunc(cleanUntrusted(s), func(m string) string { return "［［" + m[2:] })
}

// newFenceNonce is a fresh random nonce for one turn's tool result fences.
func newFenceNonce() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// fenceToolResult wraps one tool result for the loop model between markers that carry the turn's nonce,
// so text in the result cannot write the closing marker.
func fenceToolResult(nonce, body string) string {
	return "[[tool_result " + nonce + "]]\n" + body + "\n[[/tool_result " + nonce + "]]"
}

// cleanToolData is data with every text in it (keys too) passed through cleanToolText. Data with
// nothing to clean is returned as it came, so its JSON stays byte for byte the same.
func cleanToolData(data map[string]any) map[string]any {
	if len(data) == 0 {
		return data
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return nil // what cannot be checked does not reach the prompt
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // numbers keep their exact text
	var generic any
	if dec.Decode(&generic) != nil {
		return data
	}
	changed := false
	var walk func(v any) any
	walk = func(v any) any {
		switch x := v.(type) {
		case string:
			if c := cleanToolText(x); c != x {
				changed = true
				return c
			}
			return x
		case []any:
			for i := range x {
				x[i] = walk(x[i])
			}
			return x
		case map[string]any:
			out := make(map[string]any, len(x))
			for k, val := range x {
				ck := cleanToolText(k)
				if ck != k {
					changed = true
				}
				out[ck] = walk(val)
			}
			return out
		}
		return v
	}
	cleaned, ok := walk(generic).(map[string]any)
	if !changed || !ok {
		return data
	}
	return cleaned
}

// cleanToolResults is results with their texts cleaned; results with nothing to clean are kept as they
// are.
func cleanToolResults(in []ToolResult) []ToolResult {
	out := make([]ToolResult, len(in))
	for i, r := range in {
		r.Error = cleanToolText(r.Error)
		r.Data = cleanToolData(r.Data)
		out[i] = r
	}
	return out
}

// InstructionLike reports a text that reads as an instruction to the bot: it holds one of the packs'
// instruction cues or a block delimiter of the prompt. Memory never carries instructions.
func InstructionLike(packs []*lang.Pack, text string) bool {
	t := strings.ToLower(text)
	for _, p := range packs {
		if p == nil {
			continue
		}
		for _, c := range p.Memory.InstructionCues {
			if c = strings.ToLower(strings.TrimSpace(c)); c != "" && strings.Contains(t, c) {
				return true
			}
		}
		for _, d := range p.Chat.Delimiters.All() {
			if d != "" && strings.Contains(text, d) {
				return true
			}
		}
	}
	return false
}

var (
	blockTagRE  = regexp.MustCompile(`</([^<>{}\n]+)>`)
	blockTagMu  sync.Mutex
	blockTagFor = map[*lang.Pack]*regexp.Regexp{}
)

// blockTags matches the opening of a block tag of the answer prompt (<memory>, </room_state>, their
// translations), in any case and spacing; nil when the pack has none.
func blockTags(p *lang.Pack) *regexp.Regexp {
	blockTagMu.Lock()
	defer blockTagMu.Unlock()
	if re, ok := blockTagFor[p]; ok {
		return re
	}
	var names []string
	seen := map[string]bool{}
	for _, tpl := range []string{p.Chat.AnswerTemplate, p.Memory.CardBlock, p.Memory.EpisodesBlock, p.Memory.StateBlock} {
		for _, m := range blockTagRE.FindAllStringSubmatch(tpl, -1) {
			fields := strings.Fields(m[1])
			if len(fields) == 0 {
				continue
			}
			for i, f := range fields {
				fields[i] = regexp.QuoteMeta(f)
			}
			if n := strings.Join(fields, `\s+`); !seen[n] {
				seen[n] = true
				names = append(names, n)
			}
		}
	}
	var re *regexp.Regexp
	if len(names) > 0 {
		// the tag itself: "<name" or "</name" ending there, not "x < memory limit" or "<memory_x"
		re = regexp.MustCompile(`(?i)</?(?:` + strings.Join(names, "|") + `)(?:[\s>/]|$)`)
	}
	blockTagFor[p] = re
	return re
}

// memoryText is one text of room memory as the answer prompt shows it: control characters and special
// tokens dropped, the prompt's delimiters and block tags neutralized.
func memoryText(v Voice, s string) string {
	out := sanitize(v, s)
	if re := blockTags(v.pack()); re != nil {
		out = re.ReplaceAllStringFunc(out, func(m string) string { return "＜" + m[1:] })
	}
	return out
}

// UntrustedText is a text another model wrote about something a member sent (a picture's description)
// as the answer prompt may show it: memoryText, so it cannot open a turn or close a block it sits in.
func UntrustedText(p *lang.Pack, s string) string { return memoryText(Voice{Pack: p}, s) }

// cleanRoomMemory is the room memory cleaned for the answer prompt (memoryText): no control characters,
// chat-template tokens or forged block delimiters anywhere, so quoted chat stays data. A card fact that
// reads as an instruction to the bot is left out: a card goes into every turn of its person, and another
// member cannot plant "from now on always say X" there. Episode evidence is what friends said and titles
// and topics name what they talked about; the instruction cues are broad ("규칙", "답해"), so those are
// kept, cleaned, inside their data blocks rather than dropped.
func cleanRoomMemory(v Voice, rm *RoomMemory) *RoomMemory {
	packs := rm.Packs
	if len(packs) == 0 {
		packs = []*lang.Pack{v.pack()}
	}
	fact := func(s string) (string, bool) {
		if InstructionLike(packs, s) {
			return "", false
		}
		c := memoryText(v, s)
		return c, !InstructionLike(packs, c)
	}
	out := &RoomMemory{Packs: rm.Packs}
	for _, e := range rm.Episodes {
		c := e
		c.Title, c.When = memoryText(v, e.Title), memoryText(v, e.When)
		c.Status, c.Source = memoryText(v, e.Status), memoryText(v, e.Source)
		c.Evidence = nil
		for _, l := range e.Evidence {
			if t := memoryText(v, l.Text); t != "" {
				c.Evidence = append(c.Evidence, RoomLine{Date: memoryText(v, l.Date), Author: memoryText(v, l.Author), Text: t})
			}
		}
		out.Episodes = append(out.Episodes, c)
	}
	for _, t := range rm.Topics {
		c := RoomTopic{Label: memoryText(v, t.Label), Gist: memoryText(v, t.Gist)}
		for _, w := range t.Who {
			c.Who = append(c.Who, memoryText(v, w))
		}
		out.Topics = append(out.Topics, c)
	}
	for _, card := range rm.Cards {
		c := RoomCard{Name: memoryText(v, card.Name), Caller: card.Caller}
		for _, f := range card.Facts {
			if t, ok := fact(f); ok {
				c.Facts = append(c.Facts, t)
			}
		}
		out.Cards = append(out.Cards, c)
	}
	return out
}
