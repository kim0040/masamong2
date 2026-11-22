package application

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"masamong/internal/action"
	"masamong/internal/relnotes"
)

// The announcement: when the bot starts on a release it has not announced yet, it tells the people who
// asked to hear about it, once.
//
//   - Only a release counts: not a local build (…-dev), not a build made after a tag, not a
//     pre-release, never the practice instance. The first start of an installation only records the
//     running version (admin_state updates:announced): there is nothing to compare with.
//   - What is said is the user-facing part of every note in (announced, running]: a version skipped
//     (the bot was off, or updated twice) is merged into one card. Nothing user-facing, nothing sent.
//   - Who hears it: each room whose update_news is on (and where the bot answers, on an approved server,
//     not paused), and each person who subscribed (updates.subscribe) and may be sent a DM (dm.allow,
//     not blocked, outside their quiet hours).
//   - Once per (version, target): a row in update_news_sent is claimed *before* the send. A crash
//     between claim and send loses that one message; it never repeats it. A restart, a second tick
//     and a later skipped-version merge find the rows and send nothing again.
//   - updates:announced is written only after every target was handled, so a send interrupted by a
//     shutdown, or a person still in quiet hours, is picked up by the next tick.
//
// Sends are paced (a DM burst is what gets a bot rate-limited) and capped per tick; the rest follow on
// the next ones.

const (
	updatesAnnouncedKey = "updates:announced"

	updateNewsCardRunes = 3500 // inside an embed's 4096
)

// updateNewsPace is the pause between two sends; updateNewsPerTick the sends one scheduler minute makes.
var (
	updateNewsPace    = 1100 * time.Millisecond
	updateNewsPerTick = 40
)

var updateNewsRunning atomic.Bool

// updateNewsTick is one scheduler minute; a run still going is not started twice.
func (s *Service) updateNewsTick(ctx context.Context) {
	if !updateNewsRunning.CompareAndSwap(false, true) {
		return
	}
	defer updateNewsRunning.Store(false)
	s.announceUpdates(ctx)
}

// newsGateway is what the announcement needs from the live gateway.
type newsGateway interface {
	ChannelPoster
	DiscordSender
}

// announceUpdates is one pass of the announcement described above.
func (s *Service) announceUpdates(ctx context.Context) {
	if s.Store == nil || s.practice {
		return
	}
	cur, ok := newsReleaseTag(Version)
	if !ok {
		return
	}
	var last string
	if found, _ := s.kvGet(updatesAnnouncedKey, &last); !found || last == "" {
		_ = s.kvSet(updatesAnnouncedKey, cur) // the first start of this installation: nothing to announce
		return
	}
	if relnotes.Compare(last, cur) >= 0 {
		return // the same version again (a restart), or an older build: a high-water mark stays
	}
	var notes []relnotes.Note
	for _, n := range relnotes.Between(last, cur) {
		if n.Has(relnotes.AudienceUser) {
			notes = append(notes, n)
		}
	}
	if len(notes) == 0 {
		_ = s.kvSet(updatesAnnouncedKey, cur)
		return
	}
	if s.Discord == nil || !s.Discord.Connected() {
		return // after the gateway is ready
	}
	gw, ok := s.Discord.(newsGateway)
	if !ok {
		return
	}
	db, err := s.adminDB()
	if err != nil {
		return
	}
	if _, err := db.DB.Exec(`SELECT 1 FROM update_news_sent LIMIT 0`); err != nil {
		return // this storage has no ledger: nothing can be sent safely, so nothing is
	}
	run := newsRun{s: s, gw: gw, version: cur, notes: notes}
	if !run.rooms(ctx) || !run.people(ctx) {
		return
	}
	if run.deferred {
		return // somebody is in quiet hours: the next tick tries again
	}
	_ = s.kvSet(updatesAnnouncedKey, cur)
	_, _ = db.DB.Exec(`DELETE FROM update_news_sent WHERE version<>?`, cur)
}

type newsRun struct {
	s        *Service
	gw       newsGateway
	version  string
	notes    []relnotes.Note
	sent     int
	deferred bool
}

