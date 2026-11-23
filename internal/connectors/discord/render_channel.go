package discord

import (
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"

	"masamong/internal/i18n"
)

// Cards for what happens in a channel: the activity ranking, polls and drawn pictures.

// renderRanking renders activity.ranking.
func renderRanking(in renderInput, e *discordgo.MessageEmbed) Rendered {
	out, t := in.out, in.t
	e.Title = t("ranking.title." + fmt.Sprint(out["period"]))
	top, _ := out["top"].([]any)
	if len(top) == 0 {
		if l, ok := out["top"].([]map[string]any); ok {
			for _, m := range l {
				top = append(top, m)
			}
		}
	}
	if len(top) == 0 {
		e.Description = t("ranking.none")
		return in.embedOnly(e)
	}
	medals := []string{"🥇", "🥈", "🥉"}
	var lines []string
	for i, r := range top {
		m, _ := r.(map[string]any)
		mark := fmt.Sprintf("%d.", i+1)
		if i < len(medals) {
			mark = medals[i]
		}
		lines = append(lines, t("ranking.line", "mark", mark, "name", fmt.Sprint(m["name"]), "count", fmt.Sprint(m["count"]), "share", fmt.Sprint(m["share"])))
	}
	e.Description = strings.Join(lines, "\n")
	e.Footer = &discordgo.MessageEmbedFooter{Text: t("ranking.footer", "total", fmt.Sprint(out["total_messages"]), "people", fmt.Sprint(out["people"]))}
	return in.embedOnly(e)
}

// renderPollCreate renders polls.create.
func renderPollCreate(in renderInput, e *discordgo.MessageEmbed) Rendered {
	out, locale, t := in.out, in.locale, in.t
	p := nativeDiscordPoll(in.cat, out, locale)
	e.Title = t("poll.posted")
	e.Description = t("poll.how")
	if closes, _ := out["closes_at"].(string); closes != "" {
		e.Fields = []*discordgo.MessageEmbedField{{Name: t("poll.closes"), Value: discordTime(closes), Inline: true}}
	}
	e.Footer = &discordgo.MessageEmbedFooter{Text: t("poll.label", "id", out["id"])}
	return Rendered{Embed: e, Poll: p}
}

// renderPollVote renders polls.vote.
func renderPollVote(in renderInput, e *discordgo.MessageEmbed) Rendered {
	out, t := in.out, in.t
	e.Title = t("poll.voted")
	e.Description = t("poll.revision", "n", out["revision"])
	return Rendered{Embed: e, Ephemeral: true}
}

// renderPollClose renders polls.close.
func renderPollClose(in renderInput, e *discordgo.MessageEmbed) Rendered {
	t := in.t
	e.Title = t("poll.closed")
	e.Color = colorNeutral
	return in.embedOnly(e)
}

// renderPollResult renders polls.result.
func renderPollResult(in renderInput, e *discordgo.MessageEmbed) Rendered {
	out, locale, t := in.out, in.locale, in.t
	e.Title = "📊 " + fmt.Sprint(out["question"])
	by, _ := out["by_option"].([]any)
	var lines []string
	for _, row := range by {
		m, _ := row.(map[string]any)
		label := fmt.Sprint(m["label"])
		if label == "" || label == "<nil>" {
			label = fmt.Sprint(m["option_id"])
		}
		lines = append(lines, fmt.Sprintf("%s — %v", label, toInt(m["votes"])))
	}
	lines = append(lines, "", t("poll.voters", "n", out["unique_voters"], "status", pollStatus(in.cat, fmt.Sprint(out["status"]), locale)))
	if closes, _ := out["closes_at"].(string); closes != "" {
		lines = append(lines, t("poll.closes_at", "when", discordTime(closes)))
	}
	e.Description = strings.Join(lines, "\n")
	return in.embedOnly(e)
}

// renderImage renders images.generate.
func renderImage(in renderInput, e *discordgo.MessageEmbed) Rendered {
	out, status, t := in.out, in.status, in.t
	e.Title = t("image.title")
	if status == "not_implemented" || status == "unconfigured" {
		e.Color, e.Description = colorWarn, t("image.unconfigured")
		return in.embedOnly(e)
	}
	e.Description = truncate(fmt.Sprint(out["url"]), 1000)
	return in.embedOnly(e)
}

func pollStatus(cat *i18n.Catalog, s, locale string) string {
	if v, ok := cat.Lookup(locale, "discord.render.poll.status."+s); ok {
		return v
	}
	return s
}

func nativeDiscordPoll(cat *i18n.Catalog, out map[string]any, locale string) *discordgo.Poll {
	q := truncate(fmt.Sprint(out["question"]), 300)
	hours := toInt(out["duration_hours"])
	if hours < 1 {
		hours = 24
	}
	if hours > 768 {
		hours = 768
	}
	multi, _ := out["allow_multiselect"].(bool)
	if !multi && toInt(out["max_select"]) > 1 {
		multi = true
	}
	emojis := []string{"1️⃣", "2️⃣", "3️⃣", "4️⃣", "5️⃣", "6️⃣", "7️⃣", "8️⃣", "9️⃣", "🔟"}
	var answers []discordgo.PollAnswer
	if opts, ok := out["options"].([]any); ok {
		for i, o := range opts {
			if i >= 10 {
				break
			}
			m, _ := o.(map[string]any)
			label := truncate(fmt.Sprint(m["label"]), 55)
			ans := discordgo.PollAnswer{Media: &discordgo.PollMedia{Text: label}}
			if i < len(emojis) {
				ans.Media.Emoji = &discordgo.ComponentEmoji{Name: emojis[i]}
			}
			answers = append(answers, ans)
		}
	}
	if len(answers) < 2 {
		yes, no := cat.T(locale, "poll.yes"), cat.T(locale, "poll.no")
		answers = []discordgo.PollAnswer{
			{Media: &discordgo.PollMedia{Text: yes, Emoji: &discordgo.ComponentEmoji{Name: "👍"}}},
			{Media: &discordgo.PollMedia{Text: no, Emoji: &discordgo.ComponentEmoji{Name: "👎"}}},
		}
	}
	return &discordgo.Poll{
		Question:         discordgo.PollMedia{Text: q},
		Answers:          answers,
		Duration:         hours,
		AllowMultiselect: multi,
		LayoutType:       discordgo.PollLayoutTypeDefault,
	}
}
