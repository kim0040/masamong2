package discord

import (
	"strconv"
	"strings"
)

// Builders of the school notice and transfer notice commands.

// buildNotices maps the notices command's options to its Action input.
func buildNotices(o map[string]string, c InvokeContext) (string, map[string]any) {
	verb, rest := splitVerb(o["what"], o["value"], wordsFor(c.words, "notices", "verb",
		"latest", "profile", "status", "time", "pause", "resume", "delete", "mute", "unmute", "register"))
	return noticeRequest("school", verb, rest)
}

// buildNoticesRegister maps the notices-register command's options to its Action input.
func buildNoticesRegister(o map[string]string, c InvokeContext) (string, map[string]any) {
	in := map[string]any{"kind": "school", "op": "register"}
	for _, k := range []string{"school", "degree", "department", "campus", "interests", "time"} {
		if v := strings.TrimSpace(o[k]); v != "" {
			in[k] = v
		}
	}
	if g, err := strconv.Atoi(strings.TrimSpace(o["grade"])); err == nil {
		in["grade"] = g
	}
	return "notices.subscribe", in
}

// buildTransfer maps the transfer command's options to its Action input.
func buildTransfer(o map[string]string, c InvokeContext) (string, map[string]any) {
	verb, rest := splitVerb(o["what"], o["value"], wordsFor(c.words, "transfer", "verb",
		"recent", "subscribe", "status", "time", "pause", "resume", "delete"))
	if verb == "" || verb == "recent" {
		in := map[string]any{}
		if rest != "" {
			in["universities"] = rest
		}
		return "transfer.recent", in
	}
	return noticeRequest("transfer", verb, rest)
}

// noticeRequest maps a notices or transfer verb (and what follows it) to its Action.
func noticeRequest(kind, verb, rest string) (string, map[string]any) {
	rest = strings.TrimSpace(rest)
	switch verb {
	case "", "latest":
		return "notices.status", map[string]any{"kind": kind, "what": "latest"}
	case "profile", "status":
		if kind == "transfer" && verb == "status" {
			return "notices.status", map[string]any{"kind": kind, "what": "profile"}
		}
		return "notices.status", map[string]any{"kind": kind, "what": verb}
	case "register", "subscribe":
		in := map[string]any{"kind": kind, "op": verb}
		if kind == "transfer" {
			in["universities"] = rest
		} else {
			in["text"] = rest
		}
		return "notices.subscribe", in
	case "time":
		return "notices.subscribe", map[string]any{"kind": kind, "op": "time", "time": rest}
	case "mute", "unmute":
		return "notices.subscribe", map[string]any{"kind": kind, "op": verb, "topic": rest}
	}
	return "notices.subscribe", map[string]any{"kind": kind, "op": verb}
}
