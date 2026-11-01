package application

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"time"

	"masamong/internal/chat"
	"masamong/internal/config"
	"masamong/internal/lang"
	"masamong/internal/memory/corpus"
	"masamong/internal/memory/episodes"
	"masamong/internal/memory/lexical"
	"masamong/internal/memory/store"
	"masamong/internal/storage"
)

// The read path of room memory (docs/plan-memory-conversation.md §5.4.6, P4-3): what a chat turn is
// given from the room's episodes (L2), live topics (L1) and person cards (L3). No model is called: the
// episodes and topics that share words with the caller's conversation (the language pack's tokenizer),
// and the open plans of the channel's rooms when the conversation is about plans, are read from the
// database, and every evidence line is read again from the raw messages as they stand now — a deleted,
// edited-since or withdrawn line is left out, and an episode with no line left is not shown at all.
//
// Scope: the channel's rooms (channelRooms) — its own, its threads' or its parent's, and the KakaoTalk
// blocks an operator linked to it. A Kakao episode is therefore only ever seen in a linked channel, and
// it is labelled with its block.

const (
	episodeCandidates = 200 // the most recent episodes of the rooms scored per turn
	contextLines      = 6   // lines of the caller's conversation besides the message
)

// turnRoomMemory gathers the room memory of one chat turn; nil when there is none or reading is off.
func (s *Service) turnRoomMemory(ctx context.Context, in ChatInput, pol config.ChannelPolicy, voice chat.Voice, history []chat.Turn, recent []corpus.Message) *chat.RoomMemory {
	doc := s.Doc()
	if pol.MemoryUse == "off" || in.ChannelID == "" || s.Store == nil {
		return nil
	}
	if !doc.Memory.EpisodesOn() && !doc.Memory.StateOn() && !doc.Memory.CardsOn() {
		return nil
	}
	db, err := s.adminDB()
	if err != nil || !db.IsSQLite() || !roomMemoryTables(db) {
		return nil
	}
	var contextText []string
	for i := max(0, len(history)-contextLines); i < len(history); i++ {
		contextText = append(contextText, history[i].Content)
	}
	for i := max(0, len(recent)-contextLines); i < len(recent); i++ {
		contextText = append(contextText, recent[i].Content)
	}
	q := s.readQuery(voice, in.Message, contextText)
	var sources []string
	if in.sourceRefs != nil {
		sources = *in.sourceRefs
	}
	// lines the prompt already shows (recent lines, the caller's conversation) are not repeated as
	// evidence; an episode resting only on them adds nothing and is left out
	shown := map[string]bool{}
	for _, id := range sources {
		shown[id] = true
	}
	for _, m := range recent {
		shown[m.ObservationID] = true
	}
	rm := &chat.RoomMemory{Packs: s.explicitPacks(voice)} // their instruction cues keep instructions out of the reply
	rooms := s.channelRooms(in.GuildID, in.ChannelID)
	if doc.Memory.EpisodesOn() {
		rm.Episodes = s.relatedEpisodes(ctx, db, rooms, q, voice.Pack, planTalk(voice.Pack, in.Message), shown, s.guildPrivacy(in.GuildID).NoHearsay)
	}
	if doc.Memory.StateOn() {
		rm.Topics = s.linkedTopics(db, in, q, sources, s.guildPrivacy(in.GuildID).NoHearsay)
	}
	if doc.Memory.CardsOn() {
		rm.Cards = s.turnCards(db, in, recent, sources)
	}
	if len(rm.Episodes) == 0 && len(rm.Topics) == 0 && len(rm.Cards) == 0 {
		return nil
	}
	return rm
}

