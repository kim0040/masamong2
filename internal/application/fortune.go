package application

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"masamong/internal/action"
	"masamong/internal/errcode"
	"masamong/internal/features/fortune"
	"masamong/internal/features/privacy"
	"masamong/internal/hostprofile"
	"masamong/internal/lang"
	"masamong/internal/storage"
	"masamong/internal/tools"
)

// Fortune (19장 A21/A22, V1 cogs/fortune_cog.py): a personal fortune from a registered birthday, the
// twelve signs' fortunes and ranking, and a morning briefing by DM.
//
// What changed from V1: registering is one command (V1 asked three questions in a row); a slash
// command shows the full result privately in a server too (V1 sent people to DMs); the time a
// briefing arrives follows the person's own time zone and quiet hours (preferences.set); the briefing
// is written a few minutes early so it arrives on time; the hour pillar is used when a birth time is
// known. As in V1: a person's data is used only under the current fortune consent, a public channel
// only gets a three-line summary, and personal readings are limited per day including failed ones.
//
// Knobs (tools.fortune.query.defaults / tools.fortune.subscribe.defaults): daily_limit (3),
// zodiac_cooldown_sec (30), animals_cooldown_sec (60), hour_correction_minutes (30, fortune.query only;
// hourCorrection), lead_min (10), generation_attempts (3), send_attempts (5), max_tokens.

func init() {
	featureDDL = append(featureDDL,
		`CREATE TABLE IF NOT EXISTS fortune_profiles (
			user_id TEXT PRIMARY KEY,
			birth_date TEXT NOT NULL,
			birth_time TEXT NOT NULL DEFAULT '',
			gender TEXT NOT NULL DEFAULT '',
			birth_place TEXT NOT NULL DEFAULT '',
			display_name TEXT NOT NULL DEFAULT '',
			sub_time TEXT NOT NULL DEFAULT '',
			sub_active INTEGER NOT NULL DEFAULT 0,
			last_sent TEXT NOT NULL DEFAULT '',
			job TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS fortune_usage (
			user_id TEXT NOT NULL,
			day TEXT NOT NULL,
			n INTEGER NOT NULL,
			PRIMARY KEY(user_id, day)
		)`,
		`CREATE TABLE IF NOT EXISTS fortune_cache (
			cache_key TEXT PRIMARY KEY,
			content TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`)
}

type fortuneProfile struct {
	UserID, BirthDate, BirthTime, Gender, BirthPlace string
	DisplayName                                      string // how the briefing greets them
	SubTime                                          string
	SubActive                                        bool
	LastSent, Job                                    string
}

var dateRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// toolDefault reads an integer knob from tools.<tool>.defaults.
func (s *Service) toolDefault(tool, key string, def int) int {
	if v, ok := s.Doc().Tools[tool].Defaults[key]; ok {
		switch n := v.(type) {
		case int:
			return n
		case float64:
			return int(n)
		case string:
			if i, err := strconv.Atoi(n); err == nil {
				return i
			}
		}
	}
	return def
}

// catalogWords maps every loaded language's words for one value (key holds a comma list) to value.
func (s *Service) catalogWords(out map[string]string, key, value string) {
	for _, loc := range s.I18n.Locales() {
		if words, ok := s.I18n.Lookup(loc, key); ok {
			for _, w := range strings.Split(words, ",") {
				if w = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(w), " ", "")); w != "" {
					out[w] = value
				}
			}
		}
	}
}

func (s *Service) fortuneProfile(userID string) (fortuneProfile, bool) {
	db, err := s.adminDB()
	if err != nil {
		return fortuneProfile{}, false
	}
	p := fortuneProfile{UserID: userID}
	var active int
	err = db.DB.QueryRow(`SELECT birth_date, birth_time, gender, birth_place, display_name, sub_time, sub_active, last_sent, job FROM fortune_profiles WHERE user_id=?`, userID).
		Scan(&p.BirthDate, &p.BirthTime, &p.Gender, &p.BirthPlace, &p.DisplayName, &p.SubTime, &active, &p.LastSent, &p.Job)
	p.SubActive = active == 1
	return p, err == nil
}

// signOf parses a sign name (any language, fortune.sign.<i> and its .aliases) or number (1–12).
func (s *Service) signOf(text string) (int, bool) {
	key := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(text), " ", ""))
	if n, err := strconv.Atoi(key); err == nil && n >= 1 && n <= 12 {
		return n - 1, true
	}
	words := map[string]string{}
	for i := 0; i < fortune.Signs; i++ {
		s.catalogWords(words, fmt.Sprintf("fortune.sign.%d", i), strconv.Itoa(i))
		s.catalogWords(words, fmt.Sprintf("fortune.sign.%d.aliases", i), strconv.Itoa(i))
	}
	v, ok := words[key]
	if !ok {
		return 0, false
	}
	n, _ := strconv.Atoi(v)
	return n, true
}

func (s *Service) pillarText(loc string, p fortune.Pillar) string {
	return s.I18n.T(loc, fmt.Sprintf("fortune.stem.%d", p.Stem)) + s.I18n.T(loc, fmt.Sprintf("fortune.branch.%d", p.Branch))
}

func (s *Service) skyText(loc string, at time.Time) string {
	signs := fortune.SkySigns(at)
	parts := make([]string, 0, len(fortune.Bodies))
	for _, b := range fortune.Bodies {
		parts = append(parts, s.I18n.T(loc, "fortune.planet."+b)+" "+s.I18n.T(loc, fmt.Sprintf("fortune.sign.%d", signs[b])))
	}
	return strings.Join(parts, ", ")
}

func (s *Service) pillarsText(loc string, y, m, d, h fortune.Pillar, hasHour bool) string {
	text := s.I18n.T(loc, "fortune.data.pillars", "year", s.pillarText(loc, y), "month", s.pillarText(loc, m), "day", s.pillarText(loc, d))
	if hasHour {
		text += " " + s.I18n.T(loc, "fortune.data.hour_pillar", "hour", s.pillarText(loc, h))
	}
	return text
}

// validBirthDate is the one check of a birth date, for registration (parseProfile), a stored profile
// (fortuneChart) and a V1 import (migration.go): YYYY-MM-DD on the calendar, from 1900, not after today.
// Today is the date on now's own clock, so callers pass now in the birth zone (s.birthNow): a baby born
// this morning in Seoul is not "in the future" until 09:00 UTC. A V1 profile may carry a yearless
// "0000-05-15" or a "1990-02-30"; such a profile gets no chart.
func validBirthDate(v string, now time.Time) (time.Time, bool) {
	d, err := time.Parse("2006-01-02", v)
	y, m, day := now.Date()
	return d, err == nil && dateRE.MatchString(v) && d.Year() >= 1900 && !d.After(time.Date(y, m, day, 0, 0, 0, 0, time.UTC))
}

// birthNow is now on the birth zone's clock, for validBirthDate.
func (s *Service) birthNow() time.Time { return s.Clock.Now().In(s.birthZone()) }

// birthZone is the zone a birth date and time are read in: the instance's configured zone (i18n.timezone),
// else Asia/Seoul, as V1 read every birth as Korean time. It is not the person's display zone: a natal
// chart is fixed at birth, and moving one's notifications to another zone must not change it.
func (s *Service) birthZone() *time.Location {
	if z := s.Doc().I18n.Timezone; z != "" {
		if loc, err := time.LoadLocation(z); err == nil {
			return loc
		}
	}
	if loc, err := time.LoadLocation("Asia/Seoul"); err == nil {
		return loc
	}
	return time.FixedZone("KST", 9*3600) // a system without tzdata
}

