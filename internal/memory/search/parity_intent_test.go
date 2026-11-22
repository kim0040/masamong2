package search

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"masamong/internal/memory/corpus"
)

// The planner-intent part of the parity gate. A dump made with PARITY_INTENTS (parity_synth.py) holds one row
// per planner-intent case: the time range, the person targets and the recent window, with the reference clock
// the case names. These helpers read that clock and compare what the two sides report about the intent paths
// (the time scope and the person resolution) on top of the hits the main test already compares.

// dumpReference is the reference clock of a dump row (parity_dump.py reference_for).
type dumpReference struct {
	Mode    string  `json:"mode"`
	Now     string  `json:"now"`
	TZ      *string `json:"tz"`
	NaiveTZ *string `json:"naive_tz"`
	Source  string  `json:"source"`
}

// parityZone reads "+09:00" as that fixed offset (the dump uses offsets, so no zone database is needed).
func parityZone(text *string) (*time.Location, error) {
	if text == nil || *text == "" {
		return nil, nil
	}
	var sign, h, m int
	sign = 1
	if strings.HasPrefix(*text, "-") {
		sign = -1
	}
	if _, err := fmt.Sscanf(strings.TrimLeft(*text, "+-"), "%d:%d", &h, &m); err != nil {
		return nil, fmt.Errorf("zone %q: %w", *text, err)
	}
	return time.FixedZone(*text, sign*(h*3600+m*60)), nil
}

func (r *dumpReference) timeReference() (*TimeReference, error) {
	if r == nil {
		return nil, nil
	}
	now, aware, ok := corpus.ParseTime(r.Now)
	if !ok {
		return nil, fmt.Errorf("reference time %q does not parse", r.Now)
	}
	tz, err := parityZone(r.TZ)
	if err != nil {
		return nil, err
	}
	naiveTZ, err := parityZone(r.NaiveTZ)
	if err != nil {
		return nil, err
	}
	return &TimeReference{Mode: r.Mode, Now: Moment{T: now, Aware: aware}, TZ: tz, NaiveTZ: naiveTZ, Source: r.Source}, nil
}

// parityCanonical reads any JSON-shaped value through encoding/json, so a Go struct and a decoded dump
// compare the same way, and drops the empty values (false, 0, "", null, empty list or map): the Go trace omits
// them and the sandbox trace always writes them.
func parityCanonical(v any) any {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("unmarshalable: %v", err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Sprintf("unmarshalable: %v", err)
	}
	return parityDropEmpty(out)
}

