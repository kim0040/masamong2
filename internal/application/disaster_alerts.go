package application

import (
	"context"
	"encoding/json"
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
//   - Weather warnings: each room selects its watch level by KMA area; sea areas also require
//     disaster_alerts.marine. A warning that appears is announced as issued, one that
//     disappears as lifted. A rise in level for the same area and kind is one upgrade. The first check only
//     records what is active, and so does the first check on a set an older build stored.
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
	WatchLevel             map[string]int
}

// disasterChannels are the channels the bot answers in on approved servers, minus those that turned
// announcements off. doc is the configuration snapshot the caller also reads its switches from.
func (s *Service) disasterChannels(doc config.Document) []disasterChannel {
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
			out = append(out, disasterChannel{Guild: gid, Channel: cid, Locale: loc, WatchLevel: c.WatchLevel})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Guild+out[i].Channel < out[j].Guild+out[j].Channel })
	return out
}

// disasterTick is one scheduler minute: earthquakes, and warnings when five minutes have passed.
func (s *Service) disasterTick(ctx context.Context) {
	// one configuration snapshot for the whole check: the rooms' levels and the instance switches
	// (warnings, marine, magnitudes) always come from the same revision
	doc := s.Doc()
	cfg := doc.DisasterAlerts
	if s.practice || s.Store == nil || !cfg.On() {
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
	channels := s.disasterChannels(doc)
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
	if n, err := s.earthquakeTick(ctx, poster, channels, cfg); err != nil {
		fail(err)
	} else if n > 0 {
		st.LastSentAt, st.LastSentKind, st.LastSentCount = checkedAt, "earthquake", n
		dirty = true
	}
	s.disaster.mu.Lock()
	s.disaster.checkedAt = now.UTC()
	last := s.disaster.warningsAt
	due := cfg.WarningsOn() && now.Sub(last) >= warningEvery-5*time.Second
	if due {
		s.disaster.warningsAt = now.UTC()
	}
	s.disaster.mu.Unlock()
	if due {
		if n, err := s.warningTick(ctx, poster, channels, doc); err != nil {
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
func (s *Service) earthquakeTick(ctx context.Context, poster ChannelPoster, channels []disasterChannel, cfg config.DisasterAlerts) (int, error) {
	r := s.Registry()
	if err := r.reserve(r.kmaID); err != nil {
		return 0, err
	}
	all, err := r.KMA.Earthquakes(ctx, 3)
	s.noteProvider(r.kmaID, err)
	if err != nil {
		return 0, err
	}
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

// All rooms compare the same KMA observations, including advisories below a room's watch level.
type warningSnapshot map[string]kma.Warning

// warningStateVersion marks what the warning check stores in disaster:wrn_active. Builds before room
// watch levels stored a bare map that kept only what the global warning_regions and marine switches
// let through, so it cannot say which advisories were already in force. The first check that finds
// such a set (or any set without this version) records everything in force again and announces
// nothing, whatever the rooms are set to.
const warningStateVersion = 2

// warningState is what one warning check leaves for the next.
type warningState struct {
	Version int             `json:"version"`
	Active  warningSnapshot `json:"active"`
	// Sent is, for each area and kind in Active, the level each room (by channel ID) was last told is
	// in force. A lift goes only to the rooms told about that level, and an entry ends with its warning.
	Sent map[string]map[string]string `json:"sent,omitempty"`
}

// warningKey is the area and kind a warning's level belongs to.
func warningKey(w kma.Warning) string { return w.RegionCode + "|" + w.Kind }

// Lower levels are cumulative. Other advisories and warnings require level 5.
var warningWatchLevels = map[string]map[string]int{
	"N": {"2": 2, "3": 2},            // 지진해일
	"H": {"2": 4, "3": 3, "중대경보": 3}, // 폭염
	"S": {"2": 4, "3": 3},            // 대설
	"C": {"2": 4, "3": 3},            // 한파
	"T": {"2": 4, "3": 3},            // 태풍
	"R": {"3": 3},                    // 호우
	"O": {"3": 3},                    // 폭풍해일
	"Y": {"3": 4},                    // 황사
	"V": {"3": 4},                    // 풍랑
	"W": {"3": 4},                    // 강풍
}

func (ch disasterChannel) watches(w kma.Warning) bool {
	required := warningWatchLevels[w.Kind][w.Level]
	if required == 0 {
		required = 5
	}
	for _, region := range []string{w.RegionCode, w.Region, w.ParentCode, w.Parent} {
		if level, ok := ch.WatchLevel[region]; ok {
			return level >= required
		}
	}
	return false // unconfigured regions are level 1, earthquakes only
}

// Rank orders the levels locally. KMA sends the heat emergency as the literal "중대경보".
func warningRank(w kma.Warning) int {
	switch w.Level {
	case "2":
		return 2
	case "3":
		return 3
	case "중대경보":
		if w.Kind == "H" {
			return 4
		}
	}
	return 0
}

func warningCancelled(command string) bool {
	switch command {
	case "해제", "대치해제", "변경해제", "3", "4", "7":
		return true
	}
	return false
}

// warningLater says a takes effect after b, or at the same time but was announced later.
func warningLater(a, b kma.Warning) bool {
	return a.Effective > b.Effective || a.Effective == b.Effective && a.Announced > b.Announced
}

func (snapshot warningSnapshot) record(w kma.Warning) {
	key := warningKey(w)
	old := snapshot[key]
	if w.Effective < old.Effective || (w.Effective == old.Effective && w.Announced < old.Announced) {
		return
	}
	if w.Effective == old.Effective && w.Announced == old.Announced && warningRank(w) < warningRank(old) {
		return
	}
	snapshot[key] = w
}

// warningTick announces changes to the active level of each area's warning. doc is the check's
// configuration snapshot, the one channels were chosen from.
func (s *Service) warningTick(ctx context.Context, poster ChannelPoster, channels []disasterChannel, doc config.Document) (int, error) {
	cfg := doc.DisasterAlerts
	r := s.Registry()
	if err := r.reserve(r.kmaID); err != nil {
		return 0, err
	}
	all, err := r.KMA.ActiveWarnings(ctx)
	s.noteProvider(r.kmaID, err)
	if err != nil {
		return 0, err
	}
	s.learnWarningAreas(all)
	current := warningSnapshot{}
	cancelled := warningSnapshot{}
	released := map[string]kma.Warning{} // the latest cancellation of each area, kind and level
	for _, w := range all {
		rank := warningRank(w)
		if rank == 0 {
			continue
		}
		if warningCancelled(w.Command) {
			cancelled.record(w)
			if r, ok := released[warningKey(w)+"|"+w.Level]; !ok || warningLater(w, r) {
				released[warningKey(w)+"|"+w.Level] = w
			}
			continue
		}
		current.record(w)
	}
	// A release of the same level published after its issue ends it, even when the response still
	// lists the issue.
	for k, w := range current {
		if r, ok := released[k+"|"+w.Level]; ok && warningLater(r, w) {
			delete(current, k)
		}
	}
	var raw json.RawMessage
	seen, err := s.kvGet("disaster:wrn_active", &raw)
	if err != nil {
		return 0, err
	}
	var stored warningState
	if seen && (json.Unmarshal(raw, &stored) != nil || stored.Version != warningStateVersion) {
		// an older build's set: what it left out may have been in force for hours, so nothing is news yet
		s.Log.Info("disaster alerts: older warning state replaced; what is in force now is the starting point", "active", len(current))
		// It also kept no record of which rooms it told, so nothing in force now is counted as told: those
		// warnings end without a lift, as the starting point of a first check does.
		return 0, s.kvSet("disaster:wrn_active", warningState{Version: warningStateVersion, Active: current})
	}
	before := stored.Active
	if before == nil {
		before = warningSnapshot{}
	}
	// A room removed from the configuration, or one that watches no area any more (earthquakes only), is
	// told nothing more about weather: what it was told is dropped and it gets no lift. Lifts read told,
	// the stored record without those rooms.
	watching := map[string]bool{}
	for _, g := range doc.Guilds {
		for id, c := range g.Channels {
			if len(c.WatchLevel) > 0 {
				watching[id] = true
			}
		}
	}
	told := map[string]map[string]string{}
	for k, rooms := range stored.Sent {
		for room, level := range rooms {
			if watching[room] {
				if told[k] == nil {
					told[k] = map[string]string{}
				}
				told[k][room] = level
			}
		}
	}
	// what each room was told is kept only for the level still in force: a level that ended is gone with
	// its lift, whether or not the lift reached the room, and a later issue of it needs its own delivery
	next := warningState{Version: warningStateVersion, Active: current}
	for k, rooms := range told {
		w, ok := current[k]
		if !ok {
			continue
		}
		for room, level := range rooms {
			if level != w.Level {
				continue
			}
			if next.Sent == nil {
				next.Sent = map[string]map[string]string{}
			}
			if next.Sent[k] == nil {
				next.Sent[k] = map[string]string{}
			}
			next.Sent[k][room] = level
		}
	}
	// the set is stored only when it changed (or on the first check): most checks find the same warnings.
	// It is stored before anything is sent, so a crash or a failed channel never repeats a card.
	if !seen || !reflect.DeepEqual(before, current) || !reflect.DeepEqual(stored.Sent, next.Sent) {
		if err := s.kvSet("disaster:wrn_active", next); err != nil {
			return 0, err
		}
	}
	if !seen {
		return 0, nil // first check: what is already in force is the starting point
	}
	type group struct {
		kind, level, change, from string
		warnings                  []kma.Warning
		at                        string
	}
	groups := map[string]*group{}
	add := func(w kma.Warning, change, from string) {
		if !cfg.Marine && strings.HasPrefix(w.RegionCode, "S") {
			return
		}
		k := strings.Join([]string{w.KindName, w.LevelName, change, from}, "|")
		if change != "lifted" {
			k += "|" + w.Effective // one card shows one effective time
		}
		g := groups[k]
		if g == nil {
			g = &group{kind: w.KindName, level: w.LevelName, change: change, from: from, at: w.Effective}
			groups[k] = g
		}
		g.warnings = append(g.warnings, w)
	}
	for k, w := range current {
		old, was := before[k]
		// Both commands can fall between polls. An explicit cancellation at the same
		// effective time supplies the old level even if the advisory was never observed.
		if ended, ok := cancelled[k]; !was && ok && w.Effective != "" && ended.Effective == w.Effective && warningRank(ended) < warningRank(w) {
			old, was = ended, true
		}
		switch {
		case !was:
			add(w, "issued", "")
		case warningRank(w) > warningRank(old):
			add(w, "upgraded", old.LevelName)
		case old.Level != w.Level:
			add(old, "lifted", "")
			add(w, "issued", "")
		}
	}
	for k, w := range before {
		if _, still := current[k]; !still {
			add(w, "lifted", "")
		}
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	sent := 0
	recorded := false
	for _, k := range keys {
		g := groups[k]
		for _, ch := range channels {
			var shown []kma.Warning
			var regions []string
			for _, w := range g.warnings {
				// a lift goes to the rooms that were told this level is in force, an issue or an
				// upgrade to the rooms whose levels include it
				if g.change == "lifted" && told[warningKey(w)][ch.Channel] == w.Level || g.change != "lifted" && ch.watches(w) {
					shown = append(shown, w)
					regions = append(regions, w.Region)
				}
			}
			if len(regions) == 0 {
				continue
			}
			sort.Strings(regions)
			t := func(key string, args ...any) string { return s.I18n.T(ch.Locale, key, args...) }
			title := t("disaster.wrn."+g.change, "kind", g.kind, "level", g.level, "from", g.from)
			tone := "warn"
			if g.change == "lifted" {
				tone = "ok"
			}
			text := clip(strings.Join(regions, ", "), 1500)
			if at := warningTime(g.at); at != "" && g.change != "lifted" {
				text += "\n" + t("disaster.wrn.effective", "time", at)
			}
			card := action.Card{Title: title, Text: text, Tone: tone, Footer: t("disaster.footer", "time", s.Clock.Now().In(kma.KST).Format("01-02 15:04"))}
			if _, err := poster.PostChannel(ctx, ch.Channel, "", "", &card); err != nil {
				s.Log.Warn("disaster alerts: warning post failed", "channel", ch.Channel, "err", err)
				s.LogProblem(ch.Guild, ch.Channel, logDisasterFailed, "", errclass.Of(err))
				continue
			}
			sent++
			if g.change == "lifted" {
				continue // the ended level's deliveries were already left out of next
			}
			for _, w := range shown {
				wk := warningKey(w)
				if next.Sent == nil {
					next.Sent = map[string]map[string]string{}
				}
				if next.Sent[wk] == nil {
					next.Sent[wk] = map[string]string{}
				}
				next.Sent[wk][ch.Channel] = w.Level
				recorded = true
			}
		}
	}
	if recorded {
		if err := s.kvSet("disaster:wrn_active", next); err != nil {
			s.Log.Warn("disaster alerts: could not record where warnings were announced", "err", err)
		}
	}
	return sent, nil
}

// warningAreasMax bounds the area names and codes the watcher remembers; KMA has a few hundred areas.
const warningAreasMax = 4000

// learnWarningAreas remembers every area code and name KMA reports (an area and the area it belongs
// to), each pointing at "name (code)". The bot ships no KMA area list, so this is what the console
// checks a room's watch_level keys against. It writes only when a new name or code appears.
func (s *Service) learnWarningAreas(all []kma.Warning) {
	known := map[string]string{}
	_, _ = s.kvGet("disaster:wrn_areas", &known)
	added := false
	for _, w := range all {
		for _, area := range [][2]string{{w.RegionCode, w.Region}, {w.ParentCode, w.Parent}} {
			label := area[1] + " (" + area[0] + ")"
			if area[0] == "" || area[1] == "" {
				label = area[0] + area[1]
			}
			for _, key := range area {
				if key != "" && known[key] == "" && len(known) < warningAreasMax {
					known[key] = label
					added = true
				}
			}
		}
	}
	if added {
		if err := s.kvSet("disaster:wrn_areas", known); err != nil {
			s.Log.Warn("disaster alerts: could not remember KMA areas", "err", err)
		}
	}
}

// watchLevelCheck is a room's watch_areas for the console: which of its watch_level keys match an
// area KMA has reported so far ("matched": key → "name (code)"). A key that matches nothing yet is
// saved as it is, and the console flags it: it may be misspelled, or its area has had no warning since
// the bot began watching. "checked" is false until the watcher has seen any area at all.
func (s *Service) watchLevelCheck(levels map[string]int) map[string]any {
	if len(levels) == 0 {
		return nil
	}
	known := map[string]string{}
	_, _ = s.kvGet("disaster:wrn_areas", &known)
	matched := map[string]string{}
	for key := range levels {
		if label := known[key]; label != "" {
			matched[key] = label
		}
	}
	return map[string]any{"checked": len(known) > 0, "matched": matched}
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
	doc := s.Doc()
	cfg := doc.DisasterAlerts
	channels := len(s.disasterChannels(doc))
	r := s.Registry()
	reason := ""
	switch {
	case !cfg.On():
		reason = "off"
	case r == nil || r.KMA == nil || !r.KMA.Configured():
		reason = "no_kma_key"
	case s.Discord == nil || !s.Discord.Connected():
		reason = "discord_offline"
	case channels == 0:
		reason = "no_channels"
	}
	regions := cfg.WarningRegions
	if regions == nil {
		regions = []string{}
	}
	return action.OK(req, map[string]any{"enabled": cfg.On(), "warnings": cfg.WarningsOn(), "marine": cfg.Marine,
		"earthquake_min_domestic": cfg.MinMagnitude(true), "earthquake_min_overseas": cfg.MinMagnitude(false),
		"warning_regions": regions, "channels": channels, "waiting": reason, "state": st})
}
