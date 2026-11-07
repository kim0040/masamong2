package application

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"masamong/internal/action"
	"masamong/internal/chat"
	"masamong/internal/config"
	"masamong/internal/errcode"
	"masamong/internal/features/participation"
	"masamong/internal/features/privacy"
	"masamong/internal/features/reminders"
	"masamong/internal/lang"
	"masamong/internal/memory/store"
)

// participation.mode and the participation engine (12, masamong-v2-design/09): the bot joining a
// conversation nobody asked it into. It is off unless both the instance (participation.enabled and
// participation.mode) and the channel (guilds.<g>.channels.<c>.participation) allow it; the stricter
// wins. Modes: collect records moments with no model call (see below), shadow decides and records what it
// would say, conservative says it. They are ordered collect < shadow < conservative, so a stricter side
// always lowers the other.
//
// collect (experimental) is for gathering data before any channel lets the bot speak: the same code
// filters pick the moments (an unanswered question or a dated plan after the quiet wait, the bot's name
// said without calling it), and each is logged as state "collected" with its features; a moment people
// handled meanwhile is logged "cancelled" with the reason. Nothing is judged, drafted or said, so the
// cooldown and judge budgets do not apply. The rows are tied to their source message like the others.
// A collected row holds no message text: the original is referenced by its message ID only, together with
// structural features (how busy the room was, the hour, the language, the time since the bot last spoke)
// and, once, what happened next (features.next: called_bot, answered, continued or silent). Collect
// records only channels with capture on and people who agreed (participation and memory consent, not
// blocked, not paused); quiet hours do not apply because nothing is said. Collected rows are deleted with
// their source message, when the person is forgotten or withdraws participation consent.
//
// Every message goes through code filters only: a question nobody else answers within wait_sec, or a
// plan with a date or time, becomes a candidate (at most one per channel). When the wait ends with
// nobody else speaking, one bounded model judgement decides; most of the time it says silence. A
// "speak" goes through the normal reply path (memory, tools, the channel's persona and rules) with
// extra rules to be short, and is posted only if nothing new was said in the meantime (final check).
// A plan becomes an offer card: each person who presses "remind me" gets a reminder of their own —
// one person's press never sets a reminder for anybody else.
//
// Hard filters: the bot's own and other bots' messages, messages to the bot (the direct path answers
// those) or to someone else, blocked people, people who withdrew participation consent, paused
// channels, quiet hours, cooldown and hourly/daily limits, a candidate already waiting, and the judge
// budget (participation.judge_per_hour). Every candidate's outcome is logged (participation_log) for
// the operator to review in the console before moving a channel from shadow to conservative. Only the
// shadow and spoken labels feed that decision; labels on collected moments are counted apart.

func init() {
	featureDDL = append(featureDDL, `CREATE TABLE IF NOT EXISTS participation_log (
		id TEXT PRIMARY KEY,
		guild_id TEXT NOT NULL,
		channel_id TEXT NOT NULL,
		message_id TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		mode TEXT NOT NULL DEFAULT '',
		kind TEXT NOT NULL DEFAULT '',
		state TEXT NOT NULL,
		reason TEXT NOT NULL DEFAULT '',
		score REAL NOT NULL DEFAULT 0,
		message TEXT NOT NULL DEFAULT '',
		draft TEXT NOT NULL DEFAULT '',
		features TEXT NOT NULL DEFAULT '{}',
		verdict TEXT NOT NULL DEFAULT '',
		cost_usd REAL NOT NULL DEFAULT 0
	)`, `CREATE INDEX IF NOT EXISTS idx_participation_log_channel ON participation_log(guild_id, channel_id, created_at)`,
		`CREATE TABLE IF NOT EXISTS participation_spoke_turns (
			turn_id TEXT PRIMARY KEY,
			guild_id TEXT NOT NULL,
			channel_id TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_participation_spoke_turns_created ON participation_spoke_turns(created_at)`)
}

// Outcomes of a candidate.
const (
	partSilent    = "silent"    // the judge (or the reply) said nothing is worth saying
	partShadow    = "shadow"    // it would have spoken (shadow mode)
	partSpoken    = "spoken"    // it spoke (conservative mode)
	partCancelled = "cancelled" // people answered or the conversation moved on
	partCollected = "collected" // a candidate moment recorded without any model call (collect mode)
	partError     = "error"
)

// ObservedMessage is one message the gateway saw in a channel.
type ObservedMessage struct {
	GuildID, ChannelID, MessageID string
	AuthorID, AuthorName, Content string
	At                            time.Time
	FromBot                       bool // this bot's own message
	Addressed                     bool // the message calls the bot or is a command: the direct path has it
	ToSomeone                     bool // a reply to, or a mention of, another person
	// ReplyTo is the message this one replies to; Mentions the accounts it mentions (the bot included);
	// MentionsOthers a role or @everyone mention. Attribution reads them (live.go).
	ReplyTo        string
	Mentions       []string
	MentionsOthers bool
	// BotCall is how the message calls the bot: "mention" (a mention of it or a reply to it), "name" (only
	// its name, which may be talk about it) or "". Collect mode records the latter and what follows a call.
	BotCall string
}

type partMsg struct {
	id, author, name, text string
	at                     time.Time
	bot                    bool
}

type partCandidate struct {
	key, guild, channel, kind string
	msg                       partMsg
	version                   int
	gen                       uint64 // the channel entry it was made in (partChannel.gen)
	signals                   participation.Signals
	timer                     *time.Timer
	// collect marks a candidate made in collect mode (no budget was checked for it); features are its
	// structural features and next what the same person or others did while it waited (collect rows only).
	collect  bool
	features map[string]any
	next     string
}

// nextWatch follows one collected row until the first of: the bot is called, someone answers the source
// message, the same author speaks again, or the window ends. Kept in memory only (a restart ends it).
type nextWatch struct {
	row, msg, author string
	until            time.Time
}

type nextResult struct {
	row, guild, channel, next string
}

