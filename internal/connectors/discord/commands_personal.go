package discord

import (
	"strings"
)

// Builders of a member's own commands: settings, update news, consent, reminders and memory.

// buildMysettings maps the mysettings command's options to its Action input.
func buildMysettings(o map[string]string, c InvokeContext) (string, map[string]any) {
	return "preferences.set", settingsInput(o, c.Typed)
}

// settingsInput turns /mysettings options (or "!내설정 지역 서울" words) into preferences.set input.
func settingsInput(o map[string]string, typed bool) map[string]any {
	in := map[string]any{}
	// only typed words ("!내설정 초기화") carry a verb: the slash command's region option is a place, so
	// region:"초기화" is not a reset of every setting
	if typed && o["timezone"] == "" && o["quiet"] == "" && o["language"] == "" {
		// prefix form: everything is in the first option
		verb, rest := splitVerb(o["region"], "", wordsFor("mysettings", "verb", "region", "timezone", "quiet", "language", "reset"))
		switch verb {
		case "reset":
			return map[string]any{"reset": true}
		case "":
			if rest != "" {
				o = map[string]string{"region": rest}
			} else {
				return in
			}
		default:
			o = map[string]string{verb: rest}
		}
	}
	if v := strings.TrimSpace(o["region"]); v != "" {
		in["weather_region"] = v
	}
	if v := strings.TrimSpace(o["timezone"]); v != "" {
		in["timezone"] = v
	}
	if v := strings.TrimSpace(o["quiet"]); v != "" {
		if a, b, ok := strings.Cut(strings.ReplaceAll(v, "~", "-"), "-"); ok {
			in["quiet_start"], in["quiet_end"] = strings.TrimSpace(a), strings.TrimSpace(b)
		} else {
			in["quiet_start"], in["quiet_end"] = "", ""
		}
	}
	switch v := strings.TrimSpace(o["language"]); {
	case v == "":
	case wordsFor("mysettings", "value", "default")[strings.ToLower(v)] == "default":
		in["locale"] = ""
	default:
		in["locale"] = v
	}
	return in
}

// buildUpdates maps the updates command's options to its Action input.
func buildUpdates(o map[string]string, c InvokeContext) (string, map[string]any) {
	verb, rest := splitVerb(o["what"], o["value"], wordsFor("updates", "verb", "now", "previous", "subscribe", "unsubscribe", "status"))
	return updatesRequest(verb, rest)
}

// updatesRequest maps an updates verb (and what follows it) to its Action: the notes of this version,
// of the one before it, or of one named; and the person's own DM subscription.
func updatesRequest(verb, rest string) (string, map[string]any) {
	rest = strings.TrimSpace(rest)
	switch verb {
	case "subscribe":
		return "updates.subscribe", map[string]any{"op": "on"}
	case "unsubscribe":
		return "updates.subscribe", map[string]any{"op": "off"}
	case "status":
		return "updates.subscribe", map[string]any{"op": "status"}
	case "previous":
		in := map[string]any{"previous": true}
		if rest != "" {
			in["version"] = rest // the one before the version named
		}
		return "updates.notes", in
	}
	in := map[string]any{}
	if rest != "" {
		in["version"] = rest
	}
	return "updates.notes", in
}

// buildPrivacy maps the privacy command's options to its Action input.
func buildPrivacy(o map[string]string, c InvokeContext) (string, map[string]any) {
	verb, scope := splitVerb(o["action"], o["scope"], wordsFor("privacy", "verb", "status", "grant", "withdraw"))
	in := map[string]any{}
	if scope != "" {
		in["scope"] = scope
	}
	if verb == "grant" || verb == "withdraw" {
		in["decision"] = verb
	}
	return "privacy.consent", in
}

// buildRemind maps the remind command's options to its Action input.
func buildRemind(o map[string]string, c InvokeContext) (string, map[string]any) {
	// the weekday of a weekly reminder and the day of a monthly one come from the time given
	in := map[string]any{"text": o["when"]}
	if what := strings.TrimSpace(o["what"]); what != "" {
		in = map[string]any{"when": o["when"], "what": what}
	}
	if rep := o["repeat"]; rep != "" {
		in["repeat"] = rep
	}
	return "reminders.propose", in
}

// buildReminders maps the reminders command's options to its Action input.
func buildReminders(o map[string]string, c InvokeContext) (string, map[string]any) {
	return "reminders.update", map[string]any{"op": "list"}
}

// buildMymemory maps the mymemory command's options to its Action input.
func buildMymemory(o map[string]string, c InvokeContext) (string, map[string]any) {
	return "memory.mine.show", map[string]any{}
}

// buildForgetme maps the forgetme command's options to its Action input.
func buildForgetme(o map[string]string, c InvokeContext) (string, map[string]any) {
	return "memory.mine.forget", map[string]any{}
}
