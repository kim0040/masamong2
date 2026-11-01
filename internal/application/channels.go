package application

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"masamong/internal/action"
	"masamong/internal/config"
	"masamong/internal/errcode"
	"masamong/internal/memory/blocks"
	"masamong/internal/memory/engine"
	"masamong/internal/providers"
)

func (s *Service) channels(req action.Request) action.Result {
	in := inputMap(req)
	switch req.Action {
	case "channels.list":
		return s.channelsList(req)
	case "channels.configure":
		return s.channelsConfigure(req, in)
	case "channels.invite":
		return s.channelsInvite(req)
	case "channels.leave":
		return s.channelsLeave(req, in)
	case "channels.attachments.add":
		return s.channelsAttachmentAdd(req, in)
	case "channels.attachments.remove":
		return s.channelsAttachmentRemove(req, in)
	case "channels.ops_log":
		return s.channelsOpsLog(req, in)
	case "channels.view.show":
		return s.channelsViewShow(req, in)
	case "channels.view.attach":
		return s.channelsViewAttach(req, in)
	case "channels.view.detach":
		return s.channelsViewDetach(req, in)
	}
	return action.Fail(req, errcode.ActionUnknown, "action.unknown", s.msg(req, "action.unknown"), nil)
}

func (s *Service) channelsList(req action.Request) action.Result {
	live := s.Discord != nil && s.Discord.Connected()
	guilds := []any{}
	chans := []any{}
	seenGuild := map[string]bool{}
	seenChan := map[string]bool{}

	doc := s.Doc()
	guildView := func(g GuildInfo, live bool) map[string]any {
		configured := guildConfigured(doc, g.ID)
		return map[string]any{"id": g.ID, "name": g.Name, "members": g.Members, "joined_at": g.JoinedAt, "live": live,
			"allowed": s.GuildAllowed(g.ID), "approved": slices.Contains(doc.Access.Servers, g.ID), "configured": configured,
			"blocked_users": nonNil(doc.Guilds[g.ID].BlockedUsers), "log": s.serverLogView(g.ID)}
	}
	if live {
		for _, g := range s.Discord.Guilds() {
			seenGuild[g.ID] = true
			guilds = append(guilds, guildView(g, true))
			for _, c := range s.Discord.Channels(g.ID) {
				seenChan[c.ID] = true
				chans = append(chans, s.channelView(g.ID, c.ID, c.Name, c.Type, true))
			}
		}
	}
	// Configured policies not yet visible on the gateway stay listed so operators
	// can prepare isolation and RAG slots offline.
	for guildID, g := range s.Doc().Guilds {
		if !seenGuild[guildID] {
			name := guildID
			if s.practice && guildID == PracticeGuild {
				name = s.msg(req, "practice.guild_name") // the sample server has a name like a real one
			}
			guilds = append(guilds, guildView(GuildInfo{ID: guildID, Name: name}, false))
			seenGuild[guildID] = true
		}
		for channelID := range g.Channels {
			if seenChan[channelID] {
				continue
			}
			view := s.channelView(guildID, channelID, channelID, "text", false)
			if s.practice {
				view["practice"] = true // a sample channel: never on Discord, not "offline"
			}
			chans = append(chans, view)
			seenChan[channelID] = true
		}
	}

	status := "disconnected"
	if live {
		status = "ok"
	} else if len(chans) > 0 {
		status = "configured_only"
	}
	// whether a bot token can be read now, so the console asks Discord about it only when there is one
	tokenSaved := false
	if cred, err := providers.Resolve(s.Doc().Discord.TokenRef); err == nil {
		tokenSaved = cred.Present()
	}
	out := map[string]any{
		"status":          status,
		"guilds":          guilds,
		"channels":        chans,
		"discord_enabled": s.Doc().Discord.Enabled,
		"discord_token":   tokenSaved,
		"live":            live,
		"server_approval": doc.Access.ApprovalRequired(),
	}
	if h, ok := s.gatewayHealth(); ok {
		out["gateway"] = h
	}
	return action.OK(req, out)
}