const (
	collectNextWindow = 10 * time.Minute
	collectWatchCap   = 50 // per channel
)

type partChannel struct {
	// gen is unique to this entry for the life of the process: version restarts at 0 in an entry made
	// again after the old one was dropped, so a candidate is current only in the entry it was made in.
	gen      uint64
	version  int
	recent   []partMsg
	spoken   []time.Time // unasked remarks
	judged   []time.Time // model judgements
	pending  *partCandidate
	watches  []nextWatch    // collected rows waiting for what happened next
	filtered map[string]int // candidate-worthy messages the code filters dropped, by reason
	// backing off (participation_backoff.go)
	mutedUntil    time.Time
	lastCall      time.Time
	spokes        []partSpoke
	ignored       map[string]int // remarks in a row nobody took up, by kind
	backoffLoaded bool
	touched       time.Time // last use of this entry (partChannel); idle ones are dropped (sweepPartChannels)
	// inflight counts the candidates being judged or drafted (taken off pending, not finished): the entry
	// they will check and record into must stay.
	inflight atomic.Int32
}

type partEngine struct {
	mu    sync.Mutex
	chans map[string]*partChannel
	gen   uint64 // last partChannel.gen handed out
	// wait overrides the quiet wait (tests)
	wait func(kind string) time.Duration
	// afterFunc overrides quiet-wait timer creation (tests).
	afterFunc func(time.Duration, func()) *time.Timer
	// startWork overrides work launched by Observe (tests); the caller holds mu.
	startWork func(string, func(context.Context))
	// nextWindow overrides how long a collected row waits for what happened next (tests)
	nextWindow time.Duration
	// gone are the messages recently deleted (a bounded memory, its own lock because the log writer reads it
	// inside a database transaction): a collected row is never written for one of them, even when the
	// deletion's tombstone is not committed yet.
	goneMu    sync.Mutex
	gone      map[string]bool
	goneOrder []string
}

const partGoneCap = 512

const (
	// A channel's participation state (the last 20 messages, spoke/judged times, back-off counters) is kept
	// in memory only; the mute is derived again from the decision log when the entry is made. One idle for
	// a day has nothing left that matters, and the count cap bounds a server that makes many channels or
	// threads. An entry with a candidate waiting, a collected row being followed or a mute running is never
	// dropped, so no timer, privacy intent or follow-up is lost.
	partIdleEvict   = 24 * time.Hour
	partMaxChannels = 512
)

// partForget is called first when messages are deleted: it drops the candidates and the follow-ups of
// collect mode that wait on them, and remembers the IDs so a late log write skips them.
func (s *Service) partForget(guildID, channelID string, messageIDs []string) {
	ids := map[string]bool{}
	s.part.goneMu.Lock()
	if s.part.gone == nil {
		s.part.gone = map[string]bool{}
	}
	for _, id := range messageIDs {
		if id == "" {
			continue
		}
		ids[id] = true
		if !s.part.gone[id] {
			s.part.gone[id] = true
			s.part.goneOrder = append(s.part.goneOrder, id)
		}
	}
	for len(s.part.goneOrder) > partGoneCap {
		delete(s.part.gone, s.part.goneOrder[0])
		s.part.goneOrder = s.part.goneOrder[1:]
	}
	s.part.goneMu.Unlock()
	if len(ids) == 0 {
		return
	}
	s.part.mu.Lock()
	defer s.part.mu.Unlock()
	for key, c := range s.part.chans {
		if channelID != "" && key != guildID+":"+channelID {
			continue
		}
		if p := c.pending; p != nil && ids[p.msg.id] {
			p.timer.Stop()
			c.pending = nil
		}
		kept := c.watches[:0]
		for _, w := range c.watches {
			if !ids[w.msg] {
				kept = append(kept, w)
			}
		}
		c.watches = kept
	}
}

func (s *Service) partGone(messageID string) bool {
	if messageID == "" {
		return false
	}
	s.part.goneMu.Lock()
	defer s.part.goneMu.Unlock()
	return s.part.gone[messageID]
}

func (s *Service) partChannel(key string) *partChannel {
	if s.part.chans == nil {
		s.part.chans = map[string]*partChannel{}
	}
	c := s.part.chans[key]
	if c == nil {
		s.part.gen++
		c = &partChannel{gen: s.part.gen, filtered: map[string]int{}}
		s.part.chans[key] = c
	}
	c.touched = s.Clock.Now()
	if !c.backoffLoaded && s.Store != nil {
		guildID, channelID, _ := strings.Cut(key, ":")
		c.backoffLoaded = s.restorePartMute(guildID, channelID, c)
	}
	return c
}

// participationMode is the effective mode of a channel: the stricter of the instance and the channel.
func (s *Service) participationModeOf(guildID, channelID string) string {
	doc := s.Doc()
	if !doc.Participation.Enabled {
		return "off"
	}
	ch := s.channelPolicy(guildID, channelID).Participation
	if config.ParticipationRank(ch) < config.ParticipationRank(doc.Participation.Mode) {
		return config.ParticipationModes[config.ParticipationRank(ch)]
	}
	return doc.Participation.Mode
}