// claim reserves (version, target); false when it was already claimed (sent, failed or interrupted).
func (r *newsRun) claim(target string) bool {
	db, err := r.s.adminDB()
	if err != nil {
		return false
	}
	res, err := db.DB.Exec(`INSERT OR IGNORE INTO update_news_sent(version, target, state, at) VALUES(?,?,?,?)`,
		r.version, target, "sending", r.s.Clock.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	return n == 1
}

// settle records how a claimed send ended. An error that proves nothing was posted is "failed"; any
// other error leaves it unknown (it may have been posted), and neither is sent again.
func (r *newsRun) settle(target string, err error) {
	state := "sent"
	switch {
	case err == nil:
	case errors.Is(err, ErrDMClosed), errors.Is(err, ErrNotDelivered):
		state = "failed"
	default:
		state = "unknown"
	}
	if db, e := r.s.adminDB(); e == nil {
		_, _ = db.DB.Exec(`UPDATE update_news_sent SET state=? WHERE version=? AND target=?`, state, r.version, target)
	}
	if err != nil && r.s.Log != nil {
		r.s.Log.Warn("update news not delivered", "kind", strings.SplitN(target, ":", 2)[0], "state", state, "err", err) // no IDs in logs
	}
}

// pace waits between two sends; false when the service is shutting down.
func (r *newsRun) pace(ctx context.Context) bool {
	r.sent++
	if updateNewsPace <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(updateNewsPace)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// full reports whether this tick has used its sends (the next tick carries on).
func (r *newsRun) full() bool { return r.sent >= updateNewsPerTick }

// rooms posts one card to each room that turned update news on. It returns false when the pass must
// stop (shutdown, or the tick's sends are used).
func (r *newsRun) rooms(ctx context.Context) bool {
	doc := r.s.Doc()
	pause := r.s.pauseState()
	type room struct{ guild, channel, locale string }
	var list []room
	if !pause.All {
		for gid, g := range doc.Guilds {
			if !disasterGuildAllowed(doc, gid) {
				continue
			}
			loc := g.Locale
			if loc == "" {
				loc = doc.I18n.DefaultLocale
			}
			for cid, c := range g.Channels {
				if c.UpdateNews != "on" || c.Respond == "off" || pause.Channels[gid+":"+cid] {
					continue
				}
				list = append(list, room{gid, cid, loc})
			}
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].guild+list[i].channel < list[j].guild+list[j].channel })
	for _, rm := range list {
		if ctx.Err() != nil || r.full() {
			return false
		}
		target := "room:" + rm.guild + ":" + rm.channel
		if !r.claim(target) {
			continue
		}
		card := r.s.updateNewsCard(r.notes, r.version, rm.locale, "updates.announce.room_footer")
		sendCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		_, err := r.gw.PostChannel(sendCtx, rm.channel, "", "", &card)
		cancel()
		r.settle(target, err)
		if !r.pace(ctx) {
			return false
		}
	}
	return true
}

// people sends one DM to each subscriber who may be sent one.
func (r *newsRun) people(ctx context.Context) bool {
	db, err := r.s.adminDB()
	if err != nil {
		return false
	}
	rows, err := db.DB.Query(`SELECT user_id FROM user_prefs WHERE update_news=1 ORDER BY user_id`)
	if err != nil {
		return false
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	now := r.s.Clock.Now()
	for _, id := range ids {
		if ctx.Err() != nil || r.full() {
			return false
		}
		if r.s.BlockedIn("", id) || !r.s.DMChatAllowed(ctx, id) {
			continue
		}
		prefs := r.s.userPrefs(id)
		if !prefs.UpdateNews {
			continue
		}
		if r.s.quietUntil(prefs, now).After(now) {
			r.deferred = true
			continue
		}
		target := "dm:" + id
		if !r.claim(target) {
			continue
		}
		loc := prefs.Locale
		if loc == "" {
			loc = r.s.Doc().I18n.DefaultLocale
		}
		card := r.s.updateNewsCard(r.notes, r.version, loc, "updates.announce.dm_footer")
		sendCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := r.gw.SendDM(sendCtx, id, card, "")
		cancel()
		r.settle(target, err)
		if !r.pace(ctx) {
			return false
		}
	}
	return true
}

// updateNewsCard is the short card of the notes (newest first, the user-facing part only). Notes that
// would pass the size of one embed are left out and counted.
func (s *Service) updateNewsCard(notes []relnotes.Note, version, locale, footerKey string) action.Card {
	lang := relnotes.Lang(locale)
	var parts []string
	size, omitted := 0, 0
	for i := len(notes) - 1; i >= 0; i-- {
		text := relnotes.Card(notes[i], relnotes.AudienceUser, lang)
		if text == "" {
			continue
		}
		if n := len([]rune(text)) + 2; size > 0 && size+n > updateNewsCardRunes {
			omitted++
			continue
		} else {
			size += n
		}
		parts = append(parts, text)
	}
	text := strings.Join(parts, "\n\n")
	if omitted > 0 {
		text += "\n\n… (+" + strconv.Itoa(omitted) + ")"
	}
	t := func(key string, kv ...any) string { return s.I18n.T(locale, key, kv...) }
	return action.Card{Title: t("updates.announce.title", "version", version), Text: text, Tone: "ok", Footer: t(footerKey)}
}
