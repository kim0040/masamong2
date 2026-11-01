package application

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"masamong/internal/action"
	"masamong/internal/chat"
	"masamong/internal/config"
	"masamong/internal/errcode"
	"masamong/internal/features/privacy"
	"masamong/internal/lang"
	"masamong/internal/memory/corpus"
	"masamong/internal/memory/store"
	"masamong/internal/storage"
)

// Room style (docs/plan-memory-conversation.md §5.6 "방에 스며들기", P5-2; experimental, off by default):
// a short note on how a room talks, added after the persona so the bot's replies fit the room. Channel
// setting conversation.room_style: off (default) | on.
//
// How it is made:
//   - the numbers (message length, laugh marks, emoji share, languages, questions) are counted in code
//     from the room's recent messages, no model;
//   - only slang, nicknames, running jokes and topics to avoid are picked by a model, each with the
//     messages it comes from; slang and jokes need two different people (a room's way of talking, not one
//     member's), and lines are shown without names so nobody's voice is copied;
//   - a new version is always pending: an operator sees it, previews a reply with and without it, and
//     approves it. Nothing is applied by itself, and a weekly refresh only proposes;
//   - a version's evidence is indexed like other derived memory: a deleted or edited source message, or a
//     withdrawal, makes the version stale and it stops being used until a new one is approved.
//
// The note only tunes tone: the persona and the room's rules win (the pack header says so), and swearing is
// not copied (pack rules for the extraction).

const (
	styleMinMessages  = 50
	styleMaxMessages  = 600
	styleSampleLines  = 120
	styleMaxItems     = 12
	styleItemRunes    = 60
	styleNoteRunes    = 600
	styleRefreshAfter = 7 * 24 * time.Hour
	styleRefreshNew   = 200
	styleRetryAfter   = 24 * time.Hour // after a failed scheduled build
	styleTraitRunes   = 24             // slang, nickname and joke items: a word or a short phrase
	spendStyle        = "style"
	holderRoomStyle   = "room_style"
)

var styleTicking atomic.Bool

func init() {
	featureDDL = append(featureDDL,
		`CREATE TABLE IF NOT EXISTS channel_style (
		guild_id TEXT NOT NULL,
		channel_id TEXT NOT NULL,
		version INTEGER NOT NULL,
		state TEXT NOT NULL,
		stats TEXT NOT NULL DEFAULT '{}',
		items TEXT NOT NULL DEFAULT '[]',
		note TEXT NOT NULL DEFAULT '',
		messages INTEGER NOT NULL DEFAULT 0,
		cost_usd REAL NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL,
		decided_at TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (guild_id, channel_id, version))`)
}

// Version states.
const (
	stylePending    = "pending"
	styleApproved   = "approved"
	styleRejected   = "rejected"
	styleSuperseded = "superseded"
	styleStale      = "stale" // a source message changed: not used, rebuild
)

// styleStats are the counted numbers of a room's talk.
type styleStats struct {
	Messages    int            `json:"messages"`
	People      int            `json:"people"`
	MedianRunes int            `json:"median_runes"`
	P90Runes    int            `json:"p90_runes"`
	Laughs      []string       `json:"laughs,omitempty"`    // most used laugh marks (ㅋㅋ, ㅎㅎ, www)
	LaughShare  float64        `json:"laugh_share"`         // messages with one
	EmojiShare  float64        `json:"emoji_share"`         // messages with an emoji
	Questions   float64        `json:"question_share"`      // messages ending in a question mark
	Languages   map[string]int `json:"languages,omitempty"` // percent per detected language
}

// styleItem is one model-picked trait with the messages it comes from.
type styleItem struct {
	Kind     string   `json:"kind"` // slang | nickname | joke | avoid
	Text     string   `json:"text"`
	Evidence []string `json:"evidence"` // observation ids
}

var styleKinds = []string{"slang", "nickname", "joke", "avoid"}

type styleLine struct {
	id, native, account, text string
}

