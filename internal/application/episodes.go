package application

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"masamong/internal/crash"
	"masamong/internal/lang"
	"masamong/internal/memory/corpus"
	"masamong/internal/memory/engine"
	"masamong/internal/memory/episodes"
	"masamong/internal/memory/lexical"
	"masamong/internal/memory/store"
	"masamong/internal/storage"
)

// Room memory (ADR-17, docs/plan-memory-conversation.md §5.4): what a room decided, planned and laughed
// about (L2 episodes) and what it is talking about now (L1 state), made in the background from the raw
// messages of channels that store them.
//
// The write path, per pass of the memory worker (memoryPass → roomMemoryPass):
//
//  1. expiry: open/decided plans past their window become expired (code, no model);
//  2. regeneration: episodes whose evidence was deleted or edited (40% of items, or a decision) are made
//     again from the raw messages that remain;
//  3. per capturing channel: the conversation since the room's cursor is cut into segments
//     (episodes.Split); each closed segment not extracted yet goes to the summary model once, the answer
//     is validated in code (episodes.Prompt.Parse) and saved with its evidence in one transaction that
//     refuses messages that changed meanwhile;
//  4. per capturing channel: the room state is updated when the room went quiet after enough new
//     messages (or every N messages in a busy room), made from the raw messages every N updates;
//  5. Kakao blocks an operator approved (memory.episodes.extract) continue their one-off extraction.
//
// All of it spends from memory.derived_budget_usd (monthly, memory_spend kinds episode and state): the
// room state stops at 80% of it, extraction at 100%. Quiet rooms cost nothing.
//
// Invalidation: every item and state is registered in evidence_refs with its observation IDs, their
// native message IDs and their authors' person references (copySources). processRedactions calls
// invalidateRoomMemory with the same keys every other copy is redacted with: a deleted message hides the
// items citing it, an edit hides them as dirty, a withdrawn consent deletes the person's items, and a
// room state citing any of them is deleted (the next update makes it again from the raw messages).

const (
	spendEpisode = "episode"
	spendState   = "state"
	// stateShare is the part of the derived budget the room state may use: past it only extraction runs.
	stateShare = 0.8
	// work bounds of one pass
	passSegments    = 4  // closed segments extracted per room and pass
	passRegens      = 3  // episodes regenerated per pass
	kakaoSegments   = 20 // Kakao segments per pass
	roomWindow      = 3000
	stateLines      = 60
	extractTokens   = 1400
	stateTokens     = 1400
	extractTemp     = 0.2
	initialLookBack = 24 * time.Hour
)

var stateSchedule = episodes.Schedule{}

// errRoomMemoryStop ends a pass early: the budget ran out or the provider failed.
var errRoomMemoryStop = errors.New("room memory: stop this pass")

// RoomMemoryReport is what one pass did (logs, tests). It carries counts only.
type RoomMemoryReport struct {
	Expired     int    `json:"expired"`
	Regenerated int    `json:"regenerated"`
	Segments    int    `json:"segments"`
	Episodes    int    `json:"episodes"`
	Failed      int    `json:"failed"`
	States      int    `json:"states"`
	StoppedBy   string `json:"stopped_by,omitempty"`
}

// memRoom is one room room memory is made for.
type memRoom struct {
	guild, channel, room string
	source               string // discord | kakao:<block>
}

func (r memRoom) kakao() bool { return strings.HasPrefix(r.source, "kakao:") }

// derivedSpent is this month's spend of room memory.
func (s *Service) derivedSpent() float64 {
	by, _, ok := s.monthSpend()
	if !ok {
		return 0
	}
	return by[spendEpisode] + by[spendState] + by[spendStyle] + by[spendCard]
}

// derivedAllows reports whether the derived budget leaves room for one more call of kind.
func (s *Service) derivedAllows(kind string) bool {
	budget := s.Doc().Memory.DerivedBudget()
	if budget <= 0 {
		return false
	}
	spent := s.derivedSpent()
	if kind == spendState {
		return spent < budget*stateShare
	}
	return spent < budget
}

// roomMemoryPass is the memory worker's hook (memoryPass).
func (s *Service) roomMemoryPass(ctx context.Context) {
	_ = crash.Run(s.Log, "room memory pass", func() {
		rep := s.RunRoomMemory(ctx)
		if rep.Segments+rep.States+rep.Expired+rep.Regenerated > 0 || rep.StoppedBy != "" {
			s.Log.Info("memory: room memory pass", "segments", rep.Segments, "episodes", rep.Episodes, "failed", rep.Failed,
				"states", rep.States, "expired", rep.Expired, "regenerated", rep.Regenerated, "stopped_by", rep.StoppedBy)
		}
	})
}

// RunRoomMemory runs one room memory pass now (the memory worker calls it every index interval).
func (s *Service) RunRoomMemory(ctx context.Context) RoomMemoryReport {
	var rep RoomMemoryReport
	if s.Store == nil || s.practice {
		return rep
	}
	db, err := s.adminDB()
	if err != nil || !db.IsSQLite() {
		return rep
	}
	now := s.Clock.Now()
	rep.Expired = s.expireEpisodes(db, now)
	rooms := s.capturingRooms(db)
	defer s.cardsPass(ctx, db, rooms) // person cards: model-free, after what this pass extracted
	if s.Doc().Memory.DerivedBudget() <= 0 {
		return rep
	}
	stop := func(err error) bool {
		if err == nil {
			return false
		}
		if rep.StoppedBy == "" {
			rep.StoppedBy = "error"
			if errors.Is(err, errRoomMemoryStop) || errors.Is(err, ErrChatBudget) {
				rep.StoppedBy = "budget"
			}
		}
		return true
	}
	n, err := s.regenerateEpisodes(ctx, db, now)
	rep.Regenerated += n
	if stop(err) {
		return rep
	}
	doc := s.Doc()
	for _, r := range rooms {
		if ctx.Err() != nil {
			return rep
		}
		if doc.Memory.Capture {
			if s.derivedAllows(spendEpisode) {
				st, err := s.extractRoom(ctx, db, r, now, passSegments)
				rep.Segments, rep.Episodes, rep.Failed = rep.Segments+st.segments, rep.Episodes+st.saved, rep.Failed+st.failed
				if stop(err) {
					return rep
				}
			} else if rep.StoppedBy == "" {
				rep.StoppedBy = "budget"
			}
			if s.derivedAllows(spendState) {
				done, err := s.updateRoomState(ctx, db, r, now)
				if done {
					rep.States++
				}
				if stop(err) {
					return rep
				}
			} else if rep.StoppedBy == "" {
				rep.StoppedBy = "budget"
			}
		}
	}
	st, err := s.continueKakaoExtractions(ctx, db, now)
	rep.Segments, rep.Episodes, rep.Failed = rep.Segments+st.segments, rep.Episodes+st.saved, rep.Failed+st.failed
	stop(err)
	return rep
}

// capturingRooms are the Discord rooms with stored messages whose channel stores messages now. A thread
// without an entry of its own (a bot thread, say) stores when its parent channel does (roomCaptures).
func (s *Service) capturingRooms(db *storage.Store) []memRoom {
	rows, err := db.Reader().Query(`SELECT room FROM memory_rooms WHERE platform='discord' ORDER BY room`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []memRoom
	for rows.Next() {
		var room string
		if rows.Scan(&room) != nil {
			continue
		}
		parts := strings.SplitN(room, ":", 3) // engine.RoomKey
		if len(parts) != 3 || parts[1] == "0" {
			continue // a DM
		}
		if !s.roomCaptures(db, parts[1], parts[2]) {
			continue
		}
		out = append(out, memRoom{guild: parts[1], channel: parts[2], room: room, source: "discord"})
	}
	return out
}

// ---------------------------------------------------------------------------
// reading a room
// ---------------------------------------------------------------------------

// roomLine is one stored message with what the segmenter and the save guard need.
type roomLine struct {
	episodes.Message
	native, parsed string
}

func (s *Service) naiveZone() *time.Location {
	if tz := s.Doc().Memory.NaiveTimezone; tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			return loc
		}
	}
	return s.instanceZone()
}

