package application

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"masamong/internal/chat"
	"masamong/internal/config"
	"masamong/internal/conversation"
	"masamong/internal/lang"
	"masamong/internal/memory/blocks"
	"masamong/internal/memory/engine"
	"masamong/internal/memory/store"
	"masamong/internal/storage"
)

// Threads (docs/plan-memory-conversation.md §5.3).
//
// A Discord thread is its own channel ID, so without help every thread is a fresh room: no persona, no
// rules, capture off, no memory. A thread is part of its parent channel here — it follows the parent's
// policy (unless it has an entry of its own), its messages are searched with the parent's memory and the
// parent's with its, and a pause of the parent pauses it.
//
// A long one-on-one conversation clutters a shared channel, so the bot may offer to move it: a button
// under its reply, never a separate message, at most once per conversation, not again for 30 minutes
// after a "no", at most twice an hour per channel, and never when several people are talking to the bot
// together (watching that is the point). In a thread the bot made it answers without a tag, like a chat
// with one person, but still leaves alone what is said to someone else. When the thread goes quiet or is
// stopped, a thread where a plan was made posts a short recap in the parent channel.
func init() {
	featureDDL = append(featureDDL, `CREATE TABLE IF NOT EXISTS channel_threads (
		thread_id TEXT PRIMARY KEY,
		guild_id TEXT NOT NULL,
		parent_id TEXT NOT NULL,
		bot_owned INTEGER NOT NULL DEFAULT 0,
		owner_id TEXT NOT NULL DEFAULT '',
		origin_turn TEXT NOT NULL DEFAULT '',
		state TEXT NOT NULL DEFAULT 'open',
		created_at TEXT NOT NULL,
		last_at TEXT NOT NULL,
		closed_at TEXT NOT NULL DEFAULT '',
		close_reason TEXT NOT NULL DEFAULT ''
	)`, `CREATE INDEX IF NOT EXISTS idx_channel_threads_parent ON channel_threads(guild_id, parent_id)`,
		`CREATE INDEX IF NOT EXISTS idx_channel_threads_open ON channel_threads(state, last_at)`,
		`CREATE TABLE IF NOT EXISTS thread_participants (
			thread_id TEXT NOT NULL,
			user_id TEXT NOT NULL,
			added_at TEXT NOT NULL,
			PRIMARY KEY(thread_id, user_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_thread_participants_user ON thread_participants(user_id)`,
		`CREATE TABLE IF NOT EXISTS thread_tombstones (thread_id TEXT PRIMARY KEY)`,
		// Retain only thread IDs permanently, even after retention removes the closed relation row.
		`INSERT OR IGNORE INTO thread_tombstones(thread_id) SELECT thread_id FROM channel_threads WHERE close_reason='deleted'`)
}

var threadsTicking atomic.Bool

// Every relation/participant lookup uses the same durable deletion boundary.
func threadVisibleSQL(table string) string {
	return table + `.close_reason<>'deleted' AND NOT EXISTS (SELECT 1 FROM thread_tombstones d WHERE d.thread_id=` + table + `.thread_id)`
}

var threadTestHooks struct {
	afterRead              atomic.Pointer[func(threadID string)]
	afterRoomsRead         atomic.Pointer[func(parentID string)]
	beforeArchiveDB        atomic.Pointer[func(threadID string)]
	beforeArchiveWrite     atomic.Pointer[func(threadID string)]
	beforeCloseWrite       atomic.Pointer[func(threadID string)]
	beforeParticipantWrite atomic.Pointer[func(threadID string)]
}

const (
	threadSeedTurns     = 16
	threadOfferScore    = 3
	threadOfferGap      = 15 * time.Minute // a conversation's turns are this close
	threadDeclinedQuiet = 30 * time.Minute
	threadOffersPerHour = 2
	threadKeepClosed    = 30 * 24 * time.Hour // a closed thread stays in memory this long after its last message
	// A thread of people (not the bot's own) that has been quiet this long leaves memory: its row stays and
	// whatever looks the thread up next (threadParent, ThreadFor, a message in it) reads the row back
	// (recoverThread), so leaving memory changes nothing but the memory used, whether or not the gateway
	// still remembers the thread. The count cap bounds a server that makes threads all the time; the bot's
	// own open threads are never dropped here (closeIdleThreads closes them by policy).
	threadKeepHumanOpen = 24 * time.Hour
	threadMaxKnown      = 2048
	// A channel with no thread row is not looked up again for this long (threadNotThreadMax channels at
	// most). Registration, recovery and reactivation invalidate a previous miss.
	threadNotThreadTTL = 10 * time.Minute
	threadNotThreadMax = 4096
)

type threadInfo struct {
	Guild, Parent string
	BotOwned      bool
	Owner         string
	OriginTurn    string
	Open          bool
	LastAt        time.Time
	seeded        map[string]bool // parent messages copied in at the open (not part of the recap)
	humans        map[string]bool // who spoke in the thread (since the bot saw it)
	participants  map[string]bool // the owner and whoever spoke to the bot in it
}

// offerTrack is one caller's conversation in a channel, for the offer score.
type offerTrack struct {
	turns         []time.Time
	chars         int
	offered       bool
	declinedUntil time.Time
}

// threadTrackIdle: a track this quiet is a finished conversation, and its "no" has run out too.
const threadTrackIdle = time.Hour

// note adds a turn and returns how many of the track's turns are within threadOfferGap: only those count
// for the score, so only those are kept.
func (tr *offerTrack) note(now time.Time) int {
	kept := tr.turns[:0]
	for _, at := range tr.turns {
		if now.Sub(at) <= threadOfferGap {
			kept = append(kept, at)
		}
	}
	tr.turns = append(kept, now)
	return len(tr.turns)
}

// sweepTracks forgets the tracks of conversations quiet for threadTrackIdle and the offers older than an
// hour (the lock held).
func (t *threadState) sweepTracks(now time.Time) {
	for key, tr := range t.tracks {
		if n := len(tr.turns); (n == 0 || now.Sub(tr.turns[n-1]) > threadTrackIdle) && !now.Before(tr.declinedUntil) {
			delete(t.tracks, key)
		}
	}
	for ch, list := range t.offersAt {
		kept := list[:0]
		for _, at := range list {
			if now.Sub(at) < time.Hour {
				kept = append(kept, at)
			}
		}
		if len(kept) == 0 {
			delete(t.offersAt, ch)
		} else {
			t.offersAt[ch] = kept
		}
	}
}