// Uncertain natal pillars: no birth time, and a 절기 falls on the birth date (fortuneChart.uncertain).
const (
	natalCertain = iota
	natalMonthUncertain
	natalYearUncertain // 입춘: the year and the month
)

// fortuneChart is a person's chart for one reading, computed once and passed along: the natal pillars
// and, for a day's reading, the computed day (docs/plan-fortune.md §8).
type fortuneChart struct {
	natal     fortune.Natal
	valid     bool // the birth date is a real date from 1900 on; otherwise nothing is computed
	uncertain int  // natalCertain, natalMonthUncertain or natalYearUncertain
	// solar is the UTC offset of the local solar time the hour pillar was read in ("+8:30"), set only when
	// it differs from the offset of the birth's own wall clock (never without a birth time)
	solar string
	day   *fortune.Day
}

// hourCorrection is tools.fortune.query.defaults.hour_correction_minutes (-120…120): how far the birth
// zone's standard clock runs ahead of local solar time. The hour pillar of a natal chart is taken in
// solar time (fortune.NatalAt): with the Korean 만세력 convention of 30 minutes, 자시 is 23:30–01:30 on a
// Seoul clock. 0 reads the standard clock as it is (summer time is still taken off).
//
// Unset, it is 30 in Seoul's own zone (Asia/Seoul, Asia/Pyongyang), where the 30 minutes is Seoul's
// distance from its 135°E meridian, and 0 in any other zone, where that figure means nothing; a value the
// operator set is used in every zone. It is one setting for every reading, the briefings included: a chart
// is the same wherever it is shown.
func (s *Service) hourCorrection() int {
	def := 0
	if z := s.birthZone().String(); z == "Asia/Seoul" || z == "Asia/Pyongyang" {
		def = 30
	}
	return max(-120, min(120, s.toolDefault("fortune.query", "hour_correction_minutes", def)))
}

// utcOffset is a zone offset in seconds as "+8:30" or "-5:00" (the "UTC" is in the catalog line).
func utcOffset(sec int) string {
	sign := "+"
	if sec < 0 {
		sign, sec = "-", -sec
	}
	return fmt.Sprintf("%s%d:%02d", sign, sec/3600, sec%3600/60)
}

// fortuneChart reads the birth in birthZone. Without a birth time the year and month are taken at noon
// and there is no hour pillar; withDay adds the computed day of at's civil date (the same birth data and
// date always give the same scores and lucky points).
func (s *Service) fortuneChart(p fortuneProfile, at time.Time, withDay bool) fortuneChart {
	birth, ok := validBirthDate(p.BirthDate, s.birthNow())
	if !ok {
		return fortuneChart{}
	}
	zone := s.birthZone()
	h, m, has := 12, 0, false
	if t, err := time.Parse("15:04", p.BirthTime); err == nil {
		h, m, has = t.Hour(), t.Minute(), true
	}
	corr := s.hourCorrection()
	moment := fortune.BirthMoment(birth.Year(), birth.Month(), birth.Day(), h, m, zone)
	natal := fortune.NatalAt(moment, zone, has, time.Duration(corr)*time.Minute)
	c := fortuneChart{natal: natal, valid: true}
	if has {
		_, wall := moment.Zone()
		if _, solar := moment.In(fortune.SolarHourZone(zone, time.Duration(corr)*time.Minute)).Zone(); solar != wall {
			c.solar = utcOffset(solar)
		}
	}
	if !has {
		if branch, on := fortune.TermOn(birth.Year(), birth.Month(), birth.Day(), zone); on {
			c.uncertain = natalMonthUncertain
			if branch == 2 {
				c.uncertain = natalYearUncertain
			}
		}
	}
	if withDay {
		day := fortune.Analyze(c.natal, at)
		c.day = &day
	}
	return c
}

// fortuneData is the fact sheet a fortune is written from, in loc's words. A chart with a day adds the
// computed facts the reading must keep (a day's reading: today, detail, morning).
func (s *Service) fortuneData(loc string, p fortuneProfile, at time.Time, c fortuneChart) string {
	birth, _ := time.Parse("2006-01-02", p.BirthDate)
	unknown := s.I18n.T(loc, "fortune.data.not_given")
	orUnknown := func(v string) string {
		if v == "" {
			return unknown
		}
		return v
	}
	natal := unknown
	if c.valid {
		natal = s.pillarsText(loc, c.natal.Year, c.natal.Month, c.natal.Day, c.natal.Hour, c.natal.HasHour)
	}
	y, mo, d := at.Date()
	ty, tm, td := fortune.DatePillars(y, mo, d, at.Location())
	text := s.I18n.T(loc, "fortune.data.block",
		"date", at.Format("2006-01-02 15:04"),
		"birth", p.BirthDate, "birth_time", orUnknown(p.BirthTime), "place", orUnknown(p.BirthPlace),
		"natal", natal, "today", s.pillarsText(loc, ty, tm, td, fortune.Pillar{}, false),
		"sign", s.I18n.T(loc, fmt.Sprintf("fortune.sign.%d", fortune.SunSign(int(birth.Month()), birth.Day()))),
		"sky", s.skyText(loc, at))
	if c.solar != "" {
		text += "\n" + s.I18n.T(loc, "fortune.data.hour_corrected", "offset", c.solar)
	}
	switch c.uncertain {
	case natalMonthUncertain:
		text += "\n" + s.I18n.T(loc, "fortune.data.term_month")
	case natalYearUncertain:
		text += "\n" + s.I18n.T(loc, "fortune.data.term_year")
	}
	if c.day != nil {
		text += "\n\n" + s.factsText(loc, *c.day)
	}
	return text
}

// fortunePublicData is the fact sheet of a public channel's three-line summary: the date, the day's
// pillars, the sun sign and the sky. The birth date, time and place and the natal pillars stay out: the
// model's text is posted as written, and they are the person's own (the prompt asking it not to name them
// is not a guard).
func (s *Service) fortunePublicData(loc string, p fortuneProfile, at time.Time) string {
	birth, _ := time.Parse("2006-01-02", p.BirthDate)
	y, mo, d := at.Date()
	ty, tm, td := fortune.DatePillars(y, mo, d, at.Location())
	return s.I18n.T(loc, "fortune.data.public_block", "date", at.Format("2006-01-02 15:04"),
		"today", s.pillarsText(loc, ty, tm, td, fortune.Pillar{}, false),
		"sign", s.I18n.T(loc, fmt.Sprintf("fortune.sign.%d", fortune.SunSign(int(birth.Month()), birth.Day()))),
		"sky", s.skyText(loc, at))
}

func (s *Service) genderText(loc, g string) string {
	if g == "" {
		return s.I18n.T(loc, "fortune.data.not_given")
	}
	return s.I18n.T(loc, "fortune.gender."+g)
}

// fortuneRequest is the invoker of a personal fortune, with where the answer will be seen.
func fortuneInvoker(req action.Request) (userID string, private bool, ok bool) {
	if req.Invoker == nil || req.Invoker.UserID == "" {
		return "", false, false
	}
	return req.Invoker.UserID, req.Invoker.Private, true
}

// fortuneAllowed checks consent (showing the policy with its agree button when missing) and a profile.
func (s *Service) fortuneAllowed(req action.Request, userID string, needProfile bool) (fortuneProfile, *action.Result) {
	if !s.ConsentAllows(userID, privacy.Fortune) {
		res := s.consentNotice(req, userID, privacy.Fortune)
		c := res.Output["card"].(action.Card)
		c.Footer = s.msg(req, "fortune.consent_then")
		res = action.WithCard(res, c)
		res.Output["status"] = "consent_required"
		return fortuneProfile{}, &res
	}
	p, ok := s.fortuneProfile(userID)
	if needProfile && !ok {
		res := action.WithCard(action.OK(req, map[string]any{"status": "no_profile"}),
			action.Card{Title: s.msg(req, "fortune.title"), Text: s.msg(req, "fortune.register_first"), Tone: "neutral", Ephemeral: true})
		return p, &res
	}
	return p, nil
}

