package application

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"masamong/internal/action"
	"masamong/internal/conversation"
	"masamong/internal/features/reminders"
	"masamong/internal/storage"
)

// Repeating reminders and the lines of conversation a reminder carries (see reminders.go for the
// delivery state machine they plug into).
//
// Asking: the chat tool and /remind take `repeat` (daily | weekly | monthly | hours) with `weekdays`,
// `day`, `every`, and an optional end (`until` date, `count`). The time of day comes from the time the
// person gave and is fixed when they confirm (they may still have to pick between two clock times); the
// first occurrence is the first one at or after that time.

const (
	// context lines: how many, how long each, how long all together (in characters)
	contextMaxLines = 3
	contextLineMax  = 220
	contextTotalMax = 700
	// the caller's own lines count as context for this long before the reminder was asked
	contextRecent = 10 * time.Minute
	// lines older than this are not shown or kept, however long the reminder waited
	contextKeep = 30 * 24 * time.Hour
)

func ruleJSON(r *reminders.Rule) string {
	if r == nil {
		return ""
	}
	b, err := json.Marshal(r)
	if err != nil {
		return ""
	}
	return string(b)
}

// applyRepeat reads the repetition asked for into r. It returns a refusal card for a repetition that
// cannot be set (a rule that is not one, an end in the past, too many repeating reminders).
func (s *Service) applyRepeat(req action.Request, in map[string]any, r *reminder, loc *time.Location, now time.Time) *action.Result {
	kind := strings.ToLower(strVal(in, "repeat", ""))
	if kind == "" || kind == "none" || kind == "once" {
		return nil
	}
	refuse := func(status, key string, kv ...any) *action.Result {
		res := action.WithCard(action.OK(req, map[string]any{"status": status}),
			action.Card{Title: s.msg(req, "reminders.title"), Text: s.msg(req, key, kv...), Tone: "warn", Ephemeral: true})
		return &res
	}
	if len(r.Candidates) == 0 {
		return refuse("bad_repeat", "reminders.repeat_invalid")
	}
	first := r.Candidates[0].In(loc)
	rule := reminders.Rule{Kind: kind}
	switch kind {
	case reminders.RepeatDaily:
	case reminders.RepeatWeekly:
		rule.Weekdays, rule.Derived = []int{int(first.Weekday())}, true // the weekday of the time they gave
		if w := strVal(in, "weekdays", ""); w != "" {
			days, ok := reminders.ParseWeekdays(w)
			if !ok {
				return refuse("bad_repeat", "reminders.repeat_invalid")
			}
			rule.Weekdays, rule.Derived = days, false
		}
	case reminders.RepeatMonthly:
		rule.Day = intVal(in, "day", 0)
		if rule.Day == 0 {
			rule.Day, rule.Derived = first.Day(), true // the day of the time they gave
		}
		if rule.Day < 1 || rule.Day > 31 {
			return refuse("bad_repeat", "reminders.repeat_invalid")
		}
	case reminders.RepeatHours:
		rule.Every = intVal(in, "every", 0)
		if rule.Every < 1 || rule.Every > reminders.MaxEveryHours {
			return refuse("bad_repeat", "reminders.repeat_invalid")
		}
	default:
		return refuse("bad_repeat", "reminders.repeat_invalid")
	}
	if until := strVal(in, "until", ""); until != "" {
		day, err := time.ParseInLocation("2006-01-02", until[:min(len(until), 10)], loc)
		if err != nil {
			return refuse("bad_repeat", "reminders.repeat_invalid")
		}
		end := day.AddDate(0, 0, 1).Add(-time.Second) // the whole of that day
		if !end.After(now) {
			return refuse("bad_repeat", "reminders.repeat_until_past")
		}
		r.RepeatUntil = end.UTC().Format(time.RFC3339)
	}
	// a scheduled task's series of one is a model filling the field for "every day", not a request
	if n := intVal(in, "count", 0); n > 1 || (n == 1 && r.Kind != reminderKindTask) {
		r.RepeatMax = min(n, 366)
	}
	r.Repeat = &rule
	if _, err := s.ruleAt(*r, 0).Normalize(); err != nil {
		return refuse("bad_repeat", "reminders.repeat_invalid")
	}
	if status, key, n := s.repeatCapRefusal(*r, false); status != "" {
		return refuse(status, key, "n", n)
	}
	return nil
}