// Observe is what the gateway calls for every message of a channel it reads.
func (s *Service) Observe(m ObservedMessage) {
	_, done, ok := s.beginWork(context.Background())
	if !ok {
		return
	}
	defer done()
	if m.ChannelID == "" {
		return
	}
	s.countActivity(m)
	if m.At.IsZero() {
		m.At = s.Clock.Now()
	}
	s.noteLive(m)
	s.noteThreadActivity(m)
	s.implicitObserve(m)
	key := m.GuildID + ":" + m.ChannelID
	// the author's consent is read before the lock is taken, and only when a collected row waits for it
	var counted *bool
	if !m.FromBot && s.watching(key) {
		ok := s.authorCounted(m)
		counted = &ok
	}
	s.part.mu.Lock()
	defer s.part.mu.Unlock()
	startWork := s.ownedGo
	if s.part.startWork != nil {
		startWork = s.part.startWork
	}
	c := s.partChannel(key)
	c.version++
	c.recent = append(c.recent, partMsg{id: m.MessageID, author: m.AuthorID, name: m.AuthorName, text: clip(m.Content, 400), at: m.At, bot: m.FromBot})
	if len(c.recent) > 20 {
		c.recent = c.recent[len(c.recent)-20:]
	}
	if mode := s.participationModeOf(m.GuildID, m.ChannelID); mode == "shadow" || mode == "conservative" {
		pack := s.voiceFor(m.GuildID, s.channelPolicy(m.GuildID, m.ChannelID)).Pack
		if react, logged := s.backoffObserve(c, m, pack); logged != "" {
			startWork("participation back-off", func(_ context.Context) { s.backoffAfter(m, react, logged) })
		}
	}
	// what happened after the collected moments of this channel
	if done := s.advanceWatches(c, m, counted); len(done) > 0 {
		startWork("participation next", func(_ context.Context) { s.writeNext(done) })
	}
	// someone else spoke while a candidate waited: people are handling it
	if p := c.pending; p != nil && (m.FromBot || m.AuthorID != p.msg.author) {
		p.timer.Stop()
		c.pending = nil
		reason := "people_answered"
		if m.FromBot {
			reason = "bot_answered"
		} else if p.collect {
			p.next = "answered"
			if m.BotCall == "mention" {
				p.next = "called_bot"
			}
		}
		startWork("participation cancellation", func(_ context.Context) {
			s.logParticipation(p, s.participationModeOf(p.guild, p.channel), partCancelled, reason, 0, "", 0)
		})
	} else if p != nil && p.collect && p.next == "" {
		// the same person went on while their moment waited
		p.next = "continued"
		if m.BotCall == "mention" {
			p.next = "called_bot"
		}
	}
	if m.FromBot {
		return
	}
	mode := s.participationModeOf(m.GuildID, m.ChannelID)
	if mode == "off" {
		return
	}
	// the bot's name said without calling it, where no name-call check runs (name_call off, or a room that
	// answers everything); with name_call addressed the check itself records it (nameCallGate)
	if mode == "collect" && m.BotCall == "name" {
		if pol := s.channelPolicy(m.GuildID, m.ChannelID); !(pol.Respond == "mention" && pol.NameCall == "addressed") {
			if why := s.collectSkip(m.GuildID, m.ChannelID, m.AuthorID); why != "" {
				c.filtered[why]++
			} else {
				cand := &partCandidate{key: key, guild: m.GuildID, channel: m.ChannelID, kind: "bot_mentioned", collect: true,
					msg:      partMsg{id: m.MessageID, author: m.AuthorID, name: m.AuthorName, text: m.Content, at: m.At},
					features: s.momentFeatures(m.GuildID, m.ChannelID, m.At, m.Content)}
				startWork("participation mention", func(_ context.Context) {
					s.logParticipation(cand, "collect", partCollected, "bot_mentioned", 0, "", 0)
				})
			}
		}
	}
	if m.Addressed || m.ToSomeone {
		return
	}
	doc := s.Doc()
	loc := s.instanceZone()
	pack := lang.For(s.voiceFor(m.GuildID, s.channelPolicy(m.GuildID, m.ChannelID)).Pack.Tag, doc.I18n.DefaultLocale)
	sig := participation.Read(m.Content, m.At, loc, pack.Time)
	kind := sig.Kind()
	if kind == "" {
		return
	}
	// the code filters: counted, not logged (a busy channel would flood the log)
	now := s.Clock.Now()
	reason := ""
	cfg := doc.Participation
	switch {
	case c.pending != nil:
		reason = "candidate_waiting"
	case s.BlockedIn(m.GuildID, m.AuthorID):
		// a server's own block list counts as much as the instance's
		reason = "blocked"
	case !s.ConsentAllows(m.AuthorID, privacy.Participation):
		reason = "opted_out"
	case s.Paused(m.GuildID, m.ChannelID):
		reason = "paused"
	case mode == "collect" && !s.captureOn(m.GuildID, m.ChannelID):
		reason = "capture_off" // collect only records where the channel's memory capture is on
	case mode == "collect" && !s.MemoryAllowed(m.AuthorID):
		reason = "opted_out"
	case mode != "collect" && participation.Quiet(now.In(loc), cfg.QuietStart, cfg.QuietEnd):
		reason = "quiet_hours" // collect says nothing, so quiet hours do not apply to it
	case mode != "collect" && backoffBlocked(c, now) != "":
		reason = backoffBlocked(c, now)
	}
	collect := mode == "collect" // no judge and nothing said: the speaking and judge budgets do not apply
	if reason == "" && !collect {
		if ok, why := (participation.Budget{Cooldown: cfg.Cooldown(), PerHour: cfg.Hourly(), PerDay: cfg.Daily()}).Allows(c.spoken, now); !ok {
			reason = why
		}
	}
	if reason == "" && !collect {
		if ok, _ := (participation.Budget{PerHour: cfg.Judges()}).Allows(c.judged, now); !ok {
			reason = "judge_budget"
		}
	}
	if reason != "" {
		c.filtered[reason]++
		return
	}
	cand := &partCandidate{key: key, guild: m.GuildID, channel: m.ChannelID, kind: kind, version: c.version, gen: c.gen, signals: sig,
		msg: partMsg{id: m.MessageID, author: m.AuthorID, name: m.AuthorName, text: clip(m.Content, 400), at: m.At}}
	if collect {
		cand.collect, cand.features = true, s.momentFeatures(m.GuildID, m.ChannelID, m.At, m.Content)
	}
	wait := cfg.Wait()
	if kind == participation.Plan {
		wait = 3 * time.Second // a plan is not waiting for an answer, only for the next line or two
	}
	if s.part.wait != nil {
		wait = s.part.wait(kind)
	}
	afterFunc := time.AfterFunc
	if s.part.afterFunc != nil {
		afterFunc = s.part.afterFunc
	}
	cand.timer = afterFunc(wait, func() { s.considerCandidate(cand) })
	c.pending = cand
}

