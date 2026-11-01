package application

import (
	"context"
	"fmt"
	"time"

	"masamong/internal/action"
	"masamong/internal/config"
	"masamong/internal/crash"
	"masamong/internal/errcode"
	"masamong/internal/launcher"
)

// The scheduler runs time-based work inside the daemon: the automatic backup with its retention, the
// daily pruning of old operational records (usage_gc, retention.go) and a heartbeat for the uptime
// history. Every run leaves its state in admin_state so the console shows the next run, the last
// result and its error, and can run a task now.

type scheduleState struct {
	LastRun    string         `json:"last_run,omitempty"`
	OK         bool           `json:"ok"`
	Error      string         `json:"error,omitempty"`
	DurationMS int64          `json:"duration_ms,omitempty"`
	Detail     map[string]any `json:"detail,omitempty"`
}

// RunScheduler blocks until ctx ends. The daemon starts it once per process.
func (s *Service) RunScheduler(ctx context.Context) {
	ctx, done, ok := s.beginWork(ctx)
	if !ok {
		return
	}
	defer done()
	if s.Store == nil {
		return
	}
	s.noteRestoreResult()
	s.ownedGo("maintenance leftovers", func(context.Context) { s.cleanDataLeftovers() })
	s.kickRedaction() // jobs a crash left, and turns written before the evidence index
	s.noteUptime("start", "")
	s.recoverReminders()
	started := s.Clock.Now()
	lastClean := started
	var lastAlert time.Time
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	// a failing tick (backup, retention) is logged and the next minute runs as usual
	_ = crash.Run(s.Log, "scheduler", func() { s.schedulerTickContext(ctx) })
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = crash.Run(s.Log, "scheduler", func() {
				s.schedulerTickContext(ctx)
				s.ownedGo("fortune briefings", func(ctx context.Context) { s.runFortuneMorning(ctx) })
				s.ownedGo("reminders", func(ctx context.Context) { s.deliverReminders(ctx) })
				if styleTicking.CompareAndSwap(false, true) {
					s.ownedGo("room styles", func(ctx context.Context) { defer styleTicking.Store(false); s.refreshRoomStyles(ctx) })
				}
				s.sweepCollectNext() // collected moments whose follow-up window ended with nothing happening
				if threadsTicking.CompareAndSwap(false, true) {
					s.ownedGo("idle threads", func(ctx context.Context) { defer threadsTicking.Store(false); s.closeIdleThreads(ctx) })
				}
				if disasterTicking.CompareAndSwap(false, true) {
					s.ownedGo("disaster alerts", func(ctx context.Context) { defer disasterTicking.Store(false); s.disasterTick(ctx) })
				}
				if noticesTicking.CompareAndSwap(false, true) {
					s.ownedGo("notices", func(ctx context.Context) { defer noticesTicking.Store(false); s.noticesTick(ctx) })
				}
				if s.retentionDue(s.Clock.Now(), started) {
					s.runScheduledRetention(ctx)
				}
				if s.updateDue() && updateChecking.CompareAndSwap(false, true) {
					s.ownedGo("update check", func(ctx context.Context) { defer updateChecking.Store(false); s.checkUpdate(ctx) })
				}
				if now := s.Clock.Now(); now.Sub(lastClean) >= time.Hour && len(s.leftoverList()) > 0 {
					lastClean = now
					s.ownedGo("maintenance leftovers", func(context.Context) { s.cleanDataLeftovers() })
				}
				if now := s.Clock.Now(); now.Sub(started) >= alertStartDelay && now.Sub(lastAlert) >= alertEvery {
					lastAlert = now
					s.alertTick(ctx)
				}
			})
		}
	}
}

func (s *Service) schedulerTick() { s.schedulerTickContext(context.Background()) }

func (s *Service) schedulerTickContext(ctx context.Context) {
	now := s.Clock.Now()
	_ = s.recordUptimeHeartbeat(now)
	pol := s.Doc().Backup
	if pol.Schedule == "off" || pol.Schedule == "" {
		return
	}
	var st scheduleState
	_, _ = s.kvGet("schedule:backup", &st)
	slot, ok := lastBackupSlot(now, pol, s.instanceZone())
	if !ok {
		return
	}
	if last, err := time.Parse(time.RFC3339, st.LastRun); err == nil && !last.Before(slot) {
		return
	}
	// a slot missed while the service was off runs now
	s.startScheduledBackup(ctx)
}

