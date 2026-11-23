package discord

import (
	"strconv"
	"strings"

	"masamong/internal/i18n"
)

// Builders of the lookup commands: weather, alerts, quotes, exchange rates, search, ranking and places.

// buildWeather maps the weather command's options to its Action input.
func buildWeather(o map[string]string, c InvokeContext) (string, map[string]any) {
	in := map[string]any{}
	if r := strings.TrimSpace(o["region"]); r != "" {
		in["region"] = r
	}
	return "weather.query", in
}

// buildAlerts maps the alerts command's options to its Action input.
func buildAlerts(o map[string]string, c InvokeContext) (string, map[string]any) {
	return "disasters.query", map[string]any{}
}

// buildQuote maps the quote command's options to its Action input.
func buildQuote(o map[string]string, c InvokeContext) (string, map[string]any) {
	return "finance.quote", map[string]any{"symbol": o["symbol"], "krw": true}
}

// buildFX maps the fx command's options to its Action input.
func buildFX(o map[string]string, c InvokeContext) (string, map[string]any) {
	// what was typed goes to the action as it is: a given option, even a blank one, is checked there.
	// The blank home is the home currency of the request's language, which the action knows.
	// The base is sent even when nothing was typed: set, a configured tool default cannot be
	// merged in as if the person had asked for it, and blank is the home default anyway.
	in := map[string]any{"home": "", "base": ""}
	if c.Tag != "" {
		in["locale_tag"] = c.Tag
	}
	if v, given := o["currency"]; given {
		in["currency"] = v
	}
	if v, given := o["base"]; given {
		in["base"] = v
	}
	if v, given := o["amount"]; given {
		// a number option is already a number; a word of a message is the person's text, read
		// in their locale's syntax by the action
		if n, err := strconv.ParseFloat(v, 64); err == nil && !c.Typed {
			in["amount"] = n
		} else {
			in["amount"] = v
		}
	}
	return "finance.fx", in
}

// buildSearch maps the search command's options to its Action input.
func buildSearch(o map[string]string, c InvokeContext) (string, map[string]any) {
	return "search.web", map[string]any{"query": o["query"], "size": 3}
}

// buildRanking maps the ranking command's options to its Action input.
func buildRanking(o map[string]string, c InvokeContext) (string, map[string]any) {
	return "activity.ranking", map[string]any{"period": rankingPeriod(c.words, o["period"]), "scope": "discord:" + c.ChannelID}
}

// rankingPeriod reads the period people type after !랭킹 (오늘, 이번주, 이번달, 전체 …): the words of every
// language are discord.command.ranking.period.<period>. Spaces and case do not count; anything else is the
// default week (the option's description says so).
func rankingPeriod(cat *i18n.Catalog, v string) string {
	if p, ok := wordsFor(cat, "ranking", "period", "today", "week", "month", "all")[strings.ReplaceAll(strings.ToLower(strings.TrimSpace(v)), " ", "")]; ok {
		return p
	}
	return "week"
}

// buildPlaces maps the places command's options to its Action input.
func buildPlaces(o map[string]string, c InvokeContext) (string, map[string]any) {
	return "places.search", map[string]any{"query": o["query"], "size": 5}
}