func (s *Service) channelView(guildID, channelID, name, typ string, live bool) map[string]any {
	pol := s.channelPolicy(guildID, channelID)
	atts := make([]any, 0, len(pol.RAGAttachments))
	for _, a := range pol.RAGAttachments {
		atts = append(atts, a)
	}
	return map[string]any{
		"id":              channelID,
		"guild_id":        guildID,
		"name":            name,
		"type":            typ,
		"live":            live,
		"configured":      s.hasChannelPolicy(guildID, channelID),
		"respond":         pol.Respond,
		"persona_ref":     pol.PersonaRef,
		"capture":         pol.Capture,
		"logging_level":   pol.Logging.Level,
		"logging_retain":  pol.Logging.RetainDays,
		"memory_use":      pol.MemoryUse,
		"disaster_alerts": pol.DisasterAlerts != "off",
		"rules_set":       pol.Rules != "",
		"proactive":       false,
		"rag_attachments": atts,
		"memory_view":     viewMap(pol.RAGView, "discord:"+guildID+":"+channelID),
		"rag_ready":       pol.MemoryUse == "on" && s.Doc().Memory.Retrieve != "off",
		"note":            "memory searches this channel's room plus attached blocks only; staged attachments are imported with `imports start`",
	}
}

func (s *Service) hasChannelPolicy(guildID, channelID string) bool {
	g, ok := s.Doc().Guilds[guildID]
	if !ok || g.Channels == nil {
		return false
	}
	_, ok = g.Channels[channelID]
	return ok
}

// ChannelPolicyFor is the effective policy of one channel (connectors read it; they never write).
func (s *Service) ChannelPolicyFor(guildID, channelID string) config.ChannelPolicy {
	return s.channelPolicy(guildID, channelID)
}

func (s *Service) channelPolicy(guildID, channelID string) config.ChannelPolicy {
	if guildID == "" {
		pol := config.DefaultChannelPolicy()
		pol.PersonaRef = s.defaultPersonaFor("")
		pol.Respond, pol.MemoryUse, pol.Attribution, pol.Capture = "always", "off", "off", false
		return pol
	}
	// a thread follows its parent channel unless it has an entry of its own (threads.go)
	if g, ok := s.Doc().Guilds[guildID]; !ok || g.Channels == nil || !hasChannel(g.Channels, channelID) {
		if parent := s.threadParent(channelID); parent != "" {
			return s.ownChannelPolicy(guildID, parent)
		}
	}
	return s.ownChannelPolicy(guildID, channelID)
}

func hasChannel(chans map[string]config.ChannelPolicy, id string) bool {
	_, ok := chans[id]
	return ok
}

// ownChannelPolicy is a channel's own entry, or the defaults.
func (s *Service) ownChannelPolicy(guildID, channelID string) config.ChannelPolicy {
	if g, ok := s.Doc().Guilds[guildID]; ok && g.Channels != nil {
		if p, ok := g.Channels[channelID]; ok {
			if p.PersonaRef == "" {
				p.PersonaRef = s.defaultPersonaFor(guildID)
			}
			return config.NormalizeChannelPolicy(p)
		}
	}
	pol := config.DefaultChannelPolicy()
	pol.PersonaRef = s.defaultPersonaFor(guildID)
	return pol
}

// effectiveLocale is the language a server's rooms use when their persona does not set one: the
// server's own locale, then the instance default.
func (s *Service) effectiveLocale(guildID string) string {
	if g, ok := s.Doc().Guilds[guildID]; ok && g.Locale != "" {
		return g.Locale
	}
	return s.Doc().I18n.DefaultLocale
}