// reserveFortune counts one personal reading for today; false when the daily limit is used up.
func (s *Service) reserveFortune(userID string, now time.Time, limit int) (bool, int) {
	db, err := s.adminDB()
	if err != nil {
		return false, 0
	}
	day := now.In(s.instanceZone()).Format("2006-01-02")
	used, granted := 0, false
	err = db.WithWriter(func(tx *sql.Tx) error {
		_ = tx.QueryRow(`SELECT n FROM fortune_usage WHERE user_id=? AND day=?`, userID, day).Scan(&used)
		if limit > 0 && used >= limit {
			return nil
		}
		used++
		_, err := tx.Exec(`INSERT INTO fortune_usage(user_id, day, n) VALUES(?,?,1) ON CONFLICT(user_id, day) DO UPDATE SET n=n+1`, userID, day)
		granted = err == nil
		return err
	})
	return granted && err == nil, max(0, limit-used)
}

// zodiacCooldown remembers each person's last zodiac request. Only the cooldown (seconds) matters, so a day
// is more than enough to keep; the cap bounds a server whose members each ask once and never return.
const zodiacCooldownMax = 4096

var zodiacCooldown = newTTLCache[struct{}](zodiacCooldownMax, 24*time.Hour)

// A zodiac text is generated by one model call per key (day, sign, surface, language) at a time. The
// flight is registered under a short lock and the call runs outside it, so a cached key, another key and a
// request whose own deadline passed never wait behind somebody's model call. zodiacSlots bounds how many
// calls run at once: more would only queue at the provider on a small machine.
// zodiacMaxGenerations is the medium tier (today's cap). The channel is sized from the host
// profile on first use, after the daemon has installed it; tests that never install a profile
// still see this cap.
const zodiacMaxGenerations = 2

var (
	zodiacMu       sync.Mutex
	zodiacFlights  = map[zodiacFlightKey]*zodiacFlight{}
	zodiacSlotOnce sync.Once
	zodiacSlots    chan struct{}
)

func ensureZodiacSlots() chan struct{} {
	zodiacSlotOnce.Do(func() {
		n := hostprofile.Current().FortuneGenerations
		// WHY: a zero cap would block every generation. Medium, the unset profile, is two.
		if n < 1 {
			n = zodiacMaxGenerations
		}
		zodiacSlots = make(chan struct{}, n)
	})
	return zodiacSlots
}

// zodiacTestHooks let a test stop a request at the points where another one can slip in; they are nil
// outside tests.
var zodiacTestHooks struct {
	afterMiss  atomic.Pointer[func(key string)] // the cache had no text, the flight table is not looked at yet
	joined     atomic.Pointer[func(key string)] // a flight is running and this request is about to wait for it
	beforeSlot atomic.Pointer[func(key string)] // this request owns the flight and is about to wait for a slot
}

func zodiacHook(h *atomic.Pointer[func(key string)], key string) {
	if f := h.Load(); f != nil {
		(*f)(key)
	}
}

type zodiacFlightKey struct {
	svc *Service
	key string
}

type zodiacFlight struct {
	done chan struct{}
	text string
	err  error
}

// zodiacText returns the cached text of key, generating it when nobody has: cached reports whether this
// request read it rather than made it. A waiter gives up with its own context; if the request that was
// generating failed only because its own context ended, the waiter takes over instead of failing too.
func (s *Service) zodiacText(ctx context.Context, db *storage.Store, key string, generate func(context.Context) (string, error)) (text string, cached bool, err error) {
	fk := zodiacFlightKey{svc: s, key: key}
	for {
		if text, ok := zodiacCached(ctx, db, key); ok {
			return text, true, nil
		}
		if err := ctx.Err(); err != nil {
			return "", false, err
		}
		zodiacHook(&zodiacTestHooks.afterMiss, key)
		zodiacMu.Lock()
		f, running := zodiacFlights[fk]
		if !running {
			f = &zodiacFlight{done: make(chan struct{})}
			zodiacFlights[fk] = f
		}
		zodiacMu.Unlock()
		if running {
			zodiacHook(&zodiacTestHooks.joined, key)
			select {
			case <-f.done:
			case <-ctx.Done():
				return "", false, ctx.Err()
			}
			if f.err == nil {
				return f.text, true, nil
			}
			if ctx.Err() == nil && (errors.Is(f.err, context.Canceled) || errors.Is(f.err, context.DeadlineExceeded)) {
				continue // the generating request ran out of time, this one has not
			}
			return "", false, f.err
		}
		f.err = errors.New("fortune generation did not finish") // what waiters see if generate panics
		defer func() {
			zodiacMu.Lock()
			delete(zodiacFlights, fk)
			zodiacMu.Unlock()
			close(f.done)
		}()
		f.text, cached, f.err = s.generateZodiac(ctx, db, key, generate)
		return f.text, cached, f.err
	}
}

func zodiacCached(ctx context.Context, db *storage.Store, key string) (text string, ok bool) {
	return text, db.DB.QueryRowContext(ctx, `SELECT content FROM fortune_cache WHERE cache_key=?`, key).Scan(&text) == nil
}

// generateZodiac runs with the flight of key. The text may have been stored since this request looked (the
// request that was generating it finished and left the flight table between this one's cache miss and its
// turn on the table lock) or while it waited for a slot, so the cache is read again before the model is
// called: a second call would cost money and return a text different from the one everyone already has.
func (s *Service) generateZodiac(ctx context.Context, db *storage.Store, key string, generate func(context.Context) (string, error)) (text string, cached bool, err error) {
	if text, ok := zodiacCached(ctx, db, key); ok {
		return text, true, nil
	}
	zodiacHook(&zodiacTestHooks.beforeSlot, key)
	slots := ensureZodiacSlots()
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	case <-ctx.Done():
		return "", false, ctx.Err()
	}
	if text, ok := zodiacCached(ctx, db, key); ok {
		return text, true, nil
	}
	text, err = generate(ctx)
	if err != nil {
		return "", false, err
	}
	_, _ = db.DB.Exec(`INSERT INTO fortune_cache(cache_key, content, created_at) VALUES(?,?,?) ON CONFLICT(cache_key) DO NOTHING`, key, text, s.Clock.Now().UTC().Format(time.RFC3339))
	// yesterday's entries are never read again
	_, _ = db.DB.Exec(`DELETE FROM fortune_cache WHERE created_at < ?`, s.Clock.Now().UTC().AddDate(0, 0, -2).Format(time.RFC3339))
	return text, false, nil
}