// styleCache keeps each room's approved note for the chat path.
type styleCache struct {
	mu     sync.Mutex
	notes  map[string]string    // guild:channel → note ("" = none)
	gen    uint64               // bumped by forgetStyleCache: a read that began before is not stored
	failed map[string]time.Time // guild:channel → the scheduler's last failed build (styleRetryAfter)
}

func (s *Service) styleNote(guildID, channelID string) string {
	key := guildID + ":" + channelID
	s.style.mu.Lock()
	if note, ok := s.style.notes[key]; ok {
		s.style.mu.Unlock()
		return note
	}
	gen := s.style.gen
	s.style.mu.Unlock()
	note := ""
	if db, err := s.adminDB(); err == nil {
		_ = db.Reader().QueryRow(`SELECT note FROM channel_style WHERE guild_id=? AND channel_id=? AND state=? ORDER BY version DESC LIMIT 1`,
			guildID, channelID, styleApproved).Scan(&note)
	}
	s.style.mu.Lock()
	if s.style.gen == gen { // a reject or a stale version since the read must not be cached over
		if s.style.notes == nil {
			s.style.notes = map[string]string{}
		}
		s.style.notes[key] = note
	}
	s.style.mu.Unlock()
	return note
}

func (s *Service) forgetStyleCache() {
	s.style.mu.Lock()
	s.style.notes = nil
	s.style.gen++
	s.style.mu.Unlock()
}

// roomStyleRules is what a chat turn adds after the persona: the approved note of the room, when the room
// has the setting on.
func (s *Service) roomStyleRules(guildID, channelID string, pol config.ChannelPolicy, pack *lang.Pack) string {
	if pol.Conversation.RoomStyle != "on" || guildID == "" {
		return ""
	}
	note := s.styleNote(guildID, channelID)
	if note == "" {
		return ""
	}
	return pack.Features.RoomStyleHeader + "\n" + note
}

// styleLines reads the room's recent human messages (newest styleMaxMessages of the last 30 days).
func (s *Service) styleLines(db *storage.Store, guildID, channelID string) ([]styleLine, error) {
	since := s.Clock.Now().UTC().Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	rows, err := db.Reader().Query(`SELECT observation_id, COALESCE(native_message_id,''), actor_account, content FROM memory_observations
		WHERE room=? AND deleted_at='' AND record_kind=? AND parsed_time>=? ORDER BY parsed_time DESC, observation_id DESC LIMIT ?`,
		roomOf(guildID, channelID), corpus.HumanText, since, styleMaxMessages)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []styleLine
	for rows.Next() {
		var l styleLine
		if err := rows.Scan(&l.id, &l.native, &l.account, &l.text); err != nil {
			return nil, err
		}
		if l.account != "" && (!s.MemoryAllowed(l.account) || !s.ConsentAllows(l.account, privacy.Profile)) {
			continue // a person who withdrew consent ("don't analyze me" included) is not counted either
		}
		l.text = strings.TrimSpace(l.text)
		if l.text != "" {
			out = append(out, l)
		}
	}
	slices.Reverse(out) // oldest first
	return out, rows.Err()
}

// countStyle is the code-only part: no model, no names.
func countStyle(lines []styleLine) styleStats {
	st := styleStats{Messages: len(lines), Languages: map[string]int{}}
	if len(lines) == 0 {
		return st
	}
	people := map[string]bool{}
	var lengths []int
	laughs := map[string]int{}
	laughMsgs, emojiMsgs, questions := 0, 0, 0
	langs := map[string]int{}
	for _, l := range lines {
		if l.account != "" {
			people[l.account] = true
		}
		lengths = append(lengths, utf8.RuneCountInString(l.text))
		found := false
		for _, w := range strings.Fields(l.text) {
			if m := laughMark(w); m != "" {
				laughs[m]++
				found = true
			}
		}
		if found {
			laughMsgs++
		}
		if hasEmoji(l.text) {
			emojiMsgs++
		}
		if strings.HasSuffix(l.text, "?") || strings.HasSuffix(l.text, "？") {
			questions++
		}
		if utf8.RuneCountInString(l.text) >= 4 {
			if tag := chat.DetectLanguage(l.text); tag != "" {
				langs[tag]++
			}
		}
	}
	sort.Ints(lengths)
	st.People = len(people)
	st.MedianRunes = lengths[len(lengths)/2]
	st.P90Runes = lengths[min(len(lengths)-1, len(lengths)*9/10)]
	n := float64(len(lines))
	st.LaughShare, st.EmojiShare, st.Questions = round2(float64(laughMsgs)/n), round2(float64(emojiMsgs)/n), round2(float64(questions)/n)
	type kv struct {
		k string
		v int
	}
	var ls []kv
	for k, v := range laughs {
		ls = append(ls, kv{k, v})
	}
	sort.Slice(ls, func(i, j int) bool { return ls[i].v > ls[j].v || ls[i].v == ls[j].v && ls[i].k < ls[j].k })
	for i := 0; i < len(ls) && i < 3; i++ {
		st.Laughs = append(st.Laughs, ls[i].k)
	}
	total := 0
	for _, v := range langs {
		total += v
	}
	for k, v := range langs {
		if pct := v * 100 / max(total, 1); pct >= 5 {
			st.Languages[k] = pct
		}
	}
	return st
}

