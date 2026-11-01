package application

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"masamong/internal/action"
	"masamong/internal/errcode"
	"masamong/internal/lang"
)

// preferences.set (19장 A35): a person's own settings, kept per Discord account.
//
//	locale          language of the bot's cards and DMs to them ("" = the channel's / Discord's)
//	timezone        IANA zone their subscription times are read in (default: the instance's)
//	quiet_start/end "HH:MM" window in which scheduled DMs wait until it ends
//	weather_region  used by the weather command when they name no place
//
// From Discord a person only reads and changes their own; the console can look anyone up and clear.

func init() {
	featureDDL = append(featureDDL, `CREATE TABLE IF NOT EXISTS user_prefs (
		user_id TEXT PRIMARY KEY,
		locale TEXT NOT NULL DEFAULT '',
		timezone TEXT NOT NULL DEFAULT '',
		quiet_start TEXT NOT NULL DEFAULT '',
		quiet_end TEXT NOT NULL DEFAULT '',
		weather_region TEXT NOT NULL DEFAULT '',
		updated_at TEXT NOT NULL
	)`)
}

// UserPrefs is one person's settings ("" = not set).
type UserPrefs struct {
	UserID        string `json:"user_id"`
	Locale        string `json:"locale"`
	Timezone      string `json:"timezone"`
	QuietStart    string `json:"quiet_start"`
	QuietEnd      string `json:"quiet_end"`
	WeatherRegion string `json:"weather_region"`
	UpdatedAt     string `json:"updated_at,omitempty"`
}

var hhmm = regexp.MustCompile(`^([01]?\d|2[0-3]):([0-5]\d)$`)

func (s *Service) userPrefs(userID string) UserPrefs {
	p := UserPrefs{UserID: userID}
	db, err := s.adminDB()
	if err != nil || userID == "" {
		return p
	}
	_ = db.DB.QueryRow(`SELECT locale, timezone, quiet_start, quiet_end, weather_region, updated_at FROM user_prefs WHERE user_id=?`, userID).
		Scan(&p.Locale, &p.Timezone, &p.QuietStart, &p.QuietEnd, &p.WeatherRegion, &p.UpdatedAt)
	return p
}

// Location is the zone the person's times are in.
func (s *Service) userLocation(p UserPrefs) *time.Location {
	if z := p.Timezone; z != "" {
		if loc, err := time.LoadLocation(z); err == nil {
			return loc
		} else {
			s.Log.Warn("preferences: unknown user timezone; using the instance timezone", "timezone", z, "err", err)
		}
	}
	return s.instanceZone()
}

// quietUntil returns when a DM due at t may go out: t itself, or the end of the person's quiet window.
func (s *Service) quietUntil(p UserPrefs, t time.Time) time.Time {
	if p.QuietStart == "" || p.QuietEnd == "" || p.QuietStart == p.QuietEnd {
		return t
	}
	loc := s.userLocation(p)
	lt := t.In(loc)
	at := func(v string, day time.Time) time.Time {
		m := hhmm.FindStringSubmatch(v)
		var h, mi int
		fmt.Sscanf(m[1], "%d", &h)
		fmt.Sscanf(m[2], "%d", &mi)
		return time.Date(day.Year(), day.Month(), day.Day(), h, mi, 0, 0, loc)
	}
	start, end := at(p.QuietStart, lt), at(p.QuietEnd, lt)
	if !start.After(end) { // same-day window, e.g. 13:00–14:00
		if !lt.Before(start) && lt.Before(end) {
			return end
		}
		return t
	}
	// overnight window, e.g. 23:00–07:00
	if !lt.Before(start) {
		return end.AddDate(0, 0, 1)
	}
	if lt.Before(end) {
		return end
	}
	return t
}

// applyUserDefaults fills a Discord request's missing inputs from the invoker's settings: their
// language for the reply, and their weather place.
func (s *Service) applyUserDefaults(req action.Request) action.Request {
	if req.Invoker == nil || req.Invoker.UserID == "" {
		return req
	}
	if p := s.userPrefs(req.Invoker.UserID); p.Locale != "" {
		req.Locale = p.Locale
	}
	if req.Action != "weather.query" {
		return req
	}
	in := inputMap(req)
	if strings.TrimSpace(strVal(in, "region", "")) != "" {
		return req
	}
	p := s.userPrefs(req.Invoker.UserID)
	if p.WeatherRegion == "" {
		return req
	}
	in["region"] = p.WeatherRegion
	in["region_source"] = "user" // the person's own place, not an assumption
	req.Input, _ = json.Marshal(in)
	return req
}

