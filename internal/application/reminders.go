package application

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"masamong/internal/errclass"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"masamong/internal/action"
	"masamong/internal/errcode"
	"masamong/internal/features/reminders"
	"masamong/internal/lang"
)

// reminders.propose and reminders.update (12, masamong-v2-design/09 §5–8): a reminder a person asked
// for. Asking only proposes it; it is set when that same person confirms the time (picking one when
// the words allow two, "3시") and where it goes (their DMs, or the channel it was asked in). Nobody can
// confirm, change or cancel someone else's reminder, and a plan the bot noticed in a conversation (an
// offer) becomes a reminder only for each person who presses "remind me".
//
// Delivery is a due job the scheduler runs every minute. A reminder is leased (scheduled → sending)
// before it is sent, so it is sent once; a send whose outcome is unknown (the connection broke after
// Discord may have posted it, or the daemon stopped mid-send) is marked unknown and never sent again.
// A reminder more than grace_min late (the daemon was off) is marked missed instead of sent late. DMs
// wait for the end of the person's quiet hours.
//
// A reminder can repeat (reminders_repeat.go): daily, on weekdays, monthly, or every N hours. The series
// is one row. Leasing an occurrence (scheduled → sending) moves due_at to the next one in the same
// statement, so an occurrence is leased once and a crash between lease and send can neither send it twice
// nor stop the series: a restart marks that one occurrence unknown and the series goes on at its next
// due_at. After downtime only the newest occurrence still inside grace_min is sent; older ones are
// skipped, never sent in a burst. Cancel and pause apply to the series, also while one is being sent.
//
// Knobs (tools.reminders.propose.defaults): default_time ("09:00", for a day without a time),
// max_active (20 a person), max_days (366 ahead), grace_min (30), max_repeating (10 repeating reminders
// a person), max_repeating_room (30 in a channel).

const (
	reminderProposed  = "proposed"
	reminderScheduled = "scheduled"
	reminderSending   = "sending"
	reminderSent      = "sent"
	reminderFailed    = "failed"
	reminderUnknown   = "unknown"
	reminderMissed    = "missed"
	reminderCancelled = "cancelled"
	reminderPaused    = "paused" // a series that does not fire until it is resumed
)

// reminderLive is the SQL for a reminder that is still to come: asked, set, paused, or a series whose
// occurrence is being sent (the series itself goes on).
const reminderLive = `(status IN ('proposed','scheduled','paused') OR (status='sending' AND repeat_rule<>''))`

var errReminderChanged = errors.New("reminders: state changed")

