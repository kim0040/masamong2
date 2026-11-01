package application

import (
	"context"
	"fmt"
	"masamong/internal/errclass"
	"math"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"masamong/internal/action"
	"masamong/internal/config"
	"masamong/internal/providers/kma"
)

// Disaster alerts: the bot watches the Korea Meteorological Administration on its own and announces
// what it finds in the channels it answers in, as the previous bot did. Earthquakes are checked every
// minute; weather warnings every five.
//
//   - Earthquakes: notices KMA marks 국내영향없음 are skipped, then the magnitude floors
//     (disaster_alerts.earthquake_min_domestic / _overseas, 4.0 and 7.0). Events within 72 hours and
//     150 km of an incident's first event are one incident: its message in each channel is edited as
//     aftershocks come, a new incident gets a new message. A watermark (the newest event announced) is
//     kept in the database and advanced before sending, so a restart or a failed channel never
//     announces an event twice. The first check only sets the watermark: what was already published
//     before the bot started watching is not announced.
//   - Weather warnings: 경보 for every land region, 주의보 only for disaster_alerts.warning_regions, sea
//     areas only with disaster_alerts.marine. A warning that appears is announced as issued, one that
//     disappears as lifted; warnings of the same kind and level are one message. The first check only
//     records what is active.
//
// disaster_alerts.enabled / .warnings switch them off for the instance, a channel's disaster_alerts:
// off for that channel; the scheduler skips everything while the bot is paused.

const (
	eqkWindow      = 72 * time.Hour
	eqkRadiusKM    = 150.0
	eqkShownEvents = 6
	warningEvery   = 5 * time.Minute
)

var disasterTicking atomic.Bool

// disasterMem keeps when the watcher last ran in memory. A check that changed nothing writes nothing
// (each write is a commit and fsync, forever, on an SD card), so the console's "last checked" and the
// five-minute warning throttle cannot live in the database. After a restart the first tick checks
// warnings once more, which is harmless: the stored active set is what announcements compare with.
type disasterMem struct {
	mu                    sync.Mutex
	checkedAt, warningsAt time.Time
}

