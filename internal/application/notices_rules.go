package application

import (
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

func (s *Service) subSources(c *notices.Catalog, sub noticeSub) []string {
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
func (s *Service) pickUniversities(c *notices.Catalog, text string) ([]string, []string) {
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

func (s *Service) universityList(c *notices.Catalog) string {
	var names []string
	for _, src := range c.Sources {
		if src.Kind == notices.KindTransfer {
			names = append(names, src.University)
		}
	}
	return strings.Join(names, ", ")
}

func (s *Service) topicList(c *notices.Catalog) string {
	var names []string
	for _, t := range c.Vocabulary.Topics {
		names = append(names, t.Label)
	}
	return strings.Join(names, ", ")
}

func (s *Service) isSchool(c *notices.Catalog, w string) bool {
	w = squash(w)
	for _, sc := range c.Schools {
		n := squash(sc.Name)
		if w == sc.ID || w == n || strings.HasPrefix(n, w) && len([]rune(w)) >= 2 {
			return true
		}
	}
	return false
}
