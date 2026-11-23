package application

import (
	"errors"
	"strings"
	"time"

	"masamong/internal/features/notices"
	"masamong/internal/features/privacy"
)

// The notice rules that need no I/O: which consent a kind needs, matching names to the catalog's schools
// and boards, the delivery slot, grouping and wording helpers for the digest.

func noticeScope(kind string) string {
	if kind == notices.KindTransfer {
		return privacy.TransferNotice
	}
	return privacy.SchoolNotice
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

var bandIcon = map[string]string{"action": "⏰", "opportunity": "✨", "reference": "📄"}

// noticeBands is the order school notices are grouped in: what to do first, then chances, then the rest.
var noticeBands = []string{"action", "opportunity", "reference"}

var mdSpecial = strings.NewReplacer("[", "(", "]", ")", "*", "\\*", "_", "\\_", "`", "'")

func mdEscape(s string) string { return mdSpecial.Replace(s) }

// noticeSlot keeps an unsettled logical delivery date while a quiet window crosses midnight. Once
// today's due time has passed, missed older dates are drained oldest first.
func noticeSlot(now time.Time, at time.Time, last, createdAt string, quietUntil func(time.Time) time.Time) (time.Time, string) {
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	todayDue := time.Date(today.Year(), today.Month(), today.Day(), at.Hour(), at.Minute(), 0, 0, now.Location())
	candidate := time.Time{}
	if last != "" {
		if previous, err := time.ParseInLocation("2006-01-02", last, now.Location()); err == nil {
			candidate = previous.AddDate(0, 0, 1)
		}
	} else if now.Before(todayDue) {
		candidate = today.AddDate(0, 0, -1)
		previousDue := time.Date(candidate.Year(), candidate.Month(), candidate.Day(), at.Hour(), at.Minute(), 0, 0, now.Location())
		created, err := time.Parse(time.RFC3339, createdAt)
		if err != nil || created.After(previousDue) {
			candidate = time.Time{}
		}
	}
	if !candidate.IsZero() && candidate.Before(today) {
		due := time.Date(candidate.Year(), candidate.Month(), candidate.Day(), at.Hour(), at.Minute(), 0, 0, now.Location())
		if !now.Before(quietUntil(due)) {
			return due, due.Format("2006-01-02")
		}
	}
	return todayDue, today.Format("2006-01-02")
}

func orDash(v string) string {
	if strings.TrimSpace(v) == "" {
		return "-"
	}
	return v
}

func firstNonEmpty(v ...string) string {
	for _, x := range v {
		if x != "" {
			return x
		}
	}
	return ""
}

func removeString(list []string, v string) []string {
	out := []string{}
	for _, x := range list {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

func subSources(c *notices.Catalog, sub noticeSub) []string {
	if sub.Kind == notices.KindTransfer {
		return sub.Sources
	}
	var ids []string
	for _, src := range c.Sources {
		if src.Kind == notices.KindSchool && src.School == sub.Profile.School && !src.Manual {
			ids = append(ids, src.ID)
		}
	}
	return ids
}

func squash(v string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(v), " ", ""))
}

// pickUniversities matches names ("강원대", "부산대학교", a source id) to transfer boards.
func pickUniversities(c *notices.Catalog, text string) ([]string, []string) {
	var ids, unknown []string
	for _, part := range strings.FieldsFunc(text, func(r rune) bool { return r == ',' || r == '，' || r == '/' || r == ' ' }) {
		w := squash(part)
		if w == "" {
			continue
		}
		found := ""
		for _, src := range c.Sources {
			if src.Kind != notices.KindTransfer {
				continue
			}
			name := squash(src.University)
			if w == squash(src.ID) || w == strings.TrimPrefix(src.ID, "transfer_") || name == w || strings.HasPrefix(name, w) || strings.Contains(name, w) && len([]rune(w)) >= 2 {
				found = src.ID
				break
			}
		}
		if found == "" {
			unknown = append(unknown, part)
		} else if !contains(ids, found) {
			ids = append(ids, found)
		}
	}
	return ids, unknown
}

func universityList(c *notices.Catalog) string {
	var names []string
	for _, src := range c.Sources {
		if src.Kind == notices.KindTransfer {
			names = append(names, src.University)
		}
	}
	return strings.Join(names, ", ")
}

func topicList(c *notices.Catalog) string {
	var names []string
	for _, t := range c.Vocabulary.Topics {
		names = append(names, t.Label)
	}
	return strings.Join(names, ", ")
}

func isSchool(c *notices.Catalog, w string) bool {
	w = squash(w)
	for _, sc := range c.Schools {
		n := squash(sc.Name)
		if w == sc.ID || w == n || strings.HasPrefix(n, w) && len([]rune(w)) >= 2 {
			return true
		}
	}
	return false
}

// noticeSourceIDs are the boards a subscription reads: the chosen ones for transfer notices, every board
// of the school for school notices.
func noticeSourceIDs(c *notices.Catalog, sub noticeSub) []string {
	if sub.Kind == notices.KindTransfer {
		return sub.Sources
	}
	var srcIDs []string
	for _, src := range c.Sources {
		if src.Kind == notices.KindSchool && src.School == sub.Profile.School {
			srcIDs = append(srcIDs, src.ID)
		}
	}
	return srcIDs
}

// noticesSince is the oldest change a subscription's candidates may have (RFC 3339, UTC): firstDays back
// from now, or after the first delivery (unless the sent ones are included) the subscription's start when
// that is later.
func noticesSince(now time.Time, firstDays int, sub noticeSub, includeSent bool) string {
	since := now.AddDate(0, 0, -firstDays).UTC().Format(time.RFC3339)
	if sub.LastDelivery != "" && !includeSent {
		// after the first delivery only what changed since subscribing is new to them
		if sub.CreatedAt > since {
			since = sub.CreatedAt
		}
	}
	return since
}

// daysUntil is the whole days from today's date to the date d (negative once it has passed).
func daysUntil(d, today time.Time) int {
	return int(d.Sub(time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)).Hours() / 24)
}