// laughMark is a word that is one letter repeated (ㅋㅋㅋ, ㅎㅎ, www), shortened to two of it; "" otherwise.
// It knows no language's words: any script's repeated-letter laugh counts.
func laughMark(w string) string {
	w = strings.TrimFunc(w, func(r rune) bool { return unicode.IsPunct(r) || unicode.IsSymbol(r) })
	var first rune
	n := 0
	for i, r := range w {
		if i == 0 {
			first = r
		} else if r != first {
			return ""
		}
		n++
	}
	if n < 2 || !unicode.IsLetter(first) || unicode.IsUpper(first) {
		return ""
	}
	if first >= 0x3131 && first <= 0x318E && first != 'ㅋ' && first != 'ㅎ' {
		return "" // ㅇㅇ, ㅠㅠ, ㄷㄷ: a yes, a sob, a shiver, not a laugh
	}
	return string([]rune{first, first})
}

func hasEmoji(s string) bool {
	for _, r := range s {
		if r >= 0x1F300 && r <= 0x1FAFF || r >= 0x2600 && r <= 0x27BF {
			return true
		}
	}
	return false
}

func round2(f float64) float64 { return float64(int(f*100+0.5)) / 100 }

// styleAllows: the derived budget (memory.derived_budget_usd) also pays for room style.
func (s *Service) styleAllows() bool {
	budget := s.Doc().Memory.DerivedBudget()
	if budget <= 0 {
		return false
	}
	return s.derivedSpent() < budget // derivedSpent counts style spend too
}

