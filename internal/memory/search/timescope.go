package search

import (
	"math"
	"strings"
	"time"

	"masamong/internal/memory/corpus"
)

// Moment is a datetime in one of two frames: aware (an instant) or naive (a wall clock with no zone,
// e.g. Kakao). Naive wall clocks are carried in a UTC container and never compared with aware
// instants unless a zone was declared (retrieval_post contract).
type Moment struct {
	T     time.Time
	Aware bool
}

func parseMoment(value string) (Moment, bool, bool) {
	text := strings.TrimSpace(value)
	t, aware, ok := corpus.ParseTime(text)
	if !ok {
		return Moment{}, false, false
	}
	return Moment{T: t, Aware: aware}, len(text) == 10, true
}

// TimeReference is the caller-injected reference clock: live (request wall clock, must be aware) or
// replay (explicit anchor). TZ is the zone for day boundaries; NaiveTZ is the declared zone of naive
// records (nil = naive records cannot be placed in an aware frame and become unresolved).
type TimeReference struct {
	Mode    string // live | replay
	Now     Moment
	TZ      *time.Location
	NaiveTZ *time.Location
	Source  string
}

func (r *TimeReference) frame() string {
	if r.Now.Aware {
		return "aware"
	}
	return "naive"
}

func (r *TimeReference) localNow() time.Time {
	if r.Now.Aware && r.TZ != nil {
		return r.Now.T.In(r.TZ)
	}
	return r.Now.T
}

// TimeScope is a resolved time constraint: status none | soft (recent) | hard | invalid | unresolved.
// Lo inclusive, Hi exclusive, in Frame.
type TimeScope struct {
	Status   string
	Kind     string
	Lo, Hi   *time.Time
	Frame    string
	NaiveTZ  *time.Location
	Reason   string
	Reversed bool
}

func (s TimeScope) Hard() bool { return s.Status == "hard" }

// toFrame places a record time into the scope frame (nil when impossible).
func (s TimeScope) toFrame(m Moment) *time.Time {
	f := "naive"
	if m.Aware {
		f = "aware"
	}
	if f == s.Frame {
		t := m.T
		return &t
	}
	if s.NaiveTZ == nil {
		return nil
	}
	if !m.Aware { // naive record into an aware frame
		t := time.Date(m.T.Year(), m.T.Month(), m.T.Day(), m.T.Hour(), m.T.Minute(), m.T.Second(), m.T.Nanosecond(), s.NaiveTZ)
		return &t
	}
	w := m.T.In(s.NaiveTZ) // aware record into a naive frame
	t := time.Date(w.Year(), w.Month(), w.Day(), w.Hour(), w.Minute(), w.Second(), w.Nanosecond(), time.UTC)
	return &t
}

// ObservationStatus is in | out | unresolved | missing | unscoped for one record time.
func (s TimeScope) ObservationStatus(parsedTime string) string {
	if !s.Hard() {
		return "unscoped"
	}
	m, _, ok := parseMoment(parsedTime)
	if !ok {
		return "missing"
	}
	t := s.toFrame(m)
	if t == nil {
		return "unresolved"
	}
	if s.Lo != nil && t.Before(*s.Lo) {
		return "out"
	}
	if s.Hi != nil && !t.Before(*s.Hi) {
		return "out"
	}
	return "in"
}