type threadState struct {
	mu sync.Mutex
	// DB writes serialize separately: waiting for the single writer must not block cached messages.
	writeMu  sync.Mutex
	loadMu   sync.Mutex
	scopeGen uint64
	loaded   bool
	threads  map[string]*threadInfo // thread id
	tracks   map[string]*offerTrack // guild:channel:caller
	offersAt map[string][]time.Time // guild:channel → recent offers
	pending  sync.Map               // turn id → ThreadOffer, until the reply is delivered
	// notThread remembers, for threadNotThreadTTL, that a channel has no thread row (true), so the ordinary
	// channels do not ask the database on every message.
	notThread     *ttlCache[bool]
	notThreadOnce sync.Once
	// Generations exist only while reads are in flight; deleting a cache entry does not grow another
	// permanent in-memory table. Deletion authority lives in thread_tombstones on disk.
	reads   map[string]*threadRead
	deleted map[string]bool // deletion writes not yet committed (fail closed on a DB error)
	// deleteWarnAt is when a failed deletion write was last logged as a warning (see persistThreadDeletion).
	deleteWarnAt time.Time
}

const threadDeleteWarnEvery = 10 * time.Minute

type threadRead struct {
	refs int
	gen  uint64
}

// These helpers use mu. A read retains its generation until the last publication attempt finishes.
func (t *threadState) beginRead(id string) (*threadRead, uint64) {
	if t.reads == nil {
		t.reads = map[string]*threadRead{}
	}
	r := t.reads[id]
	if r == nil {
		r = &threadRead{}
		t.reads[id] = r
	}
	r.refs++
	return r, r.gen
}

func (t *threadState) endRead(id string, r *threadRead) {
	t.mu.Lock()
	defer t.mu.Unlock()
	r.refs--
	if r.refs == 0 {
		delete(t.reads, id)
	}
}

func (t *threadState) invalidateRead(id string) {
	t.scopeGen++
	if r := t.reads[id]; r != nil {
		r.gen++
	}
}

func (t *threadState) validRead(id string, r *threadRead, gen uint64) bool {
	return r.gen == gen && !t.deleted[id]
}

// ThreadOffer is what the gateway attaches to a reply: buttons (Auto false) or a thread right away.
type ThreadOffer struct {
	Auto   bool
	Text   string
	Open   string
	Stay   string
	Caller string
	Score  int
	logID  string
	key    string // the caller's conversation (offer track)
	ch     string // guild:channel
	at     time.Time
}

// load reads the known threads once (open ones, and closed ones of the last month for their parent).
func (s *Service) threadsLoaded() *threadState {
	t := &s.threads
	t.mu.Lock()
	loaded := t.loaded
	t.mu.Unlock()
	if loaded {
		return t
	}
	t.loadMu.Lock()
	defer t.loadMu.Unlock()
	t.mu.Lock()
	loaded = t.loaded
	t.mu.Unlock()
	if loaded {
		return t
	}
	threads := map[string]*threadInfo{}
	defer func() {
		t.mu.Lock()
		t.threads, t.loaded = threads, true
		t.mu.Unlock()
	}()
	db, err := s.adminDB()
	if err != nil {
		return t
	}
	since := s.Clock.Now().UTC().Add(-threadKeepClosed).Format(time.RFC3339)
	rows, err := db.Reader().Query(`SELECT thread_id, guild_id, parent_id, bot_owned, owner_id, origin_turn, state, last_at FROM channel_threads
		WHERE `+threadVisibleSQL("channel_threads")+` AND (state='open' OR last_at>=?)`, since)
	if err != nil {
		return t
	}
	defer rows.Close()
	for rows.Next() {
		var id, g, p, owner, origin, state, last string
		var bot int
		if rows.Scan(&id, &g, &p, &bot, &owner, &origin, &state, &last) != nil {
			continue
		}
		at, _ := time.Parse(time.RFC3339, last)
		if state == "open" {
			// activity is kept in memory only: after a restart an open thread is idle from now, not
			// from its last saved write (else every thread older than the idle time closes at once)
			at = s.Clock.Now()
		}
		info := &threadInfo{Guild: g, Parent: p, BotOwned: bot == 1, Owner: owner, OriginTurn: origin, Open: state == "open", LastAt: at,
			humans: map[string]bool{}, participants: map[string]bool{}}
		if owner != "" {
			info.participants[owner], info.humans[owner] = true, true
		}
		threads[id] = info
	}
	_ = rows.Close()
	_, _ = db.DB.Exec(`DELETE FROM thread_participants WHERE thread_id IN (
		SELECT thread_id FROM channel_threads WHERE state<>'open' AND last_at<?)`, since)
	s.loadThreadParticipants(db, threads, since)
	return t
}

func (s *Service) loadThreadParticipants(db *storage.Store, threads map[string]*threadInfo, since string) {
	s.loadThreadParticipantRows(db, threads, `SELECT p.thread_id,p.user_id FROM thread_participants p
		JOIN channel_threads t ON t.thread_id=p.thread_id WHERE `+threadVisibleSQL("t")+` AND (t.state='open' OR t.last_at>=?)`, since)
}

var singleThreadParticipantsSQL = `SELECT p.thread_id,p.user_id FROM thread_participants p
	JOIN channel_threads t ON t.thread_id=p.thread_id WHERE p.thread_id=? AND ` + threadVisibleSQL("t")

// Recovery visits only this thread's participant PK range, whatever the rest of the table holds.
func (s *Service) loadSingleThreadParticipants(db *storage.Store, threadID string, info *threadInfo) int {
	return s.loadThreadParticipantRows(db, map[string]*threadInfo{threadID: info}, singleThreadParticipantsSQL, threadID)
}

func (s *Service) loadThreadParticipantRows(db *storage.Store, threads map[string]*threadInfo, query string, args ...any) int {
	rows, err := db.Reader().Query(query, args...)
	if err != nil {
		return 0
	}
	defer rows.Close()
	visited := 0
	for rows.Next() {
		visited++
		var threadID, userID string
		if rows.Scan(&threadID, &userID) != nil {
			continue
		}
		if info := threads[threadID]; info != nil {
			info.participants[userID] = true
			info.humans[userID] = true
		}
	}
	return visited
}