// repeatCapRefusal says which cap a repeating reminder runs into (status "" for none). selfStored is true
// when the reminder is already saved (at confirmation), so it counts itself.
func (s *Service) repeatCapRefusal(r reminder, selfStored bool) (status, key string, n int) {
	user, room := s.repeatingCounts(r)
	extra := 0
	if selfStored {
		extra = 1
	}
	if limit := s.toolDefault("reminders.propose", "max_repeating", 10); user >= limit+extra {
		return "too_many_repeating", "reminders.too_many_repeating", user - extra
	}
	if limit := s.toolDefault("reminders.propose", "max_repeating_room", 30); r.ChannelID != "" && room >= limit+extra {
		return "too_many_repeating_room", "reminders.too_many_repeating_room", room - extra
	}
	return "", "", 0
}

// repeatingCounts is how many repeating reminders the person has, and how many were asked for in the
// same channel (by anyone), that are still to come.
func (s *Service) repeatingCounts(r reminder) (user, room int) {
	db, err := s.adminDB()
	if err != nil {
		return 0, 0
	}
	_ = db.DB.QueryRow(`SELECT COUNT(*) FROM reminders WHERE owner_id=? AND repeat_rule<>'' AND `+reminderLive, r.OwnerID).Scan(&user)
	if r.ChannelID != "" {
		_ = db.DB.QueryRow(`SELECT COUNT(*) FROM reminders WHERE guild_id=? AND channel_id=? AND repeat_rule<>'' AND `+reminderLive, r.GuildID, r.ChannelID).Scan(&room)
	}
	return user, room
}

// ruleAt is the series' rule as the i-th candidate time would set it: its clock reading (for "every N
// hours": its moment) completes the rule.
func (s *Service) ruleAt(r reminder, i int) reminders.Rule {
	if r.Repeat == nil {
		return reminders.Rule{}
	}
	rule := *r.Repeat
	if i < 0 || i >= len(r.Candidates) {
		return rule
	}
	local := r.Candidates[i].In(s.reminderLoc(r))
	if rule.Kind == reminders.RepeatHours {
		rule.Anchor = r.Candidates[i]
		return rule
	}
	rule.Time = local.Format("15:04")
	if rule.Derived { // not asked for: it follows the time chosen
		switch rule.Kind {
		case reminders.RepeatWeekly:
			rule.Weekdays = []int{int(local.Weekday())}
		case reminders.RepeatMonthly:
			rule.Day = local.Day()
		}
	}
	return rule
}

// seriesFirst is the first occurrence a series has when the i-th candidate time is the one chosen: the
// first at or after it.
func (s *Service) seriesFirst(r reminder, i int) time.Time {
	c := r.Candidates[i]
	if first, ok := s.ruleAt(r, i).Next(c.Add(-time.Nanosecond), s.reminderLoc(r)); ok {
		return first
	}
	return c
}

// materializeRepeat completes the rule of a confirmed series and gives its first occurrence.
func (s *Service) materializeRepeat(r reminder) (reminders.Rule, time.Time, bool) {
	if r.Repeat == nil || len(r.Candidates) != 1 {
		return reminders.Rule{}, time.Time{}, false
	}
	rule, err := s.ruleAt(r, 0).Normalize()
	if err != nil {
		return reminders.Rule{}, time.Time{}, false
	}
	first := s.seriesFirst(r, 0)
	return rule, first, true
}

func seriesWithin(t time.Time, untilRFC string) bool {
	until, err := time.Parse(time.RFC3339, untilRFC)
	return err != nil || !t.After(until)
}