// judgeReply is the judge's JSON answer.
type judgeReply struct {
	Speak  bool    `json:"speak"`
	Kind   string  `json:"kind"`
	Score  float64 `json:"score"`
	Reason string  `json:"reason"`
	When   string  `json:"when"`
	What   string  `json:"what"`
}

func parseJudge(text string) (judgeReply, error) {
	var j judgeReply
	i, k := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if i < 0 || k <= i {
		return j, errors.New("the judge did not answer in JSON")
	}
	err := json.Unmarshal([]byte(text[i:k+1]), &j)
	return j, err
}

// considerCandidate runs when a candidate's quiet wait ends.
func (s *Service) considerCandidate(cand *partCandidate) {
	ctx, done, ok := s.beginWork(context.Background())
	if !ok {
		return
	}
	defer done()
	s.part.mu.Lock()
	c := s.partChannel(cand.key)
	if c.pending != cand {
		s.part.mu.Unlock()
		return
	}
	c.pending = nil
	// from here until it ends the candidate is not pending but still needs its entry (the final check, the
	// speaking budget and the follow-up marks are written there)
	c.inflight.Add(1)
	defer c.inflight.Add(-1)
	if s.participationModeOf(cand.guild, cand.channel) == "collect" {
		// what the channel and the person allow may have changed during the wait
		if why := s.collectSkip(cand.guild, cand.channel, cand.msg.author); why != "" {
			c.filtered[why]++
			s.part.mu.Unlock()
			return
		}
		s.part.mu.Unlock()
		// nobody else spoke during the wait: the moment is kept as it was, with no model call
		reason := "unanswered"
		if cand.kind == participation.Plan {
			reason = "plan"
		}
		s.logParticipation(cand, "collect", partCollected, reason, 0, "", 0)
		return
	}
	if cand.collect {
		// made in collect mode, which checked no budget: the mode changed during the wait, so the limits
		// that speaking and judging answer to apply now
		if why := s.candidateBlocked(c, s.Clock.Now()); why != "" {
			s.part.mu.Unlock()
			if mode := s.participationModeOf(cand.guild, cand.channel); mode != "off" {
				s.logParticipation(cand, mode, partCancelled, why, 0, "", 0)
			}
			return
		}
	}
	if why := backoffBlocked(c, s.Clock.Now()); why != "" {
		// muted or just called while the candidate waited
		s.part.mu.Unlock()
		return
	}
	bump := s.ignoredBump(c, cand.kind, s.Clock.Now())
	c.judged = append(c.judged, s.Clock.Now())
	if len(c.judged) > 50 {
		c.judged = c.judged[len(c.judged)-50:]
	}
	var lines []string
	for _, m := range c.recent {
		name := m.name
		if m.bot {
			name = s.Doc().BotNames()[0]
		}
		lines = append(lines, name+": "+m.text)
	}
	s.part.mu.Unlock()

	mode := s.participationModeOf(cand.guild, cand.channel)
	if mode == "off" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	pol := s.channelPolicy(cand.guild, cand.channel)
	pack := s.voiceFor(cand.guild, pol).Pack
	loc := s.instanceZone()
	bot := s.Doc().BotNames()[0]
	prompt := lang.Fill(pack.Features.ParticipationJudge, "conversation", strings.Join(lines, "\n"), "speaker", cand.msg.name,
		"message", cand.msg.text, "kind", cand.kind, "now", s.Clock.Now().In(loc).Format("2006-01-02 15:04 Mon"))
	text, cost, err := s.featureText(ctx, "planner", "participation", lang.Fill(pack.Features.ParticipationJudgeSystem, "bot", bot), prompt, 250, 0)
	if err != nil {
		s.logParticipation(cand, mode, partError, clip(err.Error(), 200), 0, "", cost)
		return
	}
	j, err := parseJudge(text)
	if err != nil {
		s.logParticipation(cand, mode, partError, err.Error(), 0, "", cost)
		return
	}
	if !j.Speak || j.Kind == "none" || j.Score < s.Doc().Participation.Threshold()+bump {
		s.logParticipation(cand, mode, partSilent, clip(j.Reason, 200), j.Score, "", cost)
		return
	}
	if j.Kind == "event" || (j.Kind == "" && cand.kind == participation.Plan) {
		s.offerPlan(ctx, cand, mode, j, cost, pack, loc)
		return
	}
	// what it would say, through the normal reply path; a lab turn in shadow mode (nothing stored). In
	// conservative mode the reply is posted inside the turn, after the final check, so a draft the
	// conversation moved past or Discord refused is not kept as something the bot said.
	draft, posted, postedID, postedTurn := "", false, "", ""
	var postErr error
	if mode == "conservative" || s.Doc().Participation.Drafts() {
		in := ChatInput{GuildID: cand.guild, ChannelID: cand.channel, MessageID: cand.msg.id, Message: cand.msg.text,
			Speaker: cand.msg.name, SpeakerID: cand.msg.author, DryRun: mode != "conservative", Proactive: true}
		if mode == "conservative" {
			in.Deliver = func(ctx context.Context, res chat.Result) string {
				text := strings.TrimSpace(res.Answer)
				if text == "" || !s.stillCurrent(cand) {
					return ""
				}
				poster, ok := s.Discord.(ChannelPoster)
				if !ok || s.Discord == nil || !s.Discord.Connected() {
					postErr = errors.New("discord not connected")
					return ""
				}
				id, err := poster.PostChannel(ctx, cand.channel, cand.msg.id, text, nil)
				if err != nil {
					postErr = err
					return ""
				}
				s.NoteReplyMessages(res.TurnID, cand.guild, cand.channel, []string{id})
				posted, postedID, postedTurn = true, id, res.TurnID
				return text
			}
		}
		res, err := s.ChatTurn(ctx, in)
		cost += res.CostUSD
		if err != nil {
			s.logParticipation(cand, mode, partError, clip(err.Error(), 200), j.Score, "", cost)
			return
		}
		draft = strings.TrimSpace(res.Answer)
		if draft == "" {
			s.logParticipation(cand, mode, partSilent, "empty_reply", j.Score, "", cost)
			return
		}
	}
	switch {
	case posted:
		s.noteSpoken(cand)
		s.noteSpoke(cand, postedID, postedTurn)
		s.logParticipation(cand, mode, partSpoken, clip(j.Reason, 200), j.Score, draft, cost)
	case postErr != nil:
		s.logParticipation(cand, mode, partError, clip(postErr.Error(), 200), j.Score, draft, cost)
	case !s.stillCurrent(cand) || mode == "conservative":
		s.logParticipation(cand, mode, partCancelled, "conversation_moved", j.Score, draft, cost)
	default:
		s.logParticipation(cand, mode, partShadow, clip(j.Reason, 200), j.Score, draft, cost)
	}
}