func (s *Service) fortuneQuery(ctx context.Context, req action.Request) action.Result {
	in := inputMap(req)
	what := strVal(in, "what", "today")
	switch what {
	case "zodiac", "ranking":
		return s.zodiac(ctx, req, in, what == "ranking")
	case "animals":
		return s.animals(req)
	case "today", "detail", "month", "year":
	default:
		return action.Fail(req, errcode.InvalidInput, "generic.error", s.msg(req, "generic.error"), nil)
	}
	userID, private, ok := fortuneInvoker(req)
	if !ok {
		return action.Fail(req, errcode.InvalidInput, "fortune.discord_only", s.msg(req, "fortune.discord_only"), nil)
	}
	title := s.msg(req, "fortune.title_"+what)
	if !private && what != "today" {
		// personal readings are only shown where only the person sees them
		return action.WithCard(action.OK(req, map[string]any{"status": "private_only"}),
			action.Card{Title: title, Text: s.msg(req, "fortune.private_only"), Tone: "neutral"})
	}
	p, refused := s.fortuneAllowed(req, userID, true)
	if refused != nil {
		return *refused
	}
	now := s.Clock.Now().In(s.userLocation(s.userPrefs(userID)))
	okQuota, left := s.reserveFortune(userID, now, s.toolDefault("fortune.query", "daily_limit", 3))
	if !okQuota {
		return action.WithCard(action.OK(req, map[string]any{"status": "limited"}),
			action.Card{Title: title, Text: s.msg(req, "fortune.limited"), Tone: "warn", Ephemeral: true})
	}
	loc := req.Locale
	pack := s.packFor(loc)
	name := req.Invoker.Name
	if name == "" {
		name = s.msg(req, "fortune.someone")
	}
	birth, _ := time.Parse("2006-01-02", p.BirthDate)
	// a day's reading in private gets computed scores and lucky points (month and year are the model's).
	// A public channel's three-line summary gets only the sun sign and the day (fortunePublicData).
	chart := s.fortuneChart(p, now, private && (what == "today" || what == "detail"))
	data := s.fortunePublicData(loc, p, now)
	if private {
		data = s.fortuneData(loc, p, now, chart)
	}
	vars := []string{"data", data, "name", name, "gender", s.genderText(loc, p.Gender),
		"sign", s.I18n.T(loc, fmt.Sprintf("fortune.sign.%d", fortune.SunSign(int(birth.Month()), birth.Day())))}
	if !private {
		// the text is posted as written: the registered gender stays out of the prompt (fortune_public has no
		// {gender}); the sign is in the data block already
		vars = []string{"data", data, "name", name}
	}
	tpl := map[string]string{"today": pack.Features.FortuneToday, "detail": pack.Features.FortuneDetailDay,
		"month": pack.Features.FortuneDetailMonth, "year": pack.Features.FortuneDetailYear}[what]
	if !private {
		tpl = pack.Features.FortunePublic
	}
	tokens := 700
	if what != "today" {
		tokens = 1800
	}
	text, cost, err := s.featureText(ctx, "response", "fortune", lang.Fill(pack.Features.FortuneSystem, "bot", s.Doc().BotNames()[0]),
		lang.Fill(tpl, vars...), s.toolDefault("fortune.query", "max_tokens", tokens), 0.8)
	if err != nil {
		return s.featureFail(req, err)
	}
	// the answer is only shown if the person still agrees (they may have withdrawn meanwhile)
	if !s.ConsentAllows(userID, privacy.Fortune) {
		return action.Fail(req, errcode.PermissionDenied, "fortune.withdrawn", s.msg(req, "fortune.withdrawn"), nil)
	}
	// a public channel gets the three-line summary as written, without the person's own count of readings
	card := action.Card{Title: title, Text: text, Footer: s.msg(req, "fortune.more_private"), Tone: "fortune"}
	if private {
		footer := s.msg(req, "fortune.left", "n", left)
		card.Footer, card.Ephemeral = footer, true
		head := s.msg(req, "fortune.card.head", "name", name, "date", s.dayLabel(loc, now))
		card = s.fortuneCardFor(loc, what, card, head, s.fortuneBasis(loc, p, now)+" · "+footer, chart.day)
	}
	out := map[string]any{"status": "ok", "what": what, "text": text, "cost_usd": cost, "left_today": left}
	if chart.day != nil {
		// the computed points, not the model's: its own SCORE and LUCKY_* lines are left out of the text
		out["text"] = fortune.WithoutKeys(text, computedKeys...)
		out["scores"], out["lucky"] = s.computedOutput(loc, *chart.day)
	}
	return action.WithCard(action.OK(req, out), card)
}

// packFor is the language pack of a locale (the instance's languages as fallback).
func (s *Service) packFor(loc string) *lang.Pack {
	return lang.For(append([]string{loc}, append(append([]string{}, s.Doc().Memory.Languages...), s.Doc().I18n.DefaultLocale)...)...)
}

// featureFail turns a model failure of a feature into its result.
func (s *Service) featureFail(req action.Request, err error) action.Result {
	if errors.Is(err, ErrChatBudget) {
		return action.Fail(req, errcode.BudgetExceeded, "summary.budget", s.msg(req, "summary.budget"), nil)
	}
	return action.Fail(req, errcode.ProviderError, "feature.model_failed", s.msg(req, "feature.model_failed"), map[string]any{"error": err.Error()})
}

// zodiac is one sign's fortune for today, or the twelve signs ranked; both are shared by everyone and
// cached per day, so asking again costs nothing.
func (s *Service) zodiac(ctx context.Context, req action.Request, in map[string]any, ranking bool) action.Result {
	loc := req.Locale
	now := s.Clock.Now().In(s.instanceZone())
	private := req.Invoker == nil || req.Invoker.Private
	sign := -1
	if raw := strings.TrimSpace(strVal(in, "sign", "")); raw != "" && !ranking {
		n, ok := s.signOf(raw)
		if !ok {
			return action.Fail(req, errcode.InvalidInput, "fortune.bad_sign", s.msg(req, "fortune.bad_sign", "sign", raw), nil)
		}
		sign = n
	}
	if !ranking && sign < 0 {
		// the person's own sign, from their registered birthday (under consent)
		userID, _, ok := fortuneInvoker(req)
		p, has := fortuneProfile{}, false
		if ok && s.ConsentAllows(userID, privacy.Fortune) {
			p, has = s.fortuneProfile(userID)
		}
		if !has {
			return action.WithCard(action.OK(req, map[string]any{"status": "choose_sign"}),
				action.Card{Title: s.msg(req, "fortune.zodiac_title"), Text: s.msg(req, "fortune.zodiac_help", "signs", s.signList(loc)), Tone: "neutral"})
		}
		b, _ := time.Parse("2006-01-02", p.BirthDate)
		sign = fortune.SunSign(int(b.Month()), b.Day())
	}
	if req.Invoker != nil && req.Invoker.UserID != "" {
		cd := time.Duration(s.toolDefault("fortune.query", "zodiac_cooldown_sec", 30)) * time.Second
		now := time.Now()
		if _, last, ok := zodiacCooldown.get(req.Invoker.UserID, now); ok && now.Sub(last) < cd {
			wait := int((cd - now.Sub(last)).Seconds()) + 1
			return action.Fail(req, errcode.ToolLimited, "tool.cooldown", s.msg(req, "tool.cooldown", "tool", "fortune", "seconds", wait), nil)
		}
		zodiacCooldown.put(req.Invoker.UserID, struct{}{}, now)
	}
	pack := s.packFor(loc)
	day := now.Format("2006-01-02")
	signName := ""
	var key, tpl, title string
	if ranking {
		key, tpl, title = "ranking|"+day+"|"+loc, lang.Fill(pack.Features.ZodiacRanking, "sky", s.skyText(loc, now), "date", day), s.msg(req, "fortune.ranking_title", "date", s.dayLabel(loc, now))
	} else {
		signName = s.I18n.T(loc, fmt.Sprintf("fortune.sign.%d", sign))
		surface := "public"
		t := pack.Features.ZodiacPublic
		if private {
			surface, t = "private", pack.Features.ZodiacPrivate
		}
		key = fmt.Sprintf("sign|%s|%d|%s|%s", day, sign, surface, loc)
		tpl, title = lang.Fill(t, "sky", s.skyText(loc, now), "sign", signName), s.msg(req, "fortune.sign_title", "sign", signName)
	}
	db, err := s.adminDB()
	if err != nil {
		s.Log.Error("fortune: database unavailable", "err", err)
		return action.Fail(req, errcode.IO, "generic.error", s.msg(req, "generic.error"), nil)
	}
	text, cached, err := s.zodiacText(ctx, db, key, func(ctx context.Context) (string, error) {
		t, _, err := s.featureText(ctx, "response", "fortune", lang.Fill(pack.Features.ZodiacSystem, "bot", s.Doc().BotNames()[0]), tpl,
			s.toolDefault("fortune.query", "max_tokens", 1400), 0.8)
		return t, err
	})
	if err != nil {
		return s.featureFail(req, err)
	}
	footer := s.msg(req, "fortune.sky_at", "time", now.Format("2006-01-02 15:04"))
	if !private && !ranking {
		footer = s.msg(req, "fortune.more_private") + " · " + footer
	}
	card := action.Card{Title: title, Text: text, Footer: footer, Tone: "fortune"}
	switch {
	case ranking:
		card = s.rankingCard(loc, card)
	case private:
		card = s.fortuneCard(loc, "sign", card, s.dayLabel(loc, now), footer)
	}
	return action.WithCard(action.OK(req, map[string]any{"status": "ok", "sign": signName, "ranking": ranking, "cached": cached, "text": text}), card)
}