func (s *Service) startScheduledBackup(parent context.Context) {
	s.ownedSingleFlight(parent, &s.backupRunning, "scheduled backup", func(ctx context.Context) {
		s.runScheduledBackup(ctx)
	})
}

// runScheduledBackup makes an automatic backup and prunes old ones.
func (s *Service) runScheduledBackup(ctx context.Context) scheduleState {
	started := s.Clock.Now()
	st := scheduleState{LastRun: started.UTC().Format(time.RFC3339)}
	unlock, err := lockMaintenanceContext(ctx, s.DataRoot, false)
	name := ""
	if err == nil {
		defer unlock()
		name, _, err = s.createBackupLockedContext(ctx, backupAuto)
	}
	st.DurationMS = s.Clock.Now().Sub(started).Milliseconds()
	if err != nil {
		st.Error = err.Error()
		if ctx.Err() != nil {
			return st
		}
		s.Log.Warn("scheduled backup failed", "err", err)
	} else {
		st.OK = true
		st.Detail = map[string]any{"name": name}
		if ctx.Err() == nil {
			if removed := s.pruneKindLocked(backupAuto, s.Doc().Backup.Keep); len(removed) > 0 {
				st.Detail["pruned"] = removed
			}
		}
		s.Log.Info("scheduled backup", "name", name)
	}
	_ = s.kvSet("schedule:backup", st)
	s.emit("audit", "backup.schedule", nil)
	return st
}

// lastBackupSlot is the most recent scheduled time at or before now.
func lastBackupSlot(now time.Time, p config.Backup, loc *time.Location) (time.Time, bool) {
	local := now.In(loc)
	slot := time.Date(local.Year(), local.Month(), local.Day(), p.Hour, 0, 0, 0, loc)
	switch p.Schedule {
	case "daily":
		if slot.After(local) {
			slot = slot.AddDate(0, 0, -1)
		}
	case "weekly":
		back := (int(local.Weekday()) - p.Weekday + 7) % 7
		slot = slot.AddDate(0, 0, -back)
		if slot.After(local) {
			slot = slot.AddDate(0, 0, -7)
		}
	default:
		return time.Time{}, false
	}
	return slot, true
}

// nextBackup is the first scheduled time after now.
func nextBackup(now time.Time, p config.Backup, loc *time.Location) (time.Time, bool) {
	last, ok := lastBackupSlot(now, p, loc)
	if !ok {
		return time.Time{}, false
	}
	if p.Schedule == "daily" {
		return last.AddDate(0, 0, 1), true
	}
	return last.AddDate(0, 0, 7), true
}

