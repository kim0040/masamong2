package application

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"masamong/internal/action"
	"masamong/internal/config"
	"masamong/internal/errcode"
	"masamong/internal/tools"
)

// access.policy (19장 A37). From the console it shows and edits config `access`: operators, blocked
// people (everywhere or in one server) and which servers the bot works in. From Discord it is the admin
// command, which an operator (access.operators) or — unless switched off — the server's own managers use
// in the channel they are in: pause, resume, reply mode, memory, chiming in, and blocking people in
// their server.

var snowflakeRE = regexp.MustCompile(`^\d{5,25}$`)

// mentionRE reads a user mention (<@123> or <@!123>) or a bare ID.
var mentionRE = regexp.MustCompile(`^<@!?(\d{5,25})>$|^(\d{5,25})$`)

// Blocked reports whether the bot ignores this Discord user everywhere.
func (s *Service) Blocked(userID string) bool {
	return userID != "" && slices.Contains(s.Doc().Access.BlockedUsers, userID)
}

// BlockedIn reports whether the bot ignores this user in a server: blocked everywhere, or by that
// server's managers.
func (s *Service) BlockedIn(guildID, userID string) bool {
	if userID == "" {
		return false
	}
	d := s.Doc()
	if slices.Contains(d.Access.BlockedUsers, userID) {
		return true
	}
	return guildID != "" && slices.Contains(d.Guilds[guildID].BlockedUsers, userID)
}

// IsOperator reports whether a Discord user is one of the bot's operators.
func (s *Service) IsOperator(userID string) bool {
	return userID != "" && slices.Contains(s.Doc().Access.Operators, userID)
}

// GuildAllowed reports whether the bot works in a server: approval is off, an operator approved it, or
// it has settings (a server set up before approval existed stays served). Direct messages have no
// server and are always allowed.
func (s *Service) GuildAllowed(guildID string) bool {
	if guildID == "" {
		return true
	}
	d := s.Doc()
	if !d.Access.ApprovalRequired() || slices.Contains(d.Access.Servers, guildID) {
		return true
	}
	return guildConfigured(d, guildID)
}

// guildConfigured reports a server with its own settings (rooms or a language); a server with only a
// block list is not "set up".
func guildConfigured(d config.Document, guildID string) bool {
	gs, ok := d.Guilds[guildID]
	return ok && (len(gs.Channels) > 0 || gs.Locale != "")
}

// SeedServers approves, once, the servers the bot is already in the first time it connects with
// server approval in place (an installation from before approval existed, or the first connection
// after setup). Later invitations wait for an operator.
func (s *Service) SeedServers(ids []string) {
	var seeded bool
	if ok, _ := s.kvGet("access:servers_seeded", &seeded); ok && seeded {
		return
	}
	if a := s.Doc().Access; a.ApprovalRequired() && len(ids) > 0 {
		// through the Action, so the change log shows it (via "system")
		raw, _ := json.Marshal(map[string]any{"servers": normalizeIDs(append(slices.Clone(a.Servers), ids...))})
		res := s.Execute(context.Background(), action.Request{Action: "access.policy", RequestID: "system:seed", Input: raw})
		if !res.OK {
			s.Log.Warn("server approval: could not approve the current servers", "err", res.Error)
			return
		}
		s.Log.Info("server approval: approved the servers the bot is in", "servers", len(ids))
	}
	_ = s.kvSet("access:servers_seeded", true)
}

// mayAdmin reports whether a Discord invoker may use the admin command in their channel.
func (s *Service) mayAdmin(inv *action.Invoker) bool {
	if inv == nil || inv.UserID == "" {
		return false
	}
	a := s.Doc().Access
	return slices.Contains(a.Operators, inv.UserID) || (inv.Manager && inv.GuildID != "" && a.GuildAdminsAllowed())
}