// NoteThread records that a channel is a thread of parentID (the gateway calls it for every thread it
// sees; it is cheap after the first time).
func (s *Service) NoteThread(guildID, threadID, parentID string, botOwned bool) {
	if threadID == "" || parentID == "" || threadID == parentID {
		return
	}
	t := s.threadsLoaded()
	t.mu.Lock()
	if _, ok := t.threads[threadID]; ok {
		t.clearNotThread(threadID)
		t.mu.Unlock()
		return
	}
	if t.deleted[threadID] {
		t.mu.Unlock()
		return
	}
	r, gen := t.beginRead(threadID)
	t.mu.Unlock()
	defer t.endRead(threadID, r)
	now := s.Clock.Now()
	db, err := s.adminDB()
	if err != nil {
		return
	}
	bot := 0
	if botOwned {
		bot = 1
	}
	// Register only a genuinely new row. An existing deletion marker must never be replaced by a late
	// gateway discovery, and an old closed bot thread retains its stored state.
	at := now.UTC().Format(time.RFC3339)
	t.writeMu.Lock()
	t.mu.Lock()
	valid := t.validRead(threadID, r, gen)
	t.mu.Unlock()
	if valid {
		_, err = db.DB.Exec(`INSERT INTO channel_threads(thread_id,guild_id,parent_id,bot_owned,state,created_at,last_at)
			SELECT ?,?,?,?,'open',?,? WHERE NOT EXISTS (SELECT 1 FROM thread_tombstones WHERE thread_id=?)
			ON CONFLICT(thread_id) DO NOTHING`, threadID, guildID, parentID, bot, at, at, threadID)
	}
	t.writeMu.Unlock()
	if !valid || err != nil {
		return
	}
	info, ok := s.storedThread(threadID, now)
	if !ok {
		return
	}
	if hook := threadTestHooks.afterRead.Load(); hook != nil {
		(*hook)(threadID)
	}
	t.mu.Lock()
	if !t.validRead(threadID, r, gen) {
		t.mu.Unlock()
		return
	}
	if _, ok := t.threads[threadID]; !ok {
		t.threads[threadID] = info
	}
	t.invalidateRead(threadID)
	t.clearNotThread(threadID)
	t.mu.Unlock()
}

// storedThread reads a thread's row, active from now.
func (s *Service) storedThread(threadID string, now time.Time) (*threadInfo, bool) {
	db, err := s.adminDB()
	if err != nil {
		return nil, false
	}
	var g, p, owner, origin, state string
	var bot int
	if db.Reader().QueryRow(`SELECT guild_id, parent_id, bot_owned, owner_id, origin_turn, state FROM channel_threads WHERE thread_id=? AND `+threadVisibleSQL("channel_threads"), threadID).
		Scan(&g, &p, &bot, &owner, &origin, &state) != nil {
		return nil, false
	}
	info := &threadInfo{Guild: g, Parent: p, BotOwned: bot == 1, Owner: owner, OriginTurn: origin, Open: state == "open", LastAt: now,
		humans: map[string]bool{}, participants: map[string]bool{}}
	if owner != "" {
		info.participants[owner], info.humans[owner] = true, true
	}
	s.loadSingleThreadParticipants(db, threadID, info)
	return info, true
}

func (s *Service) saveThread(threadID string) {
	t := &s.threads
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	s.saveThreadLocked(threadID)
}

// saveThreadLocked holds writeMu, never mu, during SQL.
func (s *Service) saveThreadLocked(threadID string) {
	db, err := s.adminDB()
	if err != nil {
		return
	}
	t := &s.threads
	t.mu.Lock()
	info, ok := t.threads[threadID]
	if !ok {
		t.mu.Unlock()
		return
	}
	c := *info
	t.mu.Unlock()
	state, bot := "closed", 0
	if c.Open {
		state = "open"
	}
	if c.BotOwned {
		bot = 1
	}
	now := s.Clock.Now().UTC().Format(time.RFC3339)
	if _, err := db.DB.Exec(`INSERT INTO channel_threads(thread_id,guild_id,parent_id,bot_owned,owner_id,origin_turn,state,created_at,last_at)
		SELECT ?,?,?,?,CASE WHEN EXISTS (SELECT 1 FROM consent_state WHERE user_id=? AND scope='memory' AND status='withdrawn') THEN '' ELSE ? END,?,?,?,? WHERE NOT EXISTS (SELECT 1 FROM thread_tombstones WHERE thread_id=?)
		ON CONFLICT(thread_id) DO UPDATE SET bot_owned=excluded.bot_owned, owner_id=excluded.owner_id,
		origin_turn=excluded.origin_turn, state=excluded.state, last_at=excluded.last_at WHERE `+threadVisibleSQL("channel_threads"),
		threadID, c.Guild, c.Parent, bot, c.Owner, c.Owner, c.OriginTurn, state, now, c.LastAt.UTC().Format(time.RFC3339), threadID); err != nil {
		s.Log.Debug("thread record write failed", "err", err.Error())
	}
}

// threadParent is the parent channel of a known thread ("" for a channel). A thread that left memory is
// read back from its row first (ensureThread), so the answer does not depend on what the table holds now.
func (s *Service) threadParent(channelID string) string {
	if channelID == "" {
		return ""
	}
	t := s.ensureThread(channelID, false)
	t.mu.Lock()
	defer t.mu.Unlock()
	if info, ok := t.threads[channelID]; ok {
		return info.Parent
	}
	return ""
}

// ensureThread puts a thread that is not in the table (let go of when idle or over the cap, or closed long
// ago) back from its row, and returns the table. A channel with no row is remembered for a while so the
// ordinary channels cost no query per message. anyAge also reads back a closed thread older than the keep
// time (used by tests for old durable rows).
func (s *Service) ensureThread(channelID string, anyAge bool) *threadState {
	t := s.threadsLoaded()
	t.mu.Lock()
	_, ok := t.threads[channelID]
	t.mu.Unlock()
	if !ok {
		s.recoverThread(channelID, anyAge)
	}
	return t
}

func (t *threadState) notThreadCache() *ttlCache[bool] {
	t.notThreadOnce.Do(func() { t.notThread = newTTLCache[bool](threadNotThreadMax, threadNotThreadTTL) })
	return t.notThread
}

func (t *threadState) clearNotThread(id string) {
	cache := t.notThreadCache()
	cache.mu.Lock()
	delete(cache.m, id)
	cache.mu.Unlock()
}

// recoverThread reads one thread's row into the table (not a deleted one, and not a closed one past
// threadKeepClosed unless anyAge: what a restart would not load either). It reports whether the thread is in
// the table afterwards.
func (s *Service) recoverThread(threadID string, anyAge bool) bool {
	for attempt := 0; attempt < 2; attempt++ {
		found, retry := s.recoverThreadOnce(threadID, anyAge)
		if found || !retry {
			return found
		}
	}
	return false
}