// offerPlan turns a plan the judge found into an offer anyone in the channel can take up.
func (s *Service) offerPlan(ctx context.Context, cand *partCandidate, mode string, j judgeReply, cost float64, pack *lang.Pack, loc *time.Location) {
	when := strings.TrimSpace(j.When)
	if when == "" {
		when = cand.msg.text
	}
	w, err := reminders.Parse(when, cand.msg.at, loc, pack.Time, reminders.Options{DefaultTime: s.toolString("reminders.propose", "default_time", "09:00")})
	if err != nil {
		w, err = reminders.Parse(cand.msg.text, cand.msg.at, loc, pack.Time, reminders.Options{DefaultTime: s.toolString("reminders.propose", "default_time", "09:00")})
	}
	if err != nil || len(futureOnly(candTimes(w), s.Clock.Now())) == 0 {
		s.logParticipation(cand, mode, partSilent, "plan_time_unclear", j.Score, "", cost)
		return
	}
	what := strings.TrimSpace(j.What)
	if what == "" {
		what = w.Rest
	}
	req := action.Request{Locale: pack.Tag}
	card := action.Card{Title: s.msg(req, "reminders.offer_title"), Text: clip(what, 300), Tone: "neutral",
		Fields: []action.CardField{{Name: s.msg(req, "reminders.field.when"), Value: s.whenText(req.Locale, w.Candidates[0].At, loc), Inline: true}},
		Footer: s.msg(req, "reminders.offer_footer")}
	if len(w.Candidates) > 1 {
		card.Fields[0].Value = s.whenText(req.Locale, w.Candidates[0].At, loc) + " / " + s.whenText(req.Locale, w.Candidates[1].At, loc)
	}
	draft := card.Title + " — " + card.Text + " (" + card.Fields[0].Value + ")"
	if !s.stillCurrent(cand) {
		s.logParticipation(cand, mode, partCancelled, "conversation_moved", j.Score, draft, cost)
		return
	}
	if mode != "conservative" {
		s.logParticipation(cand, mode, partShadow, clip(j.Reason, 200), j.Score, draft, cost)
		return
	}
	poster, ok := s.Discord.(ChannelPoster)
	if !ok || s.Discord == nil || !s.Discord.Connected() {
		s.logParticipation(cand, mode, partError, "discord not connected", j.Score, draft, cost)
		return
	}
	offerID, err := s.saveReminderOffer(cand.guild, cand.channel, cand.msg.id, what, w.Matched, loc, w)
	if err != nil {
		s.logParticipation(cand, mode, partError, clip(err.Error(), 200), j.Score, draft, cost)
		return
	}
	card.Buttons = []action.CardButton{{ID: "remind:join:" + offerID, Label: s.msg(req, "reminders.button.join"), Style: "primary"}}
	if _, err := poster.PostChannel(ctx, cand.channel, cand.msg.id, "", &card); err != nil {
		s.logParticipation(cand, mode, partError, clip(err.Error(), 200), j.Score, draft, cost)
		return
	}
	s.noteSpoken(cand)
	s.logParticipation(cand, mode, partSpoken, clip(j.Reason, 200), j.Score, draft, cost)
}

func candTimes(w reminders.When) []time.Time {
	var out []time.Time
	for _, c := range w.Candidates {
		out = append(out, c.At)
	}
	return out
}

// stillCurrent is the final check: nothing was said in the channel since the candidate, and the channel's
// entry is the one the candidate was made in (a dropped and made-again entry counts versions anew).
func (s *Service) stillCurrent(cand *partCandidate) bool {
	s.part.mu.Lock()
	defer s.part.mu.Unlock()
	c := s.partChannel(cand.key)
	return c.gen == cand.gen && c.version == cand.version
}

func (s *Service) noteSpoken(cand *partCandidate) {
	s.part.mu.Lock()
	defer s.part.mu.Unlock()
	c := s.partChannel(cand.key)
	c.spoken = append(c.spoken, s.Clock.Now())
	if len(c.spoken) > 50 {
		c.spoken = c.spoken[len(c.spoken)-50:]
	}
}

// candidateBlocked is why a candidate may not be judged or said right now ("" = it may): quiet hours, the
// speaking limits and the judge budget. The caller holds s.part.mu.
func (s *Service) candidateBlocked(c *partChannel, now time.Time) string {
	cfg := s.Doc().Participation
	if participation.Quiet(now.In(s.instanceZone()), cfg.QuietStart, cfg.QuietEnd) {
		return "quiet_hours"
	}
	if why := backoffBlocked(c, now); why != "" {
		return why
	}
	if ok, why := (participation.Budget{Cooldown: cfg.Cooldown(), PerHour: cfg.Hourly(), PerDay: cfg.Daily()}).Allows(c.spoken, now); !ok {
		return why
	}
	if ok, _ := (participation.Budget{PerHour: cfg.Judges()}).Allows(c.judged, now); !ok {
		return "judge_budget"
	}
	return ""
}