func roomMemoryTables(db *storage.Store) bool {
	var n int
	return db.Reader().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='channel_episode'`).Scan(&n) == nil && n > 0
}

// readQuery builds the caller's conversation as words, with the tokenizer of the memory's languages
// and of the reply's language.
func (s *Service) readQuery(voice chat.Voice, message string, context []string) episodes.Query {
	tags := append(append([]string{}, s.Doc().Memory.Languages...), voice.Pack.Tag)
	sr := lang.MergeSearch(tags, s.Doc().BotNames())
	tok := lexical.FromLanguage(sr)
	return episodes.NewQuery(&tok, sr.Particles, message, context)
}

// planTalk reports whether a message talks about plans or asks the group's memory (the pack's optional
// plan and recall words): then the channel's open plans are related even without a shared word.
func planTalk(p *lang.Pack, message string) bool {
	m := strings.ToLower(message)
	for _, list := range [][]string{p.Time.PlanWords, p.Time.Recall} {
		for _, w := range list {
			if w = strings.ToLower(strings.TrimSpace(w)); w != "" && strings.Contains(m, w) {
				return true
			}
		}
	}
	return false
}

type episodeCandidate struct {
	id, room, source, kind, title, status, due, when string
	importance                                       int
	lastSeen                                         string
	items                                            []itemRow
	score                                            int
	openPlan                                         bool
}

type itemRow struct {
	id, kind, text string
	evidence       []string
	by             []string // the people who said it
}

// hearsay is an item about someone that only other people said (guilds.<g>.privacy.no_hearsay leaves it
// out of what a reply sees).
func (it itemRow) hearsay() bool {
	for _, r := range episodes.TextRefs(it.text) {
		if !slices.Contains(it.by, r) {
			return true
		}
	}
	return false
}

// namesOthers reports whether a stored text mentions anyone outside people.
func namesOthers(text string, people []string) bool {
	for _, r := range episodes.TextRefs(text) {
		if !slices.Contains(people, r) {
			return true
		}
	}
	return false
}

// relatedEpisodes picks up to chat.MaxEpisodes episodes of the rooms related to the conversation, with
// their live evidence, best first. Evidence in shown is not repeated.
func (s *Service) relatedEpisodes(ctx context.Context, db *storage.Store, rooms []string, q episodes.Query, pack *lang.Pack, plans bool, shown map[string]bool, noHearsay bool) []chat.RoomEpisode {
	if len(rooms) == 0 {
		return nil
	}
	cands := loadEpisodeCandidates(db, rooms)
	if noHearsay {
		kept := cands[:0]
		for _, c := range cands {
			items := c.items[:0]
			var speakers []string
			for _, it := range c.items {
				if !it.hearsay() {
					items = append(items, it)
					speakers = append(speakers, it.by...)
				}
			}
			// a title naming someone none of the shown items' speakers is ("{@A} broke up") is hearsay too
			if c.items = items; len(items) > 0 && !namesOthers(c.title, speakers) {
				kept = append(kept, c)
			}
		}
		cands = kept
	}
	if len(cands) == 0 {
		return nil
	}
	labels := map[string]func(string) string{}
	labelOf := func(room string) func(string) string {
		if f, ok := labels[room]; ok {
			return f
		}
		f := s.refLabels(db, room)
		labels[room] = f
		return f
	}
	// how many candidates each word is found in: a word found in most of them tells nothing apart
	matches := make([][]int, len(cands))
	df := make([]int, q.Words())
	for i := range cands {
		c := &cands[i]
		var b strings.Builder
		b.WriteString(episodes.StoredText(c.title, labelOf(c.room), ""))
		for _, it := range c.items {
			b.WriteString("\n" + episodes.StoredText(it.text, labelOf(c.room), ""))
		}
		matches[i] = q.Matches(b.String())
		for _, w := range matches[i] {
			df[w]++
		}
	}
	// only in a room with many episodes: in a small one (a hiking club's five hikes) the shared word is
	// what the talk is about, and the best few by importance and recency are the right ones
	common := len(cands) >= 10
	for i := range cands {
		c := &cands[i]
		for _, w := range matches[i] {
			if common && df[w]*5 > len(cands)*3 {
				continue
			}
			if q.FromMessage(w) {
				c.score += 2
			} else {
				c.score++
			}
		}
		c.openPlan = plans && c.kind == "plan" && (c.status == episodes.Open || c.status == episodes.Decided)
	}
	var picked []*episodeCandidate
	for i := range cands {
		if cands[i].score >= 2 || cands[i].openPlan {
			picked = append(picked, &cands[i])
		}
	}
	sort.SliceStable(picked, func(i, j int) bool {
		a, b := picked[i], picked[j]
		if a.score != b.score {
			return a.score > b.score
		}
		if a.importance != b.importance {
			return a.importance > b.importance
		}
		return a.lastSeen > b.lastSeen
	})
	tx, err := db.Reader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil
	}
	defer func() { _ = tx.Rollback() }()
	var out []chat.RoomEpisode
	for _, c := range picked {
		if len(out) == chat.MaxEpisodes {
			break
		}
		ev := s.evidenceNow(tx, c.items, chat.MaxEvidenceLines, shown)
		if len(ev) == 0 {
			continue // nothing it rests on stands any more, or all of it is in the prompt already
		}
		e := chat.RoomEpisode{ID: c.id, Title: episodes.StoredText(c.title, labelOf(c.room), "?"), Evidence: ev, Leaves: episodeLeaves(tx, c.id)}
		if c.kind == "plan" || c.kind == "decision" {
			e.Status = pack.Memory.Statuses[c.status]
		}
		e.When = c.when
		if e.When == "" && c.due != "" {
			e.When = c.due
		}
		if strings.HasPrefix(c.source, "kakao:") {
			label := strings.TrimPrefix(c.source, "kakao:")
			if b, ok := s.lookupBlock(label); ok && b.Label != "" {
				label = b.Label
			}
			e.Source = lang.Fill(pack.Memory.KakaoSource, "label", label)
		}
		out = append(out, e)
	}
	return out
}

func loadEpisodeCandidates(db *storage.Store, rooms []string) []episodeCandidate {
	raw, _ := json.Marshal(rooms)
	rows, err := db.Reader().Query(`SELECT id, room, source, kind, title, status, due_at, when_text, importance, last_seen FROM channel_episode
		WHERE room IN (SELECT value FROM json_each(?)) ORDER BY last_seen DESC LIMIT ?`, string(raw), episodeCandidates)
	if err != nil {
		return nil
	}
	var cands []episodeCandidate
	byID := map[string]int{}
	for rows.Next() {
		var c episodeCandidate
		if rows.Scan(&c.id, &c.room, &c.source, &c.kind, &c.title, &c.status, &c.due, &c.when, &c.importance, &c.lastSeen) == nil {
			byID[c.id] = len(cands)
			cands = append(cands, c)
		}
	}
	rows.Close()
	if len(cands) == 0 {
		return nil
	}
	ids := make([]string, 0, len(cands))
	for _, c := range cands {
		ids = append(ids, c.id)
	}
	rawIDs, _ := json.Marshal(ids)
	// only active items count: a hidden or dirty item is neither shown nor matched
	irows, err := db.Reader().Query(`SELECT episode_id, id, kind, text, evidence, by_refs FROM episode_item WHERE state='active'
		AND episode_id IN (SELECT value FROM json_each(?)) ORDER BY created_at, rowid`, string(rawIDs))
	if err != nil {
		return nil
	}
	defer irows.Close()
	for irows.Next() {
		var ep, evRaw, byRaw string
		var it itemRow
		if irows.Scan(&ep, &it.id, &it.kind, &it.text, &evRaw, &byRaw) != nil {
			continue
		}
		_ = json.Unmarshal([]byte(evRaw), &it.evidence)
		_ = json.Unmarshal([]byte(byRaw), &it.by)
		if i, ok := byID[ep]; ok {
			cands[i].items = append(cands[i].items, it)
		}
	}
	out := cands[:0]
	for _, c := range cands {
		if len(c.items) > 0 {
			out = append(out, c)
		}
	}
	return out
}

// evidenceNow reads up to n evidence lines of the items now — decisions first — leaving out any line
// that was deleted, edited after it was cited, withdrawn, or written by a bot.
func (s *Service) evidenceNow(tx *sql.Tx, items []itemRow, n int, shown map[string]bool) []chat.RoomLine {
	ordered := append([]itemRow(nil), items...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].kind == "decision" && ordered[j].kind != "decision" })
	seen := map[string]bool{}
	for id := range shown {
		seen[id] = true
	}
	naive := s.naiveZone()
	loc := s.instanceZone()
	var out []chat.RoomLine
	for _, it := range ordered {
		for _, id := range it.evidence {
			if len(out) == n {
				return out
			}
			if seen[id] {
				continue
			}
			seen[id] = true
			if l, ok := s.liveLine(tx, id, naive, loc); ok {
				out = append(out, l)
			}
		}
	}
	return out
}

func (s *Service) liveLine(tx *sql.Tx, id string, naive, loc *time.Location) (chat.RoomLine, bool) {
	var author, text, at, deleted, kind string
	if tx.QueryRow(`SELECT actor_label, content, parsed_time, deleted_at, record_kind FROM memory_observations WHERE observation_id=?`, id).
		Scan(&author, &text, &at, &deleted, &kind) != nil || deleted != "" || kind != corpus.HumanText || strings.TrimSpace(text) == "" {
		return chat.RoomLine{}, false
	}
	if ok, err := store.ObservationCurrent(tx, id); err != nil || !ok {
		return chat.RoomLine{}, false
	}
	l := chat.RoomLine{Author: author, Text: strings.TrimSpace(text)}
	if t, ok := s.obsTime(at, naive); ok {
		l.Date = t.In(loc).Format("01-02")
	}
	return l, true
}

func episodeLeaves(tx *sql.Tx, id string) []string {
	rows, err := tx.Query(`SELECT leaf_id FROM episode_leaf WHERE episode_id=?`, id)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var l string
		if rows.Scan(&l) == nil {
			out = append(out, l)
		}
	}
	return out
}

// linkedTopics are the live topics of the channel (and of its parent, for a thread) linked to the
// caller's conversation: a topic citing a line of that conversation, one the caller is in, or one that
// shares words with it.
// With noHearsay a topic whose label or gist names someone who said none of its lines is left out, and
// only the people who said them are listed.
func (s *Service) linkedTopics(db *storage.Store, in ChatInput, q episodes.Query, sources []string, noHearsay bool) []chat.RoomTopic {
	channels := []string{in.ChannelID}
	if parent := s.threadParent(in.ChannelID); parent != "" {
		channels = append(channels, parent)
	}
	inConv := map[string]bool{}
	for _, id := range sources {
		inConv[id] = true
	}
	caller := personRef(in.SpeakerID)
	now := s.Clock.Now()
	var out []chat.RoomTopic
	for _, ch := range channels {
		row := s.loadState(db, in.GuildID, ch)
		if !row.exists {
			continue
		}
		row.body.Age(now)
		labelOf := s.refLabels(db, roomOf(in.GuildID, ch))
		for _, t := range row.body.Threads {
			linked := len(q.Matches(episodes.StoredText(t.Label+"\n"+t.Gist, labelOf, ""))) > 0
			for _, id := range t.Ev {
				linked = linked || inConv[id]
			}
			for _, w := range t.Who {
				linked = linked || (caller != "" && w == caller)
			}
			if !linked {
				continue
			}
			var speakers []string
			if noHearsay {
				if speakers = lineSpeakers(db, roomOf(in.GuildID, ch), t.Ev); namesOthers(t.Label+"\n"+t.Gist, speakers) {
					continue
				}
			}
			topic := chat.RoomTopic{Label: episodes.StoredText(t.Label, labelOf, "?"), Gist: episodes.StoredText(t.Gist, labelOf, "?")}
			for _, w := range t.Who {
				if noHearsay && !slices.Contains(speakers, w) {
					continue
				}
				if l := labelOf(w); l != "" {
					topic.Who = append(topic.Who, l)
				}
			}
			out = append(out, topic)
		}
	}
	return out
}

// lineSpeakers are the references of the people who said lines ids of a Discord room (personRefOf).
func lineSpeakers(db *storage.Store, room string, ids []string) []string {
	raw, _ := json.Marshal(ids)
	rows, err := db.Reader().Query(`SELECT actor_account, actor_label FROM memory_observations WHERE observation_id IN (SELECT value FROM json_each(?))`, string(raw))
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var account, label string
		if rows.Scan(&account, &label) != nil {
			continue
		}
		if account != "" {
			out = append(out, personRef(account))
		} else {
			out = append(out, "label:"+room+":"+label)
		}
	}
	return out
}

// noteRoomMemory records what room memory a turn was given in its body-free turn record, and why the
// answer was asked again with less context (answer_retry), if it was.
func noteRoomMemory(body map[string]any, res chat.Result) {
	if res.Room != nil {
		body["room_memory"] = res.Room
	}
	if res.AnswerRetry != "" {
		body["answer_retry"] = res.AnswerRetry
	}
	if res.AnswerLoop {
		body["answer_loop"] = true
	}
}

// storedThreadParent is a thread's parent channel from the thread table, for threads the in-memory list
// no longer holds (closed more than a month ago).
func storedThreadParent(db *storage.Store, guild, channel string) string {
	var parent string
	if err := db.Reader().QueryRow(`SELECT parent_id FROM channel_threads WHERE thread_id=? AND guild_id=? AND `+threadVisibleSQL("channel_threads"), channel, guild).Scan(&parent); err != nil {
		return ""
	}
	return parent
}

// roomCaptures reports whether a Discord room's messages are stored now: its own policy, or — for a
// thread without an entry of its own — its parent channel's (a thread is part of its channel).
func (s *Service) roomCaptures(db *storage.Store, guild, channel string) bool {
	if s.ChannelPolicyFor(guild, channel).Capture {
		return true
	}
	if g, ok := s.Doc().Guilds[guild]; ok && g.Channels != nil && hasChannel(g.Channels, channel) {
		return false // the thread's own entry decides
	}
	parent := s.threadParent(channel)
	if parent == "" {
		parent = storedThreadParent(db, guild, channel)
	}
	return parent != "" && parent != channel && s.ChannelPolicyFor(guild, parent).Capture
}