func (s *Service) schedulesAction(ctx context.Context, req action.Request) action.Result {
	if _, err := s.adminDB(); err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	switch req.Action {
	case "schedules.run":
		switch strVal(inputMap(req), "id", "") {
		case "backup":
			wctx, done, ok := s.detachedWork(ctx)
			if !ok {
				return action.Fail(req, errcode.IO, "generic.error", "service is shutting down", nil)
			}
			defer done()
			st := s.runScheduledBackup(wctx)
			if !st.OK {
				return action.Fail(req, errcode.IO, "generic.error", st.Error, nil)
			}
			return action.OK(req, map[string]any{"state": st})
		case "usage_gc":
			st := s.runScheduledRetention(context.Background())
			if !st.OK {
				return action.Fail(req, errcode.IO, "generic.error", st.Error, nil)
			}
			return action.OK(req, map[string]any{"state": st})
		}
		return action.Fail(req, errcode.InvalidInput, "generic.error", "id must be backup or usage_gc", nil)
	}
	now := s.Clock.Now()
	loc := s.instanceZone()
	var backup, index scheduleState
	_, _ = s.kvGet("schedule:backup", &backup)
	_, _ = s.kvGet("schedule:index", &index)
	items := []map[string]any{}
	b := map[string]any{"id": "backup", "on": s.Doc().Backup.Schedule != "off", "schedule": s.Doc().Backup.Schedule,
		"hour": s.Doc().Backup.Hour, "weekday": s.Doc().Backup.Weekday, "keep": s.Doc().Backup.Keep, "state": backup, "runnable": true, "route": "/maintenance"}
	if next, ok := nextBackup(now, s.Doc().Backup, loc); ok && s.Doc().Backup.Schedule != "off" {
		b["next_run"] = next.UTC().Format(time.RFC3339)
	}
	items = append(items, b)
	ix := map[string]any{"id": "index", "on": s.Doc().Memory.Embed, "every_min": max(1, s.Doc().Memory.IndexIntervalMin), "state": index, "route": "/blocks?view=settings"}
	if last, err := time.Parse(time.RFC3339, index.LastRun); err == nil && s.Doc().Memory.Embed {
		ix["next_run"] = last.Add(time.Duration(max(1, s.Doc().Memory.IndexIntervalMin)) * time.Minute).UTC().Format(time.RFC3339)
	}
	items = append(items, ix)
	var gc scheduleState
	_, _ = s.kvGet("schedule:usage_gc", &gc)
	g := map[string]any{"id": "usage_gc", "on": true, "schedule": "daily", "state": gc, "runnable": true, "route": "/usage",
		"retention_days": s.Doc().Retention.Days()}
	if last, err := time.Parse(time.RFC3339, gc.LastRun); err == nil {
		g["next_run"] = last.Add(retentionEvery).UTC().Format(time.RFC3339)
	}
	items = append(items, g)
	return action.OK(req, map[string]any{"schedules": items, "timezone": loc.String()})
}

// ---------------------------------------------------------------------------
// uptime history
// ---------------------------------------------------------------------------