func midnight(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// boundInFrame reads a planner bound in the reference frame (naive planner dates are in ref.TZ).
func boundInFrame(m Moment, ref *TimeReference) *time.Time {
	t := m.T
	if ref == nil {
		return &t
	}
	if m.Aware == ref.Now.Aware {
		return &t
	}
	if ref.Now.Aware {
		loc := ref.TZ
		if loc == nil {
			loc = time.UTC
		}
		t = time.Date(m.T.Year(), m.T.Month(), m.T.Day(), m.T.Hour(), m.T.Minute(), m.T.Second(), m.T.Nanosecond(), loc)
		return &t
	}
	if ref.NaiveTZ != nil {
		w := m.T.In(ref.NaiveTZ)
		t = time.Date(w.Year(), w.Month(), w.Day(), w.Hour(), w.Minute(), w.Second(), w.Nanosecond(), time.UTC)
		return &t
	}
	return nil
}

// TimeSpec is the planner's structured time (planner_output.time).
type TimeSpec struct {
	Kind string `json:"kind"`
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
}

var relativeKinds = map[string]bool{"today": true, "yesterday": true, "last_week": true, "this_month": true}
var absoluteKinds = map[string]bool{"before": true, "after": true, "between": true}

// ResolveTimeScope is retrieval_post.resolve_time_scope (pure; never reads the wall clock).
func ResolveTimeScope(spec *TimeSpec, ref *TimeReference, recent bool) TimeScope {
	frame, naiveTZ := "naive", (*time.Location)(nil)
	if ref != nil {
		frame, naiveTZ = ref.frame(), ref.NaiveTZ
	}
	kind := ""
	if spec != nil {
		kind = strings.TrimSpace(spec.Kind)
	}
	if kind == "" {
		kind = "any"
		if recent {
			kind = "recent"
		}
	}
	switch {
	case kind == "any":
		return TimeScope{Status: "none", Kind: kind}
	case kind == "recent":
		return TimeScope{Status: "soft", Kind: kind}
	case !relativeKinds[kind] && !absoluteKinds[kind]:
		return TimeScope{Status: "invalid", Kind: kind, Reason: "unknown_kind"}
	}
	if relativeKinds[kind] {
		if ref == nil {
			return TimeScope{Status: "unresolved", Kind: kind, Reason: "reference_time_missing"}
		}
		now := ref.localNow()
		day0 := midnight(now)
		day1 := day0.AddDate(0, 0, 1)
		var lo, hi time.Time
		switch kind {
		case "today":
			lo, hi = day0, day1
		case "yesterday":
			lo, hi = day0.AddDate(0, 0, -1), day0
		case "last_week":
			lo, hi = day0.AddDate(0, 0, -7), day1
		default: // this_month
			lo, hi = time.Date(day0.Year(), day0.Month(), 1, 0, 0, 0, 0, day0.Location()), day1
		}
		return TimeScope{Status: "hard", Kind: kind, Lo: &lo, Hi: &hi, Frame: frame, NaiveTZ: naiveTZ}
	}
	var frm, to *Moment
	var toDateOnly bool
	if spec != nil {
		if m, _, ok := parseMoment(spec.From); ok {
			frm = &m
		}
		if m, dOnly, ok := parseMoment(spec.To); ok {
			to, toDateOnly = &m, dOnly
		}
	}
	var lo, hi *time.Time
	reversed := false
	switch kind {
	case "before":
		b := to
		if b == nil {
			b = frm
		}
		if b == nil {
			return TimeScope{Status: "invalid", Kind: kind, Reason: "bound_unparseable"}
		}
		if hi = boundInFrame(*b, ref); hi == nil {
			return TimeScope{Status: "unresolved", Kind: kind, Reason: "bound_frame_mismatch"}
		}
	case "after":
		b := frm
		if b == nil {
			b = to
		}
		if b == nil {
			return TimeScope{Status: "invalid", Kind: kind, Reason: "bound_unparseable"}
		}
		if lo = boundInFrame(*b, ref); lo == nil {
			return TimeScope{Status: "unresolved", Kind: kind, Reason: "bound_frame_mismatch"}
		}
	default: // between
		if frm == nil || to == nil {
			return TimeScope{Status: "invalid", Kind: kind, Reason: "bound_unparseable"}
		}
		if frm.Aware != to.Aware {
			return TimeScope{Status: "invalid", Kind: kind, Reason: "bound_frames_differ"}
		}
		if frm.T.After(to.T) {
			frm, to = to, frm
			toDateOnly = len(strings.TrimSpace(spec.From)) == 10
			reversed = true
		}
		lo = boundInFrame(*frm, ref)
		end := *to
		if toDateOnly {
			end.T = end.T.AddDate(0, 0, 1)
		}
		hi = boundInFrame(end, ref)
		if lo == nil || hi == nil {
			return TimeScope{Status: "unresolved", Kind: kind, Reason: "bound_frame_mismatch"}
		}
	}
	if ref == nil {
		probe := frm
		if probe == nil {
			probe = to
		}
		if probe != nil && probe.Aware {
			frame = "aware"
		}
	}
	return TimeScope{Status: "hard", Kind: kind, Lo: lo, Hi: hi, Frame: frame, NaiveTZ: naiveTZ, Reversed: reversed}
}

// TimeMeta is what narrowing reports into the trace.
type TimeMeta struct {
	Kind           string  `json:"time_kind,omitempty"`
	Applied        bool    `json:"time_window_applied"`
	Hard           bool    `json:"time_window_hard,omitempty"`
	RecentApplied  bool    `json:"recent_window_applied"`
	RecentDays     float64 `json:"recent_window_days,omitempty"`
	RecentLeaves   int     `json:"recent_window_leaves,omitempty"`
	PoolN          *int    `json:"time_pool_n,omitempty"`
	RecordsInRange int     `json:"time_records_in_range_n,omitempty"`
	Scale          float64 `json:"time_window_scale,omitempty"`
	ScaleApplied   bool    `json:"time_window_scale_applied,omitempty"`
	From           string  `json:"time_from,omitempty"`
	To             string  `json:"time_to,omitempty"`
	Invalid        bool    `json:"time_window_invalid,omitempty"`
	InvalidReason  string  `json:"time_window_invalid_reason,omitempty"`
	Unresolved     bool    `json:"time_unresolved,omitempty"`
	UnresolvedWhy  string  `json:"time_unresolved_reason,omitempty"`
	UnresolvedPart bool    `json:"time_unresolved_partial,omitempty"`
}

func (ix *Index) recencyAnchor() float64 {
	if ix.Cfg.RecencyAnchor == "now" {
		return float64(time.Now().UnixNano()) / 1e9
	}
	return ix.corpusTMax
}

// narrowRecentOne is retrieval_post._narrow_recent_one (per-room recent window).
func (ix *Index) narrowRecentOne(universe Set, scale float64) (Set, float64, int) {
	days := floatOr(ix.Cfg.RecentWindowDays, 30) * math.Max(1, scale)
	k := int(float64(intOr(ix.Cfg.RecentWindowLeaves, 200)) * math.Max(1, scale))
	anchor, have := 0.0, false
	for id := range universe {
		ts := ix.leafEndTS[id]
		if !have || ts > anchor {
			anchor, have = ts, true
		}
	}
	if !have {
		anchor = ix.recencyAnchor()
	}
	cutoff := anchor - math.Max(days, 0)*86400
	var byTS, fresh []string
	for _, id := range ix.byRecency {
		if universe.Has(id) {
			byTS = append(byTS, id)
			if ix.leafEndTS[id] >= cutoff {
				fresh = append(fresh, id)
			}
		}
	}
	var window Set
	if len(fresh) > 0 {
		window = setOf(fresh...)
	} else {
		window = setOf(byTS[:min(len(byTS), max(1, k))]...)
	}
	if len(window) < min(k, len(byTS)) {
		for _, id := range byTS[:min(k, len(byTS))] {
			window[id] = struct{}{}
		}
	}
	return window, days, k
}

func (ix *Index) narrowRecent(allowed Set, scale float64) (Set, TimeMeta) {
	universe := allowed
	if universe == nil {
		universe = setOf(ix.leafIDs...)
	}
	byRoom := map[string]Set{}
	for id := range universe {
		room := "_"
		if l, ok := ix.Leaf(id); ok && l.Room != "" {
			room = l.Room
		}
		if byRoom[room] == nil {
			byRoom[room] = Set{}
		}
		byRoom[room][id] = struct{}{}
	}
	meta := TimeMeta{RecentApplied: true}
	var window Set
	if len(byRoom) <= 1 {
		window, meta.RecentDays, meta.RecentLeaves = ix.narrowRecentOne(universe, scale)
	} else {
		window = Set{}
		for _, g := range byRoom {
			part, days, k := ix.narrowRecentOne(g, scale)
			meta.RecentDays, meta.RecentLeaves = days, k
			for id := range part {
				window[id] = struct{}{}
			}
		}
	}
	n := len(window)
	meta.PoolN = &n
	return window, meta
}

func fmtTime(t *time.Time, frame string) string {
	if t == nil {
		return ""
	}
	if frame == "naive" {
		return t.Format("2006-01-02T15:04:05")
	}
	return t.Format(time.RFC3339)
}

// narrowTimeIDs scopes the candidate universe by the planner time (v2) or `recent` (v1). Scope comes
// before ranking: nothing outside the window becomes a candidate. Hard windows ignore scale.
func (ix *Index) narrowTimeIDs(allowed Set, it Intent, scale float64, ref *TimeReference) (Set, TimeMeta) {
	scope := ResolveTimeScope(it.Time, ref, it.Recent)
	if scope.Status == "none" && !it.Recent {
		m := TimeMeta{}
		if it.Time != nil {
			m.Kind = scope.Kind
		}
		return allowed, m
	}
	if scope.Status == "soft" {
		window, meta := ix.narrowRecent(allowed, scale)
		meta.Applied, meta.Kind, meta.Scale, meta.ScaleApplied = true, "recent", scale, scale > 1.0
		return window, meta
	}
	meta := TimeMeta{Kind: scope.Kind, Hard: scope.Hard()}
	switch scope.Status {
	case "none":
		return allowed, meta
	case "invalid":
		meta.Invalid, meta.InvalidReason = true, scope.Reason
		return allowed, meta
	case "unresolved":
		zero := 0
		meta.Unresolved, meta.UnresolvedWhy, meta.PoolN = true, scope.Reason, &zero
		return Set{}, meta
	}
	universe := allowed
	if universe == nil {
		universe = setOf(ix.leafIDs...)
	}
	window := Set{}
	unresolved := 0
	for id := range universe {
		l, ok := ix.Leaf(id)
		if !ok {
			continue
		}
		in, unres := 0, 0
		if l.Start >= 0 && l.Start < l.End && l.End <= len(ix.Messages) {
			for _, m := range ix.Messages[l.Start:l.End] {
				switch scope.ObservationStatus(m.ParsedTime) {
				case "in":
					in++
				case "unresolved":
					unres++
				}
			}
		}
		if in > 0 {
			window[id] = struct{}{}
			meta.RecordsInRange += in
		} else if unres > 0 {
			unresolved++
		}
	}
	n := len(window)
	meta.Applied, meta.PoolN, meta.Scale = true, &n, scale
	meta.From, meta.To = fmtTime(scope.Lo, scope.Frame), fmtTime(scope.Hi, scope.Frame)
	if n == 0 && unresolved > 0 && meta.RecordsInRange == 0 {
		meta.UnresolvedPart = true
	}
	return window, meta
}
