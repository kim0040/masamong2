package discord

import (
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"
)

// Cards for web search and place search.

// renderSearch renders search.web.
func renderSearch(in renderInput, e *discordgo.MessageEmbed) Rendered {
	out, status, cmd, t := in.out, in.status, in.cmd, in.t
	e.Title = t("search.title", "query", fmt.Sprint(out["query"]))
	if status == "unconfigured" {
		e.Color, e.Description = colorWarn, t("search.unconfigured")
		return in.embedOnly(e)
	}
	results, _ := out["results"].([]any)
	if len(results) == 0 {
		e.Description = t("search.none")
		return in.embedOnly(e)
	}
	var row discordgo.ActionsRow
	for i, r := range results {
		if i >= 5 {
			break
		}
		m, _ := r.(map[string]any)
		title := truncate(fmt.Sprint(m["title"]), 256)
		url := strings.TrimSpace(fmt.Sprint(m["url"]))
		e.Fields = append(e.Fields, &discordgo.MessageEmbedField{
			Name:  title,
			Value: truncate(fmt.Sprint(m["snippet"]), 1024),
		})
		if url != "" && strings.HasPrefix(url, "http") && len(row.Components) < 5 {
			row.Components = append(row.Components, discordgo.Button{
				Label: truncate(title, 80),
				Style: discordgo.LinkButton,
				URL:   url,
			})
		}
	}
	if len(row.Components) > 0 {
		return Rendered{Embed: e, Components: []discordgo.MessageComponent{row}, Ephemeral: cmd.Ephemeral}
	}
	return in.embedOnly(e)
}

// renderPlaces renders places.search.
func renderPlaces(in renderInput, e *discordgo.MessageEmbed) Rendered {
	out, status, cmd, t := in.out, in.status, in.cmd, in.t
	e.Title = t("places.title", "query", fmt.Sprint(out["query"]))
	if status == "unconfigured" {
		e.Color, e.Description = colorWarn, t("places.unconfigured")
		return in.embedOnly(e)
	}
	places, _ := out["places"].([]map[string]any)
	if places == nil {
		for _, p := range func() []any { l, _ := out["places"].([]any); return l }() {
			if m, ok := p.(map[string]any); ok {
				places = append(places, m)
			}
		}
	}
	if len(places) == 0 {
		e.Description = t("places.none")
		return in.embedOnly(e)
	}
	var row discordgo.ActionsRow
	for i, m := range places {
		if i >= 5 {
			break
		}
		name := truncate(fmt.Sprint(m["name"]), 256)
		lines := []string{fmt.Sprint(m["address"])}
		if c, _ := m["category"].(string); c != "" {
			lines = append(lines, c)
		}
		if ph, _ := m["phone"].(string); ph != "" {
			lines = append(lines, ph)
		}
		e.Fields = append(e.Fields, &discordgo.MessageEmbedField{Name: name, Value: truncate(strings.Join(lines, "\n"), 1024)})
		if u, _ := m["url"].(string); strings.HasPrefix(u, "http") && len(row.Components) < 5 {
			row.Components = append(row.Components, discordgo.Button{Label: truncate(name, 80), Style: discordgo.LinkButton, URL: u})
		}
	}
	if len(row.Components) > 0 {
		return Rendered{Embed: e, Components: []discordgo.MessageComponent{row}, Ephemeral: cmd.Ephemeral}
	}
	return in.embedOnly(e)
}