// noticeGroup is one heading of a digest: a university's notices (transfer) or a band's (school; band ""
// holds the notices of no known band). heading is filled in by the card.
type noticeGroup struct {
	band, heading string
	items         []rankedNotice
}

// groupNotices groups a digest's notices: transfer notices by university in the order they first appear,
// school notices by band in noticeBands order, then those of no known band. Empty groups are left out.
func groupNotices(kind string, list []rankedNotice) []noticeGroup {
	var groups []noticeGroup
	if kind == notices.KindTransfer {
		at := map[string]int{}
		for _, n := range list {
			i, ok := at[n.University]
			if !ok {
				i = len(groups)
				at[n.University] = i
				groups = append(groups, noticeGroup{})
			}
			groups[i].items = append(groups[i].items, n)
		}
		return groups
	}
	for _, band := range append(noticeBands, "") {
		g := noticeGroup{band: band}
		for _, n := range list {
			if n.Score.Band == band || band == "" && bandIcon[n.Score.Band] == "" {
				g.items = append(g.items, n)
			}
		}
		if len(g.items) == 0 {
			continue
		}
		groups = append(groups, g)
	}
	return groups
}

// boardIsMine reports that a subscription reads the board: its school's boards for school notices, the
// chosen boards for transfer notices.
func boardIsMine(sub noticeSub, src notices.Source) bool {
	return (sub.Kind == notices.KindSchool && src.Kind == notices.KindSchool && src.School == sub.Profile.School) ||
		(sub.Kind == notices.KindTransfer && contains(sub.Sources, src.ID))
}

// noticeSendFailure is how a digest whose send failed is settled: a definite failure (ErrNotDelivered) is
// "failed" and leaves the day open for another try, closed DMs are "closed" and turn the subscription off,
// anything else is "unknown" (it may have been posted). Every outcome but "failed" settles the day.
func noticeSendFailure(err error) (state string, settleDate, disable bool) {
	state, settleDate = "unknown", true
	if errors.Is(err, ErrNotDelivered) {
		state, settleDate = "failed", false
	} else if errors.Is(err, ErrDMClosed) {
		state, disable = "closed", true
	}
	return state, settleDate, disable
}