// defaultPersonaFor is the persona a room without one uses: the language's warm_<tag> where it has
// one (Korean, Japanese) and i18n.polite_default is on, else friendly_<tag>, falling back to the other built-in friendly personas
// and skipping ones the operator hid. A new language only needs a friendly_<tag> built-in to become
// the default for its rooms.
func (s *Service) defaultPersonaFor(guildID string) string {
	first := ""
	for _, id := range config.DefaultPersonaCandidates(s.effectiveLocale(guildID), s.Doc().I18n.PoliteDefault) {
		if _, ok := config.BuiltinPersonas()[id]; !ok {
			continue
		}
		if first == "" {
			first = id
		}
		if p, ok := s.Doc().Personas[id]; ok && p.Hidden {
			continue
		}
		// hiding a voice (warm_ko) hides its v2 too: the operator chose against the register
		if p, ok := s.Doc().Personas[strings.TrimSuffix(id, "_v2")]; ok && p.Hidden {
			continue
		}
		return id
	}
	if first != "" {
		return first
	}
	return "friendly_ko"
}

func (s *Service) channelsConfigure(req action.Request, in map[string]any) action.Result {
	guildID := strVal(in, "guild_id", "")
	channelID := strVal(in, "id", strVal(in, "channel_id", ""))
	if guildID == "" || channelID == "" {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "guild_id and channel id required", nil)
	}
	respond := strVal(in, "respond", "")
	persona := strVal(in, "persona_ref", "")
	logging := strVal(in, "logging_level", "")
	retain := intVal(in, "logging_retain", -1)
	_, hasCapture := in["capture"]
	capture := boolVal(in, "capture")
	memoryUse := strVal(in, "memory_use", "")
	_, hasRules := in["rules"]
	rules := strVal(in, "rules", "")
	disaster := strVal(in, "disaster_alerts", "")

	res := s.mutateConfig(req, func(d *config.Document) error {
		ensureGuildChannelMaps(d, guildID)
		p := config.NormalizeChannelPolicy(d.Guilds[guildID].Channels[channelID])
		if respond != "" {
			if !config.ValidRespond(respond) {
				return fmt.Errorf("respond must be one of %v", config.RespondModes)
			}
			p.Respond = respond
		}
		if persona != "" {
			if !personaExists(*d, persona) {
				return fmt.Errorf("unknown persona_ref %s", persona)
			}
			p.PersonaRef = persona
		}
		if logging != "" {
			if !config.ValidLoggingLevel(logging) {
				return fmt.Errorf("logging_level must be one of %v", config.LoggingLevels)
			}
			p.Logging.Level = logging
		}
		if retain >= 0 {
			p.Logging.RetainDays = retain
		}
		if hasCapture {
			p.Capture = capture
		}
		if memoryUse != "" {
			if memoryUse != "on" && memoryUse != "off" {
				return fmt.Errorf("memory_use must be on or off")
			}
			p.MemoryUse = memoryUse
		}
		if disaster != "" {
			if disaster != "on" && disaster != "off" {
				return fmt.Errorf("disaster_alerts must be on or off")
			}
			p.DisasterAlerts = disaster
			if disaster == "on" {
				p.DisasterAlerts = "" // on is the default
			}
		}
		if hasRules {
			if len([]rune(rules)) > 2000 {
				return fmt.Errorf("rules must be at most 2000 characters")
			}
			p.Rules = rules
		}
		p = config.NormalizeChannelPolicy(p)
		d.Guilds[guildID].Channels[channelID] = p
		return nil
	})
	if !res.OK {
		return res
	}
	pol := s.channelPolicy(guildID, channelID)
	_ = s.appendChannelOp(guildID, channelID, "policy_update",
		fmt.Sprintf("respond=%s persona=%s logging=%s capture=%v", pol.Respond, pol.PersonaRef, pol.Logging.Level, pol.Capture),
		s.Revision())
	res.Output["id"] = channelID
	res.Output["guild_id"] = guildID
	res.Output["persisted"] = true
	res.Output["status"] = "ok"
	res.Output["respond"] = pol.Respond
	res.Output["persona_ref"] = pol.PersonaRef
	res.Output["capture"] = pol.Capture
	res.Output["disaster_alerts"] = pol.DisasterAlerts != "off"
	res.Output["logging_level"] = pol.Logging.Level
	res.Output["memory_use"] = pol.MemoryUse
	res.Output["rules_set"] = pol.Rules != ""
	res.Output["cannot_override_memory_scope"] = true
	return res
}