// repeatText says how a reminder repeats, in the language given.
func (s *Service) repeatText(locale string, rule reminders.Rule, r reminder) string {
	var base string
	switch rule.Kind {
	case reminders.RepeatDaily:
		base = s.I18n.T(locale, "reminders.repeat.daily", "time", rule.Time)
	case reminders.RepeatWeekly:
		days := make([]string, 0, len(rule.Weekdays))
		for _, d := range rule.Weekdays {
			days = append(days, s.I18n.T(locale, "reminders.weekday."+string(rune('0'+d))))
		}
		base = s.I18n.T(locale, "reminders.repeat.weekly", "days", strings.Join(days, ", "), "time", rule.Time)
	case reminders.RepeatMonthly:
		key := "reminders.repeat.monthly"
		if rule.Day > 28 {
			key = "reminders.repeat.monthly_last"
		}
		base = s.I18n.T(locale, key, "day", rule.Day, "time", rule.Time)
	case reminders.RepeatHours:
		base = s.I18n.T(locale, "reminders.repeat.hours", "n", rule.Every)
	}
	parts := []string{base}
	if r.RepeatUntil != "" {
		if end, err := time.Parse(time.RFC3339, r.RepeatUntil); err == nil {
			parts = append(parts, s.I18n.T(locale, "reminders.repeat.until", "date", end.In(s.reminderLoc(r)).Format("2006-01-02")))
		}
	}
	if r.RepeatMax > 0 {
		parts = append(parts, s.I18n.T(locale, "reminders.repeat.count", "n", r.RepeatMax))
	}
	return strings.Join(parts, " · ")
}

// nextOccurrence is the due_at the series moves to when an occurrence is leased at now, "" when that was
// its last (the end date or the count is reached). leasing is false when the series only skips ahead.
func (s *Service) nextOccurrence(r reminder, now time.Time, leasing bool) string {
	if r.Repeat == nil {
		return ""
	}
	done := r.Occurrences
	if leasing {
		done++
	}
	if r.RepeatMax > 0 && done >= r.RepeatMax {
		return ""
	}
	next, ok := r.Repeat.Next(now, s.reminderLoc(r))
	if !ok || (r.RepeatUntil != "" && !seriesWithin(next, r.RepeatUntil)) {
		return ""
	}
	return next.UTC().Format(time.RFC3339)
}

// skipSeries moves a series past occurrences that were too late to send (nothing is sent).
func (s *Service) skipSeries(r reminder, now time.Time) {
	db, err := s.adminDB()
	if err != nil {
		return
	}
	stamp := now.UTC().Format(time.RFC3339)
	if next := s.nextOccurrence(r, now, false); next != "" {
		_, _ = db.DB.Exec(`UPDATE reminders SET due_at=?, error='skipped: the bot was off', updated_at=? WHERE id=? AND status='scheduled' AND version=?`, next, stamp, r.ID, r.Version)
		return
	}
	if rowsOf(db.DB.Exec(`UPDATE reminders SET status='missed', updated_at=? WHERE id=? AND status='scheduled' AND version=?`, stamp, r.ID, r.Version)) > 0 {
		s.dropReminderContext(r.ID)
	}
}

// finishSeriesSend records how one occurrence of a series went. The series was moved on when the
// occurrence was leased, so a failed or unknown occurrence does not stop it; only the closed DM, or the
// last occurrence, ends it. Every statement requires status='sending' so a cancel or pause made while
// the send was going stays.
func (s *Service) finishSeriesSend(r reminder, msgID string, err error, stamp string) {
	db, e := s.adminDB()
	if e != nil {
		return
	}
	switch {
	case err == nil:
		_, _ = db.DB.Exec(`UPDATE reminders SET status=CASE WHEN due_at='' THEN 'sent' ELSE 'scheduled' END, attempts=0, delivered_id=?, error='', updated_at=? WHERE id=? AND status='sending'`, msgID, stamp, r.ID)
	case errors.Is(err, ErrDMClosed):
		_, _ = db.DB.Exec(`UPDATE reminders SET status='failed', error='dm_closed', updated_at=? WHERE id=? AND status='sending'`, stamp, r.ID)
	case errors.Is(err, ErrNotDelivered) && r.DueAt == "" && r.Attempts < 3:
		// the last occurrence has no next one to fall back on: try again a little later, as a one-time reminder does
		retry := s.Clock.Now().Add(time.Duration(r.Attempts*r.Attempts) * time.Minute).UTC().Format(time.RFC3339)
		_, _ = db.DB.Exec(`UPDATE reminders SET status='scheduled', due_at=?, occurrences=occurrences-1, error=?, updated_at=? WHERE id=? AND status='sending'`, retry, clip(err.Error(), 300), stamp, r.ID)
	case errors.Is(err, ErrNotDelivered):
		// nothing was posted; a series that goes on does not retry, its next occurrence is on the way
		_, _ = db.DB.Exec(`UPDATE reminders SET status=CASE WHEN due_at='' THEN 'failed' ELSE 'scheduled' END, attempts=0, error=?, updated_at=? WHERE id=? AND status='sending'`, clip(err.Error(), 300), stamp, r.ID)
	default:
		// the connection broke: this occurrence may have been posted, so it is not sent again
		_, _ = db.DB.Exec(`UPDATE reminders SET status=CASE WHEN due_at='' THEN 'unknown' ELSE 'scheduled' END, attempts=0, error=?, updated_at=? WHERE id=? AND status='sending'`, clip(err.Error(), 300), stamp, r.ID)
	}
	// a pause that raced the last occurrence leaves nothing to resume: the series is finished
	_, _ = db.DB.Exec(`UPDATE reminders SET status='sent', updated_at=? WHERE id=? AND status='paused' AND due_at=''`, stamp, r.ID)
}