// animals is today's twelve-animal (띠) reading: the same for everyone, so it needs no profile, no
// consent and no model call. Each animal is scored by its branch's relation with today's day branch
// (fortune.AnimalScores, docs/plan-fortune.md §8) on the instance's local date, and gets one catalog
// phrase by its score band.
//
// In a public channel the card is posted once per animals_cooldown_sec (60) per channel: a second ask
// within it gets a short note that only the asker sees (where the connector can show it so) instead of
// a second fourteen-line card.
func (s *Service) animals(req action.Request) action.Result {
	loc := req.Locale
	now := s.Clock.Now().In(s.instanceZone())
	if inv := req.Invoker; inv != nil && !inv.Private && inv.ChannelID != "" {
		cd := min(time.Duration(s.toolDefault("fortune.query", "animals_cooldown_sec", 60))*time.Second, time.Hour) // the cache keeps an hour
		// in memory: a restart forgets it, which at worst posts the card once more
		s.animalsOnce.Do(func() { s.animalsPosted = newTTLCache[struct{}](zodiacCooldownMax, time.Hour) })
		key := inv.GuildID + "\x00" + inv.ChannelID
		// check and set in one step: two asks in the same few milliseconds are two goroutines
		if last, first := s.animalsPosted.putUnlessWithin(key, struct{}{}, now, cd); !first {
			wait := int((cd - now.Sub(last)).Seconds()) + 1
			return action.WithCard(action.OK(req, map[string]any{"status": "recent", "what": "animals", "retry_in_sec": wait}),
				action.Card{Text: s.msg(req, "fortune.animals.recent", "seconds", wait), Tone: "neutral", Ephemeral: true})
		}
	}
	y, m, d := now.Date()
	_, _, day := fortune.DatePillars(y, m, d, now.Location())
	scores := fortune.AnimalScores(day.Branch)
	lines := make([]string, 0, 12)
	ranking := make([]map[string]any, 0, 12)
	for i, b := range fortune.AnimalRanking(day.Branch) {
		lines = append(lines, s.msg(req, "fortune.animals.line", "rank", i+1, "animal", s.msg(req, fmt.Sprintf("fortune.animal.%d", b)),
			"score", scores[b], "phrase", s.msg(req, "fortune.animals.band."+animalBand(scores[b]))))
		ranking = append(ranking, map[string]any{"branch": b, "score": scores[b]})
	}
	card := action.Card{Title: s.msg(req, "fortune.animals_title", "date", s.dayLabel(loc, now)),
		Text:   s.msg(req, "fortune.animals.head", "day", s.pillarText(loc, day)) + "\n\n" + strings.Join(lines, "\n"),
		Footer: s.msg(req, "fortune.animals.footer"), Tone: "fortune"}
	return action.WithCard(action.OK(req, map[string]any{"status": "ok", "what": "animals", "date": now.Format("2006-01-02"),
		"day": s.pillarText(loc, day), "ranking": ranking}), card)
}

// animalBand is the phrase band of an animal's score: high (a 육합 or 반합 day, 70 and up), good (the
// same branch, 61–69), plain (no relation, 56–60) or low (a clash, punishment, harm or break, 55 and below).
func animalBand(score int) string {
	switch {
	case score >= 70:
		return "high"
	case score > 60:
		return "good"
	case score > 55:
		return "plain"
	}
	return "low"
}

func (s *Service) signList(loc string) string {
	names := make([]string, fortune.Signs)
	for i := range names {
		names[i] = s.I18n.T(loc, fmt.Sprintf("fortune.sign.%d", i))
	}
	return strings.Join(names, ", ")
}

// ---------------------------------------------------------------------------
// fortune.subscribe: profile and morning briefing
// ---------------------------------------------------------------------------

func (s *Service) fortuneSubscribe(req action.Request) action.Result {
	in := inputMap(req)
	op := strVal(in, "op", "status")
	userID, private, fromDiscord := fortuneInvoker(req)
	if !fromDiscord {
		return s.fortuneConsole(req, in, op)
	}
	title := s.msg(req, "fortune.title")
	if !private {
		return action.WithCard(action.OK(req, map[string]any{"status": "private_only"}),
			action.Card{Title: title, Text: s.msg(req, "fortune.private_only"), Tone: "neutral"})
	}
	switch op {
	case "delete":
		n := s.deleteFortune(userID)
		_, _ = s.setConsent(userID, privacy.Fortune, privacy.Withdrawn, "discord")
		return action.WithCard(action.OK(req, map[string]any{"status": "deleted", "deleted": n > 0}),
			action.Card{Title: title, Text: s.msg(req, "fortune.deleted"), Tone: "neutral", Ephemeral: true})
	case "unsubscribe":
		if db, err := s.adminDB(); err == nil {
			_, _ = db.DB.Exec(`UPDATE fortune_profiles SET sub_active=0, job='', updated_at=? WHERE user_id=?`, s.Clock.Now().UTC().Format(time.RFC3339), userID)
		}
		return action.WithCard(action.OK(req, map[string]any{"status": "unsubscribed"}),
			action.Card{Title: title, Text: s.msg(req, "fortune.unsubscribed"), Tone: "neutral", Ephemeral: true})
	}
	needProfile := op == "subscribe" || op == "status"
	p, refused := s.fortuneAllowed(req, userID, needProfile)
	if refused != nil {
		return *refused
	}
	now := s.Clock.Now().UTC().Format(time.RFC3339)
	db, err := s.adminDB()
	if err != nil {
		s.Log.Error("fortune: database unavailable", "err", err)
		return action.Fail(req, errcode.IO, "generic.error", s.msg(req, "generic.error"), nil)
	}
	switch op {
	case "register":
		np, bad := s.parseProfile(req, in)
		if bad != "" {
			return action.Fail(req, errcode.InvalidInput, "fortune.bad_profile", s.msg(req, "fortune.bad_profile"), nil)
		}
		_, err := db.DB.Exec(`INSERT INTO fortune_profiles(user_id, birth_date, birth_time, gender, birth_place, display_name, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?)
			ON CONFLICT(user_id) DO UPDATE SET birth_date=excluded.birth_date, birth_time=excluded.birth_time, gender=excluded.gender,
			birth_place=excluded.birth_place, display_name=excluded.display_name, job='', updated_at=excluded.updated_at`,
			userID, np.BirthDate, np.BirthTime, np.Gender, np.BirthPlace, clip(req.Invoker.Name, 64), now, now)
		if err != nil {
			s.Log.Error("fortune: profile save failed", "err", err)
			return action.Fail(req, errcode.IO, "generic.error", s.msg(req, "generic.error"), nil)
		}
		p, _ = s.fortuneProfile(userID)
		c := s.profileCard(req, p)
		c.Title, c.Tone = s.msg(req, "fortune.registered"), "ok"
		return action.WithCard(action.OK(req, map[string]any{"status": "registered"}), c)
	case "subscribe":
		at := strings.TrimSpace(strVal(in, "time", ""))
		if !hhmm.MatchString(at) {
			return action.Fail(req, errcode.InvalidInput, "fortune.bad_time", s.msg(req, "fortune.bad_time"), nil)
		}
		t, _ := time.Parse("15:04", at)
		at = t.Format("15:04")
		if _, err := db.DB.Exec(`UPDATE fortune_profiles SET sub_time=?, sub_active=1, job='', updated_at=? WHERE user_id=?`, at, now, userID); err != nil {
			s.Log.Error("fortune: subscription save failed", "err", err)
			return action.Fail(req, errcode.IO, "generic.error", s.msg(req, "generic.error"), nil)
		}
		p, _ = s.fortuneProfile(userID)
		c := s.profileCard(req, p)
		c.Title, c.Tone = s.msg(req, "fortune.subscribed", "time", at), "ok"
		if !s.Doc().Discord.Enabled {
			c.Text = s.msg(req, "fortune.discord_off")
		}
		return action.WithCard(action.OK(req, map[string]any{"status": "subscribed", "time": at}), c)
	case "status":
		return action.WithCard(action.OK(req, map[string]any{"status": "ok", "subscribed": p.SubActive, "time": p.SubTime}), s.profileCard(req, p))
	}
	return action.Fail(req, errcode.InvalidInput, "generic.error", s.msg(req, "generic.error"), nil)
}

