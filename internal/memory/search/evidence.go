package search

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"masamong/internal/lang"
	"masamong/internal/memory/corpus"
	"masamong/internal/memory/leaves"
)

// Evidence packing (evidence_packing.py): the matched leaf's raw messages are kept first, the
// remaining budget goes to alternating context before/after within the same room. Offsets are Unicode
// code points into the message content; excerpt[excerpt_start:excerpt_end] == content[char_start:char_end].

const (
	statusComplete = "complete"
	statusPartial  = "partial"
	statusInsuff   = "insufficient"
	statusDup      = "duplicate"
	statusOmitted  = "omitted"
)

// Span is one visible raw range.
type Span struct {
	ObservationID string `json:"observation_id"`
	Role          string `json:"role"`
	CharStart     int    `json:"char_start"`
	CharEnd       int    `json:"char_end"`
	ContentLen    int    `json:"content_len"`
	Complete      bool   `json:"complete"`
	ExcerptStart  int    `json:"excerpt_start"`
	ExcerptEnd    int    `json:"excerpt_end"`
	DocID         string `json:"doc_id,omitempty"`
}

// Packed is one packed evidence document.
type Packed struct {
	DocID               string   `json:"doc_id"`
	Start, End          int      `json:"-"`
	Excerpt             string   `json:"excerpt"`
	Status              string   `json:"status"`
	InsufficientReason  string   `json:"insufficient_reason,omitempty"`
	UsedChars           int      `json:"used_chars"`
	ObservationIDs      []string `json:"observation_ids"`
	VisibleObservations []string `json:"visible_observation_ids"`
	VisibleSpans        []Span   `json:"visible_spans"`
	alreadyVisible      []string
}

type unit struct {
	index    int
	row      corpus.Message
	content  []rune
	role     string
	excluded string
	cut      bool
	sel      *[2]int
}

type packScope struct {
	allowedObs   Set // nil = no restriction
	allowedRooms Set // nil = no restriction (checked on the original room)
	timeAllows   func(corpus.Message) bool
	marks        lang.Markers // cut/omission markers (zero = default language)
}

func (p packScope) markers() lang.Markers {
	if p.marks.HeadCut == "" {
		d, _ := lang.Get(lang.Default)
		return d.Search.Markers
	}
	return p.marks
}

func (p packScope) exclusion(m corpus.Message) string {
	if p.allowedRooms != nil {
		orig := m.RoomOriginal
		if orig == "" {
			orig = m.Room
		}
		if !p.allowedRooms.Has(orig) {
			return "room"
		}
	}
	if p.allowedObs != nil && !p.allowedObs.Has(m.ObservationID) {
		return "allowed"
	}
	if p.timeAllows != nil && !p.timeAllows(m) {
		return "time"
	}
	return ""
}

func parentWindow(msgs []corpus.Message, seg leaves.Leaf, before, after int) (int, int) {
	n := len(msgs)
	lo, hi := 0, n
	if seg.Room != "" && seg.Start >= 0 && seg.Start < n {
		lo = seg.Start
		for lo > 0 && msgs[lo-1].Room == seg.Room {
			lo--
		}
		hi = seg.End
		for hi < n && msgs[hi].Room == seg.Room {
			hi++
		}
	}
	return max(lo, seg.Start-max(0, before)), min(hi, seg.End+max(0, after))
}

func prefixText(rows []corpus.Message, withPrefix bool) string {
	if !withPrefix || len(rows) == 0 {
		return ""
	}
	full := leaves.FormatBody(rows, true)
	body := leaves.FormatBody(rows, false)
	if body != "" {
		return full[:len(full)-len(body)-1]
	}
	return full
}

func runeLen(s string) int { return utf8.RuneCountInString(s) }