// buildStyle makes a new pending version of a room's style.
func (s *Service) buildStyle(ctx context.Context, guildID, channelID string, dryRun bool) (map[string]any, error) {
	db, err := s.adminDB()
	if err != nil {
		return nil, err
	}
	if !db.IsSQLite() {
		return nil, errors.New("room style needs the SQLite store")
	}
	lines, err := s.styleLines(db, guildID, channelID)
	if err != nil {
		return nil, err
	}
	st := countStyle(lines)
	out := map[string]any{"stats": st, "enough": len(lines) >= styleMinMessages, "min_messages": styleMinMessages}
	if dryRun || len(lines) < styleMinMessages {
		return out, nil
	}
	if !s.styleAllows() {
		return nil, ErrChatBudget
	}
	pol := s.channelPolicy(guildID, channelID)
	pack := s.voiceFor(guildID, pol).Pack
	sample := lines
	var kept []styleLine
	for _, l := range sample {
		if utf8.RuneCountInString(l.text) >= 4 {
			kept = append(kept, l)
		}
	}
	if len(kept) > styleSampleLines {
		kept = kept[len(kept)-styleSampleLines:]
	}
	byTag := map[string]styleLine{}
	var shown []string
	for i, l := range kept {
		tag := "m" + strconv.Itoa(i+1)
		byTag[tag] = l
		shown = append(shown, "["+tag+"] "+clip(strings.ReplaceAll(l.text, "\n", " "), 160))
	}
	text, cost, err := s.featureText(ctx, "summary", spendStyle, pack.Features.RoomStyleSystem,
		lang.Fill(pack.Features.RoomStyle, "lines", strings.Join(shown, "\n")), 500, 0)
	if err != nil {
		return nil, err
	}
	items := parseStyleItems(text, byTag)
	byID := map[string]styleLine{}
	for i := range items { // stored with observation ids, not the prompt's tags
		for j, tag := range items[i].Evidence {
			byID[byTag[tag].id] = byTag[tag]
			items[i].Evidence[j] = byTag[tag].id
		}
	}
	var note string
	now := s.Clock.Now().UTC().Format(time.RFC3339)
	version, used := 0, lines
	err = db.WithWriterContext(ctx, func(tx *sql.Tx) error {
		// the model call took seconds: a person who withdrew since is left out of the numbers and of every
		// item, and an item whose line was deleted or changed since it was read goes (a line edited before
		// it was read is quoted as it is now, so an earlier edit is no reason to refuse it)
		withdrawn, err := withdrawnTx(tx, privacy.Memory, privacy.Profile)
		if err != nil {
			return err
		}
		used = nil
		for _, l := range lines {
			if !withdrawn[l.account] {
				used = append(used, l)
			}
		}
		var keep []styleItem
		var evidence []string
		for _, it := range items {
			ok := true
			for _, id := range it.Evidence {
				l := byID[id]
				if withdrawn[l.account] {
					ok = false
				} else if ok, err = styleLineCurrent(tx, l); err != nil {
					return err
				}
				if !ok {
					break
				}
			}
			if !ok {
				continue
			}
			keep = append(keep, it)
			for _, id := range it.Evidence {
				evidence = append(evidence, id, byID[id].native)
			}
		}
		items, st = keep, countStyle(used)
		note = styleNoteText(styleNotePack(pack, st), st, items)
		statsRaw, _ := json.Marshal(st)
		itemsRaw, _ := json.Marshal(items)
		if err := tx.QueryRow(`SELECT COALESCE(MAX(version),0)+1 FROM channel_style WHERE guild_id=? AND channel_id=?`, guildID, channelID).Scan(&version); err != nil {
			return err
		}
		// an older proposal nobody decided on gives way to the new one
		if _, err := tx.Exec(`UPDATE channel_style SET state=? WHERE guild_id=? AND channel_id=? AND state=?`, styleSuperseded, guildID, channelID, stylePending); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO channel_style(guild_id,channel_id,version,state,stats,items,note,messages,cost_usd,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			guildID, channelID, version, stylePending, string(statsRaw), string(itemsRaw), note, len(used), cost, now); err != nil {
			return err
		}
		return indexCopy(tx, holderRoomStyle, styleHolder(guildID, channelID, version), uniqueStrings(evidence), false)
	})
	if err != nil {
		return nil, err
	}
	out["stats"], out["version"], out["note"], out["items"], out["cost_usd"], out["state"] = st, version, note, items, cost, stylePending
	return out, nil
}

// styleNotePack is the pack the note is written in: the room's own language (the pack of its most used
// one), else the channel's.
func styleNotePack(pack *lang.Pack, st styleStats) *lang.Pack {
	top, share := "", 0
	for k, v := range st.Languages {
		if v > share || v == share && k < top {
			top, share = k, v
		}
	}
	if top == "" {
		return pack
	}
	return lang.For(top, pack.Tag)
}

