package application

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"masamong/internal/action"
	"masamong/internal/chat"
	"masamong/internal/errcode"
	"masamong/internal/lang"
	"masamong/internal/memory/episodes"
	"masamong/internal/memory/store"
	"masamong/internal/storage"
)

// Explicit memory (docs/plan-memory-conversation.md §5.4.5, P4-4): "remember this", "that's wrong",
// "forget that" in any language, and the personal commands /mymemory (/내기억) and /forgetme (/잊어줘).
//
// The decision is a model's, not a word list's: the planner prompt stays the evaluated one, so a small
// separate classifier runs — only for a message addressed to the bot that contains one of the pack's
// optional explicit cues (most turns never pay for it). What it decides:
//
//   - remember: an episode item of kind fact, with the member's own message as its evidence, in their
//     own scope. A sentence that is an instruction to the bot ("always answer in English") is refused,
//     by the classifier's flag and by the pack's instruction cues: memory never carries instructions. A
//     "remember this" about someone else stays an episode fact said by the speaker — never a card.
//   - wrong: the matching item (from what the bot keeps about the speaker or what bears on the
//     conversation) is hidden, and its episode is never made again from the same lines.
//   - forget: a preview only; the member confirms with /forgetme, whose button applies it.
//
// The outcome reaches the answer as a tool result (memory.explicit), so the reply confirms it in a few
// words in the member's language.

const (
	explicitTool      = "memory.explicit"
	explicitItemsMax  = 8
	explicitTimeout   = 8 * time.Second
	forgetPendingLife = 15 * time.Minute
	explicitSource    = "explicit" // channel_episode.source of a remembered fact
	// explicitFactDiceThreshold is the minimum language-neutral character-bigram Dice score for treating two facts as near-identical.
	explicitFactDiceThreshold = 0.6
)

var explicitFactRefTokens = regexp.MustCompile(`\{@[^}]+\}`)

var errNoItemsSaved = errors.New("no memory items saved")

func explicitCued(packs []*lang.Pack, message string) bool {
	m := strings.ToLower(message)
	for _, p := range packs {
		for _, c := range p.Memory.ExplicitCues {
			if c = strings.ToLower(strings.TrimSpace(c)); c != "" && strings.Contains(m, c) {
				return true
			}
		}
	}
	return false
}

func instructionLike(packs []*lang.Pack, text string) bool { return chat.InstructionLike(packs, text) }

// explicitPacks are the reply's pack and the memory's languages' packs.
func (s *Service) explicitPacks(voice chat.Voice) []*lang.Pack {
	out := []*lang.Pack{voice.Pack}
	for _, t := range s.Doc().Memory.Languages {
		if p, ok := lang.Get(t); ok && p != voice.Pack {
			out = append(out, p)
		}
	}
	if len(s.Doc().Memory.Languages) == 0 {
		if p, ok := lang.Get(lang.Default); ok && p != voice.Pack {
			out = append(out, p)
		}
	}
	return out
}

type explicitAnswer struct {
	Intent      string   `json:"intent"`
	About       string   `json:"about"`
	Fact        string   `json:"fact"`
	Key         string   `json:"key"`
	Instruction bool     `json:"instruction"`
	Sensitive   bool     `json:"sensitive"` // health, romance, money… (refused where guilds.<g>.privacy.no_sensitive)
	Target      []string `json:"target"`
}

type myItem struct {
	ID, Episode, Kind, Text, Room, Channel, Created string
	Evidence                                        []string
}