// captureOn is whether the channel's messages are captured (the instance and the channel both allow it).
func (s *Service) captureOn(guildID, channelID string) bool {
	return s.Doc().Memory.Capture && s.channelPolicy(guildID, channelID).Capture
}

// collectSkip is why collect mode records nothing for this person here ("" = it may): a blocked person, one
// who withdrew participation or memory consent, a paused channel, a channel with capture off. Quiet hours do
// not count: collect says nothing.
func (s *Service) collectSkip(guildID, channelID, authorID string) string {
	switch {
	case s.BlockedIn(guildID, authorID):
		return "blocked"
	case !s.ConsentAllows(authorID, privacy.Participation) || !s.MemoryAllowed(authorID):
		return "opted_out"
	case s.Paused(guildID, channelID):
		return "paused"
	case !s.captureOn(guildID, channelID):
		return "capture_off"
	}
	return ""
}

// momentFeatures are the structural features of a collected moment, none of them a word of the
// conversation: people and messages in the last 10 minutes, the local hour, the message's language, and the
// minutes since the bot last spoke in the channel (left out when it has not within the live window).
func (s *Service) momentFeatures(guildID, channelID string, at time.Time, text string) map[string]any {
	people := map[string]bool{}
	msgs := 0
	var lastBot time.Time
	for _, l := range s.liveLines(guildID, channelID) {
		if l.At.After(at) {
			continue
		}
		if l.Bot {
			if l.At.After(lastBot) {
				lastBot = l.At
			}
			continue
		}
		if at.Sub(l.At) <= 10*time.Minute {
			people[l.AuthorID] = true
			msgs++
		}
	}
	f := map[string]any{"participants": len(people), "msgs_per_min": float64(msgs) / 10, "hour": at.In(s.instanceZone()).Hour()}
	if tag := chat.DetectLanguage(text); tag != "" {
		f["lang"] = tag
	}
	if !lastBot.IsZero() {
		f["since_bot_min"] = int(at.Sub(lastBot).Minutes())
	}
	return f
}

// collectMention records, in collect mode, that the bot's name was said without calling it, when the
// name-call check ran and judged it not to be a call. (Where no check runs, Observe records it.) It makes no
// model call, and only for channels with capture on and people who agreed.
func (s *Service) collectMention(in ChatInput) {
	if in.DryRun || in.ChannelID == "" || s.participationModeOf(in.GuildID, in.ChannelID) != "collect" {
		return
	}
	if why := s.collectSkip(in.GuildID, in.ChannelID, in.SpeakerID); why != "" {
		s.part.mu.Lock()
		s.partChannel(in.GuildID + ":" + in.ChannelID).filtered[why]++
		s.part.mu.Unlock()
		return
	}
	now := s.Clock.Now()
	cand := &partCandidate{guild: in.GuildID, channel: in.ChannelID, kind: "bot_mentioned", collect: true,
		msg:      partMsg{id: in.MessageID, author: in.SpeakerID, name: in.Speaker, text: in.Message, at: now},
		features: s.momentFeatures(in.GuildID, in.ChannelID, now, in.Message)}
	s.logParticipation(cand, "collect", partCollected, "bot_mentioned", 0, "", 0)
}

// advanceWatches settles the collected rows of a channel that this message answers: the bot called, the
// source message replied to by someone else, or the same author speaking again. Watches past their window
// end as silent. It returns the rows to write (the caller holds s.part.mu and writes them after).
//
// Whether the author may be counted is the same for every watch, so it is asked once (the caller passes the
// answer when it read it before taking the lock; nil asks here).
func (s *Service) advanceWatches(c *partChannel, m ObservedMessage, counted *bool) []nextResult {
	if len(c.watches) == 0 || m.FromBot {
		return nil
	}
	now := s.Clock.Now()
	var out []nextResult
	kept := c.watches[:0]
	for _, w := range c.watches {
		next := ""
		switch {
		case !now.Before(w.until):
			next = "silent"
		case m.MessageID == w.msg:
		case m.BotCall == "mention":
			next = "called_bot"
		case m.ReplyTo == w.msg && m.AuthorID != w.author:
			next = "answered"
		case m.AuthorID == w.author:
			next = "continued"
		}
		// what a blocked person or one who withdrew did next is not recorded
		if next != "" && next != "silent" {
			if counted == nil {
				ok := s.authorCounted(m)
				counted = &ok
			}
			if !*counted {
				next = ""
			}
		}
		if next == "" {
			kept = append(kept, w)
			continue
		}
		out = append(out, nextResult{row: w.row, guild: m.GuildID, channel: m.ChannelID, next: next})
	}
	c.watches = kept
	return out
}

// authorCounted is whether what this message's author did may be recorded as what happened next.
func (s *Service) authorCounted(m ObservedMessage) bool {
	return !s.BlockedIn(m.GuildID, m.AuthorID) && s.ConsentAllows(m.AuthorID, privacy.Participation) && s.MemoryAllowed(m.AuthorID)
}

// watching is whether a channel has collected rows waiting for what happens next.
func (s *Service) watching(key string) bool {
	s.part.mu.Lock()
	defer s.part.mu.Unlock()
	c := s.part.chans[key]
	return c != nil && len(c.watches) > 0
}

// sweepCollectNext ends the watches whose window has passed with "silent". The scheduler calls it every
// minute, so a quiet channel's rows get their answer too.
func (s *Service) sweepCollectNext() {
	now := s.Clock.Now()
	var out []nextResult
	s.part.mu.Lock()
	for key, c := range s.part.chans {
		kept := c.watches[:0]
		guild, channel, _ := strings.Cut(key, ":")
		for _, w := range c.watches {
			if now.Before(w.until) {
				kept = append(kept, w)
				continue
			}
			out = append(out, nextResult{row: w.row, guild: guild, channel: channel, next: "silent"})
		}
		c.watches = kept
	}
	s.sweepPartChannels(now)
	s.part.mu.Unlock()
	s.sweepLive(now) // same minute tick: the other per-channel memory that has no owner of its own
	s.writeNext(out)
}