// styleLineCurrent: the line an item quotes is still stored, not deleted or being deleted, and says what
// it said when it was read.
func styleLineCurrent(tx *sql.Tx, l styleLine) (bool, error) {
	var content, deleted string
	err := tx.QueryRow(`SELECT content, deleted_at FROM memory_observations WHERE observation_id=?`, l.id).Scan(&content, &deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil || deleted != "" || strings.TrimSpace(content) != l.text {
		return false, err
	}
	return store.ObservationCurrent(tx, l.id)
}

func styleHolder(guildID, channelID string, version int) string {
	return guildID + ":" + channelID + ":" + strconv.Itoa(version)
}

// parseStyleHolder reads styleHolder back: the version after the last ':', the guild before the first
// (Discord IDs have none; a channel ID with one stays whole).
func parseStyleHolder(id string) (guild, channel string, version int, ok bool) {
	i, j := strings.Index(id, ":"), strings.LastIndex(id, ":")
	if i < 0 || i == j {
		return "", "", 0, false
	}
	v, err := strconv.Atoi(id[j+1:])
	if err != nil {
		return "", "", 0, false
	}
	return id[:i], id[i+1 : j], v, true
}

// parseStyleItems keeps the items the code can check: a known kind, short one-line text with no markup
// or links, evidence the prompt showed, and two different people for slang and jokes.
func parseStyleItems(text string, byTag map[string]styleLine) []styleItem {
	var reply struct {
		Items []styleItem `json:"items"`
	}
	if !decodeJSONObject(text, &reply) {
		return nil
	}
	var out []styleItem
	seen := map[string]bool{}
	for _, it := range reply.Items {
		it.Text = strings.TrimSpace(it.Text)
		if !slices.Contains(styleKinds, it.Kind) || it.Text == "" || utf8.RuneCountInString(it.Text) > styleItemRunes ||
			strings.ContainsAny(it.Text, "\n{}<>`") || strings.Contains(strings.ToLower(it.Text), "http") || seen[it.Kind+it.Text] {
			continue
		}
		if strings.ContainsAny(it.Text, "@\r\u2028\u2029\u200b\u200c\u200d\u2060\ufeff#*_|") {
			continue // mentions, line breaks, invisible characters, markup
		}
		trait := it.Kind != "avoid" // a word people use, which must be in what they said
		if trait && (utf8.RuneCountInString(it.Text) > styleTraitRunes || len(strings.Fields(it.Text)) > 3) {
			continue
		}
		var ev []string
		people := map[string]bool{}
		want := strings.ToLower(it.Text)
		for _, tag := range it.Evidence {
			l, ok := byTag[tag]
			if !ok || slices.Contains(ev, tag) || trait && !strings.Contains(strings.ToLower(l.text), want) {
				continue
			}
			ev = append(ev, tag)
			people[l.account] = true
		}
		if len(ev) == 0 || (it.Kind == "slang" || it.Kind == "joke") && len(people) < 2 {
			continue
		}
		it.Evidence = ev
		seen[it.Kind+it.Text] = true
		out = append(out, it)
		if len(out) >= styleMaxItems {
			break
		}
	}
	return out
}

// styleNoteText puts the counted numbers and the checked items into the pack's note, within
// styleNoteRunes.
func styleNoteText(pack *lang.Pack, st styleStats, items []styleItem) string {
	f := pack.Features
	langs := make([]string, 0, len(st.Languages))
	for k, v := range st.Languages {
		langs = append(langs, fmt.Sprintf("%s %d%%", k, v))
	}
	sort.Strings(langs)
	group := map[string][]string{}
	for _, it := range items {
		t := it.Text
		if it.Kind != "avoid" {
			t = "\"" + t + "\"" // words quoted from the room, never instructions
		}
		group[it.Kind] = append(group[it.Kind], t)
	}
	var parts []string
	for _, k := range styleKinds {
		if len(group[k]) > 0 {
			parts = append(parts, f.RoomStyleKinds[k]+": "+strings.Join(group[k], ", "))
		}
	}
	laughs := strings.Join(st.Laughs, " ")
	if laughs == "" {
		laughs = "-"
	}
	note := lang.Fill(f.RoomStyleNote, "median", strconv.Itoa(st.MedianRunes), "p90", strconv.Itoa(st.P90Runes), "laughs", laughs,
		"laugh_pct", strconv.Itoa(int(st.LaughShare*100)), "emoji_pct", strconv.Itoa(int(st.EmojiShare*100)), "languages", strings.Join(langs, ", "),
		"items", strings.Join(parts, "\n"))
	if r := []rune(strings.TrimSpace(note)); len(r) > styleNoteRunes {
		note = string(r[:styleNoteRunes])
	}
	return strings.TrimSpace(note)
}

// invalidateRoomStyle makes stale every version whose evidence a deletion, an edit or a withdrawal hit.
func (s *Service) invalidateRoomStyle(ctx context.Context, db *storage.Store, jobs []redactionJob) error {
	if !db.IsSQLite() {
		return nil
	}
	var keys []string
	for _, j := range jobs {
		keys = append(append(append(append(keys, j.Messages...), j.Obs...), j.Leaves...), j.Ref)
	}
	keys = uniqueStrings(keys)
	if len(keys) == 0 {
		return nil
	}
	raw, _ := json.Marshal(keys)
	return s.staleStyles(ctx, db, `SELECT DISTINCT holder_id FROM evidence_refs WHERE holder_kind=? AND source_id IN (SELECT value FROM json_each(?))`,
		holderRoomStyle, string(raw))
}

// staleStyles makes stale the versions whose holder ids q (with args) selects: the note and items go, and
// a pending or approved version stops being used until a new one is approved.
func (s *Service) staleStyles(ctx context.Context, db *storage.Store, q string, args ...any) error {
	changed := false
	err := db.WithWriterContext(ctx, func(tx *sql.Tx) error {
		if !tableExists(tx, "channel_style") || !tableExists(tx, "evidence_refs") {
			return nil
		}
		rows, err := tx.Query(q, args...)
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
		rows.Close()
		for _, id := range ids {
			h := holder{kind: holderRoomStyle, id: id}
			g, c, v, ok := parseStyleHolder(h.id)
			if !ok {
				continue
			}
			// rejected and superseded versions keep their state (the note goes either way)
			if _, err := tx.Exec(`UPDATE channel_style SET state=CASE WHEN state IN (?,?) THEN ? ELSE state END, note='', items='[]'
				WHERE guild_id=? AND channel_id=? AND version=?`, stylePending, styleApproved, styleStale, g, c, v); err != nil {
				return err
			}
			if _, err := tx.Exec(`DELETE FROM evidence_refs WHERE holder_kind=? AND holder_id=?`, holderRoomStyle, h.id); err != nil {
				return err
			}
			changed = true
		}
		return nil
	})
	if changed {
		s.forgetStyleCache()
	}
	return err
}

// refreshRoomStyles proposes a new version for rooms with the setting on whose last version is a week old
// and that said enough since (the scheduler; never approves anything).
func (s *Service) refreshRoomStyles(ctx context.Context) {
	doc := s.Doc()
	db, err := s.adminDB()
	if err != nil || !db.IsSQLite() || s.practice {
		return
	}
	now := s.Clock.Now()
	for g, gs := range doc.Guilds {
		for c := range gs.Channels {
			if ctx.Err() != nil {
				return
			}
			if s.channelPolicy(g, c).Conversation.RoomStyle != "on" || !s.styleAllows() {
				continue
			}
			key := g + ":" + c
			s.style.mu.Lock()
			failedAt := s.style.failed[key]
			s.style.mu.Unlock()
			if !failedAt.IsZero() && now.Sub(failedAt) < styleRetryAfter {
				continue
			}
			// the newest version in any state: a rejected or stale one waits its week like the others, and
			// an undecided proposal is not replaced behind the operator's back
			var created, state string
			var messages int
			err := db.Reader().QueryRow(`SELECT created_at, messages, state FROM channel_style WHERE guild_id=? AND channel_id=? ORDER BY version DESC LIMIT 1`,
				g, c).Scan(&created, &messages, &state)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if state == stylePending {
				continue
			}
			if at, e := time.Parse(time.RFC3339, created); e == nil && now.Sub(at) < styleRefreshAfter {
				continue
			}
			lines, e := s.styleLines(db, g, c)
			if e != nil || len(lines) < styleMinMessages || created != "" && len(lines)-messages < styleRefreshNew && len(lines) < styleMaxMessages {
				continue
			}
			if _, err := s.buildStyle(ctx, g, c, false); err != nil {
				s.Log.Debug("room style refresh failed", "err", err.Error())
				s.style.mu.Lock()
				if s.style.failed == nil {
					s.style.failed = map[string]time.Time{}
				}
				s.style.failed[key] = now
				s.style.mu.Unlock()
			}
		}
	}
}

// --- Actions ---------------------------------------------------------------------------------------

// room.style.show {guild_id, channel_id} → {setting, versions: [{version, state, stats, items, note,
// messages, cost_usd, created_at, decided_at}]} (newest first, at most 5; items carry evidence counts only).
// room.style.build {guild_id, channel_id, dry_run?} → dry_run: {stats, enough}; else a pending version.
// room.style.preview {guild_id, channel_id, version, message} → {plain, styled}: two replies to message,
// without and with the version's note (one answer-model call each).
// room.style.decide {guild_id, channel_id, version, decision: approve|reject}.
func (s *Service) roomStyleAction(ctx context.Context, req action.Request) action.Result {
	if req.Invoker != nil && req.Invoker.UserID != "" {
		return action.Fail(req, errcode.PermissionDenied, "generic.error", "room style is managed in the console", nil)
	}
	in := inputMap(req)
	g, c := strVal(in, "guild_id", ""), strVal(in, "channel_id", "")
	if g == "" || c == "" {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "guild_id and channel_id are required", nil)
	}
	db, err := s.adminDB()
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	switch req.Action {
	case "room.style.show":
		return s.roomStyleShow(req, db, g, c)
	case "room.style.build":
		out, err := s.buildStyle(ctx, g, c, boolVal(in, "dry_run"))
		if errors.Is(err, ErrChatBudget) {
			return action.Fail(req, errcode.BudgetExceeded, "generic.error", "the derived memory budget is used up this month", nil)
		}
		if err != nil {
			return action.Fail(req, errcode.ProviderUnavailable, "generic.error", err.Error(), nil)
		}
		return action.OK(req, out)
	case "room.style.preview":
		return s.roomStylePreview(ctx, req, db, g, c, intVal(in, "version", 0), strVal(in, "message", ""))
	case "room.style.decide":
		version, decision := intVal(in, "version", 0), strVal(in, "decision", "")
		if version <= 0 || decision != "approve" && decision != "reject" {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "version and decision (approve|reject) are required", nil)
		}
		now := s.Clock.Now().UTC().Format(time.RFC3339)
		var found bool
		err := db.WithWriterContext(ctx, func(tx *sql.Tx) error {
			var state string
			if err := tx.QueryRow(`SELECT state FROM channel_style WHERE guild_id=? AND channel_id=? AND version=?`, g, c, version).Scan(&state); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return nil
				}
				return err
			}
			if state != stylePending {
				return fmt.Errorf("version %d is %s, not pending", version, state)
			}
			found = true
			if decision == "reject" {
				_, err := tx.Exec(`UPDATE channel_style SET state=?, decided_at=? WHERE guild_id=? AND channel_id=? AND version=?`, styleRejected, now, g, c, version)
				return err
			}
			if _, err := tx.Exec(`UPDATE channel_style SET state=? WHERE guild_id=? AND channel_id=? AND state=?`, styleSuperseded, g, c, styleApproved); err != nil {
				return err
			}
			_, err := tx.Exec(`UPDATE channel_style SET state=?, decided_at=? WHERE guild_id=? AND channel_id=? AND version=?`, styleApproved, now, g, c, version)
			return err
		})
		if err != nil {
			return action.Fail(req, errcode.StaleRevision, "generic.error", err.Error(), nil)
		}
		if !found {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "no such version", nil)
		}
		s.forgetStyleCache()
		return action.OK(req, map[string]any{"version": version, "decision": decision})
	}
	return action.Fail(req, errcode.InvalidInput, "generic.error", "unknown action", nil)
}