func (s *Service) accessPolicy(req action.Request) action.Result {
	if req.Invoker != nil && req.Invoker.UserID != "" {
		return s.discordAdmin(req)
	}
	in := inputMap(req)
	_, setOps := in["operators"]
	_, setBlocked := in["blocked_users"]
	_, setGuild := in["guild_admins"]
	_, setApproval := in["server_approval"]
	_, setServers := in["servers"]
	approve, revoke := strVal(in, "approve", ""), strVal(in, "revoke", "")
	guild, block, unblock := strVal(in, "guild_id", ""), strVal(in, "block", ""), strVal(in, "unblock", "")
	changed := setOps || setBlocked || setGuild || setApproval || setServers || approve != "" || revoke != "" || block != "" || unblock != ""
	if changed {
		ops, blocked, servers := strSlice(in, "operators"), strSlice(in, "blocked_users"), strSlice(in, "servers")
		ids := append(append(append([]string{}, ops...), blocked...), servers...)
		for _, id := range []string{approve, revoke, block, unblock, guild} {
			if id != "" {
				ids = append(ids, id)
			}
		}
		for _, id := range ids {
			if !snowflakeRE.MatchString(id) {
				return action.Fail(req, errcode.InvalidInput, "access.bad_id", s.msg(req, "access.bad_id", "id", id), nil)
			}
		}
		if (block != "" || unblock != "") && guild == "" {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "guild_id required with block or unblock", nil)
		}
		res := s.mutateConfig(req, func(d *config.Document) error {
			if setOps {
				d.Access.Operators = normalizeIDs(ops)
			}
			if setBlocked {
				d.Access.BlockedUsers = normalizeIDs(blocked)
			}
			if setGuild {
				v := boolVal(in, "guild_admins")
				d.Access.GuildAdmins = &v
			}
			if setApproval {
				v := boolVal(in, "server_approval")
				d.Access.ServerApproval = &v
			}
			if setServers {
				d.Access.Servers = normalizeIDs(servers)
			}
			if approve != "" {
				d.Access.Servers = normalizeIDs(append(d.Access.Servers, approve))
			}
			if revoke != "" {
				d.Access.Servers = normalizeIDs(slices.DeleteFunc(slices.Clone(d.Access.Servers), func(x string) bool { return x == revoke }))
			}
			if block != "" || unblock != "" {
				if d.Guilds == nil {
					d.Guilds = map[string]config.GuildSettings{}
				}
				gs := d.Guilds[guild]
				list := slices.Clone(gs.BlockedUsers)
				if block != "" {
					list = append(list, block)
				}
				if unblock != "" {
					list = slices.DeleteFunc(list, func(x string) bool { return x == unblock })
				}
				gs.BlockedUsers = normalizeIDs(list)
				d.Guilds[guild] = gs
			}
			return nil
		})
		if !res.OK {
			return res
		}
	}
	a := s.Doc().Access
	return action.OK(req, map[string]any{"operators": nonNil(a.Operators), "blocked_users": nonNil(a.BlockedUsers),
		"guild_admins": a.GuildAdminsAllowed(), "server_approval": a.ApprovalRequired(), "servers": nonNil(a.Servers),
		"guild_blocked": s.guildBlocked(), "changed": changed})
}

// guildBlocked lists the people blocked per server.
func (s *Service) guildBlocked() map[string][]string {
	out := map[string][]string{}
	for g, gs := range s.Doc().Guilds {
		if len(gs.BlockedUsers) > 0 {
			out[g] = slices.Clone(gs.BlockedUsers)
		}
	}
	return out
}

func normalizeIDs(ids []string) []string {
	out := uniqueStrings(ids)
	sort.Strings(out)
	if len(out) == 0 {
		return nil
	}
	return out
}

func nonNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// discordAdmin runs the admin command for the invoker's channel.
func (s *Service) discordAdmin(req action.Request) action.Result {
	inv := req.Invoker
	if !s.mayAdmin(inv) {
		return action.Fail(req, errcode.PermissionDenied, "access.denied", s.msg(req, "access.denied"), nil)
	}
	if inv.ChannelID == "" || inv.GuildID == "" {
		return action.Fail(req, errcode.InvalidInput, "access.server_only", s.msg(req, "access.server_only"), nil)
	}
	in := inputMap(req)
	g, c := inv.GuildID, inv.ChannelID
	op := strVal(in, "op", "status")
	// The change is the manager's own act, made through the same Actions the console uses. The change
	// log records who did it, in which server, from Discord.
	sub := func(act string, input map[string]any) action.Result {
		r := req
		raw, _ := json.Marshal(input)
		r.Action, r.Input, r.Invoker = act, raw, nil
		r.Operator = &action.Operator{ID: "discord:" + inv.UserID, Name: inv.Name, Role: "discord"}
		r.RequestID = "dc:" + strings.TrimPrefix(req.RequestID, "dc:")
		return s.Execute(context.Background(), r)
	}
	target := ""
	switch op {
	case "pause", "resume":
		if r := sub("runtime.pause", map[string]any{"scope": "channel", "guild_id": g, "channel_id": c, "paused": op == "pause"}); !r.OK {
			return r
		}
	case "respond":
		mode := strVal(in, "mode", "")
		if !config.ValidRespond(mode) {
			return action.Fail(req, errcode.InvalidInput, "access.bad_mode", s.msg(req, "access.bad_mode"), nil)
		}
		if r := sub("channels.configure", map[string]any{"guild_id": g, "channel_id": c, "respond": mode}); !r.OK {
			return r
		}
	case "memory":
		mode := strVal(in, "mode", "")
		if mode != "on" && mode != "off" {
			return action.Fail(req, errcode.InvalidInput, "access.bad_mode", s.msg(req, "access.bad_mode"), nil)
		}
		if r := sub("channels.configure", map[string]any{"guild_id": g, "channel_id": c, "capture": mode == "on"}); !r.OK {
			return r
		}
	case "chime":
		mode := strVal(in, "mode", "")
		if !stringIn(mode, config.ParticipationModes) {
			return action.Fail(req, errcode.InvalidInput, "access.bad_mode", s.msg(req, "access.bad_mode"), nil)
		}
		if r := sub("participation.mode", map[string]any{"guild_id": g, "channel_id": c, "mode": mode}); !r.OK {
			return r
		}
	case "block", "unblock":
		m := mentionRE.FindStringSubmatch(strings.TrimSpace(strVal(in, "user", "")))
		if m == nil {
			return action.Fail(req, errcode.InvalidInput, "access.need_user", s.msg(req, "access.need_user"), nil)
		}
		target = m[1] + m[2]
		if op == "block" {
			// the admin command cannot be used against its own users: the bot, the invoker or an operator
			if bot := s.Doc().Instance.ExpectedBotID; target == inv.UserID || target == bot || s.IsOperator(target) {
				return action.Fail(req, errcode.PermissionDenied, "access.cannot_block", s.msg(req, "access.cannot_block"), nil)
			}
			if s.Discord != nil {
				if id, _ := s.Discord.BotUser(); id == target {
					return action.Fail(req, errcode.PermissionDenied, "access.cannot_block", s.msg(req, "access.cannot_block"), nil)
				}
			}
		}
		if r := sub("access.policy", map[string]any{"guild_id": g, op: target}); !r.OK {
			return r
		}
	case "status":
	default:
		return action.Fail(req, errcode.InvalidInput, "access.bad_op", s.msg(req, "access.bad_op"), nil)
	}
	pol := s.channelPolicy(g, c)
	paused := s.Paused(g, c)
	onOff := func(on bool) string {
		if on {
			return s.msg(req, "access.memory_on")
		}
		return s.msg(req, "access.memory_off")
	}
	state := s.msg(req, "access.running")
	if paused {
		state = s.msg(req, "access.paused")
	}
	title := s.msg(req, "access.title")
	if op != "status" {
		title = s.msg(req, "access.done")
	}
	chime := pol.Participation
	if chime == "" {
		chime = "off"
	}
	fields := []action.CardField{
		{Name: s.msg(req, "access.state"), Value: state, Inline: true},
		{Name: s.msg(req, "access.respond"), Value: s.msg(req, "access.respond."+pol.Respond), Inline: true},
		{Name: s.msg(req, "access.memory"), Value: onOff(pol.MemoryUse == "on"), Inline: true},
		{Name: s.msg(req, "access.capture"), Value: onOff(pol.Capture && s.Doc().Memory.Capture), Inline: true},
		{Name: s.msg(req, "access.chime"), Value: s.msg(req, "access.chime."+chime), Inline: true},
		{Name: s.msg(req, "access.blocked_here"), Value: fmt.Sprint(len(s.Doc().Guilds[g].BlockedUsers)), Inline: true},
	}
	if target != "" {
		fields = append(fields, action.CardField{Name: s.msg(req, "access.target_"+op), Value: "<@" + target + ">"})
	}
	card := action.Card{Title: title, Ephemeral: true, Tone: "neutral", Fields: fields, Footer: s.msg(req, "access.help")}
	return action.WithCard(action.OK(req, map[string]any{"op": op, "paused": paused, "respond": pol.Respond, "capture": pol.Capture,
		"participation": chime}), card)
}

// gateDiscord refuses Discord requests the tool gate does not cover: anything from a blocked user, and
// feature commands in a paused channel (personal consent and settings, and the admin command that
// resumes the channel, still work there).
func (s *Service) gateDiscord(req action.Request) *action.Result {
	inv := req.Invoker
	if inv == nil || (inv.Via != "command" && inv.Via != "chat") {
		return nil
	}
	if s.BlockedIn(inv.GuildID, inv.UserID) {
		res := action.Fail(req, errcode.PermissionDenied, "access.blocked", s.msg(req, "access.blocked"), nil)
		return &res
	}
	// A command or button in a server waiting for approval gets a notice; consent can still be withdrawn and one's own memory
	// seen and forgotten there, and operators (who approve servers) are not held back. Chat turns need no check
	// here: the gateway drops messages from such a server before any turn starts, and the console's
	// test chats (lab, setup) name rooms freely.
	if inv.Via == "command" && !s.GuildAllowed(inv.GuildID) && !s.IsOperator(inv.UserID) && req.Action != "privacy.consent" && req.Action != "memory.mine.forget" && req.Action != "memory.mine.show" {
		res := action.Fail(req, errcode.PermissionDenied, "access.server_pending", s.msg(req, "access.server_pending"), nil)
		return &res
	}
	switch req.Action {
	case "privacy.consent", "preferences.set", "access.policy", "memory.mine.show", "memory.mine.forget":
		return nil
	}
	if _, isTool := tools.Find(s.toolRegistry(), req.Action); !isTool && s.Paused(inv.GuildID, inv.ChannelID) {
		res := action.Fail(req, errcode.FeatureDisabled, "runtime.paused", s.msg(req, "runtime.paused"), nil)
		return &res
	}
	return nil
}
