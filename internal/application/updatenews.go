package application

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"masamong/internal/action"
	"masamong/internal/errcode"
	"masamong/internal/relnotes"
	"masamong/internal/storage"
)

// Update news: what changed in a release, for the people who use the bot (Discord) and for the
// operator (console). The notes are written at release time and compiled into the binary
// (internal/relnotes, release-notes/); this file reads them.
//
//	updates.notes      the notes of the running version, or of a named one, or the one before
//	updates.subscribe  a person's own opt-in to a DM when the bot moves to a new release
//	update_news        a room's own switch (channels.configure), default off
//
// Nobody is told anything unless they asked: the room's switch and the person's subscription both
// start off. The announcement itself is updatenews_announce.go.

func init() {
	// who was already told which version, one row per target (updatenews_announce.go)
	featureDDL = append(featureDDL, `CREATE TABLE IF NOT EXISTS update_news_sent (
		version TEXT NOT NULL,
		target TEXT NOT NULL,
		state TEXT NOT NULL,
		at TEXT NOT NULL,
		PRIMARY KEY(version, target)
	)`)
}

// migrateUpdateNews adds the subscription column to a user_prefs table made before it existed.
func migrateUpdateNews(st *storage.Store) error {
	if _, err := st.DB.Exec(`SELECT update_news FROM user_prefs LIMIT 0`); err != nil {
		if _, err := st.DB.Exec(`ALTER TABLE user_prefs ADD COLUMN update_news INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("feature tables: add user_prefs.update_news: %w", err)
		}
	}
	return nil
}

// newsReleaseTag is the running version as a release tag ("v1.5.1"), or false for a local build
// (…-dev), a build made after a tag, or a pre-release: none of those has notes of its own.
func newsReleaseTag(v string) (string, bool) {
	p, ok := relnotes.ParseVersion(v)
	if !ok || p.Pre != "" || p.After {
		return "", false
	}
	return fmt.Sprintf("v%d.%d.%d", p.Major, p.Minor, p.Patch), true
}

// runningNote is the note of the running version. A build made after a tag shows the last note up to
// that tag; a local build has none.
func runningNote() (relnotes.Note, bool) {
	if devBuild(Version) {
		return relnotes.Note{}, false
	}
	if n, ok := relnotes.For(Version); ok {
		return n, true
	}
	if all := relnotes.Between("", Version); len(all) > 0 {
		return all[len(all)-1], true
	}
	return relnotes.Note{}, false
}

// noteBefore is the newest note below n that has something for the audience.
func noteBefore(n relnotes.Note, a relnotes.Audience) (relnotes.Note, bool) {
	all := relnotes.All()
	for i := len(all) - 1; i >= 0; i-- {
		if relnotes.Compare(all[i].Version, n.Version) < 0 && all[i].Has(a) {
			return all[i], true
		}
	}
	return relnotes.Note{}, false
}

// updatesNotes is updates.notes. Discord callers read the user-facing notes; the console and CLI read
// both audiences unless the input asks for one (audience: user | operator | all).
func (s *Service) updatesNotes(req action.Request) action.Result {
	in := inputMap(req)
	aud := relnotes.AudienceAll
	if req.Invoker != nil && req.Invoker.UserID != "" {
		aud = relnotes.AudienceUser // what the operator sees in the console is not for a chat room
	} else {
		switch strVal(in, "audience", "") {
		case "user":
			aud = relnotes.AudienceUser
		case "operator":
			aud = relnotes.AudienceOperator
		}
	}
	lang := relnotes.Lang(req.Locale)
	if req.Locale == "" {
		lang = relnotes.Lang(s.Doc().I18n.DefaultLocale)
	}
	out := map[string]any{"running": Version, "audience": string(aud), "language": lang, "found": false}
	var note relnotes.Note
	var ok bool
	switch want := strings.TrimSpace(strVal(in, "version", "")); {
	case want != "":
		if note, ok = relnotes.For(want); !ok {
			return action.Fail(req, errcode.InvalidInput, "updates.news.unknown", s.msg(req, "updates.news.unknown"), nil)
		}
	default:
		note, ok = runningNote()
	}
	if ok && boolVal(in, "previous") {
		note, ok = noteBefore(note, aud)
	}
	title := s.msg(req, "updates.news.title")
	card := action.Card{Title: title, Tone: "neutral", Ephemeral: true}
	if !ok || !note.Has(aud) {
		card.Text = s.msg(req, "updates.news.none")
		return action.WithCard(action.OK(req, out), card)
	}
	prev, hasPrev := noteBefore(note, aud)
	out["found"] = true
	out["version"], out["date"] = note.Version, note.Date
	out["user"], out["operator"] = note.UserItems(lang), note.OperatorItems(lang)
	if aud == relnotes.AudienceUser {
		out["operator"] = []string{}
	}
	if aud == relnotes.AudienceOperator {
		out["user"] = []string{}
	}
	out["markdown"] = relnotes.Render(note, aud, lang)
	if hasPrev {
		out["previous"] = prev.Version
	}
	if running, isRelease := newsReleaseTag(Version); isRelease {
		out["is_running"] = note.Version == running
	}
	card.Title = s.msg(req, "updates.news.version_title", "version", note.Version)
	card.Text = relnotes.Card(note, aud, lang)
	if hasPrev {
		card.Footer = s.msg(req, "updates.news.footer", "version", prev.Version)
	}
	return action.WithCard(action.OK(req, out), card)
}

// updateNewsOn reports whether a person asked for a DM when the bot moves to a new release.
func (s *Service) updateNewsOn(userID string) bool { return s.userPrefs(userID).UpdateNews }

// setUpdateNews saves the subscription in the person's settings row, which goes away again when nothing
// else is set in it.
func (s *Service) setUpdateNews(userID string, on bool) error {
	db, err := s.adminDB()
	if err != nil {
		return err
	}
	p := s.userPrefs(userID)
	p.UpdateNews = on
	p.UpdatedAt = s.Clock.Now().UTC().Format(time.RFC3339)
	return db.WithWriter(func(tx *sql.Tx) error {
		if p == (UserPrefs{UserID: userID, UpdatedAt: p.UpdatedAt}) {
			_, err := tx.Exec(`DELETE FROM user_prefs WHERE user_id=?`, userID)
			return err
		}
		_, err := tx.Exec(`INSERT INTO user_prefs(user_id, locale, timezone, quiet_start, quiet_end, weather_region, update_news, updated_at) VALUES(?,?,?,?,?,?,?,?)
			ON CONFLICT(user_id) DO UPDATE SET update_news=excluded.update_news, updated_at=excluded.updated_at`,
			userID, p.Locale, p.Timezone, p.QuietStart, p.QuietEnd, p.WeatherRegion, p.UpdateNews, p.UpdatedAt)
		return err
	})
}

// updatesSubscribe is updates.subscribe: a person turns their own DM news on or off (op: on | off |
// status, or enabled: true | false). From Discord the person is the invoker, nobody else's; the console
// and CLI name the person with user_id.
func (s *Service) updatesSubscribe(req action.Request) action.Result {
	in := inputMap(req)
	userID := strVal(in, "user_id", "")
	if req.Invoker != nil && req.Invoker.UserID != "" {
		userID = req.Invoker.UserID
	}
	if userID == "" {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "user_id required", nil)
	}
	op := strVal(in, "op", "status")
	if v, ok := in["enabled"]; ok {
		op = "off"
		if b, _ := v.(bool); b {
			op = "on"
		}
	}
	changed := false
	switch op {
	case "on", "subscribe", "off", "unsubscribe":
		want := op == "on" || op == "subscribe"
		if s.updateNewsOn(userID) != want {
			if err := s.setUpdateNews(userID, want); err != nil {
				return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
			}
			changed = true
		}
	case "status":
	default:
		return action.Fail(req, errcode.InvalidInput, "updates.sub.bad_op", s.msg(req, "updates.sub.bad_op"), nil)
	}
	on := s.updateNewsOn(userID)
	state := s.msg(req, "updates.sub.state_off")
	if on {
		state = s.msg(req, "updates.sub.state_on")
	}
	card := action.Card{Title: s.msg(req, "updates.sub.title"), Text: state, Tone: "neutral", Ephemeral: true, Footer: s.msg(req, "updates.sub.help")}
	if changed {
		card.Tone = "ok"
	}
	return action.WithCard(action.OK(req, map[string]any{"enabled": on, "changed": changed}), card)
}