func (s *Service) recoverThreadOnce(threadID string, anyAge bool) (bool, bool) {
	t := &s.threads
	now := s.Clock.Now()
	cache := t.notThreadCache()
	t.mu.Lock()
	if t.deleted[threadID] {
		t.mu.Unlock()
		return false, false
	}
	if t.threads[threadID] != nil {
		t.clearNotThread(threadID)
		t.mu.Unlock()
		return true, false
	}
	if v, _, ok := cache.get(threadID, now); ok && v && !anyAge {
		t.mu.Unlock()
		return false, false
	}
	r, gen := t.beginRead(threadID)
	t.mu.Unlock()
	defer t.endRead(threadID, r)
	db, err := s.adminDB()
	if err != nil {
		return false, false
	}
	var g, p, owner, origin, state, last string
	var bot int
	err = db.Reader().QueryRow(`SELECT guild_id, parent_id, bot_owned, owner_id, origin_turn, state, last_at FROM channel_threads
		WHERE thread_id=? AND `+threadVisibleSQL("channel_threads"), threadID).Scan(&g, &p, &bot, &owner, &origin, &state, &last)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) && !s.threadDeleted(threadID) {
			t.mu.Lock()
			valid := t.validRead(threadID, r, gen)
			if valid {
				cache.put(threadID, true, now)
			}
			retry := !valid && !t.deleted[threadID]
			t.mu.Unlock()
			if retry {
				return false, true
			}
		}
		return false, false
	}
	at, _ := time.Parse(time.RFC3339, last)
	if state == "open" {
		at = now // activity is kept in memory only (as threadsLoaded)
	} else if !anyAge && now.Sub(at) > threadKeepClosed {
		t.mu.Lock()
		valid := t.validRead(threadID, r, gen)
		if valid {
			cache.put(threadID, true, now)
		}
		retry := !valid && !t.deleted[threadID]
		t.mu.Unlock()
		return false, retry
	}
	info := &threadInfo{Guild: g, Parent: p, BotOwned: bot == 1, Owner: owner, OriginTurn: origin, Open: state == "open", LastAt: at,
		humans: map[string]bool{}, participants: map[string]bool{}}
	if owner != "" {
		info.participants[owner], info.humans[owner] = true, true
	}
	s.loadSingleThreadParticipants(db, threadID, info)
	if hook := threadTestHooks.afterRead.Load(); hook != nil {
		(*hook)(threadID)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.validRead(threadID, r, gen) {
		// An unrelated withdrawal or lifecycle event need not turn this message into a channel miss.
		return false, !t.deleted[threadID]
	}
	if _, ok := t.threads[threadID]; !ok {
		t.threads[threadID] = info
	}
	// Also invalidate a concurrent reader that saw no row before this successful publication.
	t.invalidateRead(threadID)
	t.clearNotThread(threadID)
	return true, false
}

// threadRooms are the rooms of a channel's known threads: the table's and those of the stored rows that
// left memory (open ones, and closed ones of the last month), so searching the parent does not change
// when a thread is let go of.
func (s *Service) threadRooms(guildID, parentID string) []string {
	t := s.threadsLoaded()
	// A lifecycle change during the DB snapshot requires another read. scopeGen moves on any thread's
	// change, not only this parent's, so a busy server can keep invalidating the read; after the bounded
	// retries the in-memory table alone answers (below) rather than nothing, which would drop every
	// thread from the parent's search.
	for attempt := 0; attempt < 3; attempt++ {
		t.mu.Lock()
		gen := t.scopeGen
		seen := map[string]bool{}
		for id, info := range t.threads {
			if info.Guild == guildID && info.Parent == parentID && !t.deleted[id] {
				seen[id] = true
			}
		}
		t.mu.Unlock()
		if db, err := s.adminDB(); err == nil {
			since := s.Clock.Now().UTC().Add(-threadKeepClosed).Format(time.RFC3339)
			rows, err := db.Reader().Query(`SELECT thread_id FROM channel_threads WHERE guild_id=? AND parent_id=? AND `+threadVisibleSQL("channel_threads")+`
				AND (state='open' OR last_at>=?)`, guildID, parentID, since)
			if err == nil {
				for rows.Next() {
					var id string
					if rows.Scan(&id) == nil {
						seen[id] = true
					}
				}
				_ = rows.Close()
			}
		}
		if hook := threadTestHooks.afterRoomsRead.Load(); hook != nil {
			(*hook)(parentID)
		}
		t.mu.Lock()
		if gen != t.scopeGen {
			t.mu.Unlock()
			continue
		}
		var out []string
		for id := range seen {
			if !t.deleted[id] {
				out = append(out, engine.RoomKey(guildID, id))
			}
		}
		t.mu.Unlock()
		return out
	}
	// Still changing: the table under mu is current (a deletion leaves it under the same lock), so it
	// never returns a deleted thread. It lacks only the stored rows that left memory.
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []string
	for id, info := range t.threads {
		if info.Guild == guildID && info.Parent == parentID && !t.deleted[id] {
			out = append(out, engine.RoomKey(guildID, id))
		}
	}
	return out
}

// A deleted channel has no search scope, including its own room. Known live threads need no read.
func (s *Service) threadDeleted(channelID string) bool {
	t := s.threadsLoaded()
	t.mu.Lock()
	if t.deleted[channelID] {
		t.mu.Unlock()
		return true
	}
	if v, _, ok := t.notThreadCache().get(channelID, s.Clock.Now()); ok && v {
		t.mu.Unlock()
		return false
	}
	_, known := t.threads[channelID]
	t.mu.Unlock()
	if known {
		return false
	}
	db, err := s.adminDB()
	if err != nil {
		return false
	}
	var deleted int
	return db.Reader().QueryRow(`SELECT 1 FROM thread_tombstones WHERE thread_id=?
		UNION ALL SELECT 1 FROM channel_threads WHERE thread_id=? AND close_reason='deleted' LIMIT 1`, channelID, channelID).Scan(&deleted) == nil
}

// noteThreadActivity keeps a thread's last activity and who is in it.
func (s *Service) noteThreadActivity(m ObservedMessage) {
	t := s.ensureThread(m.ChannelID, false)
	t.mu.Lock()
	defer t.mu.Unlock()
	info, ok := t.threads[m.ChannelID]
	if !ok {
		return
	}
	info.LastAt = m.At
	if m.FromBot || m.AuthorID == "" {
		return
	}
	info.humans[m.AuthorID] = true
}

// ThreadView is what the gateway needs to know about a thread to decide whether to answer.
type ThreadView struct {
	BotOwned    bool
	Open        bool
	Participant bool // the author is the owner or spoke to the bot here
	Alone       bool // nobody but the author has spoken here
}

// ThreadFor describes a channel when it is a known thread.
func (s *Service) ThreadFor(channelID, authorID string) (ThreadView, bool) {
	t := s.ensureThread(channelID, false)
	t.mu.Lock()
	defer t.mu.Unlock()
	info, ok := t.threads[channelID]
	if !ok {
		return ThreadView{}, false
	}
	alone := true
	for id := range info.humans {
		if id != authorID {
			alone = false
		}
	}
	return ThreadView{BotOwned: info.BotOwned, Open: info.Open, Participant: info.participants[authorID], Alone: alone}, true
}