// sweepPartChannels drops the idle channel entries (the caller holds s.part.mu). An entry that still holds
// something that limits the bot is not dropped, not even over the cap: a channel made again would start
// without its cooldown, budgets and unsettled remarks, so when nothing else can go the table stays over
// partMaxChannels until the limits run out.
func (s *Service) sweepPartChannels(now time.Time) {
	type idle struct {
		key string
		at  time.Time
	}
	var evictable []idle
	window := s.partLimitWindow()
	for key, c := range s.part.chans {
		if c.pending != nil || c.inflight.Load() > 0 || len(c.watches) > 0 || now.Before(c.mutedUntil) || partLimitsLive(c, now, window) {
			continue
		}
		if now.Sub(c.touched) > partIdleEvict {
			delete(s.part.chans, key)
			continue
		}
		evictable = append(evictable, idle{key, c.touched})
	}
	if over := len(s.part.chans) - partMaxChannels; over > 0 {
		sort.Slice(evictable, func(i, j int) bool { return evictable[i].at.Before(evictable[j].at) })
		for i := 0; i < over && i < len(evictable); i++ {
			delete(s.part.chans, evictable[i].key)
		}
	}
}

// partLimitWindow is how long a remark of the bot keeps counting against a channel: the daily budget's day,
// or the cooldown when that is longer.
func (s *Service) partLimitWindow() time.Duration {
	return max(24*time.Hour, s.Doc().Participation.Cooldown())
}

// partLimitsLive reports whether the entry still holds a limit that applies at now: the pause after a call
// to the bot, a remark inside the cooldown or the hourly and daily budgets, a model judgement inside the
// hourly judge budget, or a remark whose reception is not settled yet.
func partLimitsLive(c *partChannel, now time.Time, window time.Duration) bool {
	if !c.lastCall.IsZero() && now.Sub(c.lastCall) < partAfterCall {
		return true
	}
	for _, at := range c.spoken {
		if now.Sub(at) < window {
			return true
		}
	}
	for _, at := range c.judged {
		if now.Sub(at) < time.Hour {
			return true
		}
	}
	for _, sp := range c.spokes {
		if !sp.settled && now.Sub(sp.at) < window {
			return true
		}
	}
	return false
}

// watchNext follows a collected row for what happens next, for a bounded window.
func (s *Service) watchNext(cand *partCandidate, rowID string) {
	window := collectNextWindow
	s.part.mu.Lock()
	defer s.part.mu.Unlock()
	if s.part.nextWindow > 0 {
		window = s.part.nextWindow
	}
	c := s.partChannel(cand.guild + ":" + cand.channel)
	c.watches = append(c.watches, nextWatch{row: rowID, msg: cand.msg.id, author: cand.msg.author, until: s.Clock.Now().Add(window)})
	if len(c.watches) > collectWatchCap {
		c.watches = c.watches[len(c.watches)-collectWatchCap:]
	}
}

// writeNext stores what happened next into the collected rows, once each. A row whose channel left collect
// mode or capture meanwhile is left as it is, and a row already deleted (its source went) is not recreated.
func (s *Service) writeNext(done []nextResult) {
	if len(done) == 0 {
		return
	}
	db, err := s.adminDB()
	if err != nil {
		return
	}
	for _, r := range done {
		if s.participationModeOf(r.guild, r.channel) != "collect" || !s.captureOn(r.guild, r.channel) {
			continue
		}
		if _, err := db.DB.Exec(`UPDATE participation_log SET features=json_set(features,'$.next',?) WHERE id=? AND mode='collect' AND json_extract(features,'$.next') IS NULL`, r.next, r.row); err != nil {
			s.Log.Debug("participation next write failed", "err", err)
		}
	}
}

// logParticipation writes one outcome and returns the row's id ("" when nothing was written). In collect
// mode the row holds no message text and no draft: the source is referenced by message_id alone, and the
// row goes with it.
func (s *Service) logParticipation(cand *partCandidate, mode, state, reason string, score float64, draft string, cost float64) string {
	db, err := s.adminDB()
	if err != nil {
		return ""
	}
	collect := mode == "collect"
	if collect {
		// whatever path got here (the wait ending, a cancellation, a mention), what the person and the
		// channel allow is read again now: consent may have been withdrawn while the moment waited
		if why := s.collectSkip(cand.guild, cand.channel, cand.msg.author); why != "" {
			s.part.mu.Lock()
			s.partChannel(cand.guild + ":" + cand.channel).filtered[why]++
			s.part.mu.Unlock()
			return ""
		}
	}
	skipped := false
	feat := map[string]any{"kind": cand.kind, "signals": cand.signals}
	if collect {
		for k, v := range cand.features {
			feat[k] = v
		}
		if cand.next != "" {
			feat["next"] = cand.next
		}
	}
	features, _ := json.Marshal(feat)
	id := newID("pl_")
	err = db.WithWriter(func(tx *sql.Tx) error {
		blocked, err := store.MessageBlocked(tx, cand.msg.id, cand.msg.author)
		if err != nil {
			return err
		}
		if collect && (blocked || s.partGone(cand.msg.id)) {
			skipped = true // the message was deleted (or its author forgotten) while the moment waited
			return nil
		}
		message := cand.msg.text
		if blocked || collect {
			message, draft = "", ""
		}
		_, err = tx.Exec(`INSERT INTO participation_log(id, guild_id, channel_id, message_id, created_at, mode, kind, state, reason, score, message, draft, features, cost_usd, author_ref,evidence_state)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,1)`, id, cand.guild, cand.channel, cand.msg.id, s.Clock.Now().UTC().Format(time.RFC3339),
			mode, cand.kind, state, reason, score, message, clip(draft, 2000), string(features), cost, personRef(cand.msg.author))
		if err != nil {
			return err
		}
		keys, err := copySources(tx, []string{cand.msg.id, personRef(cand.msg.author)})
		if err != nil {
			return err
		}
		return indexCopy(tx, "participation", id, keys, false)
	})
	if err != nil {
		s.Log.Debug("participation log write failed", "err", err)
		return ""
	}
	if skipped {
		return ""
	}
	if collect && state == partCollected && cand.next == "" {
		s.watchNext(cand, id)
	}
	s.emit("participation", cand.channel, map[string]any{"state": state})
	return id
}