func render(units []*unit, withPrefix bool, mk lang.Markers) (string, []Span, int) {
	var visible []corpus.Message
	for _, u := range units {
		if u.sel != nil {
			visible = append(visible, u.row)
		}
	}
	if len(visible) == 0 {
		return "", nil, 0
	}
	prefix := prefixText(visible, withPrefix)
	var b strings.Builder
	var spans []Span
	pos := 0
	if prefix != "" {
		b.WriteString(prefix)
		pos = runeLen(prefix)
	}
	firstLine := prefix == ""
	omitted := 0
	seenVisible := false
	suppress := false
	emit := func(text string) {
		if !firstLine {
			b.WriteByte('\n')
			pos++
		}
		b.WriteString(text)
		pos += runeLen(text)
		firstLine = false
	}
	flush := func(kind string) {
		if omitted <= 0 {
			return
		}
		n := strconv.Itoa(omitted)
		switch kind {
		case "before":
			emit(lang.Fill(mk.OmitBefore, "n", n))
		case "after":
			emit(lang.Fill(mk.OmitAfter, "n", n))
		default:
			emit(lang.Fill(mk.OmitMiddle, "n", n))
		}
		omitted = 0
	}
	for _, u := range units {
		if u.excluded != "" || u.cut {
			continue
		}
		if u.sel == nil {
			omitted++
			continue
		}
		if seenVisible {
			flush("middle")
		} else {
			flush("before")
		}
		seenVisible = true
		cs, ce := u.sel[0], u.sel[1]
		head := u.row.ActorLabel + ": "
		if u.row.ActorLabel == "" {
			head = "?: "
		}
		if cs > 0 {
			head += mk.HeadCut
		}
		tail := ""
		if ce < len(u.content) {
			tail = mk.TailCut
		}
		lineStart := pos
		if !firstLine {
			lineStart++
		}
		es := lineStart + runeLen(head)
		emit(head + string(u.content[cs:ce]) + tail)
		spans = append(spans, Span{ObservationID: u.row.ObservationID, Role: u.role, CharStart: cs, CharEnd: ce,
			ContentLen: len(u.content), Complete: cs == 0 && ce == len(u.content), ExcerptStart: es, ExcerptEnd: es + (ce - cs)})
		if ce < len(u.content) {
			omitted = 0
			suppress = true
		} else {
			suppress = false
		}
	}
	if omitted > 0 && !suppress {
		flush("after")
	}
	return b.String(), spans, runeLen(prefix)
}

func selectLayout(units []*unit, charLimit int, withPrefix bool, minPartial int, mk lang.Markers) {
	fits := func() bool {
		ex, _, _ := render(units, withPrefix, mk)
		return runeLen(ex) <= charLimit
	}
	overshoot := func() int {
		ex, _, _ := render(units, withPrefix, mk)
		return runeLen(ex) - charLimit
	}
	try := func(u *unit, side string) bool {
		n := len(u.content)
		capN := n
		set := func(take int) {
			if side == "head" {
				u.sel = &[2]int{0, take}
			} else {
				u.sel = &[2]int{n - take, n}
			}
		}
		set(capN)
		over := overshoot()
		if over <= 0 {
			return true
		}
		take := capN - over
		for i := 0; i < 3; i++ {
			if take < minPartial {
				u.sel = nil
				return false
			}
			set(take)
			over = overshoot()
			if over <= 0 {
				return true
			}
			take -= over
		}
		u.sel = nil
		return false
	}
	var active, matched []*unit
	deferred := false
	for _, u := range units {
		if u.excluded == "" && !u.cut {
			u.sel = nil
			active = append(active, u)
			if u.role == "matched" {
				matched = append(matched, u)
			}
		} else if u.role == "matched" && u.cut && u.excluded == "" {
			deferred = true
		}
	}
	if len(matched) == 0 {
		if deferred {
			for _, u := range active {
				if !try(u, "head") {
					break
				}
				if u.sel == nil || u.sel[1] < len(u.content) {
					break
				}
			}
		}
		return
	}
	for _, u := range matched {
		u.sel = &[2]int{0, len(u.content)}
	}
	present := func() []*unit {
		var out []*unit
		for _, u := range matched {
			if u.sel != nil {
				out = append(out, u)
			}
		}
		return out
	}
	for !fits() && len(present()) > 1 {
		p := present()
		mid := float64(p[0].index+p[len(p)-1].index) / 2
		drop := p[0]
		for _, u := range p[1:] {
			// max by (len(content), -abs(index-mid)); first maximum wins on ties
			if len(u.content) > len(drop.content) || (len(u.content) == len(drop.content) && absf(float64(u.index)-mid) < absf(float64(drop.index)-mid)) {
				drop = u
			}
		}
		drop.sel = nil
	}
	if !fits() {
		remaining := present()
		if len(remaining) == 0 {
			remaining = matched[:1]
		}
		for _, u := range remaining {
			u.sel = nil
			try(u, "head")
		}
		for !fits() {
			held := present()
			if len(held) == 0 {
				break
			}
			held[len(held)-1].sel = nil
		}
	}
	if !fits() {
		for i := len(matched) - 1; i >= 0; i-- {
			matched[i].sel = nil
			if fits() {
				break
			}
		}
		return
	}
	for _, u := range matched {
		if u.sel != nil && (u.sel[0] > 0 || u.sel[1] < len(u.content)) {
			return
		}
	}
	indexOf := func(target *unit) int {
		for i, u := range active {
			if u == target {
				return i
			}
		}
		return -1
	}
	b, a := indexOf(matched[0])-1, indexOf(matched[len(matched)-1])+1
	openBefore, openAfter := true, true
	for (openBefore && b >= 0) || (openAfter && a < len(active)) {
		if openBefore && b >= 0 {
			u := active[b]
			if try(u, "tail") && u.sel != nil && u.sel[0] == 0 {
				b--
			} else {
				openBefore = false
			}
		}
		if openAfter && a < len(active) {
			u := active[a]
			if try(u, "head") && u.sel != nil && u.sel[1] == len(u.content) {
				a++
			} else {
				openAfter = false
			}
		}
	}
}