// roomStyleOne is one version as room.style.show lists it (nil = none).
func (s *Service) roomStyleOne(db *storage.Store, g, c string, version int) map[string]any {
	var messages int
	var state, statsRaw, itemsRaw, note, created, decided string
	var cost float64
	if db.Reader().QueryRow(`SELECT state, stats, items, note, messages, cost_usd, created_at, decided_at FROM channel_style
		WHERE guild_id=? AND channel_id=? AND version=?`, g, c, version).Scan(&state, &statsRaw, &itemsRaw, &note, &messages, &cost, &created, &decided) != nil {
		return nil
	}
	return styleVersionView(version, state, statsRaw, itemsRaw, note, messages, cost, created, decided)
}

func styleVersionView(version int, state, statsRaw, itemsRaw, note string, messages int, cost float64, created, decided string) map[string]any {
	var st styleStats
	var items []styleItem
	_ = json.Unmarshal([]byte(statsRaw), &st)
	_ = json.Unmarshal([]byte(itemsRaw), &items)
	shown := []map[string]any{}
	for _, it := range items {
		shown = append(shown, map[string]any{"kind": it.Kind, "text": it.Text, "evidence": len(it.Evidence)})
	}
	return map[string]any{"version": version, "state": state, "stats": st, "items": shown, "note": note,
		"messages": messages, "cost_usd": cost, "created_at": created, "decided_at": decided}
}