func init() {
	featureDDL = append(featureDDL,
		`CREATE TABLE IF NOT EXISTS reminders (
			id TEXT PRIMARY KEY,
			owner_id TEXT NOT NULL,
			owner_name TEXT NOT NULL DEFAULT '',
			guild_id TEXT NOT NULL DEFAULT '',
			channel_id TEXT NOT NULL DEFAULT '',
			destination TEXT NOT NULL DEFAULT '',
			content TEXT NOT NULL,
			original_text TEXT NOT NULL DEFAULT '',
			timezone TEXT NOT NULL,
			locale TEXT NOT NULL DEFAULT '',
			candidates TEXT NOT NULL DEFAULT '[]',
			precision TEXT NOT NULL DEFAULT '',
			due_at TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL,
			version INTEGER NOT NULL DEFAULT 1,
			attempts INTEGER NOT NULL DEFAULT 0,
			error TEXT NOT NULL DEFAULT '',
			delivered_id TEXT NOT NULL DEFAULT '',
			offer_id TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		// the lines of the conversation a reminder was asked in ("remind me about that"), kept by the
		// message they came from so a deleted or edited message, or a withdrawn author, removes them
		`CREATE TABLE IF NOT EXISTS reminder_context (
			reminder_id TEXT NOT NULL,
			seq INTEGER NOT NULL,
			message_id TEXT NOT NULL DEFAULT '',
			author_id TEXT NOT NULL DEFAULT '',
			author_name TEXT NOT NULL DEFAULT '',
			text TEXT NOT NULL,
			at TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (reminder_id, seq)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_reminder_context_message ON reminder_context(message_id)`,
		`CREATE INDEX IF NOT EXISTS idx_reminder_context_author ON reminder_context(author_id)`,
		`CREATE INDEX IF NOT EXISTS idx_reminders_due ON reminders(status, due_at)`,
		`CREATE INDEX IF NOT EXISTS idx_reminders_owner ON reminders(owner_id, status)`,
		`CREATE INDEX IF NOT EXISTS idx_reminders_channel ON reminders(guild_id, channel_id, created_at)`,
		`CREATE TABLE IF NOT EXISTS reminder_offers (
			id TEXT PRIMARY KEY,
			guild_id TEXT NOT NULL,
			channel_id TEXT NOT NULL,
			message_id TEXT NOT NULL DEFAULT '',
			content TEXT NOT NULL,
			original_text TEXT NOT NULL DEFAULT '',
			timezone TEXT NOT NULL,
			candidates TEXT NOT NULL DEFAULT '[]',
			precision TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			expires_at TEXT NOT NULL
		)`)
}

type reminder struct {
	ID          string      `json:"id"`
	OwnerID     string      `json:"owner_id"`
	OwnerName   string      `json:"owner_name,omitempty"`
	GuildID     string      `json:"guild_id,omitempty"`
	ChannelID   string      `json:"channel_id,omitempty"`
	Destination string      `json:"destination,omitempty"` // dm | channel
	Content     string      `json:"content"`
	Original    string      `json:"original_text,omitempty"`
	Timezone    string      `json:"timezone"`
	Locale      string      `json:"locale,omitempty"`
	Candidates  []time.Time `json:"candidates,omitempty"`
	Precision   string      `json:"precision,omitempty"`
	DueAt       string      `json:"due_at,omitempty"`
	Status      string      `json:"status"`
	Version     int         `json:"version"`
	Attempts    int         `json:"attempts"`
	Error       string      `json:"error,omitempty"`
	OfferID     string      `json:"offer_id,omitempty"`
	CreatedAt   string      `json:"created_at"`
	UpdatedAt   string      `json:"updated_at"`
	// A repeating reminder (reminders_repeat.go). DueAt is its next occurrence.
	Repeat      *reminders.Rule `json:"repeat,omitempty"`
	RepeatUntil string          `json:"repeat_until,omitempty"` // the last moment an occurrence may fall on (RFC 3339)
	RepeatMax   int             `json:"repeat_max,omitempty"`   // how many occurrences in all (0 = no limit)
	Occurrences int             `json:"occurrences,omitempty"`  // occurrences leased so far
	SendingAt   string          `json:"sending_at,omitempty"`   // the occurrence being sent
	// A scheduled task (reminders_task.go): Kind "task" runs Task, a request written to stand on its
	// own, as a bot turn when due and posts the answer; Content is its short name. "" is a reminder.
	Kind string `json:"kind,omitempty"`
	Task string `json:"task,omitempty"`
}

const reminderCols = `id, owner_id, owner_name, guild_id, channel_id, destination, content, original_text, timezone, locale,
	candidates, precision, due_at, status, version, attempts, error, offer_id, created_at, updated_at,
	repeat_rule, repeat_until, repeat_max, occurrences, sending_at, kind, task`

func scanReminder(sc interface{ Scan(...any) error }) (reminder, error) {
	var r reminder
	var cands, rule string
	err := sc.Scan(&r.ID, &r.OwnerID, &r.OwnerName, &r.GuildID, &r.ChannelID, &r.Destination, &r.Content, &r.Original, &r.Timezone, &r.Locale,
		&cands, &r.Precision, &r.DueAt, &r.Status, &r.Version, &r.Attempts, &r.Error, &r.OfferID, &r.CreatedAt, &r.UpdatedAt,
		&rule, &r.RepeatUntil, &r.RepeatMax, &r.Occurrences, &r.SendingAt, &r.Kind, &r.Task)
	_ = json.Unmarshal([]byte(cands), &r.Candidates)
	if rule != "" {
		var ru reminders.Rule
		if json.Unmarshal([]byte(rule), &ru) == nil {
			r.Repeat = &ru
		}
	}
	return r, err
}

func (s *Service) reminderByID(id string) (reminder, bool) {
	db, err := s.adminDB()
	if err != nil {
		return reminder{}, false
	}
	r, err := scanReminder(db.DB.QueryRow(`SELECT `+reminderCols+` FROM reminders WHERE id=?`, id))
	return r, err == nil
}

func (s *Service) reminderLoc(r reminder) *time.Location {
	if r.Timezone == "" {
		return s.instanceZone()
	}
	if loc, err := time.LoadLocation(r.Timezone); err == nil {
		return loc
	} else {
		s.Log.Error("reminder: unknown stored timezone; using the instance timezone", "reminder", r.ID, "timezone", r.Timezone, "err", err)
	}
	return s.instanceZone()
}

// whenText is a moment as people read it, in the reminder's time zone and language.
func (s *Service) whenText(locale string, t time.Time, loc *time.Location) string {
	lt := t.In(loc)
	return s.I18n.T(locale, "reminders.when", "month", int(lt.Month()), "day", lt.Day(),
		"weekday", s.I18n.T(locale, "reminders.weekday."+strconv.Itoa(int(lt.Weekday()))), "time", lt.Format("15:04"))
}

// ---------------------------------------------------------------------------
// reminders.propose
// ---------------------------------------------------------------------------

func (s *Service) remindersPropose(req action.Request) action.Result {
	in := inputMap(req)
	userID, private, ok := fortuneInvoker(req)
	title := s.msg(req, "reminders.title")
	if !ok {
		return action.Fail(req, errcode.InvalidInput, "reminders.discord_only", s.msg(req, "reminders.discord_only"), nil)
	}
	if n := s.activeReminders(userID); n >= s.toolDefault("reminders.propose", "max_active", 20) {
		return action.WithCard(action.OK(req, map[string]any{"status": "too_many", "active": n}),
			action.Card{Title: title, Text: s.msg(req, "reminders.too_many", "n", n), Tone: "warn", Ephemeral: true})
	}
	// a scheduled task (reminders_task.go): what to do each time, or the one question to ask first
	isTask := strings.EqualFold(strVal(in, "kind", ""), reminderKindTask) && strVal(in, "offer_id", "") == ""
	task := strings.TrimSpace(strVal(in, "task", ""))
	if isTask {
		if refused := s.taskProposalRefusal(req, in, userID, task); refused != nil {
			return *refused
		}
	}
	prefs := s.userPrefs(userID)
	loc := s.userLocation(prefs)
	now := s.Clock.Now()
	vague := false // nothing was said about what to remind: the conversation it came from may say
	r := reminder{ID: newID("rm_"), OwnerID: userID, OwnerName: clip(req.Invoker.Name, 80), GuildID: req.Invoker.GuildID, ChannelID: req.Invoker.ChannelID,
		Timezone: loc.String(), Locale: req.Locale, Status: reminderProposed, Version: 1}
	if private {
		r.GuildID, r.ChannelID = "", "" // asked in DMs: it can only go back there
	}
	if offerID := strVal(in, "offer_id", ""); offerID != "" {
		o, ok := s.reminderOffer(offerID)
		if !ok || now.After(o.expires) {
			return action.WithCard(action.OK(req, map[string]any{"status": "offer_gone"}),
				action.Card{Title: title, Text: s.msg(req, "reminders.offer_gone"), Tone: "neutral", Ephemeral: true})
		}
		r.Content, r.Original, r.Candidates, r.Precision, r.OfferID = o.content, o.original, futureOnly(o.candidates, now), o.precision, o.id
		r.GuildID, r.ChannelID = o.guild, o.channel
		if o.timezone != "" {
			r.Timezone = o.timezone
		}
		if len(r.Candidates) == 0 {
			return action.WithCard(action.OK(req, map[string]any{"status": "past"}),
				action.Card{Title: title, Text: s.msg(req, "reminders.past"), Tone: "warn", Ephemeral: true})
		}
	} else {
		text := strings.TrimSpace(strVal(in, "text", ""))
		when, what := strings.TrimSpace(strVal(in, "when", "")), strings.TrimSpace(strVal(in, "what", ""))
		if text == "" {
			text = strings.TrimSpace(when + " " + what)
		}
		if text == "" {
			return action.WithCard(action.OK(req, map[string]any{"status": "help"}),
				action.Card{Title: title, Text: s.msg(req, "reminders.help"), Tone: "neutral", Ephemeral: true})
		}
		pack := lang.For(req.Locale, s.Doc().I18n.DefaultLocale)
		w, err := reminders.Parse(text, now, loc, pack.Time, reminders.Options{DefaultTime: s.toolString("reminders.propose", "default_time", "09:00")})
		if err != nil {
			key := "reminders.no_time"
			if errors.Is(err, reminders.ErrPast) {
				key = "reminders.past"
			} else if errors.Is(err, reminders.ErrInvalidDate) {
				key = "reminders.invalid_date"
			}
			return action.WithCard(action.OK(req, map[string]any{"status": strings.TrimPrefix(key, "reminders.")}),
				action.Card{Title: title, Text: s.msg(req, key) + "\n" + s.msg(req, "reminders.help"), Tone: "warn", Ephemeral: true})
		}
		content := what
		if content == "" {
			content = w.Rest
		}
		if content == "" {
			content = s.msg(req, "reminders.default_content")
			vague = true
		}
		if isTask {
			if task == "" {
				task = strings.TrimSpace(strVal(in, "what", w.Rest))
			}
			r.Kind, r.Task = reminderKindTask, clip(task, taskMaxRunes)
			if vague {
				content, vague = task, false // a task is named by what it does
			}
		}
		r.Content, r.Original, r.Precision = clip(content, 300), clip(w.Matched, 120), w.Candidates[0].Precision
		for _, c := range w.Candidates {
			r.Candidates = append(r.Candidates, c.At)
		}
	}
	limit := now.AddDate(0, 0, s.toolDefault("reminders.propose", "max_days", 366))
	for _, c := range r.Candidates {
		if c.After(limit) {
			return action.WithCard(action.OK(req, map[string]any{"status": "too_far"}),
				action.Card{Title: title, Text: s.msg(req, "reminders.too_far"), Tone: "warn", Ephemeral: true})
		}
	}
	if r.OfferID == "" {
		if refused := s.applyRepeat(req, in, &r, loc, now); refused != nil {
			return *refused
		}
	}
	if err := s.saveReminder(r, true, 0, ""); err != nil {
		s.Log.Error("reminders: proposal save failed", "err", err)
		return action.Fail(req, errcode.IO, "generic.error", s.msg(req, "generic.error"), nil)
	}
	if r.Kind == reminderKindTask {
		// a task stands on its own (it runs with no conversation): no lines are kept with it
		return s.taskProposed(req, r, private, now)
	}
	s.keepReminderContext(req, in, r.ID, vague)
	out := map[string]any{"status": reminderProposed, "reminder": r}
	if r.Repeat != nil {
		out["repeat_text"] = s.repeatText(req.Locale, s.ruleAt(r, 0), r)
	}
	return action.WithCard(action.OK(req, out), s.proposalCard(req, r, private))
}

func futureOnly(ts []time.Time, now time.Time) []time.Time {
	var out []time.Time
	for _, t := range ts {
		if t.After(now) {
			out = append(out, t)
		}
	}
	return out
}

func (s *Service) activeReminders(userID string) int {
	db, err := s.adminDB()
	if err != nil {
		return 0
	}
	var n int
	_ = db.DB.QueryRow(`SELECT COUNT(*) FROM reminders WHERE owner_id=? AND `+reminderLive, userID).Scan(&n)
	return n
}

func (s *Service) saveReminder(r reminder, insert bool, expectedVersion int, expectedStatus string) error {
	db, err := s.adminDB()
	if err != nil {
		return err
	}
	now := s.Clock.Now().UTC().Format(time.RFC3339)
	cands, _ := json.Marshal(r.Candidates)
	return db.WithWriter(func(tx *sql.Tx) error {
		if insert {
			_, err := tx.Exec(`INSERT INTO reminders(`+reminderCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				r.ID, r.OwnerID, r.OwnerName, r.GuildID, r.ChannelID, r.Destination, r.Content, r.Original, r.Timezone, r.Locale,
				string(cands), r.Precision, r.DueAt, r.Status, r.Version, r.Attempts, r.Error, r.OfferID, now, now,
				ruleJSON(r.Repeat), r.RepeatUntil, r.RepeatMax, r.Occurrences, r.SendingAt, r.Kind, r.Task)
			return err
		}
		res, err := tx.Exec(`UPDATE reminders SET destination=?, content=?, original_text=?, candidates=?, precision=?, due_at=?, status=?, version=?,
			attempts=?, error=?, repeat_rule=?, updated_at=? WHERE id=? AND version=? AND status=?`,
			r.Destination, r.Content, r.Original, string(cands), r.Precision, r.DueAt, r.Status, r.Version, r.Attempts, r.Error, ruleJSON(r.Repeat), now, r.ID, expectedVersion, expectedStatus)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return errReminderChanged
		}
		return nil
	})
}

// proposalCard asks the person to pick a time (when the words allow two) and where the reminder goes.
func (s *Service) proposalCard(req action.Request, r reminder, private bool) action.Card {
	loc := s.reminderLoc(r)
	card := action.Card{Title: s.msg(req, "reminders.confirm_title"), Text: r.Content, Tone: "neutral", Ephemeral: true}
	if len(r.Candidates) > 1 {
		card.Footer = s.msg(req, "reminders.pick_time")
		for i, c := range r.Candidates {
			label := s.whenText(req.Locale, c, loc)
			if r.Repeat != nil {
				label = s.repeatText(req.Locale, s.ruleAt(r, i), r) // the repetition is what the person picks between
			}
			card.Buttons = append(card.Buttons, action.CardButton{ID: fmt.Sprintf("remind:pick:%s:%d", r.ID, i), Label: clip(label, 80), Style: "primary"})
		}
	} else {
		first := r.Candidates[0]
		if r.Repeat != nil {
			first = s.seriesFirst(r, 0)
		}
		card.Fields = []action.CardField{{Name: s.msg(req, "reminders.field.when"), Value: s.whenText(req.Locale, first, loc), Inline: true}}
		if r.Repeat != nil {
			card.Fields = append(card.Fields, action.CardField{Name: s.msg(req, "reminders.field.repeat"), Value: s.repeatText(req.Locale, s.ruleAt(r, 0), r), Inline: true})
		}
		if r.Precision == reminders.DayPart || r.Precision == reminders.DayOnly {
			card.Footer = s.msg(req, "reminders.approx_time")
		}
		card.Buttons = append(card.Buttons, action.CardButton{ID: "remind:dm:" + r.ID, Label: s.msg(req, "reminders.button.dm"), Style: "primary"})
		if !private && r.ChannelID != "" {
			card.Buttons = append(card.Buttons, action.CardButton{ID: "remind:here:" + r.ID, Label: s.msg(req, "reminders.button.here"), Style: "secondary"})
		}
	}
	card.Buttons = append(card.Buttons, action.CardButton{ID: "remind:cancel:" + r.ID, Label: s.msg(req, "reminders.button.cancel"), Style: "danger"})
	return card
}

// ---------------------------------------------------------------------------
// reminders.update
// ---------------------------------------------------------------------------

// Discord (the owner only):
// {op: pick, id, n}                     choose one of the times the words allowed
// {op: confirm, id, destination}        set it: dm | channel
// {op: cancel, id}
// {op: reschedule, id, text}            new time words: proposed again, confirmed again
// {op: pause, id} · {op: resume, id}    a repeating reminder: stop firing / start again from now
// {op: list}                            my reminders that are still to come
// cancel, pause and resume on a repeating reminder apply to the whole series.
// Console:
// {op: list, guild_id?, channel_id?, user_id?, status?}
// {op: cancel, id} · {op: pause, id} · {op: resume, id}
func (s *Service) remindersUpdate(req action.Request) action.Result {
	in := inputMap(req)
	op := strVal(in, "op", "list")
	userID, private, fromDiscord := fortuneInvoker(req)
	if !fromDiscord {
		return s.remindersConsole(req, in, op)
	}
	title := s.msg(req, "reminders.title")
	if op == "list" {
		return s.myReminders(req, userID)
	}
	id := strVal(in, "id", "")
	if id == "" && (op == "cancel" || op == "pause" || op == "resume") {
		// said in words ("아침 뉴스 예약 취소해 줘"): the person's own reminder the words point at
		picked, refused := s.reminderByWords(req, userID, strVal(in, "match", ""))
		if refused != nil {
			return *refused
		}
		id = picked
	}
	r, ok := s.reminderByID(id)
	if !ok {
		return action.WithCard(action.OK(req, map[string]any{"status": "not_found"}),
			action.Card{Title: title, Text: s.msg(req, "reminders.not_found"), Tone: "neutral", Ephemeral: true})
	}
	if r.OwnerID != userID {
		return action.Fail(req, errcode.PermissionDenied, "reminders.not_yours", s.msg(req, "reminders.not_yours"), nil)
	}
	loc := s.reminderLoc(r)
	now := s.Clock.Now()
	switch op {
	case "pick":
		n := intVal(in, "n", -1)
		if r.Status != reminderProposed || n < 0 || n >= len(r.Candidates) {
			return s.reminderStateCard(req, r)
		}
		expectedVersion, expectedStatus := r.Version, r.Status
		r.Candidates = []time.Time{r.Candidates[n]}
		r.Version++
		if err := s.saveReminder(r, false, expectedVersion, expectedStatus); err != nil {
			if errors.Is(err, errReminderChanged) {
				if current, ok := s.reminderByID(r.ID); ok {
					return s.reminderStateCard(req, current)
				}
			}
			s.Log.Error("reminders: proposal update failed", "err", err)
			return action.Fail(req, errcode.IO, "generic.error", s.msg(req, "generic.error"), nil)
		}
		return action.WithCard(action.OK(req, map[string]any{"status": reminderProposed, "reminder": r}), s.proposalCard(req, r, private || r.ChannelID == ""))
	case "confirm":
		dest := strVal(in, "destination", "dm")
		if r.Status != reminderProposed || len(r.Candidates) != 1 || (dest != "dm" && dest != "channel") || (dest == "channel" && r.ChannelID == "") {
			return s.reminderStateCard(req, r)
		}
		r, due, refused := s.setReminder(req, r, dest, now)
		if refused != nil {
			return *refused
		}
		where := s.msg(req, "reminders.dest.dm")
		if dest == "channel" {
			where = s.msg(req, "reminders.dest.channel")
		}
		fields := []action.CardField{{Name: s.msg(req, "reminders.field.when"), Value: s.whenText(req.Locale, due, loc), Inline: true}, {Name: s.msg(req, "reminders.field.where"), Value: where, Inline: true}}
		out := map[string]any{"status": reminderScheduled, "reminder": r}
		if r.Repeat != nil {
			text := s.repeatText(req.Locale, *r.Repeat, r)
			fields = append(fields, action.CardField{Name: s.msg(req, "reminders.field.repeat"), Value: text, Inline: true})
			out["repeat_text"] = text
		}
		return action.WithCard(action.OK(req, out), action.Card{
			Title: s.msg(req, "reminders.set_title"), Text: r.Content, Ephemeral: true, Fields: fields,
			Buttons: []action.CardButton{{ID: "remind:cancel:" + r.ID, Label: s.msg(req, "reminders.button.cancel"), Style: "danger"}},
		})
	case "cancel":
		if !cancellable(r) {
			return s.reminderStateCard(req, r)
		}
		if !s.cancelReminder(r) {
			return s.reminderStateCard(req, r)
		}
		return action.WithCard(action.OK(req, map[string]any{"status": reminderCancelled, "name": r.Content, "changed": true}),
			action.Card{Title: s.msg(req, "reminders.cancelled"), Text: r.Content, Tone: "neutral", Ephemeral: true})
	case "pause":
		if r.Repeat == nil {
			return action.WithCard(action.OK(req, map[string]any{"status": "not_repeating"}),
				action.Card{Title: title, Text: s.msg(req, "reminders.not_repeating"), Tone: "neutral", Ephemeral: true})
		}
		if !s.pauseReminder(r) {
			return s.reminderStateCard(req, r)
		}
		return action.WithCard(action.OK(req, map[string]any{"status": reminderPaused, "name": r.Content, "changed": true}),
			action.Card{Title: s.msg(req, "reminders.paused"), Text: r.Content, Tone: "neutral", Ephemeral: true,
				Buttons: []action.CardButton{{ID: "remind:resume:" + r.ID, Label: s.msg(req, "reminders.button.resume"), Style: "primary"}}})
	case "resume":
		if r.Repeat == nil || r.Status != reminderPaused {
			return s.reminderStateCard(req, r)
		}
		switch next, state := s.resumeReminder(r); state {
		case "ok":
			return action.WithCard(action.OK(req, map[string]any{"status": reminderScheduled, "name": r.Content, "changed": true, "next": s.whenText(req.Locale, next, loc)}),
				action.Card{Title: s.msg(req, "reminders.resumed"), Text: r.Content, Tone: "neutral", Ephemeral: true,
					Fields: []action.CardField{{Name: s.msg(req, "reminders.field.when"), Value: s.whenText(req.Locale, next, loc), Inline: true}}})
		case "over":
			return action.WithCard(action.OK(req, map[string]any{"status": "series_over"}),
				action.Card{Title: title, Text: s.msg(req, "reminders.series_over"), Tone: "neutral", Ephemeral: true})
		}
		return s.reminderStateCard(req, r)
	case "reschedule":
		if r.Status != reminderProposed && r.Status != reminderScheduled && r.Status != reminderPaused {
			return s.reminderStateCard(req, r)
		}
		pack := lang.For(req.Locale, s.Doc().I18n.DefaultLocale)
		w, err := reminders.Parse(strVal(in, "text", ""), now, loc, pack.Time, reminders.Options{DefaultTime: s.toolString("reminders.propose", "default_time", "09:00")})
		if err != nil {
			message := "reminders.no_time"
			status := "no_time"
			if errors.Is(err, reminders.ErrInvalidDate) {
				message = "reminders.invalid_date"
				status = "invalid_date"
			}
			return action.WithCard(action.OK(req, map[string]any{"status": status}),
				action.Card{Title: title, Text: s.msg(req, message), Tone: "warn", Ephemeral: true})
		}
		// a new time is a new agreement: it waits for confirmation again
		expectedVersion, expectedStatus := r.Version, r.Status
		r.Candidates, r.Precision, r.Original, r.Status, r.DueAt, r.Version = nil, w.Candidates[0].Precision, clip(w.Matched, 120), reminderProposed, "", r.Version+1
		for _, c := range w.Candidates {
			r.Candidates = append(r.Candidates, c.At)
		}
		if err := s.saveReminder(r, false, expectedVersion, expectedStatus); err != nil {
			if errors.Is(err, errReminderChanged) {
				if current, ok := s.reminderByID(r.ID); ok {
					return s.reminderStateCard(req, current)
				}
			}
			s.Log.Error("reminders: reschedule save failed", "err", err)
			return action.Fail(req, errcode.IO, "generic.error", s.msg(req, "generic.error"), nil)
		}
		return action.WithCard(action.OK(req, map[string]any{"status": reminderProposed, "reminder": r}), s.proposalCard(req, r, private || r.ChannelID == ""))
	}
	return action.Fail(req, errcode.InvalidInput, "generic.error", s.msg(req, "generic.error"), nil)
}

// setReminder sets a proposed reminder with one time left: it goes to dest (dm | channel) from its first
// occurrence on. It returns the saved reminder and that first moment, or the answer that refuses it (the
// time has passed, too many repeating reminders, a repetition that ends before it starts, a change made
// meanwhile).
func (s *Service) setReminder(req action.Request, r reminder, dest string, now time.Time) (reminder, time.Time, *action.Result) {
	title := s.msg(req, "reminders.title")
	refuse := func(status, key string, kv ...any) (reminder, time.Time, *action.Result) {
		res := action.WithCard(action.OK(req, map[string]any{"status": status}),
			action.Card{Title: title, Text: s.msg(req, key, kv...), Tone: "warn", Ephemeral: true})
		return r, time.Time{}, &res
	}
	if !r.Candidates[0].After(now) {
		return refuse("past", "reminders.past")
	}
	expectedVersion, expectedStatus := r.Version, r.Status
	due := r.Candidates[0]
	if r.Repeat != nil {
		if status, key, n := s.repeatCapRefusal(r, true); status != "" {
			return refuse(status, key, "n", n)
		}
		rule, first, ok := s.materializeRepeat(r)
		if !ok {
			return refuse("bad_repeat", "reminders.repeat_invalid")
		}
		r.Repeat, due = &rule, first
		if r.RepeatUntil != "" && !seriesWithin(first, r.RepeatUntil) {
			return refuse("bad_repeat", "reminders.repeat_until_past")
		}
	}
	r.Destination, r.Status, r.DueAt, r.Version = dest, reminderScheduled, due.UTC().Format(time.RFC3339), r.Version+1
	if err := s.saveReminder(r, false, expectedVersion, expectedStatus); err != nil {
		if errors.Is(err, errReminderChanged) {
			if current, ok := s.reminderByID(r.ID); ok {
				res := s.reminderStateCard(req, current)
				return r, time.Time{}, &res
			}
		}
		s.Log.Error("reminders: confirmation save failed", "err", err)
		res := action.Fail(req, errcode.IO, "generic.error", s.msg(req, "generic.error"), nil)
		return r, time.Time{}, &res
	}
	s.emit("reminders", r.ID, map[string]any{"status": r.Status})
	return r, due, nil
}

// cancelReminder cancels a reminder that is not being sent (false when it already went out or is going).
func (s *Service) cancelReminder(r reminder) bool {
	db, err := s.adminDB()
	if err != nil {
		return false
	}
	// a series is also cancelled while one of its occurrences is being sent (the send finishes, the series ends)
	res, err := db.DB.Exec(`UPDATE reminders SET status='cancelled', version=version+1, updated_at=? WHERE id=?
		AND (status IN ('proposed','scheduled','paused') OR (status='sending' AND repeat_rule<>''))`,
		s.Clock.Now().UTC().Format(time.RFC3339), r.ID)
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		s.dropReminderContext(r.ID)
		s.emit("reminders", r.ID, map[string]any{"status": reminderCancelled})
	}
	return n > 0
}

// cancellable reports whether a reminder can still be cancelled: before it is sent, or as a series.
func cancellable(r reminder) bool {
	switch r.Status {
	case reminderProposed, reminderScheduled, reminderPaused:
		return true
	case reminderSending:
		return r.Repeat != nil
	}
	return false
}

// reminderStateCard answers a press on a reminder that has moved on (already set, sent, cancelled).
func (s *Service) reminderStateCard(req action.Request, r reminder) action.Result {
	return action.WithCard(action.OK(req, map[string]any{"status": r.Status, "reminder": r}),
		action.Card{Title: s.msg(req, "reminders.title"), Text: s.msg(req, "reminders.state."+r.Status), Tone: "neutral", Ephemeral: true})
}

func (s *Service) myReminders(req action.Request, userID string) action.Result {
	list, err := s.listReminders(`owner_id=? AND `+reminderLive, []any{userID}, 25)
	if err != nil {
		s.Log.Error("reminders: list read failed", "err", err)
		return action.Fail(req, errcode.IO, "generic.error", s.msg(req, "generic.error"), nil)
	}
	card := action.Card{Title: s.msg(req, "reminders.list_title"), Tone: "neutral", Ephemeral: true}
	if len(list) == 0 {
		card.Text = s.msg(req, "reminders.list_empty")
	}
	for i, r := range list {
		when := s.msg(req, "reminders.state.proposed")
		value := clip(r.Content, 200)
		switch {
		case r.Repeat != nil && r.Status != reminderProposed:
			// a series shows its next occurrence (due_at), not the first time it was asked for
			if at, err := time.Parse(time.RFC3339, r.DueAt); err == nil && r.Status != reminderPaused {
				when = s.whenText(req.Locale, at, s.reminderLoc(r))
			} else {
				when = s.msg(req, "reminders.state.paused_label")
			}
			value = clip(r.Content, 160) + "\n" + s.repeatText(req.Locale, *r.Repeat, r)
		case r.Status == reminderScheduled && len(r.Candidates) == 1:
			when = s.whenText(req.Locale, r.Candidates[0], s.reminderLoc(r))
		}
		card.Fields = append(card.Fields, action.CardField{Name: when, Value: value})
		if i < 5 {
			card.Buttons = append(card.Buttons, action.CardButton{ID: "remind:cancel:" + r.ID,
				Label: s.msg(req, "reminders.button.cancel_n", "n", i+1), Style: "secondary"})
			if r.Repeat != nil && r.Status != reminderProposed {
				id, key := "remind:pause:"+r.ID, "reminders.button.pause_n"
				if r.Status == reminderPaused {
					id, key = "remind:resume:"+r.ID, "reminders.button.resume_n"
				}
				card.Buttons = append(card.Buttons, action.CardButton{ID: id, Label: s.msg(req, key, "n", i+1), Style: "secondary"})
			}
		}
	}
	out := map[string]any{"reminders": list}
	if req.Invoker != nil && req.Invoker.Via == "chat" {
		out = map[string]any{"reminders": s.reminderBriefs(req, list)} // the answer needs names and times, not ids of people
	}
	return action.WithCard(action.OK(req, out), card)
}

func (s *Service) listReminders(where string, args []any, limit int) ([]reminder, error) {
	db, err := s.adminDB()
	if err != nil {
		return nil, err
	}
	q := `SELECT ` + reminderCols + ` FROM reminders`
	if where != "" {
		q += ` WHERE ` + where
	}
	q += ` ORDER BY CASE WHEN due_at='' THEN created_at ELSE due_at END LIMIT ?`
	rows, err := db.DB.Query(q, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []reminder{}
	for rows.Next() {
		r, err := scanReminder(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, r)
	}
	return list, rows.Err()
}

func (s *Service) remindersConsole(req action.Request, in map[string]any, op string) action.Result {
	switch op {
	case "cancel":
		r, ok := s.reminderByID(strVal(in, "id", ""))
		if !ok {
			return action.Fail(req, errcode.InvalidInput, "generic.error", s.msg(req, "generic.error"), nil)
		}
		return action.OK(req, map[string]any{"cancelled": s.cancelReminder(r), "id": r.ID})
	case "pause", "resume":
		r, ok := s.reminderByID(strVal(in, "id", ""))
		if !ok || r.Repeat == nil {
			return action.Fail(req, errcode.InvalidInput, "generic.error", s.msg(req, "generic.error"), nil)
		}
		if op == "pause" {
			return action.OK(req, map[string]any{"paused": s.pauseReminder(r), "id": r.ID})
		}
		_, state := s.resumeReminder(r)
		return action.OK(req, map[string]any{"resumed": state == "ok", "state": state, "id": r.ID})
	case "list":
		var where []string
		var args []any
		for _, f := range []struct{ key, col string }{{"guild_id", "guild_id"}, {"channel_id", "channel_id"}, {"user_id", "owner_id"}, {"status", "status"}} {
			if v := strVal(in, f.key, ""); v != "" {
				where, args = append(where, f.col+"=?"), append(args, v)
			}
		}
		list, err := s.listReminders(strings.Join(where, " AND "), args, min(500, max(1, intVal(in, "limit", 200))))
		if err != nil {
			s.Log.Error("reminders: console list read failed", "err", err)
			return action.Fail(req, errcode.IO, "generic.error", s.msg(req, "generic.error"), nil)
		}
		return action.OK(req, map[string]any{"reminders": list})
	}
	return action.Fail(req, errcode.InvalidInput, "generic.error", s.msg(req, "generic.error"), nil)
}

// ---------------------------------------------------------------------------
// Offers: a plan the bot noticed in a conversation (participation), for anyone to take up
// ---------------------------------------------------------------------------

type reminderOfferRow struct {
	id, guild, channel, content, original, timezone, precision string
	candidates                                                 []time.Time
	expires                                                    time.Time
}

func (s *Service) saveReminderOffer(guildID, channelID, messageID, content, original string, loc *time.Location, w reminders.When) (string, error) {
	db, err := s.adminDB()
	if err != nil {
		return "", err
	}
	id := newID("ro_")
	var cands []time.Time
	for _, c := range w.Candidates {
		cands = append(cands, c.At)
	}
	raw, _ := json.Marshal(cands)
	now := s.Clock.Now()
	last := cands[len(cands)-1]
	_, err = db.DB.Exec(`INSERT INTO reminder_offers(id, guild_id, channel_id, message_id, content, original_text, timezone, candidates, precision, created_at, expires_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, id, guildID, channelID, messageID, clip(content, 300), clip(original, 120), loc.String(), string(raw),
		w.Candidates[0].Precision, now.UTC().Format(time.RFC3339), last.UTC().Format(time.RFC3339))
	return id, err
}

func (s *Service) reminderOffer(id string) (reminderOfferRow, bool) {
	db, err := s.adminDB()
	if err != nil {
		return reminderOfferRow{}, false
	}
	var o reminderOfferRow
	var cands, exp string
	err = db.DB.QueryRow(`SELECT id, guild_id, channel_id, content, original_text, timezone, candidates, precision, expires_at FROM reminder_offers WHERE id=?`, id).
		Scan(&o.id, &o.guild, &o.channel, &o.content, &o.original, &o.timezone, &cands, &o.precision, &exp)
	if err != nil {
		return o, false
	}
	_ = json.Unmarshal([]byte(cands), &o.candidates)
	o.expires, _ = time.Parse(time.RFC3339, exp)
	return o, true
}

// ---------------------------------------------------------------------------
// Delivery
// ---------------------------------------------------------------------------

var remindersRunning atomic.Bool

// recoverReminders runs once when the scheduler starts: a send the previous process did not finish may
// have been posted, so it is marked unknown and not sent again. A series goes on: that one occurrence is
// the unknown one (its due_at was already moved to the next when it was leased), the rest still come.
func (s *Service) recoverReminders() {
	db, err := s.adminDB()
	if err != nil {
		return
	}
	res, err := db.DB.Exec(`UPDATE reminders SET status=CASE WHEN repeat_rule<>'' AND due_at<>'' THEN 'scheduled' ELSE 'unknown' END,
		error='interrupted while sending', updated_at=? WHERE status='sending'`, s.Clock.Now().UTC().Format(time.RFC3339))
	if err == nil {
		if n, _ := res.RowsAffected(); n > 0 {
			s.Log.Warn("reminders: sends interrupted by a stop are marked unknown", "n", n)
		}
	}
}

// deliverReminders sends the reminders that are due.
func (s *Service) deliverReminders(ctx context.Context) {
	if !remindersRunning.CompareAndSwap(false, true) {
		return
	}
	defer remindersRunning.Store(false)
	if s.Discord == nil || !s.Discord.Connected() {
		return
	}
	now := s.Clock.Now()
	due, err := s.listReminders(`status='scheduled' AND due_at<=?`, []any{now.UTC().Format(time.RFC3339)}, 50)
	if err != nil {
		return
	}
	s.deliverRows(ctx, due, now)
}

// deliverRows sends the due reminders that were read. Every lease checks the row's version and due_at, so
// rows read earlier by another pass or process (a stale list) lease nothing that was already taken.
func (s *Service) deliverRows(ctx context.Context, due []reminder, now time.Time) {
	dm, _ := s.Discord.(DiscordSender)
	poster, _ := s.Discord.(ChannelPoster)
	db, err := s.adminDB()
	if err != nil {
		return
	}
	grace := time.Duration(s.toolDefault("reminders.propose", "grace_min", 30)) * time.Minute
	stamp := now.UTC().Format(time.RFC3339)
	for _, r := range due {
		if ctx.Err() != nil {
			return
		}
		at, _ := time.Parse(time.RFC3339, r.DueAt)
		readDue := r.DueAt
		loc := s.reminderLoc(r)
		if r.Repeat == nil {
			if now.Sub(at) > grace {
				_, _ = db.DB.Exec(`UPDATE reminders SET status='missed', updated_at=? WHERE id=? AND status='scheduled'`, stamp, r.ID)
				s.dropReminderContext(r.ID)
				continue
			}
		} else if next, ok := r.Repeat.Next(at, loc); now.Sub(at) > grace || (ok && !next.After(now)) {
			// a series that was not served (the daemon was off, or grace is longer than its interval): the
			// newest occurrence still inside grace is sent and the older ones are skipped, never sent in a burst
			if occ, ok := r.Repeat.Latest(now, grace, loc); ok {
				at = occ
			} else if now.Sub(at) > grace {
				s.skipSeries(r, now)
				continue
			}
		}
		if r.Destination == "dm" {
			if until := s.quietUntil(s.userPrefs(r.OwnerID), now); until.After(now) {
				// quiet hours: later, not missed (the wait does not count against grace)
				_, _ = db.DB.Exec(`UPDATE reminders SET due_at=?, updated_at=? WHERE id=? AND status='scheduled' AND version=?`, until.UTC().Format(time.RFC3339), stamp, r.ID, r.Version)
				continue
			}
		}
		series := r.Repeat != nil
		var res sql.Result
		if series {
			// leased and advanced in one statement: this occurrence is never leased again, and if the
			// process stops right here the series still has its next due_at
			next := s.nextOccurrence(r, now, true)
			// version and due_at are both checked and the version moves: a second process that read the row
			// before cannot lease the same occurrence again, not even after it is back to scheduled
			res, err = db.DB.Exec(`UPDATE reminders SET status='sending', attempts=attempts+1, due_at=?, sending_at=?, occurrences=occurrences+1, version=version+1, error='', updated_at=?
				WHERE id=? AND status='scheduled' AND version=? AND due_at=?`, next, at.UTC().Format(time.RFC3339), stamp, r.ID, r.Version, readDue)
			r.DueAt = next
		} else {
			res, err = db.DB.Exec(`UPDATE reminders SET status='sending', attempts=attempts+1, updated_at=? WHERE id=? AND status='scheduled' AND version=? AND due_at=?`, stamp, r.ID, r.Version, readDue)
		}
		if err != nil {
			continue
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue // cancelled or changed since it was read
		}
		r.Attempts++
		req := action.Request{Locale: r.Locale}
		if l := s.userPrefs(r.OwnerID).Locale; l != "" {
			req.Locale = l
		}
		var msgID string
		if r.Kind == reminderKindTask {
			// a task runs now and posts its answer (reminders_task.go); the lease above sends it once
			msgID, err = s.deliverTask(ctx, r, at, req.Locale, dm, poster)
		} else {
			card := action.Card{Title: s.msg(req, "reminders.delivery_title"), Text: r.Content, Tone: "reminder",
				Footer: s.msg(req, "reminders.delivery_footer", "name", r.OwnerName, "when", s.whenText(req.Locale, at, loc))}
			if series {
				card.Fields = append(card.Fields, action.CardField{Name: s.msg(req, "reminders.field.repeat"), Value: s.repeatText(req.Locale, *r.Repeat, r), Inline: true})
			}
			// the conversation lines belong to the first message of a series; later occurrences do not repeat them
			if lines := s.reminderContextLines(r); len(lines) > 0 && r.Occurrences == 0 {
				card.Fields = append(card.Fields, action.CardField{Name: s.msg(req, "reminders.context_header"), Value: lines})
			}
			sendCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			switch {
			case r.Destination == "channel" && poster != nil:
				msgID, err = poster.PostChannel(sendCtx, r.ChannelID, "", "", &card)
			case r.Destination == "dm" && dm != nil:
				err = dm.SendDM(sendCtx, r.OwnerID, card, "")
			default:
				err = ErrNotDelivered
			}
			cancel()
		}
		stamp := s.Clock.Now().UTC().Format(time.RFC3339)
		if err != nil && r.Destination == "channel" && ctx.Err() == nil {
			s.LogProblem(r.GuildID, r.ChannelID, logReminderFailed, "", errclass.Of(err))
		}
		if series {
			s.finishSeriesSend(r, msgID, err, stamp)
			if err == nil || r.DueAt != "" { // the lines went out with the first message (or its turn has passed)
				s.dropReminderContext(r.ID)
			}
		} else {
			switch {
			case err == nil:
				_, _ = db.DB.Exec(`UPDATE reminders SET status='sent', delivered_id=?, error='', updated_at=? WHERE id=?`, msgID, stamp, r.ID)
			case errors.Is(err, ErrDMClosed):
				_, _ = db.DB.Exec(`UPDATE reminders SET status='failed', error='dm_closed', updated_at=? WHERE id=?`, stamp, r.ID)
			case errors.Is(err, ErrNotDelivered) && r.Attempts < 3:
				// Discord answered with a refusal: nothing was posted, try again a little later
				next := s.Clock.Now().Add(time.Duration(r.Attempts*r.Attempts) * time.Minute).UTC().Format(time.RFC3339)
				_, _ = db.DB.Exec(`UPDATE reminders SET status='scheduled', due_at=?, error=?, updated_at=? WHERE id=?`, next, clip(err.Error(), 300), stamp, r.ID)
			case errors.Is(err, ErrNotDelivered):
				_, _ = db.DB.Exec(`UPDATE reminders SET status='failed', error=?, updated_at=? WHERE id=?`, clip(err.Error(), 300), stamp, r.ID)
			default:
				// the connection broke: it may have been posted, and sending again could post it twice
				_, _ = db.DB.Exec(`UPDATE reminders SET status='unknown', error=?, updated_at=? WHERE id=?`, clip(err.Error(), 300), stamp, r.ID)
			}
		}
		if s.reminderEnded(r.ID) {
			s.dropReminderContext(r.ID)
		}
		s.emit("reminders", r.ID, nil)
	}
}