func (s *Service) profileCard(req action.Request, p fortuneProfile) action.Card {
	loc := req.Locale
	unknown := s.I18n.T(loc, "fortune.data.not_given")
	or := func(v string) string {
		if v == "" {
			return unknown
		}
		return v
	}
	sub := s.msg(req, "fortune.sub_off")
	if p.SubActive {
		sub = s.msg(req, "fortune.sub_on", "time", p.SubTime, "zone", s.userLocation(s.userPrefs(p.UserID)).String())
	}
	b, _ := time.Parse("2006-01-02", p.BirthDate)
	// a value that fits a column sits in one; a longer one (a birthplace) gets the full width. The commands
	// go in the text, where Discord shows them as code (a footer shows the backticks as they are).
	field := func(name, value string) action.CardField {
		return action.CardField{Name: name, Value: value, Inline: utf8.RuneCountInString(value) <= 16}
	}
	return action.Card{Title: s.msg(req, "fortune.profile_title"), Tone: "neutral", Ephemeral: true, Text: s.msg(req, "fortune.profile_help"),
		Fields: []action.CardField{
			field(s.msg(req, "fortune.field.birth"), p.BirthDate+" "+or(p.BirthTime)),
			field(s.msg(req, "fortune.field.sign"), s.I18n.T(loc, fmt.Sprintf("fortune.sign.%d", fortune.SunSign(int(b.Month()), b.Day())))),
			field(s.msg(req, "fortune.field.gender"), s.genderText(loc, p.Gender)),
			field(s.msg(req, "fortune.field.place"), or(p.BirthPlace)),
			{Name: s.msg(req, "fortune.field.subscription"), Value: sub},
		}}
}

// parseProfile validates registration input; the second value is the reason when it is not valid.
func (s *Service) parseProfile(req action.Request, in map[string]any) (fortuneProfile, string) {
	if text := strings.TrimSpace(strVal(in, "text", "")); text != "" && strVal(in, "birth_date", "") == "" {
		// "!운세 등록 1990-01-01 14:30 남 서울": date, then optionally a time, a gender and a place
		in = s.splitProfileText(text)
	}
	p := fortuneProfile{BirthDate: strings.TrimSpace(strVal(in, "birth_date", ""))}
	p.BirthDate = strings.NewReplacer(".", "-", "/", "-").Replace(p.BirthDate)
	if len(p.BirthDate) == 8 && !strings.Contains(p.BirthDate, "-") { // 19900101
		p.BirthDate = p.BirthDate[:4] + "-" + p.BirthDate[4:6] + "-" + p.BirthDate[6:]
	}
	if _, ok := validBirthDate(p.BirthDate, s.birthNow()); !ok {
		return p, s.msg(req, "fortune.bad_date")
	}
	unknown := map[string]string{}
	s.catalogWords(unknown, "fortune.words.unknown", "unknown")
	if t := strings.TrimSpace(strVal(in, "birth_time", "")); t != "" && unknown[strings.ToLower(strings.ReplaceAll(t, " ", ""))] == "" {
		if !hhmm.MatchString(t) {
			return p, s.msg(req, "fortune.bad_birth_time")
		}
		tt, _ := time.Parse("15:04", t)
		p.BirthTime = tt.Format("15:04")
	}
	if g := strings.TrimSpace(strVal(in, "gender", "")); g != "" {
		words := map[string]string{"male": "male", "female": "female"}
		s.catalogWords(words, "fortune.gender.male.aliases", "male")
		s.catalogWords(words, "fortune.gender.female.aliases", "female")
		s.catalogWords(words, "fortune.words.unknown", "")
		v, ok := words[strings.ToLower(g)]
		if !ok {
			return p, s.msg(req, "fortune.bad_gender")
		}
		p.Gender = v
	}
	if place := strings.TrimSpace(strVal(in, "birth_place", "")); place != "" && unknown[strings.ToLower(strings.ReplaceAll(place, " ", ""))] == "" {
		p.BirthPlace = clip(place, 40)
	}
	return p, ""
}

func (s *Service) splitProfileText(text string) map[string]any {
	words := map[string]string{"male": "male", "female": "female"}
	s.catalogWords(words, "fortune.gender.male.aliases", "male")
	s.catalogWords(words, "fortune.gender.female.aliases", "female")
	unknown := map[string]string{}
	s.catalogWords(unknown, "fortune.words.unknown", "unknown")
	f := strings.Fields(text)
	out := map[string]any{"birth_date": f[0]}
	i := 1
	if i < len(f) && (hhmm.MatchString(f[i]) || unknown[strings.ToLower(f[i])] != "") {
		out["birth_time"] = f[i]
		i++
	}
	if i < len(f) {
		if _, ok := words[strings.ToLower(f[i])]; ok {
			out["gender"] = f[i]
			i++
		}
	}
	if i < len(f) {
		out["birth_place"] = strings.Join(f[i:], " ")
	}
	return out
}

// deleteFortune removes a person's profile, subscription, usage and pending briefing.
func (s *Service) deleteFortune(userID string) int64 {
	db, err := s.adminDB()
	if err != nil {
		return 0
	}
	var n int64
	_ = db.WithWriter(func(tx *sql.Tx) error {
		r, err := tx.Exec(`DELETE FROM fortune_profiles WHERE user_id=?`, userID)
		if err != nil {
			return err
		}
		n, _ = r.RowsAffected()
		_, err = tx.Exec(`DELETE FROM fortune_usage WHERE user_id=?`, userID)
		return err
	})
	return n
}