// disasterState is what the console shows (disasters.alerts) and what the next check compares with.
type disasterState struct {
	CheckedAt     string `json:"checked_at,omitempty"`
	WarningsAt    string `json:"warnings_checked_at,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	LastErrorAt   string `json:"last_error_at,omitempty"`
	LastSentAt    string `json:"last_sent_at,omitempty"`
	LastSentKind  string `json:"last_sent_kind,omitempty"`
	LastSentCount int    `json:"last_sent_channels,omitempty"`
}

// disasterChannel is one channel announcements go to, with its server's language.
type disasterChannel struct {
	Guild, Channel, Locale string
}

// disasterChannels are the channels the bot answers in on approved servers, minus those that turned
// announcements off.
func (s *Service) disasterChannels() []disasterChannel {
	doc := s.Doc()
	pause := s.pauseState()
	if pause.All {
		return nil
	}
	var out []disasterChannel
	for gid, g := range doc.Guilds {
		if !s.GuildAllowed(gid) {
			continue
		}
		loc := g.Locale
		if loc == "" {
			loc = doc.I18n.DefaultLocale
		}
		for cid, c := range g.Channels {
			if c.Respond == "off" || c.DisasterAlerts == "off" || pause.Channels[gid+":"+cid] {
				continue
			}
			out = append(out, disasterChannel{Guild: gid, Channel: cid, Locale: loc})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Guild+out[i].Channel < out[j].Guild+out[j].Channel })
	return out
}

// disasterTick is one scheduler minute: earthquakes, and warnings when five minutes have passed.
func (s *Service) disasterTick(ctx context.Context) {
	if s.practice || s.Store == nil || !s.Doc().DisasterAlerts.On() {
		return
	}
	r := s.Registry()
	if r == nil || r.KMA == nil || !r.KMA.Configured() || s.Discord == nil || !s.Discord.Connected() {
		return
	}
	poster, ok := s.Discord.(ChannelPoster)
	if !ok {
		return
	}
	channels := s.disasterChannels()
	if len(channels) == 0 {
		return
	}
	var st disasterState
	_, _ = s.kvGet("disaster:state", &st)
	now := s.Clock.Now()
	checkedAt := now.UTC().Format(time.RFC3339)
	dirty := false
	fail := func(err error) {
		if st.LastError != err.Error() {
			st.LastError, st.LastErrorAt = err.Error(), checkedAt
			dirty = true
		}
		s.Log.Warn("disaster alerts: check failed", "err", err)
	}
	if n, err := s.earthquakeTick(ctx, poster, channels); err != nil {
		fail(err)
	} else if n > 0 {
		st.LastSentAt, st.LastSentKind, st.LastSentCount = checkedAt, "earthquake", n
		dirty = true
	}
	s.disaster.mu.Lock()
	s.disaster.checkedAt = now.UTC()
	last := s.disaster.warningsAt
	due := s.Doc().DisasterAlerts.WarningsOn() && now.Sub(last) >= warningEvery-5*time.Second
	if due {
		s.disaster.warningsAt = now.UTC()
	}
	s.disaster.mu.Unlock()
	if due {
		if n, err := s.warningTick(ctx, poster, channels); err != nil {
			fail(err)
		} else if n > 0 {
			st.LastSentAt, st.LastSentKind, st.LastSentCount = checkedAt, "warning", n
			dirty = true
		}
	}
	if dirty {
		st.CheckedAt = checkedAt
		_ = s.kvSet("disaster:state", st)
	}
}

// ---------------------------------------------------------------------------
// Earthquakes
// ---------------------------------------------------------------------------

// earthquakeTick announces new earthquakes; it returns how many channel messages it sent or edited.
func (s *Service) earthquakeTick(ctx context.Context, poster ChannelPoster, channels []disasterChannel) (int, error) {
	r := s.Registry()
	if err := r.reserve(r.kmaID); err != nil {
		return 0, err
	}
	all, err := r.KMA.Earthquakes(ctx, 3)
	s.noteProvider(r.kmaID, err)
	if err != nil {
		return 0, err
	}
	cfg := s.Doc().DisasterAlerts
	var events []kma.Earthquake
	for _, e := range all {
		if e.NoDomesticImpact || e.EventTime.IsZero() || e.Magnitude == nil || *e.Magnitude < cfg.MinMagnitude(e.Domestic) {
			continue
		}
		events = append(events, e)
	}
	sort.Slice(events, func(i, j int) bool { return events[i].EventTime.Before(events[j].EventTime) })
	var markRaw string
	seen, _ := s.kvGet("disaster:eqk_mark", &markRaw)
	if !seen {
		// first check: what KMA already published is the starting point, not news
		mark := s.Clock.Now()
		if len(events) > 0 {
			mark = events[len(events)-1].EventTime
		}
		return 0, s.kvSet("disaster:eqk_mark", mark.UTC().Format(time.RFC3339))
	}
	mark, err := time.Parse(time.RFC3339, markRaw)
	if err != nil {
		mark = s.Clock.Now()
	}
	newest := mark
	for _, e := range events {
		if e.EventTime.After(newest) {
			newest = e.EventTime
		}
	}
	if !newest.After(mark) {
		return 0, nil
	}
	// the watermark moves before anything is sent: a crash or a failed channel never repeats an event
	if err := s.kvSet("disaster:eqk_mark", newest.UTC().Format(time.RFC3339)); err != nil {
		return 0, err
	}
	msgs := map[string]string{}
	_, _ = s.kvGet("disaster:eqk_msgs", &msgs)
	sent := 0
	for _, inc := range earthquakeIncidents(events) {
		fresh := false
		for _, e := range inc {
			fresh = fresh || e.EventTime.After(mark)
		}
		if !fresh {
			continue
		}
		key := fmt.Sprint(inc[0].EventTime.Unix())
		for _, ch := range channels {
			card := s.earthquakeCard(ch.Locale, inc)
			id := msgs[key+":"+ch.Channel]
			if id != "" {
				if ed, ok := poster.(ChannelEditor); ok {
					if err := ed.EditChannel(ctx, ch.Channel, id, &card); err == nil {
						sent++
						continue
					}
				}
			}
			newID, err := poster.PostChannel(ctx, ch.Channel, "", "", &card)
			if err != nil {
				s.Log.Warn("disaster alerts: earthquake post failed", "channel", ch.Channel, "err", err)
				s.LogProblem(ch.Guild, ch.Channel, logDisasterFailed, "", errclass.Of(err))
				continue
			}
			msgs[key+":"+ch.Channel] = newID
			sent++
		}
	}
	// incidents older than the window no longer get aftershocks
	cutoff := s.Clock.Now().Add(-eqkWindow - 24*time.Hour).Unix()
	for k := range msgs {
		var at int64
		if _, err := fmt.Sscan(strings.SplitN(k, ":", 2)[0], &at); err == nil && at < cutoff {
			delete(msgs, k)
		}
	}
	_ = s.kvSet("disaster:eqk_msgs", msgs)
	return sent, nil
}

// earthquakeIncidents groups events (oldest first) into incidents: an event within 72 hours and 150 km
// of an incident's first event belongs to it. Without coordinates the place name has to match.
func earthquakeIncidents(events []kma.Earthquake) [][]kma.Earthquake {
	var out [][]kma.Earthquake
	for _, e := range events {
		placed := false
		for i, inc := range out {
			first := inc[0]
			if e.EventTime.Sub(first.EventTime) > eqkWindow {
				continue
			}
			near := first.Location == e.Location
			if first.Lat != nil && first.Lon != nil && e.Lat != nil && e.Lon != nil {
				near = haversineKM(*first.Lat, *first.Lon, *e.Lat, *e.Lon) <= eqkRadiusKM
			}
			if near {
				out[i] = append(out[i], e)
				placed = true
				break
			}
		}
		if !placed {
			out = append(out, []kma.Earthquake{e})
		}
	}
	return out
}

func haversineKM(lat1, lon1, lat2, lon2 float64) float64 {
	const r = 6371.0
	toRad := func(d float64) float64 { return d * math.Pi / 180 }
	dLat, dLon := toRad(lat2-lat1), toRad(lon2-lon1)
	a := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(toRad(lat1))*math.Cos(toRad(lat2))*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * r * math.Asin(math.Sqrt(a))
}

// earthquakeCard is an incident's message: its strongest event first, then the latest ones.
func (s *Service) earthquakeCard(locale string, inc []kma.Earthquake) action.Card {
	t := func(key string, args ...any) string { return s.I18n.T(locale, key, args...) }
	strongest := inc[0]
	for _, e := range inc {
		if *e.Magnitude > *strongest.Magnitude {
			strongest = e
		}
	}
	line := func(e kma.Earthquake) string {
		l := t("disaster.eqk.line", "mag", fmt.Sprintf("%.1f", *e.Magnitude), "place", e.Location, "time", e.EventTime.In(kma.KST).Format("01-02 15:04"))
		if e.DepthKM != nil {
			l += t("disaster.eqk.depth", "km", fmt.Sprintf("%.0f", *e.DepthKM))
		}
		return l
	}
	var lines []string
	lines = append(lines, "**"+line(strongest)+"**")
	if rem := strings.TrimSpace(strongest.Remark); rem != "" {
		lines = append(lines, clip(rem, 300))
	}
	if len(inc) > 1 {
		lines = append(lines, "", t("disaster.eqk.sequence", "n", len(inc)))
		shown := 0
		for i := len(inc) - 1; i >= 0 && shown < eqkShownEvents; i-- {
			if inc[i].EventTime.Equal(strongest.EventTime) && inc[i].Location == strongest.Location {
				continue
			}
			lines = append(lines, "- "+line(inc[i]))
			shown++
		}
		if rest := len(inc) - 1 - shown; rest > 0 {
			lines = append(lines, t("disaster.eqk.more", "n", rest))
		}
	}
	title := t("disaster.eqk.title_overseas")
	if strongest.Domestic {
		title = t("disaster.eqk.title_domestic")
	}
	return action.Card{Title: title, Text: strings.Join(lines, "\n"), Tone: "quake", Footer: t("disaster.footer", "time", s.Clock.Now().In(kma.KST).Format("01-02 15:04"))}
}

// ---------------------------------------------------------------------------
// Weather warnings
// ---------------------------------------------------------------------------

// warningTick announces warnings that appeared or ended since the last check.
func (s *Service) warningTick(ctx context.Context, poster ChannelPoster, channels []disasterChannel) (int, error) {
	r := s.Registry()
	if err := r.reserve(r.kmaID); err != nil {
		return 0, err
	}
	all, err := r.KMA.ActiveWarnings(ctx)
	s.noteProvider(r.kmaID, err)
	if err != nil {
		return 0, err
	}
	cfg := s.Doc().DisasterAlerts
	current := map[string]kma.Warning{}
	for _, w := range all {
		if wantWarning(cfg, w) {
			current[w.RegionCode+"|"+w.Kind+"|"+w.Level] = w
		}
	}
	var before map[string]kma.Warning
	seen, _ := s.kvGet("disaster:wrn_active", &before)
	// the set is stored only when it changed (or on the first check): most checks find the same warnings
	if !seen || !reflect.DeepEqual(before, current) {
		if err := s.kvSet("disaster:wrn_active", current); err != nil {
			return 0, err
		}
	}
	if !seen {
		return 0, nil // first check: what is already in force is the starting point
	}
	type group struct {
		kind, level string
		lifted      bool
		regions     []string
		at          string
	}
	groups := map[string]*group{}
	add := func(w kma.Warning, lifted bool) {
		k := fmt.Sprint(w.KindName, w.LevelName, lifted)
		g := groups[k]
		if g == nil {
			g = &group{kind: w.KindName, level: w.LevelName, lifted: lifted, at: w.Effective}
			groups[k] = g
		}
		g.regions = append(g.regions, w.Region)
	}
	for k, w := range current {
		if _, was := before[k]; !was {
			add(w, false)
		}
	}
	for k, w := range before {
		if _, still := current[k]; !still {
			add(w, true)
		}
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	sent := 0
	for _, k := range keys {
		g := groups[k]
		sort.Strings(g.regions)
		for _, ch := range channels {
			t := func(key string, args ...any) string { return s.I18n.T(ch.Locale, key, args...) }
			title, tone := t("disaster.wrn.issued", "kind", g.kind, "level", g.level), "warn"
			if g.lifted {
				title, tone = t("disaster.wrn.lifted", "kind", g.kind, "level", g.level), "ok"
			}
			text := clip(strings.Join(g.regions, ", "), 1500)
			if at := warningTime(g.at); at != "" && !g.lifted {
				text += "\n" + t("disaster.wrn.effective", "time", at)
			}
			card := action.Card{Title: title, Text: text, Tone: tone, Footer: t("disaster.footer", "time", s.Clock.Now().In(kma.KST).Format("01-02 15:04"))}
			if _, err := poster.PostChannel(ctx, ch.Channel, "", "", &card); err != nil {
				s.Log.Warn("disaster alerts: warning post failed", "channel", ch.Channel, "err", err)
				s.LogProblem(ch.Guild, ch.Channel, logDisasterFailed, "", errclass.Of(err))
				continue
			}
			sent++
		}
	}
	return sent, nil
}

// wantWarning: 경보 on land everywhere, 주의보 on land in the chosen regions, sea areas (region codes
// starting with S) only when asked for; 예비특보 never.
func wantWarning(cfg config.DisasterAlerts, w kma.Warning) bool {
	if strings.HasPrefix(w.RegionCode, "S") && !cfg.Marine {
		return false
	}
	switch w.Level {
	case "3":
		return true
	case "2":
		for _, r := range cfg.WarningRegions {
			if r = strings.TrimSpace(r); r != "" && (strings.Contains(w.Region, r) || strings.Contains(w.Parent, r)) {
				return true
			}
		}
	}
	return false
}

// warningTime is TM_EF (YYYYMMDDHHMM) as "MM-DD HH:MM".
func warningTime(raw string) string {
	t, err := time.ParseInLocation("200601021504", strings.TrimSpace(raw), kma.KST)
	if err != nil {
		return ""
	}
	return t.Format("01-02 15:04")
}

// ---------------------------------------------------------------------------
// disasters.alerts (console status)
// ---------------------------------------------------------------------------

func (s *Service) disasterAlertsStatus(req action.Request) action.Result {
	var st disasterState
	_, _ = s.kvGet("disaster:state", &st)
	// the stored check time only moves when something was sent or failed; the watcher's own clock is newer
	s.disaster.mu.Lock()
	if at := s.disaster.checkedAt; !at.IsZero() {
		if stored, err := time.Parse(time.RFC3339, st.CheckedAt); err != nil || at.After(stored) {
			st.CheckedAt = at.Format(time.RFC3339)
		}
	}
	if at := s.disaster.warningsAt; !at.IsZero() {
		st.WarningsAt = at.Format(time.RFC3339)
	}
	s.disaster.mu.Unlock()
	cfg := s.Doc().DisasterAlerts
	r := s.Registry()
	reason := ""
	switch {
	case !cfg.On():
		reason = "off"
	case r == nil || r.KMA == nil || !r.KMA.Configured():
		reason = "no_kma_key"
	case s.Discord == nil || !s.Discord.Connected():
		reason = "discord_offline"
	case len(s.disasterChannels()) == 0:
		reason = "no_channels"
	}
	regions := cfg.WarningRegions
	if regions == nil {
		regions = []string{}
	}
	return action.OK(req, map[string]any{"enabled": cfg.On(), "warnings": cfg.WarningsOn(), "marine": cfg.Marine,
		"earthquake_min_domestic": cfg.MinMagnitude(true), "earthquake_min_overseas": cfg.MinMagnitude(false),
		"warning_regions": regions, "channels": len(s.disasterChannels()), "waiting": reason, "state": st})
}