// explicitMemoryTurn handles an explicit memory request of a chat turn. It returns the tool result the
// answer confirms (nil when the message is not one) and what the classifier cost.
func (s *Service) explicitMemoryTurn(ctx context.Context, in ChatInput, voice chat.Voice, llm chatLLM, history []chat.Turn) (*chat.ToolResult, float64) {
	if in.Proactive || in.banter || in.DryRun || in.GuildID == "" || in.GuildID == "0" || in.SpeakerID == "" || s.Store == nil {
		return nil, 0
	}
	packs := s.explicitPacks(voice)
	if !explicitCued(packs, in.Message) {
		return nil, 0
	}
	db, err := s.adminDB()
	if err != nil || !db.IsSQLite() || !roomMemoryTables(db) {
		return nil, 0
	}
	ref := personRef(in.SpeakerID)
	room := roomOf(in.GuildID, in.ChannelID)
	labels := s.refLabels(db, room)
	labelOf := func(r string) string {
		if r == ref && in.Speaker != "" {
			return in.Speaker
		}
		return labels(r)
	}
	// what the member may be pointing at: what the bot keeps about them, then what bears on this talk
	candidates := s.myItems(db, in.GuildID, ref, nil, explicitItemsMax/2)
	last := ""
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == "assistant" {
			last = history[i].Content
			break
		}
	}
	for _, it := range s.relatedItems(ctx, db, in, voice, last) {
		if len(candidates) >= explicitItemsMax {
			break
		}
		dup := false
		for _, c := range candidates {
			dup = dup || c.ID == it.ID
		}
		if !dup {
			candidates = append(candidates, it)
		}
	}
	var lines []string
	for i, it := range candidates {
		lines = append(lines, fmt.Sprintf("[i%d] %s", i+1, clip(episodes.StoredText(it.Text, labelOf, "?"), 160)))
	}
	items := strings.Join(lines, "\n")
	if items == "" {
		items = voice.Pack.Chat.None
	}
	if last == "" {
		last = voice.Pack.Chat.None
	}
	m := voice.Pack.Memory
	msgs := []chat.Msg{
		{Role: "system", Content: lang.Fill(m.ExplicitSystem, "bot", voice.BotName)},
		{Role: "user", Content: lang.Fill(m.Explicit, "speaker", in.Speaker, "message", clip(in.Message, 600), "last", clip(last, 400), "items", items)},
	}
	cctx, cancel := context.WithTimeout(ctx, explicitTimeout)
	defer cancel()
	out, err := llm.ChatDetailed(withLedgerRole(cctx, "memory_explicit"), "planner", msgs, 220, 0)
	if err != nil {
		return nil, out.CostUSD
	}
	var a explicitAnswer
	if !decodeJSONObject(out.Text, &a) {
		return nil, out.CostUSD
	}
	targets := func(own bool) []myItem {
		var picked []myItem
		for _, t := range a.Target {
			var n int
			if _, err := fmt.Sscanf(strings.TrimSpace(t), "i%d", &n); err != nil || n < 1 || n > len(candidates) {
				continue
			}
			it := candidates[n-1]
			if own && !s.itemIsMine(db, it.ID, ref) {
				continue
			}
			picked = append(picked, it)
		}
		return picked
	}
	res := &chat.ToolResult{Tool: explicitTool, OK: true, Data: map[string]any{"request": a.Intent}}
	switch a.Intent {
	case "remember":
		return s.rememberFact(ctx, db, in, packs, a, ref, labelOf, res), out.CostUSD
	case "wrong":
		picked := targets(false)
		n, err := s.markWrong(ctx, db, picked)
		if err != nil {
			s.Log.WarnContext(ctx, "memory: correction not saved", "err", err)
			res.OK, res.Status = false, "error"
			return res, out.CostUSD
		}
		res.Data["hidden"] = n
		if n == 0 {
			res.Status = "no_match"
		}
		return res, out.CostUSD
	case "forget":
		picked := targets(true)
		ids := make([]string, 0, len(picked))
		for _, it := range picked {
			ids = append(ids, it.ID)
		}
		prev := s.forgetPreview(db, in.GuildID, ref, ids)
		_ = s.kvSet(forgetPendingKey(in.GuildID, ref), forgetPending{Items: ids, At: s.Clock.Now().UTC().Format(time.RFC3339)})
		res.Data["preview_items"] = len(prev.items)
		res.Data["applied"] = false
		res.Data["confirm_with"] = "/" + s.I18n.T(in.Locale, "discord.command.forgetme.name")
		var shown []string
		for i, it := range prev.items {
			if i == 3 {
				break
			}
			shown = append(shown, clip(episodes.StoredText(it.Text, labelOf, "?"), 80))
		}
		if len(shown) > 0 {
			res.Data["examples"] = shown
		}
		return res, out.CostUSD
	}
	return nil, out.CostUSD
}

// relatedItems are the active items of the episodes the read path would show for this message and the
// bot's last reply.
func (s *Service) relatedItems(ctx context.Context, db *storage.Store, in ChatInput, voice chat.Voice, last string) []myItem {
	q := s.readQuery(voice, in.Message, []string{last})
	eps := s.relatedEpisodes(ctx, db, s.channelRooms(in.GuildID, in.ChannelID), q, voice.Pack, false, nil, false)
	if len(eps) == 0 {
		return nil
	}
	ids := make([]string, 0, len(eps))
	for _, e := range eps {
		ids = append(ids, e.ID)
	}
	raw, _ := json.Marshal(ids)
	rows, err := db.Reader().Query(`SELECT i.id, i.episode_id, i.kind, i.text, i.evidence, e.room, e.channel_id, i.created_at FROM episode_item i JOIN channel_episode e ON e.id=i.episode_id
		WHERE i.state='active' AND i.episode_id IN (SELECT value FROM json_each(?)) ORDER BY i.created_at DESC LIMIT ?`, string(raw), explicitItemsMax)
	if err != nil {
		return nil
	}
	defer rows.Close()
	return scanItems(rows)
}