// NoteThreadParticipant adds a person who spoke to the bot in a thread.
func (s *Service) NoteThreadParticipant(channelID, authorID string) {
	if authorID == "" {
		return
	}
	t := s.ensureThread(channelID, false)
	t.mu.Lock()
	info := t.threads[channelID]
	if info == nil || info.participants[authorID] || t.deleted[channelID] {
		t.mu.Unlock()
		return
	}
	r, gen := t.beginRead(channelID)
	t.mu.Unlock()
	defer t.endRead(channelID, r)
	persist := s.MemoryAllowed(authorID)
	if hook := threadTestHooks.beforeParticipantWrite.Load(); hook != nil {
		(*hook)(channelID)
	}
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	t.mu.Lock()
	info, ok := t.threads[channelID]
	add := t.validRead(channelID, r, gen) && ok && authorID != "" && !info.participants[authorID] && !t.deleted[channelID]
	if add {
		info.participants[authorID] = true
		info.humans[authorID] = true
	}
	t.mu.Unlock()
	if add && persist {
		if db, err := s.adminDB(); err == nil {
			if _, err := db.DB.Exec(`INSERT OR IGNORE INTO thread_participants(thread_id,user_id,added_at)
				SELECT thread_id,?,? FROM channel_threads WHERE thread_id=? AND `+threadVisibleSQL("channel_threads")+`
				AND NOT EXISTS (SELECT 1 FROM consent_state WHERE user_id=? AND scope='memory' AND status='withdrawn')`,
				authorID, s.Clock.Now().UTC().Format(time.RFC3339Nano), channelID, authorID); err != nil {
				s.Log.Debug("thread participant write failed", "err", err.Error())
			}
		}
	}
}

// ThreadStopCue reports a message that ends the bot's thread ("그만", "!마무리"), from a person the bot
// talks with there (a bystander's "그만" to a friend does not close it).
func (s *Service) ThreadStopCue(guildID, channelID, authorID, text string) bool {
	v, ok := s.ThreadFor(channelID, authorID)
	if !ok || !v.BotOwned || !v.Open || !v.Participant {
		return false
	}
	t := strings.ToLower(strings.TrimSpace(text))
	t = strings.TrimLeft(t, "!/")
	t = strings.TrimRightFunc(t, func(r rune) bool { return unicode.IsPunct(r) || unicode.IsSpace(r) || unicode.IsSymbol(r) })
	if t == "" {
		return false
	}
	pol := s.channelPolicy(guildID, channelID)
	voice := s.voiceFor(guildID, pol)
	for _, p := range []*lang.Pack{voice.Pack, lang.For(chat.DetectLanguage(text), voice.Pack.Tag)} {
		for _, c := range p.Thread.StopCues {
			if strings.ToLower(c) == t {
				return true
			}
		}
	}
	return false
}

// TurnStopCue reports a whole message that asks the bot to drop the reply it is writing ("그만", "stop"),
// in the channel's language or the one the message is written in. The gateway acts on it only while a
// reply for that person is in flight.
func (s *Service) TurnStopCue(guildID, channelID, text string) bool {
	t := cueOf(text)
	if t == "" {
		return false
	}
	voice := s.voiceFor(guildID, s.channelPolicy(guildID, channelID))
	other := lang.For(chat.DetectLanguage(text), voice.Pack.Tag)
	return cueIn(t, voice.Pack.Features.TurnStopCues, other.Features.TurnStopCues)
}

// decideThreadOffer scores the caller's conversation after a reply (before it is delivered) and keeps an
// offer for the gateway when the score reaches threadOfferScore. Every offer and every reason not to
// offer once the score was reached is logged (conversation_log).
func (s *Service) decideThreadOffer(in ChatInput, pol config.ChannelPolicy, res chat.Result, attribution map[string]any) {
	mode := pol.Conversation.Thread
	if mode == "" {
		mode = config.ThreadModes[0]
	}
	if in.DryRun || in.Proactive || in.GuildID == "" || in.ChannelID == "" || in.SpeakerID == "" || strings.TrimSpace(res.Answer) == "" {
		return
	}
	if s.threadParent(in.ChannelID) != "" {
		return // already in a thread
	}
	now := s.Clock.Now()
	key := in.GuildID + ":" + in.ChannelID + ":" + in.SpeakerID
	t := s.threadsLoaded()
	t.mu.Lock()
	if t.tracks == nil {
		t.tracks = map[string]*offerTrack{}
		t.offersAt = map[string][]time.Time{}
	}
	tr := t.tracks[key]
	if tr == nil || len(tr.turns) > 0 && now.Sub(tr.turns[len(tr.turns)-1]) > threadOfferGap {
		declined := time.Time{}
		if tr != nil {
			declined = tr.declinedUntil
		}
		tr = &offerTrack{declinedUntil: declined}
		t.tracks[key] = tr
	}
	recent := tr.note(now)
	t.sweepTracks(now)
	answer := len([]rune(res.Answer))
	tr.chars += answer
	score := 0
	if recent >= 6 {
		score += 2
	}
	if tr.chars >= 1200 || answer >= 600 {
		score += 2
	}
	if len(res.Tools) >= 3 {
		score += 2
	}
	if n, _ := attribution["excluded"].(int); n >= 4 {
		score += 2
	}
	busy := 0
	for _, l := range s.liveLines(in.GuildID, in.ChannelID) {
		if now.Sub(l.At) <= time.Minute {
			busy++
		}
	}
	if busy >= 10 {
		score++
	}
	ch := in.GuildID + ":" + in.ChannelID
	kept := t.offersAt[ch] // swept to the last hour
	decision := ""
	switch {
	case score < threadOfferScore:
	case mode == "off":
		decision = "would_offer"
	case tr.offered:
		decision = "skip_offered"
	case now.Before(tr.declinedUntil):
		decision = "skip_declined"
	case len(kept) >= threadOffersPerHour:
		decision = "skip_rate"
	case callers(attribution) >= 2:
		decision = "skip_group" // several people talk to the bot together: watching is the point
	case mode == "auto_task" && len(res.Tools) >= 3:
		decision = "auto"
	default:
		decision = "offer"
	}
	if decision == "offer" || decision == "auto" {
		tr.offered = true
		t.offersAt[ch] = append(kept, now)
	}
	t.mu.Unlock()
	if decision == "" {
		return
	}
	id := s.logConversation(convDecision{Guild: in.GuildID, Channel: in.ChannelID, Message: in.MessageID, Author: in.SpeakerID,
		Kind: convThread, Decision: decision, Mode: mode, Score: float64(score),
		Features: map[string]any{"turns": recent, "chars": tr.chars, "answer": answer, "tools": len(res.Tools), "busy": busy}})
	if decision != "offer" && decision != "auto" {
		return
	}
	voice := s.voiceFor(in.GuildID, pol)
	th := voice.Pack.Thread
	s.threads.pending.Store(in.turnID, ThreadOffer{Auto: decision == "auto", Text: th.Offer, Open: th.Open, Stay: th.Stay,
		Caller: in.SpeakerID, Score: score, logID: id, key: key, ch: ch, at: now})
	// offers whose reply never reached the channel are given back after a while
	s.threads.pending.Range(func(k, v any) bool {
		if o := v.(ThreadOffer); now.Sub(o.at) > 10*time.Minute {
			s.threads.pending.Delete(k)
		}
		return true
	})
}

