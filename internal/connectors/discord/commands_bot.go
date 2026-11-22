package discord

import (
	"strings"
)

// Builders of the commands that run the bot: the managers' admin command, status and help.

// buildAdmin maps the admin command's options to its Action input.
func buildAdmin(o map[string]string, c InvokeContext) (string, map[string]any) {
	verb, rest := splitVerb(o["op"], o["mode"], wordsFor("admin", "verb", "status", "pause", "resume", "respond", "memory", "chime", "news", "block", "unblock"))
	if verb == "" {
		verb = "status"
	}
	in := map[string]any{"op": verb}
	switch verb {
	case "respond", "memory", "chime", "news":
		mode := wordsFor("admin", "mode", "always", "mention", "command", "off", "on", "collect", "shadow", "conservative")[strings.ToLower(rest)]
		if mode == "" {
			mode = rest
		}
		if verb == "chime" && mode == "on" {
			mode = "conservative" // "chiming in on" means it may speak
		}
		in["mode"] = mode
	case "block", "unblock":
		in["user"] = orDefault(o["user"], rest)
	}
	return "access.policy", in
}

// buildStatus maps the status command's options to its Action input.
func buildStatus(o map[string]string, c InvokeContext) (string, map[string]any) {
	return "runtime.doctor", map[string]any{}
}

// buildHelp maps the help command's options to its Action input.
func buildHelp(o map[string]string, c InvokeContext) (string, map[string]any) {
	return "help.version", map[string]any{}
}
