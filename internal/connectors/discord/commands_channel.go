package discord

import (
	"fmt"
	"strings"
)

// Builders of the commands that make something in a channel: polls, pictures and summaries.

// buildPoll maps the poll command's options to its Action input.
func buildPoll(o map[string]string, c InvokeContext) (string, map[string]any) {
	raw := o["answers"]
	if raw == "" {
		raw = o["options"]
	}
	var opts []any
	for i, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			opts = append(opts, map[string]any{"id": fmt.Sprintf("o%d", i+1), "label": part})
		}
	}
	hours := 24
	fmt.Sscanf(o["duration"], "%d", &hours)
	multi := strings.EqualFold(strings.TrimSpace(o["multiselect"]), "true")
	in := map[string]any{
		"question":          o["question"],
		"options":           opts,
		"duration_hours":    hours,
		"allow_multiselect": multi,
		"scope":             "discord:" + c.ChannelID,
	}
	return "polls.create", in
}

// buildImage maps the image command's options to its Action input.
func buildImage(o map[string]string, c InvokeContext) (string, map[string]any) {
	return "images.generate", map[string]any{"prompt": o["prompt"], "scope": "discord:" + c.ChannelID}
}

// buildSummary maps the summary command's options to its Action input.
func buildSummary(o map[string]string, c InvokeContext) (string, map[string]any) {
	return "summaries.channel", map[string]any{}
}