// ThreadOfferFailed gives back an offer that was never shown (the reply or the edit failed), so it does
// not count as offered or against the channel's hourly offers.
func (s *Service) ThreadOfferFailed(offer ThreadOffer, reason string) {
	t := s.threadsLoaded()
	t.mu.Lock()
	if tr := t.tracks[offer.key]; tr != nil {
		tr.offered = false
	}
	var kept []time.Time
	for _, at := range t.offersAt[offer.ch] {
		if !at.Equal(offer.at) {
			kept = append(kept, at)
		}
	}
	if t.offersAt != nil {
		t.offersAt[offer.ch] = kept
	}
	t.mu.Unlock()
	guild, channel, _ := strings.Cut(offer.ch, ":")
	s.ThreadOfferDropped(guild, channel, reason)
}

func callers(attribution map[string]any) int {
	n, _ := attribution["callers"].(int)
	return n
}

// TakeThreadOffer hands the gateway the offer of a turn, once.
func (s *Service) TakeThreadOffer(turnID string) (ThreadOffer, bool) {
	v, ok := s.threads.pending.LoadAndDelete(turnID)
	if !ok {
		return ThreadOffer{}, false
	}
	return v.(ThreadOffer), true
}

// ThreadOfferDropped records an offer the gateway could not show (no permission to make threads).
func (s *Service) ThreadOfferDropped(guildID, channelID, reason string) {
	s.logConversation(convDecision{Guild: guildID, Channel: channelID, Kind: convThread, Decision: "skip_" + reason})
}

// DeclineThread is the caller's "keep it here": no offer for this conversation for a while.
func (s *Service) DeclineThread(guildID, channelID, callerID string) {
	t := s.threadsLoaded()
	t.mu.Lock()
	if tr := t.tracks[guildID+":"+channelID+":"+callerID]; tr != nil {
		tr.declinedUntil = s.Clock.Now().Add(threadDeclinedQuiet)
	}
	t.mu.Unlock()
	s.logConversation(convDecision{Guild: guildID, Channel: channelID, Author: callerID, Kind: convThread, Decision: "declined"})
}

// ThreadOpened is a thread the bot just made for a caller's conversation. It seeds the thread's context
// with the caller's last turns in the parent channel and returns the first message to post in it.
func (s *Service) ThreadOpened(ctx context.Context, guildID, parentID, threadID, callerID, turnID string) string {
	s.NoteThread(guildID, threadID, parentID, true)
	t := s.threadsLoaded()
	t.writeMu.Lock()
	// Consent is read under writeMu on purpose: forgetThreadOwner (a withdrawal) takes the same lock, so a
	// withdrawal either comes first (no owner here) or clears this owner after. The database is read only
	// until this service's opt-out cache has loaded once successfully; later calls use the cache.
	owner := callerID
	if !s.MemoryAllowed(callerID) {
		owner = ""
	}
	t.mu.Lock()
	if t.threads[threadID] == nil {
		t.mu.Unlock()
		t.writeMu.Unlock()
		return ""
	}
	now := s.Clock.Now()
	t.threads[threadID] = &threadInfo{Guild: guildID, Parent: parentID, BotOwned: true, Owner: owner, OriginTurn: turnID, Open: true,
		LastAt: now, humans: map[string]bool{callerID: true}, participants: map[string]bool{callerID: true}}
	t.invalidateRead(threadID)
	t.clearNotThread(threadID)
	t.mu.Unlock()
	s.saveThreadLocked(threadID)
	t.writeMu.Unlock()
	s.logConversation(convDecision{Guild: guildID, Channel: parentID, Author: callerID, Kind: convThread, Decision: "opened"})
	pol := s.channelPolicy(guildID, parentID)
	voice := s.voiceFor(guildID, pol)
	mention := "<@" + callerID + ">"
	seed := s.seedThread(guildID, parentID, threadID, callerID)
	if len(seed) > 0 {
		t.mu.Lock()
		if info := t.threads[threadID]; info != nil {
			info.seeded = map[string]bool{}
			for _, turn := range seed {
				info.seeded[turn.NativeID] = turn.NativeID != ""
			}
		}
		t.mu.Unlock()
	}
	summary := ""
	if len(seed) > 0 {
		var lines []conversation.Line
		for i, turn := range seed {
			l := conversation.Line{ID: "s" + strconv.Itoa(i), Author: turn.Speaker, Text: turn.Content, At: now}
			if turn.Role == "assistant" {
				l.Bot, l.Author = true, voice.BotName
			}
			lines = append(lines, l)
		}
		summary, _ = s.threadSummary(ctx, guildID, pol, voice, lines)
	}
	if summary == "" {
		return lang.Fill(voice.Pack.Thread.SeedPlain, "mention", mention)
	}
	return lang.Fill(voice.Pack.Thread.Seed, "mention", mention, "summary", summary)
}

// seedThread copies the caller's last turns with the bot in the parent channel into the thread's
// conversation (with their evidence refs, so a deleted message leaves the copy too).
func (s *Service) seedThread(guildID, parentID, threadID, callerID string) []store.ChatTurn {
	eng, err := s.MemoryEngine()
	if err != nil || !s.MemoryAllowed(callerID) {
		return nil
	}
	turns, err := eng.Store.RecentChatTurns(blocks.ChannelKey(guildID, parentID), 40)
	if err != nil {
		return nil
	}
	var mine []store.ChatTurn
	for _, t := range turns {
		if t.SpeakerID == callerID {
			mine = append(mine, t)
		}
	}
	if len(mine) > threadSeedTurns {
		mine = mine[len(mine)-threadSeedTurns:]
	}
	if len(mine) > 0 {
		_ = eng.Store.AppendChatTurns(blocks.ChannelKey(guildID, threadID), mine, 40)
	}
	return mine
}

// threadSummary asks for the conversation's gist in two or three lines and whether a plan was settled.
func (s *Service) threadSummary(ctx context.Context, guildID string, pol config.ChannelPolicy, voice chat.Voice, lines []conversation.Line) (string, bool) {
	routes, err := s.chatRoutes(s.Doc())
	if err != nil {
		return "", false
	}
	text, _ := conversation.Numbered(lines, voice.BotName, 400)
	if strings.TrimSpace(text) == "" {
		return "", false
	}
	th := voice.Pack.Thread
	msgs := []chat.Msg{{Role: "system", Content: lang.Fill(th.SummarySystem, "bot", voice.BotName)},
		{Role: "user", Content: lang.Fill(th.Summary, "lines", clip(text, 6000))}}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cctx = withLedgerRole(cctx, "thread_summary")
	out, err := chatLLM{s: s, routes: routes}.ChatDetailed(cctx, "planner", msgs, 300, 0)
	if err != nil {
		return "", false
	}
	var reply struct {
		Summary string `json:"summary"`
		HasPlan bool   `json:"has_plan"`
	}
	if !decodeJSONObject(out.Text, &reply) {
		return "", false
	}
	return clip(strings.TrimSpace(reply.Summary), 400), reply.HasPlan
}

