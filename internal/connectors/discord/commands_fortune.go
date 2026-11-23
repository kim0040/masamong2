package discord

import (
	"strings"

	"masamong/internal/i18n"
)

// Builders of the fortune commands.

// buildFortune maps the fortune command's options to its Action input.
func buildFortune(o map[string]string, c InvokeContext) (string, map[string]any) {
	verb, rest := splitVerb(o["what"], o["time"], wordsFor(c.words, "fortune", "verb",
		"today", "detail", "month", "year", "animals", "status", "subscribe", "unsubscribe", "delete", "register"))
	return fortuneRequest(c.words, verb, rest)
}

// buildFortuneRegister maps the fortune-register command's options to its Action input.
func buildFortuneRegister(o map[string]string, c InvokeContext) (string, map[string]any) {
	in := map[string]any{"op": "register"}
	for _, k := range []string{"birth_date", "birth_time", "gender", "birth_place"} {
		if v := strings.TrimSpace(o[k]); v != "" {
			in[k] = v
		}
	}
	return "fortune.subscribe", in
}

// buildFortuneMonth maps the fortune-month command's options to its Action input.
func buildFortuneMonth(o map[string]string, c InvokeContext) (string, map[string]any) {
	return "fortune.query", map[string]any{"what": "month"}
}

// buildFortuneYear maps the fortune-year command's options to its Action input.
func buildFortuneYear(o map[string]string, c InvokeContext) (string, map[string]any) {
	return "fortune.query", map[string]any{"what": "year"}
}

// buildSubscribe maps the subscribe command's options to its Action input.
func buildSubscribe(o map[string]string, c InvokeContext) (string, map[string]any) {
	return fortuneRequest(c.words, "subscribe", o["time"])
}

// buildZodiac maps the zodiac command's options to its Action input.
func buildZodiac(o map[string]string, c InvokeContext) (string, map[string]any) {
	sign := strings.TrimSpace(o["sign"])
	if wordsFor(c.words, "zodiac", "verb", "ranking")[strings.ToLower(sign)] == "ranking" {
		return "fortune.query", map[string]any{"what": "ranking"}
	}
	return "fortune.query", map[string]any{"what": "zodiac", "sign": sign}
}

// fortuneRequest maps a fortune verb (and what follows it) to its Action.
func fortuneRequest(cat *i18n.Catalog, verb, rest string) (string, map[string]any) {
	rest = strings.TrimSpace(rest)
	switch verb {
	case "", "today", "detail", "month", "year", "animals": // animals: the twelve 띠, the same for everyone
		if verb == "" {
			verb = "today"
		}
		return "fortune.query", map[string]any{"what": verb}
	case "subscribe":
		if rest == "" || wordsFor(cat, "fortune", "value", "off")[strings.ToLower(rest)] == "off" {
			if rest != "" {
				return "fortune.subscribe", map[string]any{"op": "unsubscribe"}
			}
			return "fortune.subscribe", map[string]any{"op": "status"}
		}
		return "fortune.subscribe", map[string]any{"op": "subscribe", "time": rest}
	case "register":
		return "fortune.subscribe", map[string]any{"op": "register", "text": rest}
	}
	return "fortune.subscribe", map[string]any{"op": verb}
}