// reminderEnded reports whether a reminder will not be sent any more.
func (s *Service) reminderEnded(id string) bool {
	r, ok := s.reminderByID(id)
	if !ok {
		return true
	}
	switch r.Status {
	case reminderSent, reminderFailed, reminderUnknown, reminderMissed, reminderCancelled:
		return true
	}
	return false
}

// pauseReminder stops a series from firing until it is resumed.
func (s *Service) pauseReminder(r reminder) bool {
	db, err := s.adminDB()
	if err != nil {
		return false
	}
	res, err := db.DB.Exec(`UPDATE reminders SET status='paused', version=version+1, updated_at=? WHERE id=? AND repeat_rule<>'' AND (status='scheduled' OR (status='sending' AND due_at<>''))`,
		s.Clock.Now().UTC().Format(time.RFC3339), r.ID)
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		s.emit("reminders", r.ID, map[string]any{"status": reminderPaused})
	}
	return n > 0
}

// resumeReminder starts a paused series again at its next occurrence from now. state is ok, over (the
// end date or count was reached while it was paused) or changed.
func (s *Service) resumeReminder(r reminder) (time.Time, string) {
	db, err := s.adminDB()
	if err != nil || r.Repeat == nil {
		return time.Time{}, "changed"
	}
	now := s.Clock.Now()
	stamp := now.UTC().Format(time.RFC3339)
	next := s.nextOccurrence(r, now, false)
	if next == "" {
		res, err := db.DB.Exec(`UPDATE reminders SET status='sent', due_at='', version=version+1, updated_at=? WHERE id=? AND status='paused'`, stamp, r.ID)
		if n := rowsOf(res, err); n > 0 {
			s.dropReminderContext(r.ID)
			s.emit("reminders", r.ID, map[string]any{"status": reminderSent})
			return time.Time{}, "over"
		}
		return time.Time{}, "changed"
	}
	res, err := db.DB.Exec(`UPDATE reminders SET status='scheduled', due_at=?, version=version+1, updated_at=? WHERE id=? AND status='paused'`, next, stamp, r.ID)
	if rowsOf(res, err) == 0 {
		return time.Time{}, "changed"
	}
	s.emit("reminders", r.ID, map[string]any{"status": reminderScheduled})
	at, _ := time.Parse(time.RFC3339, next)
	return at, "ok"
}

func rowsOf(res sql.Result, err error) int64 {
	if err != nil || res == nil {
		return 0
	}
	n, _ := res.RowsAffected()
	return n
}

// ---------------------------------------------------------------------------
// Context lines
// ---------------------------------------------------------------------------

