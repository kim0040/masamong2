package discord

import (
	"fmt"
	"sort"
	"strings"

	"github.com/bwmarrin/discordgo"
)

// Cards about the bot itself: its status and the command list.

// renderDoctor renders runtime.doctor.
func renderDoctor(in renderInput, e *discordgo.MessageEmbed) Rendered {
	out, locale, t := in.out, in.locale, in.t
	e.Title = t("status.title")
	feats, _ := out["features"].(map[string]any)
	keys := make([]string, 0, len(feats))
	for k := range feats {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var lines []string
	for _, k := range keys {
		m, _ := feats[k].(map[string]any)
		name := k
		if messages.Has("feature.name." + k) {
			name = messages.T(locale, "feature.name."+k)
		}
		line := fmt.Sprintf("%s %s", statusIcon(fmt.Sprint(m["status"])), name)
		if why, _ := m["reason_text"].(string); why != "" && m["status"] != "ready" {
			line += " — " + why
		}
		lines = append(lines, line)
	}
	e.Description = strings.Join(lines, "\n")
	e.Footer = &discordgo.MessageEmbedFooter{Text: fmt.Sprintf("v%v · rev %v", out["version"], out["revision"])}
	return Rendered{Embed: e, Ephemeral: true}
}

// RenderHelp renders only the command surfaces supplied by the live gateway plus the message menu
// action that is always registered.
func RenderHelp(locale string, slash, prefix []string) Rendered {
	t := func(key string) string { return messages.T(locale, "discord.render.help."+key) }
	var lines []string
	if len(slash) > 0 {
		lines = append(lines, "**"+t("slash")+"**")
		lines = append(lines, slash...)
	}
	if len(prefix) > 0 {
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, "**"+t("prefixes")+"**")
		lines = append(lines, prefix...)
	}
	if len(lines) > 0 {
		lines = append(lines, "")
	}
	lines = append(lines, "**"+t("message")+"**", messages.T(locale, "discord.feedback.command"))
	return Rendered{Embed: &discordgo.MessageEmbed{Title: t("title"), Color: colorNeutral, Description: strings.Join(lines, "\n")}, Ephemeral: true}
}

func statusIcon(s string) string {
	switch s {
	case "ready":
		return "🟢"
	case "disabled":
		return "⚪"
	case "unconfigured", "pending":
		return "🟡"
	case "error":
		return "🔴"
	}
	return "⚫"
}