func parityDropEmpty(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, e := range x {
			if e = parityDropEmpty(e); e != nil {
				out[k] = e
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	case []any:
		if len(x) == 0 {
			return nil
		}
		return x
	case string:
		if x == "" {
			return nil
		}
	case float64:
		if x == 0 {
			return nil
		}
	case bool:
		if !x {
			return nil
		}
	}
	return v
}

// parityPick keeps the listed keys of a map, so a side that reports more than the other is not a difference.
func parityPick(v any, keys ...string) any {
	m, _ := parityCanonical(v).(map[string]any)
	out := map[string]any{}
	for _, k := range keys {
		if e, ok := m[k]; ok {
			out[k] = e
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

var parityTimeKeys = []string{"time_kind", "time_window_applied", "time_window_hard", "recent_window_applied", "recent_window_days",
	"recent_window_leaves", "time_pool_n", "time_records_in_range_n", "time_window_scale", "time_window_scale_applied",
	"time_from", "time_to", "time_window_invalid", "time_window_invalid_reason", "time_unresolved", "time_unresolved_reason",
	"time_unresolved_partial"}

// parityTimeTrace is the part of trace["time"] the dump carries, in comparable form. The recent window is
// reported under other names by the two sides (a naming difference in the trace, not in the search): the
// sandbox writes its size as recent_pool_n where Go writes time_pool_n, and a window over several rooms is
// described by the sandbox as recent_window_blocks and by Go with the days and leaves it used. The size is
// compared; the days and leaves only when both sides wrote them.
func parityTimeTrace(v any) any {
	m, _ := parityPick(v, append([]string{"recent_pool_n", "recent_window_blocks"}, parityTimeKeys...)...).(map[string]any)
	if m == nil {
		return nil
	}
	if n, ok := m["recent_pool_n"]; ok {
		if _, has := m["time_pool_n"]; !has {
			m["time_pool_n"] = n
		}
		delete(m, "recent_pool_n")
	}
	if _, multi := m["recent_window_blocks"]; multi {
		delete(m, "recent_window_blocks")
		m["recent_window_multi_room"] = true
	}
	return m
}

// parityTimeDiff compares the two time traces (got = Go, want = sandbox).
func parityTimeDiff(got, want any) string {
	g, _ := parityTimeTrace(got).(map[string]any)
	w, _ := parityTimeTrace(want).(map[string]any)
	if g != nil && w != nil {
		for _, k := range []string{"recent_window_days", "recent_window_leaves"} {
			_, inG := g[k]
			_, inW := w[k]
			if inG != inW && w["recent_window_multi_room"] == true {
				delete(g, k)
				delete(w, k)
			}
		}
		delete(w, "recent_window_multi_room")
	}
	return parityDiff(nilIfEmpty(g), nilIfEmpty(w))
}

func nilIfEmpty(m map[string]any) any {
	if len(m) == 0 {
		return nil
	}
	return m
}

// parityPersonsTrace is the person resolution both sides report: the targets, the labels and speakers they
// resolved to, what stayed ambiguous or unresolved, the scope mode and the account-link counts.
func parityPersonsTrace(v any) any {
	m, _ := parityCanonical(v).(map[string]any)
	if m == nil {
		return nil
	}
	out := parityPick(m, "targets", "labels", "speakers", "ambiguous", "unresolved")
	pick := func(key string, inner ...string) any {
		sub, _ := m[key].(map[string]any)
		return parityPick(sub, inner...)
	}
	o, _ := out.(map[string]any)
	if o == nil {
		o = map[string]any{}
	}
	if s := pick("scope", "mode", "speakers_n"); s != nil {
		o["scope"] = s
	}
	if s := pick("speaker_keys", "account_linked_n", "label_only_n"); s != nil {
		o["speaker_keys"] = s
	}
	if len(o) == 0 {
		return nil
	}
	return o
}

func parityDiff(got, want any) string {
	if reflect.DeepEqual(got, want) {
		return ""
	}
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(want)
	return fmt.Sprintf("go %s\n  sandbox %s", g, w)
}

func TestParityIntentCompareReadsTheDump(t *testing.T) {
	plus9, minus := "+09:00", "-03:30"
	ref, err := (&dumpReference{Mode: "replay", Now: "2026-01-24T12:00:00+09:00", TZ: &plus9, NaiveTZ: &plus9, Source: "parity"}).timeReference()
	if err != nil || ref == nil || !ref.Now.Aware || ref.TZ == nil || ref.NaiveTZ == nil {
		t.Fatalf("aware reference: %+v %v", ref, err)
	}
	if _, off := ref.Now.T.In(ref.TZ).Zone(); off != 9*3600 {
		t.Fatalf("offset %d", off)
	}
	naive, err := (&dumpReference{Mode: "replay", Now: "2026-01-24T12:00:00"}).timeReference()
	if err != nil || naive == nil || naive.Now.Aware || naive.TZ != nil || naive.NaiveTZ != nil {
		t.Fatalf("naive reference: %+v %v", naive, err)
	}
	if ref, err := (*dumpReference)(nil).timeReference(); ref != nil || err != nil {
		t.Fatalf("no reference must stay nil: %v %v", ref, err)
	}
	if _, err := (&dumpReference{Mode: "replay", Now: "yesterday"}).timeReference(); err == nil {
		t.Fatal("an unreadable reference time must fail the test, not be skipped")
	}
	if z, _ := parityZone(&minus); z == nil {
		t.Fatal("negative offset")
	} else if _, off := time.Now().In(z).Zone(); off != -(3*3600 + 30*60) {
		t.Fatalf("negative offset read as %d", off)
	}
}

func TestParityIntentCompareRejectsDifferences(t *testing.T) {
	pool := 15
	got := TimeMeta{Kind: "between", Applied: true, Hard: true, PoolN: &pool, Scale: 1, From: "2026-01-08T00:00:00+09:00"}
	// the sandbox always writes empty fields; the Go trace leaves them out
	same := map[string]any{"time_kind": "between", "time_window_applied": true, "time_window_hard": true, "recent_window_applied": false,
		"time_pool_n": 15, "time_window_scale": 1.0, "time_window_scale_applied": false, "time_from": "2026-01-08T00:00:00+09:00", "time_to": nil}
	if d := parityTimeDiff(got, same); d != "" {
		t.Fatalf("equal traces differ: %s", d)
	}
	for name, mutate := range map[string]func(map[string]any){
		"pool":  func(m map[string]any) { m["time_pool_n"] = 14 },
		"kind":  func(m map[string]any) { m["time_kind"] = "after" },
		"bound": func(m map[string]any) { m["time_from"] = "2026-01-09T00:00:00+09:00" },
		"hard":  func(m map[string]any) { m["time_window_hard"] = false },
		"scale": func(m map[string]any) { m["time_window_scale"] = 2.0 },
	} {
		other := map[string]any{}
		for k, v := range same {
			other[k] = v
		}
		mutate(other)
		if parityTimeDiff(got, other) == "" {
			t.Errorf("a changed %s must be a difference", name)
		}
	}
	goPersons := map[string]any{"targets": []string{"가"}, "labels": []string{"가"}, "speakers": []string{"가"}, "ambiguous": []string{}, "unresolved": []string{},
		"scope": map[string]any{"mode": "none", "speakers_n": 4}, "speaker_keys": map[string]any{"account_linked_n": 1, "label_only_n": 0}}
	sandboxPersons := map[string]any{"targets": []any{"가"}, "labels": []any{"가"}, "speakers": []any{"가"}, "ambiguous": []any{}, "unresolved": []any{},
		"scope": map[string]any{"mode": "none", "leaves_n": nil, "speakers_n": 4}, "speaker_keys": map[string]any{"account_linked_n": 1, "label_only_n": 0, "per_target_n": map[string]any{"가": 1}}}
	if d := parityDiff(parityPersonsTrace(goPersons), parityPersonsTrace(sandboxPersons)); d != "" {
		t.Fatalf("equal person traces differ: %s", d)
	}
	sandboxPersons["speakers"] = []any{"나"}
	if parityDiff(parityPersonsTrace(goPersons), parityPersonsTrace(sandboxPersons)) == "" {
		t.Error("different resolved speakers must be a difference")
	}
	if parityPersonsTrace(nil) != nil || parityPersonsTrace(map[string]any{"targets": []string{}}) != nil {
		t.Error("a missing and an empty person trace are the same")
	}
}