func (s *Service) channelsInvite(req action.Request) action.Result {
	id := s.Doc().Instance.ExpectedBotID
	if s.Discord != nil && s.Discord.Connected() {
		id, _ = s.Discord.BotUser()
	}
	if id == "" {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "set instance.expected_bot_id or connect the gateway to build an invite link", nil)
	}
	// the invite asks for exactly what the bot uses (see requiredPerms and recommendedPerms); with a
	// guild_id Discord preselects that server
	u := "https://discord.com/oauth2/authorize?client_id=" + id + "&scope=bot%20applications.commands&permissions=" + strconv.FormatInt(invitePermissions(), 10)
	if g := strVal(inputMap(req), "guild_id", ""); snowflakeRE.MatchString(g) {
		u += "&guild_id=" + g + "&disable_guild_select=true"
	}
	perms := []string{}
	for _, p := range requiredPerms {
		perms = append(perms, p.Key)
	}
	for _, p := range recommendedPerms {
		perms = append(perms, p.Key)
	}
	return action.OK(req, map[string]any{
		"url":              u,
		"permissions":      perms,
		"server_approval":  s.Doc().Access.ApprovalRequired(),
		"permissions_note": "platform-owned invite; a server admin must approve it",
	})
}

// channelsLeave takes the bot out of a server an operator turned down. Without confirm it only says
// what would happen; the server's settings are kept so a later invite can be approved again.
func (s *Service) channelsLeave(req action.Request, in map[string]any) action.Result {
	guildID := strVal(in, "guild_id", "")
	if !snowflakeRE.MatchString(guildID) {
		return action.Fail(req, errcode.InvalidInput, "access.bad_id", s.msg(req, "access.bad_id", "id", guildID), nil)
	}
	name := guildID
	present := false
	if s.Discord != nil && s.Discord.Connected() {
		for _, g := range s.Discord.Guilds() {
			if g.ID == guildID {
				name, present = g.Name, true
			}
		}
	}
	if !boolVal(in, "confirm") {
		return action.OK(req, map[string]any{"guild_id": guildID, "name": name, "present": present, "confirm_required": true})
	}
	leaver, ok := s.Discord.(GuildLeaver)
	if !ok || s.Discord == nil || !s.Discord.Connected() {
		return action.Fail(req, errcode.DiscordNotConnected, "generic.error", "the Discord connection is not running", nil)
	}
	if !present {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "the bot is not in that server", nil)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := leaver.LeaveGuild(ctx, guildID); err != nil {
		return action.Fail(req, errcode.ProviderError, "generic.error", err.Error(), nil)
	}
	res := s.mutateConfig(req, func(d *config.Document) error {
		d.Access.Servers = normalizeIDs(slices.DeleteFunc(slices.Clone(d.Access.Servers), func(x string) bool { return x == guildID }))
		return nil
	})
	if !res.OK {
		return res
	}
	return action.OK(req, map[string]any{"guild_id": guildID, "name": name, "left": true})
}