func (s *Service) roomStyleShow(req action.Request, db *storage.Store, g, c string) action.Result {
	out := map[string]any{"setting": s.channelPolicy(g, c).Conversation.RoomStyle, "versions": []any{}}
	rows, err := db.Reader().Query(`SELECT version, state, stats, items, note, messages, cost_usd, created_at, decided_at FROM channel_style
		WHERE guild_id=? AND channel_id=? ORDER BY version DESC LIMIT 5`, g, c)
	if err != nil {
		return action.OK(req, out) // no table yet: nothing made
	}
	defer rows.Close()
	var versions []any
	for rows.Next() {
		var version, messages int
		var state, statsRaw, itemsRaw, note, created, decided string
		var cost float64
		if rows.Scan(&version, &state, &statsRaw, &itemsRaw, &note, &messages, &cost, &created, &decided) != nil {
			continue
		}
		versions = append(versions, styleVersionView(version, state, statsRaw, itemsRaw, note, messages, cost, created, decided))
	}
	if versions != nil {
		out["versions"] = versions
	}
	// the version in use, even when newer ones pushed it out of the list
	var approved int
	if db.Reader().QueryRow(`SELECT version FROM channel_style WHERE guild_id=? AND channel_id=? AND state=? ORDER BY version DESC LIMIT 1`,
		g, c, styleApproved).Scan(&approved) == nil {
		out["approved_version"] = approved
		listed := false
		for _, v := range versions {
			listed = listed || v.(map[string]any)["version"] == approved
		}
		if !listed {
			if r := s.roomStyleOne(db, g, c, approved); r != nil {
				out["versions"] = append(versions, r)
			}
		}
	}
	return action.OK(req, out)
}