// obsTime reads a stored time; naive (Kakao) times are in memory.naive_timezone.
func (s *Service) obsTime(parsed string, naive *time.Location) (time.Time, bool) {
	t, aware, ok := corpus.ParseTime(parsed)
	if !ok {
		return time.Time{}, false
	}
	if !aware {
		t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), naive)
	}
	return t, true
}

// roomLines loads a room's messages from since (a stored parsed_time, inclusive), oldest first.
func (s *Service) roomLines(db *storage.Store, room, since string, limit int) ([]roomLine, error) {
	rows, err := db.Reader().Query(`SELECT observation_id, COALESCE(native_message_id,''), actor_label, actor_account, content, parsed_time, record_kind
		FROM memory_observations WHERE room=? AND deleted_at='' AND record_kind IN ('`+corpus.HumanText+`','`+corpus.BotRecord+`') AND parsed_time>=?
		ORDER BY parsed_time, observation_id LIMIT ?`, room, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	naive := s.naiveZone()
	var out []roomLine
	for rows.Next() {
		var l roomLine
		var kind string
		if err := rows.Scan(&l.ID, &l.native, &l.Author, &l.Account, &l.Text, &l.parsed, &kind); err != nil {
			return nil, err
		}
		at, ok := s.obsTime(l.parsed, naive)
		if !ok {
			continue // a record without a time gets none made up
		}
		l.At, l.Bot, l.Text = at, kind == corpus.BotRecord, strings.TrimSpace(l.Text)
		out = append(out, l)
	}
	return out, rows.Err()
}

// roomLinesSince loads the newest limit messages of a room written after after, oldest first (room state).
func (s *Service) roomLinesSince(db *storage.Store, room string, after time.Time, limit int) ([]roomLine, error) {
	rows, err := db.Reader().Query(`SELECT observation_id FROM memory_observations WHERE room=? AND deleted_at='' AND record_kind IN ('`+corpus.HumanText+`','`+corpus.BotRecord+`')
		ORDER BY parsed_time DESC, observation_id DESC LIMIT ?`, room, limit)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	lines, err := s.linesByID(db, ids)
	if err != nil {
		return nil, err
	}
	out := lines[:0]
	for _, l := range lines {
		if l.At.After(after) {
			out = append(out, l)
		}
	}
	return out, nil
}

// linesByID loads stored messages by observation ID (current ones only), oldest first.
func (s *Service) linesByID(db *storage.Store, ids []string) ([]roomLine, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	raw, _ := json.Marshal(ids)
	rows, err := db.Reader().Query(`SELECT observation_id, COALESCE(native_message_id,''), actor_label, actor_account, content, parsed_time, record_kind
		FROM memory_observations WHERE observation_id IN (SELECT value FROM json_each(?)) AND deleted_at=''`, string(raw))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	naive := s.naiveZone()
	var out []roomLine
	for rows.Next() {
		var l roomLine
		var kind string
		if err := rows.Scan(&l.ID, &l.native, &l.Author, &l.Account, &l.Text, &l.parsed, &kind); err != nil {
			return nil, err
		}
		at, ok := s.obsTime(l.parsed, naive)
		if !ok {
			continue
		}
		l.At, l.Bot, l.Text = at, kind == corpus.BotRecord, strings.TrimSpace(l.Text)
		out = append(out, l)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].At.Equal(out[j].At) {
			return out[i].ID < out[j].ID
		}
		return out[i].At.Before(out[j].At)
	})
	return out, rows.Err()
}

// liveContext adds what only the gateway saw: reply links between stored messages (kept in the cursor so
// a later pass cuts the same way after the live window moved on) and the bot's own replies to them
// (context for the prompt, never evidence).
func (s *Service) liveContext(r memRoom, lines []roomLine, replies map[string]string) []episodes.Message {
	if r.kakao() {
		return nil
	}
	byNative := map[string]string{}
	for _, l := range lines {
		if l.native != "" {
			byNative[l.native] = l.ID
		}
	}
	bot := s.Doc().BotNames()[0]
	var bots []episodes.Message
	for _, l := range s.liveLines(r.guild, r.channel) {
		to, ok := byNative[l.ReplyTo]
		if !ok {
			continue
		}
		if l.Bot {
			if strings.TrimSpace(l.Text) != "" {
				bots = append(bots, episodes.Message{ID: "bot:" + l.ID, Author: bot, At: l.At, ReplyTo: to, Bot: true, Text: l.Text})
			}
			continue
		}
		if from, ok := byNative[l.ID]; ok && replies != nil {
			replies[from] = to
		}
	}
	return bots
}

func withReplies(lines []roomLine, replies map[string]string) []episodes.Message {
	out := make([]episodes.Message, 0, len(lines))
	for _, l := range lines {
		m := l.Message
		if to, ok := replies[m.ID]; ok {
			m.ReplyTo = to
		}
		out = append(out, m)
	}
	return out
}

// personRefOf is the stored reference of a speaker: the one-way account reference for Discord, the room
// and label for Kakao (kakao:<room>:<label>; never merged with a Discord person).
// A Discord message without an account (none are stored that way today) gets a label reference of the
// room, which no withdrawal can name but which still never merges two people.
func personRefOf(r memRoom, m episodes.Message) string {
	switch {
	case r.kakao():
		return kakaoPrefix(r.room) + m.Author
	case m.Account != "":
		return store.PersonRef(m.Account)
	}
	return "label:" + r.room + ":" + m.Author
}

func kakaoPrefix(room string) string { return "kakao:" + strings.TrimPrefix(room, "kakao:") + ":" }

// refLabels names the people of a room by their stored reference (from their current messages).
func (s *Service) refLabels(db *storage.Store, room string) func(string) string {
	labels := map[string]string{}
	loaded := false
	return func(ref string) string {
		if p := kakaoPrefix(room); strings.HasPrefix(ref, p) {
			return strings.TrimPrefix(ref, p)
		}
		if p := "label:" + room + ":"; strings.HasPrefix(ref, p) {
			return strings.TrimPrefix(ref, p)
		}
		if !loaded {
			loaded = true
			rows, err := db.Reader().Query(`SELECT actor_account, actor_label FROM memory_observations WHERE room=? AND deleted_at='' AND actor_account<>''
				GROUP BY actor_account ORDER BY MAX(parsed_time) DESC LIMIT 1000`, room)
			if err == nil {
				for rows.Next() {
					var account, label string
					if rows.Scan(&account, &label) == nil && s.MemoryAllowed(account) {
						labels[store.PersonRef(account)] = label
					}
				}
				rows.Close()
			}
		}
		return labels[ref]
	}
}

func (s *Service) roomVoice(r memRoom) (*lang.Pack, string) {
	v := s.voiceFor(r.guild, s.ChannelPolicyFor(r.guild, r.channel))
	return v.Pack, v.BotName
}

func tokenizerFor(p *lang.Pack) *lexical.Tokenizer {
	t := lexical.FromLanguage(p.Search)
	return &t
}

// ---------------------------------------------------------------------------
// extraction
// ---------------------------------------------------------------------------

func cursorKey(room string) string { return "episodes:cursor:" + room }