func (s *Service) channelsAttachmentAdd(req action.Request, in map[string]any) action.Result {
	guildID := strVal(in, "guild_id", "")
	channelID := strVal(in, "channel_id", strVal(in, "id", ""))
	filename := filepath.Base(strVal(in, "filename", ""))
	source := strVal(in, "source_path", strVal(in, "path", ""))
	purpose := strVal(in, "purpose", "retrieval_context")
	if guildID == "" || channelID == "" || filename == "" || source == "" {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "guild_id, channel_id, filename, and source_path required", nil)
	}
	if purpose != "retrieval_context" {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "purpose must be retrieval_context", nil)
	}
	abs, err := s.resolveStagingPath(source)
	if err != nil {
		return action.Fail(req, errcode.InvalidInput, "generic.error", err.Error(), nil)
	}
	sum, size, err := hashFile(abs)
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	stored := sum + filepath.Ext(filename)
	destDir := s.channelRAGDir(guildID, channelID)
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	dest := filepath.Join(destDir, stored)
	if _, err := os.Stat(dest); err != nil {
		if err := copyFile(abs, dest); err != nil {
			return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
		}
	}
	attID := "att_" + sum[:12]
	att := config.RAGAttachmentRef{
		ID: attID, ContentHash: "sha256:" + sum, Filename: filename,
		Mime: strVal(in, "mime", guessMIME(filename)), Bytes: size, Purpose: purpose, StoredName: stored,
	}

	res := s.mutateConfig(req, func(d *config.Document) error {
		ensureGuildChannelMaps(d, guildID)
		p := config.NormalizeChannelPolicy(d.Guilds[guildID].Channels[channelID])
		for _, existing := range p.RAGAttachments {
			if existing.ContentHash == att.ContentHash {
				return fmt.Errorf("attachment already staged on this channel")
			}
		}
		p.RAGAttachments = append(p.RAGAttachments, att)
		d.Guilds[guildID].Channels[channelID] = p
		return nil
	})
	if !res.OK {
		return res
	}
	_ = s.appendChannelOp(guildID, channelID, "attachment_add", "staged "+filename+" ("+att.ContentHash+")", s.Revision())
	res.Output["persisted"] = true
	res.Output["guild_id"] = guildID
	res.Output["channel_id"] = channelID
	res.Output["attachment"] = att
	res.Output["isolated"] = true
	res.Output["rag_ready"] = false // a staged file is not memory until it is imported and indexed
	res.Output["note"] = "file staged for this channel only; import it with `imports start --source kakao_csv` and attach the block"
	return res
}

func (s *Service) channelsAttachmentRemove(req action.Request, in map[string]any) action.Result {
	guildID := strVal(in, "guild_id", "")
	channelID := strVal(in, "channel_id", strVal(in, "id", ""))
	attID := strVal(in, "attachment_id", "")
	if guildID == "" || channelID == "" || attID == "" {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "guild_id, channel_id, and attachment_id required", nil)
	}
	var removed *config.RAGAttachmentRef
	res := s.mutateConfig(req, func(d *config.Document) error {
		ensureGuildChannelMaps(d, guildID)
		p := config.NormalizeChannelPolicy(d.Guilds[guildID].Channels[channelID])
		next := make([]config.RAGAttachmentRef, 0, len(p.RAGAttachments))
		for _, a := range p.RAGAttachments {
			if a.ID == attID {
				cp := a
				removed = &cp
				continue
			}
			next = append(next, a)
		}
		if removed == nil {
			return fmt.Errorf("attachment not found on this channel")
		}
		p.RAGAttachments = next
		d.Guilds[guildID].Channels[channelID] = p
		return nil
	})
	if !res.OK {
		return res
	}
	if removed != nil && removed.StoredName != "" {
		_ = os.Remove(filepath.Join(s.channelRAGDir(guildID, channelID), removed.StoredName))
	}
	_ = s.appendChannelOp(guildID, channelID, "attachment_remove", "removed "+attID, s.Revision())
	res.Output["persisted"] = true
	res.Output["removed"] = attID
	res.Output["guild_id"] = guildID
	res.Output["channel_id"] = channelID
	return res
}