// ThreadArchiver is implemented by a gateway that can archive a thread.
type ThreadArchiver interface {
	ArchiveThread(ctx context.Context, threadID string) error
}

// ErrNotThread: the channel is not an open thread the bot made.
var ErrNotThread = errors.New("not an open bot thread")

// CloseThread ends a bot thread: it says goodbye in it, posts a recap in the parent channel when the
// channel's thread_recap asks for it, and archives it. reason: "stop" (button or words) or "idle".
func (s *Service) CloseThread(ctx context.Context, threadID, reason string) error {
	t := s.threadsLoaded()
	t.mu.Lock()
	info, ok := t.threads[threadID]
	if !ok || !info.BotOwned || !info.Open {
		t.mu.Unlock()
		return ErrNotThread
	}
	info.Open = false
	c := *info
	info.Owner = "" // a closed thread keeps no account id (the row is pruned with the turns)
	t.invalidateRead(threadID)
	t.mu.Unlock()
	if hook := threadTestHooks.beforeCloseWrite.Load(); hook != nil {
		(*hook)(threadID)
	}
	// Persist the captured close even if the minute tick evicts the now-closed cache entry.
	t.writeMu.Lock()
	if db, err := s.adminDB(); err == nil {
		// last_at carries the activity kept only in memory while the thread was open: retention and the
		// parent's search scope count a closed thread's age from it, not from when it was opened.
		last := c.LastAt.UTC().Format(time.RFC3339)
		_, err = db.DB.Exec(`UPDATE channel_threads SET state='closed',owner_id='',closed_at=?,close_reason=?,
			last_at=CASE WHEN last_at<? THEN ? ELSE last_at END
			WHERE thread_id=? AND `+threadVisibleSQL("channel_threads"), s.Clock.Now().UTC().Format(time.RFC3339), reason, last, last, threadID)
		if err != nil {
			s.Log.Warn("thread close write failed", "err", err.Error())
		}
	}
	t.mu.Lock()
	t.invalidateRead(threadID)
	if info := t.threads[threadID]; info != nil {
		info.Open, info.Owner = false, ""
	}
	t.mu.Unlock()
	t.writeMu.Unlock()
	pol := s.channelPolicy(c.Guild, c.Parent)
	voice := s.voiceFor(c.Guild, pol)
	poster, _ := s.Discord.(ChannelPoster)
	if poster != nil && reason == "stop" {
		_, _ = poster.PostChannel(ctx, threadID, "", voice.Pack.Thread.Bye, nil)
	}
	recap := pol.Conversation.ThreadRecap
	if recap == "" {
		recap = config.ThreadRecapModes[0]
	}
	decision := "closed_" + reason
	if recap != "off" && poster != nil {
		if eng, err := s.MemoryEngine(); err == nil {
			turns, _ := eng.Store.RecentChatTurns(blocks.ChannelKey(c.Guild, threadID), 40)
			fromParent := map[string]bool{}
			if parent, err := eng.Store.RecentChatTurns(blocks.ChannelKey(c.Guild, c.Parent), 80); err == nil {
				for _, p := range parent {
					fromParent[p.NativeID] = p.NativeID != ""
				}
			}
			var lines []conversation.Line
			for i, turn := range turns {
				// the parent turns seeded at the open are context, not what came out of the thread
				if turn.NativeID != "" && (c.seeded[turn.NativeID] || fromParent[turn.NativeID]) {
					continue
				}
				l := conversation.Line{ID: "t" + strconv.Itoa(i), Author: turn.Speaker, Text: turn.Content, At: s.Clock.Now()}
				if turn.Role == "assistant" {
					l.Bot, l.Author = true, voice.BotName
				}
				lines = append(lines, l)
			}
			if len(lines) >= 2 {
				summary, plan := s.threadSummary(ctx, c.Guild, pol, voice, lines)
				if summary != "" && (recap == "always" || plan) {
					if _, err := poster.PostChannel(ctx, c.Parent, "", lang.Fill(voice.Pack.Thread.Recap, "summary", summary), nil); err == nil {
						decision += "_recap"
					}
				}
			}
		}
	}
	if a, ok := s.Discord.(ThreadArchiver); ok {
		if err := a.ArchiveThread(ctx, threadID); err != nil {
			s.Log.Debug("thread archive failed", "err", err.Error())
		}
	}
	s.logConversation(convDecision{Guild: c.Guild, Channel: c.Parent, Author: c.Owner, Kind: convThread, Decision: decision,
		Features: map[string]any{"minutes": int(s.Clock.Now().Sub(c.LastAt).Minutes())}})
	return nil
}

// closeIdleThreads ends the bot's threads that have been quiet for the channel's thread_idle_close_min.
func (s *Service) closeIdleThreads(ctx context.Context) {
	t := s.threadsLoaded()
	now := s.Clock.Now()
	var idle, pending []string
	t.mu.Lock()
	for id, info := range t.threads {
		if !info.Open && now.Sub(info.LastAt) > threadKeepClosed || !info.BotOwned && info.Open && now.Sub(info.LastAt) > threadKeepHumanOpen {
			// what a restart would not load either; the next lookup reads the row back (recoverThread)
			delete(t.threads, id)
			continue
		}
		if !info.BotOwned || !info.Open {
			continue
		}
		pol := s.channelPolicyLocked(info.Guild, info.Parent)
		if now.Sub(info.LastAt) >= pol.Conversation.ThreadIdle() {
			idle = append(idle, id)
		}
	}
	for id := range t.deleted {
		pending = append(pending, id)
	}
	t.trimKnown(now)
	t.mu.Unlock()
	for _, id := range pending {
		if ctx.Err() != nil {
			return
		}
		s.persistThreadDeletion(id)
	}
	for _, id := range idle {
		if ctx.Err() != nil {
			return
		}
		_ = s.CloseThread(ctx, id, "idle")
	}
}

// trimKnown brings the thread table down to threadMaxKnown by letting go of the quietest threads that
// are not the bot's own open ones (the lock held). Their rows stay.
func (t *threadState) trimKnown(now time.Time) {
	over := len(t.threads) - threadMaxKnown
	if over <= 0 {
		return
	}
	type quiet struct {
		id string
		at time.Time
	}
	var drop []quiet
	for id, info := range t.threads {
		if !(info.BotOwned && info.Open) {
			drop = append(drop, quiet{id, info.LastAt})
		}
	}
	sort.Slice(drop, func(i, j int) bool { return drop[i].at.Before(drop[j].at) })
	for i := 0; i < over && i < len(drop); i++ {
		delete(t.threads, drop[i].id)
	}
}