func (s *Service) roomStylePreview(ctx context.Context, req action.Request, db *storage.Store, g, c string, version int, message string) action.Result {
	message = strings.TrimSpace(message)
	if version <= 0 || message == "" {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "version and message are required", nil)
	}
	var note string
	if err := db.Reader().QueryRow(`SELECT note FROM channel_style WHERE guild_id=? AND channel_id=? AND version=?`, g, c, version).Scan(&note); err != nil || note == "" {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "no such version with a note", nil)
	}
	if !s.styleAllows() {
		return action.Fail(req, errcode.BudgetExceeded, "generic.error", "the derived memory budget is used up this month", nil)
	}
	pol := s.channelPolicy(g, c)
	voice := s.voiceFor(g, pol)
	persona := s.personaText(pol.PersonaRef)
	reply := func(rules string) (string, error) {
		text, _, err := s.featureText(ctx, "response", spendStyle, chat.ComposeSystem(voice, persona, rules, ""), clip(message, 400), 300, 0.7)
		return text, err
	}
	plain, err := reply(pol.Rules)
	if err != nil {
		return action.Fail(req, errcode.ProviderUnavailable, "generic.error", err.Error(), nil)
	}
	styled, err := reply(strings.TrimSpace(pol.Rules + "\n\n" + voice.Pack.Features.RoomStyleHeader + "\n" + note))
	if err != nil {
		return action.Fail(req, errcode.ProviderUnavailable, "generic.error", err.Error(), nil)
	}
	return action.OK(req, map[string]any{"version": version, "plain": plain, "styled": styled})
}