func (s *Service) channelsOpsLog(req action.Request, in map[string]any) action.Result {
	guildID := strVal(in, "guild_id", "")
	channelID := strVal(in, "channel_id", strVal(in, "id", ""))
	if guildID == "" || channelID == "" {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "guild_id and channel_id required", nil)
	}
	limit := intVal(in, "limit", 50)
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.Store.DB.Query(
		`SELECT id, kind, summary, IFNULL(revision,0), created_at FROM channel_ops_log
		 WHERE guild_id=? AND channel_id=? ORDER BY id DESC LIMIT ?`,
		guildID, channelID, limit,
	)
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	defer rows.Close()
	items := []any{}
	for rows.Next() {
		var id, rev int
		var kind, summary, created string
		if err := rows.Scan(&id, &kind, &summary, &rev, &created); err != nil {
			return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
		}
		items = append(items, map[string]any{"id": id, "kind": kind, "summary": summary, "revision": rev, "created_at": created})
	}
	return action.OK(req, map[string]any{
		"guild_id": guildID, "channel_id": channelID, "entries": items,
		"note": "ops metadata only; message bodies and user ids are never stored here",
	})
}

func (s *Service) channelsViewShow(req action.Request, in map[string]any) action.Result {
	guildID := strVal(in, "guild_id", "")
	channelID := strVal(in, "channel_id", strVal(in, "id", ""))
	if guildID == "" || channelID == "" {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "guild_id and channel_id required", nil)
	}
	pol := s.channelPolicy(guildID, channelID)
	key := blocks.ChannelKey(guildID, channelID)
	return action.OK(req, map[string]any{
		"guild_id":    guildID,
		"channel_id":  channelID,
		"memory_view": viewMap(pol.RAGView, key),
		"memory_use":  pol.MemoryUse,
		"rooms":       s.channelRooms(guildID, channelID),
	})
}

func (s *Service) persistView(req action.Request, guildID, channelID string, v blocks.View) action.Result {
	now := s.Clock.Now().UTC().Format(time.RFC3339)
	res := s.mutateConfig(req, func(d *config.Document) error {
		ensureGuildChannelMaps(d, guildID)
		p := config.NormalizeChannelPolicy(d.Guilds[guildID].Channels[channelID])
		p.RAGView = config.MemoryView{
			Blocks:           v.Blocks,
			Revision:         v.Revision,
			ScopeFingerprint: v.ScopeFingerprint,
			UpdatedAt:        now,
		}
		d.Guilds[guildID].Channels[channelID] = p
		return nil
	})
	if !res.OK {
		return res
	}
	pol := s.channelPolicy(guildID, channelID)
	res.Output["persisted"] = true
	res.Output["guild_id"] = guildID
	res.Output["channel_id"] = channelID
	res.Output["memory_view"] = viewMap(pol.RAGView, v.ChannelKey)
	res.Output["memory_use"] = pol.MemoryUse
	res.Output["reembedded"] = false
	return res
}

func (s *Service) channelsViewAttach(req action.Request, in map[string]any) action.Result {
	guildID := strVal(in, "guild_id", "")
	channelID := strVal(in, "channel_id", strVal(in, "id", ""))
	blockID := strVal(in, "block_id", "")
	if guildID == "" || channelID == "" || blockID == "" {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "guild_id, channel_id, and block_id required", nil)
	}
	add, ok := s.lookupBlock(blockID)
	if !ok {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "block not found", nil)
	}
	if add.SourceRoom == engine.RoomKey(guildID, channelID) {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "a channel always searches its own conversation; attach other blocks", nil)
	}
	pol := s.channelPolicy(guildID, channelID)
	for _, id := range pol.RAGView.Blocks {
		if b, ok := s.lookupBlock(id); ok && b.SourceRoom == add.SourceRoom {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "this block is already connected to the channel", nil)
		}
	}
	bound := s.lookupBlocks(pol.RAGView.Blocks)
	cur := blocks.View{
		ChannelKey:       blocks.ChannelKey(guildID, channelID),
		Blocks:           append([]string{}, pol.RAGView.Blocks...),
		Revision:         pol.RAGView.Revision,
		ScopeFingerprint: pol.RAGView.ScopeFingerprint,
	}
	next, err := blocks.Attach(cur, add, bound)
	if err != nil {
		return action.Fail(req, errcode.InvalidInput, "generic.error", err.Error(), nil)
	}
	res := s.persistView(req, guildID, channelID, next)
	if res.OK {
		_ = s.appendChannelOp(guildID, channelID, "view_attach", "block="+blockID, s.Revision())
	}
	return res
}