// ---------------------------------------------------------------------------
// participation.mode
// ---------------------------------------------------------------------------

// {guild_id, channel_id}                 the channel's mode, the instance cap, stats and recent log
// {guild_id, channel_id, mode}           set the channel's mode (off | collect | shadow | conservative)
// {review: id, verdict: good|bad|""}     the operator's verdict on a logged candidate; reviewed counts only
//
//	shadow and spoken rows (the gate to conservative), and the labels on
//	collected rows are counted apart as collected_good / collected_bad
//
// {log: true, guild_id?, channel_id?}    the log (console)
func (s *Service) participationMode(req action.Request) action.Result {
	if req.Invoker != nil && req.Invoker.UserID != "" {
		return action.Fail(req, errcode.PermissionDenied, "generic.error", "participation is set in the console", nil)
	}
	in := inputMap(req)
	db, err := s.adminDB()
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	if id := strVal(in, "review", ""); id != "" {
		v := strVal(in, "verdict", "")
		if v != "" && v != "good" && v != "bad" {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "verdict must be good, bad or empty", nil)
		}
		if _, err := db.DB.Exec(`UPDATE participation_log SET verdict=? WHERE id=?`, v, id); err != nil {
			return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
		}
		return action.OK(req, map[string]any{"id": id, "verdict": v})
	}
	g, c := strVal(in, "guild_id", ""), strVal(in, "channel_id", "")
	if mode, ok := in["mode"].(string); ok && c != "" {
		if !stringIn(mode, config.ParticipationModes) {
			return action.Fail(req, errcode.InvalidInput, "generic.error", fmt.Sprintf("mode must be one of %v", config.ParticipationModes), nil)
		}
		res := s.mutateConfig(req, func(d *config.Document) error {
			ensureGuildChannelMaps(d, g)
			p := config.NormalizeChannelPolicy(d.Guilds[g].Channels[c])
			p.Participation = mode
			if mode == "off" {
				p.Participation = ""
			}
			d.Guilds[g].Channels[c] = p
			return nil
		})
		if !res.OK {
			return res
		}
	}
	var where []string
	var args []any
	if g != "" {
		where, args = append(where, "guild_id=?"), append(args, g)
	}
	if c != "" {
		where, args = append(where, "channel_id=?"), append(args, c)
	}
	cond := ""
	if len(where) > 0 {
		cond = " WHERE " + strings.Join(where, " AND ")
	}
	rows, err := db.DB.Query(`SELECT id, guild_id, channel_id, message_id, created_at, mode, kind, state, reason, score, message, draft, verdict, cost_usd,
		COALESCE(json_extract(features,'$.next'),'')
		FROM participation_log`+cond+` ORDER BY created_at DESC LIMIT ?`, append(args, min(200, max(1, intVal(in, "limit", 40))))...)
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	log := []map[string]any{}
	for rows.Next() {
		var id, gg, cc, mid, at, mode, kind, state, reason, msg, draft, verdict, next string
		var score, cost float64
		if rows.Scan(&id, &gg, &cc, &mid, &at, &mode, &kind, &state, &reason, &score, &msg, &draft, &verdict, &cost, &next) == nil {
			log = append(log, map[string]any{"id": id, "guild_id": gg, "channel_id": cc, "message_id": mid, "at": at, "mode": mode, "kind": kind,
				"state": state, "reason": reason, "score": score, "message": msg, "draft": draft, "verdict": verdict, "cost_usd": cost, "next": next})
		}
	}
	rows.Close()
	stats := map[string]int{}
	since := s.Clock.Now().AddDate(0, 0, -7).UTC().Format(time.RFC3339)
	if r2, err := db.DB.Query(`SELECT state, COUNT(*) FROM participation_log`+cond+andWhere(cond)+`created_at>=? GROUP BY state`, append(args, since)...); err == nil {
		for r2.Next() {
			var st string
			var n int
			if r2.Scan(&st, &n) == nil {
				stats[st] = n
			}
		}
		r2.Close()
	}
	// reviewed is what the move to conservative is judged by: the labels on what the bot decided to say
	// (shadow and spoken). The labels on collected moments, which no judge looked at, are counted apart.
	reviewed := map[string]int{}
	if r3, err := db.DB.Query(`SELECT CASE WHEN state='collected' THEN 'collected_'||verdict ELSE verdict END, COUNT(*) FROM participation_log`+cond+andWhere(cond)+
		`verdict<>'' AND state IN ('shadow','spoken','collected') GROUP BY 1`, args...); err == nil {
		for r3.Next() {
			var v string
			var n int
			if r3.Scan(&v, &n) == nil {
				reviewed[v] = n
			}
		}
		r3.Close()
	}
	out := map[string]any{"log": log, "stats_7d": stats, "reviewed": reviewed,
		"instance": map[string]any{"enabled": s.Doc().Participation.Enabled, "mode": s.Doc().Participation.Mode}}
	if c != "" {
		out["mode"] = s.channelPolicy(g, c).Participation
		if out["mode"] == "" {
			out["mode"] = "off"
		}
		out["effective"] = s.participationModeOf(g, c)
		s.part.mu.Lock()
		filtered := map[string]int{}
		if ch, ok := s.part.chans[g+":"+c]; ok {
			for k, v := range ch.filtered {
				filtered[k] = v
			}
		}
		s.part.mu.Unlock()
		out["filtered"] = filtered
	}
	return action.OK(req, out)
}

func andWhere(cond string) string {
	if cond == "" {
		return " WHERE "
	}
	return " AND "
}

func stringIn(v string, list []string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