// A reminder asked for in chat with something vague ("remind me about that") carries up to three recent
// lines of the conversation it was asked in, so the reminder can say what "that" was. Only the caller's
// own conversation (the lines attribution puts with the call, and what they themself said in the last
// ten minutes) is used, only in a channel whose messages
// are stored (capture on, memory in use), and never a line of someone who withdrew memory consent. The
// lines are kept with the message they came from: deleting or editing that message, or the author
// withdrawing, removes them (redactReminderContext), and they go when the reminder is over.

type reminderContextLine struct {
	MessageID string `json:"message_id,omitempty"`
	AuthorID  string `json:"author_id,omitempty"`
	Author    string `json:"author,omitempty"`
	Text      string `json:"text"`
	At        string `json:"at,omitempty"`
}

// capContext applies the limits: the newest three lines, each at most contextLineMax characters, all
// together (names included) at most contextTotalMax. The result is oldest first.
func capContext(lines []reminderContextLine) []reminderContextLine {
	var out []reminderContextLine
	budget := contextTotalMax
	for i := len(lines) - 1; i >= 0 && len(out) < contextMaxLines; i-- {
		l := lines[i]
		l.Author = hardClip(strings.Join(strings.Fields(l.Author), " "), 40)
		l.Text = hardClip(strings.Join(strings.Fields(l.Text), " "), contextLineMax)
		if l.Text == "" {
			continue
		}
		cost := len([]rune(l.Author)) + len([]rune(l.Text)) + 2 // "name: text"
		if cost > budget {
			room := budget - len([]rune(l.Author)) - 2
			if room < 20 {
				break
			}
			l.Text = hardClip(l.Text, room)
		}
		budget -= len([]rune(l.Author)) + len([]rune(l.Text)) + 2
		out = append(out, l)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// hardClip cuts to at most n characters, ending in an ellipsis when it cut.
func hardClip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:max(n, 0)])
	}
	return string(r[:n-1]) + "…"
}

// reminderContext picks the lines for a reminder asked in this chat turn (nil when none may be kept).
func (s *Service) reminderContext(in ChatInput) []reminderContextLine {
	if in.ChannelID == "" {
		return nil
	}
	if !s.contextAllowed(in.GuildID, in.ChannelID) {
		return nil
	}
	lines := s.liveLines(in.GuildID, in.ChannelID)
	var picked []conversation.Line
	if in.MessageID == "" {
		for _, l := range lines { // no call message to follow: the caller's own words only
			if !l.Bot && l.AuthorID == in.SpeakerID {
				picked = append(picked, l)
			}
		}
	} else {
		found := false
		for _, l := range lines {
			found = found || l.ID == in.MessageID
		}
		if !found {
			lines = append(lines, conversation.Line{ID: in.MessageID, AuthorID: in.SpeakerID, Author: in.Speaker, At: s.Clock.Now(), Text: in.Message})
		}
		botID := s.botAccount(lines)
		att := conversation.Attribute(lines, []string{in.MessageID}, in.SpeakerID,
			conversation.Options{BotID: botID, Names: conversation.NamesFrom(lines)})
		// the caller's conversation, and what the caller themself just said even when other talk came
		// between ("that" is usually their own last words); other people's lines only from the conversation
		recent := s.Clock.Now().Add(-contextRecent)
		keep := append([]string(nil), att.Included...)
		for _, l := range lines {
			if !l.Bot && l.AuthorID == in.SpeakerID && l.At.After(recent) {
				keep = append(keep, l.ID)
			}
		}
		picked = conversation.Keep(lines, keep)
	}
	var out []reminderContextLine
	for _, l := range picked {
		if l.Bot || l.ID == in.MessageID || l.ID == "" || l.AuthorID == "" || strings.TrimSpace(l.Text) == "" || !s.MemoryAllowed(l.AuthorID) {
			continue
		}
		out = append(out, reminderContextLine{MessageID: l.ID, AuthorID: l.AuthorID, Author: l.Author, Text: l.Text, At: l.At.UTC().Format(time.RFC3339)})
	}
	return capContext(out)
}