// episodeCursor is where a room's extraction stands: the first message of the earliest conversation
// block not finished yet, the segments of the open blocks already extracted, and reply links.
type episodeCursor struct {
	BlockStart string            `json:"block_start,omitempty"`
	Sealed     []episodes.Sealed `json:"sealed,omitempty"`
	Replies    map[string]string `json:"replies,omitempty"`
	// Refused counts, per segment (its first message), the passes in a row whose save was refused because
	// a line changed meanwhile (errEvidenceDeleted). A lasting cause — a line of someone who withdrew is
	// still in the window — would otherwise cost a model call every pass; at sealAfterRefusals the
	// segment is sealed without an episode.
	Refused map[string]int `json:"refused,omitempty"`
}

// sealAfterRefusals is how many refused saves in a row seal a segment.
const sealAfterRefusals = 2

type extractStats struct{ segments, saved, failed int }

// extractRoom extracts up to limit closed segments of a room and moves its cursor on.
func (s *Service) extractRoom(ctx context.Context, db *storage.Store, r memRoom, now time.Time, limit int) (extractStats, error) {
	var st extractStats
	var cur episodeCursor
	_, _ = s.kvGet(cursorKey(r.room), &cur)
	if cur.Replies == nil {
		cur.Replies = map[string]string{}
	}
	since := cur.BlockStart
	if since == "" {
		if r.kakao() {
			since = "0"
		} else {
			since = now.Add(-initialLookBack).UTC().Format("2006-01-02T15:04:05+00:00")
		}
	}
	lines, err := s.roomLines(db, r.room, since, roomWindow)
	if err != nil || len(lines) == 0 {
		return st, err
	}
	bots := s.liveContext(r, lines, cur.Replies)
	pack, botName := s.roomVoice(r)
	splitNow := now
	if r.kakao() {
		splitNow = lines[len(lines)-1].At.Add(24 * time.Hour) // an import is over: every segment is closed
	}
	loc := s.instanceZone()
	if r.kakao() {
		loc = s.naiveZone()
	}
	segs := episodes.Split(withReplies(lines, cur.Replies), splitNow, episodes.Options{Location: loc, DayBoundary: s.Doc().Memory.Boundary(),
		Tokenizer: tokenizerFor(pack), Sealed: cur.Sealed})
	truncated := len(lines) == roomWindow
	lastBlock := 0
	for _, sg := range segs {
		lastBlock = max(lastBlock, sg.Block)
	}
	byID := map[string]roomLine{}
	for _, l := range lines {
		byID[l.ID] = l
	}
	var stopErr error
	for i := range segs {
		sg := &segs[i]
		if !sg.Closed || sg.Sealed || (truncated && sg.Block == lastBlock && lastBlock > 0) {
			continue
		}
		if st.segments >= limit || stopErr != nil {
			break
		}
		if !s.derivedAllows(spendEpisode) {
			stopErr = errRoomMemoryStop
			break
		}
		segLines := make([]roomLine, 0, len(sg.IDs))
		for _, id := range sg.IDs {
			segLines = append(segLines, byID[id])
		}
		saved, err := s.extractSegment(ctx, db, r, segLines, bots, pack, botName)
		switch {
		case errors.Is(err, errEvidenceDeleted):
			if cur.Refused == nil {
				cur.Refused = map[string]int{}
			}
			cur.Refused[sg.IDs[0]]++
			if cur.Refused[sg.IDs[0]] < sealAfterRefusals {
				continue // a message changed meanwhile: cut again next pass
			}
			st.failed++ // refused again: sealed without an episode below
		case errors.Is(err, episodes.ErrParse) || errors.Is(err, errExtractInvalid):
			st.failed++
			s.Log.Warn("memory: a conversation was left out of room memory", "lines", len(sg.IDs), "reason", err.Error())
		case err != nil:
			stopErr = err
			continue
		}
		delete(cur.Refused, sg.IDs[0])
		st.segments++
		st.saved += saved
		sg.Sealed = true
		// nothing written after its last message joins it again (Split's Sealed)
		cur.Sealed = append(cur.Sealed, episodes.Sealed{IDs: sg.IDs, At: sg.Last.Add(time.Nanosecond)})
	}
	before := cur.BlockStart
	s.advanceCursor(&cur, segs, byID, lastBlock)
	if truncated && cur.BlockStart == before && lastBlock == 0 {
		// one block longer than the window and every segment in it done: read on from its last message
		cur.BlockStart = lines[len(lines)-1].parsed
	}
	if err := s.kvSet(cursorKey(r.room), cur); err != nil {
		s.Log.Warn("memory: room memory cursor not saved", "err", err)
	}
	return st, stopErr
}

// advanceCursor moves the cursor to the earliest block that still has a segment to extract and forgets
// what lies before it.
func (s *Service) advanceCursor(cur *episodeCursor, segs []episodes.Segment, byID map[string]roomLine, lastBlock int) {
	if len(segs) == 0 {
		return
	}
	first := map[int]string{}
	done := map[int]bool{}
	for _, sg := range segs {
		if _, ok := first[sg.Block]; !ok {
			done[sg.Block] = true
		}
		if f, ok := first[sg.Block]; !ok || byID[sg.IDs[0]].parsed < f {
			first[sg.Block] = byID[sg.IDs[0]].parsed
		}
		if !sg.Sealed {
			done[sg.Block] = false
		}
	}
	keep := lastBlock // the last block may still grow
	for b := 0; b <= lastBlock; b++ {
		if !done[b] {
			keep = b
			break
		}
	}
	cur.BlockStart = first[keep]
	inKept := map[string]bool{}
	for _, sg := range segs {
		if sg.Block >= keep {
			for _, id := range sg.IDs {
				inKept[id] = true
			}
		}
	}
	sealed := cur.Sealed[:0]
	for _, x := range cur.Sealed {
		for _, id := range x.IDs {
			if inKept[id] {
				sealed = append(sealed, x)
				break
			}
		}
	}
	cur.Sealed = sealed
	replies := map[string]string{}
	for from, to := range cur.Replies {
		if inKept[from] {
			replies[from] = to
		}
	}
	cur.Replies = replies
	for id := range cur.Refused {
		if !inKept[id] {
			delete(cur.Refused, id)
		}
	}
}

var errExtractInvalid = errors.New("room memory: the model's answer did not validate twice")

// openPlans are the room's open or decided plans offered as link candidates, most recent first.
func (s *Service) openPlans(db *storage.Store, room string) []episodes.OpenPlan {
	rows, err := db.Reader().Query(`SELECT id, title, status, due_at, when_text, revision FROM channel_episode WHERE room=? AND kind='plan' AND status IN ('open','decided')
		ORDER BY last_seen DESC LIMIT ?`, room, episodes.MaxPlans)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []episodes.OpenPlan
	for rows.Next() {
		var p episodes.OpenPlan
		if rows.Scan(&p.ID, &p.Title, &p.Status, &p.Due, &p.WhenText, &p.Revision) == nil {
			out = append(out, p)
		}
	}
	return out
}