func absf(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// packSegment is evidence_packing.pack_segment_evidence (the fields the retriever uses).
func packSegment(msgs []corpus.Message, seg leaves.Leaf, before, after int, withPrefix bool, charLimit int, scope packScope, claimed Set) Packed {
	start, end := parentWindow(msgs, seg, before, after)
	var units []*unit
	for i := start; i < end; i++ {
		row := msgs[i]
		role := "context"
		if i >= seg.Start && i < seg.End {
			role = "matched"
		}
		u := &unit{index: i, row: row, content: []rune(row.Content), role: role, excluded: scope.exclusion(row)}
		if u.excluded == "" && claimed.Has(row.ObservationID) {
			u.cut = true
		}
		units = append(units, u)
	}
	if charLimit > 0 {
		selectLayout(units, charLimit, withPrefix, 8, scope.markers())
	}
	excerpt, spans, _ := render(units, withPrefix, scope.markers())
	if charLimit <= 0 {
		excerpt, spans = "", nil
	}
	out := Packed{DocID: seg.DocID, Start: start, End: end, Excerpt: excerpt, UsedChars: runeLen(excerpt), VisibleSpans: spans}
	spanBy := map[string]Span{}
	for _, s := range spans {
		spanBy[s.ObservationID] = s
	}
	var matchedIn, packable []*unit
	for _, u := range units {
		if u.excluded == "" {
			out.ObservationIDs = append(out.ObservationIDs, u.row.ObservationID)
			if u.role == "matched" {
				matchedIn = append(matchedIn, u)
				if !u.cut {
					packable = append(packable, u)
				}
			}
		}
		if u.sel != nil {
			out.VisibleObservations = append(out.VisibleObservations, u.row.ObservationID)
		}
		if u.excluded == "" && u.cut {
			out.alreadyVisible = append(out.alreadyVisible, u.row.ObservationID)
		}
	}
	visibleAny, visibleComplete := 0, 0
	for _, u := range matchedIn {
		if u.sel != nil {
			visibleAny++
			if spanBy[u.row.ObservationID].Complete {
				visibleComplete++
			}
		}
	}
	reason := ""
	switch {
	case charLimit <= 0:
		reason = "char_limit"
	case len(matchedIn) == 0:
		switch {
		case seg.End <= seg.Start:
			reason = "matched_empty"
		case scope.allowedObs != nil && len(scope.allowedObs) == 0:
			reason = "empty_allowed_set"
		default:
			reason = "matched_out_of_scope"
		}
	case len(packable) == 0:
		anyVisible := false
		for _, u := range units {
			if u.sel != nil {
				anyVisible = true
			}
		}
		if !anyVisible {
			reason = "matched_already_visible"
		}
	case visibleAny == 0:
		var rows []corpus.Message
		for _, u := range packable {
			rows = append(rows, u.row)
		}
		if p := prefixText(rows, withPrefix); p != "" && runeLen(p)+1 >= charLimit {
			reason = "prefix_exhausts_budget"
		} else {
			reason = "budget"
		}
	}
	switch {
	case reason == "matched_already_visible":
		out.Status = statusDup
	case reason != "":
		out.Status = statusInsuff
	case len(packable) == 0 && len(matchedIn) > 0:
		out.Status = statusComplete
	case visibleComplete == len(packable):
		out.Status = statusComplete
	default:
		out.Status = statusPartial
	}
	out.InsufficientReason = reason
	return out
}

// packSet is evidence_packing.pack_evidence_set with dedupe_visible=True.
func packSet(msgs []corpus.Message, segs []leaves.Leaf, before, after int, withPrefix bool, docLimit, totalLimit int, scope packScope) ([]Packed, []Span, []string) {
	claimed := Set{}
	claimedBy := map[string]string{}
	var docs []Packed
	used := 0
	for _, seg := range segs {
		budget := docLimit
		if totalLimit > 0 {
			budget = min(budget, totalLimit-used)
		}
		if budget <= 0 {
			docs = append(docs, Packed{DocID: seg.DocID, Status: statusOmitted, InsufficientReason: "total_budget"})
			continue
		}
		p := packSegment(msgs, seg, before, after, withPrefix, budget, scope, claimed)
		leftover := totalLimit > 0 && budget < docLimit
		switch {
		case leftover && p.Status == statusInsuff && p.UsedChars == 0:
			p.Status, p.InsufficientReason = statusOmitted, "total_budget"
			p.Excerpt, p.UsedChars, p.VisibleSpans, p.VisibleObservations = "", 0, nil, nil
		case p.Status == statusDup:
			p.Excerpt, p.UsedChars, p.VisibleSpans, p.VisibleObservations = "", 0, nil, nil
		default:
			used += p.UsedChars
			for _, oid := range p.VisibleObservations {
				if _, ok := claimedBy[oid]; !ok {
					claimedBy[oid] = seg.DocID
					claimed[oid] = struct{}{}
				}
			}
		}
		docs = append(docs, p)
	}
	var spans []Span
	var visible []string
	for _, d := range docs {
		visible = append(visible, d.VisibleObservations...)
		for _, s := range d.VisibleSpans {
			s.DocID = d.DocID
			spans = append(spans, s)
		}
	}
	return docs, spans, visible
}

// finalizeRanked is retrieval_post.finalize_ranked_ids: adjacent leaves of the same room within
// min_gap message positions are merged into the better-ranked one (mode "merge") or skipped.
func (ix *Index) finalizeRanked(ranked []string, topK int, recent bool) ([]string, map[string][]string) {
	mode := ix.Cfg.dedupMode()
	if mode == "off" || mode == "none" {
		return ranked[:min(len(ranked), max(0, topK))], map[string][]string{}
	}
	leafSize := ix.Cfg.leafSize()
	minGap := max(4, leafSize/2)
	if recent {
		minGap = leafSize * 3
	}
	var picked []string
	var starts []int
	mergeOf := map[string][]string{}
	var skipped []string
	for _, id := range ranked {
		l, ok := ix.Leaf(id)
		start := -10000
		if ok {
			start = l.Start
		}
		mate := -1
		for i, prevID := range picked {
			prev, _ := ix.Leaf(prevID)
			if prev.Room != l.Room {
				continue
			}
			if abs(start-starts[i]) < minGap {
				mate = i
				break
			}
		}
		if mate < 0 {
			picked = append(picked, id)
			starts = append(starts, start)
			mergeOf[id] = nil
			if mode != "merge" && len(picked) >= topK {
				break
			}
			continue
		}
		skipped = append(skipped, id)
		if mode == "merge" {
			mergeOf[picked[mate]] = append(mergeOf[picked[mate]], id)
		}
		if mode != "merge" && len(picked) >= topK {
			break
		}
	}
	if mode != "merge" && len(picked) < topK {
		in := setOf(picked...)
		for _, id := range append(skipped, ranked...) {
			if len(picked) >= topK {
				break
			}
			if !in.Has(id) {
				picked = append(picked, id)
				in[id] = struct{}{}
			}
		}
	}
	picked = picked[:min(len(picked), max(0, topK))]
	if mode == "merge" {
		out := map[string][]string{}
		for _, id := range picked {
			out[id] = mergeOf[id]
		}
		mergeOf = out
	}
	return picked, mergeOf
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// mergedSpan widens a leaf to cover its merged neighbours.
func (ix *Index) mergedSpan(id string, extras []string) leaves.Leaf {
	l, _ := ix.Leaf(id)
	out := l
	for _, e := range extras {
		if x, ok := ix.Leaf(e); ok {
			out.Start = min(out.Start, x.Start)
			out.End = max(out.End, x.End)
		}
	}
	return out
}