// keepReminderContext stores the context lines the chat turn attached to a new reminder. Only the chat
// path may attach them: anything in a command's or the console's input is ignored.
func (s *Service) keepReminderContext(req action.Request, in map[string]any, id string, vague bool) {
	if req.Invoker == nil || req.Invoker.Via != "chat" || !(vague || boolVal(in, "with_context")) {
		return
	}
	raw, ok := in["context_lines"]
	if !ok {
		return
	}
	var lines []reminderContextLine
	if b, err := json.Marshal(raw); err != nil || json.Unmarshal(b, &lines) != nil {
		return
	}
	lines = capContext(lines)
	db, err := s.adminDB()
	if err != nil || len(lines) == 0 {
		return
	}
	err = db.WithWriter(func(tx *sql.Tx) error {
		for i, l := range lines {
			if _, err := tx.Exec(`INSERT INTO reminder_context(reminder_id, seq, message_id, author_id, author_name, text, at) VALUES(?,?,?,?,?,?,?)`,
				id, i, l.MessageID, l.AuthorID, l.Author, l.Text, l.At); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		s.Log.Warn("reminders: context lines not kept", "err", err)
	}
}

// reminderContextLines is the text shown under the context header when a reminder fires. It is read
// under the same rules as when the lines were taken: the channel must still capture and use memory, a
// line whose author has since withdrawn memory consent is left out even if its erasure has not run yet,
// and lines older than contextKeep are not shown (the rows are dropped with them).
func (s *Service) reminderContextLines(r reminder) string {
	db, err := s.adminDB()
	if err != nil {
		return ""
	}
	if !s.contextAllowed(r.GuildID, r.ChannelID) {
		s.dropReminderContext(r.ID)
		return ""
	}
	rows, err := db.DB.Query(`SELECT author_id, author_name, text, at FROM reminder_context WHERE reminder_id=? ORDER BY seq`, r.ID)
	if err != nil {
		return ""
	}
	defer rows.Close()
	var out []string
	old := s.Clock.Now().Add(-contextKeep)
	for rows.Next() {
		var author, name, text, at string
		if rows.Scan(&author, &name, &text, &at) != nil || !s.MemoryAllowed(author) {
			continue
		}
		if t, err := time.Parse(time.RFC3339, at); err == nil && t.Before(old) {
			continue
		}
		out = append(out, "> "+name+": "+text)
	}
	return strings.Join(out, "\n")
}

// contextAllowed is whether conversation lines may be taken from, or shown for, a channel: its messages
// are stored (capture on, instance and channel) and memory is used there.
func (s *Service) contextAllowed(guildID, channelID string) bool {
	if channelID == "" {
		return false
	}
	pol := s.channelPolicy(guildID, channelID)
	return pol.Capture && s.Doc().Memory.Capture && pol.MemoryUse != "off"
}

func (s *Service) dropReminderContext(id string) {
	if db, err := s.adminDB(); err == nil {
		_, _ = db.DB.Exec(`DELETE FROM reminder_context WHERE reminder_id=?`, id)
	}
}

// dropReminderContextMessages removes the lines taken from messages that were deleted or edited. The
// durable erasure job does the same later (redactReminderContext); this makes it immediate.
func (s *Service) dropReminderContextMessages(messageIDs []string) {
	db, err := s.adminDB()
	if err != nil || len(messageIDs) == 0 {
		return
	}
	raw, _ := json.Marshal(messageIDs)
	if _, err := db.DB.Exec(`DELETE FROM reminder_context WHERE message_id<>'' AND message_id IN (SELECT value FROM json_each(?))`, string(raw)); err != nil {
		s.Log.Warn("reminders: context lines of changed messages not removed", "err", err)
	}
}

// redactReminderContext is the erasure job's part: lines from the named messages and by the named
// people go.
func (s *Service) redactReminderContext(db *storage.Store, keys map[string]bool, people []string) error {
	ids := make([]string, 0, len(keys))
	for k := range keys {
		if k != "" {
			ids = append(ids, k)
		}
	}
	rawIDs, _ := json.Marshal(ids)
	rawPeople, _ := json.Marshal(people)
	_, err := db.DB.Exec(`DELETE FROM reminder_context WHERE (message_id<>'' AND message_id IN (SELECT value FROM json_each(?))) OR (author_id<>'' AND author_id IN (SELECT value FROM json_each(?)))`,
		string(rawIDs), string(rawPeople))
	return err
}
