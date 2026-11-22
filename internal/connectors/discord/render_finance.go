package discord

import (
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"
)

// Cards for stock quotes and exchange rates.

// renderQuote renders finance.quote.
func renderQuote(in renderInput, e *discordgo.MessageEmbed) Rendered {
	out, status, t := in.out, in.status, in.t
	sym := fmt.Sprint(out["symbol"])
	e.Title = sym
	if status == "unconfigured" {
		e.Color, e.Description = colorWarn, t("quote.unconfigured")
		return in.embedOnly(e)
	}
	e.Description = fmt.Sprintf("%v %v (%v%%)", out["price"], out["currency"], out["change_pct"])
	if krw, ok := out["price_krw"]; ok {
		e.Description += fmt.Sprintf("\n≈ ₩%v", krw)
	}
	if status == "partial" {
		e.Color = colorWarn
		e.Description += "\n" + t("quote.fx_missing")
	}
	if at, _ := out["at"].(string); at != "" {
		e.Fields = []*discordgo.MessageEmbedField{{Name: t("quote.as_of"), Value: discordTime(at), Inline: true}}
		e.Footer = &discordgo.MessageEmbedFooter{Text: "Finnhub"}
	}
	return in.embedOnly(e)
}

// renderFX renders finance.fx.
func renderFX(in renderInput, e *discordgo.MessageEmbed) Rendered {
	out, status, t := in.out, in.status, in.t
	base, _ := out["base"].(string)
	e.Title = t("fx.title", "base", base)
	if status == "unconfigured" {
		e.Color, e.Description = colorWarn, t("fx.unconfigured")
		return in.embedOnly(e)
	}
	// the action states each rate in words ("100엔 ≈ 930.2원"); the card only lists them
	var lines []string
	switch quotes := out["quotes"].(type) {
	case []map[string]any:
		for _, q := range quotes {
			lines = append(lines, fmt.Sprint(q["text"]))
		}
	case []any:
		for _, q := range quotes {
			if m, ok := q.(map[string]any); ok {
				lines = append(lines, fmt.Sprint(m["text"]))
			}
		}
	}
	e.Description = strings.Join(lines, "\n")
	if e.Description == "" {
		e.Description = t("fx.no_rates")
	}
	if at, _ := out["updated_at"].(string); at != "" {
		e.Fields = []*discordgo.MessageEmbedField{{Name: t("fx.updated"), Value: discordTime(at), Inline: true}}
		e.Footer = &discordgo.MessageEmbedFooter{Text: fmt.Sprint(out["source"])}
	}
	return in.embedOnly(e)
}
