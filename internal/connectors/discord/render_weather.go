package discord

import (
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"
)

// Cards for weather and disaster alerts.

// renderWeather renders weather.query.
func renderWeather(in renderInput, e *discordgo.MessageEmbed) Rendered {
	out, status, live, locale, t := in.out, in.status, in.live, in.locale, in.t
	region := fmt.Sprint(out["region"])
	e.Title = t("weather.title", "region", region)
	switch status {
	case "unconfigured":
		e.Color, e.Description = colorWarn, t("weather.unconfigured")
	case "empty":
		e.Color, e.Description = colorWarn, t("weather.empty")
	default:
		var lines []string
		if c, _ := out["condition"].(string); c != "" {
			lines = append(lines, "🌤 "+t("weather.cond."+c))
		}
		if v, ok := out["temp_c"]; ok && v != nil {
			line := fmt.Sprintf("🌡 %s %.1f°C", t("weather.temperature"), toF(v))
			if f, ok := out["feels_like_c"]; ok && f != nil {
				line += fmt.Sprintf(" (%s %.1f°C)", t("weather.feels"), toF(f))
			}
			lines = append(lines, line)
		}
		if v, ok := out["humidity_pct"]; ok && v != nil {
			lines = append(lines, fmt.Sprintf("💧 %s %.0f%%", t("weather.humidity"), toF(v)))
		}
		if v, ok := out["wind_ms"]; ok && v != nil {
			lines = append(lines, fmt.Sprintf("🍃 %s %.1f m/s", t("weather.wind"), toF(v)))
		}
		if p, _ := out["precip"].(string); p != "" && p != "none" && p != "unknown" {
			lines = append(lines, "☔ "+t("weather.precipitation", "value", precipText(p, locale)))
		}
		if fc, ok := out["forecast"].(map[string]any); ok {
			var f []string
			if fc["max_c"] != nil && fc["min_c"] != nil {
				f = append(f, t("weather.range", "max", fmt.Sprintf("%.0f", toF(fc["max_c"])), "min", fmt.Sprintf("%.0f", toF(fc["min_c"]))))
			}
			if fc["precip_prob_pct"] != nil {
				f = append(f, t("weather.rain_chance", "value", fmt.Sprintf("%.0f", toF(fc["precip_prob_pct"]))))
			}
			if c, _ := fc["condition"].(string); c != "" {
				f = append([]string{t("weather.cond." + c)}, f...)
			}
			if len(f) > 0 {
				title := "📅 " + strings.Join(f, " · ")
				if day, _ := out["day_offset"].(int); day > 0 {
					title = "📅 " + t("weather.forecast_title", "date", fmt.Sprint(fc["date"])) + " — " + strings.Join(f, " · ")
				}
				lines = append(lines, title)
			}
		}
		e.Description = strings.Join(lines, "\n")
		if s, _ := out["source"].(string); s == "fixture" {
			e.Color = colorNeutral
			e.Description += "\n\n" + t("weather.sample")
		}
	}
	if at, _ := out["observed_at"].(string); at != "" && live {
		// Timestamp markdown is parsed in description/fields, not footers.
		e.Fields = append(e.Fields, &discordgo.MessageEmbedField{
			Name: t("weather.observed"), Value: discordTime(at), Inline: true,
		})
		footer := "KMA"
		if src, _ := out["source"].(string); src == "open_meteo" {
			footer = "Open-Meteo.com · CC BY 4.0" // the free API asks for credit under CC BY 4.0
		}
		e.Footer = &discordgo.MessageEmbedFooter{Text: footer}
	}
	return in.embedOnly(e)
}

// renderDisasters renders disasters.query.
func renderDisasters(in renderInput, e *discordgo.MessageEmbed) Rendered {
	out, status, t := in.out, in.status, in.t
	e.Title = t("alerts.title")
	switch status {
	case "unconfigured":
		e.Color, e.Description = colorWarn, t("alerts.unconfigured")
	case "unconfirmed":
		e.Color, e.Description = colorWarn, t("alerts.unconfirmed")
	case "none":
		e.Description = t("alerts.none")
	default:
		e.Color = colorWarn
		var lines []string
		if ws, ok := out["warnings"].([]any); ok {
			for i, w := range ws {
				if i >= 8 {
					lines = append(lines, t("alerts.more", "n", len(ws)-8))
					break
				}
				m, _ := w.(map[string]any)
				lines = append(lines, fmt.Sprintf("⚠️ %v: %v %v", m["region"], m["kind"], m["level"]))
			}
		}
		if qs, ok := out["earthquakes"].([]any); ok {
			for i, q := range qs {
				if i >= 5 {
					break
				}
				m, _ := q.(map[string]any)
				mag := "?"
				if v, ok := m["magnitude"]; ok && v != nil {
					mag = fmt.Sprintf("%.1f", toF(v))
				}
				when := discordTime(fmt.Sprint(m["time"]))
				lines = append(lines, fmt.Sprintf("🌐 M%s · %v · %s", mag, m["location"], when))
			}
		}
		e.Description = strings.Join(lines, "\n")
	}
	return in.embedOnly(e)
}

func precipText(p, locale string) string {
	if v, ok := messages.Lookup(locale, "discord.render.precip."+p); ok {
		return v
	}
	return p
}