func (s *Service) preferencesAction(req action.Request) action.Result {
	in := inputMap(req)
	userID := strVal(in, "user_id", "")
	fromDiscord := req.Invoker != nil && req.Invoker.UserID != ""
	if fromDiscord {
		userID = req.Invoker.UserID
	}
	if userID == "" {
		return s.preferencesList(req)
	}
	p := s.userPrefs(userID)
	changed := false
	set := func(key string, dst *string, check func(string) error) error {
		v, ok := in[key]
		if !ok {
			return nil
		}
		val := strings.TrimSpace(fmt.Sprint(v))
		if v == nil {
			val = ""
		}
		if val != "" && check != nil {
			if err := check(val); err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
		}
		*dst, changed = val, true
		return nil
	}
	if boolVal(in, "reset") {
		p = UserPrefs{UserID: userID}
		changed = true
	}
	for _, f := range []struct {
		key   string
		dst   *string
		check func(string) error
	}{
		{"locale", &p.Locale, func(v string) error {
			if lang.Normalize(v) == "" {
				return errors.New("unknown language")
			}
			return nil
		}},
		{"timezone", &p.Timezone, func(v string) error { _, err := time.LoadLocation(v); return err }},
		{"quiet_start", &p.QuietStart, checkHHMM},
		{"quiet_end", &p.QuietEnd, checkHHMM},
		{"weather_region", &p.WeatherRegion, func(v string) error {
			if len([]rune(v)) > 40 {
				return errors.New("too long")
			}
			return nil
		}},
	} {
		if err := set(f.key, f.dst, f.check); err != nil {
			return action.Fail(req, errcode.InvalidInput, "prefs.invalid", s.msg(req, "prefs.invalid", "error", err.Error()), nil)
		}
	}
	if (p.QuietStart == "") != (p.QuietEnd == "") {
		return action.Fail(req, errcode.InvalidInput, "prefs.invalid", s.msg(req, "prefs.invalid", "error", "quiet hours need both a start and an end"), nil)
	}
	if changed {
		db, err := s.adminDB()
		if err != nil {
			return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
		}
		p.UpdatedAt = s.Clock.Now().UTC().Format(time.RFC3339)
		err = db.WithWriter(func(tx *sql.Tx) error {
			if p == (UserPrefs{UserID: userID, UpdatedAt: p.UpdatedAt}) {
				_, err := tx.Exec(`DELETE FROM user_prefs WHERE user_id=?`, userID)
				return err
			}
			_, err := tx.Exec(`INSERT INTO user_prefs(user_id, locale, timezone, quiet_start, quiet_end, weather_region, updated_at) VALUES(?,?,?,?,?,?,?)
				ON CONFLICT(user_id) DO UPDATE SET locale=excluded.locale, timezone=excluded.timezone, quiet_start=excluded.quiet_start,
				quiet_end=excluded.quiet_end, weather_region=excluded.weather_region, updated_at=excluded.updated_at`,
				userID, p.Locale, p.Timezone, p.QuietStart, p.QuietEnd, p.WeatherRegion, p.UpdatedAt)
			return err
		})
		if err != nil {
			return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
		}
	}
	unset := s.msg(req, "prefs.unset")
	or := func(v string) string {
		if v == "" {
			return unset
		}
		return v
	}
	quiet := unset
	if p.QuietStart != "" {
		quiet = p.QuietStart + "–" + p.QuietEnd
	}
	card := action.Card{Title: s.msg(req, "prefs.title"), Tone: "neutral", Ephemeral: true, Footer: s.msg(req, "prefs.help"), Fields: []action.CardField{
		{Name: s.msg(req, "prefs.weather_region"), Value: or(p.WeatherRegion), Inline: true},
		{Name: s.msg(req, "prefs.timezone"), Value: or(p.Timezone), Inline: true},
		{Name: s.msg(req, "prefs.quiet"), Value: quiet, Inline: true},
		{Name: s.msg(req, "prefs.locale"), Value: or(p.Locale), Inline: true},
	}}
	if changed {
		card.Title, card.Tone = s.msg(req, "prefs.saved"), "ok"
	}
	return action.WithCard(action.OK(req, map[string]any{"prefs": p, "changed": changed}), card)
}

func checkHHMM(v string) error {
	if !hhmm.MatchString(v) {
		return errors.New("use HH:MM")
	}
	return nil
}

// preferencesList is everyone's settings (console).
func (s *Service) preferencesList(req action.Request) action.Result {
	db, err := s.adminDB()
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	rows, err := db.DB.Query(`SELECT user_id, locale, timezone, quiet_start, quiet_end, weather_region, updated_at FROM user_prefs ORDER BY updated_at DESC LIMIT 500`)
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	defer rows.Close()
	list := []UserPrefs{}
	for rows.Next() {
		var p UserPrefs
		if rows.Scan(&p.UserID, &p.Locale, &p.Timezone, &p.QuietStart, &p.QuietEnd, &p.WeatherRegion, &p.UpdatedAt) == nil {
			list = append(list, p)
		}
	}
	return action.OK(req, map[string]any{"users": list})
}