// segmentMessages is a segment's lines plus the bot replies to them, in time order.
func segmentMessages(lines []roomLine, bots []episodes.Message) []episodes.Message {
	in := map[string]bool{}
	var out []episodes.Message
	for _, l := range lines {
		in[l.ID] = true
		out = append(out, l.Message)
	}
	for _, b := range bots {
		if in[b.ReplyTo] {
			out = append(out, b)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

// askValidated calls the summary model and validates the answer, once more when it does not validate.
// validate is told when the answer is the last one asked for: a caller may then keep the entries that
// validated rather than nothing.
func (s *Service) askValidated(ctx context.Context, kind, system, prompt string, maxTokens int, validate func(text string, final bool) error) error {
	var last error
	for attempt := 0; attempt < 2; attempt++ {
		if !s.derivedAllows(kind) {
			return errRoomMemoryStop
		}
		text, _, err := s.featureText(ctx, "summary", kind, system, prompt, maxTokens, extractTemp)
		if err != nil {
			return err
		}
		if last = validate(text, attempt == 1); last == nil {
			return nil
		}
		if errors.Is(last, episodes.ErrParse) {
			last = fmt.Errorf("%w (%s)", last, answerShape(text))
		}
	}
	if errors.Is(last, episodes.ErrParse) || errors.Is(last, errExtractInvalid) {
		return last
	}
	return fmt.Errorf("%w: %v", errExtractInvalid, last)
}

// answerShape says how an answer that is not the expected JSON looks, without its text: empty, no
// object at all, an object cut off (the token cap) or an object that does not decode.
func answerShape(text string) string {
	t := strings.TrimSpace(text)
	switch {
	case t == "":
		return "empty answer"
	case !strings.Contains(t, "{"):
		return fmt.Sprintf("no JSON object, %d chars", len(t))
	case strings.Count(t, "{") > strings.Count(t, "}"):
		return fmt.Sprintf("JSON cut off, %d chars", len(t))
	}
	return fmt.Sprintf("JSON does not decode, %d chars", len(t))
}

// invalidAnswer is errExtractInvalid with the validation problems (codes like "episode 2: item 1: text
// too long"; never the text itself), for the log.
func invalidAnswer(problems []string) error {
	return fmt.Errorf("%w: %s", errExtractInvalid, clip(strings.Join(problems, "; "), 300))
}

// extractSegment asks the model about one closed segment and saves what validates. It returns how many
// episodes were created or changed.
func (s *Service) extractSegment(ctx context.Context, db *storage.Store, r memRoom, lines []roomLine, bots []episodes.Message, pack *lang.Pack, botName string) (int, error) {
	human := map[string]bool{}
	for _, l := range lines {
		if !l.Bot {
			human[l.ID] = true
		}
	}
	if len(human) < 2 {
		return 0, nil // one message is not a conversation: nothing to remember, nothing spent
	}
	msgs := segmentMessages(lines, bots)
	p := episodes.Build(episodes.Input{Lines: msgs, BotName: botName, Now: lines[len(lines)-1].At, Location: s.instanceZone(),
		Plans: s.openPlans(db, r.room), RefOf: func(m episodes.Message) string { return personRefOf(r, m) }, LabelOf: s.refLabels(db, r.room)})
	system := lang.Fill(pack.Memory.EpisodeExtractSystem, "bot", botName)
	prompt := lang.Fill(pack.Memory.EpisodeExtract, "now", p.Now, "people", p.People(), "plans", p.Plans, "lines", p.Lines)
	var res episodes.Result
	err := s.askValidated(ctx, spendEpisode, system, prompt, extractTokens, func(text string, final bool) error {
		var err error
		res, err = p.Parse(text)
		if err != nil {
			return err
		}
		if !res.Clean() {
			// Every episode and item that is kept passed its own checks, so on the last answer the
			// valid ones are saved: one item citing the bot's line used to drop the whole conversation
			// (9 conversations in a week on the live bot, 2026-10-29).
			if final && len(res.Ops) > 0 {
				s.Log.Info("memory: kept the valid part of an answer", "kind", "episodes", "dropped", clip(strings.Join(res.Problems, "; "), 300))
				return nil
			}
			return invalidAnswer(res.Problems)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if len(res.Ops) == 0 {
		return 0, nil
	}
	return s.saveEpisodeOps(ctx, db, r, lines, res.Ops, "")
}

// saveEpisodeOps commits validated operations with their evidence. It refuses the whole answer when a
// line the model read was edited, deleted or withdrawn meanwhile (errEvidenceDeleted). supersedes names
// the episode a regeneration replaces (deleted in the same transaction).
func (s *Service) saveEpisodeOps(ctx context.Context, db *storage.Store, r memRoom, lines []roomLine, ops []episodes.Op, supersedes string) (int, error) {
	byID := map[string]roomLine{}
	var segment []string
	for _, l := range lines {
		byID[l.ID] = l
		if !l.Bot {
			segment = append(segment, l.ID)
		}
	}
	loc := s.instanceZone()
	nowS := s.Clock.Now().UTC().Format(time.RFC3339)
	changed := 0
	err := db.WithWriterContext(ctx, func(tx *sql.Tx) error {
		for _, l := range lines {
			if l.Bot {
				continue
			}
			var content, deleted string
			if err := tx.QueryRowContext(ctx, `SELECT content, deleted_at FROM memory_observations WHERE observation_id=?`, l.ID).Scan(&content, &deleted); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return errEvidenceDeleted
				}
				return err
			}
			current, err := store.ObservationCurrent(tx, l.ID)
			if err != nil {
				return err
			}
			if deleted != "" || strings.TrimSpace(content) != l.Text || !current {
				return errEvidenceDeleted
			}
		}
		if supersedes != "" {
			if err := deleteEpisodeTx(tx, supersedes); err != nil {
				return err
			}
		}
		for _, op := range ops {
			items, err := omitExplicitFactDuplicates(tx, r.guild, op.Items)
			if err != nil {
				return err
			}
			op.Items = items
			if op.Op == "create" && len(op.Items) == 0 {
				continue
			}
			span := evidenceSpan(op.Evidence, byID)
			due, _ := episodes.DueTime(op.Due, loc)
			switch op.Op {
			case "create":
				id := newID("ep_")
				hasDecision := false
				for _, it := range op.Items {
					hasDecision = hasDecision || it.Kind == "decision"
				}
				status := episodes.Next("", episodes.Change{Kind: op.Kind, Op: op.Op, Want: op.Status, HasDecision: hasDecision, Due: due, EvidenceAt: span.last})
				saved, err := s.insertItems(tx, id, op.Items, nowS)
				if err != nil {
					return err
				}
				if saved == 0 {
					continue
				}
				participants, _ := json.Marshal(nonEmpty(op.Participants))
				seg, _ := json.Marshal(segment)
				if _, err := tx.Exec(`INSERT INTO channel_episode(id,guild_id,channel_id,room,kind,title,status,due_at,when_text,importance,source,participants,segment,
					first_seen,last_seen,expires_at,supersedes_id,updated_at,revision) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,1)`,
					id, r.guild, r.channel, r.room, op.Kind, op.Title, status, op.Due, op.WhenText, op.Importance, r.source, string(participants), string(seg),
					span.firstS(), span.lastS(), expiry(op.Kind, status, due, span.last), supersedes, nowS); err != nil {
					return err
				}
				if err := linkLeaves(tx, id, op.Evidence); err != nil {
					return err
				}
				changed++
			default:
				ok, err := s.applyToEpisode(tx, r, op, segment, span, due, nowS)
				if err != nil {
					return err
				}
				if ok {
					changed++
				}
			}
		}
		return nil
	})
	return changed, err
}

// omitExplicitFactDuplicates drops only model-extracted facts that satisfy the shared explicit-fact
// predicate. The check runs in the same write transaction as save.
func omitExplicitFactDuplicates(tx *sql.Tx, guildID string, items []episodes.Item) ([]episodes.Item, error) {
	kept := make([]episodes.Item, 0, len(items))
	for _, it := range items {
		if it.Kind != "fact" || len(it.Evidence) != 1 {
			kept = append(kept, it)
			continue
		}
		people := make([]string, 0, len(it.By))
		for _, ref := range uniqueStrings(it.By) {
			if explicitContains(episodes.TextRefs(it.Text), ref) {
				people = append(people, ref)
			}
		}
		if len(people) == 0 {
			kept = append(kept, it)
			continue
		}
		peopleJSON, _ := json.Marshal(people)
		rows, err := tx.Query(`SELECT saved.text,saved.evidence,saved.by_refs,x.person_ref FROM episode_item saved
			JOIN memory_explicit x ON x.episode_id=saved.episode_id
			WHERE x.guild_id=? AND x.person_ref IN (SELECT value FROM json_each(?))
			AND saved.kind='fact' AND saved.state='active'`, guildID, string(peopleJSON))
		if err != nil {
			return nil, err
		}
		duplicate := false
		for rows.Next() {
			var text, evidenceJSON, byJSON, ref string
			if err := rows.Scan(&text, &evidenceJSON, &byJSON, &ref); err != nil {
				rows.Close()
				return nil, err
			}
			explicit := episodes.Item{Kind: "fact", Text: text}
			_ = json.Unmarshal([]byte(evidenceJSON), &explicit.Evidence)
			_ = json.Unmarshal([]byte(byJSON), &explicit.By)
			if sameExplicitFact(it, explicit, ref) {
				duplicate = true
				break
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
		if !duplicate {
			kept = append(kept, it)
		}
	}
	return kept, nil
}

type span struct{ first, last time.Time }

func (sp span) firstS() string { return sp.first.UTC().Format(time.RFC3339) }
func (sp span) lastS() string  { return sp.last.UTC().Format(time.RFC3339) }

func evidenceSpan(ids []string, byID map[string]roomLine) span {
	var sp span
	for _, id := range ids {
		at := byID[id].At
		if at.IsZero() {
			continue
		}
		if sp.first.IsZero() || at.Before(sp.first) {
			sp.first = at
		}
		if at.After(sp.last) {
			sp.last = at
		}
	}
	return sp
}

func expiry(kind, status string, due, lastSeen time.Time) string {
	at := episodes.ExpiresAt(kind, status, due, lastSeen)
	if at.IsZero() {
		return ""
	}
	return at.UTC().Format(time.RFC3339)
}

func nonEmpty(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// insertItems saves items with their evidence index. An item naming a person who withdrew is skipped.
func (s *Service) insertItems(tx *sql.Tx, episodeID string, items []episodes.Item, at string) (int, error) {
	n := 0
	for _, it := range items {
		refs := append(episodes.TextRefs(it.Text), it.By...)
		blocked := false
		for _, ref := range refs {
			b, err := store.SourceBlocked(tx, ref)
			if err != nil {
				return n, err
			}
			blocked = blocked || b
		}
		if blocked {
			continue
		}
		id := newID("ei_")
		ev, _ := json.Marshal(it.Evidence)
		by, _ := json.Marshal(nonEmpty(it.By))
		if _, err := tx.Exec(`INSERT INTO episode_item(id,episode_id,kind,text,evidence,by_refs,state,created_at) VALUES(?,?,?,?,?,?,'active',?)`,
			id, episodeID, it.Kind, it.Text, string(ev), string(by), at); err != nil {
			return n, err
		}
		keys, err := copySources(tx, it.Evidence, refs)
		if err != nil {
			return n, err
		}
		if err := indexCopy(tx, store.HolderEpisodeItem, id, keys, false); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func linkLeaves(tx *sql.Tx, episodeID string, evidence []string) error {
	leafs, err := store.LeavesOf(tx, evidence)
	if err != nil {
		return err
	}
	for _, l := range leafs {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO episode_leaf(episode_id,leaf_id) VALUES(?,?)`, episodeID, l); err != nil {
			return err
		}
	}
	return nil
}

// applyToEpisode applies an update or close to the plan the model linked, if it did not change since.
func (s *Service) applyToEpisode(tx *sql.Tx, r memRoom, op episodes.Op, segment []string, sp span, due time.Time, at string) (bool, error) {
	var kind, status, dueS, whenText, participantsRaw, segRaw, lastSeen string
	var revision int64
	err := tx.QueryRow(`SELECT kind,status,due_at,when_text,participants,segment,last_seen,revision FROM channel_episode WHERE id=? AND room=?`, op.LinkTo, r.room).
		Scan(&kind, &status, &dueS, &whenText, &participantsRaw, &segRaw, &lastSeen, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if revision != op.LinkRevision || episodes.Terminal(status) {
		return false, nil // changed or finished since the model saw it
	}
	saved, err := s.insertItems(tx, op.LinkTo, op.Items, at)
	if err != nil {
		return false, err
	}
	if op.Due != "" {
		dueS = op.Due
	}
	if op.WhenText != "" {
		whenText = op.WhenText
	}
	loc := s.instanceZone()
	dueT, _ := episodes.DueTime(dueS, loc)
	var activeDecisions int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM episode_item WHERE episode_id=? AND kind='decision' AND state='active'`, op.LinkTo).Scan(&activeDecisions); err != nil {
		return false, err
	}
	next := episodes.Next(status, episodes.Change{Kind: kind, Op: op.Op, Want: op.Status, HasDecision: activeDecisions > 0, Due: dueT, EvidenceAt: sp.last})
	if saved == 0 && next == status && op.Due == "" && op.Title == "" {
		return false, nil
	}
	var participants, seg []string
	_ = json.Unmarshal([]byte(participantsRaw), &participants)
	_ = json.Unmarshal([]byte(segRaw), &seg)
	participants = uniqueStrings(append(participants, op.Participants...))
	seg = uniqueStrings(append(seg, segment...))
	last, _ := time.Parse(time.RFC3339, lastSeen)
	if sp.last.After(last) {
		last = sp.last
	}
	pj, _ := json.Marshal(nonEmpty(participants))
	sj, _ := json.Marshal(nonEmpty(seg))
	title := op.Title
	if _, err := tx.Exec(`UPDATE channel_episode SET title=CASE WHEN ?<>'' THEN ? ELSE title END, status=?, due_at=?, when_text=?, participants=?, segment=?,
		last_seen=?, expires_at=?, updated_at=?, revision=revision+1 WHERE id=?`,
		title, title, next, dueS, whenText, string(pj), string(sj), last.UTC().Format(time.RFC3339), expiry(kind, next, dueT, last), at, op.LinkTo); err != nil {
		return false, err
	}
	return true, linkLeaves(tx, op.LinkTo, op.Evidence)
}

// deleteEpisodeTx removes an episode with its items, leaf links and evidence index.
func deleteEpisodeTx(tx *sql.Tx, id string) error {
	for _, q := range []string{
		`DELETE FROM evidence_refs WHERE holder_kind='` + store.HolderEpisodeItem + `' AND holder_id IN (SELECT id FROM episode_item WHERE episode_id=?)`,
		`DELETE FROM episode_item WHERE episode_id=?`,
		`DELETE FROM episode_leaf WHERE episode_id=?`,
		`DELETE FROM channel_episode WHERE id=?`,
	} {
		if _, err := tx.Exec(q, id); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// expiry and regeneration
// ---------------------------------------------------------------------------

// expireEpisodes moves plans past their window to expired (the model never decides expiry).
func (s *Service) expireEpisodes(db *storage.Store, now time.Time) int {
	n := 0
	_ = db.WithWriter(func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE channel_episode SET status='expired', expires_at='', updated_at=?, revision=revision+1
			WHERE expires_at<>'' AND expires_at<=? AND status IN ('open','decided')`, now.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339))
		if err != nil {
			return err
		}
		c, _ := res.RowsAffected()
		n = int(c)
		return nil
	})
	return n
}

// regenerateEpisodes makes episodes that lost evidence again from the raw messages that remain.
func (s *Service) regenerateEpisodes(ctx context.Context, db *storage.Store, now time.Time) (int, error) {
	rows, err := db.Reader().Query(`SELECT id, guild_id, channel_id, room, source, segment FROM channel_episode WHERE regen=1 ORDER BY updated_at LIMIT ?`, passRegens)
	if err != nil {
		return 0, err
	}
	type job struct {
		r       memRoom
		id, seg string
	}
	var jobs []job
	for rows.Next() {
		var j job
		if rows.Scan(&j.id, &j.r.guild, &j.r.channel, &j.r.room, &j.r.source, &j.seg) == nil {
			jobs = append(jobs, j)
		}
	}
	rows.Close()
	n := 0
	for _, j := range jobs {
		if !s.derivedAllows(spendEpisode) {
			return n, errRoomMemoryStop
		}
		var ids []string
		_ = json.Unmarshal([]byte(j.seg), &ids)
		lines, err := s.linesByID(db, ids)
		if err != nil {
			return n, err
		}
		human := 0
		for _, l := range lines {
			if !l.Bot {
				human++
			}
		}
		if human < 2 {
			if err := db.WithWriter(func(tx *sql.Tx) error { return deleteEpisodeTx(tx, j.id) }); err != nil {
				return n, err
			}
			n++
			continue
		}
		pack, botName := s.roomVoice(j.r)
		p := episodes.Build(episodes.Input{Lines: withReplies(lines, nil), BotName: botName, Now: lines[len(lines)-1].At, Location: s.instanceZone(),
			RefOf: func(m episodes.Message) string { return personRefOf(j.r, m) }, LabelOf: s.refLabels(db, j.r.room)})
		system := lang.Fill(pack.Memory.EpisodeExtractSystem, "bot", botName)
		prompt := lang.Fill(pack.Memory.EpisodeExtract, "now", p.Now, "people", p.People(), "plans", p.Plans, "lines", p.Lines)
		var res episodes.Result
		// a regeneration replaces the episode, so it takes a clean answer only
		err = s.askValidated(ctx, spendEpisode, system, prompt, extractTokens, func(text string, _ bool) error {
			var err error
			if res, err = p.Parse(text); err != nil {
				return err
			}
			if !res.Clean() {
				return invalidAnswer(res.Problems)
			}
			return nil
		})
		switch {
		case errors.Is(err, episodes.ErrParse) || errors.Is(err, errExtractInvalid):
			s.Log.Warn("memory: an episode was not regenerated", "reason", err.Error())
			// keep what is still valid; do not try again every pass
			_, _ = db.DB.Exec(`UPDATE channel_episode SET regen=2 WHERE id=? AND regen=1`, j.id)
			continue
		case err != nil:
			return n, err
		}
		_, err = s.saveEpisodeOps(ctx, db, j.r, lines, res.Ops, j.id)
		if errors.Is(err, errEvidenceDeleted) {
			continue
		}
		if err != nil {
			return n, err
		}
		if len(res.Ops) == 0 {
			// nothing worth keeping remains: the old episode goes
			if err := db.WithWriter(func(tx *sql.Tx) error { return deleteEpisodeTx(tx, j.id) }); err != nil {
				return n, err
			}
		}
		n++
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// room state (L1)
// ---------------------------------------------------------------------------

type stateRow struct {
	body                episodes.State
	anchorObs, anchorAt string
	revision, updatedN  int64
	updatedAt           string
	exists              bool
}

func (s *Service) loadState(db *storage.Store, guild, channel string) stateRow {
	var row stateRow
	var body string
	err := db.Reader().QueryRow(`SELECT body, anchor_observation, anchor_time, revision, updated_n, updated_at FROM channel_state WHERE guild_id=? AND channel_id=?`, guild, channel).
		Scan(&body, &row.anchorObs, &row.anchorAt, &row.revision, &row.updatedN, &row.updatedAt)
	if err != nil {
		return stateRow{}
	}
	row.exists = json.Unmarshal([]byte(body), &row.body) == nil
	return row
}

// updateRoomState updates a room's state when the debounce says so. It reports whether a state was saved.
func (s *Service) updateRoomState(ctx context.Context, db *storage.Store, r memRoom, now time.Time) (bool, error) {
	row := s.loadState(db, r.guild, r.channel)
	var lines []roomLine
	var err error
	if row.exists && row.anchorAt != "" {
		anchor, _ := time.Parse(time.RFC3339Nano, row.anchorAt)
		lines, err = s.roomLinesSince(db, r.room, anchor, stateLines)
	} else {
		lines, err = s.roomLinesSince(db, r.room, now.Add(-episodes.DropAfter), stateLines)
	}
	if err != nil || len(lines) == 0 {
		return false, err
	}
	if !stateSchedule.Due(len(lines), lines[len(lines)-1].At, now) {
		return false, nil
	}
	full := !row.exists || stateSchedule.Full(int(row.updatedN))
	if full && row.exists {
		// made again from the raw messages: the recent window, not only what is new
		if lines, err = s.roomLinesSince(db, r.room, now.Add(-episodes.DropAfter), stateLines); err != nil || len(lines) == 0 {
			return false, err
		}
	}
	replies := map[string]string{}
	var cur episodeCursor
	if ok, _ := s.kvGet(cursorKey(r.room), &cur); ok && cur.Replies != nil {
		replies = cur.Replies
	}
	bots := s.liveContext(r, lines, replies)
	pack, botName := s.roomVoice(r)
	p := episodes.Build(episodes.Input{Lines: segmentMessages(lines, bots), BotName: botName, Now: now, Location: s.instanceZone(),
		RefOf: func(m episodes.Message) string { return personRefOf(r, m) }, LabelOf: s.refLabels(db, r.room)})
	previous := "-"
	if !full {
		previous = p.Previous(row.body)
	}
	system := lang.Fill(pack.Memory.ChannelStateSystem, "bot", botName)
	prompt := lang.Fill(pack.Memory.ChannelState, "now", p.Now, "people", p.People(), "previous", previous, "lines", p.Lines)
	var st episodes.State
	err = s.askValidated(ctx, spendState, system, prompt, stateTokens, func(text string, final bool) error {
		var problems []string
		var err error
		if st, problems, err = p.ParseState(text); err != nil {
			return err
		}
		if len(problems) > 0 {
			// as with episodes: entries that validated are kept from the last answer (a banter note
			// without evidence left the whole state unchanged 16 times in a week). A state with no entry
			// left would erase the room's topics, so that one is still refused.
			if final && !st.Empty() {
				s.Log.Info("memory: kept the valid part of an answer", "kind", "state", "dropped", clip(strings.Join(problems, "; "), 300))
				return nil
			}
			return invalidAnswer(problems)
		}
		return nil
	})
	last := lines[len(lines)-1]
	for i := len(lines) - 1; i >= 0 && last.Bot; i-- {
		last = lines[i]
	}
	if errors.Is(err, episodes.ErrParse) || errors.Is(err, errExtractInvalid) {
		s.Log.Warn("memory: the channel state was not updated", "reason", err.Error())
		// these messages are skipped; the state ages until the next update
		_ = db.WithWriter(func(tx *sql.Tx) error {
			_, err := tx.Exec(`UPDATE channel_state SET anchor_observation=?, anchor_time=? WHERE guild_id=? AND channel_id=? AND revision=?`,
				last.ID, last.At.UTC().Format(time.RFC3339Nano), r.guild, r.channel, row.revision)
			return err
		})
		return false, nil
	}
	if err != nil {
		return false, err
	}
	st.Age(now)
	err = s.saveRoomState(ctx, db, r, row, st, lines, last, now)
	if errors.Is(err, errEvidenceDeleted) {
		return false, nil
	}
	return err == nil, err
}

func (s *Service) saveRoomState(ctx context.Context, db *storage.Store, r memRoom, prev stateRow, st episodes.State, lines []roomLine, last roomLine, now time.Time) error {
	return db.WithWriterContext(ctx, func(tx *sql.Tx) error {
		for _, l := range lines {
			if l.Bot {
				continue
			}
			var content, deleted string
			if err := tx.QueryRowContext(ctx, `SELECT content, deleted_at FROM memory_observations WHERE observation_id=?`, l.ID).Scan(&content, &deleted); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return errEvidenceDeleted
				}
				return err
			}
			current, err := store.ObservationCurrent(tx, l.ID)
			if err != nil {
				return err
			}
			if deleted != "" || strings.TrimSpace(content) != l.Text || !current {
				return errEvidenceDeleted
			}
		}
		// what an earlier state carried over must still stand, and nobody it names may have withdrawn
		for _, id := range st.Evidence() {
			current, err := store.ObservationCurrent(tx, id)
			if err != nil {
				return err
			}
			if !current {
				return errEvidenceDeleted
			}
		}
		for _, ref := range st.Refs() {
			if blocked, err := store.SourceBlocked(tx, ref); err != nil || blocked {
				if err != nil {
					return err
				}
				return errEvidenceDeleted
			}
		}
		var revision int64
		err := tx.QueryRow(`SELECT revision FROM channel_state WHERE guild_id=? AND channel_id=?`, r.guild, r.channel).Scan(&revision)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if prev.exists {
				return errEvidenceDeleted // invalidated while it was being made
			}
		case err != nil:
			return err
		case !prev.exists || revision != prev.revision:
			return errEvidenceDeleted // another update won
		}
		body, _ := json.Marshal(st)
		ev, _ := json.Marshal(nonEmpty(st.Evidence()))
		holder := store.StateHolderID(r.guild, r.channel)
		if _, err := tx.Exec(`INSERT INTO channel_state(guild_id,channel_id,room,body,evidence,anchor_observation,anchor_time,revision,updated_n,updated_at) VALUES(?,?,?,?,?,?,?,1,1,?)
			ON CONFLICT(guild_id,channel_id) DO UPDATE SET body=excluded.body, evidence=excluded.evidence, anchor_observation=excluded.anchor_observation,
			anchor_time=excluded.anchor_time, revision=channel_state.revision+1, updated_n=channel_state.updated_n+1, updated_at=excluded.updated_at`,
			r.guild, r.channel, r.room, string(body), string(ev), last.ID, last.At.UTC().Format(time.RFC3339Nano), now.UTC().Format(time.RFC3339)); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM evidence_refs WHERE holder_kind=? AND holder_id=?`, store.HolderChannelState, holder); err != nil {
			return err
		}
		keys, err := copySources(tx, st.Evidence(), st.Refs())
		if err != nil {
			return err
		}
		return indexCopy(tx, store.HolderChannelState, holder, keys, false)
	})
}

// roomOf names the room of a channel (engine.RoomKey).
func roomOf(guild, channel string) string { return engine.RoomKey(guild, channel) }

// ---------------------------------------------------------------------------
// invalidation (called by processRedactions)
// ---------------------------------------------------------------------------

// invalidateRoomMemory applies a batch of redaction jobs to room memory: items citing a deleted message
// are hidden, items citing an edited one are hidden as dirty, a withdrawn person's items are deleted and
// they leave the participants, and a room state citing any of them is deleted. An episode left without
// items is deleted; one that lost 40% of its items, or a decision, is queued for regeneration.
func (s *Service) invalidateRoomMemory(ctx context.Context, db *storage.Store, jobs []redactionJob) error {
	if !db.IsSQLite() {
		return nil
	}
	var exists int
	if err := db.Reader().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='channel_episode'`).Scan(&exists); err != nil || exists == 0 {
		return err
	}
	type change struct {
		keys  []string
		state string // hidden | dirty | delete
		ref   string
	}
	var changes []change
	for _, j := range jobs {
		op := j.Operation
		if op == "" && j.User != "" {
			op = "withdraw"
		}
		keys := uniqueStrings(append(append(append([]string{}, j.Messages...), j.Obs...), j.Leaves...))
		switch op {
		case "withdraw", "withdraw_batch":
			if j.Ref != "" {
				keys = append(keys, j.Ref)
			}
			changes = append(changes, change{keys: keys, state: "delete", ref: j.Ref})
		case "edit":
			changes = append(changes, change{keys: keys, state: "dirty"})
		default:
			changes = append(changes, change{keys: keys, state: "hidden"})
		}
	}
	err := db.WithWriterContext(ctx, func(tx *sql.Tx) error {
		touched := map[string]bool{}
		lost := map[string]lostItems{} // items a withdrawal deleted, per episode
		for _, c := range changes {
			keys := uniqueStrings(c.keys) // drops empty keys
			if len(keys) == 0 {
				continue
			}
			raw, _ := json.Marshal(keys)
			holders, err := holdersOf(tx, string(raw))
			if err != nil {
				return err
			}
			for _, h := range holders {
				switch h.kind {
				case store.HolderChannelState:
					if err := deleteStateTx(tx, h.id); err != nil {
						return err
					}
				case store.HolderEpisodeItem:
					var episode, kind string
					if err := tx.QueryRow(`SELECT episode_id, kind FROM episode_item WHERE id=?`, h.id).Scan(&episode, &kind); err != nil {
						if errors.Is(err, sql.ErrNoRows) {
							continue
						}
						return err
					}
					touched[episode] = true
					if c.state == "delete" {
						l := lost[episode]
						l.items++
						l.decision = l.decision || kind == "decision"
						lost[episode] = l
						if _, err := tx.Exec(`DELETE FROM evidence_refs WHERE holder_kind=? AND holder_id=?`, store.HolderEpisodeItem, h.id); err != nil {
							return err
						}
						if _, err := tx.Exec(`DELETE FROM episode_item WHERE id=?`, h.id); err != nil {
							return err
						}
						continue
					}
					// a deletion wins over an edit: dirty never turns a hidden item back
					if _, err := tx.Exec(`UPDATE episode_item SET state=? WHERE id=? AND (state='active' OR ?='hidden')`, c.state, h.id, c.state); err != nil {
						return err
					}
				}
			}
			if c.ref != "" {
				ids, err := episodesNaming(tx, c.ref)
				if err != nil {
					return err
				}
				for _, id := range ids {
					if err := dropParticipant(tx, id, c.ref); err != nil {
						return err
					}
					touched[id] = true
				}
			}
		}
		for id := range touched {
			if err := settleEpisode(tx, id, lost[id]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return s.invalidateCards(ctx, db, jobs)
}

type holder struct{ kind, id string }

func holdersOf(tx *sql.Tx, keysJSON string) ([]holder, error) {
	rows, err := tx.Query(`SELECT DISTINCT holder_kind, holder_id FROM evidence_refs WHERE holder_kind IN (?,?) AND source_id IN (SELECT value FROM json_each(?))`,
		store.HolderEpisodeItem, store.HolderChannelState, keysJSON)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []holder
	for rows.Next() {
		var h holder
		if err := rows.Scan(&h.kind, &h.id); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func deleteStateTx(tx *sql.Tx, holderID string) error {
	if _, err := tx.Exec(`DELETE FROM evidence_refs WHERE holder_kind=? AND holder_id=?`, store.HolderChannelState, holderID); err != nil {
		return err
	}
	_, err := tx.Exec(`DELETE FROM channel_state WHERE 'state:'||guild_id||':'||channel_id=?`, holderID)
	return err
}

func episodesNaming(tx *sql.Tx, ref string) ([]string, error) {
	rows, err := tx.Query(`SELECT DISTINCT e.id FROM channel_episode e, json_each(e.participants) p WHERE p.value=?`, ref)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func dropParticipant(tx *sql.Tx, episode, ref string) error {
	var raw string
	if err := tx.QueryRow(`SELECT participants FROM channel_episode WHERE id=?`, episode).Scan(&raw); err != nil {
		return err
	}
	var list []string
	_ = json.Unmarshal([]byte(raw), &list)
	kept := []string{}
	for _, p := range list {
		if p != ref {
			kept = append(kept, p)
		}
	}
	out, _ := json.Marshal(kept)
	_, err := tx.Exec(`UPDATE channel_episode SET participants=? WHERE id=?`, string(out), episode)
	return err
}

// settleEpisode decides what an episode that lost items becomes: deleted when nothing active is left,
// queued for regeneration when 40% of its items or a decision were hidden or deleted, unchanged
// otherwise. Its revision
// moves either way, so an extraction that read it before cannot update it.
type lostItems struct {
	items    int
	decision bool
}

func settleEpisode(tx *sql.Tx, id string, lost lostItems) error {
	var total, hidden, decisionHidden, active int
	err := tx.QueryRow(`SELECT COUNT(*), COALESCE(SUM(state<>'active'),0), COALESCE(SUM(state<>'active' AND kind='decision'),0), COALESCE(SUM(state='active'),0)
		FROM episode_item WHERE episode_id=?`, id).Scan(&total, &hidden, &decisionHidden, &active)
	if err != nil {
		return err
	}
	if active == 0 {
		return deleteEpisodeTx(tx, id)
	}
	regen := 0
	// items a withdrawal deleted count as hidden: what is left is made again without them
	if episodes.NeedsRegeneration(total+lost.items, hidden+lost.items, decisionHidden > 0 || lost.decision) {
		regen = 1
	}
	// regen=3 marks an episode a member corrected or forgot from (explicit.go): it is never made again
	// from the same raw lines, which would bring back what they asked to drop
	_, err = tx.Exec(`UPDATE channel_episode SET regen=CASE WHEN ?=1 AND regen<>3 THEN 1 ELSE regen END, revision=revision+1 WHERE id=?`, regen, id)
	return err
}

// ---------------------------------------------------------------------------
// Kakao blocks (memory.episodes.extract)
// ---------------------------------------------------------------------------

func kakaoJobKey(room string) string { return "episodes:kakao:" + room }

// kakaoJob is an approved one-off extraction of a Kakao block, continued by the background pass.
type kakaoJob struct {
	Block     string  `json:"block"`
	Room      string  `json:"room"`
	BudgetUSD float64 `json:"budget_usd"`
	SpentUSD  float64 `json:"spent_usd"`
	Segments  int     `json:"segments"`
	Episodes  int     `json:"episodes"`
	Failed    int     `json:"failed"`
	Done      bool    `json:"done"`
	Stopped   string  `json:"stopped,omitempty"` // budget | error
	Started   string  `json:"started"`
}

const kakaoJobsKey = "episodes:kakao:jobs"

func (s *Service) kakaoJobs() []string {
	var rooms []string
	_, _ = s.kvGet(kakaoJobsKey, &rooms)
	return rooms
}

// continueKakaoExtractions runs the approved Kakao extractions a step further.
func (s *Service) continueKakaoExtractions(ctx context.Context, db *storage.Store, now time.Time) (extractStats, error) {
	var total extractStats
	for _, room := range s.kakaoJobs() {
		var job kakaoJob
		if ok, _ := s.kvGet(kakaoJobKey(room), &job); !ok || job.Done {
			continue
		}
		st, err := s.stepKakao(ctx, db, &job, now, kakaoSegments)
		total.segments, total.saved, total.failed = total.segments+st.segments, total.saved+st.saved, total.failed+st.failed
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// stepKakao extracts up to limit segments of a Kakao block and saves the job's progress.
func (s *Service) stepKakao(ctx context.Context, db *storage.Store, job *kakaoJob, now time.Time, limit int) (extractStats, error) {
	r := memRoom{channel: job.Room, room: job.Room, source: "kakao:" + job.Block}
	var st extractStats
	var err error
	for st.segments+st.failed < limit && !job.Done {
		if job.SpentUSD >= job.BudgetUSD {
			job.Stopped = "budget"
			break
		}
		before := s.derivedSpent()
		var step extractStats
		step, err = s.extractRoom(ctx, db, r, now, min(limit-st.segments-st.failed, 10))
		job.SpentUSD += max(0, s.derivedSpent()-before)
		st.segments, st.saved, st.failed = st.segments+step.segments, st.saved+step.saved, st.failed+step.failed
		job.Segments, job.Episodes, job.Failed = job.Segments+step.segments, job.Episodes+step.saved, job.Failed+step.failed
		if err != nil {
			job.Stopped = "error"
			if errors.Is(err, errRoomMemoryStop) || errors.Is(err, ErrChatBudget) {
				job.Stopped = "budget"
			}
			break
		}
		if step.segments+step.failed == 0 {
			job.Done, job.Stopped = true, ""
		}
	}
	if serr := s.kvSet(kakaoJobKey(job.Room), job); serr != nil && err == nil {
		err = serr
	}
	return st, err
}

// kakaoEstimate segments a whole Kakao block and estimates what extracting it costs.
func (s *Service) kakaoEstimate(db *storage.Store, r memRoom) (map[string]any, error) {
	lines, err := s.roomLines(db, r.room, "0", 1_000_000)
	if err != nil {
		return nil, err
	}
	pack, _ := s.roomVoice(r)
	out := map[string]any{"messages": len(lines), "segments": 0, "est_input_tokens": 0, "est_output_tokens": 0, "est_cost_usd": 0.0, "price_known": false}
	if len(lines) == 0 {
		return out, nil
	}
	segs := episodes.Split(withReplies(lines, nil), lines[len(lines)-1].At.Add(24*time.Hour), episodes.Options{Location: s.naiveZone(),
		DayBoundary: s.Doc().Memory.Boundary(), Tokenizer: tokenizerFor(pack)})
	byID := map[string]int{}
	for _, l := range lines {
		byID[l.ID] = len([]rune(l.Text)) + len([]rune(l.Author)) + 12
	}
	overhead := len([]rune(pack.Memory.EpisodeExtractSystem)) + len([]rune(pack.Memory.EpisodeExtract)) + 200
	chars, n := 0, 0
	for _, sg := range segs {
		if len(sg.IDs) < 2 {
			continue // one message is not a conversation; it costs nothing either (see extractSegment)
		}
		n++
		chars += overhead
		for _, id := range sg.IDs {
			chars += byID[id]
		}
	}
	in := int(float64(chars) / 2.5)
	outTok := n * 250
	out["segments"], out["est_input_tokens"], out["est_output_tokens"] = n, in, outTok
	role := "summary"
	if _, ok := modelOf(s.Doc(), role); !ok {
		role = "response"
	}
	if m, ok := modelOf(s.Doc(), role); ok {
		if p, _, ok := s.modelPrice(m.Provider, m.Model); ok {
			out["est_cost_usd"] = float64(in)*p.In + float64(outTok)*p.Out
			out["price_known"] = true
		}
		out["model"] = m.Model
	}
	return out, nil
}