// fortuneConsole is what the console sees: subscriptions without birth data, and deletion on request.
func (s *Service) fortuneConsole(req action.Request, in map[string]any, op string) action.Result {
	db, err := s.adminDB()
	if err != nil {
		s.Log.Error("fortune: database unavailable", "err", err)
		return action.Fail(req, errcode.IO, "generic.error", s.msg(req, "generic.error"), nil)
	}
	if op == "delete" || op == "unsubscribe" {
		userID := strVal(in, "user_id", "")
		if userID == "" {
			return action.Fail(req, errcode.InvalidInput, "generic.error", s.msg(req, "generic.error"), nil)
		}
		if op == "delete" {
			n := s.deleteFortune(userID)
			return action.OK(req, map[string]any{"deleted": n > 0, "user_id": userID})
		}
		_, _ = db.DB.Exec(`UPDATE fortune_profiles SET sub_active=0, job='' WHERE user_id=?`, userID)
		return action.OK(req, map[string]any{"unsubscribed": true, "user_id": userID})
	}
	rows, err := db.DB.Query(`SELECT user_id, sub_time, sub_active, last_sent, job, updated_at FROM fortune_profiles ORDER BY updated_at DESC LIMIT 500`)
	if err != nil {
		s.Log.Error("fortune: subscription list failed", "err", err)
		return action.Fail(req, errcode.IO, "generic.error", s.msg(req, "generic.error"), nil)
	}
	list := []map[string]any{}
	for rows.Next() {
		var user, at, last, job, updated string
		var active int
		if rows.Scan(&user, &at, &active, &last, &job, &updated) != nil {
			continue
		}
		var j morningJob
		_ = json.Unmarshal([]byte(job), &j)
		list = append(list, map[string]any{"user_id": user, "time": at, "active": active == 1, "last_sent": last,
			"job_state": j.State, "job_error": j.Error, "updated_at": updated})
	}
	rows.Close()
	// consent is looked up after the rows are read: SQLite has one connection and it is busy until then
	for _, p := range list {
		p["consent"] = s.ConsentAllows(p["user_id"].(string), privacy.Fortune)
	}
	return action.OK(req, map[string]any{"profiles": list})
}

// ---------------------------------------------------------------------------
// Morning briefings
// ---------------------------------------------------------------------------

// morningJob is one day's briefing for one person: written before its time, then sent, with retries
// that never write it twice (the text is kept once written).
type morningJob struct {
	Date      string `json:"date"`
	State     string `json:"state"` // written | sending | sent | failed | unknown
	Content   string `json:"content,omitempty"`
	Generated int    `json:"generated"` // attempts to write it
	Sends     int    `json:"sends"`     // attempts to send it
	NextAt    string `json:"next_at,omitempty"`
	Error     string `json:"error,omitempty"`
	// Slot and Target are what the text was written for (briefingSlot of the planned delivery moment, and
	// the moment its reading is for, RFC 3339). A send in another slot writes it again once (Rewritten).
	Slot      string `json:"slot,omitempty"`
	Target    string `json:"target,omitempty"`
	Rewritten bool   `json:"rewritten,omitempty"`
}

// targetDate is the local date the job's reading is for ("" before it has one).
func (j morningJob) targetDate(zone *time.Location) string {
	t, err := time.Parse(time.RFC3339, j.Target)
	if err != nil {
		return ""
	}
	return t.In(zone).Format("2006-01-02")
}

var morningRunning atomic.Bool

// runFortuneMorning writes and sends the briefings that are due (the scheduler calls it every minute;
// a run still going is not started twice).
func (s *Service) runFortuneMorning(ctx context.Context) {
	if !morningRunning.CompareAndSwap(false, true) {
		return
	}
	defer morningRunning.Store(false)
	sender, ok := s.Discord.(DiscordSender)
	if s.Discord == nil || !ok || !s.Discord.Connected() {
		return
	}
	if r, found := tools.Find(s.toolRegistry(), "fortune.subscribe"); found && !r.Enabled {
		return
	}
	db, err := s.adminDB()
	if err != nil {
		return
	}
	rows, err := db.DB.Query(`SELECT user_id FROM fortune_profiles WHERE sub_active=1 AND sub_time<>''`)
	if err != nil {
		return
	}
	var users []string
	for rows.Next() {
		var u string
		if rows.Scan(&u) == nil {
			users = append(users, u)
		}
	}
	rows.Close()
	for _, u := range users {
		if ctx.Err() != nil {
			return
		}
		s.morningFor(ctx, sender, u)
	}
}

// briefingSlot is the wording slot of a briefing delivered at the local moment at, and the moment its
// reading is for. The moment is the planned delivery (the subscription time, or the end of quiet hours
// that hold it; writing it lead_min early does not count) or, when a send is late, the actual one:
//   - 04:00–10:59 morning: today's reading, a morning greeting ("아침 운세");
//   - 11:00–16:59 day: today's reading, a neutral daytime greeting ("오늘의 운세");
//   - 17:00–23:59 evening: TOMORROW's reading, as commercial apps do for an evening send: the computed
//     facts, scores and lucky points are the next local date's ("내일 운세", dated tomorrow);
//   - 00:00–03:59 night: the coming day, which is the same calendar date, with a night greeting.
func briefingSlot(at time.Time) (slot string, target time.Time) {
	switch h := at.Hour(); {
	case h >= 4 && h < 11:
		return "morning", at
	case h >= 11 && h < 17:
		return "day", at
	case h >= 17:
		return "evening", at.AddDate(0, 0, 1)
	}
	return "night", at
}

// briefingTarget is the local date the briefing of now's own date is for: its slot is taken at the
// planned delivery, the subscription time on that date or the end of the quiet hours that hold it.
func (s *Service) briefingTarget(prefs UserPrefs, now, subTime time.Time) string {
	due := time.Date(now.Year(), now.Month(), now.Day(), subTime.Hour(), subTime.Minute(), 0, 0, now.Location())
	_, target := briefingSlot(s.quietUntil(prefs, due))
	return target.Format("2006-01-02")
}

// slotGrace is how far past a slot boundary a briefing planned before it still counts as being in its
// own slot: the scheduler ticks once a minute and takes subscribers one after another, each with a model
// call, so a 10:59 briefing can be sent at 11:02.
const slotGrace = 5 * time.Minute

// slotGraceHolds is true when a job whose slot looks changed at now is in fact a send planned inside its
// own slot (sendAt) and made less than slotGrace after it. It does not hold when the reading's date
// relative to the delivery changes: an evening briefing is tomorrow's reading, and past midnight it is
// today's (the night slot), which needs different words.
func slotGraceHolds(job morningJob, sendAt, now time.Time, zone *time.Location) bool {
	if d := now.Sub(sendAt); d < 0 || d >= slotGrace {
		return false
	}
	_, nowTarget := briefingSlot(now)
	plannedSlot, plannedTarget := briefingSlot(sendAt)
	return plannedSlot == job.Slot && job.Slot != "evening" && nowTarget.Format("2006-01-02") == job.targetDate(zone) &&
		plannedTarget.Format("2006-01-02") == job.targetDate(zone)
}

// briefingNeutral is the title and greeting slot of a briefing sent outside the slot it was written for
// when it cannot be written again: it names the date and claims no time of day.
const briefingNeutral = "neutral"