// ThreadArchived is the gateway's word that a thread was archived or unarchived, by a person, Discord's
// auto-archive or the bot. An archived thread is closed here (a human thread then leaves memory with the
// other closed ones); one a person unarchives is open again, unless it is the bot's own and the bot closed
// it.
func (s *Service) ThreadArchived(threadID string, archived bool) {
	if threadID == "" {
		return
	}
	t := s.threadsLoaded()
	if hook := threadTestHooks.beforeArchiveDB.Load(); hook != nil {
		(*hook)(threadID)
	}
	db, err := s.adminDB()
	if err != nil {
		return
	}
	if hook := threadTestHooks.beforeArchiveWrite.Load(); hook != nil {
		(*hook)(threadID)
	}
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	t.mu.Lock()
	deleted := t.deleted[threadID]
	t.mu.Unlock()
	if deleted {
		return
	}
	now := s.Clock.Now()
	at := now.UTC().Format(time.RFC3339)
	var result sql.Result
	// Lifecycle events change the durable row first. Eviction between event receipt and this write
	// cannot turn the event into a no-op; readers cannot publish their previous state afterwards.
	if archived {
		result, err = db.DB.Exec(`UPDATE channel_threads SET state='closed',owner_id='',closed_at=?,close_reason='archived'
			WHERE thread_id=? AND state='open' AND `+threadVisibleSQL("channel_threads"), at, threadID)
	} else {
		result, err = db.DB.Exec(`UPDATE channel_threads SET state='open',last_at=?,closed_at='',close_reason=''
			WHERE thread_id=? AND bot_owned=0 AND state<>'open' AND `+threadVisibleSQL("channel_threads"), at, threadID)
	}
	if err != nil {
		s.Log.Debug("thread lifecycle write failed", "err", err.Error())
		return
	}
	n, err := result.RowsAffected()
	if err != nil || n == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.invalidateRead(threadID)
	t.clearNotThread(threadID)
	if info := t.threads[threadID]; info != nil {
		info.Open = !archived
		if archived {
			info.Owner = ""
		} else {
			info.LastAt = now
		}
	}
}

// ThreadDeleted is the gateway's word that a thread no longer exists: it leaves memory at once and its
// row is closed, even when the thread had already left memory (the row is pruned with the turns).
func (s *Service) ThreadDeleted(threadID string) {
	if threadID == "" {
		return
	}
	t := s.threadsLoaded()
	t.mu.Lock()
	t.invalidateRead(threadID)
	t.clearNotThread(threadID)
	if t.deleted == nil {
		t.deleted = map[string]bool{}
	}
	t.deleted[threadID] = true
	delete(t.threads, threadID)
	t.mu.Unlock()
	s.persistThreadDeletion(threadID)
}

// Failed deletions remain in deleted and are retried on the next minute tick.
func (s *Service) persistThreadDeletion(threadID string) {
	t := &s.threads
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	t.mu.Lock()
	pending := t.deleted[threadID]
	t.mu.Unlock()
	if !pending {
		return
	}
	err := func() error {
		db, err := s.adminDB()
		if err != nil {
			return err
		}
		tx, err := db.DB.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err := tx.Exec(`UPDATE channel_threads SET state='closed',owner_id='',closed_at=?,close_reason='deleted'
			WHERE thread_id=? AND close_reason<>'deleted'`, s.Clock.Now().UTC().Format(time.RFC3339), threadID); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO thread_tombstones(thread_id) VALUES(?)`, threadID); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM thread_participants WHERE thread_id=?`, threadID); err != nil {
			return err
		}
		return tx.Commit()
	}()
	if err != nil {
		// The tick retries every pending deletion each minute; while the database stays unwritable that
		// would be one warning per thread per minute, so warn once per threadDeleteWarnEvery.
		t.mu.Lock()
		now := s.Clock.Now()
		warn := t.deleteWarnAt.IsZero() || now.Sub(t.deleteWarnAt) >= threadDeleteWarnEvery
		if warn {
			t.deleteWarnAt = now
		}
		t.mu.Unlock()
		if warn {
			s.Log.Warn("thread deletion write failed; retrying on next tick", "err", err.Error())
		} else {
			s.Log.Debug("thread deletion write failed again", "err", err.Error())
		}
		return
	}
	t.mu.Lock()
	t.invalidateRead(threadID)
	delete(t.threads, threadID)
	delete(t.deleted, threadID)
	t.mu.Unlock()
}

// channelPolicyLocked is channelPolicy for a parent channel while the thread lock is held (a parent is
// never a thread, so it does not look the thread table up again).
func (s *Service) channelPolicyLocked(guildID, channelID string) config.ChannelPolicy {
	return s.ownChannelPolicy(guildID, channelID)
}

// forgetThreadOwner drops a person from the thread records (consent withdrawn or forgotten).
func (s *Service) forgetThreadOwner(userID string) {
	t := s.threadsLoaded()
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	t.mu.Lock()
	t.forgetOwner(userID)
	t.mu.Unlock()
	if db, err := s.adminDB(); err == nil {
		_, _ = db.DB.Exec(`UPDATE channel_threads SET owner_id='' WHERE owner_id=?`, userID)
		_, _ = db.DB.Exec(`DELETE FROM thread_participants WHERE user_id=?`, userID)
	}
	// A recovery can start while the writer is busy and still read its previous snapshot.
	t.mu.Lock()
	t.forgetOwner(userID)
	t.mu.Unlock()
}

// forgetOwner clears cached copies and active snapshots, with mu held.
func (t *threadState) forgetOwner(userID string) {
	// Evicted threads can still be in a reader's snapshot. Invalidate every active read before clearing
	// ownership, under the same lock used by its final publication; no user IDs need to stay in memory.
	t.scopeGen++
	for _, r := range t.reads {
		r.gen++
	}
	for _, info := range t.threads {
		if info.Owner == userID {
			info.Owner = ""
		}
		delete(info.participants, userID)
		delete(info.humans, userID)
	}
}

// ThreadStopLabel and ThreadNotYours are the channel voice's words for the thread buttons.
func (s *Service) ThreadStopLabel(guildID, channelID string) string {
	return s.voiceFor(guildID, s.channelPolicy(guildID, channelID)).Pack.Thread.Stop
}

func (s *Service) ThreadNotYours(guildID, channelID string) string {
	return s.voiceFor(guildID, s.channelPolicy(guildID, channelID)).Pack.Thread.NotYours
}

// MoreLabel is the channel voice's word for the button under a trimmed reply.
func (s *Service) MoreLabel(guildID, channelID string) string {
	return s.voiceFor(guildID, s.channelPolicy(guildID, channelID)).Pack.Shape.More
}
