package jobs

import (
	"encoding/json"
	"fmt"
	"math"
	"time"
)

// Progress is stored in Job.Checkpoint as JSON. It never contains message bodies.
type Progress struct {
	Phase      string  `json:"phase,omitempty"`
	Detail     string  `json:"detail,omitempty"`
	Done       int     `json:"done"`
	Total      int     `json:"total"`
	Remaining  int     `json:"remaining"`
	PhasePct   float64 `json:"phase_pct"`
	OverallPct float64 `json:"overall_pct"`
	RatePerS   float64 `json:"rate_per_s,omitempty"`
	ETAS       *int    `json:"eta_s"`
	ETALabel   string  `json:"eta_label,omitempty"`
	ElapsedS   int     `json:"elapsed_s,omitempty"`
	Label      string  `json:"label"`
}

// FormatETA is a language-neutral estimate ("<15s", "~45s", "~2m05s", "~1h03m"). Labels are stored
// in job checkpoints and read by every surface, so they carry numbers and SI units, never words of
// one language; surfaces that want prose localize from eta_s.
func FormatETA(seconds float64) string {
	if seconds < 0 || seconds > 86400*7 || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		return ""
	}
	s := int(seconds)
	switch {
	case s < 15:
		return "<15s"
	case s < 90:
		return fmt.Sprintf("~%ds", s)
	case s < 3600:
		return fmt.Sprintf("~%dm%02ds", s/60, s%60)
	default:
		return fmt.Sprintf("~%dh%02dm", s/3600, (s%3600)/60)
	}
}

func Snapshot(phase string, done, total int, started time.Time, now time.Time) Progress {
	if total < 0 {
		total = 0
	}
	if done < 0 {
		done = 0
	}
	if done > total && total > 0 {
		done = total
	}
	remaining := total - done
	if remaining < 0 {
		remaining = 0
	}
	elapsed := now.Sub(started).Seconds()
	if elapsed < 0 {
		elapsed = 0
	}
	var rate float64
	if elapsed > 0.5 && done > 0 {
		rate = float64(done) / elapsed
	}
	var eta *int
	etaLabel := ""
	if rate > 0 && remaining > 0 {
		sec := float64(remaining) / rate
		s := int(sec)
		eta = &s
		etaLabel = FormatETA(sec)
	}
	phasePct := 0.0
	if total > 0 {
		phasePct = math.Min(100, 100*float64(done)/float64(total))
	}
	p := Progress{
		Phase:      phase,
		Done:       done,
		Total:      total,
		Remaining:  remaining,
		PhasePct:   math.Round(phasePct*10) / 10,
		OverallPct: math.Round(phasePct*10) / 10,
		RatePerS:   math.Round(rate*100) / 100,
		ETAS:       eta,
		ETALabel:   etaLabel,
		ElapsedS:   int(elapsed),
	}
	p.Label = fmt.Sprintf("%s %d/%d (%.1f%%)", phase, done, total, p.PhasePct)
	if etaLabel != "" {
		p.Label += " · " + etaLabel
	}
	return p
}

func EncodeProgress(p Progress) string {
	b, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	return string(b)
}

func ParseProgress(checkpoint string) Progress {
	if checkpoint == "" {
		return Progress{}
	}
	var p Progress
	if err := json.Unmarshal([]byte(checkpoint), &p); err != nil {
		p.Label = checkpoint
		return p
	}
	return p
}