func scanItems(rows *sql.Rows) []myItem {
	var out []myItem
	for rows.Next() {
		var it myItem
		var ev string
		if rows.Scan(&it.ID, &it.Episode, &it.Kind, &it.Text, &ev, &it.Room, &it.Channel, &it.Created) == nil {
			_ = json.Unmarshal([]byte(ev), &it.Evidence)
			out = append(out, it)
		}
	}
	return out
}

// myItems are a person's active items in a server: the ones they said (by_refs) and the ones naming them,
// newest first; only, when ids is given, those of ids.
func (s *Service) myItems(db *storage.Store, guild, ref string, ids []string, limit int) []myItem {
	if guild == "" || ref == "" {
		return nil
	}
	q := `SELECT i.id, i.episode_id, i.kind, i.text, i.evidence, e.room, e.channel_id, i.created_at FROM episode_item i JOIN channel_episode e ON e.id=i.episode_id
		WHERE e.guild_id=? AND i.state='active' AND (EXISTS (SELECT 1 FROM json_each(i.by_refs) b WHERE b.value=?) OR instr(i.text, ?)>0)`
	args := []any{guild, ref, "{@" + ref + "}"}
	if ids != nil {
		raw, _ := json.Marshal(ids)
		q += ` AND i.id IN (SELECT value FROM json_each(?))`
		args = append(args, string(raw))
	}
	q += ` ORDER BY i.created_at DESC, i.rowid DESC LIMIT ?`
	args = append(args, limit)
	rows, err := db.Reader().Query(q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	return scanItems(rows)
}

func (s *Service) itemIsMine(db *storage.Store, id, ref string) bool {
	var n int
	_ = db.Reader().QueryRow(`SELECT COUNT(*) FROM episode_item i WHERE i.id=? AND (EXISTS (SELECT 1 FROM json_each(i.by_refs) b WHERE b.value=?) OR instr(i.text, ?)>0)`,
		id, ref, "{@"+ref+"}").Scan(&n)
	return n > 0
}

// rememberFact stores a fact the member asked to have remembered.
func (s *Service) rememberFact(ctx context.Context, db *storage.Store, in ChatInput, packs []*lang.Pack, a explicitAnswer, ref string, labelOf func(string) string, res *chat.ToolResult) *chat.ToolResult {
	fact := strings.Join(strings.Fields(a.Fact), " ")
	refuse := func(reason string) *chat.ToolResult {
		res.Data["saved"], res.Status = false, "refused"
		res.Data["reason"] = reason
		return res
	}
	switch {
	case fact == "":
		return nil
	case !s.MemoryAllowed(in.SpeakerID):
		return refuse("no_consent")
	case a.Instruction || instructionLike(packs, fact) || instructionLike(packs, in.Message):
		return refuse("instruction")
	case a.Sensitive && s.guildPrivacy(in.GuildID).NoSensitive:
		return refuse("sensitive")
	case utf8.RuneCountInString(fact) > episodes.MaxText:
		return refuse("too_long")
	}
	self := a.About != "other"
	text := strings.ReplaceAll(fact, "{p1}", "{@"+ref+"}")
	if strings.ContainsAny(storedRefPattern.ReplaceAllString(text, ""), "{}") {
		return refuse("instruction") // braces other than the speaker's token: template-like text is not kept
	}
	room := roomOf(in.GuildID, in.ChannelID)
	var obs string
	if in.MessageID != "" {
		_ = db.Reader().QueryRow(`SELECT observation_id FROM memory_observations WHERE room=? AND native_message_id=? AND deleted_at=''`, room, in.MessageID).Scan(&obs)
	}
	if obs == "" {
		return refuse("not_stored") // this channel does not store messages: nothing to rest the fact on
	}
	key := strings.ToLower(strings.TrimSpace(a.Key))
	if len(key) > 40 {
		key = key[:40]
	}
	importance := 3
	if self {
		importance = 5
	}
	now := s.Clock.Now().UTC().Format(time.RFC3339)
	saved := 0
	err := db.WithWriterContext(ctx, func(tx *sql.Tx) error {
		if ok, err := store.ObservationCurrent(tx, obs); err != nil || !ok {
			if err != nil {
				return err
			}
			return errEvidenceDeleted
		}
		id := newID("ep_")
		n, err := s.insertItems(tx, id, []episodes.Item{{Kind: "fact", Text: text, Evidence: []string{obs}, By: []string{ref}}}, now)
		if err != nil {
			return err
		}
		if n == 0 {
			return errNoItemsSaved
		}
		// Do not replace or hide anything until the new fact has passed the consent check in insertItems.
		if self && key != "" {
			rows, err := tx.Query(`SELECT episode_id FROM memory_explicit WHERE guild_id=? AND person_ref=? AND key=?`, in.GuildID, ref, key)
			if err != nil {
				return err
			}
			var old []string
			for rows.Next() {
				var oldID string
				if err := rows.Scan(&oldID); err != nil {
					rows.Close()
					return err
				}
				old = append(old, oldID)
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return err
			}
			rows.Close()
			for _, oldID := range old { // the newer fact on the same topic replaces the older one
				if err := deleteEpisodeTx(tx, oldID); err != nil {
					return err
				}
				if _, err := tx.Exec(`DELETE FROM memory_explicit WHERE episode_id=?`, oldID); err != nil {
					return err
				}
			}
		}
		if self {
			if err := s.supersedeAutoFactsForExplicit(tx, in.GuildID, ref, obs, text); err != nil {
				return err
			}
		}
		participants, _ := json.Marshal([]string{ref})
		seg, _ := json.Marshal([]string{obs})
		if _, err := tx.Exec(`INSERT INTO channel_episode(id,guild_id,channel_id,room,kind,title,status,due_at,when_text,importance,source,participants,segment,
			first_seen,last_seen,expires_at,supersedes_id,updated_at,revision,regen) VALUES(?,?,?,?,'fact',?,?,'','',?,?,?,?,?,?,'','',?,1,3)`,
			id, in.GuildID, in.ChannelID, room, text, episodes.Done, importance, explicitSource, string(participants), string(seg), now, now, now); err != nil {
			return err
		}
		if err := linkLeaves(tx, id, []string{obs}); err != nil {
			return err
		}
		if self {
			if _, err := tx.Exec(`INSERT INTO memory_explicit(episode_id,guild_id,person_ref,key,created_at) VALUES(?,?,?,?,?)`, id, in.GuildID, ref, key, now); err != nil {
				return err
			}
		}
		saved = n
		return nil
	})
	if errors.Is(err, errEvidenceDeleted) {
		return refuse("not_stored")
	}
	if errors.Is(err, errNoItemsSaved) {
		return refuse("no_consent")
	}
	if err != nil {
		s.Log.WarnContext(ctx, "memory: remembered fact not saved", "err", err)
		res.OK, res.Status = false, "error"
		return res
	}
	if saved == 0 {
		return refuse("no_consent")
	}
	if self && s.Doc().Memory.CardsOn() {
		if _, err := s.promoteCards(ctx, db); err != nil {
			s.Log.WarnContext(ctx, "memory: person cards not updated", "err", err)
		}
	}
	s.emit("memory", "episodes", nil)
	res.Data["saved"] = true
	res.Data["fact"] = episodes.StoredText(text, labelOf, "?")
	if !self {
		res.Data["about"] = "other"
	}
	return res
}

// supersedeAutoFactsForExplicit hides active automatic facts that satisfy the shared exact-evidence,
// person, and text-similarity predicate. The explicit episode then becomes the sole active copy.
func (s *Service) supersedeAutoFactsForExplicit(tx *sql.Tx, guildID, ref, obs, explicitText string) error {
	rows, err := tx.Query(`SELECT i.id,i.episode_id,i.text,i.evidence,i.by_refs FROM episode_item i JOIN channel_episode e ON e.id=i.episode_id
		WHERE e.guild_id=? AND e.source<>? AND i.kind='fact' AND i.state='active'
		AND EXISTS (SELECT 1 FROM json_each(i.evidence) ev WHERE ev.value=?)`, guildID, explicitSource, obs)
	if err != nil {
		return err
	}
	type found struct {
		item, episode string
		evidence      []string
	}
	var duplicates []found
	for rows.Next() {
		var d found
		var text, evidenceJSON, byJSON string
		if err := rows.Scan(&d.item, &d.episode, &text, &evidenceJSON, &byJSON); err != nil {
			rows.Close()
			return err
		}
		auto := episodes.Item{Kind: "fact", Text: text}
		_ = json.Unmarshal([]byte(evidenceJSON), &auto.Evidence)
		_ = json.Unmarshal([]byte(byJSON), &auto.By)
		explicit := episodes.Item{Kind: "fact", Text: explicitText, Evidence: []string{obs}, By: []string{ref}}
		if sameExplicitFact(auto, explicit, ref) {
			d.evidence = auto.Evidence
			duplicates = append(duplicates, d)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	touched := map[string]bool{}
	var evidence []string
	for _, d := range duplicates {
		res, err := tx.Exec(`UPDATE episode_item SET state='hidden' WHERE id=? AND state='active'`, d.item)
		if err != nil {
			return err
		}
		if hidden, _ := res.RowsAffected(); hidden > 0 {
			touched[d.episode] = true
			evidence = append(evidence, d.evidence...)
		}
	}
	if len(touched) > 0 {
		if err := deleteCardsCitingForPerson(tx, guildID, ref, evidence); err != nil {
			return err
		}
	}
	return deleteEmptyEpisodes(tx, touched)
}

// sameExplicitFact is shared by automatic extraction filtering and explicit-save supersession.
func sameExplicitFact(auto, explicit episodes.Item, ref string) bool {
	if auto.Kind != "fact" || explicit.Kind != "fact" || ref == "" ||
		len(auto.Evidence) != 1 || len(explicit.Evidence) != 1 || auto.Evidence[0] != explicit.Evidence[0] ||
		!explicitContains(auto.By, ref) || !explicitContains(episodes.TextRefs(auto.Text), ref) ||
		!explicitContains(explicit.By, ref) || !explicitContains(episodes.TextRefs(explicit.Text), ref) {
		return false
	}
	return explicitFactDice(auto.Text, explicit.Text) >= explicitFactDiceThreshold
}

func explicitFactDice(a, b string) float64 {
	a, b = explicitFactText(a), explicitFactText(b)
	left, right := runeBigrams(a), runeBigrams(b)
	if len(left) == 0 || len(right) == 0 {
		return 0
	}
	common := 0
	for pair := range left {
		if _, ok := right[pair]; ok {
			common++
		}
	}
	return 2 * float64(common) / float64(len(left)+len(right))
}

func explicitFactText(text string) string {
	text = explicitFactRefTokens.ReplaceAllString(text, "")
	var out strings.Builder
	for _, r := range strings.ToLower(text) {
		if !unicode.IsSpace(r) && !unicode.IsPunct(r) {
			out.WriteRune(r)
		}
	}
	return out.String()
}

func runeBigrams(text string) map[string]struct{} {
	runes := []rune(text)
	out := make(map[string]struct{}, max(0, len(runes)-1))
	for i := 0; i+1 < len(runes); i++ {
		out[string(runes[i:i+2])] = struct{}{}
	}
	return out
}

func explicitContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// markWrong hides the items a member said are wrong. Their episodes are never made again from the same
// lines (regen=3), and an episode left with nothing goes.
func (s *Service) markWrong(ctx context.Context, db *storage.Store, items []myItem) (int, error) {
	if len(items) == 0 {
		return 0, nil
	}
	n := 0
	err := db.WithWriterContext(ctx, func(tx *sql.Tx) error {
		touched := map[string]bool{}
		var evidence []string
		for _, it := range items {
			res, err := tx.Exec(`UPDATE episode_item SET state='hidden' WHERE id=? AND state='active'`, it.ID)
			if err != nil {
				return err
			}
			if c, _ := res.RowsAffected(); c > 0 {
				n++
				touched[it.Episode] = true
				evidence = append(evidence, it.Evidence...)
			}
		}
		if err := s.closeCorrected(tx, touched); err != nil {
			return err
		}
		return deleteCardsCiting(tx, evidence)
	})
	if n > 0 {
		s.emit("memory", "episodes", nil)
	}
	return n, err
}

// closeCorrected settles episodes a member corrected or forgot from: deleted when nothing active is
// left, else never regenerated from the same lines.
func (s *Service) closeCorrected(tx *sql.Tx, touched map[string]bool) error {
	for id := range touched {
		var active int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM episode_item WHERE episode_id=? AND state='active'`, id).Scan(&active); err != nil {
			return err
		}
		if active == 0 {
			if err := deleteEpisodeTx(tx, id); err != nil {
				return err
			}
			if _, err := tx.Exec(`DELETE FROM memory_explicit WHERE episode_id=?`, id); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.Exec(`UPDATE channel_episode SET regen=3, revision=revision+1 WHERE id=?`, id); err != nil {
			return err
		}
	}
	return nil
}

// deleteEmptyEpisodes removes only touched episodes left without active items. It does not change the
// regeneration or revision state of episodes that still contain useful items.
func deleteEmptyEpisodes(tx *sql.Tx, touched map[string]bool) error {
	for id := range touched {
		var active int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM episode_item WHERE episode_id=? AND state='active'`, id).Scan(&active); err != nil {
			return err
		}
		if active == 0 {
			if err := deleteEpisodeTx(tx, id); err != nil {
				return err
			}
			if _, err := tx.Exec(`DELETE FROM memory_explicit WHERE episode_id=?`, id); err != nil {
				return err
			}
		}
	}
	return nil
}

// deleteCardsCiting deletes card items resting on any of the observations.
func deleteCardsCiting(tx *sql.Tx, observations []string) error {
	if len(observations) == 0 || !tableExists(tx, "person_card_item") {
		return nil
	}
	raw, _ := json.Marshal(uniqueStrings(observations))
	rows, err := tx.Query(`SELECT DISTINCT holder_id FROM evidence_refs WHERE holder_kind=? AND source_id IN (SELECT value FROM json_each(?))`, holderCardItem, string(raw))
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		if err := deleteCardItemTx(tx, id); err != nil {
			return err
		}
	}
	return nil
}

// deleteCardsCitingForPerson invalidates only this person's card lines based on the hidden items' evidence.
func deleteCardsCitingForPerson(tx *sql.Tx, guildID, ref string, observations []string) error {
	if len(observations) == 0 || !tableExists(tx, "person_card_item") {
		return nil
	}
	raw, _ := json.Marshal(uniqueStrings(observations))
	rows, err := tx.Query(`SELECT DISTINCT p.id FROM person_card_item p JOIN evidence_refs r
		ON r.holder_kind=? AND r.holder_id=p.id
		WHERE p.guild_id=? AND p.person_ref=? AND p.state='active'
		AND r.source_id IN (SELECT value FROM json_each(?))`, holderCardItem, guildID, ref, string(raw))
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, id := range ids {
		if err := deleteCardItemTx(tx, id); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// forgetting one's own memory (memory.mine.forget, /forgetme, a chat "forget that")
// ---------------------------------------------------------------------------

type forgetPending struct {
	Items []string `json:"items"`
	At    string   `json:"at"`
}

func forgetPendingKey(guild, ref string) string { return "memory:forget:pending:" + guild + ":" + ref }

type forgetPlan struct {
	items    []myItem
	episodes []string // episodes the person is a participant of
	cards    int
	all      bool
	token    string
}

// forgetPreview is what forgetting would delete: the person's items (all, or the given ones), their
// place among the participants (all only) and their card. token fingerprints it, so a confirmation
// applies exactly what was shown.
func (s *Service) forgetPreview(db *storage.Store, guild, ref string, ids []string) forgetPlan {
	p := forgetPlan{all: len(ids) == 0}
	if p.all {
		p.items = s.myItems(db, guild, ref, nil, 1000)
		rows, err := db.Reader().Query(`SELECT DISTINCT e.id FROM channel_episode e, json_each(e.participants) x WHERE e.guild_id=? AND x.value=?`, guild, ref)
		if err == nil {
			for rows.Next() {
				var id string
				if rows.Scan(&id) == nil {
					p.episodes = append(p.episodes, id)
				}
			}
			rows.Close()
		}
		_ = db.Reader().QueryRow(`SELECT COUNT(*) FROM person_card_item WHERE guild_id=? AND person_ref=? AND state='active'`, guild, ref).Scan(&p.cards)
	} else {
		p.items = s.myItems(db, guild, ref, ids, len(ids))
	}
	var keys []string
	for _, it := range p.items {
		keys = append(keys, "i:"+it.ID)
	}
	for _, e := range p.episodes {
		keys = append(keys, "e:"+e)
	}
	sort.Strings(keys)
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%t|%d|%s", guild, ref, p.all, p.cards, strings.Join(keys, ","))))
	p.token = hex.EncodeToString(sum[:6])
	return p
}

// applyForget deletes what the plan shows.
func (s *Service) applyForget(ctx context.Context, db *storage.Store, guild, ref string, p forgetPlan) (int, error) {
	deleted := 0
	err := db.WithWriterContext(ctx, func(tx *sql.Tx) error {
		touched := map[string]bool{}
		var evidence []string
		for _, it := range p.items {
			if _, err := tx.Exec(`DELETE FROM evidence_refs WHERE holder_kind=? AND holder_id=?`, store.HolderEpisodeItem, it.ID); err != nil {
				return err
			}
			res, err := tx.Exec(`DELETE FROM episode_item WHERE id=?`, it.ID)
			if err != nil {
				return err
			}
			if c, _ := res.RowsAffected(); c > 0 {
				deleted++
			}
			touched[it.Episode] = true
			evidence = append(evidence, it.Evidence...)
		}
		for _, e := range p.episodes {
			if err := dropParticipant(tx, e, ref); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			touched[e] = true
		}
		if err := s.closeCorrected(tx, touched); err != nil {
			return err
		}
		if p.all {
			if _, err := tx.Exec(`DELETE FROM memory_explicit WHERE guild_id=? AND person_ref=?`, guild, ref); err != nil {
				return err
			}
			n, err := deleteCardsOf(tx, guild, ref)
			if err != nil {
				return err
			}
			deleted += n
			// a room state naming the person goes too (the next update makes it again without them)
			rows, err := tx.Query(`SELECT DISTINCT holder_id FROM evidence_refs WHERE holder_kind=? AND source_id=?`, store.HolderChannelState, ref)
			if err != nil {
				return err
			}
			var states []string
			for rows.Next() {
				var id string
				if rows.Scan(&id) == nil && strings.HasPrefix(id, "state:"+guild+":") {
					states = append(states, id)
				}
			}
			rows.Close()
			for _, id := range states {
				if err := deleteStateTx(tx, id); err != nil {
					return err
				}
			}
			return nil
		}
		return deleteCardsCiting(tx, evidence)
	})
	if err == nil {
		_ = s.kvSet(forgetPendingKey(guild, ref), forgetPending{})
		s.emit("memory", "episodes", nil)
	}
	return deleted, err
}

// ---------------------------------------------------------------------------
// actions: memory.mine.show, memory.mine.forget
// ---------------------------------------------------------------------------

// mineAction serves the personal memory Actions.
//
// memory.mine.show {guild_id, user_id} →
//
//	{"items":[{"id","episode_id","kind","text","channel_id","created_at","explicit"}], "episodes":N, "person_card":[text], "console":"/memory"}
//
// memory.mine.forget {guild_id, user_id, confirm?, scope?: pending|all} →
//
//	preview: {"applied":false,"scope","items":N,"episodes":N,"cards":N,"token"}
//	confirm: {"applied":true,"deleted":N} — refused with a fresh preview when what it would delete changed.
//
// From Discord the invoker is always the person and the server is where the command ran; the console (an
// operator) names them.
func (s *Service) mineAction(ctx context.Context, req action.Request) action.Result {
	db, err := s.adminDB()
	if err != nil || !db.IsSQLite() || s.Store == nil || !roomMemoryTables(db) {
		return action.Fail(req, errcode.NotSetup, "generic.error", "room memory needs the SQLite memory store", nil)
	}
	in := inputMap(req)
	guild, user := strVal(in, "guild_id", ""), strVal(in, "user_id", "")
	if inv := req.Invoker; inv != nil && inv.UserID != "" && (inv.Via == "command" || inv.Via == "chat" || !s.IsOperator(inv.UserID)) {
		guild, user = inv.GuildID, inv.UserID // a member only ever sees and forgets their own
	}
	if guild == "" || user == "" {
		return action.Fail(req, errcode.InvalidInput, "memory.mine.server_only", s.msg(req, "memory.mine.server_only"), nil)
	}
	ref := personRef(user)
	command := req.Invoker != nil && req.Invoker.Via == "command"
	switch req.Action {
	case "memory.mine.show":
		items := s.myItems(db, guild, ref, nil, 50)
		byRoom := map[string]func(string) string{}
		labelIn := func(room string) func(string) string {
			f, ok := byRoom[room]
			if !ok {
				f = s.refLabels(db, room)
				byRoom[room] = f
			}
			return func(r string) string {
				if r == ref && req.Invoker != nil && req.Invoker.Name != "" {
					return req.Invoker.Name
				}
				return f(r)
			}
		}
		// what the person asked to have remembered ("remember this"), so the console can tell it apart
		told := map[string]bool{}
		if rows, err := db.Reader().Query(`SELECT episode_id FROM memory_explicit WHERE guild_id=? AND person_ref=?`, guild, ref); err == nil {
			for rows.Next() {
				var id string
				if rows.Scan(&id) == nil {
					told[id] = true
				}
			}
			rows.Close()
		}
		list := []map[string]any{}
		for _, it := range items {
			list = append(list, map[string]any{"id": it.ID, "episode_id": it.Episode, "kind": it.Kind, "text": episodes.StoredText(it.Text, labelIn(it.Room), "?"),
				"channel_id": it.Channel, "created_at": it.Created, "explicit": told[it.Episode]})
		}
		channel := ""
		if req.Invoker != nil {
			channel = req.Invoker.ChannelID
		}
		labelRoom := roomOf(guild, channel)
		if channel == "" && len(items) > 0 {
			labelRoom = items[0].Room // the console names no channel: names come from the newest item's room
		}
		labelOf := labelIn(labelRoom)
		var eps int
		_ = db.Reader().QueryRow(`SELECT COUNT(DISTINCT e.id) FROM channel_episode e, json_each(e.participants) x WHERE e.guild_id=? AND x.value=?`, guild, ref).Scan(&eps)
		card := []string{}
		if tableReady(db, "person_card_item") {
			card = append(card, s.cardOf(db, guild, ref, labelOf)...)
		}
		out := map[string]any{"items": list, "episodes": eps, "person_card": card, "console": "/memory"}
		if u := s.consoleLink("/memory"); u != "" {
			out["console_url"] = u
		}
		res := action.OK(req, out)
		if command {
			res = action.WithCard(res, s.mineCard(req, list, eps, card))
		}
		return res
	case "memory.mine.forget":
		var ids []string
		scope := strVal(in, "scope", "")
		if scope == "" || scope == "pending" {
			var pend forgetPending
			if ok, _ := s.kvGet(forgetPendingKey(guild, ref), &pend); ok && len(pend.Items) > 0 {
				if at, err := time.Parse(time.RFC3339, pend.At); err == nil && s.Clock.Now().Sub(at) < forgetPendingLife {
					ids, scope = pend.Items, "pending"
				}
			}
		}
		if ids == nil {
			scope = "all"
		}
		plan := s.forgetPreview(db, guild, ref, ids)
		preview := map[string]any{"applied": false, "scope": scope, "items": len(plan.items), "episodes": len(plan.episodes), "cards": plan.cards, "token": plan.token}
		confirm := strVal(in, "confirm", "")
		if confirm == "" || confirm != plan.token {
			if confirm != "" {
				preview["stale"] = true // what it would delete changed since the preview
			}
			res := action.OK(req, preview)
			if command {
				res = action.WithCard(res, s.forgetCard(req, plan, scope, confirm != ""))
			}
			return res
		}
		n, err := s.applyForget(ctx, db, guild, ref, plan)
		if err != nil {
			return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
		}
		res := action.OK(req, map[string]any{"applied": true, "deleted": n, "scope": scope})
		if command {
			res = action.WithCard(res, action.Card{Title: s.msg(req, "memory.forget.title"), Text: s.msg(req, "memory.forget.done", "n", n), Ephemeral: true})
		}
		return res
	}
	return action.Fail(req, errcode.ActionUnknown, "generic.error", req.Action, nil)
}

func tableReady(db *storage.Store, name string) bool {
	var n int
	return db.Reader().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name=?`, name).Scan(&n) == nil && n > 0
}

// consoleLink is the console page's address when the console listens beyond this machine ("" when it
// only listens locally: members could not open it).
func (s *Service) consoleLink(path string) string {
	bind := strings.TrimSpace(s.Doc().Admin.Web.Bind)
	if !s.Doc().Admin.Web.Enabled || bind == "" {
		return ""
	}
	host, _, err := net.SplitHostPort(bind)
	if err != nil {
		host = bind
	}
	if ip := net.ParseIP(host); host == "" || host == "localhost" || (ip != nil && (ip.IsLoopback() || ip.IsUnspecified())) {
		return ""
	}
	return "http://" + bind + path
}

func (s *Service) mineCard(req action.Request, items []map[string]any, eps int, card []string) action.Card {
	c := action.Card{Title: s.msg(req, "memory.mine.title"), Ephemeral: true, Tone: "neutral", Footer: s.msg(req, "memory.mine.footer")}
	if len(items) == 0 && len(card) == 0 {
		c.Text = s.msg(req, "memory.mine.empty")
		return c
	}
	var lines []string
	for i, it := range items {
		if i == 15 {
			lines = append(lines, s.msg(req, "memory.mine.more", "n", len(items)-15))
			break
		}
		lines = append(lines, "• "+fmt.Sprint(it["text"]))
	}
	c.Text = strings.Join(lines, "\n")
	if len(card) > 0 {
		c.Fields = append(c.Fields, action.CardField{Name: s.msg(req, "memory.mine.card"), Value: "• " + strings.Join(card, "\n• ")})
	}
	c.Fields = append(c.Fields, action.CardField{Name: s.msg(req, "memory.mine.episodes"), Value: fmt.Sprint(eps), Inline: true})
	if u := s.consoleLink("/memory"); u != "" {
		c.Buttons = append(c.Buttons, action.CardButton{Label: s.msg(req, "memory.mine.console"), Style: "link", URL: u})
	}
	return c
}

func (s *Service) forgetCard(req action.Request, p forgetPlan, scope string, stale bool) action.Card {
	c := action.Card{Title: s.msg(req, "memory.forget.title"), Ephemeral: true, Tone: "warn"}
	if len(p.items) == 0 && len(p.episodes) == 0 && p.cards == 0 {
		c.Text, c.Tone = s.msg(req, "memory.forget.nothing"), "neutral"
		return c
	}
	text := s.msg(req, "memory.forget.preview", "items", len(p.items), "episodes", len(p.episodes), "cards", p.cards)
	if stale {
		text = s.msg(req, "memory.forget.stale") + "\n" + text
	}
	c.Text = text
	sc := "a"
	if scope == "pending" {
		sc = "p"
	}
	c.Buttons = []action.CardButton{{ID: "mem:forget:" + sc + ":" + p.token, Label: s.msg(req, "memory.forget.confirm"), Style: "danger"}}
	return c
}
