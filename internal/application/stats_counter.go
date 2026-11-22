package application

import (
	"context"
	"errors"
	"sync"
	"time"

	"masamong/internal/action"
	"masamong/internal/catalog"
	"masamong/internal/tools"
)

// The action counter (review M7): every call through Service.Execute is counted in memory by the
// local day it was counted on, the action id folded to its catalog name (an operator's HTTP tool is
// custom_http, anything else unknown), where the request came from and its result code. The
// scheduler flushes the deltas at most every ten minutes, Close flushes them once, and a crash
// loses at most what arrived since the last flush.
//
// Incrementing is one map write under a short lock; flushing swaps the map under the lock and
// writes to the store outside it (defensive-patterns: no store call under a lock). A write that
// fails merges the deltas back so they are retried; stats off drops them.

// statsFlushEvery is the longest the counted deltas stay in memory.
const statsFlushEvery = 10 * time.Minute

// statsCountKey is one counted (day, action, via, code).
type statsCountKey struct {
	day, action, via, code string
}

// statsActionCounter holds the unflushed counts. flushed throttles scheduled flush attempts, so a
// failing store is retried every statsFlushEvery, not every scheduler tick.
type statsActionCounter struct {
	mu      sync.Mutex
	counts  map[statsCountKey]int64
	flushed time.Time
}

// statsCount records one Execute. Practice mode and a service without a store are skipped, as
// noteLLMCall does (ledger.go).
func (s *Service) statsCount(req action.Request, res action.Result) {
	if !s.statsOn() || s.Store == nil {
		return
	}
	key := statsCountKey{
		day:    s.Clock.Now().In(s.instanceZone()).Format("2006-01-02"),
		action: req.Action,
		via:    actionVia(req),
		code:   "ok",
	}
	if _, known := catalog.Lookup(req.Action); !known {
		opTool := false
		if r, isTool := tools.Find(s.toolRegistry(), req.Action); isTool && r.Kind == "http" {
			opTool = true
		}
		key.action = statsActionID(req.Action, opTool)
	}
	if !res.OK {
		key.code = "none"
		if res.Error != nil && res.Error.Code != "" {
			key.code = res.Error.Code
		}
	}
	s.actionStats.mu.Lock()
	if s.actionStats.counts == nil {
		s.actionStats.counts = map[statsCountKey]int64{}
	}
	s.actionStats.counts[key]++
	s.actionStats.mu.Unlock()
}

// statsFlush writes the counted deltas with statsAdd. The map is swapped under the lock and the
// store write happens after it is released; a failed write merges the deltas back (errStatsOff
// drops them: the config was switched off, so collection must not resume when it is back on).
func (s *Service) statsFlush(ctx context.Context) {
	s.actionStats.mu.Lock()
	counts := s.actionStats.counts
	s.actionStats.counts = nil
	s.actionStats.mu.Unlock()
	if len(counts) == 0 {
		return
	}
	rows := make([]statsRow, 0, len(counts))
	for k, n := range counts {
		rows = append(rows, statsRow{
			Day: k.day, Metric: "actions.calls", N: n,
			Dims: map[string]string{"action": k.action, "via": k.via, "code": k.code},
		})
	}
	switch err := s.statsAdd(ctx, rows); {
	case err == nil, errors.Is(err, errStatsOff):
	default:
		s.Log.Debug("stats: action counter flush failed; counts kept", "err", err.Error())
		s.actionStats.mu.Lock()
		if s.actionStats.counts == nil {
			s.actionStats.counts = map[statsCountKey]int64{}
		}
		for k, n := range counts {
			s.actionStats.counts[k] += n
		}
		s.actionStats.mu.Unlock()
	}
}

// statsFlushTick is the scheduler's flush: at most every statsFlushEvery and only when there is
// something to write (the same reasoning as the uptime heartbeat).
func (s *Service) statsFlushTick(ctx context.Context) {
	now := s.Clock.Now()
	s.actionStats.mu.Lock()
	if len(s.actionStats.counts) == 0 || now.Sub(s.actionStats.flushed) < statsFlushEvery {
		s.actionStats.mu.Unlock()
		return
	}
	s.actionStats.flushed = now
	s.actionStats.mu.Unlock()
	s.statsFlush(ctx)
}
