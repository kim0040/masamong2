package application

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"masamong/internal/action"
	"masamong/internal/config"
	"masamong/internal/errcode"
	"masamong/internal/providers"
)

// discordAPI is where discord.check asks; tests point it at a local server.
const discordAPIDefault = "https://discord.com/api/v10"

// Application flags (discord.com/developers/docs/resources/application#application-object-application-flags,
// read 2026-10-01): the Message Content intent switched on in the Developer Portal, for bots in 100+
// servers or fewer.
const (
	flagMessageContent        = 1 << 18
	flagMessageContentLimited = 1 << 19
)

// discordCheck asks Discord who the stored bot token belongs to (GET /applications/@me) so the operator
// never copies ids from the Developer Portal: the bot id (the invite link and the wrong-token guard use
// it), the app's name, and whether the Message Content intent is on there. apply=true stores the bot id.
// It also says when config asks for message content the portal does not grant: the gateway would
// refuse to connect (close code 4014).
func (s *Service) discordCheck(ctx context.Context, req action.Request) action.Result {
	in := inputMap(req)
	doc := s.Doc()
	cred, err := providers.Resolve(doc.Discord.TokenRef)
	if err != nil || !cred.Present() {
		return action.Fail(req, errcode.ProviderUnconfigured, "generic.error", "no Discord bot token stored yet", map[string]any{"reason": "no_token"})
	}
	base := s.discordAPI
	if base == "" {
		base = discordAPIDefault
	}
	c := providers.NewClient(15*time.Second, s.Log)
	c.MaxRetries = 1
	resp, err := c.Get(ctx, base+"/applications/@me", map[string]string{"Authorization": "Bot " + cred.Value()})
	if err != nil {
		var pe *providers.Error
		if errors.As(err, &pe) && (pe.Status == 401 || pe.Status == 403) {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "Discord refused the token: reset it in the Developer Portal (Bot → Reset Token) and paste the new one", map[string]any{"reason": "token_rejected"})
		}
		return action.Fail(req, errcode.ProviderUnavailable, "generic.error", "Discord could not be reached: "+err.Error(), map[string]any{"reason": "unreachable"})
	}
	var app struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Flags int64  `json:"flags"`
		Bot   *struct {
			ID       string `json:"id"`
			Username string `json:"username"`
		} `json:"bot"`
	}
	if err := json.Unmarshal(resp.Body, &app); err != nil || app.ID == "" {
		return action.Fail(req, errcode.ProviderUnavailable, "generic.error", "unexpected reply from Discord", map[string]any{"reason": "bad_reply"})
	}
	botID, botName := app.ID, app.Name
	if app.Bot != nil && app.Bot.ID != "" {
		botID, botName = app.Bot.ID, app.Bot.Username
	}
	portal := app.Flags&(flagMessageContent|flagMessageContentLimited) != 0
	out := map[string]any{"application_id": app.ID, "application_name": app.Name, "bot_id": botID, "bot_name": botName,
		"message_content_portal": portal, "message_content_config": doc.Discord.MessageContent,
		"message_content_blocked": doc.Discord.MessageContent && !portal,
		"portal_url":              "https://discord.com/developers/applications/" + app.ID + "/bot"}
	if boolVal(in, "apply") && doc.Instance.ExpectedBotID != botID {
		res := s.mutateConfig(req, func(d *config.Document) error {
			d.Instance.ExpectedBotID = botID
			return nil
		})
		if !res.OK {
			return res
		}
		for k, v := range out {
			res.Output[k] = v
		}
		res.Output["stored"] = true
		return res
	}
	return action.OK(req, out)
}