type uptimeEvent struct {
	At      string `json:"at"`
	Kind    string `json:"kind"` // start | stop | restart | crash
	Version string `json:"version,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

const maxUptimeEvents = 400

const (
	uptimeHeartbeatEvery = 10 * time.Minute
	// the scheduler ticks once a minute (plus jitter); a tick that skips because the last write is a
	// hair too young would push the next write a whole tick later, so the age that triggers a write is
	// one tick plus slack below the interval, and the stored time is never more than ten minutes old
	uptimeHeartbeatSlack = time.Minute + 10*time.Second
)

// recordUptimeHeartbeat persists the scheduler's clock at most about every ten minutes (a commit and
// fsync each minute, forever, is a lot for an SD card). noteUptime reads it only when the previous
// run never recorded a stop, to date the crash; a clean stop records its own event, so NoteStop does
// not flush it. A clock that moved backwards writes at once, or the value would stay in the future.
func (s *Service) recordUptimeHeartbeat(now time.Time) error {
	s.uptimeMu.Lock()
	defer s.uptimeMu.Unlock()
	if s.Store == nil || s.practice {
		return nil
	}
	now = now.UTC()
	if age := now.Sub(s.uptimeWritten); !s.uptimeWritten.IsZero() && age >= 0 && age < uptimeHeartbeatEvery-uptimeHeartbeatSlack {
		return nil
	}
	if err := s.kvSet("uptime:heartbeat", now.Format(time.RFC3339)); err != nil {
		return err
	}
	s.uptimeWritten = now
	return nil
}

func (s *Service) noteUptime(kind, reason string) {
	if s.Store == nil || s.practice {
		return
	}
	var ev []uptimeEvent
	_, _ = s.kvGet("uptime:events", &ev)
	now := s.Clock.Now().UTC().Format(time.RFC3339)
	if kind == "start" {
		// the supervisor (masamongd --supervise) says why the previous run ended, when it ended badly
		note, noted := launcher.TakeCrashNote(s.DataRoot)
		if len(ev) > 0 && ev[len(ev)-1].Kind == "start" {
			// the previous run never recorded a stop: it ended without a clean shutdown
			var hb string
			_, _ = s.kvGet("uptime:heartbeat", &hb)
			if hb == "" {
				hb = ev[len(ev)-1].At
			}
			ev = append(ev, uptimeEvent{At: hb, Kind: "crash", Reason: crashReason(note, noted)})
			s.queueAlert(alertCrash, map[string]any{"reason": note.Reason, "gave_up": note.GaveUp})
		}
	}
	ev = append(ev, uptimeEvent{At: now, Kind: kind, Version: Version, Reason: reason})
	if len(ev) > maxUptimeEvents {
		ev = ev[len(ev)-maxUptimeEvents:]
	}
	_ = s.kvSet("uptime:events", ev)
}

// crashReason is what the uptime history shows for an abnormal end.
func crashReason(n launcher.CrashNote, ok bool) string {
	if !ok {
		return ""
	}
	r := n.Reason
	if n.Detail != "" {
		r += " — " + n.Detail
	}
	if n.GaveUp {
		r += fmt.Sprintf(" (%d times in a row; not restarted)", n.Restarts)
	}
	return r
}

// NoteStop records a clean shutdown (restart = the daemon re-executes itself right after). The stop
// event is what tells the next start the run ended cleanly; the heartbeat is not needed for that.
func (s *Service) NoteStop(restart bool) {
	kind := "stop"
	if restart {
		kind = "restart"
	}
	s.noteUptime(kind, "")
}

func (s *Service) runtimeHistory(req action.Request) action.Result {
	var ev []uptimeEvent
	now := s.Clock.Now()
	started := startedAt
	if s.practice {
		// the practice instance is not a process of its own: it shows a sample history instead
		ev, started = practiceUptime(now)
	} else {
		_, _ = s.kvGet("uptime:events", &ev)
	}
	days := min(max(intVal(inputMap(req), "days", 30), 1), 90)
	since := now.AddDate(0, 0, -days)
	// intervals: start → next stop/restart/crash (or now for the running process)
	type span struct{ From, To time.Time }
	var spans []span
	var open *time.Time
	for _, e := range ev {
		at, err := time.Parse(time.RFC3339, e.At)
		if err != nil {
			continue
		}
		switch e.Kind {
		case "start":
			t := at
			open = &t
		default:
			if open != nil {
				spans = append(spans, span{*open, at})
				open = nil
			}
		}
	}
	if open != nil {
		spans = append(spans, span{*open, now})
	}
	var up time.Duration
	outSpans := []map[string]any{}
	for _, sp := range spans {
		from, to := sp.From, sp.To
		if to.Before(since) {
			continue
		}
		if from.Before(since) {
			from = since
		}
		up += to.Sub(from)
		outSpans = append(outSpans, map[string]any{"from": from.UTC().Format(time.RFC3339), "to": to.UTC().Format(time.RFC3339)})
	}
	crashes := 0
	list := []uptimeEvent{}
	for i := len(ev) - 1; i >= 0; i-- {
		if at, err := time.Parse(time.RFC3339, ev[i].At); err == nil && at.After(since) {
			if ev[i].Kind == "crash" {
				crashes++
			}
			list = append(list, ev[i])
		}
	}
	window, from := now.Sub(since), since
	if len(ev) > 0 {
		if first, err := time.Parse(time.RFC3339, ev[0].At); err == nil && first.After(since) {
			window, from = now.Sub(first), first // history started inside the window
		}
	}
	pct := 0.0
	if window > 0 {
		pct = min(100, 100*up.Seconds()/window.Seconds())
	}
	// window_from is where the timeline starts: a history younger than the range is drawn from its
	// first event, not as weeks of nothing before it
	return action.OK(req, map[string]any{"events": list, "spans": outSpans, "uptime_pct": pct, "crashes": crashes,
		"days": days, "since": since.UTC().Format(time.RFC3339), "window_from": from.UTC().Format(time.RFC3339),
		"started_at": started.UTC().Format(time.RFC3339), "sample": s.practice})
}

// autostartAction reads or changes starting at login for this data folder (current OS user only).
func (s *Service) autostartAction(req action.Request) action.Result {
	if s.practice {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "not available in practice mode", nil)
	}
	if req.Action == "runtime.autostart.set" {
		in := inputMap(req)
		if _, ok := in["enabled"]; !ok {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "enabled (true or false) required", nil)
		}
		st, err := launcher.SetAutostart(s.DataRoot, boolVal(in, "enabled"))
		if err != nil {
			return action.Fail(req, errcode.IO, "generic.error", err.Error(), map[string]any{"autostart": st})
		}
		return action.OK(req, map[string]any{"autostart": st})
	}
	return action.OK(req, map[string]any{"autostart": launcher.AutostartStatus(s.DataRoot)})
}