func (s *Service) channelsViewDetach(req action.Request, in map[string]any) action.Result {
	guildID := strVal(in, "guild_id", "")
	channelID := strVal(in, "channel_id", strVal(in, "id", ""))
	blockID := strVal(in, "block_id", "")
	if guildID == "" || channelID == "" || blockID == "" {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "guild_id, channel_id, and block_id required", nil)
	}
	pol := s.channelPolicy(guildID, channelID)
	cur := blocks.View{
		ChannelKey:       blocks.ChannelKey(guildID, channelID),
		Blocks:           append([]string{}, pol.RAGView.Blocks...),
		Revision:         pol.RAGView.Revision,
		ScopeFingerprint: pol.RAGView.ScopeFingerprint,
	}
	next := blocks.Detach(cur, blockID)
	// The console may send the current width's ID while the policy still holds
	// its previous width's alias. Remove that same room's stored references too.
	if target, ok := s.lookupBlock(blockID); ok {
		for _, id := range pol.RAGView.Blocks {
			if b, found := s.lookupBlock(id); found && sameBlockPreparation(target, b) {
				next.Blocks = slices.DeleteFunc(next.Blocks, func(v string) bool { return v == id })
			}
		}
		next = blocks.MakeView(cur.ChannelKey, next.Blocks, cur.Revision+1)
	}
	res := s.persistView(req, guildID, channelID, next)
	if res.OK {
		_ = s.appendChannelOp(guildID, channelID, "view_detach", "block="+blockID, s.Revision())
	}
	return res
}

func (s *Service) personas(req action.Request) action.Result {
	in := inputMap(req)
	switch req.Action {
	case "personas.list":
		return action.OK(req, map[string]any{"personas": s.personaList(), "cannot_override_memory_scope": true})
	case "personas.apply":
		// one room (guild_id + channel_id) or several (targets: ["guild:channel", ...]) in one change
		personaID := strVal(in, "id", strVal(in, "persona_ref", "friendly_ko"))
		type room struct{ guild, channel string }
		var rooms []room
		if g, c := strVal(in, "guild_id", ""), strVal(in, "channel_id", ""); g != "" && c != "" {
			rooms = append(rooms, room{g, c})
		}
		for _, t := range strSlice(in, "targets") {
			g, c, ok := strings.Cut(t, ":")
			if !ok || !snowflakeRE.MatchString(g) || !snowflakeRE.MatchString(c) {
				return action.Fail(req, errcode.InvalidInput, "generic.error", "targets are guild:channel IDs", nil)
			}
			rooms = append(rooms, room{g, c})
		}
		if len(rooms) == 0 {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "guild_id and channel_id (or targets) required to apply a persona", nil)
		}
		if len(rooms) > 500 {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "at most 500 rooms at once", nil)
		}
		if !personaExists(s.Doc(), personaID) {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "unknown persona "+personaID, nil)
		}
		res := s.mutateConfig(req, func(d *config.Document) error {
			for _, r := range rooms {
				ensureGuildChannelMaps(d, r.guild)
				p := config.NormalizeChannelPolicy(d.Guilds[r.guild].Channels[r.channel])
				p.PersonaRef = personaID
				d.Guilds[r.guild].Channels[r.channel] = p
			}
			return nil
		})
		if !res.OK {
			return res
		}
		for _, r := range rooms {
			_ = s.appendChannelOp(r.guild, r.channel, "persona_apply", "persona="+personaID, s.Revision())
		}
		res.Output["applied"] = personaID
		res.Output["rooms"] = len(rooms)
		if len(rooms) == 1 {
			res.Output["guild_id"] = rooms[0].guild
			res.Output["channel_id"] = rooms[0].channel
		}
		res.Output["persisted"] = true
		res.Output["cannot_override_memory_scope"] = true
		return res
	}
	return action.Fail(req, errcode.ActionUnknown, "action.unknown", s.msg(req, "action.unknown"), nil)
}