func (s *Service) morningFor(ctx context.Context, sender DiscordSender, userID string) {
	p, ok := s.fortuneProfile(userID)
	if !ok || !p.SubActive || !s.ConsentAllows(userID, privacy.Fortune) {
		return
	}
	prefs := s.userPrefs(userID)
	zone := s.userLocation(prefs)
	now := s.Clock.Now().In(zone)
	t, err := time.Parse("15:04", p.SubTime)
	if err != nil {
		return
	}
	var job morningJob
	_ = json.Unmarshal([]byte(p.Job), &job)
	today := now.Format("2006-01-02")
	date := today
	if job.Date != "" && job.Date != today && job.Date != p.LastSent && job.State != "sent" && job.State != "failed" && job.State != "unknown" {
		date = job.Date // an unsettled job keeps its original local subscription day after midnight.
	}
	slot, err := time.ParseInLocation("2006-01-02", date, zone)
	if err != nil {
		date, slot = today, time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, zone)
	}
	due := time.Date(slot.Year(), slot.Month(), slot.Day(), t.Hour(), t.Minute(), 0, 0, zone)
	if p.LastSent == date {
		return
	}
	lead := time.Duration(s.toolDefault("fortune.subscribe", "lead_min", 10)) * time.Minute
	if now.Before(due.Add(-lead)) {
		return
	}
	if job.Date != date {
		job = morningJob{Date: date}
	}
	if job.State == "sending" {
		job.State, job.NextAt = "unknown", ""
		job.Error = "delivery was interrupted before its outcome was recorded"
		raw, _ := json.Marshal(job)
		if db, err := s.adminDB(); err == nil {
			_, _ = db.DB.Exec(`UPDATE fortune_profiles SET job=? WHERE user_id=?`, string(raw), userID)
		}
		return
	}
	if job.State == "failed" || job.State == "unknown" || job.State == "sent" {
		return
	}
	if job.NextAt != "" {
		if next, err := time.Parse(time.RFC3339, job.NextAt); err == nil && now.Before(next) {
			return
		}
	}
	save := func() {
		raw, _ := json.Marshal(job)
		if db, err := s.adminDB(); err == nil {
			_, _ = db.DB.Exec(`UPDATE fortune_profiles SET job=? WHERE user_id=?`, string(raw), userID)
		}
	}
	retry := func(n int) string {
		return now.Add(time.Duration(1<<min(n, 5)) * time.Minute).UTC().Format(time.RFC3339)
	}
	loc := prefs.Locale
	if loc == "" {
		loc = s.Doc().I18n.DefaultLocale
	}
	genCap := s.toolDefault("fortune.subscribe", "generation_attempts", 3)
	plan := func(at time.Time) {
		slot, target := briefingSlot(at)
		job.Slot, job.Target = slot, target.Format(time.RFC3339)
	}
	sendAt := s.quietUntil(prefs, due) // the planned delivery: the due time, or the end of quiet hours
	if job.Slot == "" {
		if job.Content != "" {
			plan(due) // written before slots were stored: it was worded from the due time
		} else {
			plan(sendAt)
		}
	}
	name := p.DisplayName
	if name == "" {
		name = s.I18n.T(loc, "fortune.someone")
	}
	// write returns the text for a slot and the moment its reading is for; false when it could not (retried
	// later). It counts the attempt and saves that before the call, but never touches the stored Content,
	// Slot or Target: a process that stops mid-call leaves whatever text was written before it, which can
	// still go out. The caller swaps the new text in once it has it.
	write := func(slot string, target time.Time) (string, bool) {
		target = target.In(zone)
		job.Generated++
		job.NextAt = retry(job.Generated) // kept if the process stops mid-call
		save()
		pack := s.packFor(loc)
		day := target.Format("2006-01-02")
		chart := s.fortuneChart(p, target, true)
		prompt := lang.Fill(pack.Features.FortuneMorning, "data", s.fortuneData(loc, p, target, chart), "name", name, "gender", s.genderText(loc, p.Gender), "date", day,
			"task", s.I18n.T(loc, "fortune.briefing.task."+slot, "date", day), "greeting", s.I18n.T(loc, "fortune.briefing.greeting."+slot, "name", name))
		text, _, err := s.featureText(ctx, "response", "fortune", lang.Fill(pack.Features.FortuneSystem, "bot", s.Doc().BotNames()[0]), prompt,
			s.toolDefault("fortune.subscribe", "max_tokens", 900), 0.8)
		if err != nil {
			job.Error = clip(err.Error(), 200)
			save()
			s.Log.Warn("fortune: briefing not written", "attempt", job.Generated, "err", err)
			return "", false
		}
		return text, true
	}
	if job.Content == "" {
		if job.Generated >= genCap {
			job.State, job.NextAt = "failed", ""
			save()
			return
		}
		target, _ := time.Parse(time.RFC3339, job.Target)
		text, ok := write(job.Slot, target)
		if !ok {
			return
		}
		job.Content, job.State, job.Error, job.NextAt = text, "written", "", ""
		save()
	}
	if now.Before(sendAt) {
		return
	}
	if !s.ConsentAllows(userID, privacy.Fortune) {
		return
	}
	// a send in another slot than the text was written for (a catch-up after downtime, a retry past a
	// slot boundary) writes it again once for the moment it goes out; with the cap spent, or when that
	// one rewrite fails, it goes as it was written, under a title and greeting that claim no time of day
	// (the reading still holds for its own date)
	when := job.Slot
	nowSlot, nowTarget := briefingSlot(now)
	changed := nowSlot != job.Slot || nowTarget.Format("2006-01-02") != job.targetDate(zone)
	if changed && slotGraceHolds(job, sendAt, now, zone) {
		changed = false // a tick a minute or two past the boundary the briefing was planned before
	}
	if changed {
		if date != today && nowTarget.Format("2006-01-02") == s.briefingTarget(prefs, now, t) {
			// a day-old job would be rewritten for the date today's own briefing is for: today's goes instead
			job.State, job.NextAt, job.Error = "failed", "", "superseded by the next day's briefing"
			save()
			return
		}
		if !job.Rewritten && job.Generated < genCap {
			// the old text stays in the job until the new one is in hand: a failed call, or a process that
			// stops in the middle of it, leaves a briefing that goes out as it is, neutrally, at the next try
			job.Rewritten = true // saved with the attempt, so it is the one rewrite either way
			text, ok := write(nowSlot, nowTarget)
			if !ok {
				return
			}
			job.Content, job.Slot, job.Target, job.State, job.Error, job.NextAt = text, nowSlot, nowTarget.Format(time.RFC3339), "written", "", ""
			save()
			when = job.Slot
		} else {
			when = briefingNeutral
		}
	}
	if job.Sends >= s.toolDefault("fortune.subscribe", "send_attempts", 5) {
		job.State, job.NextAt = "failed", ""
		save()
		return
	}
	job.Sends++
	job.State, job.NextAt = "sending", ""
	save()
	target, _ := time.Parse(time.RFC3339, job.Target)
	target = target.In(zone)
	chart := s.fortuneChart(p, target, true)
	text := job.Content
	if when == briefingNeutral {
		text = fortune.WithoutKeys(text, "GREETING") // the card's head line (name and date) takes its place
	}
	basis := s.fortuneBasis(loc, p, target)
	card := action.Card{Title: s.I18n.T(loc, "fortune.briefing_title."+when, "date", s.dayLabel(loc, target)), Text: text, Footer: basis, Tone: "fortune",
		Buttons: []action.CardButton{{ID: "fortune:unsub", Label: s.I18n.T(loc, "fortune.card.unsubscribe"), Style: "secondary"}}}
	card = s.fortuneCardFor(loc, "morning", card, s.I18n.T(loc, "fortune.card.head", "name", name, "date", s.dayLabel(loc, target)), basis, chart.day)
	sendCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	err = sender.SendDM(sendCtx, userID, card, "")
	cancel()
	if err != nil {
		job.Error = clip(err.Error(), 200)
		switch {
		case errors.Is(err, ErrDMClosed):
			job.State, job.NextAt = "failed", ""
		case errors.Is(err, ErrNotDelivered):
			job.State, job.NextAt = "written", retry(job.Sends)
		default:
			job.State, job.NextAt = "unknown", ""
		}
		save()
		s.Log.Warn("fortune: briefing not sent", "attempt", job.Sends, "err", err)
		return
	}
	job.State, job.NextAt, job.Error = "sent", "", ""
	raw, _ := json.Marshal(job)
	if db, err := s.adminDB(); err == nil {
		_, _ = db.DB.Exec(`UPDATE fortune_profiles SET job=?, last_sent=? WHERE user_id=?`, string(raw), date, userID)
	}
}
