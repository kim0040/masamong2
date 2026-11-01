package application

import (
	"sort"

	"masamong/internal/action"
	"masamong/internal/errcode"
)

// The bot needs these permissions in every room it answers in (the invite link asks for exactly
// these). The console shows what is missing per room and raises an alert when a room loses one
// (someone changed a role or channel override on Discord).
var requiredPerms = []struct {
	Key string
	Bit int64
}{
	{"view_channel", 1 << 10}, {"send_messages", 1 << 11}, {"embed_links", 1 << 14}, {"read_message_history", 1 << 16},
}

// recommendedPerms are needed by some features only: a generated picture (attach files), /poll
// (send polls) and the bot's threads (make a public thread, answer inside it). A room without them
// still gets text answers.
var recommendedPerms = []struct {
	Key string
	Bit int64
}{
	{"attach_files", 1 << 15}, {"send_polls", 1 << 49}, {"create_public_threads", 1 << 35}, {"send_messages_in_threads", 1 << 38},
}

// threadPerms are what the bot's own threads (conversation.thread other than off) need.
var threadPerms = []string{"create_public_threads", "send_messages_in_threads"}

const permAdministrator = int64(1 << 3)

// invitePermissions is what the invite link asks for: the required and the recommended permissions,
// never Administrator.
func invitePermissions() int64 {
	var bits int64
	for _, p := range requiredPerms {
		bits |= p.Bit
	}
	for _, p := range recommendedPerms {
		bits |= p.Bit
	}
	return bits
}

// missingRecommended lists the recommended permissions the bot lacks in a channel.
func (s *Service) missingRecommended(guildID, channelID string) []string {
	out := []string{}
	if s.Discord == nil || !s.Discord.Connected() {
		return out
	}
	bits, ok := s.Discord.Permissions(guildID, channelID)
	if !ok || bits&permAdministrator != 0 {
		return out
	}
	for _, p := range recommendedPerms {
		if bits&p.Bit == 0 {
			out = append(out, p.Key)
		}
	}
	return out
}

// missingPerms lists the required permissions the bot lacks in a channel; known=false without a gateway.
func (s *Service) missingPerms(guildID, channelID string) (missing []string, known bool) {
	if s.Discord == nil || !s.Discord.Connected() {
		return nil, false
	}
	bits, ok := s.Discord.Permissions(guildID, channelID)
	if !ok {
		return nil, false
	}
	missing = []string{}
	if bits&permAdministrator != 0 {
		return missing, true
	}
	for _, p := range requiredPerms {
		if bits&p.Bit == 0 {
			missing = append(missing, p.Key)
		}
	}
	return missing, true
}

// permsAttention lists answering rooms that lack a permission. A room that once had every permission
// is remembered (discord:perms_ok), so losing one stays "lost" (danger) until it is fixed, however
// often the dashboard is loaded.
func (s *Service) permsAttention() []attention {
	if s.Discord == nil || !s.Discord.Connected() {
		return nil
	}
	now := map[string][]string{}
	for g, gs := range s.Doc().Guilds {
		for c, pol := range gs.Channels {
			if pol.Respond == "" || pol.Respond == "off" {
				continue
			}
			if miss, ok := s.missingPerms(g, c); ok {
				now[g+":"+c] = miss
			}
		}
	}
	hadAll := map[string]bool{}
	_, _ = s.kvGet("discord:perms_ok", &hadAll)
	changed := false
	var att []attention
	keys := make([]string, 0, len(now))
	for k := range now {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		miss := now[k]
		if len(miss) == 0 {
			if !hadAll[k] {
				hadAll[k], changed = true, true
			}
			continue
		}
		g, c := splitKey(k)
		sev, key := "warn", "discord_perms_missing"
		if hadAll[k] {
			sev, key = "danger", "discord_perms_lost"
		}
		att = append(att, attention{sev, key, map[string]any{"guild_id": g, "channel_id": c, "missing": miss}, "/rooms/" + g + "/" + c + "?tab=access"})
	}
	if changed {
		_ = s.kvSet("discord:perms_ok", hadAll)
	}
	return att
}

func splitKey(k string) (string, string) {
	for i := 0; i < len(k); i++ {
		if k[i] == ':' {
			return k[:i], k[i+1:]
		}
	}
	return k, ""
}

// channelsPermissions is channels.permissions: what the bot can and cannot do in one room.
func (s *Service) channelsPermissions(req action.Request) action.Result {
	in := inputMap(req)
	g, c := strVal(in, "guild_id", ""), strVal(in, "channel_id", "")
	if g == "" || c == "" {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "guild_id and channel_id required", nil)
	}
	miss, known := s.missingPerms(g, c)
	req2 := []string{}
	for _, p := range requiredPerms {
		req2 = append(req2, p.Key)
	}
	rec := []string{}
	for _, p := range recommendedPerms {
		rec = append(rec, p.Key)
	}
	recMiss := s.missingRecommended(g, c)
	// thread_missing: the thread permissions this room lacks while its thread setting is not off (the
	// default "offer" counts), so the Conversation card can say why the thread button never appears
	threadMiss := []string{}
	if s.Doc().Guilds[g].Channels[c].Conversation.Thread != "off" {
		for _, p := range recMiss {
			for _, tp := range threadPerms {
				if p == tp {
					threadMiss = append(threadMiss, p)
				}
			}
		}
	}
	return action.OK(req, map[string]any{"known": known, "missing": miss, "required": req2, "recommended": rec,
		"recommended_missing": recMiss, "thread_missing": threadMiss})
}

// serversAttention lists servers the bot was invited to that wait for an operator's approval.
func (s *Service) serversAttention() []attention {
	if s.Discord == nil || !s.Discord.Connected() {
		return nil
	}
	var names []string
	for _, g := range s.Discord.Guilds() {
		if !s.GuildAllowed(g.ID) {
			names = append(names, g.Name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)
	return []attention{{"warn", "servers_pending", map[string]any{"n": len(names), "name": names[0]}, "/rooms?view=servers"}}
}