func (s *Service) personaList() []any {
	// built-ins in a stable order, then the operator's own by name: a stable order on every load.
	// Hidden personas stay in the list (flagged) so the console can show and restore them.
	out := []any{}
	seen := map[string]bool{}
	builtin := config.BuiltinPersonas()
	hidden := func(id string) bool { return s.Doc().Personas[id].Hidden }
	for _, id := range config.BuiltinPersonaIDs() {
		p := builtin[id]
		seen[id] = true
		out = append(out, map[string]any{"id": id, "locale": p.Locale, "label": p.Label, "builtin": true, "hidden": hidden(id), "style": p.Style})
	}
	own := s.Doc().Personas
	ids := slices.Collect(maps.Keys(own))
	label := func(id string) string {
		if l := own[id].Label; l != "" {
			return l
		}
		return id
	}
	slices.SortFunc(ids, func(a, b string) int { return strings.Compare(strings.ToLower(label(a)), strings.ToLower(label(b))) })
	for _, id := range ids {
		if seen[id] {
			continue
		}
		p := own[id]
		out = append(out, map[string]any{"id": id, "locale": p.Locale, "label": label(id), "builtin": p.Builtin, "hidden": p.Hidden, "style": p.Style})
	}
	return out
}

func personaExists(d config.Document, id string) bool {
	if _, ok := config.BuiltinPersonas()[id]; ok {
		return true
	}
	_, ok := d.Personas[id]
	return ok
}

func ensureGuildChannelMaps(d *config.Document, guildID string) {
	if d.Guilds == nil {
		d.Guilds = map[string]config.GuildSettings{}
	}
	g := d.Guilds[guildID]
	if g.Channels == nil {
		g.Channels = map[string]config.ChannelPolicy{}
	}
	d.Guilds[guildID] = g
}

func (s *Service) channelRAGDir(guildID, channelID string) string {
	return filepath.Join(s.DataRoot, "rag", "channels", safePathID(guildID), safePathID(channelID))
}

func (s *Service) resolveStagingPath(source string) (string, error) {
	source = filepath.Clean(source)
	if strings.Contains(source, "..") {
		return "", fmt.Errorf("source_path may not include a parent-directory marker")
	}
	stagingRoot := filepath.Join(s.DataRoot, "rag_staging")
	if !filepath.IsAbs(source) {
		source = filepath.Join(stagingRoot, source)
	}
	abs, err := filepath.Abs(source)
	if err != nil {
		return "", err
	}
	root, err := filepath.Abs(stagingRoot)
	if err != nil {
		return "", err
	}
	if abs != root && !strings.HasPrefix(abs, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("source_path must be under %s", stagingRoot)
	}
	st, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("source file not found under rag_staging")
	}
	if st.IsDir() {
		return "", fmt.Errorf("source_path must be a file")
	}
	return abs, nil
}

func (s *Service) appendChannelOp(guildID, channelID, kind, summary string, revision int) error {
	_, err := s.Store.DB.Exec(
		`INSERT INTO channel_ops_log(guild_id, channel_id, kind, summary, revision, created_at) VALUES(?,?,?,?,?,?)`,
		guildID, channelID, kind, summary, revision, s.Clock.Now().UTC().Format(time.RFC3339),
	)
	return err
}

func safePathID(id string) string {
	id = strings.TrimSpace(id)
	var b strings.Builder
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}

func hashFile(path string) (sum string, size int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

func guessMIME(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".md", ".markdown":
		return "text/markdown"
	case ".txt":
		return "text/plain"
	case ".json":
		return "application/json"
	case ".csv":
		return "text/csv"
	default:
		return "application/octet-stream"
	}
}

func newOpaqueID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
